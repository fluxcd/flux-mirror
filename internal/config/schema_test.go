// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/santhosh-tekuri/jsonschema/v6"
	k8syaml "sigs.k8s.io/yaml"
)

// validateConfigSchema compiles the generated config JSON Schema and validates
// the given YAML document against it, guarding that the published schema stays
// in sync with the API types.
func validateConfigSchema(t *testing.T, raw string) {
	t.Helper()
	g := NewWithT(t)

	var doc any
	g.Expect(k8syaml.Unmarshal([]byte(raw), &doc)).To(Succeed())

	abs, err := filepath.Abs(filepath.Join("..", "..", "docs", "config", "config-v1beta1.json"))
	g.Expect(err).ToNot(HaveOccurred())

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	schema, err := compiler.Compile("file://" + filepath.ToSlash(abs))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(schema.Validate(doc)).To(Succeed())
}

func TestConfig_Schema(t *testing.T) {
	validateConfigSchema(t, `apiVersion: mirror.fluxcd.io/v1beta1
kind: Config
hosts:
  - host: ghcr.io
    credential:
      provider: github
  - host: registry.example.com
    tls:
      serverAuth:
        fromPath: /etc/ssl/ca.crt
      clientAuth:
        provider: x509-svid
  - host: 123456789.dkr.ecr.us-east-1.amazonaws.com
    provider: ecr
charts:
  - source: https://charts.dexidp.io
    destination: oci://ghcr.io/example/charts
    name: dex
    version: ">=0.19.0 <1.0.0"
    limit: 5
    overwrite: true
artifacts:
  - source: ghcr.io/dexidp/dex
    destination: ghcr.io/example/mirror/dex
    includeReferrers: true
    selector:
      regex:
        pattern: '^v(?P<version>.*)$'
        extract: version
      semver: ">=2.0.0"
      sortBy: semver
      limit: 3
    verify:
      provider: cosign
      matchOIDCIdentity:
        - issuer: https://token.actions.githubusercontent.com
          subject: ^https://github.com/dexidp/.*$
      minAge: 24h
`)
}
