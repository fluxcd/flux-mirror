// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package charts

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	"github.com/fluxcd/flux-mirror/internal/config"
	"github.com/fluxcd/flux-mirror/internal/oci"
	"github.com/fluxcd/flux-mirror/internal/sync"
	"github.com/fluxcd/flux-mirror/internal/testregistry"

	"github.com/fluxcd/pkg/helmtestserver"
)

const fixtureChart = "../helmrepo/testdata/podinfo"

var dockerReg string

func TestMain(m *testing.M) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, err := testregistry.Start(ctx)
	if err != nil {
		panic(fmt.Sprintf("start registry: %s", err))
	}
	dockerReg = addr
	os.Exit(m.Run())
}

func newRunner() *sync.Runner {
	return &sync.Runner{
		Concurrency:   2,
		Retries:       0,
		PerJobTimeout: 10 * time.Second,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newHTTPHelmRepo(t *testing.T, versions ...string) string {
	t.Helper()
	testregistry.UseEmptyDockerConfig(t)
	srv, err := helmtestserver.NewTempHelmServer()
	if err != nil {
		t.Fatalf("new helm server: %s", err)
	}
	t.Cleanup(srv.Stop)
	for _, v := range versions {
		if err := srv.PackageChartWithVersion(fixtureChart, v); err != nil {
			t.Fatalf("package %s: %s", v, err)
		}
	}
	if err := srv.GenerateIndex(); err != nil {
		t.Fatalf("generate index: %s", err)
	}
	srv.Start()
	return srv.URL()
}

// pushHelmFixture publishes a Helm chart to the OCI destination using the
// same deterministic-manifest path the production code uses, so tests that
// pre-populate the destination get byte-equivalent state.
func pushHelmFixture(t *testing.T, client *oci.Client, ref, version string) {
	t.Helper()
	testregistry.UseEmptyDockerConfig(t)
	tgz := testregistry.PackageChart(t, fixtureChart, version)
	cfg := testregistry.ChartConfigJSON(t, tgz)
	if _, err := client.PushHelmChart(context.Background(), ref, cfg, tgz); err != nil {
		t.Fatalf("push %s: %s", ref, err)
	}
}

func TestChartsMirror_CopiesTopN(t *testing.T) {
	g := NewWithT(t)
	srcURL := newHTTPHelmRepo(t, "0.1.0", "0.2.0", "1.0.0", "1.1.0")
	dst := testregistry.Repo(dockerReg, "charts-copyn")

	c := oci.NewClient(oci.Insecure())
	entry := config.ChartEntry{
		Source:      srcURL,
		Destination: "oci://" + dst,
		Name:        "podinfo",
		Limit:       new(2),
	}
	mirror, err := New(c, entry, Options{Logger: discardLogger()})
	g.Expect(err).ToNot(HaveOccurred())

	res, err := newRunner().Run(context.Background(), []sync.EntryMirror{mirror})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.HasFailures()).To(BeFalse())
	// Top-2 of {0.1.0, 0.2.0, 1.0.0, 1.1.0} by semver = 1.0.0 and 1.1.0.
	g.Expect(res.Entries[0].Outcomes[sync.OutcomeCopied]).To(ConsistOf("1.0.0", "1.1.0"))

	tags, err := c.ListTags(context.Background(), dst+"/podinfo")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(tags).To(ConsistOf("1.0.0", "1.1.0"))
}

func TestChartsMirror_VersionConstraint(t *testing.T) {
	g := NewWithT(t)
	srcURL := newHTTPHelmRepo(t, "0.1.0", "1.0.0", "1.1.0", "2.0.0")
	dst := testregistry.Repo(dockerReg, "charts-semver")

	c := oci.NewClient(oci.Insecure())
	entry := config.ChartEntry{
		Source:      srcURL,
		Destination: "oci://" + dst,
		Name:        "podinfo",
		Version:     ">=1.0.0 <2.0.0",
		Limit:       new(0), // unlimited
	}
	mirror, err := New(c, entry, Options{Logger: discardLogger()})
	g.Expect(err).ToNot(HaveOccurred())

	res, err := newRunner().Run(context.Background(), []sync.EntryMirror{mirror})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Entries[0].Outcomes[sync.OutcomeCopied]).To(ConsistOf("1.0.0", "1.1.0"))
}

func TestChartsMirror_SkipsExisting(t *testing.T) {
	g := NewWithT(t)
	srcURL := newHTTPHelmRepo(t, "1.0.0")
	dst := testregistry.Repo(dockerReg, "charts-skip")

	c := oci.NewClient(oci.Insecure())
	entry := config.ChartEntry{
		Source:      srcURL,
		Destination: "oci://" + dst,
		Name:        "podinfo",
		Limit:       new(1),
	}
	mirror, err := New(c, entry, Options{Logger: discardLogger()})
	g.Expect(err).ToNot(HaveOccurred())

	runner := newRunner()
	_, err = runner.Run(context.Background(), []sync.EntryMirror{mirror})
	g.Expect(err).ToNot(HaveOccurred())

	// Re-run: same source bytes → same chart-layer digest → skipped.
	res, err := runner.Run(context.Background(), []sync.EntryMirror{mirror})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Entries[0].Outcomes[sync.OutcomeSkipped]).To(Equal([]string{"1.0.0"}))
	g.Expect(res.Entries[0].Outcomes[sync.OutcomeCopied]).To(BeEmpty())
}

