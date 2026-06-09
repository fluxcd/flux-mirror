// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"

	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta1"
)

// NeedsTLS reports whether any host configures transport-layer TLS.
func NeedsTLS(hosts []apiv1.RegistryHost) bool {
	for _, h := range hosts {
		if h.TLS != nil {
			return true
		}
	}
	return false
}

// tlsDispatchTransport routes each request to the per-host RoundTripper carrying
// that host's TLS settings, falling back to inner for any host without TLS.
type tlsDispatchTransport struct {
	byHost map[string]http.RoundTripper
	inner  http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t tlsDispatchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Match on the full host (including any port) so a host configured as
	// "registry.example.com:8443" keeps its TLS config. This mirrors the cijwt
	// transport, which also keys on req.URL.Host.
	if rt, ok := t.byHost[req.URL.Host]; ok {
		return rt.RoundTrip(req)
	}
	return t.inner.RoundTrip(req)
}

// NewTLSTransport wraps inner with a transport that applies each host's
// configured TLS settings (custom CA, client certificate, or SPIFFE X.509-SVID
// mTLS) to requests for that host. Hosts without TLS fall through to inner
// unchanged. Returns inner and a no-op closer when no host configures TLS.
//
// The returned closer must be called when the transport is no longer needed; it
// closes the SPIFFE Workload API source, if one was opened.
func NewTLSTransport(ctx context.Context, inner http.RoundTripper, hosts []apiv1.RegistryHost) (http.RoundTripper, func() error, error) {
	if !NeedsTLS(hosts) {
		return inner, func() error { return nil }, nil
	}

	// All SPIFFE-using hosts share a single Workload API source: it represents
	// this workload's own identity and trust bundle, not a per-host one, so one
	// socket and one rotation goroutine suffice. It is opened lazily, the first
	// time a host actually needs SPIFFE.
	byHost := make(map[string]http.RoundTripper)
	var spiffeSrc *workloadapi.X509Source
	closeSource := func() error {
		if spiffeSrc != nil {
			return spiffeSrc.Close()
		}
		return nil
	}

	for _, h := range hosts {
		if h.TLS == nil {
			continue
		}
		if spiffeSrc == nil && tlsNeedsSPIFFE(h.TLS) {
			s, err := workloadapi.NewX509Source(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("host %q: tls: spiffe: create X.509 source: %w", h.Host, err)
			}
			spiffeSrc = s
		}
		tlsCfg, err := buildTLSConfig(h.TLS, spiffeSrc)
		if err != nil {
			_ = closeSource()
			return nil, nil, fmt.Errorf("host %q: tls: %w", h.Host, err)
		}
		rt := http.DefaultTransport.(*http.Transport).Clone()
		rt.TLSClientConfig = tlsCfg
		byHost[h.Host] = rt
	}

	return tlsDispatchTransport{byHost: byHost, inner: inner}, closeSource, nil
}

// tlsNeedsSPIFFE reports whether either half of the TLS config uses SPIFFE and so
// requires a Workload API source.
func tlsNeedsSPIFFE(t *apiv1.TLS) bool {
	clientSPIFFE := t.ClientAuth != nil && t.ClientAuth.Provider == apiv1.TLSClientProviderX509SVID
	serverSPIFFE := t.ServerAuth != nil && t.ServerAuth.SPIFFE != nil
	return clientSPIFFE || serverSPIFFE
}

// buildTLSConfig builds a *tls.Config for a host from its independent server and
// client settings, each of which may be SPIFFE or static. When either side uses
// SPIFFE it draws on src, the shared Workload API source owned by the caller
// (which must be non-nil whenever the config uses SPIFFE).
func buildTLSConfig(t *apiv1.TLS, src *workloadapi.X509Source) (*tls.Config, error) {
	clientSPIFFE := t.ClientAuth != nil && t.ClientAuth.Provider == apiv1.TLSClientProviderX509SVID
	serverSPIFFE := t.ServerAuth != nil && t.ServerAuth.SPIFFE != nil

	cfg := &tls.Config{MinVersion: tls.VersionTLS12}

	// Server verification: SPIFFE, a custom CA bundle, or (unset) system roots.
	switch {
	case serverSPIFFE:
		authorizer, err := spiffeAuthorizer(src, t.ServerAuth.SPIFFE)
		if err != nil {
			return nil, fmt.Errorf("serverAuth: spiffe: %w", err)
		}
		tlsconfig.HookTLSClientConfig(cfg, src, authorizer)
	case t.ServerAuth != nil:
		pemBytes, err := readPEMSource(t.ServerAuth.FromPath, t.ServerAuth.FromEnv, t.ServerAuth.FromBytes)
		if err != nil {
			return nil, fmt.Errorf("serverAuth: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("serverAuth: no valid certificates in CA bundle")
		}
		cfg.RootCAs = pool
	}

	// Client certificate: SPIFFE X.509-SVID or a static cert/key pair.
	switch {
	case clientSPIFFE:
		cfg.GetClientCertificate = tlsconfig.GetClientCertificate(src)
	case t.ClientAuth != nil:
		certPEM, err := readTLSData(t.ClientAuth.Certificate)
		if err != nil {
			return nil, fmt.Errorf("clientAuth: certificate: %w", err)
		}
		keyPEM, err := readPEMSource(t.ClientAuth.Key.FromPath, t.ClientAuth.Key.FromEnv, "")
		if err != nil {
			return nil, fmt.Errorf("clientAuth: key: %w", err)
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("clientAuth: load key pair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// spiffeAuthorizer turns the configured authorization rule into a tlsconfig
// Authorizer. trustDomain "self" resolves to the client's own trust domain.
func spiffeAuthorizer(src *workloadapi.X509Source, s *apiv1.SPIFFETLS) (tlsconfig.Authorizer, error) {
	switch {
	case s.AuthorizeAny:
		return tlsconfig.AuthorizeAny(), nil
	case s.ServerID != "":
		id, err := spiffeid.FromString(s.ServerID)
		if err != nil {
			return nil, fmt.Errorf("serverID: %w", err)
		}
		return tlsconfig.AuthorizeID(id), nil
	case s.TrustDomain != "":
		var td spiffeid.TrustDomain
		if s.TrustDomain == apiv1.TrustDomainSelf {
			svid, err := src.GetX509SVID()
			if err != nil {
				return nil, fmt.Errorf("read own SVID for trustDomain self: %w", err)
			}
			td = svid.ID.TrustDomain()
		} else {
			var err error
			if td, err = spiffeid.TrustDomainFromString(s.TrustDomain); err != nil {
				return nil, fmt.Errorf("trustDomain: %w", err)
			}
		}
		return tlsconfig.AuthorizeMemberOf(td), nil
	default:
		return nil, fmt.Errorf("no authorizer configured")
	}
}

// readTLSData reads a single PEM value from the one configured source.
func readTLSData(d *apiv1.TLSData) ([]byte, error) {
	return readPEMSource(d.FromPath, d.FromEnv, d.FromBytes)
}

// readPEMSource reads PEM bytes from exactly one of a file path, an environment
// variable, or an inline value.
func readPEMSource(fromPath, fromEnv, fromBytes string) ([]byte, error) {
	switch {
	case fromPath != "":
		b, err := os.ReadFile(fromPath)
		if err != nil {
			return nil, fmt.Errorf("read fromPath: %w", err)
		}
		return b, nil
	case fromEnv != "":
		v, err := requireEnv(fromEnv)
		if err != nil {
			return nil, err
		}
		return []byte(v), nil
	case fromBytes != "":
		return []byte(fromBytes), nil
	default:
		return nil, fmt.Errorf("no source set")
	}
}
