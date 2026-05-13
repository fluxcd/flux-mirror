// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Helm OCI media types per the Helm-OCI spec. These are the discriminators
// that distinguish a Helm chart manifest from any other OCI artifact in the
// same repository.
const (
	HelmConfigMediaType     = types.MediaType("application/vnd.cncf.helm.config.v1+json")
	HelmChartLayerMediaType = types.MediaType("application/vnd.cncf.helm.chart.content.v1.tar+gzip")
)

// IsHelmChart reports whether ref resolves to an OCI Helm chart manifest
// (i.e. its config blob is the Helm chart config type). Used to filter
// non-chart tags out of mixed-purpose OCI repos before treating them as
// chart versions. Returns (false, nil) on a 404 so callers can race-tolerate
// tags that disappear between a List and the per-tag check.
func (c *Client) IsHelmChart(ctx context.Context, ref string) (bool, error) {
	parsed, err := name.ParseReference(ref, c.staticNameOpts...)
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", ref, err)
	}
	desc, err := remote.Get(parsed, c.remoteOpts(ctx)...)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get manifest %s: %w", ref, err)
	}
	cfg, err := configMediaType(desc.Manifest)
	if err != nil {
		return false, fmt.Errorf("read config mediaType from %s: %w", ref, err)
	}
	return cfg == string(HelmConfigMediaType), nil
}

// HelmChartLayerDigest returns the chart-content layer digest of an OCI
// Helm chart at ref, or ("", nil) if the tag is missing.
//
// Comparing chart-layer digests is the right drift check for Helm OCI: two
// pushes of identical chart bytes produce different manifest digests
// (because helm.sh/helm/v4/pkg/registry.Client.Push stamps a creation
// timestamp into manifest annotations) but identical layer digests. So
// `sha256(srcChartTGZ) == HelmChartLayerDigest(dst)` is a content-equivalence
// check that survives the Helm-side timestamp non-determinism.
func (c *Client) HelmChartLayerDigest(ctx context.Context, ref string) (string, error) {
	parsed, err := name.ParseReference(ref, c.staticNameOpts...)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", ref, err)
	}
	desc, err := remote.Get(parsed, c.remoteOpts(ctx)...)
	if err != nil {
		if isNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("get manifest %s: %w", ref, err)
	}
	dig, err := helmChartLayerDigest(desc.Manifest)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", ref, err)
	}
	return dig, nil
}

// PullHelmChart fetches the .tgz chart layer of an OCI Helm chart at ref.
// Returns the raw tarball bytes and the source manifest digest. Errors if
// ref is not a Helm chart or has no chart-content layer.
func (c *Client) PullHelmChart(ctx context.Context, ref string) (data []byte, manifestDigest string, err error) {
	parsed, err := name.ParseReference(ref, c.staticNameOpts...)
	if err != nil {
		return nil, "", fmt.Errorf("parse %s: %w", ref, err)
	}
	opts := c.remoteOpts(ctx)
	desc, err := remote.Get(parsed, opts...)
	if err != nil {
		return nil, "", fmt.Errorf("get manifest %s: %w", ref, err)
	}

	layerDigest, err := helmChartLayerDigest(desc.Manifest)
	if err != nil {
		return nil, "", fmt.Errorf("inspect %s: %w", ref, err)
	}

	layerRef, err := name.NewDigest(parsed.Context().Name()+"@"+layerDigest, c.staticNameOpts...)
	if err != nil {
		return nil, "", fmt.Errorf("build layer ref for %s: %w", ref, err)
	}
	layer, err := remote.Layer(layerRef, opts...)
	if err != nil {
		return nil, "", fmt.Errorf("get layer %s: %w", layerRef, err)
	}
	// Helm chart layers are already gzip-compressed (.tgz). Compressed()
	// returns the bytes as uploaded — exactly what we want to re-push.
	rc, err := layer.Compressed()
	if err != nil {
		return nil, "", fmt.Errorf("open layer %s: %w", layerRef, err)
	}
	defer rc.Close()
	data, err = io.ReadAll(rc)
	if err != nil {
		return nil, "", fmt.Errorf("read layer %s: %w", layerRef, err)
	}
	return data, desc.Digest.String(), nil
}

