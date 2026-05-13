// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/fluxcd/flux-mirror/internal/config"
	"github.com/fluxcd/flux-mirror/internal/oci"
	"github.com/fluxcd/flux-mirror/internal/selector"
	"github.com/fluxcd/flux-mirror/internal/sync"
)

// Options configures the mirror behavior across all entries.
type Options struct {
	// Overwrite forces overwrite=true on every entry, regardless of the
	// per-entry config. This is the global --overwrite flag.
	Overwrite bool
	// DryRun skips the actual CopyTag/CopyReferrer calls and records what
	// would be copied in the summary as would-* outcomes.
	DryRun bool
	// Verbose toggles selector debug output (logs tags excluded by
	// regex/semver with reasons).
	Verbose bool
	// CopyJobs is the intra-copy blob parallelism passed to crane.WithJobs.
	// Most useful for large multi-arch images. Default 1 (sequential).
	CopyJobs int
	// Logger is used for warnings (drift, dry-run preview, excluded tags).
	Logger *slog.Logger
}

// New builds an EntryMirror for the given artifact entry.
func New(client *oci.Client, entry config.ArtifactEntry, opts Options) sync.EntryMirror {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.CopyJobs <= 0 {
		opts.CopyJobs = 1
	}
	return &mirror{client: client, entry: entry, opts: opts}
}

type mirror struct {
	client *oci.Client
	entry  config.ArtifactEntry
	opts   Options
}

// tagJob carries everything a per-tag job needs that was resolved at plan
// time. Pulling these into a struct keeps runTag's signature compact and
// makes the "captured at plan time" relationship explicit.
type tagJob struct {
	tag       string
	src       string
	dst       string
	srcDigest string // pre-fetched only when refs were snapshotted; otherwise ""
	refs      []oci.Referrer
	overwrite bool
}

func (m *mirror) Plan(ctx context.Context) (sync.Plan, error) {
	plan := sync.Plan{Name: m.entry.Source}

	tags, err := m.client.ListTags(ctx, m.entry.Source)
	if err != nil {
		return plan, err
	}

	sel, err := selector.Select(tags, m.entry.Selector, selector.Options{Verbose: m.opts.Verbose})
	if err != nil {
		return plan, fmt.Errorf("select tags: %w", err)
	}
	for _, ex := range sel.Excluded {
		m.opts.Logger.Debug("tag excluded by selector",
			"source", m.entry.Source, "tag", ex.Tag, "reason", ex.Reason)
	}

	overwrite := m.entry.Overwrite || m.opts.Overwrite

	plan.Jobs = make([]sync.Job, 0, len(sel.Tags))
	for _, tag := range sel.Tags {
		job := tagJob{
			tag:       tag,
			src:       m.entry.Source + ":" + tag,
			dst:       m.entry.Destination + ":" + tag,
			overwrite: overwrite,
		}

		// Snapshot referrers at plan time, not inside the retry closure —
		// this fixes the set for the duration of any per-tag retries the
		// runner does. Without this, a transient failure mid-mirror could
		// re-list and mix referrers from before/after a publish.
		if m.entry.IncludeReferrers {
			srcDigest, err := m.client.Digest(ctx, job.src)
			if err != nil {
				return plan, fmt.Errorf("resolve digest for referrers (%s): %w", job.src, err)
			}
			refs, err := m.client.SnapshotReferrers(ctx, m.entry.Source, srcDigest)
			if err != nil {
				return plan, fmt.Errorf("snapshot referrers for %s: %w", job.src, err)
			}
			job.srcDigest = srcDigest
			job.refs = refs
		}

		plan.Jobs = append(plan.Jobs, sync.Job{
			ID:  tag,
			Run: func(jctx context.Context) (sync.Outcome, error) { return m.runTag(jctx, job) },
		})
	}
	return plan, nil
}

