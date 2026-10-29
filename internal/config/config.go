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

	"sigs.k8s.io/yaml"

	cryptov1 "github.com/fluxcd/pkg/apis/crypto/v1"

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
  - tls is now tls.path (a directory), inline ca.crt/tls.crt/tls.key, or
    tls.spiffe.serverIDs (SPIFFE mutual TLS)
  - inline v1beta1 tls values and spiffe serverAuth with clientAuth
    x509-svid convert automatically; per-file paths, one-sided SPIFFE, and
    authorizeAny must be migrated manually
See docs/config.md for the full specification.`

// DeprecationWarning returns the migration warning to print for a decoded
// config, naming the apiVersion declared by the source document. It is empty
// for the current version. Decode already performs the v1beta1 -> v1beta2
// conversion, so callers only surface the warning; they do not act on the
// version.
func DeprecationWarning(declaredAPIVersion string) string {
	guide := MigrationGuide(declaredAPIVersion)
	if guide == "" {
		return ""
	}
	return V1Beta1MigrationWarning + "\n" + guide
}

// MigrationGuide returns the v1beta1 -> v1beta2 migration guide for the given
// declared apiVersion, or "" for the current version.
func MigrationGuide(declaredAPIVersion string) string {
	if declaredAPIVersion != DeprecatedV1Beta1APIVersion {
		return ""
	}
	return V1Beta1Migration
}

// Decode reads YAML from r into a Config without validating it. It returns the
// config, which is always in the current v1beta2 shape, and the apiVersion
// declared by the source document. A v1beta1 document is converted to v1beta2
// in memory; pass the declared version to DeprecationWarning to surface the
// migration warning.
func Decode(r io.Reader) (*apiv1.Config, string, error) {
	return DecodeWithEnvSubst(r, true)
}

// DecodeWithEnvSubst is like Decode, with optional strict environment variable
// substitution before parsing.
func DecodeWithEnvSubst(r io.Reader, substitute bool) (*apiv1.Config, string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, "", fmt.Errorf("read config: %w", err)
	}
	rendered := data
	if substitute {
		out, err := envsubst.EvalEnv(string(data), true)
		if err != nil {
			return nil, "", fmt.Errorf("substitute environment variables: %w", err)
		}
		rendered = []byte(out)
	}
	// Peek at apiVersion leniently to branch the right wire struct. A v1beta1
	// document uses the old field names, which the strict v1beta2 decoder would
	// reject as unknown fields before apiVersion is ever checked. A malformed
	// document yields no peeked version and falls through to the strict decoder,
	// which reports the parse error.
	declaredAPIVersion, _ := peekAPIVersion(rendered)
	switch declaredAPIVersion {
	case DeprecatedV1Beta1APIVersion:
		var old apiv1beta1.Config
		// v1beta1 is frozen: decode it leniently, like the release that
		// introduced it, so unknown fields do not stop an old config loading.
		if err := yaml.Unmarshal(rendered, &old); err != nil {
			return nil, declaredAPIVersion, fmt.Errorf("parse config: %w", err)
		}
		cfg, err := apiv1.FromV1Beta1(&old)
		if err != nil {
			return nil, declaredAPIVersion, fmt.Errorf("convert v1beta1 config: %w", err)
		}
		return cfg, declaredAPIVersion, nil
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
		return nil, declaredAPIVersion, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, declaredAPIVersion, nil
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
	if v != apiv1.GroupVersion.String() {
		return fmt.Errorf("apiVersion must be %q, got %q", apiv1.GroupVersion.String(), v)
	}
	return nil
}

// ResolvePaths resolves every file-path field (credential fromPath and jwkPath,
// and the tls path) against baseDir — the directory of the config file —
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
		ps = append(ps, &h.TLS.Path)
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
	if err := checkAPIVersion(c.APIVersion); err != nil {
		return err
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
	h.Provider = strings.TrimSpace(h.Provider)
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

// validateTLS checks the TLS settings: exactly one of path, inline certificate
// fields, or spiffe must be set. Inline data is validated here; a path directory
// is checked when the transport is built, because paths are resolved after
// validation. A spiffe block must authorize at least one parseable server
// identity.
func validateTLS(t apiv1.TLS) error {
	inline := t.CACert != "" || t.TLSCert != "" || t.TLSKey != ""
	if countTrue(strings.TrimSpace(t.Path) != "", inline, t.SPIFFE != nil) != 1 {
		return fmt.Errorf("exactly one of path, inline certificates, or spiffe must be set")
	}
	if inline {
		return validateTLSInline(t)
	}
	if t.SPIFFE == nil {
		return nil
	}
	if len(t.SPIFFE.ServerIDs) == 0 {
		return fmt.Errorf("spiffe.serverIDs is required")
	}
	for i, raw := range t.SPIFFE.ServerIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			return fmt.Errorf("spiffe.serverIDs[%d] must not be empty", i)
		}
		if _, err := cryptov1.ParseSPIFFEID(id); err != nil {
			return fmt.Errorf("spiffe.serverIDs[%d]: %w", i, err)
		}
	}
	return nil
}

// validateTLSInline checks the inline PEM fields: tls.crt and tls.key must be
// set together, at least one of ca.crt or the pair must be present, and no
// inline value may be blank.
func validateTLSInline(t apiv1.TLS) error {
	hasCACert := strings.TrimSpace(t.CACert) != ""
	hasCert := strings.TrimSpace(t.TLSCert) != ""
	hasKey := strings.TrimSpace(t.TLSKey) != ""
	if t.CACert != "" && !hasCACert {
		return fmt.Errorf("inline ca.crt must not be empty")
	}
	if t.TLSCert != "" && !hasCert {
		return fmt.Errorf("inline tls.crt must not be empty")
	}
	if t.TLSKey != "" && !hasKey {
		return fmt.Errorf("inline tls.key must not be empty")
	}
	if hasCert != hasKey {
		return fmt.Errorf("inline tls.crt and tls.key must be set together")
	}
	if !hasCert && !hasCACert {
		return fmt.Errorf("inline must contain ca.crt or both tls.crt and tls.key")
	}
	return nil
}

func validateCredential(j apiv1.RegistryCredential) error {
	credentialType := strings.TrimSpace(j.Type)
	if credentialType == "" {
		return fmt.Errorf("type is required")
	}
	if credentialType != apiv1.CredentialTypeJWT {
		return fmt.Errorf("type %q must be %q", credentialType, apiv1.CredentialTypeJWT)
	}

	provider := strings.TrimSpace(j.Provider)
	value := strings.TrimSpace(j.Value)
	fromPath := strings.TrimSpace(j.FromPath)
	jwkPath := strings.TrimSpace(j.JWKPath)
	jwkValue := strings.TrimSpace(j.JWKValue)

	if countTrue(provider != "", value != "", fromPath != "", jwkPath != "", jwkValue != "") != 1 {
		return fmt.Errorf("exactly one of provider, value, fromPath, jwkPath, or jwkValue must be set")
	}

	if err := envelope.Validate(j.Envelope); err != nil {
		return err
	}

	hasIssuer := strings.TrimSpace(j.Issuer) != ""
	hasSubject := strings.TrimSpace(j.Subject) != ""
	hasAudiences := len(j.Audiences) > 0
	hasExpiration := j.Expiration != nil

	switch {
	case jwkPath != "", jwkValue != "":
		if !hasIssuer {
			return fmt.Errorf("issuer is required with jwkPath or jwkValue")
		}
		if !hasSubject {
			return fmt.Errorf("subject is required with jwkPath or jwkValue")
		}
		if hasExpiration && j.Expiration.Duration <= 0 {
			return fmt.Errorf("expiration must be a positive duration")
		}
	case provider != "":
		switch provider {
		case apiv1.JWTProviderGitHub, apiv1.JWTProviderForgejo, apiv1.JWTProviderGCP, apiv1.JWTProviderAzure, apiv1.JWTProviderAWS, apiv1.JWTProviderSPIFFE:
		default:
			return fmt.Errorf("provider %q must be one of: %s, %s, %s, %s, %s, %s",
				provider, apiv1.JWTProviderGitHub, apiv1.JWTProviderForgejo, apiv1.JWTProviderGCP, apiv1.JWTProviderAzure, apiv1.JWTProviderAWS, apiv1.JWTProviderSPIFFE)
		}
		if len(j.Audiences) > 1 && (provider == apiv1.JWTProviderGCP || provider == apiv1.JWTProviderAzure) {
			return fmt.Errorf("provider %q accepts at most one audience", provider)
		}
		if hasIssuer || hasSubject {
			return fmt.Errorf("issuer and subject can only be set with jwkPath or jwkValue")
		}
		if hasExpiration {
			return fmt.Errorf("expiration can only be set with jwkPath or jwkValue")
		}
	case value != "", fromPath != "":
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

	seen := make(map[string]bool, len(j.Audiences))
	for i, raw := range j.Audiences {
		a := strings.TrimSpace(raw)
		if a == "" {
			return fmt.Errorf("audiences[%d] must not be empty", i)
		}
		if seen[a] {
			return fmt.Errorf("audiences[%d] duplicates %q", i, a)
		}
		seen[a] = true
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
	switch provider := strings.TrimSpace(v.Provider); provider {
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
