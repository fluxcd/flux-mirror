// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"
)

func TestDecode_FullExample(t *testing.T) {
	g := NewWithT(t)

	src := `
apiVersion: mirror.fluxcd.io/v1alpha1
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
	g.Expect(cfg.Validate()).To(Succeed())

	g.Expect(cfg.Charts).To(HaveLen(1))
	g.Expect(cfg.Charts[0].Name).To(Equal("dex"))
	g.Expect(cfg.Charts[0].EffectiveLimit()).To(Equal(5))

	g.Expect(cfg.Artifacts).To(HaveLen(2))
	g.Expect(cfg.Artifacts[0].IncludeReferrers).To(BeTrue())
	g.Expect(cfg.Artifacts[0].Verify.Provider).To(Equal(VerifyProviderCosign))
	g.Expect(cfg.Artifacts[0].Verify.MatchOIDCIdentity).To(Equal([]OIDCIdentity{{
		Issuer:  "https://token.actions.githubusercontent.com",
		Subject: `^https://github\.com/dexidp/.*$`,
	}}))
	g.Expect(cfg.Artifacts[0].Selector.EffectiveLimit()).To(Equal(10))
	g.Expect(cfg.Artifacts[0].Selector.EffectiveSortBy()).To(Equal(SortBySemver))
	g.Expect(cfg.Artifacts[1].Selector.Regex.Extract).To(Equal("$ts"))
	g.Expect(cfg.Artifacts[1].Selector.EffectiveSortBy()).To(Equal(SortByNumerical))
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
				APIVersion: APIVersion, Kind: Kind,
				Artifacts: []ArtifactEntry{{
					Source: "ghcr.io/a/b", Destination: "ghcr.io/c/d",
					Selector: Selector{Semver: ">=1.0.0", Limit: new(5)},
				}},
			},
		},
		{
			name: "valid charts only with unlimited",
			cfg: Config{
				APIVersion: APIVersion, Kind: Kind,
				Charts: []ChartEntry{{
					Source: "https://charts.example.com", Destination: "oci://ghcr.io/x",
					Name: "foo", Limit: new(0),
				}},
			},
		},
		{
			name:   "wrong apiVersion",
			cfg:    Config{APIVersion: "v1", Kind: Kind, Artifacts: validArtifact()},
			errMsg: "apiVersion must be",
		},
		{
			name:   "wrong kind",
			cfg:    Config{APIVersion: APIVersion, Kind: "Other", Artifacts: validArtifact()},
			errMsg: "kind must be",
		},
		{
			name:   "no entries",
			cfg:    Config{APIVersion: APIVersion, Kind: Kind},
			errMsg: "no entries",
		},
		{
			name: "chart bad source scheme",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Charts: []ChartEntry{{
				Source: "ftp://example.com", Destination: "oci://ghcr.io/x", Name: "foo",
			}}},
			errMsg: "scheme \"ftp\"",
		},
		{
			name: "chart destination not oci",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Charts: []ChartEntry{{
				Source: "https://charts.example.com", Destination: "https://ghcr.io/x", Name: "foo",
			}}},
			errMsg: "scheme must be oci",
		},
		{
			name: "chart bad version constraint",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Charts: []ChartEntry{{
				Source: "https://charts.example.com", Destination: "oci://ghcr.io/x",
				Name: "foo", Version: "not-a-semver",
			}}},
			errMsg: "valid semver constraint",
		},
		{
			name: "chart negative limit",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Charts: []ChartEntry{{
				Source: "https://charts.example.com", Destination: "oci://ghcr.io/x",
				Name: "foo", Limit: &negOne,
			}}},
			errMsg: "limit must be >= 0",
		},
		{
			name: "artifact bad source",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: []ArtifactEntry{{
				Source: "not a repo!", Destination: "ghcr.io/c/d",
			}}},
			errMsg: "valid OCI repository",
		},
		{
			name: "selector bad regex",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: []ArtifactEntry{{
				Source: "ghcr.io/a/b", Destination: "ghcr.io/c/d",
				Selector: Selector{Regex: &RegexFilter{Pattern: "(unclosed"}},
			}}},
			errMsg: "does not compile",
		},
		{
			name: "selector bad semver",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: []ArtifactEntry{{
				Source: "ghcr.io/a/b", Destination: "ghcr.io/c/d",
				Selector: Selector{Semver: "not-a-semver"},
			}}},
			errMsg: "valid constraint",
		},
		{
			name: "selector unknown sortBy",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: []ArtifactEntry{{
				Source: "ghcr.io/a/b", Destination: "ghcr.io/c/d",
				Selector: Selector{SortBy: "random"},
			}}},
			errMsg: "sortBy",
		},
		{
			name: "selector negative limit",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: []ArtifactEntry{{
				Source: "ghcr.io/a/b", Destination: "ghcr.io/c/d",
				Selector: Selector{Limit: &negOne},
			}}},
			errMsg: "limit must be >= 0",
		},
		{
			name: "verify unknown provider",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: []ArtifactEntry{{
				Source: "ghcr.io/a/b", Destination: "ghcr.io/c/d",
				Verify: &ArtifactVerification{Provider: "unknown"},
			}}},
			errMsg: "provider",
		},
		{
			name: "verify missing identities",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: []ArtifactEntry{{
				Source: "ghcr.io/a/b", Destination: "ghcr.io/c/d",
				Verify: &ArtifactVerification{Provider: VerifyProviderCosign},
			}}},
			errMsg: "matchOIDCIdentity",
		},
		{
			name: "verify missing issuer",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: []ArtifactEntry{{
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
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: []ArtifactEntry{{
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
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: validArtifact(),
				Auth: &Auth{Hosts: []AuthHost{{
					Host: "mint.example", JWT: &AuthJWT{Provider: JWTProviderGitHub, Aud: "custom"},
				}}}},
		},
		{
			name: "auth valid fromEnv",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: validArtifact(),
				Auth: &Auth{Hosts: []AuthHost{{
					Host: "static.example", JWT: &AuthJWT{FromEnv: "TOKEN"},
				}}}},
		},
		{
			name: "auth valid jwkPath",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: validArtifact(),
				Auth: &Auth{Hosts: []AuthHost{{
					Host: "registry.example", JWT: &AuthJWT{
						JWKPath: "/path/jwk.json", Iss: "https://issuer", Sub: "client", Aud: "registry.example",
					},
				}}}},
		},
		{
			name: "auth empty hosts",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: validArtifact(),
				Auth: &Auth{}},
			errMsg: "hosts must contain at least one host",
		},
		{
			name: "auth missing host",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: validArtifact(),
				Auth: &Auth{Hosts: []AuthHost{{JWT: &AuthJWT{FromEnv: "TOKEN"}}}}},
			errMsg: "host is required",
		},
		{
			name: "auth missing jwt",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: validArtifact(),
				Auth: &Auth{Hosts: []AuthHost{{Host: "h.example"}}}},
			errMsg: "jwt is required",
		},
		{
			name: "auth duplicate host",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: validArtifact(),
				Auth: &Auth{Hosts: []AuthHost{
					{Host: "dup.example", JWT: &AuthJWT{FromEnv: "A"}},
					{Host: "dup.example", JWT: &AuthJWT{FromEnv: "B"}},
				}}},
			errMsg: "configured more than once",
		},
		{
			name: "auth no source set",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: validArtifact(),
				Auth: &Auth{Hosts: []AuthHost{{Host: "h.example", JWT: &AuthJWT{}}}}},
			errMsg: "exactly one of provider, fromEnv, or jwkPath",
		},
		{
			name: "auth two sources set",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: validArtifact(),
				Auth: &Auth{Hosts: []AuthHost{{Host: "h.example", JWT: &AuthJWT{
					Provider: JWTProviderGitHub, FromEnv: "TOKEN",
				}}}}},
			errMsg: "exactly one of provider, fromEnv, or jwkPath",
		},
		{
			name: "auth invalid provider",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: validArtifact(),
				Auth: &Auth{Hosts: []AuthHost{{Host: "h.example", JWT: &AuthJWT{
					Provider: "gitlab",
				}}}}},
			errMsg: "must be one of: github, forgejo",
		},
		{
			name: "auth jwkPath missing iss",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: validArtifact(),
				Auth: &Auth{Hosts: []AuthHost{{Host: "h.example", JWT: &AuthJWT{
					JWKPath: "/k.json", Sub: "client",
				}}}}},
			errMsg: "iss is required with jwkPath",
		},
		{
			name: "auth jwkPath missing sub",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: validArtifact(),
				Auth: &Auth{Hosts: []AuthHost{{Host: "h.example", JWT: &AuthJWT{
					JWKPath: "/k.json", Iss: "https://issuer",
				}}}}},
			errMsg: "sub is required with jwkPath",
		},
		{
			name: "auth iss set without jwkPath",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: validArtifact(),
				Auth: &Auth{Hosts: []AuthHost{{Host: "h.example", JWT: &AuthJWT{
					Provider: JWTProviderGitHub, Iss: "https://issuer",
				}}}}},
			errMsg: "iss and sub can only be set with jwkPath",
		},
		{
			name: "auth aud set with fromEnv",
			cfg: Config{APIVersion: APIVersion, Kind: Kind, Artifacts: validArtifact(),
				Auth: &Auth{Hosts: []AuthHost{{Host: "h.example", JWT: &AuthJWT{
					FromEnv: "TOKEN", Aud: "nope",
				}}}}},
			errMsg: "aud can only be set with jwkPath or provider",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			err := tt.cfg.Validate()
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
