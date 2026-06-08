// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta1"
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
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1.GroupVersion.String(),
			Kind:       apiv1.ReportKind,
		},
		Schema: apiv1.ReportSchema,
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