// runTag executes one tag's mirror flow. Idempotent — safe to retry under
// the runner's per-tag budget.
func (m *mirror) runTag(ctx context.Context, j tagJob) (sync.Outcome, error) {
	start := time.Now()
	m.opts.Logger.Info("mirroring tag", "src", j.src, "dst", j.dst)

	// When refs were snapshotted at plan time, j.srcDigest is also known —
	// skip the redundant src-side Digest call.
	cmp, err := m.compareTag(ctx, j.src, j.dst, j.srcDigest)
	if err != nil {
		return "", err
	}

	oc, err := m.applyOutcome(ctx, j.src, j.dst, cmp, j.overwrite, m.opts.CopyJobs)
	if err != nil {
		return "", err
	}

	if len(j.refs) > 0 {
		if err := m.mirrorReferrers(ctx, j.refs, j.overwrite); err != nil {
			return "", err
		}
	}
	m.opts.Logger.Info("tag done",
		"src", j.src,
		"outcome", string(oc),
		"elapsed", time.Since(start).Round(time.Millisecond))
	return oc, nil
}

// compareTag does Compare or CompareWithKnownSrc depending on whether we
// already have the src digest from plan-time work.
func (m *mirror) compareTag(ctx context.Context, src, dst, knownSrcDigest string) (oci.CompareResult, error) {
	if knownSrcDigest != "" {
		return m.client.CompareWithKnownSrc(ctx, knownSrcDigest, dst)
	}
	return m.client.Compare(ctx, src, dst)
}

// applyOutcome consolidates the Compare → Copy/skip switch shared between
// the parent tag and each referrer.
func (m *mirror) applyOutcome(ctx context.Context, src, dst string, cmp oci.CompareResult, overwrite bool, jobs int) (sync.Outcome, error) {
	switch cmp.State {
	case oci.StateEqual:
		return sync.OutcomeSkipped, nil
	case oci.StateMissing:
		if m.opts.DryRun {
			return sync.OutcomeWouldCopy, nil
		}
		if err := m.client.CopyTag(ctx, src, dst, jobs); err != nil {
			return "", err
		}
		return sync.OutcomeCopied, nil
	case oci.StateDrifted:
		if !overwrite {
			m.opts.Logger.Warn("destination tag drifted from source; skipping (overwrite=false)",
				"src", src, "src_digest", cmp.SrcDigest,
				"dst", dst, "dst_digest", cmp.DstDigest)
			return sync.OutcomeDrifted, nil
		}
		if m.opts.DryRun {
			return sync.OutcomeWouldOverwrite, nil
		}
		if err := m.client.CopyTag(ctx, src, dst, jobs); err != nil {
			return "", err
		}
		return sync.OutcomeOverwritten, nil
	}
	return "", fmt.Errorf("compare returned unknown state %d", cmp.State)
}

// mirrorReferrers walks the snapshotted referrer set and applies the same
// Compare → Copy gate as the parent tag. The src digest of each referrer is
// the referrer's own digest (we know it without a network call), so we use
// CompareWithKnownSrc to skip the redundant src-side fetch per referrer.
func (m *mirror) mirrorReferrers(ctx context.Context, refs []oci.Referrer, overwrite bool) error {
	for _, r := range refs {
		dstRef := m.entry.Destination + "@" + r.Digest
		cmp, err := m.client.CompareWithKnownSrc(ctx, r.Digest, dstRef)
		if err != nil {
			return fmt.Errorf("compare referrer %s: %w", r.Digest, err)
		}
		switch cmp.State {
		case oci.StateEqual:
			continue
		case oci.StateMissing, oci.StateDrifted:
			if cmp.State == oci.StateDrifted && !overwrite {
				m.opts.Logger.Warn("destination referrer drifted; skipping (overwrite=false)",
					"src_repo", m.entry.Source, "dst_repo", m.entry.Destination,
					"digest", r.Digest, "dst_digest", cmp.DstDigest)
				continue
			}
			if m.opts.DryRun {
				m.opts.Logger.Info("would copy referrer (dry-run)",
					"src_repo", m.entry.Source, "dst_repo", m.entry.Destination,
					"digest", r.Digest, "state", cmp.State.String())
				continue
			}
			if err := m.client.CopyReferrer(ctx, m.entry.Source, m.entry.Destination, r.Digest); err != nil {
				return err
			}
		}
	}
	return nil
}
