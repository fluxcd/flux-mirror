// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta2"
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
	// PlanConcurrency is the maximum number of tag-planning operations to run
	// in parallel per entry. Planning resolves each tag's digest and snapshots
	// its referrers, which is one or two API calls per tag. Default 1
	// (sequential).
	PlanConcurrency int
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
	Verify(ctx context.Context, ref string, cfg apiv1.ArtifactVerification) (*oci.VerificationInfo, error)
}

// New builds an EntryMirror for the given artifact entry.
func New(client *oci.Client, entry apiv1.ArtifactEntry, opts Options) sync.EntryMirror {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.CopyJobs <= 0 {
		opts.CopyJobs = 1
	}
	if opts.PlanConcurrency <= 0 {
		opts.PlanConcurrency = 1
	}
	if opts.Verifier == nil {
		opts.Verifier = oci.NewVerifier(client)
	}
	return &mirror{client: client, entry: entry, opts: opts}
}

type mirror struct {
	client *oci.Client
	entry  apiv1.ArtifactEntry
	opts   Options
}

// tagJob carries everything a per-tag job needs that was resolved at plan
// time. Pulling these into a struct keeps runTag's signature compact and
// makes the "captured at plan time" relationship explicit.
type tagJob struct {
	tag          string
	src          string
	dst          string
	srcDigest    string // pre-fetched when verified or when refs were snapshotted; otherwise ""
	refs         []oci.Referrer
	overwrite    bool
	verification *apiv1.Verification // confirmed at plan time when entry.verify is set
}

