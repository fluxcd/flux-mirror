// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/fluxcd/pkg/auth/utils"
	"github.com/fluxcd/pkg/auth/utils/cijwt"

	"github.com/fluxcd/flux-mirror/internal/config"
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
func SelectAuthHosts(cfg *config.Config, filter []string) ([]config.AuthHost, error) {
	if cfg.Auth == nil || len(cfg.Auth.Hosts) == 0 {
		return nil, fmt.Errorf("config has no auth.hosts")
	}
	if len(filter) == 0 {
		return cfg.Auth.Hosts, nil
	}
	byHost := make(map[string]config.AuthHost, len(cfg.Auth.Hosts))
	for _, h := range cfg.Auth.Hosts {
		byHost[h.Host] = h
	}
	out := make([]config.AuthHost, 0, len(filter))
	for _, name := range filter {
		h, ok := byHost[name]
		if !ok {
			return nil, fmt.Errorf("host %q not found in auth.hosts", name)
		}
		out = append(out, h)
	}
	return out, nil
}

// ResolveHostAuth resolves the credential for any auth host:
//   - a provider host yields the cloud registry username/password;
//   - a credential host with a username yields that username and the
//     minted/static credential as the password;
//   - a credential host without a username yields the credential as a bearer
//     RegistryToken.
func ResolveHostAuth(ctx context.Context, h config.AuthHost) (HostAuth, error) {
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
	if h.Credential.Username != "" {
		return HostAuth{Username: h.Credential.Username, Password: cred}, nil
	}
	return HostAuth{RegistryToken: cred}, nil
}

// pkgAuthProviderName maps a flux-mirror registry provider (ecr/acr/gar) to the
// pkg/auth provider name (aws/azure/gcp).
func pkgAuthProviderName(provider string) (string, error) {
	switch provider {
	case config.RegistryProviderECR:
		return "aws", nil
	case config.RegistryProviderACR:
		return "azure", nil
	case config.RegistryProviderGAR:
		return "gcp", nil
	default:
		return "", fmt.Errorf("unknown registry provider %q", provider)
	}
}

// providerAuthenticator obtains an authn.Authenticator for a provider host from
// the cloud provider's ambient workload identity, the same way the
// `flux push artifact` family does.
func providerAuthenticator(ctx context.Context, h config.AuthHost) (authn.Authenticator, error) {
	name, err := pkgAuthProviderName(h.Provider)
	if err != nil {
		return nil, err
	}
	a, err := utils.GetArtifactRegistryCredentials(ctx, name, h.Host)
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
	host config.AuthHost
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
	return &authn.AuthConfig{Username: m.host.Credential.Username, Password: cred}, nil
}

// BuildKeychain returns a keychain that serves the hosts which authenticate
// through the standard registry auth challenge — cloud provider hosts and
// credential hosts with a username — over the ambient Docker config. Provider
// credentials are pre-fetched; username credentials are minted on demand.
// Returns nil when no such host exists, so callers keep the default keychain.
func BuildKeychain(ctx context.Context, auth *config.Auth) (authn.Keychain, error) {
	if auth == nil {
		return nil, nil
	}
	auths := make(map[string]authn.Authenticator)
	for _, h := range auth.Hosts {
		switch {
		case h.Provider != "":
			a, err := providerAuthenticator(ctx, h)
			if err != nil {
				return nil, err
			}
			auths[h.Host] = a
		case h.Credential != nil && h.Credential.Username != "":
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
func NeedsCredentialTransport(auth *config.Auth) bool {
	if auth == nil {
		return false
	}
	for _, h := range auth.Hosts {
		if h.Credential != nil && h.Credential.Username == "" {
			return true
		}
	}
	return false
}

// NewCredentialTransport wraps inner with a cijwt transport that stamps the
// per-host JWT credential on each request. Provider hosts are skipped (they
// authenticate through the keychain). Call only when HasCredentialHosts is true.
func NewCredentialTransport(inner http.RoundTripper, auth *config.Auth) (http.RoundTripper, error) {
	opts, err := JWTTransportOptions(inner, auth)
	if err != nil {
		return nil, err
	}
	return cijwt.NewTransport(opts...)
}

// JWTTransportOptions turns the validated auth.hosts config into cijwt options,
// reading FromEnv environment variables and JWKPath files at this point and
// wiring each Provider to its token-minting closure. FromPath is wired straight
// to cijwt, which re-reads the file on every request. It assumes the config has
// passed validation, so it only reports errors that need runtime state: an
// unset env var or an unreadable key file.
func JWTTransportOptions(inner http.RoundTripper, auth *config.Auth) ([]cijwt.Option, error) {
	opts := []cijwt.Option{cijwt.WithInner(inner)}

	for _, h := range auth.Hosts {
		if h.Credential == nil || h.Credential.Username != "" {
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
		case j.FromEnv != "":
			token := os.Getenv(j.FromEnv)
			if token == "" {
				return nil, fmt.Errorf("auth host %q: environment variable %q is not set or empty", h.Host, j.FromEnv)
			}
			opts = append(opts, cijwt.WithHostToken(h.Host, token))
		case j.FromPath != "":
			opts = append(opts, cijwt.WithHostTokenFile(h.Host, j.FromPath))
		case j.JWKPath != "":
			jwk, err := jwkio.ReadPrivateJWK(j.JWKPath)
			if err != nil {
				return nil, fmt.Errorf("auth host %q: read jwkPath: %w", h.Host, err)
			}
			if j.Exp == nil {
				// Default: cijwt signs a fresh 60s token per request.
				opts = append(opts, cijwt.WithHostJWK(h.Host, jwk, j.Iss, aud, j.Sub))
				break
			}
			// Custom exp: sign it ourselves and let cijwt cache per its exp.
			fn, err := jwkTokenFunc(jwk, j.Iss, j.Sub, aud, j.Exp.Duration)
			if err != nil {
				return nil, fmt.Errorf("auth host %q: parse jwkPath: %w", h.Host, err)
			}
			opts = append(opts, cijwt.WithHostTokenFunc(h.Host, fn))
		}
	}

	return opts, nil
}
