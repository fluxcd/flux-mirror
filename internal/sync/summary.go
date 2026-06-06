// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

// Result is the aggregate output of Runner.Run. Duration is the total wall
// time the run took (set by the runner) and is excluded from the per-entry
// JSON/YAML output — the structured Report (see NewReport) surfaces it as a
// plain millisecond integer instead, since a Go time.Duration would marshal
// as opaque nanoseconds.
type Result struct {
	Entries  []EntryResult `json:"entries"`
	Duration time.Duration `json:"-"`
}

// EntryStatus is the entry-level outcome. It answers "was the entry planned
// and run, or did it abort at plan time" — distinct from the per-tag Status.
type EntryStatus string

const (
	// EntryCompleted means the entry produced a plan and ran. Its Tags may be
	// empty (nothing matched the selector) or contain StatusFailed rows
	// (per-tag problems) — neither makes the entry itself a failure.
	EntryCompleted EntryStatus = "completed"
	// EntryFailed means the entry could not be planned (e.g. ListTags
	// rejected). Error carries the plan-time message and Tags is empty.
	EntryFailed EntryStatus = "failed"
)

// EntryResult is the per-entry slice of a Result. Each tag job becomes a
// TagResult row carrying its own status, digest, skip reason, error, and
// referrers — so verification and per-tag detail are expressible without a
// side channel. Status disambiguates an empty Tags array (nothing matched vs
// plan failed).
type EntryResult struct {
	Name   string      `json:"name"`
	Status EntryStatus `json:"status"`
	Error  string      `json:"error,omitempty"`
	Tags   []TagResult `json:"tags"`
}

// TagResult is one tag's row in the report. Optional fields follow the wire
// contract: digest when resolved; reason only on skipped; error only on
// failed; referrers only when includeReferrers surfaced any; verification only
// when a signature was confirmed.
type TagResult struct {
	Tag          string           `json:"tag"`
	Status       Status           `json:"status"`
	Digest       string           `json:"digest,omitempty"`
	Reason       string           `json:"reason,omitempty"`
	Error        string           `json:"error,omitempty"`
	Referrers    []ReferrerResult `json:"referrers,omitempty"`
	Verification *Verification    `json:"verification,omitempty"`
}

// ReferrerResult is one mirrored referrer (cosign signature bundle, SBOM,
// attestation) of a tag. It reuses the per-tag Status enum; Reason is set to
// up-to-date on a skipped referrer (same always-on-skip rule as tags).
type ReferrerResult struct {
	Digest       string `json:"digest"`
	ArtifactType string `json:"artifactType,omitempty"`
	Status       Status `json:"status"`
	Reason       string `json:"reason,omitempty"`
}

// Verification is the signature verification metadata recorded on a tag row.
// Only Provider is guaranteed; everything else is best-effort and omitted when
// empty: Issuer/Identity are set when the keyless cert identity is recovered;
// IntegratedTime only when the bundle carries a verified transparency-log
// timestamp; Age/MinAge only on the signature-too-new skip.
type Verification struct {
	Provider       string `json:"provider"`
	Issuer         string `json:"issuer,omitempty"`
	Identity       string `json:"identity,omitempty"`
	IntegratedTime string `json:"integratedTime,omitempty"`
	Age            string `json:"age,omitempty"`
	MinAge         string `json:"minAge,omitempty"`
}

// HasFailures reports whether any tag job failed across all entries.
func (r Result) HasFailures() bool {
	return r.TotalFailures() > 0
}

// TotalFailures sums per-tag StatusFailed rows plus plan-failed entries.
func (r Result) TotalFailures() int {
	n := 0
	for _, e := range r.Entries {
		n += e.failedCount()
	}
	return n
}

// TotalDrifted sums per-tag drift counts.
func (r Result) TotalDrifted() int {
	n := 0
	for _, e := range r.Entries {
		for _, t := range e.Tags {
			if t.Status == StatusDrifted {
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
	totals := map[Status]int{}
	for _, e := range r.Entries {
		logger.Info("entry summary", entryAttrs(e)...)
		for st, n := range e.statusCounts() {
			if st == StatusFailed {
				continue
			}
			totals[st] += n
		}
	}
	logger.Info("sync complete", totalAttrs(len(r.Entries), totals, r.TotalFailures(), r.Duration)...)
}

// statusCounts tallies non-failed tag rows by status across the whole result.
func (r Result) statusCounts() map[Status]int {
	out := map[Status]int{}
	for _, e := range r.Entries {
		for st, n := range e.statusCounts() {
			if st == StatusFailed {
				continue
			}
			out[st] += n
		}
	}
	return out
}

// statusCounts tallies this entry's tag rows by status (including StatusFailed).
func (e EntryResult) statusCounts() map[Status]int {
	counts := map[Status]int{}
	for _, t := range e.Tags {
		counts[t.Status]++
	}
	return counts
}

// failedCount is the entry's contribution to the failure total: its
// StatusFailed tag rows, plus one when the entry itself failed at plan time.
func (e EntryResult) failedCount() int {
	n := e.statusCounts()[StatusFailed]
	if e.Status == EntryFailed {
		n++
	}
	return n
}

// summaryOrder pins the key order so log lines are stable for grep/diff.
// would-* keys are appended only when present (dry-run only).
var summaryOrder = []Status{
	StatusCopied,
	StatusOverwritten,
	StatusSkipped,
	StatusDrifted,
}

// prettyPrintOrder controls the order statuses appear in the Summary
// block of PrettyPrint: action-y statuses first, drift last, dry-run
// forecasts appended.
var prettyPrintOrder = []Status{
	StatusCopied,
	StatusOverwritten,
	StatusSkipped,
	StatusDrifted,
	StatusWouldCopy,
	StatusWouldOverwrite,
}

func entryAttrs(e EntryResult) []any {
	counts := e.statusCounts()
	attrs := []any{"name", e.Name}
	for _, st := range summaryOrder {
		attrs = append(attrs, string(st), counts[st])
	}
	if n := counts[StatusWouldCopy]; n > 0 {
		attrs = append(attrs, string(StatusWouldCopy), n)
	}
	if n := counts[StatusWouldOverwrite]; n > 0 {
		attrs = append(attrs, string(StatusWouldOverwrite), n)
	}
	attrs = append(attrs, "failed", e.failedCount())
	return attrs
}

func totalAttrs(entries int, totals map[Status]int, failed int, duration time.Duration) []any {
	attrs := []any{"entries", entries}
	for _, st := range summaryOrder {
		attrs = append(attrs, string(st), totals[st])
	}
	if n := totals[StatusWouldCopy]; n > 0 {
		attrs = append(attrs, string(StatusWouldCopy), n)
	}
	if n := totals[StatusWouldOverwrite]; n > 0 {
		attrs = append(attrs, string(StatusWouldOverwrite), n)
	}
	attrs = append(attrs, "failed", failed)
	if duration > 0 {
		attrs = append(attrs, "duration", duration)
	}
	return attrs
}
