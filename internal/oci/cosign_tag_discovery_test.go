// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/sigstore/rekor/pkg/types/hashedrekord"
	hashedrekordv001 "github.com/sigstore/rekor/pkg/types/hashedrekord/v0.0.1"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/fluxcd/flux-mirror/internal/testregistry"
)

func TestCosignSignatureTag(t *testing.T) {
	g := NewWithT(t)
	digest, err := v1.NewHash("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(cosignSignatureTag(digest)).To(Equal("sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.sig"))
}

// pushCosignTagSignature pushes a manifest tagged per cosign's
// tag-based discovery scheme (SIGNATURE_SPEC.md), carrying a single
// simplesigning layer with the annotations a real cosign-published signature
// would have. It returns the raw "simple signing" payload bytes.
func pushCosignTagSignature(t testing.TB, repoAddr string, digest v1.Hash, mutateAnnotations func(map[string]string)) []byte {
	t.Helper()
	testregistry.UseEmptyDockerConfig(t)

	payload := []byte(fmt.Sprintf(`{"critical":{"identity":{"docker-reference":""},"image":{"docker-manifest-digest":%q},"type":"cosign container image signature"}}`, digest.String()))

	key, certPEM := newSelfSignedKeyAndCertPEM(t)
	sum := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatalf("sign payload: %s", err)
	}

	rekorBody := newHashedRekordBody(t, payload, sig, certPEM)
	rekorBundle := cosignTagRekorBundle{
		SignedEntryTimestamp: base64.StdEncoding.EncodeToString([]byte("test-set")),
	}
	rekorBundle.Payload.Body = base64.StdEncoding.EncodeToString(rekorBody)
	rekorBundle.Payload.IntegratedTime = 1727368221
	rekorBundle.Payload.LogIndex = 134365715
	rekorBundle.Payload.LogID = "c0d23d6ad406973f9559f3ba2d1ca01f84147d8ffc5b8445c224f98b9591801d"
	rekorBundleJSON, err := json.Marshal(rekorBundle)
	if err != nil {
		t.Fatalf("marshal rekor bundle: %s", err)
	}

	annotations := map[string]string{
		cosignTagAnnotationSignature:   base64.StdEncoding.EncodeToString(sig),
		cosignTagAnnotationCertificate: certPEM,
		cosignTagAnnotationBundle:      string(rekorBundleJSON),
	}
	if mutateAnnotations != nil {
		mutateAnnotations(annotations)
	}

	layer := static.NewLayer(payload, types.MediaType(cosignTagSignatureMediaType))
	img, err := mutate.Append(empty.Image, mutate.Addendum{
		Layer:       layer,
		Annotations: annotations,
	})
	if err != nil {
		t.Fatalf("build cosign tag signature image: %s", err)
	}
	img = mutate.MediaType(img, types.OCIManifestSchema1)

	tagRef := repoAddr + ":" + cosignSignatureTag(digest)
	parsed, err := name.ParseReference(tagRef, name.Insecure)
	if err != nil {
		t.Fatalf("parse %s: %s", tagRef, err)
	}
	if err := remote.Push(parsed, img); err != nil {
		t.Fatalf("push %s: %s", tagRef, err)
	}
	return payload
}

