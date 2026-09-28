// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta1"
)

func authHostsConfig(hosts ...string) *apiv1.Config {
	cfg := &apiv1.Config{}
	for _, h := range hosts {
		cfg.Hosts = append(cfg.Hosts, apiv1.RegistryHost{
			Host:       h,
			Credential: &apiv1.RegistryCredential{Value: "X"},
		})
	}
	return cfg
}

func TestSelectAuthHosts_AllByDefault(t *testing.T) {
	g := NewWithT(t)
	hosts, err := SelectAuthHosts(authHostsConfig("a.example", "b.example"), nil)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(hosts).To(HaveLen(2))
}

func TestSelectAuthHosts_Filter(t *testing.T) {
	g := NewWithT(t)
	cfg := authHostsConfig("a.example", "b.example", "c.example")
	hosts, err := SelectAuthHosts(cfg, []string{"b.example", "c.example"})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(hosts).To(HaveLen(2))
	g.Expect(hosts[0].Host).To(Equal("b.example"))
	g.Expect(hosts[1].Host).To(Equal("c.example"))
}

func TestSelectAuthHosts_UnknownHost(t *testing.T) {
	g := NewWithT(t)
	_, err := SelectAuthHosts(authHostsConfig("a.example"), []string{"missing.example"})
	g.Expect(err).To(MatchError(ContainSubstring(`host "missing.example" not found`)))
}

func TestSelectAuthHosts_NoAuth(t *testing.T) {
	g := NewWithT(t)
	_, err := SelectAuthHosts(&apiv1.Config{}, nil)
	g.Expect(err).To(MatchError(ContainSubstring("no hosts")))
}

func TestUsernameSwitch(t *testing.T) {
	g := NewWithT(t)

	bearer := []apiv1.RegistryHost{{
		Host: "bearer.example", Credential: &apiv1.RegistryCredential{Value: "X"},
	}}
	userpass := []apiv1.RegistryHost{{
		Host: "userpass.example", Username: "robot", Credential: &apiv1.RegistryCredential{Value: "X"},
	}}

	// cijwt (bearer-stamp) transport is needed only when a credential host has
	// no username.
	g.Expect(NeedsCredentialTransport(bearer)).To(BeTrue())
	g.Expect(NeedsCredentialTransport(userpass)).To(BeFalse())

	// A username credential is skipped by the cijwt options (goes via keychain).
	t.Setenv("X", "tok")
	opts, err := JWTTransportOptions(nil, userpass)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(opts).To(HaveLen(1)) // only WithInner; the host is skipped

	// ...and is served by the keychain instead.
	kc, err := BuildKeychain(context.Background(), userpass)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(kc).ToNot(BeNil())
}

// TestProviderTokenFunc_GCPUserCredentialRejectsAud proves the GCP provider
// rejects an explicitly configured audience when ADC resolves to user
// credentials, whose ID token is always minted for the gcloud OAuth client ID
// rather than the requested audience.
func TestProviderTokenFunc_GCPUserCredentialRejectsAud(t *testing.T) {
	g := NewWithT(t)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", writeAuthorizedUserCreds(t))
	t.Setenv("GOOGLE_API_GO_EXPERIMENTAL_ENABLE_NEW_AUTH_LIB", "false")

	fn, err := providerTokenFunc(apiv1.JWTProviderGCP, "registry.example.com", true)
	g.Expect(err).ToNot(HaveOccurred())
	_, err = fn(context.Background())
	g.Expect(err).To(MatchError(ContainSubstring("cannot be honored with GCP user credentials")))
}

func writeAuthorizedUserCreds(t *testing.T) string {
	t.Helper()
	g := NewWithT(t)
	path := filepath.Join(t.TempDir(), "adc.json")
	body := `{"type":"authorized_user","client_id":"x.apps.googleusercontent.com",` +
		`"client_secret":"secret","refresh_token":"refresh"}`
	g.Expect(os.WriteFile(path, []byte(body), 0o600)).To(Succeed())
	return path
}

func TestPkgAuthProviderName(t *testing.T) {
	g := NewWithT(t)
	cases := map[string]string{
		apiv1.RegistryProviderECR: "aws",
		apiv1.RegistryProviderACR: "azure",
		apiv1.RegistryProviderGAR: "gcp",
	}
	for in, want := range cases {
		got, err := pkgAuthProviderName(in)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got).To(Equal(want))
	}

	_, err := pkgAuthProviderName("dockerhub")
	g.Expect(err).To(MatchError(ContainSubstring("unknown registry provider")))
}

func TestHasCredentialAndTLSOnly(t *testing.T) {
	g := NewWithT(t)

	tlsOnly := apiv1.RegistryHost{Host: "tls.example", TLS: &apiv1.TLS{
		ServerAuth: &apiv1.TLSServerAuth{Value: "x"},
	}}
	withCred := apiv1.RegistryHost{Host: "c.example", Credential: &apiv1.RegistryCredential{Value: "X"}}
	withProvider := apiv1.RegistryHost{Host: "p.example", Provider: apiv1.RegistryProviderECR}

	g.Expect(HasCredential(tlsOnly)).To(BeFalse())
	g.Expect(HasCredential(withCred)).To(BeTrue())
	g.Expect(HasCredential(withProvider)).To(BeTrue())

	// A TLS-only host returns a clear error instead of panicking.
	_, err := ResolveHostAuth(context.Background(), tlsOnly)
	g.Expect(err).To(MatchError(ContainSubstring("no credential or provider configured")))
}

