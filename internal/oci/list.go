// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/crane"
)

// ListTags returns all tags in repo.
func (c *Client) ListTags(ctx context.Context, repo string) ([]string, error) {
	tags, err := crane.ListTags(repo, c.craneOptions(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("list tags %s: %w", repo, err)
	}
	return tags, nil
}
