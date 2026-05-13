// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/fluxcd/flux-mirror/internal/testregistry"
)

// TestSnapshotAndCopyReferrers exercises the OCI 1.1 Referrers API end-to-end:
// pushes a subject + two cosign-shaped referrers via the testregistry helper,
// snapshots them, copies each to a destination repo, then asserts both are
// indexed at the destination *and* their `subject` field is preserved —
// without that, mirroring referrers would silently orphan them.
func TestSnapshotAndCopyReferrers(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	c := NewClient(Insecure())

	srcRepoStr := repo("ref-src")
	dstRepoStr := repo("ref-dst")

	subjectRef := srcRepoStr + ":v1"
	testregistry.PushImage(t, subjectRef)
	subjectDigStr, err := crane.Digest(subjectRef, crane.Insecure)
	g.Expect(err).ToNot(HaveOccurred())
	subjectHash, err := v1.NewHash(subjectDigStr)
	g.Expect(err).ToNot(HaveOccurred())
	subject := v1.Descriptor{
		MediaType: types.OCIManifestSchema1,
		Digest:    subjectHash,
	}
	testregistry.PushReferrer(t, srcRepoStr, subject, "application/vnd.dev.cosign.artifact.sig.v1+json")
	testregistry.PushReferrer(t, srcRepoStr, subject, "application/spdx+json")

	refs, err := c.SnapshotReferrers(ctx, srcRepoStr, subjectHash.String())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(refs).To(HaveLen(2))
	for _, r := range refs {
		g.Expect(r.Digest).To(HavePrefix("sha256:"))
	}

	dstSubject := dstRepoStr + ":v1"
	g.Expect(c.CopyTag(ctx, subjectRef, dstSubject, 1)).To(Succeed())
	for _, r := range refs {
		g.Expect(c.CopyReferrer(ctx, srcRepoStr, dstRepoStr, r.Digest)).To(Succeed())
	}

	dstRepoName, err := name.NewRepository(dstRepoStr, name.Insecure)
	g.Expect(err).ToNot(HaveOccurred())
	dstSubjectDigest := dstRepoName.Digest(subjectHash.String())
	dstIdx, err := remote.Referrers(dstSubjectDigest)
	g.Expect(err).ToNot(HaveOccurred())
	dstIdxManifest, err := dstIdx.IndexManifest()
	g.Expect(err).ToNot(HaveOccurred())

	srcDigests := map[string]bool{}
	for _, r := range refs {
		srcDigests[r.Digest] = true
	}
	g.Expect(dstIdxManifest.Manifests).To(HaveLen(2))
	for _, desc := range dstIdxManifest.Manifests {
		g.Expect(srcDigests).To(HaveKey(desc.Digest.String()), "referrer digest preserved")

		dstRefRef := dstRepoName.Digest(desc.Digest.String())
		img, err := remote.Image(dstRefRef)
		g.Expect(err).ToNot(HaveOccurred())
		manifest, err := img.Manifest()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(manifest.Subject).ToNot(BeNil())
		g.Expect(manifest.Subject.Digest.String()).To(Equal(subjectHash.String()))
	}
}
