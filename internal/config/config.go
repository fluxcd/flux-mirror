// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/fluxcd/pkg/envsubst"

	"github.com/Masterminds/semver/v3"
	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta1"
)

// Decode reads YAML from r into a Config without validating it.
func Decode(r io.Reader) (*apiv1.Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	rendered, err := envsubst.EvalEnv(string(data), false)
	if err != nil {
		return nil, fmt.Errorf("substitute environment variables: %w", err)
	}
	var cfg apiv1.Config
	if err := yaml.Unmarshal([]byte(rendered), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

// ResolvePaths resolves every file-path field (credential fromPath and jwkPath,
// and the tls path fields) against baseDir — the directory of the config file —
// using SecureJoin, so the result is always confined within baseDir: relative
// paths, absolute paths, "../" segments, and symlinks cannot escape it. Empty
// values are left empty. A baseDir of "" is a no-op (paths stay as written).
func ResolvePaths(c *apiv1.Config, baseDir string) error {
	if baseDir == "" {
		return nil
	}
	for i := range c.Hosts {
		h := &c.Hosts[i]
		paths := hostPathFields(h)
		for _, p := range paths {
			if *p == "" {
				continue
			}
			joined, err := securejoin.SecureJoin(baseDir, *p)
			if err != nil {
				return fmt.Errorf("hosts[%d] %q: resolve path: %w", i, h.Host, err)
			}
			*p = joined
		}
	}
	return nil
}

// hostPathFields returns pointers to the file-path string fields set on a host.
func hostPathFields(h *apiv1.RegistryHost) []*string {
	var ps []*string
	if h.Credential != nil {
		ps = append(ps, &h.Credential.FromPath, &h.Credential.JWKPath)
	}
	if h.TLS != nil {
		if h.TLS.ServerAuth != nil {
			ps = append(ps, &h.TLS.ServerAuth.FromPath)
		}
		if h.TLS.ClientAuth != nil {
			if h.TLS.ClientAuth.Certificate != nil {
				ps = append(ps, &h.TLS.ClientAuth.Certificate.FromPath)
			}
			if h.TLS.ClientAuth.Key != nil {
				ps = append(ps, &h.TLS.ClientAuth.Key.FromPath)
			}
		}
	}
	return ps
}

// Validate checks the config for semantic correctness. It does not perform any
// network operations; it only verifies that values parse and that required
// fields are present. A config must declare at least one charts or artifacts
// entry.
func Validate(c *apiv1.Config) error {
	return validate(c, true)
}

// ValidateNoEntriesOK is like Validate but permits a config with no charts or
// artifacts. Used by commands that consume only the hosts section (e.g.
// `flux-mirror login`), for which a mirror entry would be meaningless.
func ValidateNoEntriesOK(c *apiv1.Config) error {
	return validate(c, false)
}

func validate(c *apiv1.Config, requireEntries bool) error {
	if c.APIVersion != apiv1.GroupVersion.String() {
		return fmt.Errorf("apiVersion must be %q, got %q", apiv1.GroupVersion.String(), c.APIVersion)
	}
	if c.Kind != apiv1.ConfigKind {
		return fmt.Errorf("kind must be %q, got %q", apiv1.ConfigKind, c.Kind)
	}
	if requireEntries && len(c.Charts) == 0 && len(c.Artifacts) == 0 {
		return fmt.Errorf("config has no entries: at least one of 'charts' or 'artifacts' must be set")
	}
	seen := make(map[string]bool, len(c.Hosts))
	for i, h := range c.Hosts {
		if err := validateHost(h); err != nil {
			return fmt.Errorf("hosts[%d]: %w", i, err)
		}
		if seen[h.Host] {
			return fmt.Errorf("hosts[%d]: host %q is configured more than once", i, h.Host)
		}
		seen[h.Host] = true
	}
	for i, ch := range c.Charts {
		if err := validateChartEntry(ch); err != nil {
			return fmt.Errorf("charts[%d]: %w", i, err)
		}
	}
	for i, a := range c.Artifacts {
		if err := validateArtifactEntry(a); err != nil {
			return fmt.Errorf("artifacts[%d]: %w", i, err)
		}
	}
	return nil
}

func validateHost(h apiv1.RegistryHost) error {
	if strings.TrimSpace(h.Host) == "" {
		return fmt.Errorf("host is required")
	}
	provider := strings.TrimSpace(h.Provider)
	if strings.TrimSpace(h.Username) != "" && h.Credential == nil {
		return fmt.Errorf("username requires credential")
	}
	if h.Credential != nil && provider != "" {
		return fmt.Errorf("credential and provider are mutually exclusive")
	}
	if provider != "" && h.TLS != nil {
		return fmt.Errorf("provider and tls are mutually exclusive")
	}
	if h.Credential != nil {
		if err := validateCredential(*h.Credential); err != nil {
			return fmt.Errorf("credential: %w", err)
		}
	}
	if provider != "" {
		switch provider {
		case apiv1.RegistryProviderECR, apiv1.RegistryProviderACR, apiv1.RegistryProviderGAR:
		default:
			return fmt.Errorf("provider %q must be one of: %s, %s, %s",
				provider, apiv1.RegistryProviderECR, apiv1.RegistryProviderACR, apiv1.RegistryProviderGAR)
		}
	}
	if h.TLS != nil {
		if err := validateTLS(*h.TLS); err != nil {
			return fmt.Errorf("tls: %w", err)
		}
	}
	if h.MaxChunkSize < 0 {
		return fmt.Errorf("maxChunkSize must be >= 0 (0 disables chunking)")
	}
	if h.Credential == nil && provider == "" && h.TLS == nil && h.MaxChunkSize == 0 {
		return fmt.Errorf("one of credential, provider, tls, or maxChunkSize is required")
	}
	return nil
}

// countTrue counts how many of the given conditions are true. It is used by the
// "exactly one of ..." validators below.
func countTrue(conds ...bool) int {
	n := 0
	for _, c := range conds {
		if c {
			n++
		}
	}
	return n
}

// validateTLS checks the TLS settings: at least one of serverAuth or clientAuth
// must be set, and each (if set) must itself be valid.
func validateTLS(t apiv1.TLS) error {
	if t.ServerAuth == nil && t.ClientAuth == nil {
		return fmt.Errorf("one of serverAuth or clientAuth is required")
	}
	if t.ServerAuth != nil {
		if err := validateTLSServerAuth(*t.ServerAuth); err != nil {
			return fmt.Errorf("serverAuth: %w", err)
		}
	}
	if t.ClientAuth != nil {
		if err := validateTLSClientAuth(*t.ClientAuth); err != nil {
			return fmt.Errorf("clientAuth: %w", err)
		}
	}
	return nil
}

// validateTLSServerAuth checks that exactly one of the CA-bundle sources or
// spiffe is set.
func validateTLSServerAuth(s apiv1.TLSServerAuth) error {
	if countTrue(
		strings.TrimSpace(s.FromPath) != "",
		strings.TrimSpace(s.Value) != "",
		s.SPIFFE != nil,
	) != 1 {
		return fmt.Errorf("exactly one of fromPath, value, or spiffe must be set")
	}
	if s.SPIFFE != nil {
		if err := validateSPIFFE(*s.SPIFFE); err != nil {
			return fmt.Errorf("spiffe: %w", err)
		}
	}
	return nil
}

// validateTLSData checks that exactly one source is set.
func validateTLSData(d apiv1.TLSData) error {
	if countTrue(
		strings.TrimSpace(d.FromPath) != "",
		strings.TrimSpace(d.Value) != "",
	) != 1 {
		return fmt.Errorf("exactly one of fromPath or value must be set")
	}
	return nil
}

// validateTLSKey checks that exactly one source is set.
func validateTLSKey(k apiv1.TLSKey) error {
	if countTrue(
		strings.TrimSpace(k.FromPath) != "",
		strings.TrimSpace(k.Value) != "",
	) != 1 {
		return fmt.Errorf("exactly one of fromPath or value must be set")
	}
	return nil
}

// validateTLSClientAuth checks the client cert source: either provider
// (x509-svid) or a static certificate/key pair, mutually exclusive.
func validateTLSClientAuth(c apiv1.TLSClientAuth) error {
	provider := strings.TrimSpace(c.Provider)
	hasStatic := c.Certificate != nil || c.Key != nil
	if provider != "" && hasStatic {
		return fmt.Errorf("provider is mutually exclusive with certificate and key")
	}
	if provider != "" {
		if provider != apiv1.TLSClientProviderX509SVID {
			return fmt.Errorf("provider %q must be %q", provider, apiv1.TLSClientProviderX509SVID)
		}
		return nil
	}
	if c.Certificate == nil {
		return fmt.Errorf("certificate is required")
	}
	if err := validateTLSData(*c.Certificate); err != nil {
		return fmt.Errorf("certificate: %w", err)
	}
	if c.Key == nil {
		return fmt.Errorf("key is required")
	}
	if err := validateTLSKey(*c.Key); err != nil {
		return fmt.Errorf("key: %w", err)
	}
	return nil
}

// validateSPIFFE checks that exactly one authorizer is set and parses.
func validateSPIFFE(s apiv1.SPIFFETLS) error {
	serverID := strings.TrimSpace(s.ServerID)
	trustDomain := strings.TrimSpace(s.TrustDomain)

	if countTrue(serverID != "", trustDomain != "", s.AuthorizeAny) != 1 {
		return fmt.Errorf("exactly one of serverID, trustDomain, or authorizeAny must be set")
	}
	if serverID != "" {
		if _, err := spiffeid.FromString(serverID); err != nil {
			return fmt.Errorf("serverID %q is not a valid SPIFFE ID: %w", serverID, err)
		}
	}
	if trustDomain != "" && trustDomain != apiv1.TrustDomainSelf {
		if _, err := spiffeid.TrustDomainFromString(trustDomain); err != nil {
			return fmt.Errorf("trustDomain %q is not valid: %w", trustDomain, err)
		}
	}
	return nil
}

func validateCredential(j apiv1.RegistryCredential) error {
	provider := strings.TrimSpace(j.Provider)
	value := strings.TrimSpace(j.Value)
	fromPath := strings.TrimSpace(j.FromPath)
	jwkPath := strings.TrimSpace(j.JWKPath)
	jwkValue := strings.TrimSpace(j.JWKValue)

	if countTrue(provider != "", value != "", fromPath != "", jwkPath != "", jwkValue != "") != 1 {
		return fmt.Errorf("exactly one of provider, value, fromPath, jwkPath, or jwkValue must be set")
	}

	hasIss := strings.TrimSpace(j.Iss) != ""
	hasSub := strings.TrimSpace(j.Sub) != ""
	hasAud := strings.TrimSpace(j.Aud) != ""
	hasExp := j.Exp != nil

	switch {
	case jwkPath != "", jwkValue != "":
		if !hasIss {
			return fmt.Errorf("iss is required with jwkPath or jwkValue")
		}
		if !hasSub {
			return fmt.Errorf("sub is required with jwkPath or jwkValue")
		}
		if hasExp && j.Exp.Duration <= 0 {
			return fmt.Errorf("exp must be a positive duration")
		}
	case provider != "":
		switch provider {
		case apiv1.JWTProviderGitHub, apiv1.JWTProviderForgejo, apiv1.JWTProviderGCP, apiv1.JWTProviderAzure, apiv1.JWTProviderAWS, apiv1.JWTProviderJWTSVID:
		default:
			return fmt.Errorf("provider %q must be one of: %s, %s, %s, %s, %s, %s",
				provider, apiv1.JWTProviderGitHub, apiv1.JWTProviderForgejo, apiv1.JWTProviderGCP, apiv1.JWTProviderAzure, apiv1.JWTProviderAWS, apiv1.JWTProviderJWTSVID)
		}
		if hasIss || hasSub {
			return fmt.Errorf("iss and sub can only be set with jwkPath or jwkValue")
		}
		if hasExp {
			return fmt.Errorf("exp can only be set with jwkPath or jwkValue")
		}
	case value != "", fromPath != "":
		if hasIss || hasSub {
			return fmt.Errorf("iss and sub can only be set with jwkPath or jwkValue")
		}
		if hasAud {
			return fmt.Errorf("aud can only be set with jwkPath, jwkValue, or provider")
		}
		if hasExp {
			return fmt.Errorf("exp can only be set with jwkPath or jwkValue")
		}
	}
	return nil
}

func validateChartEntry(c apiv1.ChartEntry) error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if err := validateChartSource(c.Source); err != nil {
		return fmt.Errorf("source: %w", err)
	}
	if err := validateOCIURL(c.Destination); err != nil {
		return fmt.Errorf("destination: %w", err)
	}
	if _, err := semver.NewConstraint(c.EffectiveVersion()); err != nil {
		return fmt.Errorf("version %q is not a valid semver constraint: %w", c.EffectiveVersion(), err)
	}
	if c.Limit != nil && *c.Limit < 0 {
		return fmt.Errorf("limit must be >= 0 (0 = unlimited)")
	}
	return nil
}

func validateArtifactEntry(a apiv1.ArtifactEntry) error {
	if _, err := name.NewRepository(a.Source); err != nil {
		return fmt.Errorf("source %q is not a valid OCI repository: %w", a.Source, err)
	}
	if _, err := name.NewRepository(a.Destination); err != nil {
		return fmt.Errorf("destination %q is not a valid OCI repository: %w", a.Destination, err)
	}
	if err := validateSelector(a.Selector); err != nil {
		return fmt.Errorf("selector: %w", err)
	}
	if a.Verify != nil {
		if err := validateVerification(*a.Verify); err != nil {
			return fmt.Errorf("verify: %w", err)
		}
	}
	return nil
}

func validateVerification(v apiv1.ArtifactVerification) error {
	switch strings.TrimSpace(v.Provider) {
	case apiv1.VerifyProviderCosign:
	default:
		return fmt.Errorf("provider %q must be %q", v.Provider, apiv1.VerifyProviderCosign)
	}
	if len(v.MatchOIDCIdentity) == 0 {
		return fmt.Errorf("matchOIDCIdentity must contain at least one identity")
	}
	if v.MinAge != nil && v.MinAge.Duration < 0 {
		return fmt.Errorf("minAge must be >= 0")
	}
	for i, id := range v.MatchOIDCIdentity {
		if strings.TrimSpace(id.Issuer) == "" {
			return fmt.Errorf("matchOIDCIdentity[%d].issuer is required", i)
		}
		if strings.TrimSpace(id.Subject) == "" {
			return fmt.Errorf("matchOIDCIdentity[%d].subject is required", i)
		}
		if _, err := regexp.Compile(id.Subject); err != nil {
			return fmt.Errorf("matchOIDCIdentity[%d].subject %q does not compile: %w", i, id.Subject, err)
		}
	}
	return nil
}

func validateSelector(s apiv1.Selector) error {
	if s.Regex != nil {
		if strings.TrimSpace(s.Regex.Pattern) == "" {
			return fmt.Errorf("regex.pattern is required when regex is set")
		}
		if _, err := regexp.Compile(s.Regex.Pattern); err != nil {
			return fmt.Errorf("regex.pattern %q does not compile: %w", s.Regex.Pattern, err)
		}
	}
	if strings.TrimSpace(s.Semver) != "" {
		if _, err := semver.NewConstraint(s.Semver); err != nil {
			return fmt.Errorf("semver %q is not a valid constraint: %w", s.Semver, err)
		}
	}
	switch s.EffectiveSortBy() {
	case apiv1.SortBySemver, apiv1.SortByAlphabetical, apiv1.SortByNumerical:
	default:
		return fmt.Errorf("sortBy %q must be one of: semver, alphabetical, numerical", s.SortBy)
	}
	if s.Limit != nil && *s.Limit < 0 {
		return fmt.Errorf("limit must be >= 0 (0 = unlimited)")
	}
	return nil
}

func validateChartSource(s string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("source is required")
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("scheme %q must be one of: http, https "+
			"(use 'artifacts' to mirror an OCI Helm chart to another OCI repository)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("URL %q is missing a host", s)
	}
	return nil
}

func validateOCIURL(s string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("destination is required")
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "oci" {
		return fmt.Errorf("scheme must be oci://, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("URL %q is missing a host", s)
	}
	return nil
}