func TestChartsMirror_DriftWithoutOverwrite(t *testing.T) {
	g := NewWithT(t)
	srcURL := newHTTPHelmRepo(t, "1.0.0")
	dst := testregistry.Repo(dockerReg, "charts-drift")

	c := oci.NewClient(oci.Insecure())
	// Pre-populate the destination tag with a different chart version's
	// bytes — that's drift.
	pushHelmFixture(t, c, dst+"/podinfo:1.0.0", "9.9.9")

	entry := config.ChartEntry{
		Source:      srcURL,
		Destination: "oci://" + dst,
		Name:        "podinfo",
		Limit:       new(1),
	}
	mirror, err := New(c, entry, Options{Logger: discardLogger()})
	g.Expect(err).ToNot(HaveOccurred())

	res, err := newRunner().Run(context.Background(), []sync.EntryMirror{mirror})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Entries[0].Outcomes[sync.OutcomeDrifted]).To(Equal([]string{"1.0.0"}))
	g.Expect(res.HasDrift()).To(BeTrue())
	g.Expect(res.HasFailures()).To(BeFalse())
	g.Expect(res.ExitCode()).To(Equal(2))
}

func TestChartsMirror_DriftWithOverwrite(t *testing.T) {
	g := NewWithT(t)
	srcURL := newHTTPHelmRepo(t, "1.0.0")
	dst := testregistry.Repo(dockerReg, "charts-ow")

	c := oci.NewClient(oci.Insecure())
	pushHelmFixture(t, c, dst+"/podinfo:1.0.0", "9.9.9")

	entry := config.ChartEntry{
		Source:      srcURL,
		Destination: "oci://" + dst,
		Name:        "podinfo",
		Limit:       new(1),
	}
	mirror, err := New(c, entry, Options{Overwrite: true, Logger: discardLogger()})
	g.Expect(err).ToNot(HaveOccurred())

	res, err := newRunner().Run(context.Background(), []sync.EntryMirror{mirror})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Entries[0].Outcomes[sync.OutcomeOverwritten]).To(Equal([]string{"1.0.0"}))

	// Re-run with overwrite still on: dst now matches src → skipped.
	res2, err := newRunner().Run(context.Background(), []sync.EntryMirror{mirror})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res2.Entries[0].Outcomes[sync.OutcomeSkipped]).To(Equal([]string{"1.0.0"}))
}

func TestChartsMirror_DryRun(t *testing.T) {
	g := NewWithT(t)
	srcURL := newHTTPHelmRepo(t, "1.0.0", "2.0.0")
	dst := testregistry.Repo(dockerReg, "charts-dry")

	c := oci.NewClient(oci.Insecure())
	entry := config.ChartEntry{
		Source:      srcURL,
		Destination: "oci://" + dst,
		Name:        "podinfo",
		Limit:       new(0),
	}
	mirror, err := New(c, entry, Options{DryRun: true, Logger: discardLogger()})
	g.Expect(err).ToNot(HaveOccurred())

	res, err := newRunner().Run(context.Background(), []sync.EntryMirror{mirror})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Entries[0].Outcomes[sync.OutcomeWouldCopy]).To(ConsistOf("1.0.0", "2.0.0"))
	g.Expect(res.HasFailures()).To(BeFalse())

	// Destination must remain empty.
	_, err = c.ListTags(context.Background(), dst+"/podinfo")
	g.Expect(err).To(HaveOccurred(), "destination repo should not exist after dry-run")
}

func TestChartsMirror_OCISourceToOCIDest(t *testing.T) {
	g := NewWithT(t)
	c := oci.NewClient(oci.Insecure())

	srcBase := testregistry.Repo(dockerReg, "charts-o2o-src")
	dstBase := testregistry.Repo(dockerReg, "charts-o2o-dst")
	pushHelmFixture(t, c, srcBase+"/podinfo:1.0.0", "1.0.0")
	pushHelmFixture(t, c, srcBase+"/podinfo:1.1.0", "1.1.0")

	entry := config.ChartEntry{
		Source:      "oci://" + srcBase,
		Destination: "oci://" + dstBase,
		Name:        "podinfo",
		Limit:       new(0),
	}
	mirror, err := New(c, entry, Options{Logger: discardLogger()})
	g.Expect(err).ToNot(HaveOccurred())

	res, err := newRunner().Run(context.Background(), []sync.EntryMirror{mirror})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Entries[0].Outcomes[sync.OutcomeCopied]).To(ConsistOf("1.0.0", "1.1.0"))

	tags, err := c.ListTags(context.Background(), dstBase+"/podinfo")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(tags).To(ConsistOf("1.0.0", "1.1.0"))
}
