// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package helmrepo

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"

	. "github.com/onsi/gomega"
	"helm.sh/helm/v4/pkg/chart/v2/loader"

	"github.com/fluxcd/flux-mirror/internal/oci"
	"github.com/fluxcd/flux-mirror/internal/testregistry"
)

var ociReg string

func TestMain(m *testing.M) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, err := testregistry.Start(ctx)
	if err != nil {
		panic(fmt.Sprintf("start registry: %s", err))
	}
	ociReg = addr
	os.Exit(m.Run())
}

// pushHelmFixture packages and uploads a chart at version into the OCI repo
// at chartRepo. The OCI tag is derived from version (`+` → `_`).
func pushHelmFixture(t *testing.T, client *oci.Client, chartRepo, version string) {
	t.Helper()
	tgz := testregistry.PackageChart(t, "testdata/podinfo", version)
	cfg := testregistry.ChartConfigJSON(t, tgz)
	ref := chartRepo + ":" + VersionToTag(version)
	if _, err := client.PushHelmChart(context.Background(), ref, cfg, tgz); err != nil {
		t.Fatalf("push %s: %s", ref, err)
	}
}

func TestOCISource_ListVersions(t *testing.T) {
	g := NewWithT(t)
	client := oci.NewClient(oci.Insecure())
	base := testregistry.Repo(ociReg, "oci-list")
	// One chart with three versions (each chart name is its own OCI repo).
	chartRepo := base + "/podinfo"
	pushHelmFixture(t, client, chartRepo, "0.1.0")
	pushHelmFixture(t, client, chartRepo, "0.2.0")
	pushHelmFixture(t, client, chartRepo, "1.0.0")

	src := NewOCISource(base, client)
	versions, err := src.ListVersions(context.Background(), "podinfo")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(versions).To(ConsistOf("0.1.0", "0.2.0", "1.0.0"))
}

func TestOCISource_ListVersions_FiltersNonChartTags(t *testing.T) {
	g := NewWithT(t)
	client := oci.NewClient(oci.Insecure())
	base := testregistry.Repo(ociReg, "oci-mixed")
	chartRepo := base + "/podinfo"

	// One real chart and one regular OCI image co-located in the same repo.
	pushHelmFixture(t, client, chartRepo, "1.0.0")
	testregistry.PushImage(t, chartRepo+":notachart")

	src := NewOCISource(base, client)
	versions, err := src.ListVersions(context.Background(), "podinfo")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(versions).To(ConsistOf("1.0.0"), "non-chart tag must be filtered out")
}

func TestOCISource_ListVersions_BuildMetadata(t *testing.T) {
	g := NewWithT(t)
	client := oci.NewClient(oci.Insecure())
	base := testregistry.Repo(ociReg, "oci-build")
	chartRepo := base + "/podinfo"

	// Helm OCI replaces '+' with '_' in tags. The source must invert that
	// substitution when reporting versions.
	pushHelmFixture(t, client, chartRepo, "1.0.0+meta")

	src := NewOCISource(base, client)
	versions, err := src.ListVersions(context.Background(), "podinfo")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(versions).To(ConsistOf("1.0.0+meta"))
}

func TestOCISource_Download(t *testing.T) {
	g := NewWithT(t)
	client := oci.NewClient(oci.Insecure())
	base := testregistry.Repo(ociReg, "oci-dl")
	chartRepo := base + "/podinfo"

	pushHelmFixture(t, client, chartRepo, "1.2.3")

	src := NewOCISource(base, client)
	tgz, err := src.Download(context.Background(), "podinfo", "1.2.3")
	g.Expect(err).ToNot(HaveOccurred())

	chrt, err := loader.LoadArchive(bytes.NewReader(tgz))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(chrt.Metadata.Name).To(Equal("podinfo"))
	g.Expect(chrt.Metadata.Version).To(Equal("1.2.3"))
}

func TestOCISource_Download_BuildMetadata(t *testing.T) {
	g := NewWithT(t)
	client := oci.NewClient(oci.Insecure())
	base := testregistry.Repo(ociReg, "oci-dl-build")
	chartRepo := base + "/podinfo"

	pushHelmFixture(t, client, chartRepo, "1.0.0+meta")

	// Caller passes the natural semver, OCISource translates it to the
	// underscore-tagged ref before pulling.
	src := NewOCISource(base, client)
	tgz, err := src.Download(context.Background(), "podinfo", "1.0.0+meta")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(tgz).ToNot(BeEmpty())
}
