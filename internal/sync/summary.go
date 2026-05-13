// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// Result is the aggregate output of Runner.Run. Duration is the total wall
// time the run took (set by the runner) and is excluded from the structured
// JSON/YAML output — those are meant for programmatic consumers, where a
// Go time.Duration would marshal as opaque nanoseconds.
type Result struct {
	Entries  []EntryResult `json:"entries"`
	Duration time.Duration `json:"-"`
}

// EntryResult is the per-entry slice of a Result. Outcomes maps each
// outcome to the list of tag IDs that landed in that bucket; the count is
// just `len(Outcomes[oc])`. Failures are tracked separately because they
// carry an error message alongside the tag.
type EntryResult struct {
	Name     string               `json:"name"`
	Outcomes map[Outcome][]string `json:"outcomes"`
	Failures []TagFailure         `json:"failures,omitempty"`
}

// TagFailure records a single per-tag failure with its error string.
type TagFailure struct {
	Tag string `json:"tag"`
	Err string `json:"err"`
}

// HasFailures reports whether any tag job failed across all entries.
func (r Result) HasFailures() bool {
	return r.TotalFailures() > 0
}

// TotalFailures sums per-entry failure counts.
func (r Result) TotalFailures() int {
	n := 0
	for _, e := range r.Entries {
		n += len(e.Failures)
	}
	return n
}

// TotalDrifted sums per-entry drift counts.
func (r Result) TotalDrifted() int {
	n := 0
	for _, e := range r.Entries {
		n += len(e.Outcomes[OutcomeDrifted])
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

// Render writes the result to w in the requested structured format.
// Only "yaml" and "json" are supported here; the human-readable text path
// flows through LogSummary so it shares the runner's log formatting.
func (r Result) Render(w io.Writer, format string) error {
	switch format {
	case "yaml":
		out, err := yaml.Marshal(r)
		if err != nil {
			return err
		}
		_, err = w.Write(out)
		return err
	case "json":
		out, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		if _, err := w.Write(out); err != nil {
			return err
		}
		_, err = io.WriteString(w, "\n")
		return err
	default:
		return fmt.Errorf("unsupported output format %q", format)
	}
}

// PrettyPrint writes a one-line totals summary to w. Used in pretty-print
// (non-verbose, text-output) mode, below the per-tag completion lines —
// which already enumerate the tags, so the summary itself stays tight.
// Zero-valued outcome buckets are omitted; an empty run renders as
// "Summary: nothing to mirror.".
func (r Result) PrettyPrint(w io.Writer) error {
	tagsByOutcome := r.tagsByOutcome()
	failed := r.TotalFailures()

	parts := make([]string, 0, len(prettyPrintOrder)+1)
	for _, oc := range prettyPrintOrder {
		if n := len(tagsByOutcome[oc]); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, oc))
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
	totals := map[Outcome]int{}
	totalFailed := 0
	for _, e := range r.Entries {
		logger.Info("entry summary", entryAttrs(e)...)
		for k, v := range e.Outcomes {
			totals[k] += len(v)
		}
		totalFailed += len(e.Failures)
	}
	logger.Info("sync complete", totalAttrs(len(r.Entries), totals, totalFailed, r.Duration)...)
}

// tagsByOutcome flattens per-entry tag lists into a single map across the
// whole result, preserving the order tags were recorded in.
func (r Result) tagsByOutcome() map[Outcome][]string {
	out := map[Outcome][]string{}
	for _, e := range r.Entries {
		for oc, tags := range e.Outcomes {
			out[oc] = append(out[oc], tags...)
		}
	}
	return out
}

// summaryOrder pins the key order so log lines are stable for grep/diff.
// would-* keys are appended only when present (dry-run only).
var summaryOrder = []Outcome{
	OutcomeCopied,
	OutcomeOverwritten,
	OutcomeSkipped,
	OutcomeDrifted,
}

// prettyPrintOrder controls the order outcomes appear in the Summary
// block of PrettyPrint: action-y outcomes first, drift last, dry-run
// forecasts appended.
var prettyPrintOrder = []Outcome{
	OutcomeCopied,
	OutcomeOverwritten,
	OutcomeSkipped,
	OutcomeDrifted,
	OutcomeWouldCopy,
	OutcomeWouldOverwrite,
}

func entryAttrs(e EntryResult) []any {
	attrs := []any{"name", e.Name}
	for _, oc := range summaryOrder {
		attrs = append(attrs, string(oc), len(e.Outcomes[oc]))
	}
	if n := len(e.Outcomes[OutcomeWouldCopy]); n > 0 {
		attrs = append(attrs, string(OutcomeWouldCopy), n)
	}
	if n := len(e.Outcomes[OutcomeWouldOverwrite]); n > 0 {
		attrs = append(attrs, string(OutcomeWouldOverwrite), n)
	}
	attrs = append(attrs, "failed", len(e.Failures))
	return attrs
}

func totalAttrs(entries int, totals map[Outcome]int, failed int, duration time.Duration) []any {
	attrs := []any{"entries", entries}
	for _, oc := range summaryOrder {
		attrs = append(attrs, string(oc), totals[oc])
	}
	if n := totals[OutcomeWouldCopy]; n > 0 {
		attrs = append(attrs, string(OutcomeWouldCopy), n)
	}
	if n := totals[OutcomeWouldOverwrite]; n > 0 {
		attrs = append(attrs, string(OutcomeWouldOverwrite), n)
	}
	attrs = append(attrs, "failed", failed)
	if duration > 0 {
		attrs = append(attrs, "duration", duration)
	}
	return attrs
}
