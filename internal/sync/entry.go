// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package sync

import "context"

// EntryMirror is the consumer-side interface the Runner uses to drive any
// kind of mirror entry (artifacts, charts, future). Entry-type specifics
// (config parsing, OCI vs Helm semantics) live in the implementing package.
//
// Plan resolves what work this entry needs to do — which tags or chart
// versions to mirror — and returns one Job per work unit. Plan runs once
// per entry; the runner executes the Jobs concurrently.
type EntryMirror interface {
	Plan(ctx context.Context) (Plan, error)
}

// Plan is the schedulable form of an EntryMirror.
type Plan struct {
	// Name identifies this entry in logs and summaries (e.g. the source repo).
	Name string
	// Jobs is the set of work units to execute, all bound to this entry.
	Jobs []Job
}

// Job is a single mirror operation (typically: copy one tag, with optional
// referrers). The closure captures whatever it needs from the entry config —
// OCI client, dry-run flag, overwrite, plus any per-job state that must be
// frozen at plan time (e.g. the referrer snapshot, which the runner would
// otherwise re-take on every retry).
type Job struct {
	// ID is a short identifier for the work unit (tag name for artifacts,
	// chart version for charts in M2). Used in summary output and log lines.
	ID string
	// Dst is the fully-qualified destination reference (`repo:tag`) for
	// this unit of work. Surfaced verbatim to OnJobFinished so the cmd
	// layer can display the "-> dst" line without re-deriving it.
	Dst string
	// Verification carries the signature verification metadata confirmed at
	// plan time (cosign keyless identity, issuer, integrated time). Nil when
	// the entry has no verify: block. Attached to the tag row at record time
	// so a verified copy is distinguishable from an unverified one.
	Verification *Verification
	// PlanError, when non-nil, marks a work unit the plan already determined
	// has failed (e.g. signature verification failed at plan time). The runner
	// records it as a StatusFailed row carrying this error and invokes neither
	// Run nor any retry budget — Run is ignored when PlanError is set.
	PlanError error
	// Run performs the work. Must be safe to invoke multiple times — the
	// runner may retry on transient errors. crane.Copy is content-addressed
	// and idempotent at the blob level, so retrying a partially-copied tag
	// does not corrupt anything.
	Run func(ctx context.Context) (JobResult, error)
}

// JobResult is the successful result of one Job.Run. It carries the per-tag
// metadata the report records as a row: the status, the resolved source
// digest, the skip reason (when skipped), and any mirrored referrers.
type JobResult struct {
	// Status is the outcome of the job (copied, skipped, drifted, …). Must be
	// one of the Run-returnable values (Valid reports this); StatusFailed is
	// runner-assigned on error and never returned here.
	Status Status
	// Digest is the source artifact digest, when the job resolved it. Present
	// for every status whose path computed it (including the too-new skip).
	Digest string
	// Reason explains a skip (up-to-date or signature-too-new). Set only when
	// Status is StatusSkipped; empty otherwise.
	Reason string
	// Referrers lists the mirrored sub-artifacts (signatures, SBOMs,
	// attestations), in snapshot order. Empty unless includeReferrers is set.
	Referrers []ReferrerResult
}

// Status is the result of one Job.Run, or runner-assigned on failure.
type Status string

const (
	StatusCopied         Status = "copied"
	StatusOverwritten    Status = "overwritten"
	StatusSkipped        Status = "skipped" // nothing was copied; see Reason
	StatusDrifted        Status = "drifted" // dst had a different digest, overwrite=false
	StatusWouldCopy      Status = "would-copy"
	StatusWouldOverwrite Status = "would-overwrite"
	// StatusFailed marks a failed mirror. The runner assigns it as a job's own
	// status when the job errors (or carries a Job.PlanError); a Job never
	// returns it as JobResult.Status, so it is excluded from validStatuses.
	// Nested ReferrerResult rows may carry it directly when a referrer fails.
	StatusFailed Status = "failed"
)

// Reason values explain why a skipped tag or referrer was skipped. Always set
// on a StatusSkipped result, absent otherwise.
const (
	ReasonUpToDate        = "up-to-date"
	ReasonSignatureTooNew = "signature-too-new"
)

var validStatuses = map[Status]struct{}{
	StatusCopied:         {},
	StatusOverwritten:    {},
	StatusSkipped:        {},
	StatusDrifted:        {},
	StatusWouldCopy:      {},
	StatusWouldOverwrite: {},
}

// Valid reports whether s is one of the documented Run-returnable statuses.
// Catches typos in entry implementations (a `return "coppied"` would otherwise
// compile and produce an unmatched status that's silently ignored by Render).
// StatusFailed is excluded by design: it is runner-assigned, not returned.
func (s Status) Valid() bool {
	_, ok := validStatuses[s]
	return ok
}
