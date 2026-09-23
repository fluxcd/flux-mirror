// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"crypto/sha256"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	sigverify "github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/fluxcd/flux-mirror/internal/testregistry"
)

func TestEnforceMinAge(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		result    *sigverify.VerificationResult
		minAge    time.Duration
		wantErr   string
		wantYoung bool
	}{
		{
			name: "old enough",
			result: &sigverify.VerificationResult{VerifiedTimestamps: []sigverify.TimestampVerificationResult{{
				Type:      "Tlog",
				Timestamp: now.Add(-2 * time.Hour),
			}}},
			minAge: time.Hour,
		},
		{
			name: "uses oldest verified tlog timestamp",
			result: &sigverify.VerificationResult{VerifiedTimestamps: []sigverify.TimestampVerificationResult{
				{Type: "Tlog", Timestamp: now.Add(-30 * time.Minute)},
				{Type: "Tlog", Timestamp: now.Add(-2 * time.Hour)},
			}},
			minAge: time.Hour,
		},
		{
			name: "too new",
			result: &sigverify.VerificationResult{VerifiedTimestamps: []sigverify.TimestampVerificationResult{{
				Type:      "Tlog",
				Timestamp: now.Add(-30 * time.Minute),
			}}},
			minAge:    time.Hour,
			wantErr:   "signature age (30m0s) is less than the required minAge (1h0m0s)",
			wantYoung: true,
		},
		{
			name: "no tlog timestamp",
			result: &sigverify.VerificationResult{VerifiedTimestamps: []sigverify.TimestampVerificationResult{{
				Type:      "TimestampAuthority",
				Timestamp: now.Add(-2 * time.Hour),
			}}},
			minAge:  time.Hour,
			wantErr: "no verified transparency log integrated timestamps",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			err := enforceMinAge(tt.result, tt.minAge, now)
			if tt.wantErr == "" {
				g.Expect(err).ToNot(HaveOccurred())
				return
			}
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tt.wantErr))
			var young *SignatureTooNewError
			g.Expect(errors.As(err, &young)).To(Equal(tt.wantYoung))
		})
	}
}

// newTestSigstoreBundleJSON builds a minimal, schema-valid sigstore bundle
// (single self-signed certificate + dummy message signature, no tlog
// entries) at the given bundle media type, so tests can exercise
// findReferrerBundle's parsing/version-gating without a real signing
// pipeline. It reuses newSelfSignedKeyAndCertPEM from
// cosign_tag_discovery_test.go (same package).
func newTestSigstoreBundleJSON(t testing.TB, mediaType string) []byte {
	t.Helper()
	_, certPEM := newSelfSignedKeyAndCertPEM(t)
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatalf("decode test certificate PEM")
		return nil
	}

	pb := &protobundle.Bundle{
		MediaType: mediaType,
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_Certificate{
				Certificate: &protocommon.X509Certificate{RawBytes: block.Bytes},
			},
		},
		Content: &protobundle.Bundle_MessageSignature{
			MessageSignature: &protocommon.MessageSignature{
				MessageDigest: &protocommon.HashOutput{
					Algorithm: protocommon.HashAlgorithm_SHA2_256,
					Digest:    make([]byte, sha256.Size),
				},
				Signature: []byte("test-signature-bytes"),
			},
		},
	}
	b, err := bundle.NewBundle(pb)
	if err != nil {
		t.Fatalf("build test sigstore bundle: %s", err)
	}
	data, err := b.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal test sigstore bundle: %s", err)
	}
	return data
}

// pushReferrerBundle pushes bundleJSON as a single-layer OCI referrer of
// mediaType, linked to digest via the `subject` manifest field, so it is
// discoverable through the OCI 1.1 referrers API.
func pushReferrerBundle(t testing.TB, repoAddr string, digest v1.Hash, mediaType string, bundleJSON []byte) {
	t.Helper()
	testregistry.UseEmptyDockerConfig(t)

	layer := static.NewLayer(bundleJSON, types.MediaType(mediaType))
	img, err := mutate.Append(empty.Image, mutate.Addendum{Layer: layer})
	if err != nil {
		t.Fatalf("build referrer bundle image: %s", err)
	}
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	subject := v1.Descriptor{MediaType: types.OCIManifestSchema1, Digest: digest}
	img = mutate.Subject(img, subject).(v1.Image)

	dig, err := img.Digest()
	if err != nil {
		t.Fatalf("digest referrer bundle: %s", err)
	}
	ref, err := name.NewDigest(repoAddr+"@"+dig.String(), name.Insecure)
	if err != nil {
		t.Fatalf("parse referrer bundle ref: %s", err)
	}
	if err := remote.Push(ref, img); err != nil {
		t.Fatalf("push referrer bundle %s: %s", ref, err)
	}
}

// mustBundleMediaType returns the current media type string sigstore-go uses
// for the given bundle version (e.g. "0.3"), via the SDK's own
// bundle.MediaTypeString, so tests stay in sync with whatever format the SDK
// actually produces instead of hardcoding a media type string ourselves.
func mustBundleMediaType(t testing.TB, version string) string {
	t.Helper()
	mediaType, err := bundle.MediaTypeString(version)
	if err != nil {
		t.Fatalf("build bundle media type for version %q: %s", version, err)
	}
	return mediaType
}

