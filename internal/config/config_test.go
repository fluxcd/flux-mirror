// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	. "github.com/fluxcd/flux-mirror/api/v1beta1"
)

func TestDecode_FullExample(t *testing.T) {
	g := NewWithT(t)

	src := `
apiVersion: mirror.plugin.fluxcd.io/v1beta1
kind: Config
charts:
  - source: https://charts.dexidp.io
    destination: oci://ghcr.io/example/charts
    name: dex
    version: ">=0.19.0 <1.0.0"
    limit: 5
artifacts:
  - source: ghcr.io/dexidp/dex
    destination: ghcr.io/example/mirror/dex
    selector:
      semver: ">=2.40.0 <3.0.0"
      limit: 10
    includeReferrers: true
    verify:
      provider: cosign
      minAge: 12h
      matchOIDCIdentity:
        - issuer: https://token.actions.githubusercontent.com
          subject: ^https://github\.com/dexidp/.*$
  - source: ghcr.io/example/nightly-build
    destination: ghcr.io/example/mirror/nightly-build
    selector:
      regex:
        pattern: '^\d+\.\d+\.\d+-(?P<ts>\d+)$'
        extract: '$ts'
      sortBy: numerical
      limit: 5
`
	cfg, err := Decode(strings.NewReader(src))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(Validate(cfg)).To(Succeed())

	g.Expect(cfg.Charts).To(HaveLen(1))
	g.Expect(cfg.Charts[0].Name).To(Equal("dex"))
	g.Expect(cfg.Charts[0].EffectiveLimit()).To(Equal(5))

	g.Expect(cfg.Artifacts).To(HaveLen(2))
	g.Expect(cfg.Artifacts[0].IncludeReferrers).To(BeTrue())
	g.Expect(cfg.Artifacts[0].Verify.Provider).To(Equal(VerifyProviderCosign))
	g.Expect(cfg.Artifacts[0].Verify.MinAge).ToNot(BeNil())
	g.Expect(cfg.Artifacts[0].Verify.MinAge.Duration).To(Equal(12 * time.Hour))
	g.Expect(cfg.Artifacts[0].Verify.MatchOIDCIdentity).To(Equal([]OIDCIdentity{{
		Issuer:  "https://token.actions.githubusercontent.com",
		Subject: `^https://github\.com/dexidp/.*$`,
	}}))
	g.Expect(cfg.Artifacts[0].Selector.EffectiveLimit()).To(Equal(10))
	g.Expect(cfg.Artifacts[0].Selector.EffectiveSortBy()).To(Equal(SortBySemver))
	g.Expect(cfg.Artifacts[1].Selector.Regex.Extract).To(Equal("$ts"))
	g.Expect(cfg.Artifacts[1].Selector.EffectiveSortBy()).To(Equal(SortByNumerical))
}

func TestDecode_EnvSubstitution(t *testing.T) {
	g := NewWithT(t)
	t.Setenv("CREDENTIAL_KEY", "value")
	t.Setenv("REGISTRY_TOKEN", "env-token")

	src := `apiVersion: mirror.plugin.fluxcd.io/v1beta1
kind: Config
hosts:
  - host: registry.example.com
    credential:
      ${CREDENTIAL_KEY}: ${REGISTRY_TOKEN}
`
	cfg, err := Decode(strings.NewReader(src))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(cfg.Hosts).To(HaveLen(1))
	g.Expect(cfg.Hosts[0].Credential.Value).To(Equal("env-token"))
}

func TestDecode_EnvSubstitutionIgnoresSingleDollar(t *testing.T) {
	g := NewWithT(t)

	src := `apiVersion: mirror.plugin.fluxcd.io/v1beta1
kind: Config
artifacts:
  - source: ghcr.io/a/b
    destination: ghcr.io/c/d
    selector:
      regex:
        pattern: .*
        extract: "$patch"
`
	cfg, err := Decode(strings.NewReader(src))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(cfg.Artifacts[0].Selector.Regex.Extract).To(Equal("$patch"))
}

