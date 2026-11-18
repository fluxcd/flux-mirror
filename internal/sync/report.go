// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta2"
)

// NewReport assembles a Report from a Result. The caller owns the reporter
// string (typically "flux-mirror/"+VERSION) and the timestamp so tests can
// pin both.
func NewReport(reporter string, timestamp time.Time, r Result) apiv1.Report {
	counts := r.statusCounts()
	results := r.Entries
	if results == nil {
		results = []apiv1.EntryResult{}
	}
	return apiv1.Report{
		APIVersion: apiv1.GroupVersion.String(),
		Kind:       apiv1.ReportKind,
		Schema:     apiv1.ReportSchema,
		Report: apiv1.ReportSpec{
			Reporter:   reporter,
			Timestamp:  timestamp.UTC().Format(time.RFC3339),
			DurationMs: r.Duration.Milliseconds(),
			Summary: apiv1.ReportSummary{
				Entries:        len(r.Entries),
				Copied:         counts[apiv1.StatusCopied],
				Overwritten:    counts[apiv1.StatusOverwritten],
				Skipped:        counts[apiv1.StatusSkipped],
				Drifted:        counts[apiv1.StatusDrifted],
				WouldCopy:      counts[apiv1.StatusWouldCopy],
				WouldOverwrite: counts[apiv1.StatusWouldOverwrite],
				Failed:         r.TotalFailures(),
				ExitCode:       r.ExitCode(),
			},
			Results: results,
		},
	}
}

// RenderReport writes the report to w in the requested structured format.
// Only "yaml" and "json" are supported here; the human-readable text path
// flows through Result.LogSummary / Result.PrettyPrint instead.
//
// The $schema key is JSON-only: it points at a JSON Schema document and
// carries no meaning for YAML consumers, so it is dropped in YAML mode.
func RenderReport(w io.Writer, format string, rep apiv1.Report) error {
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

// LogEntry emits one "entry" record for the entry result (attribute "entry").
// Entries that failed, or contain a failed tag, are logged at error level.
// With a JSON handler this yields one line of the jsonl output format.
func LogEntry(logger *slog.Logger, e apiv1.EntryResult) {
	level := slog.LevelInfo
	if entryHasFailure(e) {
		level = slog.LevelError
	}
	logger.LogAttrs(context.Background(), level, "entry", slog.Any("entry", e))
}

// LogReportSummary emits the final "summary" record carrying the run
// metadata and aggregate counts. It is error level when any job failed and
// warn level on drift.
func LogReportSummary(logger *slog.Logger, rep apiv1.Report) {
	level := slog.LevelInfo
	switch {
	case rep.Report.Summary.Failed > 0:
		level = slog.LevelError
	case rep.Report.Summary.Drifted > 0:
		level = slog.LevelWarn
	}
	logger.LogAttrs(context.Background(), level, "summary",
		slog.String("reporter", rep.Report.Reporter),
		slog.String("reportTimestamp", rep.Report.Timestamp),
		slog.Int64("durationMs", rep.Report.DurationMs),
		slog.Any("summary", rep.Report.Summary),
	)
}
