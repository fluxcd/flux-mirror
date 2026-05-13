// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/crane"
)

// CopyTag copies src to dst preserving the digest. Multi-arch manifest lists
// are mirrored as a whole (no platform filtering). crane.Copy is content-
// addressed and idempotent at the blob level, so retrying a partially-copied
// tag is safe — existing blobs are skipped via HEAD/mount.
//
// jobs controls intra-copy parallelism: how many blobs of a single image
// (or per-platform image inside an index) are pushed concurrently. The
// runner already parallelizes across tags via errgroup; this is a second
// axis of parallelism that matters for large multi-arch images.
func (c *Client) CopyTag(ctx context.Context, src, dst string, jobs int) error {
	opts := c.craneOptions(ctx)
	if jobs > 1 {
		opts = append(opts, crane.WithJobs(jobs))
	}
	if err := crane.Copy(src, dst, opts...); err != nil {
		return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}
	return nil
}