func TestResolveCredential_Envelope(t *testing.T) {
	g := NewWithT(t)
	h := apiv1.RegistryHost{Host: "h.example", Credential: &apiv1.RegistryCredential{
		Value:    "abc",
		Envelope: "token-{{ hex .Token }}",
	}}
	got, err := resolveCredential(context.Background(), h)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(got).To(Equal("token-616263"))
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEnvelopeTransport(t *testing.T) {
	g := NewWithT(t)
	hosts := []apiv1.RegistryHost{
		{Host: "h.example", Credential: &apiv1.RegistryCredential{
			Value: "abc", Envelope: "token-{{ hex .Token }}",
		}},
		{Host: "plain.example", Credential: &apiv1.RegistryCredential{Value: "abc"}},
	}

	var got http.Header
	inner := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r.Header.Clone()
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})
	et, err := newEnvelopeTransport(inner, hosts)
	g.Expect(err).ToNot(HaveOccurred())

	req, err := http.NewRequest(http.MethodGet, "https://h.example/v2/", nil)
	g.Expect(err).ToNot(HaveOccurred())
	req.Header.Set("Authorization", "Bearer abc")
	_, err = et.RoundTrip(req)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(got.Get("Authorization")).To(Equal("Bearer token-616263"))
	g.Expect(req.Header.Get("Authorization")).To(Equal("Bearer abc")) // caller's request is not mutated.

	// A host without an envelope passes through unchanged.
	req, _ = http.NewRequest(http.MethodGet, "https://plain.example/v2/", nil)
	req.Header.Set("Authorization", "Bearer abc")
	_, err = et.RoundTrip(req)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(got.Get("Authorization")).To(Equal("Bearer abc"))

	// A non-bearer Authorization on an enveloped host passes through unchanged.
	req, _ = http.NewRequest(http.MethodGet, "https://h.example/v2/", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	_, err = et.RoundTrip(req)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(got.Get("Authorization")).To(Equal("Basic dXNlcjpwYXNz"))

	// A malformed envelope fails when the transport is built.
	_, err = newEnvelopeTransport(inner, []apiv1.RegistryHost{{
		Host: "bad.example", Credential: &apiv1.RegistryCredential{
			Provider: apiv1.JWTProviderGitHub, Envelope: "{{ nope .Token }}",
		},
	}})
	g.Expect(err).To(MatchError(ContainSubstring("parse envelope")))
}

func TestNewCredentialTransport_EnvelopeStatic(t *testing.T) {
	g := NewWithT(t)
	var got string
	inner := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r.Header.Get("Authorization")
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})
	tr, err := NewCredentialTransport(inner, []apiv1.RegistryHost{{
		Host:       "h.example",
		Credential: &apiv1.RegistryCredential{Value: "abc", Envelope: "token-{{ hex .Token }}"},
	}})
	g.Expect(err).ToNot(HaveOccurred())

	req, err := http.NewRequest(http.MethodGet, "https://h.example/v2/", nil)
	g.Expect(err).ToNot(HaveOccurred())
	_, err = tr.RoundTrip(req)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(got).To(Equal("Bearer token-616263"))
}

// TestNewCredentialTransport_EnvelopePreservesExpParsing proves the cijwt
// transport still sees the raw JWT (needed to parse its exp claim) while the
// wire carries the enveloped string.
func TestNewCredentialTransport_EnvelopePreservesExpParsing(t *testing.T) {
	g := NewWithT(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	g.Expect(err).ToNot(HaveOccurred())
	jwk, err := json.Marshal(jose.JSONWebKey{Key: priv, KeyID: "k", Algorithm: "EdDSA"})
	g.Expect(err).ToNot(HaveOccurred())

	var got string
	inner := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r.Header.Get("Authorization")
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})
	tr, err := NewCredentialTransport(inner, []apiv1.RegistryHost{{
		Host: "h.example",
		Credential: &apiv1.RegistryCredential{
			JWKValue: string(jwk),
			Iss:      "https://issuer.example",
			Sub:      "client-id",
			Exp:      &metav1.Duration{Duration: time.Hour},
			Envelope: "token-{{ .Token }}",
		},
	}})
	g.Expect(err).ToNot(HaveOccurred())

	req, err := http.NewRequest(http.MethodGet, "https://h.example/v2/", nil)
	g.Expect(err).ToNot(HaveOccurred())
	_, err = tr.RoundTrip(req)
	g.Expect(err).ToNot(HaveOccurred())

	raw, ok := strings.CutPrefix(got, "Bearer token-")
	g.Expect(ok).To(BeTrue(), "envelope prefix should have been applied")
	_, err = jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.EdDSA})
	g.Expect(err).ToNot(HaveOccurred())
}
