// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/santhosh-tekuri/jsonschema/v6"

	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta1"
)

// validateReportSchema compiles the generated report JSON Schema and validates
// the given JSON document against it, guarding that the published schema stays
// in sync with the API types.
func validateReportSchema(t *testing.T, raw string) {
	t.Helper()
	g := NewWithT(t)

	var doc any
	g.Expect(json.Unmarshal([]byte(raw), &doc)).To(Succeed())

	abs, err := filepath.Abs(filepath.Join("..", "..", "docs", "report-v1beta1.json"))
	g.Expect(err).ToNot(HaveOccurred())

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	schema, err := compiler.Compile("file://" + filepath.ToSlash(abs))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(schema.Validate(doc)).To(Succeed())
}

func TestReport_Schema(t *testing.T) {
	g := NewWithT(t)

	res := Result{
		Duration: 1500 * time.Millisecond,
		Entries: []apiv1.EntryResult{
			{
				Source:      "ghcr.io/foo/bar",
				Destination: "ghcr.io/mirror/bar",
				Status:      apiv1.EntryCompleted,
				Tags: []apiv1.TagResult{
					{
						Tag:    "v1.0.0",
						Status: apiv1.StatusCopied,
						Digest: "sha256:abc",
						Verification: &apiv1.Verification{
							Provider: apiv1.VerifyProviderCosign,
							Issuer:   "https://token.actions.githubusercontent.com",
							Identity: "https://github.com/foo/bar/.github/workflows/release.yml@refs/tags/v1.0.0",
						},
						Referrers: []apiv1.ReferrerResult{
							{Digest: "sha256:sig", ArtifactType: "application/vnd.dev.cosign.artifact.sig.v1+json", Status: apiv1.StatusCopied},
						},
					},
					{Tag: "v0.9.0", Status: apiv1.StatusSkipped, Reason: apiv1.ReasonUpToDate},
					{Tag: "v0.8.0", Status: apiv1.StatusFailed, Error: "boom"},
				},
			},
			{
				Source:      "ghcr.io/foo/baz",
				Destination: "ghcr.io/mirror/baz",
				Status:      apiv1.EntryFailed,
				Error:       "list tags: denied",
				Tags:        []apiv1.TagResult{},
			},
		},
	}

	ts := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	report := NewReport("flux-mirror/v1.2.3", ts, res)

	var buf bytes.Buffer
	g.Expect(RenderReport(&buf, "json", report)).To(Succeed())
	validateReportSchema(t, buf.String())
}
