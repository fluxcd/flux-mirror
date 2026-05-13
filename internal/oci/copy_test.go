// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
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
