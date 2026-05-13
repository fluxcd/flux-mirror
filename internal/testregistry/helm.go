// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package testregistry

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/chart/v2/util"
)

// PackageChart packages the chart at chartDir at the requested version and
// returns the tarball bytes. Uses Helm v4's chartutil.Save underneath so
// the resulting .tgz is bit-for-bit what `helm package` would produce —
// matters for chart-layer digest assertions in destination-side tests.
func PackageChart(t testing.TB, chartDir, version string) []byte {
	t.Helper()
	chrt, err := loader.LoadDir(chartDir)
	if err != nil {
		t.Fatalf("load chart %s: %s", chartDir, err)
	}
	chrt.Metadata.Version = version

	tmp := t.TempDir()
	path, err := util.Save(chrt, tmp)
	if err != nil {
		t.Fatalf("save chart %s@%s: %s", chartDir, version, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read packaged chart %s: %s", path, err)
	}
	return data
}

// ChartConfigJSON extracts a chart's metadata from a .tgz and returns the
// JSON-encoded form used as the OCI Helm config blob (mediaType
// application/vnd.cncf.helm.config.v1+json). This is the same blob the
// production push path constructs, so test-side fixtures land at the same
// chart-layer / manifest digests as a real sync.
func ChartConfigJSON(t testing.TB, tgz []byte) []byte {
	t.Helper()
	chrt, err := loader.LoadArchive(bytes.NewReader(tgz))
	if err != nil {
		t.Fatalf("load chart archive: %s", err)
	}
	cfg, err := json.Marshal(chrt.Metadata)
	if err != nil {
		t.Fatalf("marshal chart metadata: %s", err)
	}
	return cfg
}
