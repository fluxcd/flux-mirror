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

// Client wraps go-containerregistry with source and destination auth/transport
// settings. Reuse one Client across every Copy/Compare/List call to avoid
// re-resolving auth or reallocating transports per tag.
type Client struct {
	keychain  authn.Keychain
	insecure  bool
	transport http.RoundTripper

	// Per-direction overrides. When unset the base keychain/transport above is
	// used for both sides.
	sourceKeychain       authn.Keychain
	destinationKeychain  authn.Keychain
	sourceTransport      http.RoundTripper
	destinationTransport http.RoundTripper

	// Pre-built static option slices. Only the context varies per call.
	sourceCraneOpts       []crane.Option
	destinationCraneOpts  []crane.Option
	sourceRemoteOpts      []remote.Option
	destinationRemoteOpts []remote.Option
	staticNameOpts        []name.Option
}

// ClientOption customizes Client construction.
type ClientOption func(*Client)

// WithKeychain replaces the default keychain for both directions.
func WithKeychain(k authn.Keychain) ClientOption {
	return func(c *Client) { c.keychain = k }
}

// WithSourceKeychain replaces the keychain used for source pulls.
func WithSourceKeychain(k authn.Keychain) ClientOption {
	return func(c *Client) { c.sourceKeychain = k }
}

// WithDestinationKeychain replaces the keychain used for destination pushes.
func WithDestinationKeychain(k authn.Keychain) ClientOption {
	return func(c *Client) { c.destinationKeychain = k }
}

// Insecure allows references over plaintext HTTP and skips TLS verification.
// Intended for local-registry tests; do not use in production.
func Insecure() ClientOption {
	return func(c *Client) { c.insecure = true }
}

// WithTransport sets the HTTP RoundTripper used for both directions.
func WithTransport(t http.RoundTripper) ClientOption {
	return func(c *Client) { c.transport = t }
}

// WithSourceTransport sets the HTTP RoundTripper used for source pulls.
func WithSourceTransport(t http.RoundTripper) ClientOption {
	return func(c *Client) { c.sourceTransport = t }
}

// WithDestinationTransport sets the HTTP RoundTripper used for destination pushes.
func WithDestinationTransport(t http.RoundTripper) ClientOption {
	return func(c *Client) { c.destinationTransport = t }
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
	if c.keychain == nil {
		c.keychain = authn.DefaultKeychain
	}
	srcKC := c.sourceKeychain
	if srcKC == nil {
		srcKC = c.keychain
	}
	dstKC := c.destinationKeychain
	if dstKC == nil {
		dstKC = c.keychain
	}
	srcT := c.sourceTransport
	if srcT == nil {
		srcT = c.transport
	}
	dstT := c.destinationTransport
	if dstT == nil {
		dstT = c.transport
	}
	c.sourceCraneOpts = buildCraneOpts(srcKC, srcT, c.insecure)
	c.destinationCraneOpts = buildCraneOpts(dstKC, dstT, c.insecure)
	c.sourceRemoteOpts = buildRemoteOpts(srcKC, srcT)
	c.destinationRemoteOpts = buildRemoteOpts(dstKC, dstT)
	if c.insecure {
		c.staticNameOpts = []name.Option{name.Insecure}
	}
	return c
}

func buildCraneOpts(kc authn.Keychain, t http.RoundTripper, insecure bool) []crane.Option {
	opts := []crane.Option{crane.WithAuthFromKeychain(kc)}
	if t != nil {
		opts = append(opts, crane.WithTransport(t))
	}
	if insecure {
		opts = append(opts, crane.Insecure)
	}
	return opts
}

func buildRemoteOpts(kc authn.Keychain, t http.RoundTripper) []remote.Option {
	opts := []remote.Option{remote.WithAuthFromKeychain(kc)}
	if t != nil {
		opts = append(opts, remote.WithTransport(t))
	}
	return opts
}

func (c *Client) sourceCraneOptions(ctx context.Context) []crane.Option {
	opts := make([]crane.Option, 0, len(c.sourceCraneOpts)+1)
	opts = append(opts, crane.WithContext(ctx))
	return append(opts, c.sourceCraneOpts...)
}

func (c *Client) destinationCraneOptions(ctx context.Context) []crane.Option {
	opts := make([]crane.Option, 0, len(c.destinationCraneOpts)+1)
	opts = append(opts, crane.WithContext(ctx))
	return append(opts, c.destinationCraneOpts...)
}

func (c *Client) sourceRemoteOptions(ctx context.Context) []remote.Option {
	opts := make([]remote.Option, 0, len(c.sourceRemoteOpts)+1)
	opts = append(opts, remote.WithContext(ctx))
	return append(opts, c.sourceRemoteOpts...)
}

func (c *Client) destinationRemoteOptions(ctx context.Context) []remote.Option {
	opts := make([]remote.Option, 0, len(c.destinationRemoteOpts)+1)
	opts = append(opts, remote.WithContext(ctx))
	return append(opts, c.destinationRemoteOpts...)
}
