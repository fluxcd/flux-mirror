// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package v1beta2

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1beta1 "github.com/fluxcd/flux-mirror/api/v1beta1"
)

// FromV1Beta1 converts a v1beta1 Config to the current v1beta2 shape. It
// renames the credential claims (aud->audiences, iss->issuer, sub->subject,
// exp->expiration), moves hosts[].username under credential, renames the SPIFFE
// credential provider to spiffe, and converts the v1beta1 tls shapes that map
// exactly: inline certificate values and SPIFFE mTLS with an X.509-SVID client
// certificate. Per-file TLS paths, mixed inline/SPIFFE halves, one-sided
// SPIFFE, and authorizeAny have no v1beta2 equivalent and return an error.
// Callers gate this behind a deprecation warning. The declared Kind is carried
// over unchanged so validation still rejects a v1beta1 document that names the
// wrong kind.
func FromV1Beta1(in *apiv1beta1.Config) (*Config, error) {
	if in == nil {
		return nil, nil
	}
	out := &Config{
		// Preserve the declared kind so Validate still rejects a mismatched one;
		// Decode returns the source apiVersion separately for the warning.
		TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: in.Kind},
	}
	for i, h := range in.Hosts {
		converted, err := registryHostFromV1Beta1(h)
		if err != nil {
			return nil, fmt.Errorf("hosts[%d]: %w", i, err)
		}
		out.Hosts = append(out.Hosts, converted)
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
	return out, nil
}

func registryHostFromV1Beta1(in apiv1beta1.RegistryHost) (RegistryHost, error) {
	if strings.TrimSpace(in.Username) != "" && in.Credential == nil {
		// v1beta1 validation rejected this; do not silently drop the username.
		return RegistryHost{}, fmt.Errorf("username requires credential")
	}
	tls, err := tlsFromV1Beta1(in.TLS)
	if err != nil {
		return RegistryHost{}, err
	}
	out := RegistryHost{
		Host:         in.Host,
		Provider:     in.Provider,
		TLS:          tls,
		MaxChunkSize: in.MaxChunkSize,
	}
	if in.Credential != nil {
		out.Credential = credentialFromV1Beta1(*in.Credential, in.Username)
	}
	return out, nil
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
	if strings.TrimSpace(in.Aud) != "" {
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

// tlsFromV1Beta1 converts the v1beta1 tls shapes that map exactly onto
// v1beta2: inline PEM values and SPIFFE mutual TLS with an X.509-SVID client
// certificate. Per-file paths, mixed inline/SPIFFE halves, one-sided SPIFFE,
// and authorizeAny cannot be represented and return migration guidance.
func tlsFromV1Beta1(in *apiv1beta1.TLS) (*TLS, error) {
	if in == nil {
		return nil, nil
	}

	if in.ServerAuth != nil && in.ServerAuth.SPIFFE != nil {
		if strings.TrimSpace(in.ServerAuth.FromPath) != "" || strings.TrimSpace(in.ServerAuth.Value) != "" {
			return nil, fmt.Errorf("serverAuth.spiffe cannot be combined with fromPath or value")
		}
		provider := ""
		if in.ClientAuth != nil {
			provider = strings.TrimSpace(in.ClientAuth.Provider)
		}
		if provider != apiv1beta1.TLSClientProviderX509SVID {
			return nil, fmt.Errorf("SPIFFE serverAuth requires clientAuth.provider x509-svid; " +
				"use tls.spiffe.serverIDs")
		}
		if in.ClientAuth.Certificate != nil || in.ClientAuth.Key != nil {
			return nil, fmt.Errorf("static clientAuth cannot be combined with SPIFFE serverAuth")
		}
		serverIDs, err := serverIDsFromV1Beta1(in.ServerAuth.SPIFFE)
		if err != nil {
			return nil, err
		}
		return &TLS{SPIFFE: &SPIFFETLS{ServerIDs: serverIDs}}, nil
	}

	if in.ServerAuth != nil && strings.TrimSpace(in.ServerAuth.FromPath) != "" {
		return nil, fmt.Errorf("serverAuth.fromPath cannot be converted; use tls.path")
	}
	if in.ClientAuth != nil {
		if provider := strings.TrimSpace(in.ClientAuth.Provider); provider != "" {
			return nil, fmt.Errorf("clientAuth.provider %q cannot be converted without serverAuth.spiffe", provider)
		}
		if in.ClientAuth.Certificate != nil && strings.TrimSpace(in.ClientAuth.Certificate.FromPath) != "" {
			return nil, fmt.Errorf("clientAuth.certificate.fromPath cannot be converted; use tls.path")
		}
		if in.ClientAuth.Key != nil && strings.TrimSpace(in.ClientAuth.Key.FromPath) != "" {
			return nil, fmt.Errorf("clientAuth.key.fromPath cannot be converted; use tls.path")
		}
	}

	out := &TLS{}
	if in.ServerAuth != nil {
		out.CACert = strings.TrimSpace(in.ServerAuth.Value)
	}
	if in.ClientAuth != nil {
		if in.ClientAuth.Certificate != nil {
			out.TLSCert = strings.TrimSpace(in.ClientAuth.Certificate.Value)
		}
		if in.ClientAuth.Key != nil {
			out.TLSKey = strings.TrimSpace(in.ClientAuth.Key.Value)
		}
	}
	if out.CACert == "" && out.TLSCert == "" && out.TLSKey == "" {
		return nil, fmt.Errorf("v1beta1 tls has no inline values to convert; use tls.path or tls.spiffe")
	}
	return out, nil
}

// serverIDsFromV1Beta1 maps the v1beta1 single-authorizer SPIFFE block to the
// v1beta2 serverIDs list accepted by the Flux crypto API.
func serverIDsFromV1Beta1(in *apiv1beta1.SPIFFETLS) ([]string, error) {
	serverID := strings.TrimSpace(in.ServerID)
	trustDomain := strings.TrimSpace(in.TrustDomain)

	n := 0
	if serverID != "" {
		n++
	}
	if trustDomain != "" {
		n++
	}
	if in.AuthorizeAny {
		n++
	}
	if n != 1 {
		return nil, fmt.Errorf("serverAuth.spiffe must set exactly one of serverID, trustDomain, or authorizeAny")
	}
	if in.AuthorizeAny {
		return nil, fmt.Errorf("authorizeAny cannot be converted; set serverID or trustDomain")
	}
	if serverID != "" {
		return []string{serverID}, nil
	}
	if trustDomain == apiv1beta1.TrustDomainSelf {
		return []string{"spiffe://self"}, nil
	}
	return []string{"spiffe://" + trustDomain}, nil
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
