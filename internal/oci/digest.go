// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// CompareState describes the relationship between a source tag and the
// equivalent destination tag.
type CompareState int

const (
	// StateMissing means the destination does not have the tag.
	StateMissing CompareState = iota
	// StateEqual means src and dst resolve to the same digest.
	StateEqual
	// StateDrifted means dst has the tag but its digest differs from src.
	StateDrifted
)

func (s CompareState) String() string {
	switch s {
	case StateMissing:
		return "missing"
	case StateEqual:
		return "equal"
	case StateDrifted:
		return "drifted"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

// CompareResult is the outcome of comparing a src tag to a dst tag.
type CompareResult struct {
	State     CompareState
	SrcDigest string
	DstDigest string // empty when State == StateMissing
}

// Digest resolves ref to its current digest. Returns an error (including for
// 404 — caller decides what "missing" means in their context). Use Compare
// when you specifically want to classify missing/equal/drifted between two refs.
func (c *Client) Digest(ctx context.Context, ref string) (string, error) {
	dig, err := crane.Digest(ref, c.craneOptions(ctx)...)
	if err != nil {
		return "", fmt.Errorf("digest %s: %w", ref, err)
	}
	return dig, nil
}

// Compare resolves both refs to digests and classifies the relationship.
// Only a 404 from the destination is mapped to StateMissing — auth failures
// (401/403) are propagated as errors so the user sees a meaningful message
// instead of "tried to push to missing tag and got auth failure."
func (c *Client) Compare(ctx context.Context, src, dst string) (CompareResult, error) {
	srcDigest, err := c.Digest(ctx, src)
	if err != nil {
		return CompareResult{}, err
	}
	return c.CompareWithKnownSrc(ctx, srcDigest, dst)
}

// CompareWithKnownSrc skips the src-side Digest call by trusting the caller's
// srcDigest. Used in two cases:
//   - The src digest was already fetched at plan time (avoids a duplicate
//     round trip when the runner retries the tag job).
//   - The src ref is itself a `repo@digest` reference (the digest IS the
//     ref) — fetching it again would just round-trip back what we already
//     know. This is the common case for referrer mirroring.
func (c *Client) CompareWithKnownSrc(ctx context.Context, srcDigest, dst string) (CompareResult, error) {
	dstDigest, err := crane.Digest(dst, c.craneOptions(ctx)...)
	if err != nil {
		if isNotFound(err) {
			return CompareResult{State: StateMissing, SrcDigest: srcDigest}, nil
		}
		return CompareResult{}, fmt.Errorf("digest %s: %w", dst, err)
	}
	if srcDigest == dstDigest {
		return CompareResult{State: StateEqual, SrcDigest: srcDigest, DstDigest: dstDigest}, nil
	}
	return CompareResult{State: StateDrifted, SrcDigest: srcDigest, DstDigest: dstDigest}, nil
}

// isNotFound is true only for HTTP 404. 401/403 mean "auth required /
// forbidden", not "tag doesn't exist" — surfacing them as missing would
// silently mask credential problems. Some registries (GHCR, ECR) do return
// 401 for missing repos to anon clients, but once you're authenticated a
// 401/403 is a real auth failure, and that's the behavior we want to fail
// loudly for.
func isNotFound(err error) bool {
	var terr *transport.Error
	if !errors.As(err, &terr) {
		return false
	}
	return terr.StatusCode == http.StatusNotFound
}
