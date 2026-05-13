// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

// Package charts implements sync.EntryMirror for ChartEntry config entries.
// It downloads chart .tgz bytes from the source (HTTP/S or OCI), then pushes
// them to the OCI destination as a deterministic Helm-OCI artifact.
//
// "Deterministic" matters for drift detection: the destination chart-layer
// digest equals sha256(srcChartTGZ), so a re-run with no changes is a clean
// no-op rather than a perpetual "different" state. See oci.HelmChartLayerDigest
// for the why.
package charts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"helm.sh/helm/v4/pkg/chart/v2/loader"

	"github.com/fluxcd/flux-mirror/internal/config"
	"github.com/fluxcd/flux-mirror/internal/helmrepo"
	"github.com/fluxcd/flux-mirror/internal/oci"
	"github.com/fluxcd/flux-mirror/internal/selector"
	"github.com/fluxcd/flux-mirror/internal/sync"
)

// Options configures the chart mirror across all entries. Mirrors the shape
// of internal/artifacts.Options for symmetry between the two halves.
type Options struct {
	// Overwrite forces overwrite=true on every entry, regardless of the
	// per-entry config. This is the global --overwrite flag.
	Overwrite bool
	// DryRun skips the actual push and reports would-* outcomes instead.
	DryRun bool
	// Verbose toggles selector debug output (logs versions excluded by
	// the semver constraint with reasons).
	Verbose bool
	// Logger is used for warnings and per-version progress lines.
	Logger *slog.Logger
}

// New builds an EntryMirror for the given chart entry. The Source
// implementation is picked from entry.Source's URL scheme.
func New(client *oci.Client, entry config.ChartEntry, opts Options) (sync.EntryMirror, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	src, err := helmrepo.New(entry.Source, client)
	if err != nil {
		return nil, err
	}
	return &mirror{client: client, entry: entry, source: src, opts: opts}, nil
}

type mirror struct {
	client *oci.Client
	entry  config.ChartEntry
	source helmrepo.Source
	opts   Options
}

// versionJob captures everything resolved at plan time for a single chart
// version, so the Job's Run closure stays small.
type versionJob struct {
	version   string
	dst       string // fully-qualified destination ref including chart-name path
	overwrite bool
}

// Plan resolves matching chart versions and emits one Job per version.
// Plan.Name uses "<source>/<chartName>" so logs and summary lines disambiguate
// when multiple charts share a source URL.
func (m *mirror) Plan(ctx context.Context) (sync.Plan, error) {
	plan := sync.Plan{Name: m.planName()}

	versions, err := m.source.ListVersions(ctx, m.entry.Name)
	if err != nil {
		return plan, err
	}

	// Versions feed the selector as opaque strings — same shape as artifact tags.
	sel := config.Selector{
		Semver: m.entry.EffectiveVersion(),
		Limit:  new(m.entry.EffectiveLimit()),
	}
	res, err := selector.Select(versions, sel, selector.Options{Verbose: m.opts.Verbose})
	if err != nil {
		return plan, fmt.Errorf("select versions: %w", err)
	}
	for _, ex := range res.Excluded {
		m.opts.Logger.Debug("version excluded by selector",
			"chart", m.entry.Name, "version", ex.Tag, "reason", ex.Reason)
	}

	overwrite := m.entry.Overwrite || m.opts.Overwrite
	dstRepo := m.dstRepo()

	plan.Jobs = make([]sync.Job, 0, len(res.Tags))
	for _, version := range res.Tags {
		job := versionJob{
			version:   version,
			dst:       dstRepo + ":" + helmrepo.VersionToTag(version),
			overwrite: overwrite,
		}
		plan.Jobs = append(plan.Jobs, sync.Job{
			ID:  version,
			Dst: job.dst,
			Run: func(jctx context.Context) (sync.Outcome, error) { return m.runVersion(jctx, job) },
		})
	}
	return plan, nil
}

// runVersion mirrors one chart version. Idempotent — safe to retry under
// the runner's per-tag budget.
func (m *mirror) runVersion(ctx context.Context, j versionJob) (sync.Outcome, error) {
	start := time.Now()
	chartRef := m.entry.Name + "@" + j.version
	m.opts.Logger.Info("mirroring chart", "chart", chartRef, "dst", j.dst)

	oc, err := m.mirrorVersion(ctx, j)
	if err != nil {
		return "", err
	}
	m.opts.Logger.Info("chart done",
		"chart", chartRef,
		"dst", j.dst,
		"outcome", string(oc),
		"elapsed", time.Since(start).Round(time.Millisecond))
	return oc, nil
}

func (m *mirror) mirrorVersion(ctx context.Context, j versionJob) (sync.Outcome, error) {
	// Download src .tgz once. Bytes are pushed verbatim as the chart layer,
	// so sha256(tgz) is also the chart-layer digest at the destination if
	// our push goes through.
	tgz, err := m.source.Download(ctx, m.entry.Name, j.version)
	if err != nil {
		return "", err
	}
	srcLayerDigest := digestSHA256(tgz)

	// Compare against the destination's chart-layer digest, not manifest
	// digest — manifest digests are non-deterministic for charts pushed by
	// helm.sh/helm/v4/pkg/registry.Client.Push (timestamp annotation), but
	// layer digests are content-addressed and stable.
	dstLayerDigest, err := m.client.HelmChartLayerDigest(ctx, j.dst)
	if err != nil {
		return "", err
	}

	switch dstLayerDigest {
	case "":
		if m.opts.DryRun {
			return sync.OutcomeWouldCopy, nil
		}
		if err := m.push(ctx, j.dst, tgz); err != nil {
			return "", err
		}
		return sync.OutcomeCopied, nil
	case srcLayerDigest:
		return sync.OutcomeSkipped, nil
	default:
		if !j.overwrite {
			m.opts.Logger.Warn("destination chart drifted from source; skipping (overwrite=false)",
				"chart", m.entry.Name, "version", j.version,
				"src_digest", srcLayerDigest, "dst_digest", dstLayerDigest)
			return sync.OutcomeDrifted, nil
		}
		if m.opts.DryRun {
			return sync.OutcomeWouldOverwrite, nil
		}
		if err := m.push(ctx, j.dst, tgz); err != nil {
			return "", err
		}
		return sync.OutcomeOverwritten, nil
	}
}

// push extracts chart metadata from the .tgz to build the OCI config blob,
// then uploads the deterministic Helm-OCI artifact.
func (m *mirror) push(ctx context.Context, dst string, tgz []byte) error {
	chrt, err := loader.LoadArchive(bytes.NewReader(tgz))
	if err != nil {
		return fmt.Errorf("load chart archive: %w", err)
	}
	cfgJSON, err := json.Marshal(chrt.Metadata)
	if err != nil {
		return fmt.Errorf("marshal chart metadata: %w", err)
	}
	if _, err := m.client.PushHelmChart(ctx, dst, cfgJSON, tgz); err != nil {
		return err
	}
	return nil
}

func (m *mirror) planName() string {
	return strings.TrimRight(m.entry.Source, "/") + "/" + m.entry.Name
}

func (m *mirror) dstRepo() string {
	base := strings.TrimPrefix(m.entry.Destination, "oci://")
	return strings.TrimRight(base, "/") + "/" + m.entry.Name
}

func digestSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
