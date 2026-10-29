// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/fluxcd/pkg/runtime/secrets"
	fluxspiffe "github.com/fluxcd/pkg/spiffe"
	"github.com/spiffe/go-spiffe/v2/workloadapi"

	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta2"
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
// configured TLS settings to requests for that host: a PEM directory (custom CA
// and/or client certificate) or SPIFFE mutual TLS. Hosts without TLS fall
// through to inner unchanged. Returns inner and a no-op closer when no host
// configures TLS.
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
		inline := h.TLS.CACert != "" || h.TLS.TLSCert != "" || h.TLS.TLSKey != ""
		if h.TLS.SPIFFE == nil && h.TLS.Path == "" && !inline {
			_ = closeSource()
			return nil, nil, fmt.Errorf("host %q: tls: one of path, inline certificates, or spiffe is required", h.Host)
		}
		var tlsCfg *tls.Config
		var err error
		switch {
		case h.TLS.SPIFFE != nil:
			if spiffeSrc == nil {
				spiffeSrc, err = workloadapi.NewX509Source(ctx)
				if err != nil {
					return nil, nil, fmt.Errorf("host %q: tls: spiffe: create X.509 source: %w", h.Host, err)
				}
			}
			tlsCfg, err = fluxspiffe.BuildTLSConfig(spiffeSrc, trimAll(h.TLS.SPIFFE.ServerIDs)...)
		case inline:
			tlsCfg, err = tlsConfigFromInline(ctx, *h.TLS)
		default:
			tlsCfg, err = tlsConfigFromPath(ctx, h.TLS.Path)
		}
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

// tlsConfigFromPath builds a *tls.Config from a directory laid out like a
// mounted Flux certSecretRef: ca.crt supplies the server CA bundle, and/or
// tls.crt with tls.key presents a client certificate. The Flux secrets package
// performs the same key lookup and validation as the Flux API, so error messages
// and accepted layouts stay consistent with the controllers.
func tlsConfigFromPath(ctx context.Context, dir string) (*tls.Config, error) {
	data := make(map[string][]byte, 6)
	// Read both the standard certificate keys and the legacy Flux keys; the
	// secrets package gives the standard keys precedence and warns about legacy
	// ones, matching how Flux loads a certSecretRef.
	for _, key := range []string{
		secrets.KeyCACert, secrets.KeyTLSCert, secrets.KeyTLSPrivateKey,
		secrets.LegacyKeyCACert, secrets.LegacyKeyTLSCert, secrets.LegacyKeyTLSPrivateKey,
	} {
		b, err := os.ReadFile(filepath.Join(dir, key))
		switch {
		case err == nil:
			data[key] = b
		case errors.Is(err, fs.ErrNotExist):
			// The file is optional; the secrets package validates that the
			// combination that is present is usable.
		default:
			return nil, fmt.Errorf("read %s: %w", filepath.Join(dir, key), err)
		}
	}

	return tlsConfigFromData(ctx, dir, data)
}

// tlsConfigFromInline builds a *tls.Config from the inline PEM fields, using the
// same key names as a certSecretRef Secret.
func tlsConfigFromInline(ctx context.Context, t apiv1.TLS) (*tls.Config, error) {
	data := make(map[string][]byte, 3)
	for key, value := range map[string]string{
		secrets.KeyCACert:        t.CACert,
		secrets.KeyTLSCert:       t.TLSCert,
		secrets.KeyTLSPrivateKey: t.TLSKey,
	} {
		if value != "" {
			data[key] = []byte(value)
		}
	}
	return tlsConfigFromData(ctx, "tls (inline)", data)
}

// tlsConfigFromData reuses the Flux validation and TLS assembly by presenting
// the PEM data as an in-memory Secret. name identifies the source in validation
// errors.
func tlsConfigFromData(ctx context.Context, name string, data map[string][]byte) (*tls.Config, error) {
	secret := &corev1.Secret{Data: data}
	secret.Name = name
	cfg, err := secrets.TLSConfigFromSecret(ctx, secret)
	if err != nil {
		return nil, err
	}
	cfg.MinVersion = tls.VersionTLS12
	return cfg, nil
}

// trimAll trims surrounding whitespace from each entry, matching how Flux
// parses the RFC-0014 annotation lists, and drops entries that become empty.
func trimAll(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
