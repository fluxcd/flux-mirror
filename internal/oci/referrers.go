// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Referrer is a single OCI 1.1 referrer (cosign signature, SBOM, attestation,
// etc.) addressed by digest. We deliberately don't expose the underlying
// go-containerregistry v1.Descriptor here — callers shouldn't need it, and
// keeping the public surface small makes the upstream dep swappable.
type Referrer struct {
	// Digest is the referrer manifest's digest, e.g. "sha256:abc...".
	Digest string
	// ArtifactType is the manifest's artifactType field if set; empty otherwise.
	ArtifactType string
}

// SnapshotReferrers fetches the referrers index for repo@digest once and
// returns it as a list of Referrer. Snapshotting up-front fixes the set
// for the duration of any retries the caller does (the runner retries the
// whole tag job on transient errors), avoiding "list-changed-mid-retry"
// inconsistencies.
func (c *Client) SnapshotReferrers(ctx context.Context, repo, digest string) ([]Referrer, error) {
	srcRepo, err := name.NewRepository(repo, c.staticNameOpts...)
	if err != nil {
		return nil, fmt.Errorf("parse repository %q: %w", repo, err)
	}
	srcDigest := srcRepo.Digest(digest)

	idx, err := remote.Referrers(srcDigest, c.remoteOpts(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("list referrers for %s: %w", srcDigest, err)
	}
	manifest, err := idx.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("read referrers index for %s: %w", srcDigest, err)
	}

	out := make([]Referrer, 0, len(manifest.Manifests))
	for _, desc := range manifest.Manifests {
		out = append(out, Referrer{
			Digest:       desc.Digest.String(),
			ArtifactType: desc.ArtifactType,
		})
	}
	return out, nil
}

// CopyReferrer copies a single referrer manifest (and its blobs) from
// srcRepo@digest to dstRepo@digest, preserving the digest. Referrers are
// typically small single-arch manifests, so jobs=1 is fine.
func (c *Client) CopyReferrer(ctx context.Context, srcRepo, dstRepo, digest string) error {
	src := srcRepo + "@" + digest
	dst := dstRepo + "@" + digest
	if err := c.CopyTag(ctx, src, dst, 1); err != nil {
		return fmt.Errorf("copy referrer %s -> %s: %w", src, dst, err)
	}
	return nil
}

// remoteOpts is the lower-level transport+auth slice used when crane doesn't
// expose what we need (e.g. remote.Referrers has no crane wrapper).
func (c *Client) remoteOpts(ctx context.Context) []remote.Option {
	opts := make([]remote.Option, 0, len(c.staticRemoteOpts)+1)
	opts = append(opts, remote.WithContext(ctx))
	return append(opts, c.staticRemoteOpts...)
}
