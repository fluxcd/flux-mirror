// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"io"
	"net/url"
	"regexp"

	"github.com/fluxcd/pkg/envsubst"

	"github.com/Masterminds/semver/v3"
	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"sigs.k8s.io/yaml"

	apiv1beta1 "github.com/fluxcd/flux-mirror/api/v1beta1"
	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta2"
	"github.com/fluxcd/flux-mirror/internal/envelope"
)

// DeprecatedV1Beta1APIVersion is the deprecated v1beta1 apiVersion. A config
// that declares it still loads, but is converted to v1beta2 and a migration
// warning is printed.
var DeprecatedV1Beta1APIVersion = apiv1beta1.GroupVersion.String()

// V1Beta1MigrationWarning describes how a v1beta1 config maps onto v1beta2. It
// is printed as a warning when a v1beta1 config is loaded, and doubles as the
// migration guide for the breaking rename.
const V1Beta1MigrationWarning = `apiVersion "mirror.plugin.fluxcd.io/v1beta1" is deprecated and will be removed in a future release; the config is being migrated to "mirror.plugin.fluxcd.io/v1beta2" in memory.`

// V1Beta1Migration describes how to move a v1beta1 config to v1beta2.
const V1Beta1Migration = `v1beta2 is a breaking change:
  - credential.type is required (set type: jwt)
  - credential.aud -> credential.audiences
  - credential.iss -> credential.issuer
  - credential.sub -> credential.subject
  - credential.exp -> credential.expiration
  - hosts[].username -> hosts[].credential.username
  - credential provider jwt-svid -> spiffe
  - tls clientAuth/serverAuth provider x509-svid -> spiffe
See docs/config.md for the full specification.`

// DeprecationWarning returns the migration warning to print for a decoded
// config, naming the declared apiVersion. It is empty for the current version.
// Decode already performs the v1beta1 -> v1beta2 conversion, so callers only
// surface the warning; they do not act on the version.
func DeprecationWarning(apiVersion string) string {
	if apiVersion != DeprecatedV1Beta1APIVersion {
		return ""
	}
	return V1Beta1MigrationWarning + "\n" + V1Beta1Migration
}

// Decode reads YAML from r into a Config without validating it. A v1beta1
// document is converted to v1beta2 in memory; use DeprecationWarning on the
// returned config's apiVersion to surface the migration warning.
func Decode(r io.Reader) (*apiv1.Config, error) {
	return DecodeWithEnvSubst(r, true)
}

