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
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/fluxcd/flux-mirror/internal/oci"
	"helm.sh/helm/v4/pkg/cli"
	repo "helm.sh/helm/v4/pkg/repo/v1"
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
//   - http, https → HTTPSource (auth via Helm repositories.yaml when present)
//   - oci         → OCISource (auth via the OCI client: ambient Docker config, or a per-host JWT from the config's auth section)
func New(sourceURL string, ociClient *oci.Client) (Source, error) {
	u, err := url.Parse(sourceURL)
	if err != nil {
		return nil, fmt.Errorf("parse source URL %q: %w", sourceURL, err)
	}
	switch u.Scheme {
	case "http", "https":
		entry, err := httpRepositoryEntry(sourceURL)
		if err != nil {
			return nil, err
		}
		return NewHTTPSourceWithEntry(sourceURL, entry)
	case "oci":
		return NewOCISource(strings.TrimPrefix(sourceURL, "oci://"), ociClient), nil
	default:
		return nil, fmt.Errorf("unsupported source scheme %q (want http, https, or oci)", u.Scheme)
	}
}

func httpRepositoryEntry(sourceURL string) (*repo.Entry, error) {
	settings := cli.New()
	repos, err := repo.LoadFile(settings.RepositoryConfig)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("load Helm repository config %s: %w", settings.RepositoryConfig, err)
	}
	sourceKey, err := normalizeRepositoryURL(sourceURL)
	if err != nil {
		return nil, fmt.Errorf("normalize source URL %q: %w", sourceURL, err)
	}
	for _, entry := range repos.Repositories {
		if entry == nil || strings.TrimSpace(entry.URL) == "" {
			continue
		}
		entryKey, err := normalizeRepositoryURL(entry.URL)
		if err != nil {
			return nil, fmt.Errorf("normalize Helm repository %q URL %q: %w", entry.Name, entry.URL, err)
		}
		if entryKey == sourceKey {
			entryCopy := *entry
			return &entryCopy, nil
		}
	}
	return nil, nil
}

func normalizeRepositoryURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("missing scheme or host")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}