// pushReferrerBundleVersion builds a minimal, schema-valid sigstore bundle
// at the given version (e.g. "0.3") and pushes it as an OCI referrer of
// digest, combining mustBundleMediaType, newTestSigstoreBundleJSON, and
// pushReferrerBundle for the common case of "push a referrer bundle of this
// version".
func pushReferrerBundleVersion(t testing.TB, repoAddr string, digest v1.Hash, version string) {
	t.Helper()
	mediaType := mustBundleMediaType(t, version)
	bundleJSON := newTestSigstoreBundleJSON(t, mediaType)
	pushReferrerBundle(t, repoAddr, digest, mediaType, bundleJSON)
}

func TestFindReferrerBundle(t *testing.T) {
	tests := []struct {
		name      string
		repoStem  string
		setup     func(t testing.TB, repoAddr string, digest v1.Hash)
		wantErr   string
		wantErrIs error
	}{
		{
			name:     "referrer bundle found and parsed",
			repoStem: "referrer-ok",
			setup: func(t testing.TB, repoAddr string, digest v1.Hash) {
				pushReferrerBundleVersion(t, repoAddr, digest, "0.3")
			},
		},
		{
			name:      "no referrers present",
			repoStem:  "referrer-missing",
			setup:     func(t testing.TB, repoAddr string, digest v1.Hash) {},
			wantErrIs: errNoReferrerBundle,
		},
		{
			name:     "unsupported bundle version",
			repoStem: "referrer-oldver",
			setup: func(t testing.TB, repoAddr string, digest v1.Hash) {
				pushReferrerBundleVersion(t, repoAddr, digest, "0.1")
			},
			wantErr: "unsupported sigstore bundle version",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			repoAddr := repo(tt.repoStem)
			digestStr := testregistry.PushImage(t, repoAddr+":v1")
			digest, err := v1.NewHash(digestStr)
			g.Expect(err).ToNot(HaveOccurred())

			tt.setup(t, repoAddr, digest)

			repoName, err := name.NewRepository(repoAddr, name.Insecure)
			g.Expect(err).ToNot(HaveOccurred())

			discovered, err := findReferrerBundle(repoName, digest)
			if tt.wantErrIs != nil {
				g.Expect(err).To(MatchError(tt.wantErrIs))
				return
			}
			if tt.wantErr != "" {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring(tt.wantErr))
				return
			}
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(discovered.bundle).ToNot(BeNil())
			g.Expect(discovered.artifact).ToNot(BeNil())

			// The returned artifact option must actually require an artifact
			// match (not silently accept anything), confirming it was wired
			// up correctly.
			policy := sigverify.NewPolicy(discovered.artifact, sigverify.WithoutIdentitiesUnsafe())
			cfg, err := policy.BuildConfig()
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(cfg.RequireArtifact()).To(BeTrue())
		})
	}
}

func TestDiscoverSignatureBundle(t *testing.T) {
	tests := []struct {
		name     string
		repoStem string
		setup    func(t testing.TB, repoAddr string, digest v1.Hash)
		wantErr  string
	}{
		{
			name:     "uses referrer without falling back to cosign tag",
			repoStem: "discover-referrer",
			setup: func(t testing.TB, repoAddr string, digest v1.Hash) {
				pushReferrerBundleVersion(t, repoAddr, digest, "0.3")
				// Deliberately no cosign tag-based signature pushed: if
				// discoverSignatureBundle fell back unnecessarily, this
				// would fail.
			},
		},
		{
			name:     "falls back to cosign tag-based discovery",
			repoStem: "discover-fallback",
			setup: func(t testing.TB, repoAddr string, digest v1.Hash) {
				// No referrer pushed at all: the registry supports the OCI
				// referrers API (as asserted by TestSnapshotAndCopyReferrers)
				// but has no referrer for this digest, so
				// discoverSignatureBundle must fall back to the cosign
				// tag-based scheme.
				pushCosignTagSignature(t, repoAddr, digest, nil)
			},
		},
		{
			name:     "no signature found anywhere",
			repoStem: "discover-none",
			setup:    func(t testing.TB, repoAddr string, digest v1.Hash) {},
			wantErr:  "no sigstore bundle found via OCI referrers or cosign tag-based discovery",
		},
		{
			name:     "hard referrer error does not fall back",
			repoStem: "discover-harderr",
			setup: func(t testing.TB, repoAddr string, digest v1.Hash) {
				// An unsupported (too old) bundle version is a hard error,
				// not "no bundle found", so discoverSignatureBundle must
				// propagate it even though a valid cosign tag-based
				// signature is also present below.
				pushReferrerBundleVersion(t, repoAddr, digest, "0.1")
				pushCosignTagSignature(t, repoAddr, digest, nil)
			},
			wantErr: "unsupported sigstore bundle version",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			repoAddr := repo(tt.repoStem)
			digestStr := testregistry.PushImage(t, repoAddr+":v1")
			digest, err := v1.NewHash(digestStr)
			g.Expect(err).ToNot(HaveOccurred())

			tt.setup(t, repoAddr, digest)

			repoName, err := name.NewRepository(repoAddr, name.Insecure)
			g.Expect(err).ToNot(HaveOccurred())
			desc := &remote.Descriptor{}
			desc.Digest = digest

			discovered, err := discoverSignatureBundle(repoName, desc)
			if tt.wantErr != "" {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring(tt.wantErr))
				return
			}
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(discovered.bundle).ToNot(BeNil())
			g.Expect(discovered.artifact).ToNot(BeNil())
		})
	}
}
