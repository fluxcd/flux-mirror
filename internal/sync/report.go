// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"sigs.k8s.io/yaml"
)

// ReportVersion pins the wire format version emitted by NewReport.
const ReportVersion = "1.0.0"

// ReportSchema is the canonical URL of the JSON Schema describing the Report shape.
const ReportSchema = "https://raw.githubusercontent.com/fluxcd/flux-mirror/main/docs/report/schema-1.0.0.json"

// Report is the envelope emitted when `flux-mirror sync` runs with
// --output json or --output yaml. It wraps the per-entry Result with run
// metadata (reporter, timestamp, duration) and an aggregate summary.
type Report struct {
	Version string     `json:"version"`
	Schema  string     `json:"$schema,omitempty"`
	Report  ReportBody `json:"report"`
}

// ReportBody holds the run metadata and results. DurationMs is the total wall
// time in milliseconds — milliseconds rather than a Go time.Duration so it
// marshals as a plain integer instead of opaque nanoseconds.
type ReportBody struct {
	Reporter   string        `json:"reporter"`
	Timestamp  string        `json:"timestamp"`
	DurationMs int64         `json:"durationMs"`
	Summary    ReportSummary `json:"summary"`
	Results    []EntryResult `json:"results"`
}

// ReportSummary aggregates per-outcome tag counts across all entries. Every
// field is always present (zero when unused) so consumers can rely on a stable
// key set. ExitCode is the semantic sync exit code (0 clean, 1 failures,
// 2 drift) and is not affected by the --drift-exit-code override.
type ReportSummary struct {
	Entries        int `json:"entries"`
	Copied         int `json:"copied"`
	Overwritten    int `json:"overwritten"`
	Skipped        int `json:"skipped"`
	Drifted        int `json:"drifted"`
	WouldCopy      int `json:"wouldCopy"`
	WouldOverwrite int `json:"wouldOverwrite"`
	Failed         int `json:"failed"`
	ExitCode       int `json:"exitCode"`
}

// NewReport assembles a Report from a Result. The caller owns the reporter
// string (typically "flux-mirror/"+VERSION) and the timestamp so tests can
// pin both.
func NewReport(reporter string, timestamp time.Time, r Result) Report {
	counts := r.statusCounts()
	results := r.Entries
	if results == nil {
		results = []EntryResult{}
	}
	return Report{
		Version: ReportVersion,
		Schema:  ReportSchema,
		Report: ReportBody{
			Reporter:   reporter,
			Timestamp:  timestamp.UTC().Format(time.RFC3339),
			DurationMs: r.Duration.Milliseconds(),
			Summary: ReportSummary{
				Entries:        len(r.Entries),
				Copied:         counts[StatusCopied],
				Overwritten:    counts[StatusOverwritten],
				Skipped:        counts[StatusSkipped],
				Drifted:        counts[StatusDrifted],
				WouldCopy:      counts[StatusWouldCopy],
				WouldOverwrite: counts[StatusWouldOverwrite],
				Failed:         r.TotalFailures(),
				ExitCode:       r.ExitCode(),
			},
			Results: results,
		},
	}
}

// Render writes the report to w in the requested structured format.
// Only "yaml" and "json" are supported here; the human-readable text path
// flows through Result.LogSummary / Result.PrettyPrint instead.
//
// The $schema key is JSON-only: it points at a JSON Schema document and
// carries no meaning for YAML consumers, so it is dropped in YAML mode.
func (rep Report) Render(w io.Writer, format string) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	case "yaml":
		rep.Schema = ""
		out, err := yaml.Marshal(rep)
		if err != nil {
			return err
		}
		_, err = w.Write(out)
		return err
	default:
		return fmt.Errorf("unsupported output format %q", format)
	}
}