func TestDecodeWithEnvSubstDisabled(t *testing.T) {
	g := NewWithT(t)
	t.Setenv("REGISTRY_TOKEN", "env-token")

	src := `apiVersion: mirror.plugin.fluxcd.io/v1beta1
kind: Config
hosts:
  - host: registry.example.com
    credential:
      value: ${REGISTRY_TOKEN}
`
	cfg, err := DecodeWithEnvSubst(strings.NewReader(src), false)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(cfg.Hosts[0].Credential.Value).To(Equal("${REGISTRY_TOKEN}"))
}

func TestDecode_EnvSubstitutionStrict(t *testing.T) {
	g := NewWithT(t)
	const missingToken = "FLUX_MIRROR_TEST_MISSING_TOKEN_STRICT"
	old, hadOld := os.LookupEnv(missingToken)
	g.Expect(os.Unsetenv(missingToken)).To(Succeed())
	t.Cleanup(func() {
		if hadOld {
			_ = os.Setenv(missingToken, old)
		}
	})

	src := `apiVersion: mirror.plugin.fluxcd.io/v1beta1
kind: Config
hosts:
  - host: registry.example.com
    credential:
      value: ${FLUX_MIRROR_TEST_MISSING_TOKEN_STRICT}
`
	_, err := Decode(strings.NewReader(src))
	g.Expect(err).To(MatchError(ContainSubstring("substitute environment variables")))
}

func TestDecode_EnvSubstitutionStrictAllowsEmpty(t *testing.T) {
	g := NewWithT(t)
	t.Setenv("EMPTY_TOKEN", "")

	src := `apiVersion: mirror.plugin.fluxcd.io/v1beta1
kind: Config
hosts:
  - host: registry.example.com
    credential:
      value: ${EMPTY_TOKEN}
`
	cfg, err := Decode(strings.NewReader(src))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(cfg.Hosts[0].Credential.Value).To(BeEmpty())
}

func TestDefaults(t *testing.T) {
	g := NewWithT(t)

	c := ChartEntry{}
	g.Expect(c.EffectiveVersion()).To(Equal("*"))
	g.Expect(c.EffectiveLimit()).To(Equal(1))

	s := Selector{}
	g.Expect(s.EffectiveSortBy()).To(Equal(SortBySemver))
	g.Expect(s.EffectiveLimit()).To(Equal(1))

	s.Limit = new(0)
	g.Expect(s.EffectiveLimit()).To(Equal(0)) // 0 = unlimited (selector enforces this)
}

func TestResolvePaths(t *testing.T) {
	g := NewWithT(t)

	mkCfg := func() *Config {
		return &Config{Hosts: []RegistryHost{{
			Host: "h.example",
			Credential: &RegistryCredential{
				FromPath: "tokens/jwt",
				JWKPath:  "/abs/keys/jwk.json", // absolute: clamped within baseDir
			},
			TLS: &TLS{
				ServerAuth: &TLSServerAuth{FromPath: "../../etc/shadow"}, // traversal: clamped
				ClientAuth: &TLSClientAuth{
					Certificate: &TLSData{FromPath: "certs/client.crt"},
					Key:         &TLSKey{Value: "CLIENT_KEY"}, // not a path: untouched
				},
			},
		}}}
	}

	// SecureJoin confines every path within baseDir — relative, absolute, and
	// "../" escapes alike. Non-path fields are untouched.
	cfg := mkCfg()
	g.Expect(ResolvePaths(cfg, "/etc/flux-mirror")).To(Succeed())
	h := cfg.Hosts[0]
	g.Expect(h.Credential.FromPath).To(Equal("/etc/flux-mirror/tokens/jwt"))
	g.Expect(h.Credential.JWKPath).To(Equal("/etc/flux-mirror/abs/keys/jwk.json"))
	g.Expect(h.TLS.ServerAuth.FromPath).To(Equal("/etc/flux-mirror/etc/shadow")) // escape clamped
	g.Expect(h.TLS.ClientAuth.Certificate.FromPath).To(Equal("/etc/flux-mirror/certs/client.crt"))
	g.Expect(h.TLS.ClientAuth.Key.Value).To(Equal("CLIENT_KEY"))

	// Empty baseDir is a no-op: paths stay as written.
	cfg = mkCfg()
	g.Expect(ResolvePaths(cfg, "")).To(Succeed())
	g.Expect(cfg.Hosts[0].Credential.FromPath).To(Equal("tokens/jwt"))
	g.Expect(cfg.Hosts[0].TLS.ServerAuth.FromPath).To(Equal("../../etc/shadow"))
}

