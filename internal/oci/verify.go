// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"

	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta1"
)

const (
	minBundleVersion              = "v0.3"
	sigstoreBundleMediaTypePrefix = "application/vnd.dev.sigstore.bundle"
)

// VerificationInfo carries the metadata of a confirmed cosign signature, so
// the report can record who signed it and when. Only Provider is guaranteed;
// the rest is best-effort: Issuer/Identity are populated from the keyless
// certificate identity, and IntegratedTime only when the bundle carries a
// verified transparency-log timestamp (otherwise it is the zero time).
type VerificationInfo struct {
	Provider       string
	Issuer         string
	Identity       string
	Digest         string
	IntegratedTime time.Time
}

// SignatureTooNewError reports a valid signature that has not aged past the
// configured minimum transparency-log integration age.
type SignatureTooNewError struct {
	IntegratedTime time.Time
	Age            time.Duration
	MinAge         time.Duration
}

func (e *SignatureTooNewError) Error() string {
	return fmt.Sprintf("signature age (%s) is less than the required minAge (%s); signature was integrated at %s",
		e.Age.Round(time.Second),
		e.MinAge,
		e.IntegratedTime.Format(time.RFC3339))
}

// Verifier verifies cosign keyless signatures stored as OCI referrers.
type Verifier struct {
	client *Client

	mu          sync.Mutex
	trustedRoot *root.TrustedRoot
}

// NewVerifier returns a verifier using the same registry options as the OCI client.
func NewVerifier(client *Client) *Verifier {
	if client == nil {
		client = NewClient()
	}
	return &Verifier{client: client}
}

// Verify checks the cosign signature for ref against the configured OIDC
// identities. On success it returns the confirmed VerificationInfo (provider,
// keyless identity/issuer, source digest, and the Tlog integrated time when
// present). When the signature is valid but younger than cfg.MinAge it returns
// both the info and a *SignatureTooNewError so the caller can record the
// deferral with its metadata; any other failure returns (nil, err).
func (v *Verifier) Verify(ctx context.Context, ref string, cfg apiv1.ArtifactVerification) (*VerificationInfo, error) {
	if strings.TrimSpace(cfg.Provider) != apiv1.VerifyProviderCosign {
		return nil, fmt.Errorf("unsupported verification provider %q", cfg.Provider)
	}
	if len(cfg.MatchOIDCIdentity) == 0 {
		return nil, fmt.Errorf("at least one OIDC identity matcher is required")
	}

	ref = strings.TrimPrefix(ref, "oci://")
	parsed, err := name.ParseReference(ref, v.client.staticNameOpts...)
	if err != nil {
		return nil, fmt.Errorf("parse reference %q: %w", ref, err)
	}

	opts := v.remoteOptions(ctx)
	desc, err := remote.Get(parsed, opts...)
	if err != nil {
		return nil, fmt.Errorf("fetch descriptor for %q: %w", ref, err)
	}

	repo := parsed.Context()
	digestRef := repo.Digest(desc.Digest.String())
	idx, err := remote.Referrers(digestRef, opts...)
	if err != nil {
		return nil, fmt.Errorf("list referrers for %s: %w", digestRef, err)
	}
	manifest, err := idx.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("read referrers index for %s: %w", digestRef, err)
	}

	bundleBytes, err := findSigstoreBundle(repo, manifest, opts...)
	if err != nil {
		return nil, err
	}

	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleBytes); err != nil {
		return nil, fmt.Errorf("parse sigstore bundle: %w", err)
	}
	if !b.MinVersion(minBundleVersion) {
		return nil, fmt.Errorf("unsupported sigstore bundle version: minimum %s required", minBundleVersion)
	}

	trustedRoot, err := v.getTrustedRoot()
	if err != nil {
		return nil, err
	}
	sigVerifier, err := verify.NewVerifier(trustedRoot,
		verify.WithSignedCertificateTimestamps(1),
		verify.WithIntegratedTimestamps(1),
		verify.WithTransparencyLog(1),
	)
	if err != nil {
		return nil, fmt.Errorf("create signature verifier: %w", err)
	}

	identityOptions, err := certificateIdentityOptions(cfg.MatchOIDCIdentity)
	if err != nil {
		return nil, err
	}
	digestBytes, err := hex.DecodeString(desc.Digest.Hex)
	if err != nil {
		return nil, fmt.Errorf("decode digest %s: %w", desc.Digest.String(), err)
	}

	policyOptions := append([]verify.PolicyOption(nil), identityOptions...)
	policy := verify.NewPolicy(
		verify.WithArtifactDigest(desc.Digest.Algorithm, digestBytes),
		policyOptions...,
	)
	result, err := sigVerifier.Verify(&b, policy)
	if err != nil {
		return nil, fmt.Errorf("signature verification failed: %w", err)
	}

	info := &VerificationInfo{
		Provider: apiv1.VerifyProviderCosign,
		Digest:   desc.Digest.String(),
	}
	// Read the actual signed identity off the verified certificate summary, not
	// result.VerifiedIdentity — the latter echoes the configured matcher (whose
	// SAN is empty when a regex matcher is used and whose issuer is only the
	// configured value), whereas the certificate summary carries the real SAN
	// and the OIDC issuer extension recorded at signing time.
	if result.Signature != nil && result.Signature.Certificate != nil {
		cert := result.Signature.Certificate
		info.Identity = cert.SubjectAlternativeName
		info.Issuer = cert.Extensions.Issuer
	}
	// IntegratedTime is best-effort: present only when the bundle carries a
	// verified Tlog timestamp. Left zero (omitted in the report) otherwise.
	if t, ok := signatureIntegratedTime(result); ok {
		info.IntegratedTime = t
	}

	// On a too-new signature, hand back both the metadata and the typed signal
	// so the caller can record the deferral with its verification block; any
	// other minAge error (no Tlog timestamp) is a hard failure.
	if cfg.MinAge != nil && cfg.MinAge.Duration > 0 {
		if err := enforceMinAge(result, cfg.MinAge.Duration, time.Now()); err != nil {
			var tooNew *SignatureTooNewError
			if errors.As(err, &tooNew) {
				return info, err
			}
			return nil, err
		}
	}
	return info, nil
}

