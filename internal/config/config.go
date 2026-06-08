// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
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
	// JWTProviderJWTSVID fetches a JWT-SVID from the SPIFFE Workload API
	// (ambient SPIFFE_ENDPOINT_SOCKET) for the audience (defaults to host) and
	// sends it as the registry credential. This is the HTTP-layer counterpart to
	// the transport-layer tls.spiffe (X.509-SVID mTLS); the two are independent.
	JWTProviderJWTSVID = "jwt-svid"

	// TrustDomainSelf, used in tls.serverAuth.spiffe.trustDomain, authorizes any
	// server SVID in the client's own trust domain (read from its X.509-SVID).
	TrustDomainSelf = "self"

	// TLSClientProviderX509SVID, used in tls.clientAuth.provider, presents a
	// SPIFFE X.509-SVID from the ambient Workload API as the client certificate.
	TLSClientProviderX509SVID = "x509-svid"

	// RegistryProviderECR, RegistryProviderACR and RegistryProviderGAR select a
	// cloud registry provider for hosts[].provider. They obtain registry
	// credentials from the cloud provider's workload identity (the same way the
	// `flux push artifact` family does), and are mutually exclusive with the
	// per-host credential. ECR maps to AWS, ACR to Azure, GAR to GCP.
	RegistryProviderECR = "ecr"
	RegistryProviderACR = "acr"
	RegistryProviderGAR = "gar"

	defaultChartVersion = "*"
	defaultLimit        = 1

	// defaultJWKExp is the jwkPath JWT lifetime when exp is unset. It matches
	// cijwt's per-request signing lifetime, keeping the default behavior of
	// short-lived, freshly signed tokens.
	defaultJWKExp = 60 * time.Second
)

// Config is the top-level flux-mirror declarative config.
//
// Hosts configures per-host authentication for outbound OCI registry requests.
// Hosts that are not listed keep their ambient keychain authentication; a given
// host should use either a configured credential or ambient credentials, not
// both. See docs/config.md#hosts.
type Config struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Hosts      []AuthHost      `json:"hosts,omitempty"`
	Charts     []ChartEntry    `json:"charts,omitempty"`
	Artifacts  []ArtifactEntry `json:"artifacts,omitempty"`
}

