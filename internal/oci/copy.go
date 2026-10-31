// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/logs"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// CopyTag copies src to dst preserving the digest. Multi-arch manifest lists
// are mirrored as a whole (no platform filtering). The puller and pusher use
// separate auth/transport settings, so a source and destination on the same
// host can carry different credentials (hosts[].direction).
//
// jobs controls intra-copy parallelism: how many blobs of a single image
// (or per-platform image inside an index) are pushed concurrently. The
// runner already parallelizes across tags via errgroup; this is a second
// axis of parallelism that matters for large multi-arch images.
func (c *Client) CopyTag(ctx context.Context, src, dst string, jobs int) error {
	srcRef, err := name.ParseReference(src, c.staticNameOpts...)
	if err != nil {
		return fmt.Errorf("parse source %q: %w", src, err)
	}
	dstRef, err := name.ParseReference(dst, c.staticNameOpts...)
	if err != nil {
		return fmt.Errorf("parse destination %q: %w", dst, err)
	}

	pullOpts := c.sourceRemoteOptions(ctx)
	pushOpts := c.destinationRemoteOptions(ctx)
	if jobs > 1 {
		pullOpts = append(pullOpts, remote.WithJobs(jobs))
		pushOpts = append(pushOpts, remote.WithJobs(jobs))
	}
	puller, err := remote.NewPuller(pullOpts...)
	if err != nil {
		return fmt.Errorf("source puller for %s: %w", src, err)
	}
	pusher, err := remote.NewPusher(pushOpts...)
	if err != nil {
		return fmt.Errorf("destination pusher for %s: %w", dst, err)
	}

	logs.Progress.Printf("Copying from %v to %v", srcRef, dstRef)
	desc, err := puller.Get(ctx, srcRef)
	if err != nil {
		return fmt.Errorf("copy %s -> %s: fetching %q: %w", src, dst, src, err)
	}
	if err := pusher.Push(ctx, dstRef, desc); err != nil {
		return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}
	return nil
}
