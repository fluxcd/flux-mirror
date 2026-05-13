// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package helmrepo

import (
	"bytes"
	"context"
	"testing"

	"github.com/fluxcd/pkg/helmtestserver"
	. "github.com/onsi/gomega"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
)

const fixtureChart = "testdata/podinfo"

func newHelmServer(t *testing.T, versions ...string) *helmtestserver.HelmServer {
	t.Helper()
	srv, err := helmtestserver.NewTempHelmServer()
	if err != nil {
		t.Fatalf("new helm test server: %s", err)
	}
	t.Cleanup(srv.Stop)
	for _, v := range versions {
		if err := srv.PackageChartWithVersion(fixtureChart, v); err != nil {
			t.Fatalf("package %s@%s: %s", fixtureChart, v, err)
		}
	}
	if err := srv.GenerateIndex(); err != nil {
		t.Fatalf("generate index: %s", err)
	}
	srv.Start()
	return srv
}

func TestHTTPSource_ListVersions(t *testing.T) {
	g := NewWithT(t)
	srv := newHelmServer(t, "0.1.0", "0.2.0", "1.0.0")

	src := NewHTTPSource(srv.URL())
	versions, err := src.ListVersions(context.Background(), "podinfo")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(versions).To(ConsistOf("0.1.0", "0.2.0", "1.0.0"))
}

func TestHTTPSource_ListVersions_UnknownChart(t *testing.T) {
	g := NewWithT(t)
	srv := newHelmServer(t, "0.1.0")

	src := NewHTTPSource(srv.URL())
	_, err := src.ListVersions(context.Background(), "nope")
	g.Expect(err).To(MatchError(ContainSubstring(`chart "nope" not found`)))
}

func TestHTTPSource_Download(t *testing.T) {
	g := NewWithT(t)
	srv := newHelmServer(t, "0.1.0", "1.2.3")

	src := NewHTTPSource(srv.URL())
	tgz, err := src.Download(context.Background(), "podinfo", "1.2.3")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(tgz).ToNot(BeEmpty())

	// Verify the bytes really are a chart tarball at the requested version.
	chrt, err := loader.LoadArchive(bytes.NewReader(tgz))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(chrt.Metadata.Name).To(Equal("podinfo"))
	g.Expect(chrt.Metadata.Version).To(Equal("1.2.3"))
}

func TestHTTPSource_Download_UnknownVersion(t *testing.T) {
	g := NewWithT(t)
	srv := newHelmServer(t, "0.1.0")

	src := NewHTTPSource(srv.URL())
	_, err := src.Download(context.Background(), "podinfo", "9.9.9")
	g.Expect(err).To(HaveOccurred())
}

// TestHTTPSource_IndexCachedOnce confirms that subsequent ListVersions calls
// reuse the parsed index (no re-fetch). We verify by stopping the server
// after the first call and seeing the second succeed.
func TestHTTPSource_IndexCachedOnce(t *testing.T) {
	g := NewWithT(t)
	srv := newHelmServer(t, "0.1.0", "0.2.0")

	src := NewHTTPSource(srv.URL())
	_, err := src.ListVersions(context.Background(), "podinfo")
	g.Expect(err).ToNot(HaveOccurred())

	srv.Stop()
	// Index already cached — second call should not hit the network.
	versions, err := src.ListVersions(context.Background(), "podinfo")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(versions).To(ConsistOf("0.1.0", "0.2.0"))
}
