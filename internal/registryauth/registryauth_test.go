// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	gojwt "github.com/golang-jwt/jwt/v5"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta2"
)

func authHostsConfig(hosts ...string) *apiv1.Config {
	cfg := &apiv1.Config{}
	for _, h := range hosts {
		cfg.Hosts = append(cfg.Hosts, apiv1.RegistryHost{
			Host:       h,
			Credential: &apiv1.RegistryCredential{Type: apiv1.CredentialTypeJWT, Value: "X"},
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
		Host: "bearer.example", Credential: &apiv1.RegistryCredential{Type: apiv1.CredentialTypeJWT, Value: "X"},
	}}
	userpass := []apiv1.RegistryHost{{
		Host: "userpass.example", Credential: &apiv1.RegistryCredential{Type: apiv1.CredentialTypeJWT, Value: "X", Username: "robot"},
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

	fn, err := providerTokenFunc(apiv1.JWTProviderGCP, []string{"registry.example.com"}, true)
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
	withCred := apiv1.RegistryHost{Host: "c.example", Credential: &apiv1.RegistryCredential{Type: apiv1.CredentialTypeJWT, Value: "X"}}
	withProvider := apiv1.RegistryHost{Host: "p.example", Provider: apiv1.RegistryProviderECR}
	// The generic provider is not a credential source: it behaves like an unset
	// provider, so a generic host needs a credential to yield one.
	withGenericOnly := apiv1.RegistryHost{Host: "g.example", Provider: apiv1.RegistryProviderGeneric}
	withGenericCred := apiv1.RegistryHost{Host: "gc.example", Provider: apiv1.RegistryProviderGeneric,
		Credential: &apiv1.RegistryCredential{Type: apiv1.CredentialTypeJWT, Value: "X"}}

	g.Expect(HasCredential(tlsOnly)).To(BeFalse())
	g.Expect(HasCredential(withCred)).To(BeTrue())
	g.Expect(HasCredential(withProvider)).To(BeTrue())
	g.Expect(HasCredential(withGenericOnly)).To(BeFalse())
	g.Expect(HasCredential(withGenericCred)).To(BeTrue())

	// A TLS-only host returns a clear error instead of panicking.
	_, err := ResolveHostAuth(context.Background(), tlsOnly)
	g.Expect(err).To(MatchError(ContainSubstring("no credential or provider configured")))
}

func TestResolveCredential_Envelope(t *testing.T) {
	g := NewWithT(t)
	h := apiv1.RegistryHost{Host: "h.example", Credential: &apiv1.RegistryCredential{Type: apiv1.CredentialTypeJWT,
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
		{Host: "h.example", Credential: &apiv1.RegistryCredential{Type: apiv1.CredentialTypeJWT,
			Value: "abc", Envelope: "token-{{ hex .Token }}",
		}},
		{Host: "plain.example", Credential: &apiv1.RegistryCredential{Type: apiv1.CredentialTypeJWT, Value: "abc"}},
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
		Host: "bad.example", Credential: &apiv1.RegistryCredential{Type: apiv1.CredentialTypeJWT,
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
		Credential: &apiv1.RegistryCredential{Type: apiv1.CredentialTypeJWT, Value: "abc", Envelope: "token-{{ hex .Token }}"},
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
		Credential: &apiv1.RegistryCredential{Type: apiv1.CredentialTypeJWT,
			JWKValue:   string(jwk),
			Issuer:     "https://issuer.example",
			Subject:    "client-id",
			Expiration: &metav1.Duration{Duration: time.Hour},
			Envelope:   "token-{{ .Token }}",
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

// TestJWKTokenFunc_MultipleAudiences proves a jwk-signed JWT carries every
// configured audience as a JSON array (RFC 7519).
func TestJWKTokenFunc_MultipleAudiences(t *testing.T) {
	g := NewWithT(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	g.Expect(err).ToNot(HaveOccurred())
	jwk, err := json.Marshal(jose.JSONWebKey{Key: priv, KeyID: "k", Algorithm: "EdDSA"})
	g.Expect(err).ToNot(HaveOccurred())

	fn, err := jwkTokenFunc(string(jwk), "https://issuer.example", "client-id",
		[]string{"a.example", "b.example"}, time.Minute)
	g.Expect(err).ToNot(HaveOccurred())
	token, err := fn(context.Background())
	g.Expect(err).ToNot(HaveOccurred())

	claims := gojwt.MapClaims{}
	_, _, err = gojwt.NewParser().ParseUnverified(token, claims)
	g.Expect(err).ToNot(HaveOccurred())
	aud, err := claims.GetAudience()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect([]string(aud)).To(Equal([]string{"a.example", "b.example"}))
}

// staticCredentialsProvider is an aws.CredentialsProvider that returns fixed
// credentials, so signing works without contacting IMDS.
type staticCredentialsProvider struct{ creds aws.Credentials }

func (p staticCredentialsProvider) Retrieve(context.Context) (aws.Credentials, error) {
	return p.creds, nil
}

// TestMintAWSSTSToken_Audiences proves the envelope carries every configured
// audience both as repeated X-Audience header values covered by the SigV4
// signature and as an aud array — including the single-audience case, where aud
// is still an array rather than a bare string.
func TestMintAWSSTSToken_Audiences(t *testing.T) {
	creds := staticCredentialsProvider{creds: aws.Credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "secret",
	}}

	for _, tc := range []struct {
		name      string
		audiences []string
	}{
		{name: "single audience", audiences: []string{"a.example"}},
		{name: "multiple audiences", audiences: []string{"a.example", "b.example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			token, err := mintAWSSTSToken(context.Background(), creds, v4.NewSigner(), "us-east-1", tc.audiences)
			g.Expect(err).ToNot(HaveOccurred())

			parts := strings.Split(token, ".")
			g.Expect(parts).To(HaveLen(3))
			claimsRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
			g.Expect(err).ToNot(HaveOccurred())
			var claims struct {
				Aud []string `json:"aud"`
				STS struct {
					Method  string              `json:"method"`
					URL     string              `json:"url"`
					Headers map[string][]string `json:"headers"`
					Body    string              `json:"body"`
				} `json:"sts"`
			}
			g.Expect(json.Unmarshal(claimsRaw, &claims)).To(Succeed())
			g.Expect(claims.Aud).To(Equal(tc.audiences))
			g.Expect(claims.STS.Headers["X-Audience"]).To(Equal(tc.audiences))

			// The X-Audience header must be covered by the SigV4 signature;
			// otherwise the replay pinning it provides could be stripped.
			authz := claims.STS.Headers["Authorization"]
			g.Expect(authz).To(HaveLen(1))
			var signedHeaders []string
			for _, part := range strings.Split(authz[0], ",") {
				if v, ok := strings.CutPrefix(strings.TrimSpace(part), "SignedHeaders="); ok {
					signedHeaders = strings.Split(v, ";")
				}
			}
			g.Expect(signedHeaders).To(ContainElement("x-audience"))

			// Re-sign the request reconstructed from the envelope with the same
			// credentials and signing time. The signature must match, proving
			// every X-Audience value (not just the header name) is part of the
			// canonical request.
			req, err := http.NewRequest(claims.STS.Method, claims.STS.URL, strings.NewReader(claims.STS.Body))
			g.Expect(err).ToNot(HaveOccurred())
			for k, vs := range claims.STS.Headers {
				req.Header[k] = vs
			}
			signedAt, err := time.Parse("20060102T150405Z", req.Header.Get("X-Amz-Date"))
			g.Expect(err).ToNot(HaveOccurred())
			sum := sha256.Sum256([]byte(claims.STS.Body))
			g.Expect(v4.NewSigner().SignHTTP(context.Background(), creds.creds, req,
				hex.EncodeToString(sum[:]), "sts", "us-east-1", signedAt)).To(Succeed())
			g.Expect(req.Header.Get("Authorization")).To(Equal(authz[0]))
		})
	}
}

// TestProviderTokenFunc_AudienceGuards proves providerTokenFunc enforces its
// audience contract independently of config validation: at least one audience
// for every provider, and at most one for the single-audience providers.
func TestProviderTokenFunc_AudienceGuards(t *testing.T) {
	g := NewWithT(t)

	_, err := providerTokenFunc(apiv1.JWTProviderGitHub, nil, false)
	g.Expect(err).To(MatchError(ContainSubstring("requires at least one audience")))

	for _, provider := range []string{apiv1.JWTProviderGCP, apiv1.JWTProviderAzure} {
		_, err := providerTokenFunc(provider, []string{"a.example", "b.example"}, true)
		g.Expect(err).To(MatchError(ContainSubstring(`provider "` + provider + `" accepts at most one audience`)))
	}
}

// TestProviderTokenFunc_ExactProvider proves provider values are matched as
// written: an unpadded value dispatches, a padded one does not.
func TestProviderTokenFunc_ExactProvider(t *testing.T) {
	g := NewWithT(t)

	fn, err := providerTokenFunc(apiv1.JWTProviderGitHub, []string{"a.example"}, false)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(fn).ToNot(BeNil())

	_, err = providerTokenFunc(" "+apiv1.JWTProviderGitHub+" ", []string{"a.example"}, false)
	g.Expect(err).To(MatchError(ContainSubstring("unknown provider")))
}

// TestPkgAuthProviderName_ExactProvider proves the registry provider translation
// matches the value as written.
func TestPkgAuthProviderName_ExactProvider(t *testing.T) {
	g := NewWithT(t)

	for raw, want := range map[string]string{
		"ecr": "aws",
		"acr": "azure",
		"gar": "gcp",
	} {
		got, err := pkgAuthProviderName(raw)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got).To(Equal(want))
	}

	for _, raw := range []string{"s3", " ecr "} {
		_, err := pkgAuthProviderName(raw)
		g.Expect(err).To(MatchError(ContainSubstring("unknown registry provider")))
	}
}

// TestGenericProviderHostResolution proves a generic (default) provider host is
// not a cloud provider and resolves its credential through the credential path:
// as username/password when credential.username is set, else as a bearer token.
func TestGenericProviderHostResolution(t *testing.T) {
	g := NewWithT(t)

	userpass := apiv1.RegistryHost{
		Host:     "h.example",
		Provider: apiv1.RegistryProviderGeneric,
		Credential: &apiv1.RegistryCredential{
			Type:     apiv1.CredentialTypeJWT,
			Value:    "X",
			Username: "robot",
		},
	}
	g.Expect(userpass.IsCloudProvider()).To(BeFalse())
	g.Expect(HasCredential(userpass)).To(BeTrue())
	ha, err := ResolveHostAuth(context.Background(), userpass)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(ha.Username).To(Equal("robot"))
	g.Expect(ha.Password).To(Equal("X"))
	g.Expect(ha.RegistryToken).To(BeEmpty())

	bearer := apiv1.RegistryHost{
		Host:     "h.example",
		Provider: apiv1.RegistryProviderGeneric,
		Credential: &apiv1.RegistryCredential{
			Type:  apiv1.CredentialTypeJWT,
			Value: "X",
		},
	}
	g.Expect(HasCredential(bearer)).To(BeTrue())
	ha, err = ResolveHostAuth(context.Background(), bearer)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(ha.RegistryToken).To(Equal("X"))
	g.Expect(ha.Username).To(BeEmpty())

	// The generic provider alone yields no credential, like an unset provider.
	genericOnly := apiv1.RegistryHost{Host: "h.example", Provider: apiv1.RegistryProviderGeneric}
	g.Expect(HasCredential(genericOnly)).To(BeFalse())
	_, err = ResolveHostAuth(context.Background(), genericOnly)
	g.Expect(err).To(MatchError(ContainSubstring("has no credential or provider")))
}
