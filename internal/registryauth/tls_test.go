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

	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta2"
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

// certDir writes the given files into a fresh directory and returns its path.
func certDir(t *testing.T, files map[string][]byte) string {
	t.Helper()
	g := NewWithT(t)
	dir := t.TempDir()
	for name, data := range files {
		g.Expect(os.WriteFile(filepath.Join(dir, name), data, 0o600)).To(Succeed())
	}
	return dir
}

// TestNewTLSTransport_MTLSEndToEnd drives a real mTLS handshake through the
// dispatching transport: a CA-backed server that requires a client certificate,
// reached with a tls.path directory containing ca.crt, tls.crt, and tls.key. It
// also checks that a CA-only directory fails the client-cert requirement, and
// that the wrong CA fails server verification.
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

	t.Run("CA and client certificate succeed", func(t *testing.T) {
		g := NewWithT(t)
		dir := certDir(t, map[string][]byte{
			"ca.crt":  ca.certPEM,
			"tls.crt": clientCertPEM,
			"tls.key": clientKeyPEM,
		})
		rt, closeFn, err := NewTLSTransport(context.Background(), http.DefaultTransport, []apiv1.RegistryHost{{
			Host: host,
			TLS:  &apiv1.TLS{Path: dir},
		}})
		g.Expect(err).ToNot(HaveOccurred())
		defer closeFn()

		resp, err := rt.RoundTrip(mustGet(t, srv.URL))
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(resp.StatusCode).To(Equal(http.StatusNoContent))
	})

	t.Run("inline CA and client certificate succeed", func(t *testing.T) {
		g := NewWithT(t)
		rt, closeFn, err := NewTLSTransport(context.Background(), http.DefaultTransport, []apiv1.RegistryHost{{
			Host: host,
			TLS: &apiv1.TLS{
				CACert:  string(ca.certPEM),
				TLSCert: string(clientCertPEM),
				TLSKey:  string(clientKeyPEM),
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
		dir := certDir(t, map[string][]byte{"ca.crt": ca.certPEM})
		rt, closeFn, err := NewTLSTransport(context.Background(), http.DefaultTransport, []apiv1.RegistryHost{{
			Host: host,
			TLS:  &apiv1.TLS{Path: dir},
		}})
		g.Expect(err).ToNot(HaveOccurred())
		defer closeFn()

		_, err = rt.RoundTrip(mustGet(t, srv.URL))
		g.Expect(err).To(HaveOccurred()) // server requires a client cert
	})

	t.Run("wrong CA fails server verification", func(t *testing.T) {
		g := NewWithT(t)
		otherCA := newTestCA(t)
		dir := certDir(t, map[string][]byte{
			"ca.crt":  otherCA.certPEM,
			"tls.crt": clientCertPEM,
			"tls.key": clientKeyPEM,
		})
		rt, closeFn, err := NewTLSTransport(context.Background(), http.DefaultTransport, []apiv1.RegistryHost{{
			Host: host,
			TLS:  &apiv1.TLS{Path: dir},
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
	return mustURL(t, rawURL).Host
}

func mustGet(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	g := NewWithT(t)
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	g.Expect(err).ToNot(HaveOccurred())
	return req
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
	g.Expect(NeedsTLS([]apiv1.RegistryHost{{Host: "a", Credential: &apiv1.RegistryCredential{Type: apiv1.CredentialTypeJWT, Value: "X"}}})).To(BeFalse())
	g.Expect(NeedsTLS([]apiv1.RegistryHost{{Host: "a", TLS: &apiv1.TLS{Path: "/certs"}}})).To(BeTrue())
}

func TestNewTLSTransport_NoTLSReturnsInner(t *testing.T) {
	g := NewWithT(t)
	inner := http.DefaultTransport
	rt, closeFn, err := NewTLSTransport(context.Background(), inner, []apiv1.RegistryHost{
		{Host: "a", Credential: &apiv1.RegistryCredential{Type: apiv1.CredentialTypeJWT, Value: "X"}},
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(rt).To(BeIdenticalTo(inner))
	g.Expect(closeFn()).To(Succeed())
}

func TestNewTLSTransport_StaticDispatch(t *testing.T) {
	g := NewWithT(t)
	caPEM, _ := makeCertPEM(t)
	certPEM, keyPEM := makeCertPEM(t)

	caDir := certDir(t, map[string][]byte{"ca.crt": caPEM})
	mtlsDir := certDir(t, map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM})

	hosts := []apiv1.RegistryHost{
		{Host: "ca.example", TLS: &apiv1.TLS{Path: caDir}},
		{Host: "mtls.example", TLS: &apiv1.TLS{Path: mtlsDir}},
		{Host: "inline.example", TLS: &apiv1.TLS{CACert: string(caPEM)}},
		{Host: "plain.example", Credential: &apiv1.RegistryCredential{Type: apiv1.CredentialTypeJWT, Value: "X"}},
	}
	rt, closeFn, err := NewTLSTransport(context.Background(), http.DefaultTransport, hosts)
	g.Expect(err).ToNot(HaveOccurred())
	defer closeFn()

	disp, ok := rt.(tlsDispatchTransport)
	g.Expect(ok).To(BeTrue())
	g.Expect(disp.byHost).To(HaveKey("ca.example"))
	g.Expect(disp.byHost).To(HaveKey("mtls.example"))
	g.Expect(disp.byHost).To(HaveKey("inline.example"))
	g.Expect(disp.byHost).ToNot(HaveKey("plain.example")) // no TLS → falls through to inner

	caRT := disp.byHost["ca.example"].(*http.Transport)
	g.Expect(caRT.TLSClientConfig.RootCAs).ToNot(BeNil())
	mtlsRT := disp.byHost["mtls.example"].(*http.Transport)
	g.Expect(mtlsRT.TLSClientConfig.Certificates).To(HaveLen(1))
	inlineRT := disp.byHost["inline.example"].(*http.Transport)
	g.Expect(inlineRT.TLSClientConfig.RootCAs).ToNot(BeNil())
}

// TestNewTLSTransport_LegacyKeys proves the directory loader accepts the legacy
// Flux certificate key names as fallbacks, like a certSecretRef Secret.
func TestNewTLSTransport_LegacyKeys(t *testing.T) {
	g := NewWithT(t)
	caPEM, _ := makeCertPEM(t)
	dir := certDir(t, map[string][]byte{"caFile": caPEM})

	rt, closeFn, err := NewTLSTransport(context.Background(), http.DefaultTransport, []apiv1.RegistryHost{
		{Host: "ca.example", TLS: &apiv1.TLS{Path: dir}},
	})
	g.Expect(err).ToNot(HaveOccurred())
	defer closeFn()

	disp, ok := rt.(tlsDispatchTransport)
	g.Expect(ok).To(BeTrue())
	caRT := disp.byHost["ca.example"].(*http.Transport)
	g.Expect(caRT.TLSClientConfig.RootCAs).ToNot(BeNil())
}

// TestNewTLSTransport_MissingTLSConfig guards unvalidated callers: a TLS block
// without path, inline certificates, or spiffe must not fall back to reading the working
// directory.
func TestNewTLSTransport_MissingTLSConfig(t *testing.T) {
	g := NewWithT(t)
	_, _, err := NewTLSTransport(context.Background(), http.DefaultTransport, []apiv1.RegistryHost{
		{Host: "empty.example", TLS: &apiv1.TLS{}},
	})
	g.Expect(err).To(MatchError(ContainSubstring("one of path, inline certificates, or spiffe is required")))
}

func TestNewTLSTransport_BadCABundle(t *testing.T) {
	g := NewWithT(t)
	dir := certDir(t, map[string][]byte{"ca.crt": []byte("not a pem")})
	_, _, err := NewTLSTransport(context.Background(), http.DefaultTransport, []apiv1.RegistryHost{
		{Host: "ca.example", TLS: &apiv1.TLS{Path: dir}},
	})
	g.Expect(err).To(MatchError(ContainSubstring("failed to parse CA certificate")))
}

func TestNewTLSTransport_BadInlineData(t *testing.T) {
	g := NewWithT(t)
	_, _, err := NewTLSTransport(context.Background(), http.DefaultTransport, []apiv1.RegistryHost{
		{Host: "ca.example", TLS: &apiv1.TLS{CACert: "not a pem"}},
	})
	g.Expect(err).To(MatchError(ContainSubstring("failed to parse CA certificate")))
}

func TestNewTLSTransport_PathValidation(t *testing.T) {
	certPEM, keyPEM := makeCertPEM(t)

	for _, tc := range []struct {
		name    string
		files   map[string][]byte
		errText string
	}{
		{
			name:    "empty directory",
			files:   map[string][]byte{},
			errText: "must contain either 'ca.crt' or both 'tls.crt' and 'tls.key'",
		},
		{
			name:    "cert without key",
			files:   map[string][]byte{"tls.crt": certPEM},
			errText: "contains 'tls.crt' but missing 'tls.key'",
		},
		{
			name:    "key without cert",
			files:   map[string][]byte{"tls.key": keyPEM},
			errText: "contains 'tls.key' but missing 'tls.crt'",
		},
		{
			name:    "invalid key pair",
			files:   map[string][]byte{"tls.crt": certPEM, "tls.key": []byte("not a key")},
			errText: "failed to parse TLS certificate and key",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			dir := certDir(t, tc.files)
			_, _, err := NewTLSTransport(context.Background(), http.DefaultTransport, []apiv1.RegistryHost{
				{Host: "ca.example", TLS: &apiv1.TLS{Path: dir}},
			})
			g.Expect(err).To(MatchError(ContainSubstring(tc.errText)))
		})
	}
}
