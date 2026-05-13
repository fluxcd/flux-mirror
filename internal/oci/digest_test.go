// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/fluxcd/flux-mirror/internal/testregistry"
)

func TestCompare_Missing(t *testing.T) {
	g := NewWithT(t)
	c := NewClient(Insecure())

	src := repo("cmp-missing-src") + ":v1"
	testregistry.PushImage(t, src)
	dst := repo("cmp-missing-dst") + ":v1" // never pushed

	res, err := c.Compare(context.Background(), src, dst)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.State).To(Equal(StateMissing))
	g.Expect(res.SrcDigest).ToNot(BeEmpty())
	g.Expect(res.DstDigest).To(BeEmpty())
}

func TestCompare_Equal(t *testing.T) {
	g := NewWithT(t)
	c := NewClient(Insecure())

	src := repo("cmp-equal-src") + ":v1"
	dst := repo("cmp-equal-dst") + ":v1"
	testregistry.PushImage(t, src)
	g.Expect(c.CopyTag(context.Background(), src, dst, 1)).To(Succeed())

	res, err := c.Compare(context.Background(), src, dst)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.State).To(Equal(StateEqual))
	g.Expect(res.SrcDigest).To(Equal(res.DstDigest))
}

func TestCompare_Drifted(t *testing.T) {
	g := NewWithT(t)
	c := NewClient(Insecure())

	src := repo("cmp-drift-src") + ":v1"
	dst := repo("cmp-drift-dst") + ":v1"
	testregistry.PushImage(t, src)
	testregistry.PushImage(t, dst) // independent push → different digest

	res, err := c.Compare(context.Background(), src, dst)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.State).To(Equal(StateDrifted))
	g.Expect(res.SrcDigest).ToNot(BeEmpty())
	g.Expect(res.DstDigest).ToNot(BeEmpty())
	g.Expect(res.SrcDigest).ToNot(Equal(res.DstDigest))
}

// TestCompare_AuthErrorIsNotMissing verifies that a 401 from the destination
// surfaces as an error rather than being silently mapped to StateMissing,
// which would cause the runner to attempt a doomed push and report a
// confusing "copy failed" instead of an auth failure.
func TestCompare_AuthErrorIsNotMissing(t *testing.T) {
	g := NewWithT(t)
	c := NewClient(Insecure())

	// Push a real source image so the src-side Digest call succeeds.
	src := repo("auth-src") + ":v1"
	testregistry.PushImage(t, src)

	// Reverse-proxy the destination registry, returning 401 on every
	// manifest HEAD/GET. The src-side calls go straight to dockerReg.
	upstream, err := url.Parse("http://" + dockerReg)
	g.Expect(err).ToNot(HaveOccurred())
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/manifests/") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="example"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Pass other requests through (e.g. /v2/ for ping).
		r.Host = upstream.Host
		r.URL.Scheme = upstream.Scheme
		r.URL.Host = upstream.Host
		http.DefaultClient.Do(r) //nolint:errcheck // best-effort proxy for /v2/ ping
	}))
	defer proxy.Close()

	proxyURL, err := url.Parse(proxy.URL)
	g.Expect(err).ToNot(HaveOccurred())
	dst := proxyURL.Host + "/auth-dst:v1"

	res, err := c.Compare(context.Background(), src, dst)
	g.Expect(err).To(HaveOccurred(), "401 must not be mapped to StateMissing")
	g.Expect(res.State).To(Equal(StateMissing)) // zero value; the assert is on err
	g.Expect(err.Error()).To(ContainSubstring("401"))
}

func TestListTags(t *testing.T) {
	g := NewWithT(t)
	c := NewClient(Insecure())

	r := repo("list-tags")
	testregistry.PushImage(t, r+":v1")
	testregistry.PushImage(t, r+":v2")
	testregistry.PushImage(t, r+":v3")

	tags, err := c.ListTags(context.Background(), r)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(tags).To(ConsistOf("v1", "v2", "v3"))
}
