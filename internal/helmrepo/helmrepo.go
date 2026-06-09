// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

// Package helmrepo adapts an HTTP/S index.yaml Helm chart repository to a
// Source interface for use by the charts mirror. Destination push is handled
// directly by internal/oci. Mirroring a Helm chart that already lives in an OCI
// registry is an OCI-to-OCI copy and goes through the artifacts mirror, not
// here — the charts mirror exists only for the HTTP/S → OCI conversion.
package helmrepo

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"helm.sh/helm/v4/pkg/cli"
	repo "helm.sh/helm/v4/pkg/repo/v1"
)

// Source is a Helm chart catalog backed by an HTTP/S index.yaml repository.
type Source interface {
	// ListVersions returns all available versions of chartName, in arbitrary
	// order. Sort and limit are applied downstream by the selector.
	ListVersions(ctx context.Context, chartName string) ([]string, error)

	// Download fetches the chart .tgz bytes for chartName at version.
	Download(ctx context.Context, chartName, version string) ([]byte, error)
}

// New builds the HTTP/S Source for sourceURL (auth via Helm repositories.yaml
// when present). Only http and https are supported: an OCI Helm chart is
// mirrored OCI-to-OCI through the artifacts mirror, never here.
func New(sourceURL string) (Source, error) {
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
	default:
		return nil, fmt.Errorf("unsupported source scheme %q (want http or https)", u.Scheme)
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

// VersionToTag implements the `+` → `_` substitution Helm OCI uses to encode
// semver build metadata in OCI tags (since `+` isn't a valid OCI tag
// character). The charts mirror needs it to construct destination tags.
func VersionToTag(version string) string { return strings.ReplaceAll(version, "+", "_") }

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
