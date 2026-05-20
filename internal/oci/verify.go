// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/fluxcd/flux-mirror/internal/config"
)

const (
	minBundleVersion              = "v0.3"
	sigstoreBundleMediaTypePrefix = "application/vnd.dev.sigstore.bundle"
)

// Verifier verifies cosign keyless signatures stored as OCI referrers.
type Verifier struct {
	client *Client

	mu          sync.Mutex
	trustedRoot *root.TrustedRoot
}

// NewVerifier returns a verifier using the same registry options as the OCI client.
func NewVerifier(client *Client) *Verifier {
	if client == nil {
		client = NewClient()
	}
	return &Verifier{client: client}
}

// Verify checks the cosign signature for ref against the configured OIDC identities.
func (v *Verifier) Verify(ctx context.Context, ref string, cfg config.ArtifactVerification) error {
	if strings.TrimSpace(cfg.Provider) != config.VerifyProviderCosign {
		return fmt.Errorf("unsupported verification provider %q", cfg.Provider)
	}
	if len(cfg.MatchOIDCIdentity) == 0 {
		return fmt.Errorf("at least one OIDC identity matcher is required")
	}

	ref = strings.TrimPrefix(ref, "oci://")
	parsed, err := name.ParseReference(ref, v.client.staticNameOpts...)
	if err != nil {
		return fmt.Errorf("parse reference %q: %w", ref, err)
	}

	opts := v.remoteOptions(ctx)
	desc, err := remote.Get(parsed, opts...)
	if err != nil {
		return fmt.Errorf("fetch descriptor for %q: %w", ref, err)
	}

	repo := parsed.Context()
	digestRef := repo.Digest(desc.Digest.String())
	idx, err := remote.Referrers(digestRef, opts...)
	if err != nil {
		return fmt.Errorf("list referrers for %s: %w", digestRef, err)
	}
	manifest, err := idx.IndexManifest()
	if err != nil {
		return fmt.Errorf("read referrers index for %s: %w", digestRef, err)
	}

	bundleBytes, err := findSigstoreBundle(repo, manifest, opts...)
	if err != nil {
		return err
	}

	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleBytes); err != nil {
		return fmt.Errorf("parse sigstore bundle: %w", err)
	}
	if !b.MinVersion(minBundleVersion) {
		return fmt.Errorf("unsupported sigstore bundle version: minimum %s required", minBundleVersion)
	}

	trustedRoot, err := v.getTrustedRoot()
	if err != nil {
		return err
	}
	sigVerifier, err := verify.NewVerifier(trustedRoot,
		verify.WithSignedCertificateTimestamps(1),
		verify.WithIntegratedTimestamps(1),
		verify.WithTransparencyLog(1),
	)
	if err != nil {
		return fmt.Errorf("create signature verifier: %w", err)
	}

	identityOptions, err := certificateIdentityOptions(cfg.MatchOIDCIdentity)
	if err != nil {
		return err
	}
	digestBytes, err := hex.DecodeString(desc.Digest.Hex)
	if err != nil {
		return fmt.Errorf("decode digest %s: %w", desc.Digest.String(), err)
	}

	policyOptions := append([]verify.PolicyOption(nil), identityOptions...)
	policy := verify.NewPolicy(
		verify.WithArtifactDigest(desc.Digest.Algorithm, digestBytes),
		policyOptions...,
	)
	if _, err := sigVerifier.Verify(&b, policy); err != nil {
		return fmt.Errorf("signature verification failed: %w", err)
	}
	return nil
}

func (v *Verifier) remoteOptions(ctx context.Context) []remote.Option {
	opts := make([]remote.Option, 0, len(v.client.staticRemoteOpts)+1)
	opts = append(opts, remote.WithContext(ctx))
	return append(opts, v.client.staticRemoteOpts...)
}

func (v *Verifier) getTrustedRoot() (*root.TrustedRoot, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.trustedRoot != nil {
		return v.trustedRoot, nil
	}

	opts := tuf.DefaultOptions()
	opts.WithDisableLocalCache()
	tufClient, err := tuf.New(opts)
	if err != nil {
		return nil, fmt.Errorf("create TUF client: %w", err)
	}
	trustedRootJSON, err := tufClient.GetTarget("trusted_root.json")
	if err != nil {
		return nil, fmt.Errorf("fetch trusted root: %w", err)
	}
	trustedRoot, err := root.NewTrustedRootFromJSON(trustedRootJSON)
	if err != nil {
		return nil, fmt.Errorf("parse trusted root: %w", err)
	}
	v.trustedRoot = trustedRoot
	return trustedRoot, nil
}

func certificateIdentityOptions(identities []config.OIDCIdentity) ([]verify.PolicyOption, error) {
	opts := make([]verify.PolicyOption, 0, len(identities))
	for i, id := range identities {
		certID, err := verify.NewShortCertificateIdentity(id.Issuer, "", "", id.Subject)
		if err != nil {
			return nil, fmt.Errorf("create OIDC identity matcher %d: %w", i, err)
		}
		opts = append(opts, verify.WithCertificateIdentity(certID))
	}
	return opts, nil
}

func findSigstoreBundle(repo name.Repository, manifest *v1.IndexManifest, opts ...remote.Option) ([]byte, error) {
	for _, m := range manifest.Manifests {
		img, err := remote.Image(repo.Digest(m.Digest.String()), opts...)
		if err != nil {
			continue
		}
		layers, err := img.Layers()
		if err != nil || len(layers) != 1 {
			continue
		}
		mediaType, err := layers[0].MediaType()
		if err != nil || !strings.HasPrefix(string(mediaType), sigstoreBundleMediaTypePrefix) {
			continue
		}
		reader, err := layers[0].Uncompressed()
		if err != nil {
			continue
		}
		data, readErr := io.ReadAll(reader)
		if closeErr := reader.Close(); closeErr != nil {
			return nil, fmt.Errorf("close sigstore bundle layer: %w", closeErr)
		}
		if readErr != nil {
			return nil, fmt.Errorf("read sigstore bundle layer: %w", readErr)
		}
		return data, nil
	}
	return nil, fmt.Errorf("no sigstore bundle found in referrers")
}
