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
	// Run performs the work. Must be safe to invoke multiple times — the
	// runner may retry on transient errors. crane.Copy is content-addressed
	// and idempotent at the blob level, so retrying a partially-copied tag
	// does not corrupt anything.
	Run func(ctx context.Context) (Outcome, error)
}

// Outcome is the result of one Job.Run.
type Outcome string

const (
	OutcomeCopied         Outcome = "copied"
	OutcomeOverwritten    Outcome = "overwritten"
	OutcomeSkipped        Outcome = "skipped" // nothing was copied
	OutcomeDrifted        Outcome = "drifted" // dst had a different digest, overwrite=false
	OutcomeWouldCopy      Outcome = "would-copy"
	OutcomeWouldOverwrite Outcome = "would-overwrite"
)

var validOutcomes = map[Outcome]struct{}{
	OutcomeCopied:         {},
	OutcomeOverwritten:    {},
	OutcomeSkipped:        {},
	OutcomeDrifted:        {},
	OutcomeWouldCopy:      {},
	OutcomeWouldOverwrite: {},
}

// Valid reports whether o is one of the documented outcomes. Catches typos
// in entry implementations (a `return "coppied"` would otherwise compile and
// produce an unmatched outcome that's silently ignored by Render).
func (o Outcome) Valid() bool {
	_, ok := validOutcomes[o]
	return ok
}
