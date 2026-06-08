// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package v1beta1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	// ReportKind is the kind used by sync report envelopes.
	ReportKind = "Report"

	// ReportSchema is the canonical URL of the JSON Schema describing the Report shape.
	ReportSchema = "https://raw.githubusercontent.com/fluxcd/flux-mirror/main/docs/report/report-v1beta1.json"
)

// Report is the sync report envelope.
//
// +kubebuilder:object:root=true
type Report struct {
	// TypeMeta identifies the report API version and kind.
	metav1.TypeMeta `json:",inline"`

	// Schema identifies the JSON Schema for this report.
	// +optional
	Schema string `json:"$schema,omitempty"`

	// Report contains the sync report data.
	Report ReportSpec `json:"report"`
}

// ReportSpec holds the sync run metadata and results.
type ReportSpec struct {
	// Reporter identifies the software that produced the report.
	// +kubebuilder:validation:Pattern=`^flux-mirror/`
	Reporter string `json:"reporter"`

	// Timestamp is the report creation time in RFC 3339 format.
	// +kubebuilder:validation:Format=date-time
	Timestamp string `json:"timestamp"`

	// DurationMs is the total wall time of the run in milliseconds.
	// +kubebuilder:validation:Minimum=0
	DurationMs int64 `json:"durationMs"`

	// Summary contains aggregate per-outcome tag counts.
	Summary ReportSummary `json:"summary"`

	// Results contains one result per mirror entry.
	Results []EntryResult `json:"results"`
}

// ReportSummary aggregates per-outcome tag counts across all entries. Every
// field is always present (zero when unused) so consumers can rely on a stable
// key set. ExitCode is the semantic sync exit code (0 clean, 1 failures,
// 2 drift) and is not affected by the --drift-exit-code override.
type ReportSummary struct {
	// Entries is the number of mirror entries processed.
	// +kubebuilder:validation:Minimum=0
	Entries int `json:"entries"`

	// Copied is the number of tags copied.
	// +kubebuilder:validation:Minimum=0
	Copied int `json:"copied"`

	// Overwritten is the number of tags overwritten.
	// +kubebuilder:validation:Minimum=0
	Overwritten int `json:"overwritten"`

	// Skipped is the number of tags skipped.
	// +kubebuilder:validation:Minimum=0
	Skipped int `json:"skipped"`

	// Drifted is the number of tags that drifted.
	// +kubebuilder:validation:Minimum=0
	Drifted int `json:"drifted"`

	// WouldCopy is the number of tags that would be copied (dry-run).
	// +kubebuilder:validation:Minimum=0
	WouldCopy int `json:"wouldCopy"`

	// WouldOverwrite is the number of tags that would be overwritten (dry-run).
	// +kubebuilder:validation:Minimum=0
	WouldOverwrite int `json:"wouldOverwrite"`

	// Failed is the number of failed tags and plan-failed entries.
	// +kubebuilder:validation:Minimum=0
	Failed int `json:"failed"`

	// ExitCode is the semantic sync exit code (0 clean, 1 failures, 2 drift).
	// +kubebuilder:validation:Minimum=0
	ExitCode int `json:"exitCode"`
}

// EntryStatus is the entry-level outcome. It answers "was the entry planned
// and run, or did it abort at plan time" — distinct from the per-tag Status.
//
// +kubebuilder:validation:Enum=completed;failed
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

// EntryResult is the per-entry slice of a report. Each tag job becomes a
// TagResult row carrying its own status, digest, skip reason, error, and
// referrers — so verification and per-tag detail are expressible without a
// side channel. Status disambiguate an empty Tags array (nothing matched vs
// plan failed).
type EntryResult struct {
	// Source identifies the entry's source.
	Source string `json:"source"`

	// Destination is the entry's destination repository.
	Destination string `json:"destination"`

	// Status is the entry-level outcome.
	Status EntryStatus `json:"status"`

	// Error carries the plan-time failure message when Status is failed.
	// +optional
	Error string `json:"error,omitempty"`

	// Tags lists the per-tag results.
	Tags []TagResult `json:"tags"`
}

