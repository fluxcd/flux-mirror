// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta2"
	"github.com/fluxcd/flux-mirror/internal/oci"
	"github.com/fluxcd/flux-mirror/internal/sync"
	"github.com/fluxcd/flux-mirror/internal/testregistry"
)

// refusingProxy forwards to the shared in-process test registry. While refuse
// is set, it answers 403 to a PUT of a referrers fallback tag
// (`sha256-<digest>`) and counts the refusal. The test registry does not serve
// the OCI referrers API, so the client maintains that tag itself.
type refusingProxy struct {
	host    string
	refuse  atomic.Bool
	refused atomic.Int32
}

var fallbackTagPut = regexp.MustCompile(`^/v2/.+/manifests/sha256-[0-9a-f]{64}$`)

func newRefusingProxy(t *testing.T) *refusingProxy {
	t.Helper()
	p := &refusingProxy{}
	upstream, err := url.Parse("http://" + dockerReg)
	if err != nil {
		t.Fatalf("parse registry address: %s", err)
	}
	forward := httputil.NewSingleHostReverseProxy(upstream)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && fallbackTagPut.MatchString(r.URL.Path) && p.refuse.Load() {
			p.refused.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"errors":[{"code":"DENIED","message":"refused by test"}]}`)
			return
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	p.host = strings.TrimPrefix(srv.URL, "http://")
	return p
}

// pushSubjectWithReferrer pushes tag 1.0.0 and one referrer of it to src and
// returns the subject digest and the referrer digest.
func pushSubjectWithReferrer(t *testing.T, g Gomega, src string) (v1.Hash, v1.Hash) {
	t.Helper()
	subjectRef := src + ":1.0.0"
	testregistry.PushImage(t, subjectRef)
	subjectDig, err := crane.Digest(subjectRef, crane.Insecure)
	g.Expect(err).ToNot(HaveOccurred())
	subjectHash, err := v1.NewHash(subjectDig)
	g.Expect(err).ToNot(HaveOccurred())
	referrer := testregistry.PushReferrer(t, src,
		v1.Descriptor{MediaType: types.OCIManifestSchema1, Digest: subjectHash},
		"application/vnd.dev.cosign.artifact.sig.v1+json")
	return subjectHash, referrer
}

// listedReferrers returns the digests the registry lists as referrers of
// subject. It reads the registry directly, never through the proxy.
func listedReferrers(g Gomega, repoAddr string, subject v1.Hash) []string {
	r, err := name.NewRepository(repoAddr, name.Insecure)
	g.Expect(err).ToNot(HaveOccurred())
	idx, err := remote.Referrers(r.Digest(subject.String()))
	g.Expect(err).ToNot(HaveOccurred())
	man, err := idx.IndexManifest()
	g.Expect(err).ToNot(HaveOccurred())
	out := make([]string, 0, len(man.Manifests))
	for _, d := range man.Manifests {
		out = append(out, d.Digest.String())
	}
	return out
}

// fallbackTags returns the tags of repoAddr that are referrers fallback tags.
func fallbackTags(g Gomega, repoAddr string) []string {
	tags, err := crane.ListTags(repoAddr, crane.Insecure)
	g.Expect(err).ToNot(HaveOccurred())
	var out []string
	for _, t := range tags {
		if strings.HasPrefix(t, "sha256-") {
			out = append(out, t)
		}
	}
	return out
}

func runReferrerSync(ctx context.Context, src, dst string, retries int) (sync.Result, error) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner := &sync.Runner{
		Concurrency:   2,
		Retries:       retries,
		PerJobTimeout: 10 * time.Second,
		Logger:        quiet,
	}
	entry := apiv1.ArtifactEntry{
		Source: src, Destination: dst,
		Selector:         apiv1.Selector{Limit: new(1)},
		IncludeReferrers: true,
	}
	return runner.Run(ctx, []sync.EntryMirror{
		New(oci.NewClient(oci.Insecure()), entry, Options{Logger: quiet}),
	})
}

// Control: through the proxy with nothing refused, the destination gets the
// fallback tag and lists the referrer. It also pins the premise of the two
// tests below: the test registry uses the fallback tag, not the referrers API.
func TestMirror_ReferrerFallback_Control(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	src := repo("fb-ctl-src")
	dstRepo := testregistry.Repo("", "fb-ctl-dst") // "/fb-ctl-dst-<suffix>"
	proxy := newRefusingProxy(t)
	subject, referrer := pushSubjectWithReferrer(t, g, src)
	g.Expect(fallbackTags(g, src)).To(HaveLen(1), "the test registry is expected to need the fallback tag")

	res, err := runReferrerSync(ctx, src, proxy.host+dstRepo, 0)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.HasFailures()).To(BeFalse())
	g.Expect(proxy.refused.Load()).To(BeZero())
	g.Expect(fallbackTags(g, dockerReg+dstRepo)).To(HaveLen(1))
	g.Expect(listedReferrers(g, dockerReg+dstRepo, subject)).To(ConsistOf(referrer.String()))
}

// A fallback-tag write that fails once leaves the referrer manifest stored and
// unlisted. The next sync must repair the list, not report up-to-date.
func TestMirror_ReferrerFallback_RepairedByNextSync(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	src := repo("fb-fix-src")
	dstRepo := testregistry.Repo("", "fb-fix-dst")
	proxy := newRefusingProxy(t)
	subject, referrer := pushSubjectWithReferrer(t, g, src)

	proxy.refuse.Store(true)
	res, _ := runReferrerSync(ctx, src, proxy.host+dstRepo, 0)
	g.Expect(res.HasFailures()).To(BeTrue(), "the refused fallback-tag write must fail the first sync")
	g.Expect(proxy.refused.Load()).To(Equal(int32(1)))

	// The state the failure leaves: manifest stored by digest, not listed.
	_, err := crane.Digest(dockerReg+dstRepo+"@"+referrer.String(), crane.Insecure)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(listedReferrers(g, dockerReg+dstRepo, subject)).To(BeEmpty())

	// Second sync, straight to the registry, nothing refused.
	res, err = runReferrerSync(ctx, src, dockerReg+dstRepo, 0)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.HasFailures()).To(BeFalse())
	g.Expect(listedReferrers(g, dockerReg+dstRepo, subject)).To(ConsistOf(referrer.String()),
		"a sync that reports no failure must leave every source referrer listed at the destination")
}

// With retries, a refusal that lasts for the whole sync must end in a failure,
// not in a skipped/up-to-date row produced by the second attempt.
func TestMirror_ReferrerFallback_RetryDoesNotHideFailure(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	src := repo("fb-retry-src")
	dstRepo := testregistry.Repo("", "fb-retry-dst")
	proxy := newRefusingProxy(t)
	subject, _ := pushSubjectWithReferrer(t, g, src)

	proxy.refuse.Store(true)
	res, _ := runReferrerSync(ctx, src, proxy.host+dstRepo, 3)
	g.Expect(proxy.refused.Load()).To(BeNumerically(">=", int32(1)))
	g.Expect(listedReferrers(g, dockerReg+dstRepo, subject)).To(BeEmpty())
	g.Expect(res.HasFailures()).To(BeTrue(),
		"the destination lists no referrer, so the sync must not report success")
}
