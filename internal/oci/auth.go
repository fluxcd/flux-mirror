// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"net/http"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Client wraps go-containerregistry with a fixed keychain (the ambient Docker
// config by default) and a single shared HTTP transport. Reuse one Client
// across every Copy/Compare/List call to avoid re-resolving auth or
// reallocating transports per tag.
type Client struct {
	keychain  authn.Keychain
	insecure  bool
	transport http.RoundTripper

	// Pre-built static option slices. The only thing varying per call is
	// the context, which is prepended at call time. The auth keychain and
	// insecure flag never change after construction.
	staticCraneOpts  []crane.Option
	staticRemoteOpts []remote.Option
	staticNameOpts   []name.Option
}

// ClientOption customizes Client construction.
type ClientOption func(*Client)

// WithKeychain replaces the default ambient-Docker-config keychain.
func WithKeychain(k authn.Keychain) ClientOption {
	return func(c *Client) { c.keychain = k }
}

// Insecure allows references over plaintext HTTP and skips TLS verification.
// Intended for local-registry tests; do not use in production.
func Insecure() ClientOption {
	return func(c *Client) { c.insecure = true }
}

// WithTransport sets the HTTP RoundTripper used for every registry call.
// Wrap http.DefaultTransport with ChunkingTransport / JWTBearerTransport
// (or any other RoundTripper) and pass the outermost wrapper here.
func WithTransport(t http.RoundTripper) ClientOption {
	return func(c *Client) { c.transport = t }
}

// NewClient returns a Client. By default it uses authn.DefaultKeychain, which
// reads ~/.docker/config.json (or $DOCKER_CONFIG) and any configured
// credential helpers — exactly what `docker login` and `helm registry login`
// populate.
func NewClient(opts ...ClientOption) *Client {
	c := &Client{keychain: authn.DefaultKeychain}
	for _, opt := range opts {
		opt(c)
	}
	c.staticCraneOpts = []crane.Option{crane.WithAuthFromKeychain(c.keychain)}
	c.staticRemoteOpts = []remote.Option{remote.WithAuthFromKeychain(c.keychain)}
	if c.transport != nil {
		c.staticCraneOpts = append(c.staticCraneOpts, crane.WithTransport(c.transport))
		c.staticRemoteOpts = append(c.staticRemoteOpts, remote.WithTransport(c.transport))
	}
	if c.insecure {
		c.staticCraneOpts = append(c.staticCraneOpts, crane.Insecure)
		c.staticNameOpts = []name.Option{name.Insecure}
	}
	return c
}

// craneOptions returns the per-call option slice. Only the context varies —
// auth and transport are reused from the cached static slice.
func (c *Client) craneOptions(ctx context.Context) []crane.Option {
	opts := make([]crane.Option, 0, len(c.staticCraneOpts)+1)
	opts = append(opts, crane.WithContext(ctx))
	return append(opts, c.staticCraneOpts...)
}