// newHashedRekordBody builds a minimal, schema-valid Rekor "hashedrekord"
// entry body, matching what a real Rekor log returns and what
// tlog.ParseTransparencyLogEntry requires to parse the reconstructed bundle.
func newHashedRekordBody(t testing.TB, payload, sig []byte, certPEM string) []byte {
	t.Helper()
	sum := sha256.Sum256(payload)
	body := map[string]any{
		"apiVersion": hashedrekordv001.APIVERSION,
		"kind":       hashedrekord.KIND,
		"spec": map[string]any{
			"data": map[string]any{
				"hash": map[string]any{
					"algorithm": "sha256",
					"value":     hex.EncodeToString(sum[:]),
				},
			},
			"signature": map[string]any{
				"content": base64.StdEncoding.EncodeToString(sig),
				"publicKey": map[string]any{
					"content": base64.StdEncoding.EncodeToString([]byte(certPEM)),
				},
			},
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal hashedrekord body: %s", err)
	}
	return data
}

func newSelfSignedCertPEM(t testing.TB) string {
	t.Helper()
	_, certPEM := newSelfSignedKeyAndCertPEM(t)
	return certPEM
}

// newSelfSignedKeyAndCertPEM generates an ECDSA P-256 key and a matching
// self-signed PEM certificate, so tests can produce a real signature that
// passes Rekor's hashedrekord entry validation (which cryptographically
// verifies the signature against the embedded public key).
func newSelfSignedKeyAndCertPEM(t testing.TB) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %s", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %s", err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestFindCosignTagBundle(t *testing.T) {
	tests := []struct {
		name      string
		repoStem  string
		setup     func(t testing.TB, repoAddr string, digest v1.Hash) []byte // returns the pushed payload, nil if not applicable
		wantErr   string
		wantErrIs error
	}{
		{
			name:     "signature found and parsed",
			repoStem: "cosign-tag-ok",
			setup: func(t testing.TB, repoAddr string, digest v1.Hash) []byte {
				return pushCosignTagSignature(t, repoAddr, digest, nil)
			},
		},
		{
			name:      "no signature tag",
			repoStem:  "cosign-tag-missing",
			setup:     func(t testing.TB, repoAddr string, digest v1.Hash) []byte { return nil },
			wantErrIs: errNoCosignTagSignature,
		},
		{
			name:     "payload digest does not match",
			repoStem: "cosign-tag-mismatch",
			setup: func(t testing.TB, repoAddr string, digest v1.Hash) []byte {
				// The payload embeds a different digest than the one looked up.
				otherDigest, err := v1.NewHash("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
				if err != nil {
					t.Fatalf("parse other digest: %s", err)
				}
				pushMismatchedCosignTagSignature(t, repoAddr, digest, otherDigest)
				return nil
			},
			wantErr: "does not match",
		},
		{
			name:     "missing certificate annotation",
			repoStem: "cosign-tag-nocert",
			setup: func(t testing.TB, repoAddr string, digest v1.Hash) []byte {
				pushCosignTagSignature(t, repoAddr, digest, func(a map[string]string) {
					delete(a, cosignTagAnnotationCertificate)
				})
				return nil
			},
			wantErr: "key-based signatures are not supported",
		},
		{
			name:     "missing rekor bundle annotation",
			repoStem: "cosign-tag-notlog",
			setup: func(t testing.TB, repoAddr string, digest v1.Hash) []byte {
				pushCosignTagSignature(t, repoAddr, digest, func(a map[string]string) {
					delete(a, cosignTagAnnotationBundle)
				})
				return nil
			},
			wantErr: "transparency log verification",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			repoAddr := repo(tt.repoStem)
			digestStr := testregistry.PushImage(t, repoAddr+":v1")
			digest, err := v1.NewHash(digestStr)
			g.Expect(err).ToNot(HaveOccurred())

			payload := tt.setup(t, repoAddr, digest)

			repoName, err := name.NewRepository(repoAddr, name.Insecure)
			g.Expect(err).ToNot(HaveOccurred())

			b, gotPayload, err := findCosignTagBundle(repoName, digest)
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
			g.Expect(gotPayload).To(Equal(payload))

			g.Expect(b.MediaType).To(Equal(mustBundleMediaType(t, cosignTagBundleVersion)))
			msg := b.GetMessageSignature()
			g.Expect(msg).ToNot(BeNil())
			sum := sha256.Sum256(payload)
			g.Expect(msg.MessageDigest.Digest).To(Equal(sum[:]))
			g.Expect(msg.Signature).ToNot(BeEmpty())

			vm := b.VerificationMaterial
			g.Expect(vm.GetX509CertificateChain().Certificates).To(HaveLen(1))
			g.Expect(vm.TlogEntries).To(HaveLen(1))
			entry := vm.TlogEntries[0]
			g.Expect(entry.KindVersion.Kind).To(Equal(hashedrekord.KIND))
			g.Expect(entry.KindVersion.Version).To(Equal(hashedrekordv001.APIVERSION))
			g.Expect(entry.LogIndex).To(Equal(int64(134365715)))
			g.Expect(entry.InclusionPromise.SignedEntryTimestamp).To(Equal([]byte("test-set")))
		})
	}
}

// pushMismatchedCosignTagSignature pushes a cosign tag-based signature whose
// "simple signing" payload embeds otherDigest instead of digest, so
// findCosignTagBundle's digest-binding check can be exercised.
func pushMismatchedCosignTagSignature(t testing.TB, repoAddr string, digest, otherDigest v1.Hash) {
	t.Helper()
	testregistry.UseEmptyDockerConfig(t)
	payload := []byte(fmt.Sprintf(`{"critical":{"image":{"docker-manifest-digest":%q}}}`, otherDigest.String()))
	layer := static.NewLayer(payload, types.MediaType(cosignTagSignatureMediaType))
	img, err := mutate.Append(empty.Image, mutate.Addendum{
		Layer: layer,
		Annotations: map[string]string{
			cosignTagAnnotationSignature:   base64.StdEncoding.EncodeToString([]byte("sig")),
			cosignTagAnnotationCertificate: newSelfSignedCertPEM(t),
			cosignTagAnnotationBundle:      `{"SignedEntryTimestamp":"","Payload":{"body":"","integratedTime":0,"logIndex":0,"logID":""}}`,
		},
	})
	if err != nil {
		t.Fatalf("build mismatched cosign tag signature image: %s", err)
	}
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	tagRef := repoAddr + ":" + cosignSignatureTag(digest)
	parsed, err := name.ParseReference(tagRef, name.Insecure)
	if err != nil {
		t.Fatalf("parse %s: %s", tagRef, err)
	}
	if err := remote.Push(parsed, img); err != nil {
		t.Fatalf("push %s: %s", tagRef, err)
	}
}
