// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"text/template"

	"github.com/google/go-containerregistry/pkg/authn"

	cryptov1 "github.com/fluxcd/pkg/apis/crypto/v1"
	"github.com/fluxcd/pkg/auth"
	"github.com/fluxcd/pkg/auth/azure"
	"github.com/fluxcd/pkg/auth/utils"
	"github.com/fluxcd/pkg/auth/utils/cijwt"
	"github.com/fluxcd/pkg/cache"

	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta2"
	"github.com/fluxcd/flux-mirror/internal/envelope"
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

// HostsForDirection returns the hosts that apply to a side of a mirror:
// direction source includes entries marked source or both, and direction
// destination includes entries marked destination or both.
func HostsForDirection(hosts []apiv1.RegistryHost, direction string) []apiv1.RegistryHost {
	out := make([]apiv1.RegistryHost, 0, len(hosts))
	for _, h := range hosts {
		d := h.EffectiveDirection()
		if d == apiv1.DirectionBoth || d == direction {
			out = append(out, h)
		}
	}
	return out
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
	return h.IsCloudProvider() || h.Credential != nil
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
	if h.IsCloudProvider() {
		a, err := providerAuthenticator(ctx, h, nil)
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
// `flux push artifact` family does. When the host sets a credential, the cloud
// provider instead exchanges the credential identity for its access credential.
func providerAuthenticator(ctx context.Context, h apiv1.RegistryHost, tokenCache *cache.TokenCache) (authn.Authenticator, error) {
	name, err := pkgAuthProviderName(h.Provider)
	if err != nil {
		return nil, err
	}
	opts := providerOptions(h)
	if name == azure.ProviderName {
		// ACR token exchange shells out to fetch an ARM access token; without
		// this the azure provider cannot mint registry credentials.
		opts = append(opts, auth.WithAllowShellOut())
	}
	if tokenCache != nil {
		// Cache the provider access credential and registry token. Otherwise
		// pkg/auth mints new credentials on every registry request, which for
		// Actions OIDC means a new GitHub ID token and STS
		// AssumeRoleWithWebIdentity call per request.
		opts = append(opts, auth.WithCache(*tokenCache))
	}
	a, err := utils.GetArtifactRegistryCredentials(ctx, name, h.Host, opts...)
	if err != nil {
		return nil, fmt.Errorf("get %s registry credentials for %q: %w", h.Provider, h.Host, err)
	}
	return a, nil
}

// providerIdentity is the workload identity passed to the auth package when
// exchanging a credential for cloud registry access. It names the AWS role
// session and the generic provider identity.
const providerIdentity = "flux-mirror"

// cloudCredentialProvider maps a flux-mirror credential provider to the auth
// package's credential provider. Providers that are not renamed map through
// unchanged.
func cloudCredentialProvider(provider string) string {
	switch provider {
	case apiv1.JWTProviderGitHub, apiv1.JWTProviderForgejo:
		return cryptov1.CredentialProviderActionsOIDC
	default:
		return provider
	}
}

// providerOptions maps a cloud provider host to auth options. Provider-specific
// settings come from the matching ecr/acr/gar block. A credential selects the
// ID token source used for the exchange; explicitly configured audiences are
// forwarded, otherwise the cloud provider applies its own default audience,
// which is what AWS and Azure expect for federation.
func providerOptions(h apiv1.RegistryHost) []auth.Option {
	var opts []auth.Option
	switch h.Provider {
	case apiv1.RegistryProviderECR:
		if h.ECR != nil {
			opts = append(opts, auth.WithAWSRoleARN(strings.TrimSpace(h.ECR.RoleARN)))
			if endpoint := strings.TrimSpace(h.ECR.STSEndpoint); endpoint != "" {
				opts = append(opts, auth.WithAWSSTSEndpoint(endpoint))
			}
		}
	case apiv1.RegistryProviderACR:
		if h.ACR != nil {
			opts = append(opts,
				auth.WithAzureTenantID(strings.TrimSpace(h.ACR.TenantID)),
				auth.WithAzureClientID(strings.TrimSpace(h.ACR.ClientID)))
		}
	case apiv1.RegistryProviderGAR:
		if h.GAR != nil {
			opts = append(opts, auth.WithGCPWorkloadIdentityProvider(strings.TrimSpace(h.GAR.WorkloadIdentityProvider)))
			if sa := strings.TrimSpace(h.GAR.GCPServiceAccount); sa != "" {
				opts = append(opts, auth.WithGCPServiceAccount(sa))
			}
		}
	}
	if h.Credential == nil {
		return opts
	}
	opts = append(opts,
		auth.WithCredentialProvider(cloudCredentialProvider(h.Credential.Provider)),
		auth.WithIdentity(providerIdentity))
	audiences := make([]string, 0, len(h.Credential.Audiences))
	for _, raw := range h.Credential.Audiences {
		if aud := strings.TrimSpace(raw); aud != "" {
			audiences = append(audiences, aud)
		}
	}
	if len(audiences) > 0 {
		opts = append(opts, auth.WithCredentialAudiences(audiences...))
	}
	return opts
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
	return &authn.AuthConfig{Username: m.host.Credential.Username, Password: cred}, nil
}

// BuildKeychain returns a keychain that serves the hosts which authenticate
// through the standard registry auth challenge — cloud provider hosts and
// credential hosts with a username — over the ambient Docker config. Provider
// credentials are pre-fetched; username credentials are minted on demand.
// Returns nil when no such host exists, so callers keep the default keychain.
func BuildKeychain(ctx context.Context, hosts []apiv1.RegistryHost, tokenCache *cache.TokenCache) (authn.Keychain, error) {
	auths := make(map[string]authn.Authenticator)
	for _, h := range hosts {
		switch {
		case h.IsCloudProvider():
			a, err := providerAuthenticator(ctx, h, tokenCache)
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

// usesCredentialTransport reports whether the cijwt bearer-stamp transport
// manages this host: a non-cloud credential host without a username (its
// credential is stamped directly as a Bearer token). Cloud provider hosts and
// username credentials authenticate through the keychain instead.
func usesCredentialTransport(h apiv1.RegistryHost) bool {
	return !h.IsCloudProvider() && h.Credential != nil && h.Credential.Username == ""
}

// NeedsCredentialTransport reports whether the cijwt bearer-stamp transport is
// needed: true when any credential host has no username (a bearer token stamped
// directly). Credential hosts with a username and provider hosts go through the
// keychain instead.
func NeedsCredentialTransport(hosts []apiv1.RegistryHost) bool {
	for _, h := range hosts {
		if usesCredentialTransport(h) {
			return true
		}
	}
	return false
}

// NewCredentialTransport wraps inner with a cijwt transport that stamps the
// per-host JWT credential on each request, enveloping it first when the host
// configures an envelope. Provider hosts are skipped (they authenticate through
// the keychain). Call only when NeedsCredentialTransport is true.
func NewCredentialTransport(inner http.RoundTripper, hosts []apiv1.RegistryHost) (http.RoundTripper, error) {
	et, err := newEnvelopeTransport(inner, hosts)
	if err != nil {
		return nil, err
	}
	opts, err := JWTTransportOptions(et, hosts)
	if err != nil {
		return nil, err
	}
	return cijwt.NewTransport(opts...)
}

// envelopeTransport rewrites the Authorization bearer token stamped by the
// cijwt transport into the host's configured envelope. It sits between the
// cijwt transport and the base transport, so cijwt sees the raw token — and can
// still parse its exp claim to schedule re-minting — while the registry
// receives the enveloped string. It is built only for hosts without a username
// (the cijwt-managed bearer hosts); a username/password host's Bearer registry
// token, and any request without an Authorization bearer header, pass through
// untouched.
type envelopeTransport struct {
	inner     http.RoundTripper
	envelopes map[string]*template.Template
}

// newEnvelopeTransport builds an envelope transport for the credential hosts
// that configure an envelope. It parses each template up front, so a malformed
// envelope fails the run before any request. Only hosts without a username are
// included: they are the ones the cijwt transport stamps a Bearer token for. A
// username credential host authenticates through the keychain, and after the
// auth challenge it also carries a Bearer registry token — transforming that
// token would corrupt it. Its envelope is applied to the password instead, by
// resolveCredential.
func newEnvelopeTransport(inner http.RoundTripper, hosts []apiv1.RegistryHost) (*envelopeTransport, error) {
	envelopes := make(map[string]*template.Template)
	for _, h := range hosts {
		if !usesCredentialTransport(h) || h.Credential.Envelope == "" {
			continue
		}
		t, err := envelope.Parse(h.Credential.Envelope)
		if err != nil {
			return nil, fmt.Errorf("host %q: %w", h.Host, err)
		}
		envelopes[h.Host] = t
	}
	return &envelopeTransport{inner: inner, envelopes: envelopes}, nil
}

// RoundTrip implements http.RoundTripper.
func (t *envelopeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tmpl, ok := t.envelopes[req.URL.Host]
	if !ok {
		return t.inner.RoundTrip(req)
	}
	const prefix = "Bearer "
	header := req.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return t.inner.RoundTrip(req)
	}
	enveloped, err := envelope.Render(tmpl, strings.TrimPrefix(header, prefix))
	if err != nil {
		return nil, fmt.Errorf("host %q: %w", req.URL.Host, err)
	}
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", prefix+enveloped)
	return t.inner.RoundTrip(cloned)
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
		if !usesCredentialTransport(h) {
			// Cloud provider hosts and username credentials authenticate via the
			// keychain (standard challenge), not the cijwt bearer-stamp.
			continue
		}
		j := h.Credential
		audiences := h.EffectiveAudiences()
		switch {
		case j.Provider != "":
			fn, err := providerTokenFunc(j.Provider, audiences, len(j.Audiences) > 0)
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
			if j.Expiration == nil {
				// Default: cijwt signs a fresh 60s token per request.
				opts = append(opts, cijwt.WithHostJWK(h.Host, jwk, j.Issuer, audiences, j.Subject))
				break
			}
			// Custom expiration: sign it ourselves and let cijwt cache per its exp.
			fn, err := jwkTokenFunc(jwk, j.Issuer, j.Subject, audiences, j.Expiration.Duration)
			if err != nil {
				return nil, fmt.Errorf("auth host %q: parse JWK: %w", h.Host, err)
			}
			opts = append(opts, cijwt.WithHostTokenFunc(h.Host, fn))
		}
	}

	return opts, nil
}
