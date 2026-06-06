// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	. "github.com/onsi/gomega"
	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"google.golang.org/grpc"

	"github.com/fluxcd/flux-mirror/internal/config"
)

const (
	testTrustDomain = "example.org"
	testServerID    = "spiffe://example.org/server"
	testClientID    = "spiffe://example.org/client"
)

// fakeWorkloadAPI is a minimal SPIFFE Workload API server: it hands out a fixed
// client X.509-SVID + trust bundle and mints unsigned-validation JWT-SVIDs.
type fakeWorkloadAPI struct {
	workload.UnimplementedSpiffeWorkloadAPIServer

	clientCertDER []byte
	clientKeyDER  []byte
	bundleDER     []byte
	jwtKey        *ecdsa.PrivateKey
}

func (f *fakeWorkloadAPI) FetchX509SVID(_ *workload.X509SVIDRequest, stream workload.SpiffeWorkloadAPI_FetchX509SVIDServer) error {
	resp := &workload.X509SVIDResponse{Svids: []*workload.X509SVID{{
		SpiffeId:    testClientID,
		X509Svid:    f.clientCertDER,
		X509SvidKey: f.clientKeyDER,
		Bundle:      f.bundleDER,
	}}}
	if err := stream.Send(resp); err != nil {
		return err
	}
	<-stream.Context().Done() // keep the stream open until the client disconnects
	return nil
}

func (f *fakeWorkloadAPI) FetchJWTSVID(_ context.Context, req *workload.JWTSVIDRequest) (*workload.JWTSVIDResponse, error) {
	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: f.jwtKey},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return nil, err
	}
	token, err := jwt.Signed(sig).Claims(jwt.Claims{
		Subject:  testClientID,
		Audience: jwt.Audience(req.Audience),
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).Serialize()
	if err != nil {
		return nil, err
	}
	return &workload.JWTSVIDResponse{Svids: []*workload.JWTSVID{{
		SpiffeId: testClientID,
		Svid:     token,
	}}}, nil
}

func (f *fakeWorkloadAPI) FetchJWTBundles(_ *workload.JWTBundlesRequest, stream workload.SpiffeWorkloadAPI_FetchJWTBundlesServer) error {
	jwk := jose.JSONWebKey{Key: &f.jwtKey.PublicKey, KeyID: "k"}
	keyJSON, err := jwk.MarshalJSON()
	if err != nil {
		return err
	}
	jwks := []byte(`{"keys":[` + string(keyJSON) + `]}`)
	if err := stream.Send(&workload.JWTBundlesResponse{
		Bundles: map[string][]byte{testTrustDomain: jwks},
	}); err != nil {
		return err
	}
	<-stream.Context().Done() // keep the stream open until the client disconnects
	return nil
}

// startFakeWorkloadAPI serves the fake over a temp UDS and points
// SPIFFE_ENDPOINT_SOCKET at it for the duration of the test.
func startFakeWorkloadAPI(t *testing.T, ca *testCA) {
	t.Helper()
	g := NewWithT(t)

	clientCertPEM, clientKeyPEM := ca.issue(t, "client", nil, []*url.URL{mustURL(t, testClientID)})
	jwtKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	g.Expect(err).ToNot(HaveOccurred())

	fake := &fakeWorkloadAPI{
		clientCertDER: pemToDER(t, clientCertPEM),
		clientKeyDER:  pemToDER(t, clientKeyPEM),
		bundleDER:     pemToDER(t, ca.certPEM),
		jwtKey:        jwtKey,
	}

	sock := filepath.Join(t.TempDir(), "agent.sock")
	lis, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", sock)
	g.Expect(err).ToNot(HaveOccurred())

	srv := grpc.NewServer()
	workload.RegisterSpiffeWorkloadAPIServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	t.Setenv("SPIFFE_ENDPOINT_SOCKET", "unix://"+sock)
}

