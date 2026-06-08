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
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	"github.com/fluxcd/flux-mirror/internal/config"
)

// testCA is a self-signed CA used to issue server and client certificates for
// the TLS/mTLS tests.
type testCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	g := NewWithT(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	g.Expect(err).ToNot(HaveOccurred())
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	g.Expect(err).ToNot(HaveOccurred())
	cert, err := x509.ParseCertificate(der)
	g.Expect(err).ToNot(HaveOccurred())
	return &testCA{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

// issue signs a leaf certificate with the given DNS names, IPs, and SPIFFE URI
// SANs, returning the cert and key PEM.
func (ca *testCA) issue(t *testing.T, cn string, ips []net.IP, uris []*url.URL) (certPEM, keyPEM []byte) {
	t.Helper()
	g := NewWithT(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	g.Expect(err).ToNot(HaveOccurred())
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:  ips,
		URIs:         uris,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	g.Expect(err).ToNot(HaveOccurred())
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	g.Expect(err).ToNot(HaveOccurred())
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

// TestNewTLSTransport_MTLSEndToEnd drives a real mTLS handshake through the
// dispatching transport: a CA-backed server that requires a client certificate,
// reached with serverAuth (custom CA) + clientAuth (client cert) configured for
// its host. It also checks that the wrong CA and a missing client cert fail, and
// that an unconfigured host falls through to the inner transport.
func TestNewTLSTransport_MTLSEndToEnd(t *testing.T) {
	g := NewWithT(t)
	ca := newTestCA(t)
	serverCertPEM, serverKeyPEM := ca.issue(t, "server", []net.IP{net.IPv4(127, 0, 0, 1)}, nil)
	clientCertPEM, clientKeyPEM := ca.issue(t, "client", nil, nil)

	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	g.Expect(err).ToNot(HaveOccurred())
	clientCAPool := x509.NewCertPool()
	g.Expect(clientCAPool.AppendCertsFromPEM(ca.certPEM)).To(BeTrue())

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    clientCAPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	srv.StartTLS()
	defer srv.Close()

	host := mustHost(t, srv.URL) // e.g. "127.0.0.1:NNNNN" (with port)

	t.Run("serverAuth + clientAuth succeeds", func(t *testing.T) {
		g := NewWithT(t)
		rt, closeFn, err := NewTLSTransport(context.Background(), http.DefaultTransport, []config.AuthHost{{
			Host: host,
			TLS: &config.TLS{
				ServerAuth: &config.TLSServerAuth{FromBytes: string(ca.certPEM)},
				ClientAuth: &config.TLSClientAuth{
					Certificate: &config.TLSData{FromBytes: string(clientCertPEM)},
					Key:         &config.TLSKey{FromPath: writeTemp(t, clientKeyPEM)},
				},
			},
		}})
		g.Expect(err).ToNot(HaveOccurred())
		defer closeFn()

		resp, err := rt.RoundTrip(mustGet(t, srv.URL))
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(resp.StatusCode).To(Equal(http.StatusNoContent))
	})

	t.Run("missing client cert is rejected by the server", func(t *testing.T) {
		g := NewWithT(t)
		rt, closeFn, err := NewTLSTransport(context.Background(), http.DefaultTransport, []config.AuthHost{{
			Host: host,
			TLS:  &config.TLS{ServerAuth: &config.TLSServerAuth{FromBytes: string(ca.certPEM)}},
		}})
		g.Expect(err).ToNot(HaveOccurred())
		defer closeFn()

		_, err = rt.RoundTrip(mustGet(t, srv.URL))
		g.Expect(err).To(HaveOccurred()) // server requires a client cert
	})

	t.Run("wrong CA fails server verification", func(t *testing.T) {
		g := NewWithT(t)
		otherCA := newTestCA(t)
		rt, closeFn, err := NewTLSTransport(context.Background(), http.DefaultTransport, []config.AuthHost{{
			Host: host,
			TLS: &config.TLS{
				ServerAuth: &config.TLSServerAuth{FromBytes: string(otherCA.certPEM)},
				ClientAuth: &config.TLSClientAuth{
					Certificate: &config.TLSData{FromBytes: string(clientCertPEM)},
					Key:         &config.TLSKey{FromPath: writeTemp(t, clientKeyPEM)},
				},
			},
		}})
		g.Expect(err).ToNot(HaveOccurred())
		defer closeFn()

		_, err = rt.RoundTrip(mustGet(t, srv.URL))
		g.Expect(err).To(MatchError(ContainSubstring("certificate")))
	})
}

// mustHost returns the URL's host including any port — the form a config `host`
// takes and that the TLS dispatch matches on.
func mustHost(t *testing.T, rawURL string) string {
	t.Helper()
	g := NewWithT(t)
	u, err := url.Parse(rawURL)
	g.Expect(err).ToNot(HaveOccurred())
	return u.Host
}

func mustGet(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	g := NewWithT(t)
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	g.Expect(err).ToNot(HaveOccurred())
	return req
}

// writeTemp writes data to a temp file and returns its path.
func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	g := NewWithT(t)
	p := filepath.Join(t.TempDir(), "data.pem")
	g.Expect(os.WriteFile(p, data, 0o600)).To(Succeed())
	return p
}

// makeCertPEM returns a self-signed certificate and its private key, PEM-encoded.
func makeCertPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	g := NewWithT(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	g.Expect(err).ToNot(HaveOccurred())
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	g.Expect(err).ToNot(HaveOccurred())
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	g.Expect(err).ToNot(HaveOccurred())
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func TestNeedsTLS(t *testing.T) {
	g := NewWithT(t)
	g.Expect(NeedsTLS(nil)).To(BeFalse())
	g.Expect(NeedsTLS([]config.AuthHost{{Host: "a", Credential: &config.AuthCredential{FromEnv: "X"}}})).To(BeFalse())
	g.Expect(NeedsTLS([]config.AuthHost{{Host: "a", TLS: &config.TLS{ServerAuth: &config.TLSServerAuth{FromBytes: "x"}}}})).To(BeTrue())
}

func TestNewTLSTransport_NoTLSReturnsInner(t *testing.T) {
	g := NewWithT(t)
	inner := http.DefaultTransport
	rt, closeFn, err := NewTLSTransport(context.Background(), inner, []config.AuthHost{
		{Host: "a", Credential: &config.AuthCredential{FromEnv: "X"}},
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(rt).To(BeIdenticalTo(inner))
	g.Expect(closeFn()).To(Succeed())
}

func TestNewTLSTransport_StaticDispatch(t *testing.T) {
	g := NewWithT(t)
	caPEM, _ := makeCertPEM(t)
	certPEM, keyPEM := makeCertPEM(t)

	hosts := []config.AuthHost{
		{Host: "ca.example", TLS: &config.TLS{ServerAuth: &config.TLSServerAuth{FromBytes: string(caPEM)}}},
		{Host: "mtls.example", TLS: &config.TLS{ClientAuth: &config.TLSClientAuth{
			Certificate: &config.TLSData{FromBytes: string(certPEM)},
			Key:         &config.TLSKey{FromPath: writeTemp(t, keyPEM)},
		}}},
		{Host: "plain.example", Credential: &config.AuthCredential{FromEnv: "X"}},
	}
	rt, closeFn, err := NewTLSTransport(context.Background(), http.DefaultTransport, hosts)
	g.Expect(err).ToNot(HaveOccurred())
	defer closeFn()

	disp, ok := rt.(tlsDispatchTransport)
	g.Expect(ok).To(BeTrue())
	g.Expect(disp.byHost).To(HaveKey("ca.example"))
	g.Expect(disp.byHost).To(HaveKey("mtls.example"))
	g.Expect(disp.byHost).ToNot(HaveKey("plain.example")) // no TLS → falls through to inner

	caRT := disp.byHost["ca.example"].(*http.Transport)
	g.Expect(caRT.TLSClientConfig.RootCAs).ToNot(BeNil())
	mtlsRT := disp.byHost["mtls.example"].(*http.Transport)
	g.Expect(mtlsRT.TLSClientConfig.Certificates).To(HaveLen(1))
}

func TestNewTLSTransport_BadCABundle(t *testing.T) {
	g := NewWithT(t)
	_, _, err := NewTLSTransport(context.Background(), http.DefaultTransport, []config.AuthHost{
		{Host: "ca.example", TLS: &config.TLS{ServerAuth: &config.TLSServerAuth{FromBytes: "not a pem"}}},
	})
	g.Expect(err).To(MatchError(ContainSubstring("no valid certificates in CA bundle")))
}

func TestReadTLSData(t *testing.T) {
	g := NewWithT(t)

	// fromBytes
	b, err := readTLSData(&config.TLSData{FromBytes: "inline"})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(string(b)).To(Equal("inline"))

	// fromEnv
	t.Setenv("TLS_TEST_VAR", "envval")
	b, err = readTLSData(&config.TLSData{FromEnv: "TLS_TEST_VAR"})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(string(b)).To(Equal("envval"))

	// fromEnv unset
	t.Setenv("TLS_TEST_VAR", "")
	_, err = readTLSData(&config.TLSData{FromEnv: "TLS_TEST_VAR"})
	g.Expect(err).To(MatchError(ContainSubstring("is not set or empty")))

	// fromPath
	p := filepath.Join(t.TempDir(), "f")
	g.Expect(os.WriteFile(p, []byte("filedata"), 0o600)).To(Succeed())
	b, err = readTLSData(&config.TLSData{FromPath: p})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(string(b)).To(Equal("filedata"))
}

func TestSpiffeAuthorizer(t *testing.T) {
	g := NewWithT(t)

	// AuthorizeAny and ServerID and a concrete trustDomain do not need a source.
	a, err := spiffeAuthorizer(nil, &config.SPIFFETLS{AuthorizeAny: true})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(a).ToNot(BeNil())

	a, err = spiffeAuthorizer(nil, &config.SPIFFETLS{ServerID: "spiffe://example.org/registry"})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(a).ToNot(BeNil())

	a, err = spiffeAuthorizer(nil, &config.SPIFFETLS{TrustDomain: "example.org"})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(a).ToNot(BeNil())

	_, err = spiffeAuthorizer(nil, &config.SPIFFETLS{ServerID: "not-a-spiffe-id"})
	g.Expect(err).To(MatchError(ContainSubstring("serverID")))
}
