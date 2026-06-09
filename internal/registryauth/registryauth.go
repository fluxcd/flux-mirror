// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/fluxcd/pkg/auth"
	"github.com/fluxcd/pkg/auth/azure"
	"github.com/fluxcd/pkg/auth/utils"
	"github.com/fluxcd/pkg/auth/utils/cijwt"

	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta1"
	"github.com/fluxcd/flux-mirror/internal/jwkio"
)

// HostAuth is a resolved credential for a registry host. Either RegistryToken is
// set (a bearer token, for credential hosts without a username), or Username and
// Password are set (provider hosts, and credential hosts with a username).
type HostAuth struct {
	Username      string
	Password      string
	RegistryToken string
}

// SelectAuthHosts returns the auth hosts to act on: the ones named in filter
// (erroring on any not present), or all configured hosts when filter is empty.
func SelectAuthHosts(cfg *apiv1.Config, filter []string) ([]apiv1.RegistryHost, error) {
	if len(cfg.Hosts) == 0 {
		return nil, fmt.Errorf("config has no hosts")
	}
	if len(filter) == 0 {
		return cfg.Hosts, nil
	}
	byHost := make(map[string]apiv1.RegistryHost, len(cfg.Hosts))
	for _, h := range cfg.Hosts {
		byHost[h.Host] = h
	}
	out := make([]apiv1.RegistryHost, 0, len(filter))
	for _, name := range filter {
		h, ok := byHost[name]
		if !ok {
			return nil, fmt.Errorf("host %q not found in hosts", name)
		}
		out = append(out, h)
	}
	return out, nil
}

// HasCredential reports whether the host yields a registry credential — i.e. it
// sets a provider or a credential. A TLS-only host (only tls configured) does
// not, so the login/secret commands skip it.
func HasCredential(h apiv1.RegistryHost) bool {
	return h.Provider != "" || h.Credential != nil
}

// ResolveHostAuth resolves the credential for any auth host:
//   - a provider host yields the cloud registry username/password;
//   - a credential host with a username yields that username and the
//     minted/static credential as the password;
//   - a credential host without a username yields the credential as a bearer
//     RegistryToken.
//
// It errors on a host that configures neither (a TLS-only host); callers that
// iterate all hosts should skip those via HasCredential first.
func ResolveHostAuth(ctx context.Context, h apiv1.RegistryHost) (HostAuth, error) {
	if !HasCredential(h) {
		return HostAuth{}, fmt.Errorf("host %q has no credential or provider configured", h.Host)
	}
	if h.Provider != "" {
		a, err := providerAuthenticator(ctx, h)
		if err != nil {
			return HostAuth{}, err
		}
		ac, err := a.Authorization()
		if err != nil {
			return HostAuth{}, fmt.Errorf("authorization for %q: %w", h.Host, err)
		}
		return HostAuth{Username: ac.Username, Password: ac.Password}, nil
	}
	cred, err := resolveCredential(ctx, h)
	if err != nil {
		return HostAuth{}, err
	}
	if h.Username != "" {
		return HostAuth{Username: h.Username, Password: cred}, nil
	}
	return HostAuth{RegistryToken: cred}, nil
}

// pkgAuthProviderName maps a flux-mirror registry provider (ecr/acr/gar) to the
// pkg/auth provider name (aws/azure/gcp).
func pkgAuthProviderName(provider string) (string, error) {
	switch provider {
	case apiv1.RegistryProviderECR:
		return "aws", nil
	case apiv1.RegistryProviderACR:
		return "azure", nil
	case apiv1.RegistryProviderGAR:
		return "gcp", nil
	default:
		return "", fmt.Errorf("unknown registry provider %q", provider)
	}
}

// providerAuthenticator obtains an authn.Authenticator for a provider host from
// the cloud provider's ambient workload identity, the same way the
// `flux push artifact` family does.
func providerAuthenticator(ctx context.Context, h apiv1.RegistryHost) (authn.Authenticator, error) {
	name, err := pkgAuthProviderName(h.Provider)
	if err != nil {
		return nil, err
	}
	var opts []auth.Option
	if name == azure.ProviderName {
		// ACR token exchange shells out to fetch an ARM access token; without
		// this the azure provider cannot mint registry credentials.
		opts = append(opts, auth.WithAllowShellOut())
	}
	a, err := utils.GetArtifactRegistryCredentials(ctx, name, h.Host, opts...)
	if err != nil {
		return nil, fmt.Errorf("get %s registry credentials for %q: %w", h.Provider, h.Host, err)
	}
	return a, nil
}

// providerKeychain serves pre-fetched cloud registry credentials for provider
// hosts and delegates everything else to an inner keychain.
type providerKeychain struct {
	auths map[string]authn.Authenticator
	inner authn.Keychain
}

// Resolve implements authn.Keychain.
func (k providerKeychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	if a, ok := k.auths[res.RegistryStr()]; ok {
		return a, nil
	}
	return k.inner.Resolve(res)
}

// mintingAuthenticator resolves a credential host's username/password on each
// call, so go-containerregistry re-mints (e.g. a fresh jwkPath JWT) whenever it
// refreshes the registry token. Used for credential hosts that set a username.
type mintingAuthenticator struct {
	host apiv1.RegistryHost
}

