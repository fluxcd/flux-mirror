// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package v1beta2

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1beta1 "github.com/fluxcd/flux-mirror/api/v1beta1"
)

func TestFromV1Beta1(t *testing.T) {
	g := NewWithT(t)

	in := &apiv1beta1.Config{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1beta1.GroupVersion.String(),
			Kind:       apiv1beta1.ConfigKind,
		},
		Hosts: []apiv1beta1.RegistryHost{{
			Host:     "registry.example.com",
			Username: "robot",
			Credential: &apiv1beta1.RegistryCredential{
				Provider: apiv1beta1.JWTProviderJWTSVID,
				Iss:      "https://issuer.example",
				Sub:      "client-id",
				Aud:      "custom-aud",
				Exp:      &metav1.Duration{Duration: time.Hour},
			},
			MaxChunkSize: 1024,
		}},
		Charts: []apiv1beta1.ChartEntry{{
			Source:      "https://charts.example.com",
			Destination: "oci://ghcr.io/x",
			Name:        "foo",
			Version:     ">=1.0.0",
			Limit:       new(5),
			Overwrite:   true,
		}},
		Artifacts: []apiv1beta1.ArtifactEntry{{
			Source:      "ghcr.io/a/b",
			Destination: "ghcr.io/c/d",
			Selector: apiv1beta1.Selector{
				Regex:  &apiv1beta1.RegexFilter{Pattern: ".*", Extract: "v"},
				SortBy: apiv1beta1.SortByNumerical,
			},
			Overwrite:        true,
			IncludeReferrers: true,
			Verify: &apiv1beta1.ArtifactVerification{
				Provider: apiv1beta1.VerifyProviderCosign,
				MinAge:   &metav1.Duration{Duration: time.Hour},
				MatchOIDCIdentity: []apiv1beta1.OIDCIdentity{{
					Issuer:  "https://issuer.example",
					Subject: "subject",
				}},
			},
		}},
	}

	out, err := FromV1Beta1(in)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(out.APIVersion).To(Equal(GroupVersion.String()))
	g.Expect(out.Kind).To(Equal(ConfigKind))

	g.Expect(out.Hosts).To(HaveLen(1))
	h := out.Hosts[0]
	g.Expect(h.Host).To(Equal("registry.example.com"))
	g.Expect(h.MaxChunkSize).To(Equal(1024))
	g.Expect(h.Credential.Type).To(Equal(CredentialTypeJWT))
	g.Expect(h.Credential.Provider).To(Equal(JWTProviderSPIFFE))
	g.Expect(h.Credential.Username).To(Equal("robot"))
	g.Expect(h.Credential.Issuer).To(Equal("https://issuer.example"))
	g.Expect(h.Credential.Subject).To(Equal("client-id"))
	g.Expect(h.Credential.Audiences).To(Equal([]string{"custom-aud"}))
	g.Expect(h.Credential.Expiration.Duration).To(Equal(time.Hour))
	g.Expect(h.TLS).To(BeNil())

	g.Expect(out.Charts).To(HaveLen(1))
	g.Expect(out.Charts[0].Name).To(Equal("foo"))
	g.Expect(*out.Charts[0].Limit).To(Equal(5))
	g.Expect(out.Charts[0].Overwrite).To(BeTrue())

	g.Expect(out.Artifacts).To(HaveLen(1))
	g.Expect(out.Artifacts[0].IncludeReferrers).To(BeTrue())
	g.Expect(out.Artifacts[0].Selector.Regex.Pattern).To(Equal(".*"))
	g.Expect(out.Artifacts[0].Selector.SortBy).To(Equal(SortByNumerical))
	g.Expect(out.Artifacts[0].Verify.Provider).To(Equal(VerifyProviderCosign))
	g.Expect(out.Artifacts[0].Verify.MinAge.Duration).To(Equal(time.Hour))
	g.Expect(out.Artifacts[0].Verify.MatchOIDCIdentity).To(Equal([]OIDCIdentity{{
		Issuer:  "https://issuer.example",
		Subject: "subject",
	}}))
}

func TestFromV1Beta1_Nil(t *testing.T) {
	g := NewWithT(t)
	out, err := FromV1Beta1(nil)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(out).To(BeNil())
}

// TestFromV1Beta1_TLSInline proves v1beta1 inline certificate values map onto
// the v1beta2 inline TLS fields.
func TestFromV1Beta1_TLSInline(t *testing.T) {
	g := NewWithT(t)
	out, err := FromV1Beta1(&apiv1beta1.Config{
		Hosts: []apiv1beta1.RegistryHost{{
			Host: "registry.example.com",
			TLS: &apiv1beta1.TLS{
				ServerAuth: &apiv1beta1.TLSServerAuth{Value: "CA"},
				ClientAuth: &apiv1beta1.TLSClientAuth{
					Certificate: &apiv1beta1.TLSData{Value: "CERT"},
					Key:         &apiv1beta1.TLSKey{Value: "KEY"},
				},
			},
		}},
	})
	g.Expect(err).ToNot(HaveOccurred())
	tls := out.Hosts[0].TLS
	g.Expect(tls.Path).To(BeEmpty())
	g.Expect(tls.SPIFFE).To(BeNil())
	g.Expect(tls.CACert).To(Equal("CA"))
	g.Expect(tls.TLSCert).To(Equal("CERT"))
	g.Expect(tls.TLSKey).To(Equal("KEY"))
}

