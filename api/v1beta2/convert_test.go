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
			TLS: &apiv1beta1.TLS{
				ServerAuth: &apiv1beta1.TLSServerAuth{
					SPIFFE: &apiv1beta1.SPIFFETLS{TrustDomain: apiv1beta1.TrustDomainSelf},
				},
				ClientAuth: &apiv1beta1.TLSClientAuth{Provider: apiv1beta1.TLSClientProviderX509SVID},
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

	out := FromV1Beta1(in)
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
	g.Expect(h.TLS.ServerAuth.Provider).To(Equal(TLSProviderSPIFFE))
	g.Expect(h.TLS.ServerAuth.SPIFFE.TrustDomain).To(Equal(TrustDomainSelf))
	g.Expect(h.TLS.ClientAuth.Provider).To(Equal(TLSProviderSPIFFE))

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
	g.Expect(FromV1Beta1(nil)).To(BeNil())
}