func TestValidate_Table(t *testing.T) {
	negOne := -1
	tests := []struct {
		name   string
		cfg    Config
		errMsg string
	}{
		{
			name: "valid artifacts only",
			cfg: Config{
				TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind},
				Artifacts: []ArtifactEntry{{
					Source: "ghcr.io/a/b", Destination: "ghcr.io/c/d",
					Selector: Selector{Semver: ">=1.0.0", Limit: new(5)},
				}},
			},
		},
		{
			name: "valid charts only with unlimited",
			cfg: Config{
				TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind},
				Charts: []ChartEntry{{
					Source: "https://charts.example.com", Destination: "oci://ghcr.io/x",
					Name: "foo", Limit: new(0),
				}},
			},
		},
		{
			name:   "wrong apiVersion",
			cfg:    Config{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: ConfigKind}, Artifacts: validArtifact()},
			errMsg: "apiVersion must be",
		},
		{
			name:   "wrong kind",
			cfg:    Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "Other"}, Artifacts: validArtifact()},
			errMsg: "kind must be",
		},
		{
			name:   "no entries",
			cfg:    Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}},
			errMsg: "no entries",
		},
		{
			name: "chart bad source scheme",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Charts: []ChartEntry{{
				Source: "ftp://example.com", Destination: "oci://ghcr.io/x", Name: "foo",
			}}},
			errMsg: "scheme \"ftp\"",
		},
		{
			name: "chart oci source rejected",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Charts: []ChartEntry{{
				Source: "oci://ghcr.io/example/charts", Destination: "oci://ghcr.io/x", Name: "foo",
			}}},
			errMsg: "use 'artifacts' to mirror an OCI Helm chart",
		},
		{
			name: "chart destination not oci",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Charts: []ChartEntry{{
				Source: "https://charts.example.com", Destination: "https://ghcr.io/x", Name: "foo",
			}}},
			errMsg: "scheme must be oci",
		},
		{
			name: "chart bad version constraint",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Charts: []ChartEntry{{
				Source: "https://charts.example.com", Destination: "oci://ghcr.io/x",
				Name: "foo", Version: "not-a-semver",
			}}},
			errMsg: "valid semver constraint",
		},
		{
			name: "chart negative limit",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Charts: []ChartEntry{{
				Source: "https://charts.example.com", Destination: "oci://ghcr.io/x",
				Name: "foo", Limit: &negOne,
			}}},
			errMsg: "limit must be >= 0",
		},
		{
			name: "artifact bad source",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: []ArtifactEntry{{
				Source: "not a repo!", Destination: "ghcr.io/c/d",
			}}},
			errMsg: "valid OCI repository",
		},
		{
			name: "selector bad regex",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: []ArtifactEntry{{
				Source: "ghcr.io/a/b", Destination: "ghcr.io/c/d",
				Selector: Selector{Regex: &RegexFilter{Pattern: "(unclosed"}},
			}}},
			errMsg: "does not compile",
		},
		{
			name: "selector bad semver",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: []ArtifactEntry{{
				Source: "ghcr.io/a/b", Destination: "ghcr.io/c/d",
				Selector: Selector{Semver: "not-a-semver"},
			}}},
			errMsg: "valid constraint",
		},
		{
			name: "selector unknown sortBy",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: []ArtifactEntry{{
				Source: "ghcr.io/a/b", Destination: "ghcr.io/c/d",
				Selector: Selector{SortBy: "random"},
			}}},
			errMsg: "sortBy",
		},
		{
			name: "selector negative limit",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: []ArtifactEntry{{
				Source: "ghcr.io/a/b", Destination: "ghcr.io/c/d",
				Selector: Selector{Limit: &negOne},
			}}},
			errMsg: "limit must be >= 0",
		},
		{
			name: "verify unknown provider",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: []ArtifactEntry{{
				Source: "ghcr.io/a/b", Destination: "ghcr.io/c/d",
				Verify: &ArtifactVerification{Provider: "unknown"},
			}}},
			errMsg: "provider",
		},
		{
			name: "verify missing identities",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: []ArtifactEntry{{
				Source: "ghcr.io/a/b", Destination: "ghcr.io/c/d",
				Verify: &ArtifactVerification{Provider: VerifyProviderCosign},
			}}},
			errMsg: "matchOIDCIdentity",
		},
		{
			name: "verify missing issuer",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: []ArtifactEntry{{
				Source: "ghcr.io/a/b", Destination: "ghcr.io/c/d",
				Verify: &ArtifactVerification{
					Provider: VerifyProviderCosign,
					MatchOIDCIdentity: []OIDCIdentity{{
						Subject: `^https://github\.com/a/.*$`,
					}},
				},
			}}},
			errMsg: "issuer is required",
		},
		{
			name: "verify bad subject regex",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: []ArtifactEntry{{
				Source: "ghcr.io/a/b", Destination: "ghcr.io/c/d",
				Verify: &ArtifactVerification{
					Provider: VerifyProviderCosign,
					MatchOIDCIdentity: []OIDCIdentity{{
						Issuer:  "https://token.actions.githubusercontent.com",
						Subject: "(unclosed",
					}},
				},
			}}},
			errMsg: "does not compile",
		},
		{
			name: "auth valid provider",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{
					Host: "mint.example", Credential: &RegistryCredential{Provider: JWTProviderGitHub, Aud: "custom"},
				}}},
		},
		{
			name: "auth valid provider gcp",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{
					Host: "us-docker.pkg.dev", Credential: &RegistryCredential{Provider: JWTProviderGCP, Aud: "custom"},
				}}},
		},
		{
			name: "auth valid provider azure",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{
					Host: "myregistry.azurecr.io", Credential: &RegistryCredential{Provider: JWTProviderAzure, Aud: "custom"},
				}}},
		},
		{
			name: "auth valid provider aws",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{
					Host: "registry.example.com", Credential: &RegistryCredential{Provider: JWTProviderAWS, Aud: "custom"},
				}}},
		},
		{
			name: "auth valid value",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{
					Host: "static.example", Credential: &RegistryCredential{Value: "TOKEN"},
				}}},
		},
		{
			name: "auth valid fromPath",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{
					Host: "static.example", Credential: &RegistryCredential{FromPath: "/run/secrets/token"},
				}}},
		},
		{
			name: "auth valid jwkPath",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{
					Host: "registry.example", Credential: &RegistryCredential{
						JWKPath: "/path/jwk.json", Iss: "https://issuer", Sub: "client", Aud: "registry.example",
					},
				}}},
		},
		{
			name: "auth valid jwkPath with exp",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{
					Host: "registry.example", Credential: &RegistryCredential{
						JWKPath: "/path/jwk.json", Iss: "https://issuer", Sub: "client",
						Exp: &metav1.Duration{Duration: time.Hour},
					},
				}}},
		},
		{
			name: "auth valid jwkValue",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{
					Host: "registry.example", Credential: &RegistryCredential{
						JWKValue: "REGISTRY_JWK", Iss: "https://issuer", Sub: "client", Aud: "registry.example",
						Exp: &metav1.Duration{Duration: time.Hour},
					},
				}}},
		},
		{
			name: "auth jwkValue missing iss",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{
					Host: "registry.example", Credential: &RegistryCredential{
						JWKValue: "REGISTRY_JWK", Sub: "client",
					},
				}}},
			errMsg: "iss is required with jwkPath or jwkValue",
		},
		{
			name: "auth jwkPath and jwkValue both set",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{
					Host: "registry.example", Credential: &RegistryCredential{
						JWKPath: "/path/jwk.json", JWKValue: "REGISTRY_JWK", Iss: "https://issuer", Sub: "client",
					},
				}}},
			errMsg: "exactly one of provider, value, fromPath, jwkPath, or jwkValue",
		},
		{
			name: "auth jwkPath exp non-positive",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{
					Host: "registry.example", Credential: &RegistryCredential{
						JWKPath: "/path/jwk.json", Iss: "https://issuer", Sub: "client",
						Exp: &metav1.Duration{Duration: 0},
					},
				}}},
			errMsg: "exp must be a positive duration",
		},
		{
			name: "auth exp rejected with provider",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{
					Host: "registry.example", Credential: &RegistryCredential{
						Provider: JWTProviderGitHub, Exp: &metav1.Duration{Duration: time.Hour},
					},
				}}},
			errMsg: "exp can only be set with jwkPath",
		},
		{
			name: "auth exp rejected with value",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{
					Host: "registry.example", Credential: &RegistryCredential{
						Value: "TOKEN", Exp: &metav1.Duration{Duration: time.Hour},
					},
				}}},
			errMsg: "exp can only be set with jwkPath",
		},
		{
			name: "auth missing host",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Credential: &RegistryCredential{Value: "TOKEN"}}}},
			errMsg: "host is required",
		},
		{
			name: "auth missing credential, provider, tls and maxChunkSize",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example"}}},
			errMsg: "one of credential, provider, tls, or maxChunkSize is required",
		},
		{
			name: "auth maxChunkSize only is valid",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", MaxChunkSize: 1024}}},
		},
		{
			name: "auth negative maxChunkSize",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", MaxChunkSize: -1}}},
			errMsg: "maxChunkSize must be >= 0",
		},
		{
			name: "auth valid provider ecr",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{
					Host: "123.dkr.ecr.us-east-1.amazonaws.com", Provider: RegistryProviderECR,
				}}},
		},
		{
			name: "auth invalid provider",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", Provider: "dockerhub"}}},
			errMsg: "provider \"dockerhub\" must be one of: ecr, acr, gar",
		},
		{
			name: "auth credential and provider mutually exclusive",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{
					Host: "h.example", Provider: RegistryProviderGAR,
					Credential: &RegistryCredential{Value: "TOKEN"},
				}}},
			errMsg: "credential and provider are mutually exclusive",
		},
		{
			name: "auth username with credential is valid",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{
					Host: "h.example", Username: "bob",
					Credential: &RegistryCredential{Value: "TOKEN"},
				}}},
		},
		{
			name: "auth username without credential",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", Username: "bob", MaxChunkSize: 1024}}},
			errMsg: "username requires credential",
		},
		{
			name: "auth username with tls but no credential",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", Username: "bob",
					TLS: &TLS{ServerAuth: &TLSServerAuth{FromPath: "/ca.crt"}},
				}}},
			errMsg: "username requires credential",
		},
		{
			name: "auth duplicate host",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{
					{Host: "dup.example", Credential: &RegistryCredential{Value: "A"}},
					{Host: "dup.example", Credential: &RegistryCredential{Value: "B"}},
				}},
			errMsg: "configured more than once",
		},
		{
			name: "auth no source set",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", Credential: &RegistryCredential{}}}},
			errMsg: "exactly one of provider, value, fromPath, jwkPath, or jwkValue",
		},
		{
			name: "auth two sources set",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", Credential: &RegistryCredential{
					Provider: JWTProviderGitHub, Value: "TOKEN",
				}}}},
			errMsg: "exactly one of provider, value, fromPath, jwkPath, or jwkValue",
		},
		{
			name: "auth invalid provider",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", Credential: &RegistryCredential{
					Provider: "gitlab",
				}}}},
			errMsg: "must be one of: github, forgejo, gcp, azure, aws",
		},
		{
			name: "auth jwkPath missing iss",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", Credential: &RegistryCredential{
					JWKPath: "/k.json", Sub: "client",
				}}}},
			errMsg: "iss is required with jwkPath",
		},
		{
			name: "auth jwkPath missing sub",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", Credential: &RegistryCredential{
					JWKPath: "/k.json", Iss: "https://issuer",
				}}}},
			errMsg: "sub is required with jwkPath",
		},
		{
			name: "auth iss set without jwkPath",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", Credential: &RegistryCredential{
					Provider: JWTProviderGitHub, Iss: "https://issuer",
				}}}},
			errMsg: "iss and sub can only be set with jwkPath",
		},
		{
			name: "auth aud set with value",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", Credential: &RegistryCredential{
					Value: "TOKEN", Aud: "nope",
				}}}},
			errMsg: "aud can only be set with jwkPath, jwkValue, or provider",
		},
		{
			name: "auth aud set with fromPath",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", Credential: &RegistryCredential{
					FromPath: "/path/token", Aud: "nope",
				}}}},
			errMsg: "aud can only be set with jwkPath, jwkValue, or provider",
		},
		{
			name: "auth iss set with fromPath",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", Credential: &RegistryCredential{
					FromPath: "/path/token", Iss: "https://issuer",
				}}}},
			errMsg: "iss and sub can only be set with jwkPath",
		},
		{
			name: "auth fromPath and value set",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", Credential: &RegistryCredential{
					Value: "TOKEN", FromPath: "/path/token",
				}}}},
			errMsg: "exactly one of provider, value, fromPath, jwkPath, or jwkValue",
		},
		{
			name: "credential provider jwt-svid is valid",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", Credential: &RegistryCredential{
					Provider: JWTProviderJWTSVID, Aud: "h.example",
				}}}},
		},
		{
			name: "tls serverAuth only host is valid",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", TLS: &TLS{
					ServerAuth: &TLSServerAuth{FromPath: "/ca.crt"},
				}}}},
		},
		{
			name: "tls and provider mutually exclusive",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", Provider: RegistryProviderECR,
					TLS: &TLS{ServerAuth: &TLSServerAuth{FromPath: "/ca.crt"}},
				}}},
			errMsg: "provider and tls are mutually exclusive",
		},
		{
			name: "tls credential and tls together is valid",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example",
					Credential: &RegistryCredential{Value: "TOKEN"},
					TLS:        &TLS{ClientAuth: &TLSClientAuth{Certificate: &TLSData{FromPath: "/c.crt"}, Key: &TLSKey{FromPath: "/c.key"}}},
				}}},
		},
		{
			name: "tls client-only spiffe with custom CA server is valid",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", TLS: &TLS{
					ServerAuth: &TLSServerAuth{FromPath: "/ca.crt"},
					ClientAuth: &TLSClientAuth{Provider: TLSClientProviderX509SVID},
				}}}},
		},
		{
			name: "tls full spiffe (client svid + server spiffe) is valid",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", TLS: &TLS{
					ServerAuth: &TLSServerAuth{SPIFFE: &SPIFFETLS{TrustDomain: TrustDomainSelf}},
					ClientAuth: &TLSClientAuth{Provider: TLSClientProviderX509SVID},
				}}}},
		},
		{
			name: "tls empty is rejected",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", TLS: &TLS{}}}},
			errMsg: "one of serverAuth or clientAuth is required",
		},
		{
			name: "tls serverAuth two sources",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", TLS: &TLS{
					ServerAuth: &TLSServerAuth{FromPath: "/ca.crt", Value: "CA"},
				}}}},
			errMsg: "serverAuth: exactly one of fromPath, value, or spiffe",
		},
		{
			name: "tls serverAuth ca and spiffe together",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", TLS: &TLS{
					ServerAuth: &TLSServerAuth{FromPath: "/ca.crt", SPIFFE: &SPIFFETLS{AuthorizeAny: true}},
				}}}},
			errMsg: "serverAuth: exactly one of fromPath, value, or spiffe",
		},
		{
			name: "tls clientAuth missing key",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", TLS: &TLS{
					ClientAuth: &TLSClientAuth{Certificate: &TLSData{FromPath: "/c.crt"}},
				}}}},
			errMsg: "clientAuth: key is required",
		},
		{
			name: "tls clientAuth provider and static mutually exclusive",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", TLS: &TLS{
					ClientAuth: &TLSClientAuth{Provider: TLSClientProviderX509SVID, Key: &TLSKey{FromPath: "/c.key"}},
				}}}},
			errMsg: "provider is mutually exclusive with certificate and key",
		},
		{
			name: "tls clientAuth invalid provider",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", TLS: &TLS{
					ClientAuth: &TLSClientAuth{Provider: "jwt-svid"},
				}}}},
			errMsg: `provider "jwt-svid" must be "x509-svid"`,
		},
		{
			name: "tls serverAuth spiffe serverID valid",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", TLS: &TLS{
					ServerAuth: &TLSServerAuth{SPIFFE: &SPIFFETLS{ServerID: "spiffe://example.org/registry"}},
				}}}},
		},
		{
			name: "tls serverAuth spiffe no authorizer",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", TLS: &TLS{
					ServerAuth: &TLSServerAuth{SPIFFE: &SPIFFETLS{}},
				}}}},
			errMsg: "exactly one of serverID, trustDomain, or authorizeAny must be set",
		},
		{
			name: "tls serverAuth spiffe two authorizers",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", TLS: &TLS{
					ServerAuth: &TLSServerAuth{SPIFFE: &SPIFFETLS{TrustDomain: TrustDomainSelf, AuthorizeAny: true}},
				}}}},
			errMsg: "exactly one of serverID, trustDomain, or authorizeAny must be set",
		},
		{
			name: "tls serverAuth spiffe invalid serverID",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: validArtifact(),
				Hosts: []RegistryHost{{Host: "h.example", TLS: &TLS{
					ServerAuth: &TLSServerAuth{SPIFFE: &SPIFFETLS{ServerID: "not-a-spiffe-id"}},
				}}}},
			errMsg: "is not a valid SPIFFE ID",
		},
		{
			name: "verify negative minAge",
			cfg: Config{TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind}, Artifacts: []ArtifactEntry{{
				Source: "ghcr.io/a/b", Destination: "ghcr.io/c/d",
				Verify: &ArtifactVerification{
					Provider: VerifyProviderCosign,
					MinAge:   &metav1.Duration{Duration: -time.Second},
					MatchOIDCIdentity: []OIDCIdentity{{
						Issuer:  "https://token.actions.githubusercontent.com",
						Subject: `^https://github\.com/a/.*$`,
					}},
				},
			}}},
			errMsg: "minAge must be >= 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			err := Validate(&tt.cfg)
			if tt.errMsg == "" {
				g.Expect(err).ToNot(HaveOccurred())
				return
			}
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tt.errMsg))
		})
	}
}

func validArtifact() []ArtifactEntry {
	return []ArtifactEntry{{
		Source: "ghcr.io/a/b", Destination: "ghcr.io/c/d",
	}}
}

func TestDecode_BadYAML(t *testing.T) {
	g := NewWithT(t)
	_, err := Decode(strings.NewReader("apiVersion: [oops"))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("parse config"))
}

func TestDecode_BadDuration(t *testing.T) {
	g := NewWithT(t)
	_, err := Decode(strings.NewReader(`
apiVersion: mirror.plugin.fluxcd.io/v1beta1
kind: Config
artifacts:
  - source: ghcr.io/a/b
    destination: ghcr.io/c/d
    verify:
      provider: cosign
      minAge: soon
      matchOIDCIdentity:
        - issuer: https://token.actions.githubusercontent.com
          subject: ^https://github\.com/a/.*$
`))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("invalid duration"))
}