// Authorization implements authn.Authenticator.
func (m mintingAuthenticator) Authorization() (*authn.AuthConfig, error) {
	return m.AuthorizationContext(context.Background())
}

// AuthorizationContext implements authn.ContextAuthenticator.
func (m mintingAuthenticator) AuthorizationContext(ctx context.Context) (*authn.AuthConfig, error) {
	cred, err := resolveCredential(ctx, m.host)
	if err != nil {
		return nil, err
	}
	return &authn.AuthConfig{Username: m.host.Username, Password: cred}, nil
}

// BuildKeychain returns a keychain that serves the hosts which authenticate
// through the standard registry auth challenge — cloud provider hosts and
// credential hosts with a username — over the ambient Docker config. Provider
// credentials are pre-fetched; username credentials are minted on demand.
// Returns nil when no such host exists, so callers keep the default keychain.
func BuildKeychain(ctx context.Context, hosts []apiv1.RegistryHost) (authn.Keychain, error) {
	auths := make(map[string]authn.Authenticator)
	for _, h := range hosts {
		switch {
		case h.Provider != "":
			a, err := providerAuthenticator(ctx, h)
			if err != nil {
				return nil, err
			}
			auths[h.Host] = a
		case h.Credential != nil && h.Username != "":
			auths[h.Host] = mintingAuthenticator{host: h}
		}
	}
	if len(auths) == 0 {
		return nil, nil
	}
	return providerKeychain{auths: auths, inner: authn.DefaultKeychain}, nil
}

// NeedsCredentialTransport reports whether the cijwt bearer-stamp transport is
// needed: true when any credential host has no username (a bearer token stamped
// directly). Credential hosts with a username and provider hosts go through the
// keychain instead.
func NeedsCredentialTransport(hosts []apiv1.RegistryHost) bool {
	for _, h := range hosts {
		if h.Credential != nil && h.Username == "" {
			return true
		}
	}
	return false
}

// NewCredentialTransport wraps inner with a cijwt transport that stamps the
// per-host JWT credential on each request. Provider hosts are skipped (they
// authenticate through the keychain). Call only when HasCredentialHosts is true.
func NewCredentialTransport(inner http.RoundTripper, hosts []apiv1.RegistryHost) (http.RoundTripper, error) {
	opts, err := JWTTransportOptions(inner, hosts)
	if err != nil {
		return nil, err
	}
	return cijwt.NewTransport(opts...)
}

// readJWK returns the private JWK for a jwkPath or jwkValue credential. Exactly one of the two is set by the time this is called (enforced by validation).
func readJWK(j *apiv1.RegistryCredential) (string, error) {
	if j.JWKValue != "" {
		jwk, err := jwkio.ReadPrivateJWKFromBytes([]byte(j.JWKValue))
		if err != nil {
			return "", fmt.Errorf("read jwkValue: %w", err)
		}
		return jwk, nil
	}
	jwk, err := jwkio.ReadPrivateJWK(j.JWKPath)
	if err != nil {
		return "", fmt.Errorf("read jwkPath: %w", err)
	}
	return jwk, nil
}

// JWTTransportOptions turns the validated hosts config into cijwt options,
// reading Value/JWKValue fields and JWKPath files at this point and
// wiring each Provider to its token-minting closure. FromPath is wired straight
// to cijwt, which re-reads the file on every request. It assumes the config has
// passed validation, so it only reports errors that need runtime state: an unreadable key file.
func JWTTransportOptions(inner http.RoundTripper, hosts []apiv1.RegistryHost) ([]cijwt.Option, error) {
	opts := []cijwt.Option{cijwt.WithInner(inner)}

	for _, h := range hosts {
		if h.Credential == nil || h.Username != "" {
			// Provider hosts and username credentials authenticate via the
			// keychain (standard challenge), not the cijwt bearer-stamp.
			continue
		}
		j := h.Credential
		aud := h.EffectiveAud()
		switch {
		case j.Provider != "":
			fn, err := providerTokenFunc(j.Provider, aud)
			if err != nil {
				return nil, fmt.Errorf("auth host %q: %w", h.Host, err)
			}
			opts = append(opts, cijwt.WithHostTokenFunc(h.Host, fn))
		case j.Value != "":
			opts = append(opts, cijwt.WithHostToken(h.Host, j.Value))
		case j.FromPath != "":
			opts = append(opts, cijwt.WithHostTokenFile(h.Host, j.FromPath))
		case j.JWKPath != "", j.JWKValue != "":
			jwk, err := readJWK(j)
			if err != nil {
				return nil, fmt.Errorf("auth host %q: %w", h.Host, err)
			}
			if j.Exp == nil {
				// Default: cijwt signs a fresh 60s token per request.
				opts = append(opts, cijwt.WithHostJWK(h.Host, jwk, j.Iss, aud, j.Sub))
				break
			}
			// Custom exp: sign it ourselves and let cijwt cache per its exp.
			fn, err := jwkTokenFunc(jwk, j.Iss, j.Sub, aud, j.Exp.Duration)
			if err != nil {
				return nil, fmt.Errorf("auth host %q: parse JWK: %w", h.Host, err)
			}
			opts = append(opts, cijwt.WithHostTokenFunc(h.Host, fn))
		}
	}

	return opts, nil
}