// signatureIntegratedTime returns the oldest verified transparency-log (Tlog)
// integration timestamp in the result, and whether one was found.
func signatureIntegratedTime(result *verify.VerificationResult) (time.Time, bool) {
	var signatureTime time.Time
	for _, ts := range result.VerifiedTimestamps {
		if ts.Type != "Tlog" || ts.Timestamp.IsZero() {
			continue
		}
		if signatureTime.IsZero() || ts.Timestamp.Before(signatureTime) {
			signatureTime = ts.Timestamp
		}
	}
	return signatureTime, !signatureTime.IsZero()
}

// enforceMinAge checks the signature's transparency-log age against minAge. It
// returns a *SignatureTooNewError when the signature is valid but too young, a
// plain error when no Tlog timestamp is available, and nil when old enough.
// now is injectable so the age math is unit-testable.
func enforceMinAge(result *verify.VerificationResult, minAge time.Duration, now time.Time) error {
	signatureTime, ok := signatureIntegratedTime(result)
	if !ok {
		return fmt.Errorf("cannot enforce minAge: no verified transparency log integrated timestamps found in signature bundle")
	}
	signatureAge := now.Sub(signatureTime)
	if signatureAge < 0 {
		signatureAge = 0
	}
	if signatureAge < minAge {
		return &SignatureTooNewError{
			IntegratedTime: signatureTime,
			Age:            signatureAge,
			MinAge:         minAge,
		}
	}
	return nil
}

func (v *Verifier) remoteOptions(ctx context.Context) []remote.Option {
	opts := make([]remote.Option, 0, len(v.client.staticRemoteOpts)+1)
	opts = append(opts, remote.WithContext(ctx))
	return append(opts, v.client.staticRemoteOpts...)
}

func (v *Verifier) getTrustedRoot() (*root.TrustedRoot, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.trustedRoot != nil {
		return v.trustedRoot, nil
	}

	opts := tuf.DefaultOptions()
	opts.WithDisableLocalCache()
	tufClient, err := tuf.New(opts)
	if err != nil {
		return nil, fmt.Errorf("create TUF client: %w", err)
	}
	trustedRootJSON, err := tufClient.GetTarget("trusted_root.json")
	if err != nil {
		return nil, fmt.Errorf("fetch trusted root: %w", err)
	}
	trustedRoot, err := root.NewTrustedRootFromJSON(trustedRootJSON)
	if err != nil {
		return nil, fmt.Errorf("parse trusted root: %w", err)
	}
	v.trustedRoot = trustedRoot
	return trustedRoot, nil
}

func certificateIdentityOptions(identities []apiv1.OIDCIdentity) ([]verify.PolicyOption, error) {
	opts := make([]verify.PolicyOption, 0, len(identities))
	for i, id := range identities {
		certID, err := verify.NewShortCertificateIdentity(id.Issuer, "", "", id.Subject)
		if err != nil {
			return nil, fmt.Errorf("create OIDC identity matcher %d: %w", i, err)
		}
		opts = append(opts, verify.WithCertificateIdentity(certID))
	}
	return opts, nil
}

func findSigstoreBundle(repo name.Repository, manifest *v1.IndexManifest, opts ...remote.Option) ([]byte, error) {
	for _, m := range manifest.Manifests {
		img, err := remote.Image(repo.Digest(m.Digest.String()), opts...)
		if err != nil {
			continue
		}
		layers, err := img.Layers()
		if err != nil || len(layers) != 1 {
			continue
		}
		mediaType, err := layers[0].MediaType()
		if err != nil || !strings.HasPrefix(string(mediaType), sigstoreBundleMediaTypePrefix) {
			continue
		}
		reader, err := layers[0].Uncompressed()
		if err != nil {
			continue
		}
		data, readErr := io.ReadAll(reader)
		if closeErr := reader.Close(); closeErr != nil {
			return nil, fmt.Errorf("close sigstore bundle layer: %w", closeErr)
		}
		if readErr != nil {
			return nil, fmt.Errorf("read sigstore bundle layer: %w", readErr)
		}
		return data, nil
	}
	return nil, fmt.Errorf("no sigstore bundle found in referrers")
}
