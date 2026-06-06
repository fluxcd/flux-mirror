// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"context"
	"errors"
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
	// Verifier verifies source artifact signatures when entry.verify is set.
	Verifier Verifier
}

// Verifier checks a selected source reference before it is mirrored. On
// success it returns the confirmed signature metadata so the report can record
// the verified identity; a *oci.SignatureTooNewError is returned alongside the
// info when the signature is valid but younger than the configured minAge.
type Verifier interface {
	Verify(ctx context.Context, ref string, cfg config.ArtifactVerification) (*oci.VerificationInfo, error)
}

// New builds an EntryMirror for the given artifact entry.
func New(client *oci.Client, entry config.ArtifactEntry, opts Options) sync.EntryMirror {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.CopyJobs <= 0 {
		opts.CopyJobs = 1
	}
	if opts.Verifier == nil {
		opts.Verifier = oci.NewVerifier(client)
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
	tag          string
	src          string
	dst          string
	srcDigest    string // pre-fetched only when refs were snapshotted; otherwise ""
	refs         []oci.Referrer
	overwrite    bool
	verification *sync.Verification // confirmed at plan time when entry.verify is set
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

		if m.entry.Verify != nil {
			info, err := m.opts.Verifier.Verify(ctx, job.src, *m.entry.Verify)
			if err != nil {
				var tooNew *oci.SignatureTooNewError
				if errors.As(err, &tooNew) {
					m.opts.Logger.Info("signature too new; skipping tag",
						"src", job.src,
						"integrated_time", tooNew.IntegratedTime.Format(time.RFC3339),
						"age", tooNew.Age.Round(time.Second),
						"min_age", tooNew.MinAge)
					ver := toVerification(info)
					if ver == nil {
						ver = &sync.Verification{}
					}
					ver.Age = tooNew.Age.Round(time.Second).String()
					ver.MinAge = tooNew.MinAge.String()
					skipDigest := ""
					if info != nil {
						skipDigest = info.Digest
					}
					plan.Jobs = append(plan.Jobs, sync.Job{
						ID:           tag,
						Dst:          job.dst,
						Verification: ver,
						Run: func(context.Context) (sync.JobResult, error) {
							return sync.JobResult{
								Status: sync.StatusSkipped,
								Reason: sync.ReasonSignatureTooNew,
								Digest: skipDigest,
							}, nil
						},
					})
					continue
				}
				// Any other verify failure becomes a failed tag row carrying the
				// error; planning stops after it (tags verified earlier still
				// run, later tags are not attempted). Unlike a plan-time error,
				// this keeps the entry status "completed".
				plan.Jobs = append(plan.Jobs, sync.Job{
					ID:        tag,
					Dst:       job.dst,
					PlanError: fmt.Errorf("verify %s: %w", job.src, err),
				})
				break
			}
			job.verification = toVerification(info)
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
			ID:           tag,
			Dst:          job.dst,
			Verification: job.verification,
			Run:          func(jctx context.Context) (sync.JobResult, error) { return m.runTag(jctx, job) },
		})
	}
	return plan, nil
}

// runTag executes one tag's mirror flow. Idempotent — safe to retry under
// the runner's per-tag budget.
func (m *mirror) runTag(ctx context.Context, j tagJob) (sync.JobResult, error) {
	start := time.Now()
	m.opts.Logger.Info("mirroring tag", "src", j.src, "dst", j.dst)

	// When refs were snapshotted at plan time, j.srcDigest is also known —
	// skip the redundant src-side Digest call.
	cmp, err := m.compareTag(ctx, j.src, j.dst, j.srcDigest)
	if err != nil {
		return sync.JobResult{}, err
	}

	st, reason, err := m.applyOutcome(ctx, j.src, j.dst, cmp, j.overwrite, m.opts.CopyJobs)
	if err != nil {
		// Compare resolved the source digest before the copy failed — keep it
		// on the failed row.
		return sync.JobResult{Digest: cmp.SrcDigest}, err
	}

	var refResults []sync.ReferrerResult
	if len(j.refs) > 0 {
		refResults, err = m.mirrorReferrers(ctx, j.refs, j.overwrite)
		if err != nil {
			// A referrer failure fails the parent tag; carry the digest and the
			// partial referrer list (including the failed one) on the row.
			return sync.JobResult{Digest: cmp.SrcDigest, Referrers: refResults}, err
		}
	}
	m.opts.Logger.Info("tag done",
		"src", j.src,
		"status", string(st),
		"elapsed", time.Since(start).Round(time.Millisecond))
	return sync.JobResult{Status: st, Digest: cmp.SrcDigest, Reason: reason, Referrers: refResults}, nil
}

// compareTag does Compare or CompareWithKnownSrc depending on whether we
// already have the src digest from plan-time work.
func (m *mirror) compareTag(ctx context.Context, src, dst, knownSrcDigest string) (oci.CompareResult, error) {
	if knownSrcDigest != "" {
		return m.client.CompareWithKnownSrc(ctx, knownSrcDigest, dst)
	}
	return m.client.Compare(ctx, src, dst)
}

