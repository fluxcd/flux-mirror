// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package v1beta2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1beta1 "github.com/fluxcd/flux-mirror/api/v1beta1"
)

// FromV1Beta1 converts a v1beta1 Config to the current v1beta2 shape. The two
// revisions differ only in wire names: v1beta2 makes credential.type required,
// renames the credential claims (aud->audiences, iss->issuer, sub->subject,
// exp->expiration), moves hosts[].username under credential, and renames the
// SPIFFE credential and TLS providers to spiffe. Callers gate this behind a
// deprecation warning; the conversion preserves the caller's intent so an old
// config keeps working.
func FromV1Beta1(in *apiv1beta1.Config) *Config {
	if in == nil {
		return nil
	}
	out := &Config{
		TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: ConfigKind},
	}
	for _, h := range in.Hosts {
		out.Hosts = append(out.Hosts, registryHostFromV1Beta1(h))
	}
	for _, c := range in.Charts {
		out.Charts = append(out.Charts, ChartEntry{
			Source:      c.Source,
			Destination: c.Destination,
			Name:        c.Name,
			Version:     c.Version,
			Limit:       c.Limit,
			Overwrite:   c.Overwrite,
		})
	}
	for _, a := range in.Artifacts {
		out.Artifacts = append(out.Artifacts, ArtifactEntry{
			Source:           a.Source,
			Destination:      a.Destination,
			Selector:         selectorFromV1Beta1(a.Selector),
			Overwrite:        a.Overwrite,
			IncludeReferrers: a.IncludeReferrers,
			Verify:           verificationFromV1Beta1(a.Verify),
		})
	}
	return out
}

func registryHostFromV1Beta1(in apiv1beta1.RegistryHost) RegistryHost {
	out := RegistryHost{
		Host:         in.Host,
		Provider:     in.Provider,
		TLS:          tlsFromV1Beta1(in.TLS),
		MaxChunkSize: in.MaxChunkSize,
	}
	if in.Credential != nil {
		out.Credential = credentialFromV1Beta1(*in.Credential, in.Username)
	}
	return out
}

func credentialFromV1Beta1(in apiv1beta1.RegistryCredential, username string) *RegistryCredential {
	out := &RegistryCredential{
		Type:       CredentialTypeJWT,
		Provider:   providerFromV1Beta1(in.Provider),
		Value:      in.Value,
		FromPath:   in.FromPath,
		JWKPath:    in.JWKPath,
		JWKValue:   in.JWKValue,
		Issuer:     in.Iss,
		Subject:    in.Sub,
		Expiration: in.Exp,
		Username:   username,
		Envelope:   in.Envelope,
	}
	if in.Aud != "" {
		out.Audiences = []string{in.Aud}
	}
	return out
}

// providerFromV1Beta1 maps the v1beta1 credential provider jwt-svid to v1beta2's
// spiffe; every other value is carried over unchanged.
func providerFromV1Beta1(provider string) string {
	if provider == apiv1beta1.JWTProviderJWTSVID {
		return JWTProviderSPIFFE
	}
	return provider
}

func tlsFromV1Beta1(in *apiv1beta1.TLS) *TLS {
	if in == nil {
		return nil
	}
	return &TLS{
		ServerAuth: serverAuthFromV1Beta1(in.ServerAuth),
		ClientAuth: clientAuthFromV1Beta1(in.ClientAuth),
	}
}

func serverAuthFromV1Beta1(in *apiv1beta1.TLSServerAuth) *TLSServerAuth {
	if in == nil {
		return nil
	}
	out := &TLSServerAuth{
		FromPath: in.FromPath,
		Value:    in.Value,
		SPIFFE:   spiffeFromV1Beta1(in.SPIFFE),
	}
	// v1beta1 selects SPIFFE over the file fields by setting spiffe; v1beta2
	// selects it with the provider enum.
	if in.SPIFFE != nil {
		out.Provider = TLSProviderSPIFFE
	}
	return out
}

func clientAuthFromV1Beta1(in *apiv1beta1.TLSClientAuth) *TLSClientAuth {
	if in == nil {
		return nil
	}
	out := &TLSClientAuth{
		Provider:    providerFromV1Beta1TLS(in.Provider),
		Certificate: tlsDataFromV1Beta1(in.Certificate),
		Key:         tlsKeyFromV1Beta1(in.Key),
	}
	return out
}

// providerFromV1Beta1TLS maps the v1beta1 client auth provider x509-svid to
// v1beta2's spiffe; every other value is carried over unchanged.
func providerFromV1Beta1TLS(provider string) string {
	if provider == apiv1beta1.TLSClientProviderX509SVID {
		return TLSProviderSPIFFE
	}
	return provider
}

func spiffeFromV1Beta1(in *apiv1beta1.SPIFFETLS) *SPIFFETLS {
	if in == nil {
		return nil
	}
	return &SPIFFETLS{
		ServerID:     in.ServerID,
		TrustDomain:  in.TrustDomain,
		AuthorizeAny: in.AuthorizeAny,
	}
}

func tlsDataFromV1Beta1(in *apiv1beta1.TLSData) *TLSData {
	if in == nil {
		return nil
	}
	return &TLSData{FromPath: in.FromPath, Value: in.Value}
}

func tlsKeyFromV1Beta1(in *apiv1beta1.TLSKey) *TLSKey {
	if in == nil {
		return nil
	}
	return &TLSKey{FromPath: in.FromPath, Value: in.Value}
}

func selectorFromV1Beta1(in apiv1beta1.Selector) Selector {
	out := Selector{
		Semver: in.Semver,
		SortBy: in.SortBy,
		Limit:  in.Limit,
	}
	if in.Regex != nil {
		out.Regex = &RegexFilter{Pattern: in.Regex.Pattern, Extract: in.Regex.Extract}
	}
	return out
}

func verificationFromV1Beta1(in *apiv1beta1.ArtifactVerification) *ArtifactVerification {
	if in == nil {
		return nil
	}
	out := &ArtifactVerification{
		Provider: in.Provider,
		MinAge:   in.MinAge,
	}
	for _, id := range in.MatchOIDCIdentity {
		out.MatchOIDCIdentity = append(out.MatchOIDCIdentity, OIDCIdentity{
			Issuer:  id.Issuer,
			Subject: id.Subject,
		})
	}
	return out
}