func (m *mirror) Plan(ctx context.Context) (sync.Plan, error) {
	plan := sync.Plan{
		Source:                 m.entry.Source,
		Destination:            m.entry.Destination,
		IgnoreDriftForExitCode: m.entry.IgnoreDriftForExitCode,
	}
	start := time.Now()

	m.opts.Logger.Info("planning tags", "source", m.entry.Source, "destination", m.entry.Destination)
	sel, err := m.selectTags(ctx)
	if err != nil {
		return plan, err
	}
	for _, ex := range sel.Excluded {
		m.opts.Logger.Debug("tag excluded by selector",
			"source", m.entry.Source, "tag", ex.Tag, "reason", ex.Reason)
	}

	overwrite := m.entry.Overwrite || m.opts.Overwrite

	m.opts.Logger.Info("tags selected",
		"source", m.entry.Source,
		"tags", len(sel.Tags),
		"includeReferrers", m.entry.IncludeReferrers,
		"elapsed", time.Since(start).Round(time.Millisecond))

	jobs := make([]*tagJob, len(sel.Tags))
	overrides := make([]*sync.Job, len(sel.Tags))
	limit := len(sel.Tags)

	// Verification is sequential: its failure semantics decide which later
	// tags are planned at all, so parallelising it would change behavior.
	for i, tag := range sel.Tags {
		job := &tagJob{
			tag:       tag,
			src:       m.entry.Source + ":" + tag,
			dst:       m.entry.Destination + ":" + tag,
			overwrite: overwrite,
		}
		jobs[i] = job

		if m.entry.Verify == nil {
			continue
		}
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
					ver = &apiv1.Verification{}
				}
				ver.Age = tooNew.Age.Round(time.Second).String()
				ver.MinAge = tooNew.MinAge.String()
				skipDigest := ""
				if info != nil {
					skipDigest = info.Digest
				}
				overrides[i] = &sync.Job{
					ID:           tag,
					Dst:          job.dst,
					Verification: ver,
					Run: func(context.Context) (sync.JobResult, error) {
						return sync.JobResult{
							Status: apiv1.StatusSkipped,
							Reason: apiv1.ReasonSignatureTooNew,
							Digest: skipDigest,
						}, nil
					},
				}
				jobs[i] = nil
				continue
			}
			// Any other verify failure becomes a failed tag row carrying the
			// error; planning stops after it (tags verified earlier still
			// run, later tags are not attempted). Unlike a plan-time error,
			// this keeps the entry status "completed".
			overrides[i] = &sync.Job{
				ID:        tag,
				Dst:       job.dst,
				PlanError: fmt.Errorf("verify %s: %w", job.src, err),
			}
			jobs[i] = nil
			limit = i + 1
			break
		}
		if info == nil || info.Digest == "" {
			return plan, fmt.Errorf("verify %s: verifier returned no digest", job.src)
		}
		job.srcDigest = info.Digest
		job.verification = toVerification(info)
	}

	// Snapshot referrers at plan time, not inside the retry closure — this
	// fixes the set for the duration of any per-tag retries the runner does.
	// Digest + referrer resolution dominates planning, so run it with the
	// configured plan concurrency.
	if m.entry.IncludeReferrers {
		total := 0
		for i := 0; i < limit; i++ {
			if jobs[i] != nil {
				total++
			}
		}
		m.opts.Logger.Info("planning referrers",
			"source", m.entry.Source,
			"tags", total,
			"concurrency", m.opts.PlanConcurrency)

		var planned atomic.Int64
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(m.opts.PlanConcurrency)
		for i := 0; i < limit; i++ {
			job := jobs[i]
			if job == nil {
				continue
			}
			g.Go(func() error {
				if job.srcDigest == "" {
					srcDigest, err := m.client.Digest(gctx, job.src)
					if err != nil {
						return fmt.Errorf("resolve digest for referrers (%s): %w", job.src, err)
					}
					job.srcDigest = srcDigest
				}
				refs, err := m.client.SnapshotReferrers(gctx, m.entry.Source, job.srcDigest)
				if err != nil {
					return fmt.Errorf("snapshot referrers for %s: %w", job.src, err)
				}
				job.refs = refs
				done := int(planned.Add(1))
				if done%50 == 0 || done == total {
					m.opts.Logger.Info("planning progress",
						"source", m.entry.Source,
						"done", done,
						"total", total,
						"elapsed", time.Since(start).Round(time.Second))
				}
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return plan, err
		}
	}

	plan.Jobs = make([]sync.Job, 0, limit)
	for i := 0; i < limit; i++ {
		if overrides[i] != nil {
			plan.Jobs = append(plan.Jobs, *overrides[i])
			continue
		}
		job := jobs[i]
		if job == nil {
			continue
		}
		plan.Jobs = append(plan.Jobs, sync.Job{
			ID:           job.tag,
			Dst:          job.dst,
			Verification: job.verification,
			Run:          func(jctx context.Context) (sync.JobResult, error) { return m.runTag(jctx, *job) },
		})
	}
	return plan, nil
}

// selectTags returns the tags to mirror. Explicit selector tags are used
// as given, without listing the source repository.
func (m *mirror) selectTags(ctx context.Context) (selector.Result, error) {
	if len(m.entry.Selector.Tags) > 0 {
		return selector.Result{Tags: m.entry.Selector.Tags}, nil
	}
	tags, err := m.client.ListTags(ctx, m.entry.Source)
	if err != nil {
		return selector.Result{}, err
	}
	sel, err := selector.Select(tags, m.entry.Selector, selector.Options{Verbose: m.opts.Verbose})
	if err != nil {
		return selector.Result{}, fmt.Errorf("select tags: %w", err)
	}
	return sel, nil
}

// runTag executes one tag's mirror flow. Idempotent — safe to retry under
// the runner's per-tag budget.
func (m *mirror) runTag(ctx context.Context, j tagJob) (sync.JobResult, error) {
	start := time.Now()
	m.opts.Logger.Info("mirroring tag", "src", j.src, "dst", j.dst)

	// When verification or referrer snapshotting resolved the source digest at
	// plan time, use that digest as the source of truth.
	cmp, err := m.compareTag(ctx, j.src, j.dst, j.srcDigest)
	if err != nil {
		return sync.JobResult{}, err
	}

	copySrc := m.entry.Source + "@" + cmp.SrcDigest
	st, reason, err := m.applyOutcome(ctx, copySrc, j.dst, cmp, j.overwrite, m.opts.CopyJobs)
	if err != nil {
		// Compare resolved the source digest before the copy failed — keep it
		// on the failed row.
		return sync.JobResult{Digest: cmp.SrcDigest}, err
	}

	var refResults []apiv1.ReferrerResult
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
func (m *mirror) resolveCompare(cmp oci.CompareResult, overwrite bool, copyFn func() error, onDrift func()) (apiv1.Status, string, error) {
	switch cmp.State {
	case oci.StateEqual:
		return apiv1.StatusSkipped, apiv1.ReasonUpToDate, nil
	case oci.StateMissing:
		if m.opts.DryRun {
			return apiv1.StatusWouldCopy, "", nil
		}
		if err := copyFn(); err != nil {
			return "", "", err
		}
		return apiv1.StatusCopied, "", nil
	case oci.StateDrifted:
		if !overwrite {
			onDrift()
			return apiv1.StatusDrifted, "", nil
		}
		if m.opts.DryRun {
			return apiv1.StatusWouldOverwrite, "", nil
		}
		if err := copyFn(); err != nil {
			return "", "", err
		}
		return apiv1.StatusOverwritten, "", nil
	}
	return "", "", fmt.Errorf("compare returned unknown state %d", cmp.State)
}

// applyOutcome resolves the Compare → Copy/skip switch for the parent tag.
func (m *mirror) applyOutcome(ctx context.Context, src, dst string, cmp oci.CompareResult, overwrite bool, jobs int) (apiv1.Status, string, error) {
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
func (m *mirror) mirrorReferrers(ctx context.Context, refs []oci.Referrer, overwrite bool) ([]apiv1.ReferrerResult, error) {
	results := make([]apiv1.ReferrerResult, 0, len(refs))
	for _, r := range refs {
		dstRef := m.entry.Destination + "@" + r.Digest
		cmp, err := m.client.CompareWithKnownSrc(ctx, r.Digest, dstRef)
		if err != nil {
			results = append(results, apiv1.ReferrerResult{
				Digest: r.Digest, ArtifactType: r.ArtifactType, Status: apiv1.StatusFailed,
			})
			return results, fmt.Errorf("compare referrer %s: %w", r.Digest, err)
		}
		st, reason, err := m.applyReferrerOutcome(ctx, r, cmp, overwrite)
		if err != nil {
			results = append(results, apiv1.ReferrerResult{
				Digest: r.Digest, ArtifactType: r.ArtifactType, Status: apiv1.StatusFailed,
			})
			return results, err
		}
		results = append(results, apiv1.ReferrerResult{
			Digest: r.Digest, ArtifactType: r.ArtifactType, Status: st, Reason: reason,
		})
	}
	return results, nil
}

// applyReferrerOutcome resolves one referrer's compare state, copying via
// CopyReferrer. Mirrors applyOutcome through the shared resolveCompare switch.
func (m *mirror) applyReferrerOutcome(ctx context.Context, r oci.Referrer, cmp oci.CompareResult, overwrite bool) (apiv1.Status, string, error) {
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
func toVerification(info *oci.VerificationInfo) *apiv1.Verification {
	if info == nil {
		return nil
	}
	v := &apiv1.Verification{
		Provider: info.Provider,
		Issuer:   info.Issuer,
		Identity: info.Identity,
	}
	if !info.IntegratedTime.IsZero() {
		v.IntegratedTime = info.IntegratedTime.UTC().Format(time.RFC3339)
	}
	return v
}
