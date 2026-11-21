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

	idx, err := remote.Referrers(srcDigest, c.sourceRemoteOptions(ctx)...)
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

// DestinationReferrers returns the digests the destination lists as referrers
// of repo@digest, through the referrers API or the fallback tag.
func (c *Client) DestinationReferrers(ctx context.Context, repo, digest string) (map[string]struct{}, error) {
	dstRepo, err := name.NewRepository(repo, c.staticNameOpts...)
	if err != nil {
		return nil, fmt.Errorf("parse repository %q: %w", repo, err)
	}
	dstDigest := dstRepo.Digest(digest)
	idx, err := remote.Referrers(dstDigest, c.destinationRemoteOptions(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("list referrers for %s: %w", dstDigest, err)
	}
	manifest, err := idx.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("read referrers index for %s: %w", dstDigest, err)
	}
	out := make(map[string]struct{}, len(manifest.Manifests))
	for _, desc := range manifest.Manifests {
		out[desc.Digest.String()] = struct{}{}
	}
	return out, nil
}

// RelinkReferrer commits a referrer manifest that is already stored at the
// destination once more, so that the destination's referrers list gains its
// entry. It uses Put, not Push: Push returns early when the manifest exists
// and never reaches the referrers update.
func (c *Client) RelinkReferrer(ctx context.Context, srcRepo, dstRepo, digest string) error {
	src := srcRepo + "@" + digest
	dst := dstRepo + "@" + digest
	srcRef, err := name.ParseReference(src, c.staticNameOpts...)
	if err != nil {
		return fmt.Errorf("parse source %q: %w", src, err)
	}
	dstRef, err := name.ParseReference(dst, c.staticNameOpts...)
	if err != nil {
		return fmt.Errorf("parse destination %q: %w", dst, err)
	}
	puller, err := remote.NewPuller(c.sourceRemoteOptions(ctx)...)
	if err != nil {
		return fmt.Errorf("source puller for %s: %w", src, err)
	}
	pusher, err := remote.NewPusher(c.destinationRemoteOptions(ctx)...)
	if err != nil {
		return fmt.Errorf("destination pusher for %s: %w", dst, err)
	}
	desc, err := puller.Get(ctx, srcRef)
	if err != nil {
		return fmt.Errorf("relink referrer %s -> %s: fetching %q: %w", src, dst, src, err)
	}
	if err := pusher.Put(ctx, dstRef, desc); err != nil {
		return fmt.Errorf("relink referrer %s -> %s: %w", src, dst, err)
	}
	return nil
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
