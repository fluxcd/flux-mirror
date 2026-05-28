// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/google/go-containerregistry/pkg/name"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

const (
	APIVersion = "mirror.fluxcd.io/v1alpha1"
	Kind       = "Config"

	SortBySemver       = "semver"
	SortByAlphabetical = "alphabetical"
	SortByNumerical    = "numerical"

	VerifyProviderCosign = "cosign"

	// JWTProviderGitHub and JWTProviderForgejo are the OIDC providers that can
	// mint ID tokens for an auth host. Both currently mint tokens the same way
	// (the GitHub/Forgejo Actions endpoint), but they are kept as distinct values
	// so the config can adapt to each platform's breaking changes.
	JWTProviderGitHub  = "github"
	JWTProviderForgejo = "forgejo"
	// JWTProviderGCP mints a Google ID token for the audience via Application
	// Default Credentials (GKE/GCE metadata server, service account key file,
	// workload identity federation, ...).
	JWTProviderGCP = "gcp"
	// JWTProviderAzure mints a Microsoft Entra ID access token for the audience
	// via the default Azure credential chain (AKS/managed identity, workload
	// identity federation, environment credentials, ...).
	JWTProviderAzure = "azure"
	// JWTProviderAWS proves the caller's AWS identity to the registry. AWS mints
	// no JWT, so instead of an OIDC token this signs an sts:GetCallerIdentity
	// request with the ambient role credentials (IMDS, env, ...) and wraps it in
	// a JWT-shaped envelope; the registry replays the signed request to STS to
	// verify it and read the caller's account/ARN. aud pins the target registry
	// via a signed header, not an OIDC audience claim.
	JWTProviderAWS = "aws"

	defaultChartVersion = "*"
	defaultLimit        = 1
)

// Config is the top-level flux-mirror declarative config.
type Config struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Auth       *Auth           `json:"auth,omitempty"`
	Charts     []ChartEntry    `json:"charts,omitempty"`
	Artifacts  []ArtifactEntry `json:"artifacts,omitempty"`
}

// Auth configures per-host credential authentication for outbound OCI registry
// requests. Hosts that are not listed keep their ambient keychain
// authentication; a given host should use either auth or ambient credentials,
// not both. See docs/config.md#auth.
type Auth struct {
	Hosts []AuthHost `json:"hosts,omitempty"`
}

// AuthHost binds an authentication method to a registry host.
type AuthHost struct {
	Host       string          `json:"host"`
	Credential *AuthCredential `json:"credential,omitempty"`
}

// AuthCredential configures a per-host credential. Exactly one of Provider,
// FromEnv, FromPath, or JWKPath selects how the credential is obtained:
//
//   - Provider mints a per-request credential for Aud (an OIDC token for the
//     OIDC providers, or a signed sts:GetCallerIdentity envelope for aws; see
//     JWTProviderGitHub, JWTProviderForgejo, JWTProviderGCP, JWTProviderAzure,
//     JWTProviderAWS).
//   - FromEnv sends a static JWT read as-is from the named environment variable
//     (e.g. a GitLab CI id_token).
//   - FromPath sends a static JWT read from the file at the path, with leading
//     and trailing whitespace trimmed.
//   - JWKPath signs a fresh JWT on every request with the private JSON Web Key
//     at the path.
//
// Iss and Sub are required for, and may only be set with, JWKPath. Aud is
// optional and may only be set with JWKPath or Provider; it defaults to Host.
type AuthCredential struct {
	Provider string `json:"provider,omitempty"`
	FromEnv  string `json:"fromEnv,omitempty"`
	FromPath string `json:"fromPath,omitempty"`
	JWKPath  string `json:"jwkPath,omitempty"`

	Iss string `json:"iss,omitempty"`
	Sub string `json:"sub,omitempty"`

	Aud string `json:"aud,omitempty"`
}

