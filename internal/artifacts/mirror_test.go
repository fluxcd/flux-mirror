// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/fluxcd/flux-mirror/internal/config"
	"github.com/fluxcd/flux-mirror/internal/oci"
	"github.com/fluxcd/flux-mirror/internal/sync"
	"github.com/fluxcd/flux-mirror/internal/testregistry"
)

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

func repo(stem string) string {
	return testregistry.Repo(dockerReg, stem)
}

func newRunner() *sync.Runner {
	return &sync.Runner{
		Concurrency:   2,
		Retries:       0,
		PerJobTimeout: 5 * time.Second,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

type recordingVerifier struct {
	refs []string
	err  error
}

func (v *recordingVerifier) Verify(_ context.Context, ref string, _ config.ArtifactVerification) error {
	v.refs = append(v.refs, ref)
	return v.err
}

func TestMirror_CopiesSelectedTags(t *testing.T) {
	g := NewWithT(t)
	src := repo("a-src")
	dst := repo("a-dst")

	testregistry.PushImage(t, src+":1.0.0")
	testregistry.PushImage(t, src+":1.1.0")
	testregistry.PushImage(t, src+":1.2.0")

	c := oci.NewClient(oci.Insecure())
	entry := config.ArtifactEntry{
		Source:      src,
		Destination: dst,
		Selector:    config.Selector{Limit: new(2)},
	}
	res, err := newRunner().Run(context.Background(), []sync.EntryMirror{
		New(c, entry, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Entries[0].Outcomes[sync.OutcomeCopied]).To(HaveLen(2))
	g.Expect(res.HasFailures()).To(BeFalse())

	tags, err := c.ListTags(context.Background(), dst)
	g.Expect(err).ToNot(HaveOccurred())
	// Top-2 of {1.0.0, 1.1.0, 1.2.0} by semver = 1.1.0 and 1.2.0.
	g.Expect(tags).To(ConsistOf("1.1.0", "1.2.0"))
}

func TestMirror_VerifiesSelectedTags(t *testing.T) {
	g := NewWithT(t)
	src := repo("verify-src")
	dst := repo("verify-dst")

	testregistry.PushImage(t, src+":1.0.0")
	testregistry.PushImage(t, src+":1.1.0")

	c := oci.NewClient(oci.Insecure())
	entry := config.ArtifactEntry{
		Source:      src,
		Destination: dst,
		Selector:    config.Selector{Limit: new(1)},
		Verify: &config.ArtifactVerification{
			Provider: config.VerifyProviderCosign,
			MatchOIDCIdentity: []config.OIDCIdentity{{
				Issuer:  "https://token.actions.githubusercontent.com",
				Subject: `^https://github\.com/example/.*$`,
			}},
		},
	}
	verifier := &recordingVerifier{}
	plan, err := New(c, entry, Options{
		Verifier: verifier,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Plan(context.Background())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(plan.Jobs).To(HaveLen(1))
	g.Expect(verifier.refs).To(Equal([]string{src + ":1.1.0"}))
}

func TestMirror_VerificationFailureStopsPlanning(t *testing.T) {
	g := NewWithT(t)
	src := repo("verify-fail-src")
	dst := repo("verify-fail-dst")

	testregistry.PushImage(t, src+":1.0.0")

	c := oci.NewClient(oci.Insecure())
	entry := config.ArtifactEntry{
		Source:      src,
		Destination: dst,
		Selector:    config.Selector{Limit: new(1)},
		Verify: &config.ArtifactVerification{
			Provider: config.VerifyProviderCosign,
			MatchOIDCIdentity: []config.OIDCIdentity{{
				Issuer:  "https://token.actions.githubusercontent.com",
				Subject: `^https://github\.com/example/.*$`,
			}},
		},
	}
	verifier := &recordingVerifier{err: errors.New("denied")}
	plan, err := New(c, entry, Options{
		Verifier: verifier,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Plan(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("verify " + src + ":1.0.0"))
	g.Expect(err.Error()).To(ContainSubstring("denied"))
	g.Expect(plan.Jobs).To(BeEmpty())
}

func TestMirror_SkipsEqual(t *testing.T) {
	g := NewWithT(t)
	src := repo("eq-src")
	dst := repo("eq-dst")
	testregistry.PushImage(t, src+":1.0.0")

	c := oci.NewClient(oci.Insecure())
	entry := config.ArtifactEntry{
		Source: src, Destination: dst,
		Selector: config.Selector{Limit: new(1)},
	}
	mirror := New(c, entry, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	runner := newRunner()

	_, err := runner.Run(context.Background(), []sync.EntryMirror{mirror})
	g.Expect(err).ToNot(HaveOccurred())
	res2, err := runner.Run(context.Background(), []sync.EntryMirror{mirror})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res2.Entries[0].Outcomes[sync.OutcomeSkipped]).To(HaveLen(1))
	g.Expect(res2.Entries[0].Outcomes[sync.OutcomeCopied]).To(BeEmpty())
}

func TestMirror_DriftWithoutOverwrite(t *testing.T) {
	g := NewWithT(t)
	src := repo("drift-src")
	dst := repo("drift-dst")
	testregistry.PushImage(t, src+":1.0.0")
	testregistry.PushImage(t, dst+":1.0.0") // independent push → different digest

	c := oci.NewClient(oci.Insecure())
	entry := config.ArtifactEntry{
		Source: src, Destination: dst,
		Selector: config.Selector{Limit: new(1)},
	}
	res, err := newRunner().Run(context.Background(), []sync.EntryMirror{
		New(c, entry, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Entries[0].Outcomes[sync.OutcomeDrifted]).To(Equal([]string{"1.0.0"}))
	g.Expect(res.HasDrift()).To(BeTrue())
	g.Expect(res.HasFailures()).To(BeFalse())
	g.Expect(res.ExitCode()).To(Equal(2))
}

func TestMirror_DriftWithOverwrite(t *testing.T) {
	g := NewWithT(t)
	src := repo("ow-src")
	dst := repo("ow-dst")
	srcDigest := testregistry.PushImage(t, src+":1.0.0")
	testregistry.PushImage(t, dst+":1.0.0")

	c := oci.NewClient(oci.Insecure())
	entry := config.ArtifactEntry{
		Source: src, Destination: dst,
		Selector: config.Selector{Limit: new(1)},
	}
	res, err := newRunner().Run(context.Background(), []sync.EntryMirror{
		New(c, entry, Options{Overwrite: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Entries[0].Outcomes[sync.OutcomeOverwritten]).To(HaveLen(1))

	dig, err := crane.Digest(dst+":1.0.0", crane.Insecure)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(dig).To(Equal(srcDigest))
}

func TestMirror_IncludeReferrers(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	src := repo("ref-src")
	dst := repo("ref-dst")

	subjectRef := src + ":1.0.0"
	testregistry.PushImage(t, subjectRef)
	subjectDigStr, err := crane.Digest(subjectRef, crane.Insecure)
	g.Expect(err).ToNot(HaveOccurred())
	subjectHash, err := v1.NewHash(subjectDigStr)
	g.Expect(err).ToNot(HaveOccurred())
	subject := v1.Descriptor{
		MediaType: types.OCIManifestSchema1,
		Digest:    subjectHash,
	}
	testregistry.PushReferrer(t, src, subject, "application/vnd.dev.cosign.artifact.sig.v1+json")
	testregistry.PushReferrer(t, src, subject, "application/spdx+json")

	c := oci.NewClient(oci.Insecure())
	entry := config.ArtifactEntry{
		Source: src, Destination: dst,
		Selector:         config.Selector{Limit: new(1)},
		IncludeReferrers: true,
	}
	res, err := newRunner().Run(ctx, []sync.EntryMirror{
		New(c, entry, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.HasFailures()).To(BeFalse())
	g.Expect(res.Entries[0].Outcomes[sync.OutcomeCopied]).To(HaveLen(1))

	dstRepo, err := name.NewRepository(dst, name.Insecure)
	g.Expect(err).ToNot(HaveOccurred())
	dstSubject := dstRepo.Digest(subjectHash.String())
	idx, err := remote.Referrers(dstSubject)
	g.Expect(err).ToNot(HaveOccurred())
	man, err := idx.IndexManifest()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(man.Manifests).To(HaveLen(2))
	for _, desc := range man.Manifests {
		img, err := remote.Image(dstRepo.Digest(desc.Digest.String()))
		g.Expect(err).ToNot(HaveOccurred())
		m, err := img.Manifest()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(m.Subject).ToNot(BeNil())
		g.Expect(m.Subject.Digest.String()).To(Equal(subjectHash.String()))
	}

	// Idempotent re-run: subject + both referrers report skipped.
	res2, err := newRunner().Run(ctx, []sync.EntryMirror{
		New(c, entry, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res2.Entries[0].Outcomes[sync.OutcomeSkipped]).To(HaveLen(1))
	g.Expect(res2.Entries[0].Outcomes[sync.OutcomeCopied]).To(BeEmpty())
}

func TestMirror_DryRun(t *testing.T) {
	g := NewWithT(t)
	src := repo("dry-src")
	dst := repo("dry-dst")
	testregistry.PushImage(t, src+":1.0.0")

	c := oci.NewClient(oci.Insecure())
	entry := config.ArtifactEntry{
		Source: src, Destination: dst,
		Selector: config.Selector{Limit: new(1)},
	}
	res, err := newRunner().Run(context.Background(), []sync.EntryMirror{
		New(c, entry, Options{DryRun: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Entries[0].Outcomes[sync.OutcomeWouldCopy]).To(HaveLen(1))
	g.Expect(res.Entries[0].Outcomes[sync.OutcomeCopied]).To(BeEmpty())

	_, err = c.ListTags(context.Background(), dst)
	g.Expect(err).To(HaveOccurred()) // 404 — repo was never created
}
