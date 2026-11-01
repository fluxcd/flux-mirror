// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"net/http"
	"sync"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/google/go-containerregistry/pkg/crane"

	"github.com/fluxcd/flux-mirror/internal/testregistry"
)

func TestCopyTag_SingleArch(t *testing.T) {
	g := NewWithT(t)
	c := NewClient(Insecure())
	src := repo("copy-src") + ":v1"
	dst := repo("copy-dst") + ":v1"

	srcDigest := testregistry.PushImage(t, src)
	g.Expect(c.CopyTag(context.Background(), src, dst, 1)).To(Succeed())

	dstDigest, err := crane.Digest(dst, crane.Insecure)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(dstDigest).To(Equal(srcDigest), "digest must be preserved across copy")
}

func TestCopyTag_MultiArchManifestList(t *testing.T) {
	g := NewWithT(t)
	c := NewClient(Insecure())
	src := repo("copy-src-mi") + ":multi"
	dst := repo("copy-dst-mi") + ":multi"

	srcDigest := testregistry.PushIndex(t, src)
	g.Expect(c.CopyTag(context.Background(), src, dst, 1)).To(Succeed())

	dstDigest, err := crane.Digest(dst, crane.Insecure)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(dstDigest).To(Equal(srcDigest), "manifest list digest must be preserved")
}

func TestCopyTag_Idempotent(t *testing.T) {
	g := NewWithT(t)
	c := NewClient(Insecure())
	src := repo("idem-src") + ":v1"
	dst := repo("idem-dst") + ":v1"

	srcDigest := testregistry.PushImage(t, src)
	g.Expect(c.CopyTag(context.Background(), src, dst, 1)).To(Succeed())
	// Second copy must be a no-op (blobs already present, manifest digest matches).
	g.Expect(c.CopyTag(context.Background(), src, dst, 1)).To(Succeed())

	dstDigest, err := crane.Digest(dst, crane.Insecure)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(dstDigest).To(Equal(srcDigest))
}

type countingTransport struct {
	inner http.RoundTripper
	mu    sync.Mutex
	seen  map[string]int
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	if t.seen == nil {
		t.seen = map[string]int{}
	}
	t.seen[req.Method]++
	t.mu.Unlock()
	return t.inner.RoundTrip(req)
}

func (t *countingTransport) counts() map[string]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]int, len(t.seen))
	for k, v := range t.seen {
		out[k] = v
	}
	return out
}

func TestCopyTag_SeparateSourceAndDestinationTransports(t *testing.T) {
	g := NewWithT(t)
	srcRT := &countingTransport{inner: http.DefaultTransport}
	dstRT := &countingTransport{inner: http.DefaultTransport}
	c := NewClient(Insecure(), WithSourceTransport(srcRT), WithDestinationTransport(dstRT))
	src := repo("split-src") + ":v1"
	dst := repo("split-dst") + ":v1"

	testregistry.PushImage(t, src)
	g.Expect(c.CopyTag(context.Background(), src, dst, 1)).To(Succeed())

	srcCounts := srcRT.counts()
	dstCounts := dstRT.counts()
	g.Expect(srcCounts).To(HaveKey("GET"))
	g.Expect(srcCounts).ToNot(HaveKey("PUT"))
	g.Expect(srcCounts).ToNot(HaveKey("POST"))
	g.Expect(dstCounts).To(HaveKey("PUT"))
	g.Expect(dstCounts).To(HaveKey("POST"))
}
