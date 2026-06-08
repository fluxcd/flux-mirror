// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta1"
)

// Result is the aggregate output of Runner.Run. Duration is the total wall
// time the run took (set by the runner) and is excluded from the per-entry
// JSON/YAML output — the structured Report (see NewReport) surfaces it as a
// plain millisecond integer instead, since a Go time.Duration would marshal
// as opaque nanoseconds.
type Result struct {
	Entries  []apiv1.EntryResult `json:"entries"`
	Duration time.Duration       `json:"-"`
}

// HasFailures reports whether any tag job failed across all entries.
func (r Result) HasFailures() bool {
	return r.TotalFailures() > 0
}

// TotalFailures sums per-tag StatusFailed rows plus plan-failed entries.
func (r Result) TotalFailures() int {
	n := 0
	for _, e := range r.Entries {
		n += entryFailedCount(e)
	}
	return n
}

// TotalDrifted sums per-tag drift counts.
func (r Result) TotalDrifted() int {
	n := 0
	for _, e := range r.Entries {
		for _, t := range e.Tags {
			if t.Status == apiv1.StatusDrifted {
				n++
			}
		}
	}
	return n
}

// HasDrift reports whether any tag was drifted (different digest, no
// overwrite gate set).
func (r Result) HasDrift() bool {
	return r.TotalDrifted() > 0
}

// ExitCode follows the documented convention:
//
//	0 — clean
//	1 — at least one tag failed
//	2 — no failures but at least one drift detected
//
// Failures take precedence over drift (a real error is always page-worthy).
func (r Result) ExitCode() int {
	if r.HasFailures() {
		return 1
	}
	if r.HasDrift() {
		return 2
	}
	return 0
}

// PrettyPrint writes a one-line totals summary to w. Used in pretty-print
// (non-verbose, text-output) mode, below the per-tag completion lines —
// which already enumerate the tags, so the summary itself stays tight.
// Zero-valued status buckets are omitted; an empty run renders as
// "Summary: nothing to mirror.".
func (r Result) PrettyPrint(w io.Writer) error {
	counts := r.statusCounts()
	failed := r.TotalFailures()

	parts := make([]string, 0, len(prettyPrintOrder)+1)
	for _, st := range prettyPrintOrder {
		if n := counts[st]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, st))
		}
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	var line string
	switch {
	case len(parts) == 0 && r.Duration > 0:
		line = fmt.Sprintf("Summary: nothing to mirror in %s.\n", r.Duration)
	case len(parts) == 0:
		line = "Summary: nothing to mirror.\n"
	case r.Duration > 0:
		line = fmt.Sprintf("Summary: %s in %s.\n", strings.Join(parts, ", "), r.Duration)
	default:
		line = "Summary: " + strings.Join(parts, ", ") + ".\n"
	}
	_, err := io.WriteString(w, line)
	return err
}

// LogSummary emits one "entry summary" line per entry plus a final
// "sync complete" total, all through the supplied logger so they share
// the same timestamp/format as the per-tag progress logs above them.
func (r Result) LogSummary(logger *slog.Logger) {
	totals := map[apiv1.Status]int{}
	for _, e := range r.Entries {
		logger.Info("entry summary", entryAttrs(e)...)
		for st, n := range entryStatusCounts(e) {
			if st == apiv1.StatusFailed {
				continue
			}
			totals[st] += n
		}
	}
	logger.Info("sync complete", totalAttrs(len(r.Entries), totals, r.TotalFailures(), r.Duration)...)
}

// statusCounts tallies non-failed tag rows by status across the whole result.
func (r Result) statusCounts() map[apiv1.Status]int {
	out := map[apiv1.Status]int{}
	for _, e := range r.Entries {
		for st, n := range entryStatusCounts(e) {
			if st == apiv1.StatusFailed {
				continue
			}
			out[st] += n
		}
	}
	return out
}

// entryStatusCounts tallies an entry's tag rows by status (including StatusFailed).
func entryStatusCounts(e apiv1.EntryResult) map[apiv1.Status]int {
	counts := map[apiv1.Status]int{}
	for _, t := range e.Tags {
		counts[t.Status]++
	}
	return counts
}

// entryFailedCount is the entry's contribution to the failure total: its
// StatusFailed tag rows, plus one when the entry itself failed at plan time.
func entryFailedCount(e apiv1.EntryResult) int {
	n := entryStatusCounts(e)[apiv1.StatusFailed]
	if e.Status == apiv1.EntryFailed {
		n++
	}
	return n
}

// summaryOrder pins the key order so log lines are stable for grep/diff.
// would-* keys are appended only when present (dry-run only).
var summaryOrder = []apiv1.Status{
	apiv1.StatusCopied,
	apiv1.StatusOverwritten,
	apiv1.StatusSkipped,
	apiv1.StatusDrifted,
}

// prettyPrintOrder controls the order statuses appear in the Summary
// block of PrettyPrint: action-y statuses first, drift last, dry-run
// forecasts appended.
var prettyPrintOrder = []apiv1.Status{
	apiv1.StatusCopied,
	apiv1.StatusOverwritten,
	apiv1.StatusSkipped,
	apiv1.StatusDrifted,
	apiv1.StatusWouldCopy,
	apiv1.StatusWouldOverwrite,
}

func entryAttrs(e apiv1.EntryResult) []any {
	counts := entryStatusCounts(e)
	attrs := []any{"source", e.Source}
	for _, st := range summaryOrder {
		attrs = append(attrs, string(st), counts[st])
	}
	if n := counts[apiv1.StatusWouldCopy]; n > 0 {
		attrs = append(attrs, string(apiv1.StatusWouldCopy), n)
	}
	if n := counts[apiv1.StatusWouldOverwrite]; n > 0 {
		attrs = append(attrs, string(apiv1.StatusWouldOverwrite), n)
	}
	attrs = append(attrs, "failed", entryFailedCount(e))
	return attrs
}

func totalAttrs(entries int, totals map[apiv1.Status]int, failed int, duration time.Duration) []any {
	attrs := []any{"entries", entries}
	for _, st := range summaryOrder {
		attrs = append(attrs, string(st), totals[st])
	}
	if n := totals[apiv1.StatusWouldCopy]; n > 0 {
		attrs = append(attrs, string(apiv1.StatusWouldCopy), n)
	}
	if n := totals[apiv1.StatusWouldOverwrite]; n > 0 {
		attrs = append(attrs, string(apiv1.StatusWouldOverwrite), n)
	}
	attrs = append(attrs, "failed", failed)
	if duration > 0 {
		attrs = append(attrs, "duration", duration)
	}
	return attrs
}