// TestSPIFFE_MTLSEndToEnd drives a real X.509-SVID mTLS handshake: a SPIFFE
// server SVID on the server side, flux-mirror's tls.spiffe transport (fed by the
// fake Workload API) on the client side, exercising trustDomain: self, an exact
// serverID, and a serverID mismatch.
func TestSPIFFE_MTLSEndToEnd(t *testing.T) {
	g := NewWithT(t)
	ca := newTestCA(t)
	startFakeWorkloadAPI(t, ca)

	serverCertPEM, serverKeyPEM := ca.issue(t, "server", nil, []*url.URL{mustURL(t, testServerID)})
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	g.Expect(err).ToNot(HaveOccurred())
	caPool := x509.NewCertPool()
	g.Expect(caPool.AppendCertsFromPEM(ca.certPEM)).To(BeTrue())

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	srv.StartTLS()
	defer srv.Close()
	host := mustHostname(t, srv.URL)

	cases := []struct {
		name   string
		spiffe *config.SPIFFETLS
		wantOK bool
	}{
		{"trustDomain self", &config.SPIFFETLS{TrustDomain: config.TrustDomainSelf}, true},
		{"exact serverID", &config.SPIFFETLS{ServerID: testServerID}, true},
		{"serverID mismatch", &config.SPIFFETLS{ServerID: "spiffe://example.org/wrong"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			rt, closeFn, err := NewTLSTransport(context.Background(), http.DefaultTransport,
				[]config.AuthHost{{Host: host, TLS: &config.TLS{
					ClientAuth: &config.TLSClientAuth{Provider: config.TLSClientProviderX509SVID},
					ServerAuth: &config.TLSServerAuth{SPIFFE: tc.spiffe},
				}}})
			g.Expect(err).ToNot(HaveOccurred())
			defer closeFn()

			resp, err := rt.RoundTrip(mustGet(t, srv.URL))
			if tc.wantOK {
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(resp.StatusCode).To(Equal(http.StatusNoContent))
			} else {
				g.Expect(err).To(HaveOccurred())
			}
		})
	}
}

// TestSPIFFE_ClientOnly drives mTLS where the client presents a SPIFFE X.509-SVID
// but the server is verified with a normal CA bundle (not SPIFFE) — the
// client-only case enabled by splitting serverAuth and clientAuth.
func TestSPIFFE_ClientOnly(t *testing.T) {
	g := NewWithT(t)
	ca := newTestCA(t)
	startFakeWorkloadAPI(t, ca)

	// Normal (non-SPIFFE) server cert with an IP SAN for hostname verification.
	serverCertPEM, serverKeyPEM := ca.issue(t, "server", []net.IP{net.IPv4(127, 0, 0, 1)}, nil)
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	g.Expect(err).ToNot(HaveOccurred())
	caPool := x509.NewCertPool()
	g.Expect(caPool.AppendCertsFromPEM(ca.certPEM)).To(BeTrue())

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	srv.StartTLS()
	defer srv.Close()
	host := mustHostname(t, srv.URL)

	rt, closeFn, err := NewTLSTransport(context.Background(), http.DefaultTransport, []config.AuthHost{{
		Host: host,
		TLS: &config.TLS{
			ServerAuth: &config.TLSServerAuth{FromBytes: string(ca.certPEM)},
			ClientAuth: &config.TLSClientAuth{Provider: config.TLSClientProviderX509SVID},
		},
	}})
	g.Expect(err).ToNot(HaveOccurred())
	defer closeFn()

	resp, err := rt.RoundTrip(mustGet(t, srv.URL))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(resp.StatusCode).To(Equal(http.StatusNoContent))
}

// TestSPIFFE_JWTSVIDCredential resolves a credential with provider jwt-svid
// against the fake Workload API and checks the returned JWT carries the host as
// audience and the SPIFFE ID as subject.
func TestSPIFFE_JWTSVIDCredential(t *testing.T) {
	g := NewWithT(t)
	ca := newTestCA(t)
	startFakeWorkloadAPI(t, ca)

	h := config.AuthHost{Host: "registry.example.com", Credential: &config.AuthCredential{
		Provider: config.JWTProviderJWTSVID,
	}}
	cred, err := resolveCredential(context.Background(), h)
	g.Expect(err).ToNot(HaveOccurred())

	tok, err := jwt.ParseSigned(cred, []jose.SignatureAlgorithm{jose.ES256})
	g.Expect(err).ToNot(HaveOccurred())
	var claims jwt.Claims
	g.Expect(tok.UnsafeClaimsWithoutVerification(&claims)).To(Succeed())
	g.Expect(claims.Subject).To(Equal(testClientID))
	g.Expect(claims.Audience).To(ContainElement("registry.example.com"))
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	g := NewWithT(t)
	u, err := url.Parse(raw)
	g.Expect(err).ToNot(HaveOccurred())
	return u
}

func pemToDER(t *testing.T, pemBytes []byte) []byte {
	t.Helper()
	g := NewWithT(t)
	block, _ := pem.Decode(pemBytes)
	g.Expect(block).ToNot(BeNil())
	return block.Bytes
}
