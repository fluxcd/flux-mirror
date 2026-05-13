// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package helmrepo

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/fluxcd/flux-mirror/internal/oci"
)

// OCISource is a Helm chart source backed by an OCI registry. The Helm-OCI
// convention is one OCI repository per chart name (at <baseRepo>/<chartName>),
// with each chart version stored as an OCI tag.
type OCISource struct {
	baseRepo string // e.g. "ghcr.io/example/charts" — no oci:// prefix
	client   *oci.Client
}

// NewOCISource constructs an OCISource. baseRepo must already have any
// `oci://` prefix stripped.
func NewOCISource(baseRepo string, client *oci.Client) *OCISource {
	return &OCISource{baseRepo: strings.TrimRight(baseRepo, "/"), client: client}
}

// ListVersions enumerates Helm chart tags in the OCI repo at baseRepo/chartName.
// Tags whose OCI manifest is not a Helm chart (cosign signatures, SBOMs, etc.)
// are filtered out. The per-tag IsHelmChart calls run in parallel since each
// is an independent network round trip; the cap is fixed (not user-tunable)
// because this happens at plan time, before --concurrency takes effect.
func (s *OCISource) ListVersions(ctx context.Context, chartName string) ([]string, error) {
	chartRepo := s.chartRepo(chartName)
	tags, err := s.client.ListTags(ctx, chartRepo)
	if err != nil {
		return nil, err
	}

	keep := make([]bool, len(tags))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(listVersionsConcurrency)
	for i, tag := range tags {
		g.Go(func() error {
			ok, err := s.client.IsHelmChart(gctx, chartRepo+":"+tag)
			if err != nil {
				return fmt.Errorf("inspect %s:%s: %w", chartRepo, tag, err)
			}
			keep[i] = ok
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	out := make([]string, 0, len(tags))
	for i, tag := range tags {
		if keep[i] {
			out = append(out, TagToVersion(tag))
		}
	}
	return out, nil
}

// Download fetches the chart .tgz for chartName at version. The version is
// translated to its OCI tag form (`+` → `_`) before resolution.
func (s *OCISource) Download(ctx context.Context, chartName, version string) ([]byte, error) {
	ref := s.chartRepo(chartName) + ":" + VersionToTag(version)
	data, _, err := s.client.PullHelmChart(ctx, ref)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (s *OCISource) chartRepo(chartName string) string {
	return s.baseRepo + "/" + chartName
}

// TagToVersion / VersionToTag implement the `+` ↔ `_` substitution that Helm
// OCI uses to encode semver build metadata in OCI tags (since `+` isn't a
// valid OCI tag character). Exported because the chart mirror needs the same
// substitution to construct destination refs.
func TagToVersion(tag string) string     { return strings.ReplaceAll(tag, "_", "+") }
func VersionToTag(version string) string { return strings.ReplaceAll(version, "+", "_") }

// listVersionsConcurrency caps the parallel IsHelmChart probes during ListVersions.
// 8 keeps a typical chart repo (5–30 tags) latency-bound by ~1 round trip.
const listVersionsConcurrency = 8
