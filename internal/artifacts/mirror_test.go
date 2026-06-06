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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	info *oci.VerificationInfo
	err  error
}

func (v *recordingVerifier) Verify(_ context.Context, ref string, _ config.ArtifactVerification) (*oci.VerificationInfo, error) {
	v.refs = append(v.refs, ref)
	return v.info, v.err
}

// tagsWithStatus returns the entry's tag rows that landed in the given status,
// for order-independent assertions against the row-based report.
func tagsWithStatus(e sync.EntryResult, st sync.Status) []sync.TagResult {
	var out []sync.TagResult
	for _, t := range e.Tags {
		if t.Status == st {
			out = append(out, t)
		}
	}
	return out
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
	g.Expect(tagsWithStatus(res.Entries[0], sync.StatusCopied)).To(HaveLen(2))
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
	integrated := time.Date(2026, 5, 20, 9, 14, 2, 0, time.UTC)
	verifier := &recordingVerifier{info: &oci.VerificationInfo{
		Provider:       config.VerifyProviderCosign,
		Issuer:         "https://token.actions.githubusercontent.com",
		Identity:       "https://github.com/example/app/.github/workflows/release.yml@refs/tags/1.1.0",
		IntegratedTime: integrated,
	}}
	res, err := newRunner().Run(context.Background(), []sync.EntryMirror{
		New(c, entry, Options{
			Verifier: verifier,
			Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		}),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(verifier.refs).To(Equal([]string{src + ":1.1.0"}))

	copied := tagsWithStatus(res.Entries[0], sync.StatusCopied)
	g.Expect(copied).To(HaveLen(1))
	row := copied[0]
	g.Expect(row.Tag).To(Equal("1.1.0"))
	// The source digest the compare resolved is recorded on the row.
	g.Expect(row.Digest).To(HavePrefix("sha256:"))
	// The confirmed signature metadata is attached.
	g.Expect(row.Verification).ToNot(BeNil())
	g.Expect(row.Verification.Provider).To(Equal("cosign"))
	g.Expect(row.Verification.Issuer).To(Equal("https://token.actions.githubusercontent.com"))
	g.Expect(row.Verification.Identity).To(ContainSubstring("github.com/example/app"))
	g.Expect(row.Verification.IntegratedTime).To(Equal("2026-05-20T09:14:02Z"))
	// Age/minAge only appear on the too-new skip, not a normal verified copy.
	g.Expect(row.Verification.Age).To(BeEmpty())
	g.Expect(row.Verification.MinAge).To(BeEmpty())
}

func TestMirror_VerificationFailureStopsPlanning(t *testing.T) {
	g := NewWithT(t)
	src := repo("verify-fail-src")
	dst := repo("verify-fail-dst")

	// Two tags; the selector orders highest-semver first, so 1.1.0 is verified
	// first. A verify failure there must stop planning before 1.0.0.
	testregistry.PushImage(t, src+":1.0.0")
	testregistry.PushImage(t, src+":1.1.0")

	c := oci.NewClient(oci.Insecure())
	entry := config.ArtifactEntry{
		Source:      src,
		Destination: dst,
		Selector:    config.Selector{Limit: new(2)},
		Verify: &config.ArtifactVerification{
			Provider: config.VerifyProviderCosign,
			MatchOIDCIdentity: []config.OIDCIdentity{{
				Issuer:  "https://token.actions.githubusercontent.com",
				Subject: `^https://github\.com/example/.*$`,
			}},
		},
	}
	verifier := &recordingVerifier{err: errors.New("denied")}

	// Plan no longer errors on a verify failure: it appends a single failed job
	// and stops (the entry stays runnable).
	plan, err := New(c, entry, Options{
		Verifier: verifier,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Plan(context.Background())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(plan.Jobs).To(HaveLen(1)) // stopped after the first verify failure
	g.Expect(verifier.refs).To(Equal([]string{src + ":1.1.0"}))

	verifier = &recordingVerifier{err: errors.New("denied")}
	res, err := newRunner().Run(context.Background(), []sync.EntryMirror{
		New(c, entry, Options{
			Verifier: verifier,
			Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		}),
	})
	g.Expect(err).ToNot(HaveOccurred())
	// The entry itself completed; the failure is a per-tag failed row.
	g.Expect(res.Entries[0].Status).To(Equal(sync.EntryCompleted))
	g.Expect(res.Entries[0].Tags).To(HaveLen(1))
	failed := res.Entries[0].Tags[0]
	g.Expect(failed.Tag).To(Equal("1.1.0"))
	g.Expect(failed.Status).To(Equal(sync.StatusFailed))
	g.Expect(failed.Error).To(ContainSubstring("verify " + src + ":1.1.0"))
	g.Expect(failed.Error).To(ContainSubstring("denied"))
	// A verify-failed row carries no digest and no verification block.
	g.Expect(failed.Digest).To(BeEmpty())
	g.Expect(failed.Verification).To(BeNil())
	g.Expect(res.HasFailures()).To(BeTrue())
	g.Expect(res.ExitCode()).To(Equal(1))
}

func TestMirror_SkipsSignatureTooNew(t *testing.T) {
	g := NewWithT(t)
	src := repo("verify-young-src")
	dst := repo("verify-young-dst")

	testregistry.PushImage(t, src+":1.0.0")

	c := oci.NewClient(oci.Insecure())
	entry := config.ArtifactEntry{
		Source:      src,
		Destination: dst,
		Selector:    config.Selector{Limit: new(1)},
		Verify: &config.ArtifactVerification{
			Provider: config.VerifyProviderCosign,
			MinAge:   &metav1.Duration{Duration: time.Hour},
			MatchOIDCIdentity: []config.OIDCIdentity{{
				Issuer:  "https://token.actions.githubusercontent.com",
				Subject: `^https://github\.com/example/.*$`,
			}},
		},
		IncludeReferrers: true,
	}
	integrated := time.Now().Add(-30 * time.Minute)
	verifier := &recordingVerifier{
		info: &oci.VerificationInfo{
			Provider:       config.VerifyProviderCosign,
			Issuer:         "https://token.actions.githubusercontent.com",
			Identity:       "https://github.com/example/app/.github/workflows/release.yml@refs/tags/1.0.0",
			Digest:         "sha256:9a8b7c6d5e4f30211223344556677889aabbccddeeff00112233445566778899",
			IntegratedTime: integrated,
		},
		err: &oci.SignatureTooNewError{
			IntegratedTime: integrated,
			Age:            30 * time.Minute,
			MinAge:         time.Hour,
		},
	}
	res, err := newRunner().Run(context.Background(), []sync.EntryMirror{
		New(c, entry, Options{
			Verifier: verifier,
			Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		}),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.HasFailures()).To(BeFalse())
	g.Expect(verifier.refs).To(Equal([]string{src + ":1.0.0"}))

	skipped := tagsWithStatus(res.Entries[0], sync.StatusSkipped)
	g.Expect(skipped).To(HaveLen(1))
	row := skipped[0]
	g.Expect(row.Tag).To(Equal("1.0.0"))
	// A skip always carries a reason; here it is the deferral.
	g.Expect(row.Reason).To(Equal(sync.ReasonSignatureTooNew))
	// The digest is known even though nothing was copied.
	g.Expect(row.Digest).To(Equal("sha256:9a8b7c6d5e4f30211223344556677889aabbccddeeff00112233445566778899"))
	// The too-new skip carries the age/minAge in its verification block.
	g.Expect(row.Verification).ToNot(BeNil())
	g.Expect(row.Verification.Age).To(Equal("30m0s"))
	g.Expect(row.Verification.MinAge).To(Equal("1h0m0s"))
	g.Expect(row.Verification.Issuer).To(Equal("https://token.actions.githubusercontent.com"))

	_, err = crane.Digest(dst+":1.0.0", crane.Insecure)
	g.Expect(err).To(HaveOccurred())
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
	skipped := tagsWithStatus(res2.Entries[0], sync.StatusSkipped)
	g.Expect(skipped).To(HaveLen(1))
	// An up-to-date skip always carries its reason.
	g.Expect(skipped[0].Reason).To(Equal(sync.ReasonUpToDate))
	g.Expect(tagsWithStatus(res2.Entries[0], sync.StatusCopied)).To(BeEmpty())
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
	drifted := tagsWithStatus(res.Entries[0], sync.StatusDrifted)
	g.Expect(drifted).To(HaveLen(1))
	g.Expect(drifted[0].Tag).To(Equal("1.0.0"))
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
	g.Expect(tagsWithStatus(res.Entries[0], sync.StatusOverwritten)).To(HaveLen(1))

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
	copied := tagsWithStatus(res.Entries[0], sync.StatusCopied)
	g.Expect(copied).To(HaveLen(1))

	// The copied tag row carries a referrers[] array, one entry per referrer.
	refs := copied[0].Referrers
	g.Expect(refs).To(HaveLen(2))
	refTypes := make([]string, 0, len(refs))
	for _, r := range refs {
		g.Expect(r.Digest).To(HavePrefix("sha256:"))
		g.Expect(r.Status).To(Equal(sync.StatusCopied))
		g.Expect(r.Reason).To(BeEmpty()) // reason only on skipped
		refTypes = append(refTypes, r.ArtifactType)
	}
	g.Expect(refTypes).To(ConsistOf(
		"application/vnd.dev.cosign.artifact.sig.v1+json",
		"application/spdx+json",
	))

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
	skipped := tagsWithStatus(res2.Entries[0], sync.StatusSkipped)
	g.Expect(skipped).To(HaveLen(1))
	g.Expect(tagsWithStatus(res2.Entries[0], sync.StatusCopied)).To(BeEmpty())

	// Re-run: the subject and both referrers are up-to-date, each carrying a
	// skipped status with the up-to-date reason.
	g.Expect(skipped[0].Reason).To(Equal(sync.ReasonUpToDate))
	g.Expect(skipped[0].Referrers).To(HaveLen(2))
	for _, r := range skipped[0].Referrers {
		g.Expect(r.Status).To(Equal(sync.StatusSkipped))
		g.Expect(r.Reason).To(Equal(sync.ReasonUpToDate))
	}
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
	g.Expect(tagsWithStatus(res.Entries[0], sync.StatusWouldCopy)).To(HaveLen(1))
	g.Expect(tagsWithStatus(res.Entries[0], sync.StatusCopied)).To(BeEmpty())

	_, err = c.ListTags(context.Background(), dst)
	g.Expect(err).To(HaveOccurred()) // 404 — repo was never created
}