// AuthHost binds an authentication method to a registry host. Credential and
// Provider configure the HTTP-layer registry credential (mutually exclusive).
// TLS configures the transport-layer TLS/mTLS settings: it composes with
// Credential, but is mutually exclusive with Provider — a cloud registry
// provider is a managed registry whose transport flux-mirror does not customize.
// At least one of Credential, Provider, or TLS must be set.
type AuthHost struct {
	Host       string          `json:"host"`
	Credential *AuthCredential `json:"credential,omitempty"`
	// Provider authenticates the host with a cloud registry provider's workload
	// identity, one of RegistryProviderECR, RegistryProviderACR or
	// RegistryProviderGAR. Mutually exclusive with Credential.
	Provider string `json:"provider,omitempty"`
	// TLS configures transport-layer TLS for the host: server verification
	// (custom CA), client certificate (mTLS), or SPIFFE X.509-SVID mTLS.
	TLS *TLS `json:"tls,omitempty"`
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
//   - JWKPath signs a fresh JWT with the private JSON Web Key at the path.
//
// Iss and Sub are required for, and may only be set with, JWKPath. Aud is
// optional and may only be set with JWKPath or Provider; it defaults to Host.
// Exp sets the JWT lifetime and may only be set with JWKPath, the one source
// whose lifetime flux-mirror controls; it defaults to a short 60s. Every other
// source's lifetime is fixed by an external issuer or is an opaque static
// token, so Exp is rejected for them.
//
// Username changes how the credential is presented. When unset, the credential
// is a bearer token: sync stamps it as Authorization: Bearer (no auth
// challenge), and login/create write it to the Docker config's registrytoken
// field. When set, the credential becomes the password of a username/password
// pair: sync goes through the standard registry auth challenge (like the cloud
// providers), and login/create write username/password/auth.
type AuthCredential struct {
	Provider string `json:"provider,omitempty"`
	FromEnv  string `json:"fromEnv,omitempty"`
	FromPath string `json:"fromPath,omitempty"`
	JWKPath  string `json:"jwkPath,omitempty"`

	Iss string `json:"iss,omitempty"`
	Sub string `json:"sub,omitempty"`

	Aud      string           `json:"aud,omitempty"`
	Exp      *metav1.Duration `json:"exp,omitempty"`
	Username string           `json:"username,omitempty"`
}

// EffectiveExp returns the jwkPath JWT lifetime with the documented default
// (60s) applied. Only meaningful for jwkPath credentials.
func (c AuthCredential) EffectiveExp() time.Duration {
	if c.Exp != nil {
		return c.Exp.Duration
	}
	return defaultJWKExp
}

// EffectiveAud returns the audience with the documented default (the host) applied.
func (h AuthHost) EffectiveAud() string {
	if h.Credential == nil || strings.TrimSpace(h.Credential.Aud) == "" {
		return h.Host
	}
	return h.Credential.Aud
}

// TLS configures transport-layer TLS for a host. ServerAuth verifies the
// registry's server certificate; ClientAuth presents a client certificate
// (mTLS). Either may use SPIFFE independently, so SPIFFE can authenticate the
// client while a normal/custom CA verifies the server, or vice versa. At least
// one of ServerAuth or ClientAuth must be set.
type TLS struct {
	ServerAuth *TLSServerAuth `json:"serverAuth,omitempty"`
	ClientAuth *TLSClientAuth `json:"clientAuth,omitempty"`
}

// TLSServerAuth verifies the registry's server certificate. Exactly one of the
// fields is set: FromPath/FromEnv/FromBytes provide a custom CA bundle (one or
// more concatenated PEM certificates), or SPIFFE verifies the server's
// X.509-SVID against the SPIFFE trust bundle. When ServerAuth is unset entirely,
// the system trust pool is used.
type TLSServerAuth struct {
	FromPath  string     `json:"fromPath,omitempty"`
	FromEnv   string     `json:"fromEnv,omitempty"`
	FromBytes string     `json:"fromBytes,omitempty"`
	SPIFFE    *SPIFFETLS `json:"spiffe,omitempty"`
}

// TLSData is a single PEM-encoded public value (a client cert chain or a CA
// bundle) obtained from exactly one of FromPath, FromEnv, or FromBytes.
type TLSData struct {
	FromPath  string `json:"fromPath,omitempty"`
	FromEnv   string `json:"fromEnv,omitempty"`
	FromBytes string `json:"fromBytes,omitempty"`
}

// TLSKey is a private key source. Unlike the public certificate and CA values it
// is a secret, so it cannot be inlined in the config (no FromBytes): exactly one
// of FromPath or FromEnv.
type TLSKey struct {
	FromPath string `json:"fromPath,omitempty"`
	FromEnv  string `json:"fromEnv,omitempty"`
}

// TLSClientAuth presents a client certificate (mTLS). Either Provider is set
// (TLSClientProviderX509SVID, presenting a SPIFFE X.509-SVID from the Workload
// API), or the static Certificate and Key pair is set — the two are mutually
// exclusive.
type TLSClientAuth struct {
	Provider    string   `json:"provider,omitempty"`
	Certificate *TLSData `json:"certificate,omitempty"`
	Key         *TLSKey  `json:"key,omitempty"`
}

// SPIFFETLS configures SPIFFE X.509-SVID server verification under
// TLSServerAuth. The trust bundle comes from the ambient Workload API socket
// (SPIFFE_ENDPOINT_SOCKET); the only configuration is how to authorize the
// server's SVID. Exactly one of ServerID, TrustDomain, or AuthorizeAny is set:
//
//   - ServerID authorizes one exact SPIFFE ID.
//   - TrustDomain authorizes any SVID in the named trust domain; the value
//     TrustDomainSelf ("self") uses the client's own trust domain.
//   - AuthorizeAny accepts any SVID the bundle can validate (discouraged).
type SPIFFETLS struct {
	ServerID     string `json:"serverID,omitempty"`
	TrustDomain  string `json:"trustDomain,omitempty"`
	AuthorizeAny bool   `json:"authorizeAny,omitempty"`
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

// ResolvePaths resolves every file-path field (credential fromPath and jwkPath,
// and the tls path fields) against baseDir — the directory of the config file —
// using SecureJoin, so the result is always confined within baseDir: relative
// paths, absolute paths, "../" segments, and symlinks cannot escape it. Empty
// values are left empty. A baseDir of "" is a no-op (paths stay as written).
func (c *Config) ResolvePaths(baseDir string) error {
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
func hostPathFields(h *AuthHost) []*string {
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
func (c *Config) Validate() error {
	return c.validate(true)
}

// ValidateNoEntriesOK is like Validate but permits a config with no charts or
// artifacts. Used by commands that consume only the hosts section (e.g.
// `flux-mirror login`), for which a mirror entry would be meaningless.
func (c *Config) ValidateNoEntriesOK() error {
	return c.validate(false)
}

func (c *Config) validate(requireEntries bool) error {
	if c.APIVersion != APIVersion {
		return fmt.Errorf("apiVersion must be %q, got %q", APIVersion, c.APIVersion)
	}
	if c.Kind != Kind {
		return fmt.Errorf("kind must be %q, got %q", Kind, c.Kind)
	}
	if requireEntries && len(c.Charts) == 0 && len(c.Artifacts) == 0 {
		return fmt.Errorf("config has no entries: at least one of 'charts' or 'artifacts' must be set")
	}
	seen := make(map[string]bool, len(c.Hosts))
	for i, h := range c.Hosts {
		if err := h.validate(); err != nil {
			return fmt.Errorf("hosts[%d]: %w", i, err)
		}
		if seen[h.Host] {
			return fmt.Errorf("hosts[%d]: host %q is configured more than once", i, h.Host)
		}
		seen[h.Host] = true
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

func (h AuthHost) validate() error {
	if strings.TrimSpace(h.Host) == "" {
		return fmt.Errorf("host is required")
	}
	provider := strings.TrimSpace(h.Provider)
	if h.Credential != nil && provider != "" {
		return fmt.Errorf("credential and provider are mutually exclusive")
	}
	if provider != "" && h.TLS != nil {
		return fmt.Errorf("provider and tls are mutually exclusive")
	}
	if h.Credential != nil {
		if err := h.Credential.validate(); err != nil {
			return fmt.Errorf("credential: %w", err)
		}
	}
	if provider != "" {
		switch provider {
		case RegistryProviderECR, RegistryProviderACR, RegistryProviderGAR:
		default:
			return fmt.Errorf("provider %q must be one of: %s, %s, %s",
				provider, RegistryProviderECR, RegistryProviderACR, RegistryProviderGAR)
		}
	}
	if h.TLS != nil {
		if err := h.TLS.validate(); err != nil {
			return fmt.Errorf("tls: %w", err)
		}
	}
	if h.Credential == nil && provider == "" && h.TLS == nil {
		return fmt.Errorf("one of credential, provider, or tls is required")
	}
	return nil
}

// validate checks the TLS settings: at least one of serverAuth or clientAuth
// must be set, and each (if set) must itself be valid.
func (t TLS) validate() error {
	if t.ServerAuth == nil && t.ClientAuth == nil {
		return fmt.Errorf("one of serverAuth or clientAuth is required")
	}
	if t.ServerAuth != nil {
		if err := t.ServerAuth.validate(); err != nil {
			return fmt.Errorf("serverAuth: %w", err)
		}
	}
	if t.ClientAuth != nil {
		if err := t.ClientAuth.validate(); err != nil {
			return fmt.Errorf("clientAuth: %w", err)
		}
	}
	return nil
}

// validate checks that exactly one of the CA-bundle sources or spiffe is set.
func (s TLSServerAuth) validate() error {
	n := 0
	for _, set := range []bool{
		strings.TrimSpace(s.FromPath) != "",
		strings.TrimSpace(s.FromEnv) != "",
		strings.TrimSpace(s.FromBytes) != "",
		s.SPIFFE != nil,
	} {
		if set {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("exactly one of fromPath, fromEnv, fromBytes, or spiffe must be set")
	}
	if s.SPIFFE != nil {
		if err := s.SPIFFE.validate(); err != nil {
			return fmt.Errorf("spiffe: %w", err)
		}
	}
	return nil
}

// validate checks that exactly one source is set.
func (d TLSData) validate() error {
	n := 0
	for _, set := range []bool{
		strings.TrimSpace(d.FromPath) != "",
		strings.TrimSpace(d.FromEnv) != "",
		strings.TrimSpace(d.FromBytes) != "",
	} {
		if set {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("exactly one of fromPath, fromEnv, or fromBytes must be set")
	}
	return nil
}

// validate checks that exactly one source is set. A private key cannot be inlined.
func (k TLSKey) validate() error {
	n := 0
	for _, set := range []bool{
		strings.TrimSpace(k.FromPath) != "",
		strings.TrimSpace(k.FromEnv) != "",
	} {
		if set {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("exactly one of fromPath or fromEnv must be set")
	}
	return nil
}

// validate checks the client cert source: either provider (x509-svid) or a
// static certificate/key pair, mutually exclusive.
func (c TLSClientAuth) validate() error {
	provider := strings.TrimSpace(c.Provider)
	hasStatic := c.Certificate != nil || c.Key != nil
	if provider != "" && hasStatic {
		return fmt.Errorf("provider is mutually exclusive with certificate and key")
	}
	if provider != "" {
		if provider != TLSClientProviderX509SVID {
			return fmt.Errorf("provider %q must be %q", provider, TLSClientProviderX509SVID)
		}
		return nil
	}
	if c.Certificate == nil {
		return fmt.Errorf("certificate is required")
	}
	if err := c.Certificate.validate(); err != nil {
		return fmt.Errorf("certificate: %w", err)
	}
	if c.Key == nil {
		return fmt.Errorf("key is required")
	}
	if err := c.Key.validate(); err != nil {
		return fmt.Errorf("key: %w", err)
	}
	return nil
}

// validate checks that exactly one authorizer is set and parses.
func (s SPIFFETLS) validate() error {
	serverID := strings.TrimSpace(s.ServerID)
	trustDomain := strings.TrimSpace(s.TrustDomain)

	n := 0
	for _, set := range []bool{serverID != "", trustDomain != "", s.AuthorizeAny} {
		if set {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("exactly one of serverID, trustDomain, or authorizeAny must be set")
	}
	if serverID != "" {
		if _, err := spiffeid.FromString(serverID); err != nil {
			return fmt.Errorf("serverID %q is not a valid SPIFFE ID: %w", serverID, err)
		}
	}
	if trustDomain != "" && trustDomain != TrustDomainSelf {
		if _, err := spiffeid.TrustDomainFromString(trustDomain); err != nil {
			return fmt.Errorf("trustDomain %q is not valid: %w", trustDomain, err)
		}
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
	hasExp := j.Exp != nil

	switch {
	case jwkPath != "":
		if !hasIss {
			return fmt.Errorf("iss is required with jwkPath")
		}
		if !hasSub {
			return fmt.Errorf("sub is required with jwkPath")
		}
		if hasExp && j.Exp.Duration <= 0 {
			return fmt.Errorf("exp must be a positive duration")
		}
	case provider != "":
		switch provider {
		case JWTProviderGitHub, JWTProviderForgejo, JWTProviderGCP, JWTProviderAzure, JWTProviderAWS, JWTProviderJWTSVID:
		default:
			return fmt.Errorf("provider %q must be one of: %s, %s, %s, %s, %s, %s",
				provider, JWTProviderGitHub, JWTProviderForgejo, JWTProviderGCP, JWTProviderAzure, JWTProviderAWS, JWTProviderJWTSVID)
		}
		if hasIss || hasSub {
			return fmt.Errorf("iss and sub can only be set with jwkPath")
		}
		if hasExp {
			return fmt.Errorf("exp can only be set with jwkPath")
		}
	case fromEnv != "", fromPath != "":
		if hasIss || hasSub {
			return fmt.Errorf("iss and sub can only be set with jwkPath")
		}
		if hasAud {
			return fmt.Errorf("aud can only be set with jwkPath or provider")
		}
		if hasExp {
			return fmt.Errorf("exp can only be set with jwkPath")
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