// EffectiveAud returns the audience with the documented default (the host) applied.
func (h AuthHost) EffectiveAud() string {
	if h.Credential == nil || strings.TrimSpace(h.Credential.Aud) == "" {
		return h.Host
	}
	return h.Credential.Aud
}

// ChartEntry mirrors a Helm chart from an HTTP/S or OCI source to an OCI destination.
type ChartEntry struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Limit       *int   `json:"limit,omitempty"`
	Overwrite   bool   `json:"overwrite,omitempty"`
}

// EffectiveVersion returns the version constraint with the documented default applied.
func (c ChartEntry) EffectiveVersion() string {
	if strings.TrimSpace(c.Version) == "" {
		return defaultChartVersion
	}
	return c.Version
}

// EffectiveLimit returns the limit with the documented default applied.
func (c ChartEntry) EffectiveLimit() int {
	if c.Limit == nil {
		return defaultLimit
	}
	return *c.Limit
}

// ArtifactEntry mirrors an OCI artifact (image, OCI chart, signed manifest, etc.).
type ArtifactEntry struct {
	Source           string                `json:"source"`
	Destination      string                `json:"destination"`
	Selector         Selector              `json:"selector"`
	Overwrite        bool                  `json:"overwrite,omitempty"`
	IncludeReferrers bool                  `json:"includeReferrers,omitempty"`
	Verify           *ArtifactVerification `json:"verify,omitempty"`
}

// ArtifactVerification configures signature verification for source artifacts.
type ArtifactVerification struct {
	Provider          string           `json:"provider"`
	MatchOIDCIdentity []OIDCIdentity   `json:"matchOIDCIdentity,omitempty"`
	MinAge            *metav1.Duration `json:"minAge,omitempty"`
}

// OIDCIdentity matches a Fulcio certificate identity.
type OIDCIdentity struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// Selector is the four-step tag selection pipeline:
// regex → semver → sort → top-N.
type Selector struct {
	Regex  *RegexFilter `json:"regex,omitempty"`
	Semver string       `json:"semver,omitempty"`
	SortBy string       `json:"sortBy,omitempty"`
	Limit  *int         `json:"limit,omitempty"`
}

// EffectiveSortBy returns the sort strategy with the documented default applied.
func (s Selector) EffectiveSortBy() string {
	if strings.TrimSpace(s.SortBy) == "" {
		return SortBySemver
	}
	return s.SortBy
}

// EffectiveLimit returns the cap with the documented default applied.
// Returns 0 to mean "no cap" (unlimited).
func (s Selector) EffectiveLimit() int {
	if s.Limit == nil {
		return defaultLimit
	}
	return *s.Limit
}

// RegexFilter applies a Go regexp prefilter to tags, optionally extracting a
// substring via a named capture group for use as the sort/semver value.
type RegexFilter struct {
	Pattern string `json:"pattern"`
	Extract string `json:"extract,omitempty"`
}

