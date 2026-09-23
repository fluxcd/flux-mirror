// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
)

const (
	// cosignTagSignatureMediaType is the OCI layer media type cosign uses for
	// the "simple signing" payload under its tag-based discovery scheme.
	// Also exported as SimpleSigningMediaType in cosign/v2/pkg/types;
	// duplicated as a literal to avoid depending on cosign for one frozen,
	// spec-defined string.
	// See: https://github.com/sigstore/cosign/blob/main/specs/SIGNATURE_SPEC.md#tag-based-discovery
	cosignTagSignatureMediaType = "application/vnd.dev.cosign.simplesigning.v1+json"

	// cosignTagBundleVersion is the exact bundle version reconstructed
	// bundles are tagged as, not a floor: these annotations only carry a
	// Rekor SET (an inclusion promise, not a proof) and a certificate chain
	// (not a single leaf cert), and sigstore-go's Bundle.validate only
	// accepts that shape at v0.1 — v0.2+ requires an inclusion proof, v0.3+
	// rejects a certificate chain.
	cosignTagBundleVersion = "0.1"

	// OCI annotation keys carrying the detached signature and its keyless
	// verification material (see the SIGNATURE_SPEC above). Equivalent
	// constants exist in cosign/v2/pkg/oci/static, but that package pulls in
	// cosign's full KMS/signing dependency tree (80+ indirect modules);
	// duplicated here as literals instead.
	cosignTagAnnotationSignature   = "dev.cosignproject.cosign/signature"
	cosignTagAnnotationCertificate = "dev.sigstore.cosign/certificate"
	cosignTagAnnotationChain       = "dev.sigstore.cosign/chain"
	cosignTagAnnotationBundle      = "dev.sigstore.cosign/bundle"
)

// errNoCosignTagSignature signals that no cosign tag-based signature
// exists, distinguishing "not found" from a hard fetch or parse failure.
var errNoCosignTagSignature = errors.New("no cosign tag-based signature found")

// cosignTagRekorBundle mirrors the JSON in the dev.sigstore.cosign/bundle
// annotation: a Rekor signed entry timestamp (SET) plus the fields needed
// to reconstruct its signed payload.
type cosignTagRekorBundle struct {
	SignedEntryTimestamp string `json:"SignedEntryTimestamp"`
	Payload              struct {
		Body           string `json:"body"`
		IntegratedTime int64  `json:"integratedTime"`
		LogIndex       int64  `json:"logIndex"`
		LogID          string `json:"logID"`
	} `json:"Payload"`
}

// cosignTagRekorEntryHeader captures just enough of the canonicalized Rekor
// entry body to identify its kind and version, needed to populate a
// sigstore bundle's TransparencyLogEntry.KindVersion.
type cosignTagRekorEntryHeader struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

// cosignTagSimpleSigningPayload is the minimal shape of the "simple signing"
// payload cosign signs in this scheme, used only to bind the signature to
// the verified image digest.
type cosignTagSimpleSigningPayload struct {
	Critical struct {
		Image struct {
			DockerManifestDigest string `json:"docker-manifest-digest"`
		} `json:"image"`
	} `json:"critical"`
}

// cosignSignatureTag computes the tag cosign's tag-based discovery scheme
// uses for digest, e.g. sha256:abcd... becomes sha256-abcd....sig.
func cosignSignatureTag(digest v1.Hash) string {
	return fmt.Sprintf("%s-%s.sig", digest.Algorithm, digest.Hex)
}

// findCosignTagBundle looks up a cosign signature published under cosign's
// tag-based discovery scheme, and reconstructs it as a sigstore Bundle so
// it verifies the same way as an OCI-referrer-based bundle. It also
// returns the raw "simple signing" payload, since that — not the image
// digest — is what the signature was computed over.
func findCosignTagBundle(repo name.Repository, digest v1.Hash, opts ...remote.Option) (*bundle.Bundle, []byte, error) {
	tagRef := repo.Tag(cosignSignatureTag(digest))
	desc, err := remote.Get(tagRef, opts...)
	if err != nil {
		var terr *transport.Error
		if errors.As(err, &terr) && terr.StatusCode == http.StatusNotFound {
			return nil, nil, errNoCosignTagSignature
		}
		return nil, nil, fmt.Errorf("fetch cosign signature tag %s: %w", tagRef, err)
	}
	manifest, err := v1.ParseManifest(bytes.NewReader(desc.Manifest))
	if err != nil {
		return nil, nil, fmt.Errorf("parse cosign signature manifest %s: %w", tagRef, err)
	}

	for _, layer := range manifest.Layers {
		if string(layer.MediaType) != cosignTagSignatureMediaType {
			continue
		}
		return buildCosignTagBundle(repo, digest, layer, opts...)
	}
	return nil, nil, errNoCosignTagSignature
}

