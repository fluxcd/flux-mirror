// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"sigs.k8s.io/yaml"
)

// Result is the aggregate output of Runner.Run.
type Result struct {
	Entries []EntryResult `json:"entries"`
}

// EntryResult is the per-entry slice of a Result.
type EntryResult struct {
	Name     string          `json:"name"`
	Outcomes map[Outcome]int `json:"outcomes"`
	Failures []TagFailure    `json:"failures,omitempty"`
	Drifted  []string        `json:"drifted,omitempty"`
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
		n += len(e.Drifted)
	}
	return n
}

// HasDrift reports whether any tag was drifted (different digest, no
// overwrite gate set).
func (r Result) HasDrift() bool {
	for _, e := range r.Entries {
		if len(e.Drifted) > 0 {
			return true
		}
	}
	return false
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

// LogSummary emits one "entry summary" line per entry plus a final
// "sync complete" total, all through the supplied logger so they share
// the same timestamp/format as the per-tag progress logs above them.
func (r Result) LogSummary(logger *slog.Logger) {
	totals := map[Outcome]int{}
	totalFailed := 0
	for _, e := range r.Entries {
		logger.Info("entry summary", entryAttrs(e)...)
		for k, v := range e.Outcomes {
			totals[k] += v
		}
		totalFailed += len(e.Failures)
	}
	logger.Info("sync complete", totalAttrs(len(r.Entries), totals, totalFailed)...)
}

// summaryOrder pins the key order so log lines are stable for grep/diff.
// would-* keys are appended only when present (dry-run only).
var summaryOrder = []Outcome{
	OutcomeCopied,
	OutcomeOverwritten,
	OutcomeSkipped,
	OutcomeDrifted,
}

func entryAttrs(e EntryResult) []any {
	attrs := []any{"name", e.Name}
	for _, oc := range summaryOrder {
		attrs = append(attrs, string(oc), e.Outcomes[oc])
	}
	if n := e.Outcomes[OutcomeWouldCopy]; n > 0 {
		attrs = append(attrs, string(OutcomeWouldCopy), n)
	}
	if n := e.Outcomes[OutcomeWouldOverwrite]; n > 0 {
		attrs = append(attrs, string(OutcomeWouldOverwrite), n)
	}
	attrs = append(attrs, "failed", len(e.Failures))
	return attrs
}

func totalAttrs(entries int, totals map[Outcome]int, failed int) []any {
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
	return attrs
}