// resolveCompare maps a Compare state to a status + skip reason, running copy
// for the StateMissing/StateDrifted cases unless dry-run. copy performs the
// actual mirror; onDrift logs the overwrite=false skip. Shared by the parent
// tag and each referrer so the status mapping lives in one place.
func (m *mirror) resolveCompare(cmp oci.CompareResult, overwrite bool, copyFn func() error, onDrift func()) (sync.Status, string, error) {
	switch cmp.State {
	case oci.StateEqual:
		return sync.StatusSkipped, sync.ReasonUpToDate, nil
	case oci.StateMissing:
		if m.opts.DryRun {
			return sync.StatusWouldCopy, "", nil
		}
		if err := copyFn(); err != nil {
			return "", "", err
		}
		return sync.StatusCopied, "", nil
	case oci.StateDrifted:
		if !overwrite {
			onDrift()
			return sync.StatusDrifted, "", nil
		}
		if m.opts.DryRun {
			return sync.StatusWouldOverwrite, "", nil
		}
		if err := copyFn(); err != nil {
			return "", "", err
		}
		return sync.StatusOverwritten, "", nil
	}
	return "", "", fmt.Errorf("compare returned unknown state %d", cmp.State)
}

// applyOutcome resolves the Compare → Copy/skip switch for the parent tag.
func (m *mirror) applyOutcome(ctx context.Context, src, dst string, cmp oci.CompareResult, overwrite bool, jobs int) (sync.Status, string, error) {
	return m.resolveCompare(cmp, overwrite,
		func() error { return m.client.CopyTag(ctx, src, dst, jobs) },
		func() {
			m.opts.Logger.Warn("destination tag drifted from source; skipping (overwrite=false)",
				"src", src, "src_digest", cmp.SrcDigest,
				"dst", dst, "dst_digest", cmp.DstDigest)
		})
}

// mirrorReferrers walks the snapshotted referrer set and applies the same
// Compare → Copy gate as the parent tag, collecting a per-referrer result row.
// The src digest of each referrer is the referrer's own digest (we know it
// without a network call), so we use CompareWithKnownSrc to skip the redundant
// src-side fetch per referrer. On the first referrer error it appends a failed
// row for that referrer and returns the rows collected so far alongside the
// error (a referrer failure fails the parent tag).
func (m *mirror) mirrorReferrers(ctx context.Context, refs []oci.Referrer, overwrite bool) ([]sync.ReferrerResult, error) {
	results := make([]sync.ReferrerResult, 0, len(refs))
	for _, r := range refs {
		dstRef := m.entry.Destination + "@" + r.Digest
		cmp, err := m.client.CompareWithKnownSrc(ctx, r.Digest, dstRef)
		if err != nil {
			results = append(results, sync.ReferrerResult{
				Digest: r.Digest, ArtifactType: r.ArtifactType, Status: sync.StatusFailed,
			})
			return results, fmt.Errorf("compare referrer %s: %w", r.Digest, err)
		}
		st, reason, err := m.applyReferrerOutcome(ctx, r, cmp, overwrite)
		if err != nil {
			results = append(results, sync.ReferrerResult{
				Digest: r.Digest, ArtifactType: r.ArtifactType, Status: sync.StatusFailed,
			})
			return results, err
		}
		results = append(results, sync.ReferrerResult{
			Digest: r.Digest, ArtifactType: r.ArtifactType, Status: st, Reason: reason,
		})
	}
	return results, nil
}

// applyReferrerOutcome resolves one referrer's compare state, copying via
// CopyReferrer. Mirrors applyOutcome through the shared resolveCompare switch.
func (m *mirror) applyReferrerOutcome(ctx context.Context, r oci.Referrer, cmp oci.CompareResult, overwrite bool) (sync.Status, string, error) {
	return m.resolveCompare(cmp, overwrite,
		func() error { return m.client.CopyReferrer(ctx, m.entry.Source, m.entry.Destination, r.Digest) },
		func() {
			m.opts.Logger.Warn("destination referrer drifted; skipping (overwrite=false)",
				"src_repo", m.entry.Source, "dst_repo", m.entry.Destination,
				"digest", r.Digest, "dst_digest", cmp.DstDigest)
		})
}

// toVerification converts the oci-layer verification info into the report's
// wire type, formatting the integrated time as RFC 3339 (omitted when zero).
// Returns nil when info is nil (no verify configured / nothing confirmed).
func toVerification(info *oci.VerificationInfo) *sync.Verification {
	if info == nil {
		return nil
	}
	v := &sync.Verification{
		Provider: info.Provider,
		Issuer:   info.Issuer,
		Identity: info.Identity,
	}
	if !info.IntegratedTime.IsZero() {
		v.IntegratedTime = info.IntegratedTime.UTC().Format(time.RFC3339)
	}
	return v
}
