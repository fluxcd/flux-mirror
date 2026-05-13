// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

// Package helmrepo adapts Helm chart sources (HTTP/S index.yaml repositories
// and OCI Helm registries) to a single Source interface for use by the
// charts mirror. Destination push is handled directly by internal/oci —
// only sources need an adapter, since they have two very different protocols
// (index.yaml + tarball download vs. OCI manifest + chart-content layer).
package helmrepo

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/fluxcd/flux-mirror/internal/oci"
)

// Source is a Helm chart catalog backed by HTTP/S or OCI.
type Source interface {
	// ListVersions returns all available versions of chartName, in arbitrary
	// order. Sort and limit are applied downstream by the selector.
	ListVersions(ctx context.Context, chartName string) ([]string, error)

	// Download fetches the chart .tgz bytes for chartName at version.
	Download(ctx context.Context, chartName, version string) ([]byte, error)
}

// New picks the Source implementation by URL scheme:
//   - http, https → HTTPSource (assumed public; no auth wiring per spec)
//   - oci         → OCISource (auth via ociClient's ambient Docker config)
func New(sourceURL string, ociClient *oci.Client) (Source, error) {
	u, err := url.Parse(sourceURL)
	if err != nil {
		return nil, fmt.Errorf("parse source URL %q: %w", sourceURL, err)
	}
	switch u.Scheme {
	case "http", "https":
		return NewHTTPSource(sourceURL), nil
	case "oci":
		return NewOCISource(strings.TrimPrefix(sourceURL, "oci://"), ociClient), nil
	default:
		return nil, fmt.Errorf("unsupported source scheme %q (want http, https, or oci)", u.Scheme)
	}
}
