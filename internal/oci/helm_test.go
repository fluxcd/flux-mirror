// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/fluxcd/flux-mirror/internal/testregistry"
)

const helmFixture = "../helmrepo/testdata/podinfo"

func helmChartBytes(t *testing.T, version string) (cfg, tgz []byte) {
	t.Helper()
	tgz = testregistry.PackageChart(t, helmFixture, version)
	cfg = testregistry.ChartConfigJSON(t, tgz)
	return cfg, tgz
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestPushHelmChart_RoundTrip(t *testing.T) {
	g := NewWithT(t)
	c := NewClient(Insecure())
	dst := repo("helm-rt") + ":1.2.3"

	cfg, tgz := helmChartBytes(t, "1.2.3")
	manifestDigest, err := c.PushHelmChart(context.Background(), dst, cfg, tgz)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(manifestDigest).To(HavePrefix("sha256:"))

	// Pull back and confirm bytes match.
	pulled, gotManifestDigest, err := c.PullHelmChart(context.Background(), dst)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(pulled).To(Equal(tgz))
	g.Expect(gotManifestDigest).To(Equal(manifestDigest))
}

func TestPushHelmChart_DeterministicManifest(t *testing.T) {
	g := NewWithT(t)
	c := NewClient(Insecure())
	dstA := repo("helm-det-a") + ":1.0.0"
	dstB := repo("helm-det-b") + ":1.0.0"

	cfg, tgz := helmChartBytes(t, "1.0.0")

	digA, err := c.PushHelmChart(context.Background(), dstA, cfg, tgz)
	g.Expect(err).ToNot(HaveOccurred())
	digB, err := c.PushHelmChart(context.Background(), dstB, cfg, tgz)
	g.Expect(err).ToNot(HaveOccurred())

	// Same content → same manifest digest. This is the property that lets
	// the chart mirror's drift detection work via HelmChartLayerDigest
	// instead of having to dance around timestamp annotations.
	g.Expect(digA).To(Equal(digB))
}

func TestHelmChartLayerDigest(t *testing.T) {
	g := NewWithT(t)
	c := NewClient(Insecure())
	dst := repo("helm-layer") + ":1.0.0"

	cfg, tgz := helmChartBytes(t, "1.0.0")
	_, err := c.PushHelmChart(context.Background(), dst, cfg, tgz)
	g.Expect(err).ToNot(HaveOccurred())

	got, err := c.HelmChartLayerDigest(context.Background(), dst)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(got).To(Equal(sha256Hex(tgz)))
}

func TestHelmChartLayerDigest_Missing(t *testing.T) {
	g := NewWithT(t)
	c := NewClient(Insecure())
	missing := repo("helm-missing") + ":1.0.0"

	got, err := c.HelmChartLayerDigest(context.Background(), missing)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(got).To(BeEmpty())
}

func TestIsHelmChart(t *testing.T) {
	g := NewWithT(t)
	c := NewClient(Insecure())
	chartRef := repo("ishelm-yes") + ":1.0.0"
	imageRef := repo("ishelm-no") + ":v1"

	cfg, tgz := helmChartBytes(t, "1.0.0")
	_, err := c.PushHelmChart(context.Background(), chartRef, cfg, tgz)
	g.Expect(err).ToNot(HaveOccurred())
	testregistry.PushImage(t, imageRef)

	yes, err := c.IsHelmChart(context.Background(), chartRef)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(yes).To(BeTrue(), "chart manifest should be detected as Helm")

	no, err := c.IsHelmChart(context.Background(), imageRef)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(no).To(BeFalse(), "regular image should not be detected as Helm")
}

func TestIsHelmChart_MissingTag(t *testing.T) {
	g := NewWithT(t)
	c := NewClient(Insecure())
	missing := repo("ishelm-missing") + ":nope"

	yes, err := c.IsHelmChart(context.Background(), missing)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(yes).To(BeFalse())
}