// TagResult is one tag's row in the report. Optional fields follow the wire
// contract: digest when resolved; reason only on skipped; error only on
// failed; referrers only when includeReferrers surfaced any; verification only
// when a signature was confirmed.
type TagResult struct {
	// Tag is the tag name.
	Tag string `json:"tag"`

	// Status is the per-tag outcome.
	Status Status `json:"status"`

	// Digest is the resolved source artifact digest.
	// +optional
	Digest string `json:"digest,omitempty"`

	// Reason explains a skip (up-to-date or signature-too-new).
	// +optional
	Reason string `json:"reason,omitempty"`

	// Error carries the failure message when Status is failed.
	// +optional
	Error string `json:"error,omitempty"`

	// Referrers lists the mirrored referrers (signatures, SBOMs, attestations).
	// +optional
	Referrers []ReferrerResult `json:"referrers,omitempty"`

	// Verification carries the signature verification metadata, when confirmed.
	// +optional
	Verification *Verification `json:"verification,omitempty"`
}

// ReferrerResult is one mirrored referrer (cosign signature bundle, SBOM,
// attestation) of a tag. It reuses the per-tag Status enum; Reason is set to
// up-to-date on a skipped referrer (same always-on-skip rule as tags).
type ReferrerResult struct {
	// Digest is the referrer digest.
	Digest string `json:"digest"`

	// ArtifactType is the referrer's OCI artifact type.
	// +optional
	ArtifactType string `json:"artifactType,omitempty"`

	// Status is the referrer outcome.
	Status Status `json:"status"`

	// Reason explains a skip.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// Verification is the signature verification metadata recorded on a tag row.
// Only Provider is guaranteed; everything else is best-effort and omitted when
// empty: Issuer/Identity are set when the keyless cert identity is recovered;
// IntegratedTime only when the bundle carries a verified transparency-log
// timestamp; Age/MinAge only on the signature-too-new skip.
type Verification struct {
	// Provider is the verification provider (VerifyProviderCosign).
	Provider string `json:"provider"`

	// Issuer is the recovered keyless certificate issuer.
	// +optional
	Issuer string `json:"issuer,omitempty"`

	// Identity is the recovered keyless certificate identity.
	// +optional
	Identity string `json:"identity,omitempty"`

	// IntegratedTime is the verified transparency-log timestamp.
	// +optional
	IntegratedTime string `json:"integratedTime,omitempty"`

	// Age is the signature age on the signature-too-new skip.
	// +optional
	Age string `json:"age,omitempty"`

	// MinAge is the configured minimum age on the signature-too-new skip.
	// +optional
	MinAge string `json:"minAge,omitempty"`
}

// Status is the result of one mirror job, or runner-assigned on failure.
//
// +kubebuilder:validation:Enum=copied;overwritten;skipped;drifted;would-copy;would-overwrite;failed
type Status string

const (
	// StatusCopied means the destination did not have the tag and it was copied.
	StatusCopied Status = "copied"
	// StatusOverwritten means the destination had a different digest and it was
	// replaced (overwrite enabled).
	StatusOverwritten Status = "overwritten"
	// StatusSkipped means nothing was copied; the Reason explains why.
	StatusSkipped Status = "skipped"
	// StatusDrifted means the destination had a different digest and was left
	// alone (overwrite disabled).
	StatusDrifted Status = "drifted"
	// StatusWouldCopy is the dry-run forecast for a tag that would be copied.
	StatusWouldCopy Status = "would-copy"
	// StatusWouldOverwrite is the dry-run forecast for a tag that would be
	// overwritten.
	StatusWouldOverwrite Status = "would-overwrite"
	// StatusFailed marks a failed mirror. The runner assigns it as a job's own
	// status when the job errors (or carries a Job.PlanError); a job never
	// returns it as a result status, so it is excluded from validStatuses.
	// Nested ReferrerResult rows may carry it directly when a referrer fails.
	StatusFailed Status = "failed"
)

const (
	// ReasonUpToDate means the destination already has the same digest. Always
	// set on a StatusSkipped result, absent otherwise.
	ReasonUpToDate = "up-to-date"
	// ReasonSignatureTooNew means a valid signature was deferred because it is
	// younger than the configured verify.minAge. Always set on a StatusSkipped
	// result, absent otherwise.
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

// Valid reports whether s is one of the documented job-returnable statuses.
// Catches typos in entry implementations (a `return "coppied"` would otherwise
// compile and produce an unmatched status that's silently ignored by Render).
// StatusFailed is excluded by design: it is runner-assigned, not returned.
func (s Status) Valid() bool {
	_, ok := validStatuses[s]
	return ok
}
