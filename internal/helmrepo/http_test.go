// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package helmrepo

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/fluxcd/pkg/helmtestserver"
	. "github.com/onsi/gomega"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	repo "helm.sh/helm/v4/pkg/repo/v1"
)

const fixtureChart = "testdata/podinfo"

func newHelmServer(t *testing.T, versions ...string) *helmtestserver.HelmServer {
	t.Helper()
	return newHelmServerWithMiddleware(t, nil, versions...)
}

func newHelmServerWithMiddleware(t *testing.T, middleware func(http.Handler) http.Handler, versions ...string) *helmtestserver.HelmServer {
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
	if middleware != nil {
		srv.WithMiddleware(middleware)
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

func TestHTTPSource_AuthenticatedIndexAndDownload(t *testing.T) {
	g := NewWithT(t)
	srv := newHelmServerWithMiddleware(t, requireBasicAuth("user", "pass"), "0.1.0", "1.2.3")

	src, err := NewHTTPSourceWithEntry(srv.URL(), &repo.Entry{
		URL:      srv.URL(),
		Username: "user",
		Password: "pass",
	})
	g.Expect(err).ToNot(HaveOccurred())

	versions, err := src.ListVersions(context.Background(), "podinfo")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(versions).To(ConsistOf("0.1.0", "1.2.3"))

	tgz, err := src.Download(context.Background(), "podinfo", "1.2.3")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(tgz).ToNot(BeEmpty())
}

func TestHTTPSource_DownloadPassCredentialsAll(t *testing.T) {
	g := NewWithT(t)
	indexSrv := newHelmServerWithMiddleware(t, requireBasicAuth("user", "pass"), "0.1.0")
	var chartAuth []string
	chartSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chartAuth = append(chartAuth, r.Header.Get("Authorization"))
		http.FileServer(http.Dir(indexSrv.Root())).ServeHTTP(w, r)
	}))
	t.Cleanup(chartSrv.Close)

	idx, err := repo.LoadIndexFile(filepath.Join(indexSrv.Root(), "index.yaml"))
	g.Expect(err).ToNot(HaveOccurred())
	idx.Entries["podinfo"][0].URLs = []string{chartSrv.URL + "/podinfo-0.1.0.tgz"}
	g.Expect(idx.WriteFile(filepath.Join(indexSrv.Root(), "index.yaml"), 0o644)).To(Succeed())

	src, err := NewHTTPSourceWithEntry(indexSrv.URL(), &repo.Entry{
		URL:      indexSrv.URL(),
		Username: "user",
		Password: "pass",
	})
	g.Expect(err).ToNot(HaveOccurred())
	_, err = src.Download(context.Background(), "podinfo", "0.1.0")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(chartAuth).To(ConsistOf(""))

	chartAuth = nil
	src, err = NewHTTPSourceWithEntry(indexSrv.URL(), &repo.Entry{
		URL:                indexSrv.URL(),
		Username:           "user",
		Password:           "pass",
		PassCredentialsAll: true,
	})
	g.Expect(err).ToNot(HaveOccurred())
	_, err = src.Download(context.Background(), "podinfo", "0.1.0")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(chartAuth).To(ConsistOf("Basic dXNlcjpwYXNz"))
}

func TestNew_LoadsHelmRepositoryConfig(t *testing.T) {
	g := NewWithT(t)
	srv := newHelmServerWithMiddleware(t, requireBasicAuth("user", "pass"), "0.1.0")
	reposPath := writeRepositoriesFile(t, &repo.Entry{
		Name:     "private",
		URL:      srv.URL() + "/",
		Username: "user",
		Password: "pass",
	})
	t.Setenv("HELM_REPOSITORY_CONFIG", reposPath)

	src, err := New(srv.URL())
	g.Expect(err).ToNot(HaveOccurred())
	versions, err := src.ListVersions(context.Background(), "podinfo")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(versions).To(ConsistOf("0.1.0"))
}

func TestNew_MissingHelmRepositoryConfigFallsBackAnonymous(t *testing.T) {
	g := NewWithT(t)
	srv := newHelmServer(t, "0.1.0")
	t.Setenv("HELM_REPOSITORY_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))

	src, err := New(srv.URL())
	g.Expect(err).ToNot(HaveOccurred())
	versions, err := src.ListVersions(context.Background(), "podinfo")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(versions).To(ConsistOf("0.1.0"))
}

func requireBasicAuth(username, password string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotUser, gotPass, ok := r.BasicAuth()
			if !ok || gotUser != username || gotPass != password {
				w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeRepositoriesFile(t *testing.T, entries ...*repo.Entry) string {
	t.Helper()
	repos := repo.NewFile()
	repos.Add(entries...)
	path := filepath.Join(t.TempDir(), "repositories.yaml")
	if err := repos.WriteFile(path, 0o600); err != nil {
		t.Fatalf("write repositories file: %s", err)
	}
	return path
}