// Decode reads YAML from r into a Config without validating it.
func Decode(r io.Reader) (*Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

// Load reads and validates a config file from disk.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	cfg, err := Decode(f)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks the config for semantic correctness. It does not perform any
// network operations; it only verifies that values parse and that required
// fields are present.
func (c *Config) Validate() error {
	if c.APIVersion != APIVersion {
		return fmt.Errorf("apiVersion must be %q, got %q", APIVersion, c.APIVersion)
	}
	if c.Kind != Kind {
		return fmt.Errorf("kind must be %q, got %q", Kind, c.Kind)
	}
	if len(c.Charts) == 0 && len(c.Artifacts) == 0 {
		return fmt.Errorf("config has no entries: at least one of 'charts' or 'artifacts' must be set")
	}
	if c.Auth != nil {
		if err := c.Auth.validate(); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}
	for i, ch := range c.Charts {
		if err := ch.validate(); err != nil {
			return fmt.Errorf("charts[%d]: %w", i, err)
		}
	}
	for i, a := range c.Artifacts {
		if err := a.validate(); err != nil {
			return fmt.Errorf("artifacts[%d]: %w", i, err)
		}
	}
	return nil
}

func (a Auth) validate() error {
	if len(a.Hosts) == 0 {
		return fmt.Errorf("hosts must contain at least one host")
	}
	seen := make(map[string]bool, len(a.Hosts))
	for i, h := range a.Hosts {
		if err := h.validate(); err != nil {
			return fmt.Errorf("hosts[%d]: %w", i, err)
		}
		if seen[h.Host] {
			return fmt.Errorf("hosts[%d]: host %q is configured more than once", i, h.Host)
		}
		seen[h.Host] = true
	}
	return nil
}

func (h AuthHost) validate() error {
	if strings.TrimSpace(h.Host) == "" {
		return fmt.Errorf("host is required")
	}
	if h.Credential == nil {
		return fmt.Errorf("credential is required")
	}
	if err := h.Credential.validate(); err != nil {
		return fmt.Errorf("credential: %w", err)
	}
	return nil
}

func (j AuthCredential) validate() error {
	provider := strings.TrimSpace(j.Provider)
	fromEnv := strings.TrimSpace(j.FromEnv)
	fromPath := strings.TrimSpace(j.FromPath)
	jwkPath := strings.TrimSpace(j.JWKPath)

	n := 0
	for _, set := range []bool{provider != "", fromEnv != "", fromPath != "", jwkPath != ""} {
		if set {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("exactly one of provider, fromEnv, fromPath, or jwkPath must be set")
	}

	hasIss := strings.TrimSpace(j.Iss) != ""
	hasSub := strings.TrimSpace(j.Sub) != ""
	hasAud := strings.TrimSpace(j.Aud) != ""

	switch {
	case jwkPath != "":
		if !hasIss {
			return fmt.Errorf("iss is required with jwkPath")
		}
		if !hasSub {
			return fmt.Errorf("sub is required with jwkPath")
		}
	case provider != "":
		switch provider {
		case JWTProviderGitHub, JWTProviderForgejo, JWTProviderGCP, JWTProviderAzure, JWTProviderAWS:
		default:
			return fmt.Errorf("provider %q must be one of: %s, %s, %s, %s, %s",
				provider, JWTProviderGitHub, JWTProviderForgejo, JWTProviderGCP, JWTProviderAzure, JWTProviderAWS)
		}
		if hasIss || hasSub {
			return fmt.Errorf("iss and sub can only be set with jwkPath")
		}
	case fromEnv != "", fromPath != "":
		if hasIss || hasSub {
			return fmt.Errorf("iss and sub can only be set with jwkPath")
		}
		if hasAud {
			return fmt.Errorf("aud can only be set with jwkPath or provider")
		}
	}
	return nil
}

func (c ChartEntry) validate() error {
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

func (a ArtifactEntry) validate() error {
	if _, err := name.NewRepository(a.Source); err != nil {
		return fmt.Errorf("source %q is not a valid OCI repository: %w", a.Source, err)
	}
	if _, err := name.NewRepository(a.Destination); err != nil {
		return fmt.Errorf("destination %q is not a valid OCI repository: %w", a.Destination, err)
	}
	if err := a.Selector.validate(); err != nil {
		return fmt.Errorf("selector: %w", err)
	}
	if a.Verify != nil {
		if err := a.Verify.validate(); err != nil {
			return fmt.Errorf("verify: %w", err)
		}
	}
	return nil
}

func (v ArtifactVerification) validate() error {
	switch strings.TrimSpace(v.Provider) {
	case VerifyProviderCosign:
	default:
		return fmt.Errorf("provider %q must be %q", v.Provider, VerifyProviderCosign)
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

func (s Selector) validate() error {
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
	case SortBySemver, SortByAlphabetical, SortByNumerical:
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
	case "http", "https", "oci":
	default:
		return fmt.Errorf("scheme %q must be one of: http, https, oci", u.Scheme)
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