// PushHelmChart uploads a Helm chart to dst. configJSON is the marshaled
// chart.Metadata (becomes the chart's OCI config blob); chartTGZ is the
// chart tarball.
//
// The resulting OCI manifest is fully deterministic for identical
// (configJSON, chartTGZ) inputs — no creation-timestamp annotation, no
// random fields. This is the difference from helm.sh/helm/v4/pkg/registry's
// Client.Push, which stamps `org.opencontainers.image.created = time.Now()`
// and so produces a different manifest digest on every push of the same
// content. Determinism is what lets the caller use Compare() to detect
// genuine drift between source and destination.
//
// Returns the destination manifest digest.
func (c *Client) PushHelmChart(ctx context.Context, dst string, configJSON, chartTGZ []byte) (string, error) {
	ref, err := name.ParseReference(dst, c.staticNameOpts...)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", dst, err)
	}
	opts := c.remoteOpts(ctx)

	configLayer := static.NewLayer(configJSON, HelmConfigMediaType)
	chartLayer := static.NewLayer(chartTGZ, HelmChartLayerMediaType)

	if err := remote.WriteLayer(ref.Context(), configLayer, opts...); err != nil {
		return "", fmt.Errorf("push helm config to %s: %w", dst, err)
	}
	if err := remote.WriteLayer(ref.Context(), chartLayer, opts...); err != nil {
		return "", fmt.Errorf("push helm chart layer to %s: %w", dst, err)
	}

	raw, manifestDigest, err := buildHelmManifest(configLayer, chartLayer)
	if err != nil {
		return "", err
	}
	if err := remote.Put(ref, &helmManifest{raw: raw, digest: manifestDigest}, opts...); err != nil {
		return "", fmt.Errorf("push helm manifest to %s: %w", dst, err)
	}
	return manifestDigest.String(), nil
}

// helmManifest wraps a pre-rendered OCI manifest for remote.Put. The extra
// descriptor methods (Digest/Size/MediaType) beyond Taggable's RawManifest
// let remote.Put skip a re-parse of the manifest body when uploading.
type helmManifest struct {
	raw    []byte
	digest v1.Hash
}

func (m *helmManifest) RawManifest() ([]byte, error)        { return m.raw, nil }
func (m *helmManifest) MediaType() (types.MediaType, error) { return types.OCIManifestSchema1, nil }
func (m *helmManifest) Digest() (v1.Hash, error)            { return m.digest, nil }
func (m *helmManifest) Size() (int64, error)                { return int64(len(m.raw)), nil }

// helmManifestJSON is the on-the-wire shape of an OCI image manifest, with
// only the fields a Helm chart manifest carries. Field order is significant
// for digest determinism — encoding/json marshals struct fields in their
// declared order.
type helmManifestJSON struct {
	SchemaVersion int              `json:"schemaVersion"`
	MediaType     string           `json:"mediaType"`
	Config        helmDescriptor   `json:"config"`
	Layers        []helmDescriptor `json:"layers"`
}

type helmDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

func buildHelmManifest(config, layer v1.Layer) ([]byte, v1.Hash, error) {
	cfgDigest, err := config.Digest()
	if err != nil {
		return nil, v1.Hash{}, fmt.Errorf("config digest: %w", err)
	}
	cfgSize, err := config.Size()
	if err != nil {
		return nil, v1.Hash{}, fmt.Errorf("config size: %w", err)
	}
	layerDigest, err := layer.Digest()
	if err != nil {
		return nil, v1.Hash{}, fmt.Errorf("layer digest: %w", err)
	}
	layerSize, err := layer.Size()
	if err != nil {
		return nil, v1.Hash{}, fmt.Errorf("layer size: %w", err)
	}
	raw, err := json.Marshal(helmManifestJSON{
		SchemaVersion: 2,
		MediaType:     string(types.OCIManifestSchema1),
		Config: helmDescriptor{
			MediaType: string(HelmConfigMediaType),
			Digest:    cfgDigest.String(),
			Size:      cfgSize,
		},
		Layers: []helmDescriptor{{
			MediaType: string(HelmChartLayerMediaType),
			Digest:    layerDigest.String(),
			Size:      layerSize,
		}},
	})
	if err != nil {
		return nil, v1.Hash{}, fmt.Errorf("marshal helm manifest: %w", err)
	}
	digest, _, err := v1.SHA256(bytes.NewReader(raw))
	if err != nil {
		return nil, v1.Hash{}, fmt.Errorf("hash helm manifest: %w", err)
	}
	return raw, digest, nil
}

// configMediaType decodes just enough of an OCI manifest to read its
// .config.mediaType field. Used by IsHelmChart to discriminate chart tags
// without paying for full manifest unmarshaling.
func configMediaType(rawManifest []byte) (string, error) {
	var m struct {
		Config struct {
			MediaType string `json:"mediaType"`
		} `json:"config"`
	}
	if err := json.Unmarshal(rawManifest, &m); err != nil {
		return "", err
	}
	return m.Config.MediaType, nil
}

// helmChartLayerDigest finds the chart-content layer in an OCI manifest and
// returns its digest. Errors if the manifest's config blob is not a Helm
// chart config or no chart layer is present.
func helmChartLayerDigest(rawManifest []byte) (string, error) {
	var m struct {
		Config struct {
			MediaType string `json:"mediaType"`
		} `json:"config"`
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(rawManifest, &m); err != nil {
		return "", err
	}
	if m.Config.MediaType != string(HelmConfigMediaType) {
		return "", fmt.Errorf("not a helm chart (config mediaType %q)", m.Config.MediaType)
	}
	for _, l := range m.Layers {
		if l.MediaType == string(HelmChartLayerMediaType) {
			return l.Digest, nil
		}
	}
	return "", fmt.Errorf("no helm chart content layer (mediaType %s)", HelmChartLayerMediaType)
}