// TestFromV1Beta1_TLSSPIFFE proves the v1beta1 SPIFFE server authorizer maps to
// tls.spiffe.serverIDs.
func TestFromV1Beta1_TLSSPIFFE(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   *apiv1beta1.SPIFFETLS
		want []string
	}{
		{
			"serverID",
			&apiv1beta1.SPIFFETLS{ServerID: "spiffe://example.org/registry"},
			[]string{"spiffe://example.org/registry"},
		},
		{"trustDomain", &apiv1beta1.SPIFFETLS{TrustDomain: "example.org"}, []string{"spiffe://example.org"}},
		{"self", &apiv1beta1.SPIFFETLS{TrustDomain: apiv1beta1.TrustDomainSelf}, []string{"spiffe://self"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			out, err := FromV1Beta1(&apiv1beta1.Config{
				Hosts: []apiv1beta1.RegistryHost{{
					Host: "registry.example.com",
					TLS: &apiv1beta1.TLS{
						ServerAuth: &apiv1beta1.TLSServerAuth{SPIFFE: tc.in},
						ClientAuth: &apiv1beta1.TLSClientAuth{Provider: apiv1beta1.TLSClientProviderX509SVID},
					},
				}},
			})
			g.Expect(err).ToNot(HaveOccurred())
			tls := out.Hosts[0].TLS
			g.Expect(tls.Path).To(BeEmpty())
			g.Expect(tls.CACert).To(BeEmpty())
			g.Expect(tls.SPIFFE.ServerIDs).To(Equal(tc.want))
		})
	}
}

// TestFromV1Beta1_RejectsUnmappableTLS covers the v1beta1 TLS shapes with no
// v1beta2 equivalent.
func TestFromV1Beta1_RejectsUnmappableTLS(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tls     *apiv1beta1.TLS
		errText string
	}{
		{
			name:    "gives up on per-file server CA",
			tls:     &apiv1beta1.TLS{ServerAuth: &apiv1beta1.TLSServerAuth{FromPath: "/ca.crt"}},
			errText: "serverAuth.fromPath cannot be converted",
		},
		{
			name: "gives up on per-file client cert",
			tls: &apiv1beta1.TLS{ClientAuth: &apiv1beta1.TLSClientAuth{
				Certificate: &apiv1beta1.TLSData{FromPath: "/tls.crt"},
				Key:         &apiv1beta1.TLSKey{Value: "KEY"},
			}},
			errText: "clientAuth.certificate.fromPath cannot be converted",
		},
		{
			name: "gives up on one-sided SPIFFE",
			tls: &apiv1beta1.TLS{ServerAuth: &apiv1beta1.TLSServerAuth{
				SPIFFE: &apiv1beta1.SPIFFETLS{TrustDomain: apiv1beta1.TrustDomainSelf},
			}},
			errText: "requires clientAuth.provider x509-svid",
		},
		{
			name: "gives up on authorizeAny",
			tls: &apiv1beta1.TLS{
				ServerAuth: &apiv1beta1.TLSServerAuth{SPIFFE: &apiv1beta1.SPIFFETLS{AuthorizeAny: true}},
				ClientAuth: &apiv1beta1.TLSClientAuth{Provider: apiv1beta1.TLSClientProviderX509SVID},
			},
			errText: "authorizeAny cannot be converted",
		},
		{
			name: "gives up on mixed SPIFFE and value",
			tls: &apiv1beta1.TLS{
				ServerAuth: &apiv1beta1.TLSServerAuth{
					Value:  "CA",
					SPIFFE: &apiv1beta1.SPIFFETLS{TrustDomain: apiv1beta1.TrustDomainSelf},
				},
				ClientAuth: &apiv1beta1.TLSClientAuth{Provider: apiv1beta1.TLSClientProviderX509SVID},
			},
			errText: "cannot be combined",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			_, err := FromV1Beta1(&apiv1beta1.Config{
				Hosts: []apiv1beta1.RegistryHost{{Host: "registry.example.com", TLS: tc.tls}},
			})
			g.Expect(err).To(MatchError(ContainSubstring(tc.errText)))
		})
	}
}

// TestFromV1Beta1_RejectsUsernameWithoutCredential proves conversion does not
// silently drop a host-level username that v1beta1 validation would have
// rejected.
func TestFromV1Beta1_RejectsUsernameWithoutCredential(t *testing.T) {
	g := NewWithT(t)
	_, err := FromV1Beta1(&apiv1beta1.Config{
		Hosts: []apiv1beta1.RegistryHost{{
			Host:     "registry.example.com",
			Username: "robot",
			TLS:      &apiv1beta1.TLS{},
		}},
	})
	g.Expect(err).To(MatchError(ContainSubstring("username requires credential")))
}

// TestFromV1Beta1_PreservesKind proves the conversion carries the declared kind
// over unchanged, so Validate can reject a mismatched one.
func TestFromV1Beta1_PreservesKind(t *testing.T) {
	g := NewWithT(t)
	out, err := FromV1Beta1(&apiv1beta1.Config{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1beta1.GroupVersion.String(),
			Kind:       "NotConfig",
		},
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(out.Kind).To(Equal("NotConfig"))
}