// buildCosignTagBundle assembles a sigstore Bundle from a single cosign
// tag-based signature layer and its OCI annotations.
func buildCosignTagBundle(repo name.Repository, digest v1.Hash, layer v1.Descriptor, opts ...remote.Option) (*bundle.Bundle, []byte, error) {
	payload, err := fetchLayerBlob(repo, layer.Digest, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch cosign tag signature payload: %w", err)
	}

	var simpleSigning cosignTagSimpleSigningPayload
	if err := json.Unmarshal(payload, &simpleSigning); err != nil {
		return nil, nil, fmt.Errorf("parse cosign tag signature payload: %w", err)
	}
	if simpleSigning.Critical.Image.DockerManifestDigest != digest.String() {
		return nil, nil, fmt.Errorf("cosign tag signature payload digest %q does not match %q",
			simpleSigning.Critical.Image.DockerManifestDigest, digest.String())
	}

	sigB64, ok := layer.Annotations[cosignTagAnnotationSignature]
	if !ok {
		return nil, nil, fmt.Errorf("cosign tag signature is missing the %q annotation", cosignTagAnnotationSignature)
	}
	signature, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return nil, nil, fmt.Errorf("decode cosign tag signature: %w", err)
	}

	certPEM, ok := layer.Annotations[cosignTagAnnotationCertificate]
	if !ok {
		return nil, nil, fmt.Errorf("cosign tag signature has no certificate; key-based signatures are not supported")
	}
	certs, err := decodeCosignTagCertificateChain(certPEM, layer.Annotations[cosignTagAnnotationChain])
	if err != nil {
		return nil, nil, err
	}

	tlogEntries, err := decodeCosignTagTlogEntries(layer.Annotations[cosignTagAnnotationBundle])
	if err != nil {
		return nil, nil, err
	}

	digestSum := sha256.Sum256(payload)
	mediaType, err := bundle.MediaTypeString(cosignTagBundleVersion)
	if err != nil {
		return nil, nil, fmt.Errorf("build sigstore bundle media type: %w", err)
	}
	pb := &protobundle.Bundle{
		MediaType: mediaType,
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_X509CertificateChain{
				X509CertificateChain: &protocommon.X509CertificateChain{Certificates: certs},
			},
			TlogEntries: tlogEntries,
		},
		Content: &protobundle.Bundle_MessageSignature{
			MessageSignature: &protocommon.MessageSignature{
				MessageDigest: &protocommon.HashOutput{
					Algorithm: protocommon.HashAlgorithm_SHA2_256,
					Digest:    digestSum[:],
				},
				Signature: signature,
			},
		},
	}

	b, err := bundle.NewBundle(pb)
	if err != nil {
		return nil, nil, fmt.Errorf("build sigstore bundle from cosign tag signature: %w", err)
	}
	return b, payload, nil
}

// decodeCosignTagCertificateChain decodes the leaf certificate and optional
// intermediate chain from their PEM annotation values into DER-encoded
// certificates, leaf first.
func decodeCosignTagCertificateChain(certPEM, chainPEM string) ([]*protocommon.X509Certificate, error) {
	leaf, rest := pem.Decode([]byte(certPEM))
	if leaf == nil || len(rest) != 0 {
		return nil, errors.New("decode cosign tag signature certificate: invalid PEM")
	}
	certs := []*protocommon.X509Certificate{{RawBytes: leaf.Bytes}}

	remaining := []byte(chainPEM)
	for {
		var block *pem.Block
		block, remaining = pem.Decode(remaining)
		if block == nil {
			break
		}
		certs = append(certs, &protocommon.X509Certificate{RawBytes: block.Bytes})
	}
	return certs, nil
}

// decodeCosignTagTlogEntries reconstructs a TransparencyLogEntry from the
// dev.sigstore.cosign/bundle annotation, which carries a Rekor v1 SET
// (inclusion promise) rather than an inclusion proof.
func decodeCosignTagTlogEntries(bundleJSON string) ([]*protorekor.TransparencyLogEntry, error) {
	if bundleJSON == "" {
		return nil, fmt.Errorf("cosign tag signature is missing the %q annotation required for transparency log verification", cosignTagAnnotationBundle)
	}

	var rekorBundle cosignTagRekorBundle
	if err := json.Unmarshal([]byte(bundleJSON), &rekorBundle); err != nil {
		return nil, fmt.Errorf("parse cosign tag Rekor bundle annotation: %w", err)
	}

	signedEntryTimestamp, err := base64.StdEncoding.DecodeString(rekorBundle.SignedEntryTimestamp)
	if err != nil {
		return nil, fmt.Errorf("decode cosign tag Rekor SET: %w", err)
	}
	body, err := base64.StdEncoding.DecodeString(rekorBundle.Payload.Body)
	if err != nil {
		return nil, fmt.Errorf("decode cosign tag Rekor entry body: %w", err)
	}
	logID, err := hex.DecodeString(rekorBundle.Payload.LogID)
	if err != nil {
		return nil, fmt.Errorf("decode cosign tag Rekor log ID: %w", err)
	}

	var header cosignTagRekorEntryHeader
	if err := json.Unmarshal(body, &header); err != nil {
		return nil, fmt.Errorf("parse cosign tag Rekor entry body: %w", err)
	}

	return []*protorekor.TransparencyLogEntry{{
		LogIndex:          rekorBundle.Payload.LogIndex,
		LogId:             &protocommon.LogId{KeyId: logID},
		KindVersion:       &protorekor.KindVersion{Kind: header.Kind, Version: header.APIVersion},
		IntegratedTime:    rekorBundle.Payload.IntegratedTime,
		InclusionPromise:  &protorekor.InclusionPromise{SignedEntryTimestamp: signedEntryTimestamp},
		CanonicalizedBody: body,
	}}, nil
}

// fetchLayerBlob reads and returns the uncompressed contents of the blob
// addressed by digest in repo.
func fetchLayerBlob(repo name.Repository, digest v1.Hash, opts ...remote.Option) ([]byte, error) {
	layer, err := remote.Layer(repo.Digest(digest.String()), opts...)
	if err != nil {
		return nil, err
	}
	reader, err := layer.Uncompressed()
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(reader)
	if closeErr := reader.Close(); closeErr != nil {
		return nil, fmt.Errorf("close blob: %w", closeErr)
	}
	if readErr != nil {
		return nil, fmt.Errorf("read blob: %w", readErr)
	}
	return data, nil
}