// DecodeWithEnvSubst reads YAML from r into a Config, optionally applying
// strict environment variable substitution before parsing.
func DecodeWithEnvSubst(r io.Reader, substitute bool) (*apiv1.Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	rendered := data
	if substitute {
		out, err := envsubst.EvalEnv(string(data), true)
		if err != nil {
			return nil, fmt.Errorf("substitute environment variables: %w", err)
		}
		rendered = []byte(out)
	}
	// Peek at apiVersion leniently to branch the right wire struct. A v1beta1
	// document uses the old field names, which the strict v1beta2 decoder would
	// reject as unknown fields before apiVersion is ever checked. A malformed
	// document yields no peeked version and falls through to the strict decoder,
	// which reports the parse error.
	apiVersion, _ := peekAPIVersion(rendered)
	switch apiVersion {
	case DeprecatedV1Beta1APIVersion:
		var old apiv1beta1.Config
		if err := yaml.UnmarshalStrict(rendered, &old); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
		cfg := apiv1.FromV1Beta1(&old)
		// Preserve the deprecated apiVersion so callers can warn; Validate
		// accepts both versions.
		cfg.APIVersion = DeprecatedV1Beta1APIVersion
		return cfg, nil
	case apiv1.GroupVersion.String():
		// Fall through to the strict current-version decode below.
	default:
		// Any other declared version (including an unknown one) is not a v1beta1
		// migration, so let the strict decoder report the unknown fields; a
		// version mismatch is reported by Validate.
	}
	var cfg apiv1.Config
	// Strict decoding rejects unknown fields (e.g. a removed or misspelled
	// key), so a config written for an older schema fails loudly instead of
	// silently dropping the field.
	if err := yaml.UnmarshalStrict(rendered, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

// peekAPIVersion reads only the apiVersion field, ignoring unknown fields so a
// v1beta1 document is not rejected by the strict current-version decoder.
func peekAPIVersion(rendered []byte) (string, error) {
	var meta struct {
		APIVersion string `json:"apiVersion"`
	}
	if err := yaml.Unmarshal(rendered, &meta); err != nil {
		return "", err
	}
	return meta.APIVersion, nil
}

// checkAPIVersion requires a known apiVersion, turning an unknown one into an
// actionable error rather than a generic mismatch.
func checkAPIVersion(v string) error {
	switch v {
	case apiv1.GroupVersion.String(), DeprecatedV1Beta1APIVersion:
		return nil
	default:
		return fmt.Errorf("apiVersion must be %q, got %q", apiv1.GroupVersion.String(), v)
	}
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
	// A v1beta1 config is converted to v1beta2 in memory by Decode, which keeps
	// the deprecated apiVersion so callers can warn; accept it here too.
	if c.APIVersion != DeprecatedV1Beta1APIVersion {
		if err := checkAPIVersion(c.APIVersion); err != nil {
			return err
		}
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
	if h.Host == "" {
		return fmt.Errorf("host is required")
	}
	// A generic provider is the default and behaves like an unset provider: it
	// composes with credential and tls, unlike a cloud provider.
	cloud := h.IsCloudProvider()
	if h.Credential != nil && cloud {
		return fmt.Errorf("credential and cloud provider are mutually exclusive")
	}
	if cloud && h.TLS != nil {
		return fmt.Errorf("cloud provider and tls are mutually exclusive")
	}
	if h.Credential != nil {
		if err := validateCredential(*h.Credential); err != nil {
			return fmt.Errorf("credential: %w", err)
		}
	}
	switch h.Provider {
	case "", apiv1.RegistryProviderGeneric, apiv1.RegistryProviderECR, apiv1.RegistryProviderACR, apiv1.RegistryProviderGAR:
	default:
		return fmt.Errorf("provider %q must be one of: %s, %s, %s, %s",
			h.Provider, apiv1.RegistryProviderGeneric, apiv1.RegistryProviderECR, apiv1.RegistryProviderACR, apiv1.RegistryProviderGAR)
	}
	if h.TLS != nil {
		if err := validateTLS(*h.TLS); err != nil {
			return fmt.Errorf("tls: %w", err)
		}
	}
	if h.MaxChunkSize < 0 {
		return fmt.Errorf("maxChunkSize must be >= 0 (0 disables chunking)")
	}
	if h.Credential == nil && !cloud && h.TLS == nil && h.MaxChunkSize == 0 {
		return fmt.Errorf("one of credential, a cloud provider, tls, or maxChunkSize is required")
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

// validateTLSServerAuth checks the server certificate source. With provider unset
// exactly one file-based CA source must be set; provider spiffe requires the
// spiffe authorizer and no file source.
func validateTLSServerAuth(s apiv1.TLSServerAuth) error {
	switch s.Provider {
	case apiv1.TLSProviderSPIFFE:
		if s.FromPath != "" || s.Value != "" {
			return fmt.Errorf("fromPath and value are not allowed with provider %q", apiv1.TLSProviderSPIFFE)
		}
		if s.SPIFFE == nil {
			return fmt.Errorf("spiffe is required with provider %q", apiv1.TLSProviderSPIFFE)
		}
		if err := validateSPIFFE(*s.SPIFFE); err != nil {
			return fmt.Errorf("spiffe: %w", err)
		}
	case "":
		if s.SPIFFE != nil {
			return fmt.Errorf("spiffe requires provider %q", apiv1.TLSProviderSPIFFE)
		}
		if countTrue(
			s.FromPath != "",
			s.Value != "",
		) != 1 {
			return fmt.Errorf("exactly one of fromPath or value must be set")
		}
	default:
		return fmt.Errorf("provider %q must be %q", s.Provider, apiv1.TLSProviderSPIFFE)
	}
	return nil
}

// validateTLSData checks that exactly one source is set.
func validateTLSData(d apiv1.TLSData) error {
	if countTrue(
		d.FromPath != "",
		d.Value != "",
	) != 1 {
		return fmt.Errorf("exactly one of fromPath or value must be set")
	}
	return nil
}

// validateTLSKey checks that exactly one source is set.
func validateTLSKey(k apiv1.TLSKey) error {
	if countTrue(
		k.FromPath != "",
		k.Value != "",
	) != 1 {
		return fmt.Errorf("exactly one of fromPath or value must be set")
	}
	return nil
}

// validateTLSClientAuth checks the client certificate source. With provider unset
// the static certificate/key pair is required; provider spiffe uses a Workload
// API X.509-SVID and rejects the static pair.
func validateTLSClientAuth(c apiv1.TLSClientAuth) error {
	switch c.Provider {
	case apiv1.TLSProviderSPIFFE:
		if c.Certificate != nil || c.Key != nil {
			return fmt.Errorf("certificate and key are not allowed with provider %q", apiv1.TLSProviderSPIFFE)
		}
	case "":
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
	default:
		return fmt.Errorf("provider %q must be %q", c.Provider, apiv1.TLSProviderSPIFFE)
	}
	return nil
}

// validateSPIFFE checks that exactly one authorizer is set and parses.
func validateSPIFFE(s apiv1.SPIFFETLS) error {
	if countTrue(s.ServerID != "", s.TrustDomain != "", s.AuthorizeAny) != 1 {
		return fmt.Errorf("exactly one of serverID, trustDomain, or authorizeAny must be set")
	}
	if s.ServerID != "" {
		if _, err := spiffeid.FromString(s.ServerID); err != nil {
			return fmt.Errorf("serverID %q is not a valid SPIFFE ID: %w", s.ServerID, err)
		}
	}
	if s.TrustDomain != "" && s.TrustDomain != apiv1.TrustDomainSelf {
		if _, err := spiffeid.TrustDomainFromString(s.TrustDomain); err != nil {
			return fmt.Errorf("trustDomain %q is not valid: %w", s.TrustDomain, err)
		}
	}
	return nil
}

func validateCredential(j apiv1.RegistryCredential) error {
	if j.Type == "" {
		return fmt.Errorf("type is required")
	}
	if j.Type != apiv1.CredentialTypeJWT {
		return fmt.Errorf("type %q must be %q", j.Type, apiv1.CredentialTypeJWT)
	}

	if countTrue(j.Provider != "", j.Value != "", j.FromPath != "", j.JWKPath != "", j.JWKValue != "") != 1 {
		return fmt.Errorf("exactly one of provider, value, fromPath, jwkPath, or jwkValue must be set")
	}

	if err := envelope.Validate(j.Envelope); err != nil {
		return err
	}

	hasIssuer := j.Issuer != ""
	hasSubject := j.Subject != ""
	hasAudiences := len(j.Audiences) > 0
	hasExpiration := j.Expiration != nil

	switch {
	case j.JWKPath != "", j.JWKValue != "":
		if !hasIssuer {
			return fmt.Errorf("issuer is required with jwkPath or jwkValue")
		}
		if !hasSubject {
			return fmt.Errorf("subject is required with jwkPath or jwkValue")
		}
		if hasExpiration && j.Expiration.Duration <= 0 {
			return fmt.Errorf("expiration must be a positive duration")
		}
	case j.Provider != "":
		switch j.Provider {
		case apiv1.JWTProviderGitHub, apiv1.JWTProviderForgejo, apiv1.JWTProviderGCP, apiv1.JWTProviderAzure, apiv1.JWTProviderAWS, apiv1.JWTProviderSPIFFE:
		default:
			return fmt.Errorf("provider %q must be one of: %s, %s, %s, %s, %s, %s",
				j.Provider, apiv1.JWTProviderGitHub, apiv1.JWTProviderForgejo, apiv1.JWTProviderGCP, apiv1.JWTProviderAzure, apiv1.JWTProviderAWS, apiv1.JWTProviderSPIFFE)
		}
		if len(j.Audiences) > 1 && (j.Provider == apiv1.JWTProviderGCP || j.Provider == apiv1.JWTProviderAzure) {
			return fmt.Errorf("provider %q accepts at most one audience", j.Provider)
		}
		if hasIssuer || hasSubject {
			return fmt.Errorf("issuer and subject can only be set with jwkPath or jwkValue")
		}
		if hasExpiration {
			return fmt.Errorf("expiration can only be set with jwkPath or jwkValue")
		}
	case j.Value != "", j.FromPath != "":
		if hasIssuer || hasSubject {
			return fmt.Errorf("issuer and subject can only be set with jwkPath or jwkValue")
		}
		if hasAudiences {
			return fmt.Errorf("audiences can only be set with jwkPath, jwkValue, or provider")
		}
		if hasExpiration {
			return fmt.Errorf("expiration can only be set with jwkPath or jwkValue")
		}
	}

	for i, a := range j.Audiences {
		if a == "" {
			return fmt.Errorf("audiences[%d] must not be empty", i)
		}
	}
	return nil
}

func validateChartEntry(c apiv1.ChartEntry) error {
	if c.Name == "" {
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
	switch v.Provider {
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
		if id.Issuer == "" {
			return fmt.Errorf("matchOIDCIdentity[%d].issuer is required", i)
		}
		if id.Subject == "" {
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
		if s.Regex.Pattern == "" {
			return fmt.Errorf("regex.pattern is required when regex is set")
		}
		if _, err := regexp.Compile(s.Regex.Pattern); err != nil {
			return fmt.Errorf("regex.pattern %q does not compile: %w", s.Regex.Pattern, err)
		}
	}
	if s.Semver != "" {
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
	if s == "" {
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
	if s == "" {
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
