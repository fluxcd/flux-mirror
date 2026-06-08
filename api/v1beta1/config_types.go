// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package v1beta1

import (
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ConfigKind is the kind used by flux-mirror config files.
	ConfigKind = "Config"

	// SortBySemver sorts selected tags by semantic version (Selector.SortBy).
	SortBySemver = "semver"
	// SortByAlphabetical sorts selected tags lexicographically (Selector.SortBy).
	SortByAlphabetical = "alphabetical"
	// SortByNumerical sorts selected tags as numbers (Selector.SortBy).
	SortByNumerical = "numerical"

	// VerifyProviderCosign selects cosign keyless signature verification
	// (ArtifactVerification.Provider).
	VerifyProviderCosign = "cosign"

	// JWTProviderGitHub mints a GitHub Actions OIDC ID token for the audience.
	// It and JWTProviderForgejo currently mint tokens the same way (the
	// GitHub/Forgejo Actions endpoint), but they are kept as distinct values so
	// the config can adapt to each platform's breaking changes.
	JWTProviderGitHub = "github"
	// JWTProviderForgejo mints a Forgejo Actions OIDC ID token for the audience.
	// See JWTProviderGitHub for why the two providers are kept distinct.
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

	// TrustDomainSelf used in tls.serverAuth.spiffe.trustDomain, authorizes any
	// server SVID in the client's own trust domain (read from its X.509-SVID).
	TrustDomainSelf = "self"

	// TLSClientProviderX509SVID used in tls.clientAuth.provider, presents a
	// SPIFFE X.509-SVID from the ambient Workload API as the client certificate.
	TLSClientProviderX509SVID = "x509-svid"

	// RegistryProviderECR selects AWS ECR workload-identity credentials for a
	// host (hosts[].provider). It maps to AWS and is mutually exclusive with the
	// per-host credential.
	RegistryProviderECR = "ecr"
	// RegistryProviderACR selects Azure ACR workload-identity credentials for a
	// host (hosts[].provider). It maps to Azure and is mutually exclusive with
	// the per-host credential.
	RegistryProviderACR = "acr"
	// RegistryProviderGAR selects Google GAR workload-identity credentials for a
	// host (hosts[].provider). It maps to GCP and is mutually exclusive with the
	// per-host credential.
	RegistryProviderGAR = "gar"

	// defaultChartVersion is the chart version constraint applied when unset.
	defaultChartVersion = "*"
	// defaultLimit is the chart/selector limit applied when unset.
	defaultLimit = 1

	// defaultJWKExp is the jwkPath JWT lifetime when exp is unset. It matches
	// cijwt's per-request signing lifetime, keeping the default behavior of
	// short-lived, freshly signed tokens.
	defaultJWKExp = 60 * time.Second
)

// Config is the flux-mirror configuration file.
//
// It lists the registry hosts to authenticate and the OCI artifacts and Helm
// charts to mirror.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
type Config struct {
	// TypeMeta identifies the config API version and kind.
	metav1.TypeMeta `json:",inline"`

	// Hosts configures per-host authentication for OCI registry requests, both
	// source pulls and destination pushes.
	// +optional
	Hosts []RegistryHost `json:"hosts,omitempty"`

	// Charts lists Helm charts to mirror from an HTTP/S or OCI source.
	// +optional
	Charts []ChartEntry `json:"charts,omitempty"`

	// Artifacts lists OCI artifacts to mirror.
	// +optional
	Artifacts []ArtifactEntry `json:"artifacts,omitempty"`
}

// RegistryHost binds an authentication method to a registry host. Credential and
// Provider configure the HTTP-layer registry credential (mutually exclusive).
// TLS configures the transport-layer TLS/mTLS settings: it composes with
// Credential, but is mutually exclusive with Provider — a cloud registry
// provider is a managed registry whose transport flux-mirror does not customize.
// At least one of Credential, Provider, TLS, or MaxChunkSize must be set.
type RegistryHost struct {
	// Host is the registry host (and optional port) the auth applies to.
	Host string `json:"host"`

	// Credential configures the HTTP-layer registry credential for the host.
	// +optional
	Credential *RegistryCredential `json:"credential,omitempty"`

	// Provider authenticates the host with a cloud registry provider's workload
	// identity, one of RegistryProviderECR, RegistryProviderACR or
	// RegistryProviderGAR. Mutually exclusive with Credential.
	// +optional
	// +kubebuilder:validation:Enum=ecr;acr;gar
	Provider string `json:"provider,omitempty"`

	// TLS configures transport-layer TLS for the host: server verification
	// (custom CA), client certificate (mTLS), or SPIFFE X.509-SVID mTLS.
	// +optional
	TLS *TLS `json:"tls,omitempty"`

	// MaxChunkSize is the maximum size in KiB (1024 bytes) for an OCI blob
	// upload PATCH to this host; larger blobs are split into chunked PATCH
	// uploads. 0 (the default) disables chunking, sending a single monolithic
	// PATCH per blob. Useful for registries or proxies that cap request bodies.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxChunkSize int `json:"maxChunkSize,omitempty"`
}

// RegistryCredential configures a per-host credential. Exactly one of Provider,
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
type RegistryCredential struct {
	// Provider mints a per-request credential for the audience, one of
	// JWTProviderGitHub, JWTProviderForgejo, JWTProviderGCP, JWTProviderAzure,
	// JWTProviderAWS, or JWTProviderJWTSVID.
	// +optional
	// +kubebuilder:validation:Enum=github;forgejo;gcp;azure;aws;jwt-svid
	Provider string `json:"provider,omitempty"`

	// FromEnv sends a static JWT read from the named environment variable.
	// +optional
	FromEnv string `json:"fromEnv,omitempty"`

	// FromPath sends a static JWT read from the file at the path.
	// +optional
	FromPath string `json:"fromPath,omitempty"`

	// JWKPath signs a fresh JWT with the private JSON Web Key at the path.
	// +optional
	JWKPath string `json:"jwkPath,omitempty"`

	// Iss is the issuer claim for jwkPath-signed tokens.
	// +optional
	Iss string `json:"iss,omitempty"`

	// Sub is the subject claim for jwkPath-signed tokens.
	// +optional
	Sub string `json:"sub,omitempty"`

	// Aud is the audience claim. Defaults to Host.
	// +optional
	Aud string `json:"aud,omitempty"`

	// Exp is the jwkPath JWT lifetime. Defaults to 60s.
	// +optional
	Exp *metav1.Duration `json:"exp,omitempty"`

	// Username presents the credential as a username/password pair.
	// +optional
	Username string `json:"username,omitempty"`
}

// EffectiveExp returns the jwkPath JWT lifetime with the documented default
// (60s) applied. Only meaningful for jwkPath credentials.
func (c RegistryCredential) EffectiveExp() time.Duration {
	if c.Exp != nil {
		return c.Exp.Duration
	}
	return defaultJWKExp
}

// EffectiveAud returns the audience with the documented default (the host) applied.
func (h RegistryHost) EffectiveAud() string {
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
	// ServerAuth verifies the registry's server certificate.
	// +optional
	ServerAuth *TLSServerAuth `json:"serverAuth,omitempty"`

	// ClientAuth presents a client certificate (mTLS).
	// +optional
	ClientAuth *TLSClientAuth `json:"clientAuth,omitempty"`
}

// TLSServerAuth verifies the registry's server certificate. Exactly one of the
// fields is set: FromPath/FromEnv/FromBytes provide a custom CA bundle (one or
// more concatenated PEM certificates), or SPIFFE verifies the server's
// X.509-SVID against the SPIFFE trust bundle. When ServerAuth is unset entirely,
// the system trust pool is used.
type TLSServerAuth struct {
	// FromPath reads the CA bundle from the file at the path.
	// +optional
	FromPath string `json:"fromPath,omitempty"`

	// FromEnv reads the CA bundle from the named environment variable.
	// +optional
	FromEnv string `json:"fromEnv,omitempty"`

	// FromBytes inlines the PEM-encoded CA bundle.
	// +optional
	FromBytes string `json:"fromBytes,omitempty"`

	// SPIFFE verifies the server's X.509-SVID against the SPIFFE trust bundle.
	// +optional
	SPIFFE *SPIFFETLS `json:"spiffe,omitempty"`
}

// TLSData is a single PEM-encoded public value (a client cert chain or a CA
// bundle) obtained from exactly one of FromPath, FromEnv, or FromBytes.
type TLSData struct {
	// FromPath reads the value from the file at the path.
	// +optional
	FromPath string `json:"fromPath,omitempty"`

	// FromEnv reads the value from the named environment variable.
	// +optional
	FromEnv string `json:"fromEnv,omitempty"`

	// FromBytes inlines the PEM-encoded value.
	// +optional
	FromBytes string `json:"fromBytes,omitempty"`
}

// TLSKey is a private key source. Unlike the public certificate and CA values it
// is a secret, so it cannot be inlined in the config (no FromBytes): exactly one
// of FromPath or FromEnv.
type TLSKey struct {
	// FromPath reads the private key from the file at the path.
	// +optional
	FromPath string `json:"fromPath,omitempty"`

	// FromEnv reads the private key from the named environment variable.
	// +optional
	FromEnv string `json:"fromEnv,omitempty"`
}

// TLSClientAuth presents a client certificate (mTLS). Either Provider is set
// (TLSClientProviderX509SVID, presenting a SPIFFE X.509-SVID from the Workload
// API), or the static Certificate and Key pair is set — the two are mutually
// exclusive.
type TLSClientAuth struct {
	// Provider presents a SPIFFE X.509-SVID from the Workload API as the client
	// certificate (TLSClientProviderX509SVID).
	// +optional
	// +kubebuilder:validation:Enum=x509-svid
	Provider string `json:"provider,omitempty"`

	// Certificate is the static client certificate chain.
	// +optional
	Certificate *TLSData `json:"certificate,omitempty"`

	// Key is the static client private key.
	// +optional
	Key *TLSKey `json:"key,omitempty"`
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
	// ServerID authorizes one exact SPIFFE ID.
	// +optional
	ServerID string `json:"serverID,omitempty"`

	// TrustDomain authorizes any SVID in the named trust domain; TrustDomainSelf
	// ("self") uses the client's own trust domain.
	// +optional
	TrustDomain string `json:"trustDomain,omitempty"`

	// AuthorizeAny accepts any SVID the bundle can validate (discouraged).
	// +optional
	AuthorizeAny bool `json:"authorizeAny,omitempty"`
}

// ChartEntry mirrors a Helm chart from an HTTP/S or OCI source to an OCI destination.
type ChartEntry struct {
	// Source is the HTTP/S or OCI repository URL the chart is mirrored from.
	Source string `json:"source"`

	// Destination is the OCI repository URL the chart is mirrored to.
	Destination string `json:"destination"`

	// Name is the chart name.
	Name string `json:"name"`

	// Version is the semver constraint selecting which chart versions to mirror.
	// +optional
	Version string `json:"version,omitempty"`

	// Limit caps how many matching versions are mirrored. 0 means unlimited.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Limit *int `json:"limit,omitempty"`

	// Overwrite re-mirrors versions that already exist at the destination.
	// +optional
	Overwrite bool `json:"overwrite,omitempty"`
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
	// Source is the OCI repository the artifact is mirrored from.
	Source string `json:"source"`

	// Destination is the OCI repository the artifact is mirrored to.
	Destination string `json:"destination"`

	// Selector selects which tags to mirror.
	Selector Selector `json:"selector"`

	// Overwrite re-mirrors tags that already exist at the destination.
	// +optional
	Overwrite bool `json:"overwrite,omitempty"`

	// IncludeReferrers also mirrors the artifact's referrers (signatures, SBOMs).
	// +optional
	IncludeReferrers bool `json:"includeReferrers,omitempty"`

	// Verify configures signature verification for source artifacts.
	// +optional
	Verify *ArtifactVerification `json:"verify,omitempty"`
}

// ArtifactVerification configures signature verification for source artifacts.
type ArtifactVerification struct {
	// Provider is the verification provider (VerifyProviderCosign).
	// +kubebuilder:validation:Enum=cosign
	Provider string `json:"provider"`

	// MatchOIDCIdentity lists the Fulcio certificate identities to accept.
	// +optional
	MatchOIDCIdentity []OIDCIdentity `json:"matchOIDCIdentity,omitempty"`

	// MinAge requires a signature to be at least this old before mirroring.
	// +optional
	MinAge *metav1.Duration `json:"minAge,omitempty"`
}

// OIDCIdentity matches a Fulcio certificate identity.
type OIDCIdentity struct {
	// Issuer is the OIDC issuer URL of the signing identity.
	Issuer string `json:"issuer"`

	// Subject is a regexp matched against the certificate subject.
	Subject string `json:"subject"`
}

// Selector is the four-step tag selection pipeline:
// regex → semver → sort → top-N.
type Selector struct {
	// Regex applies a Go regexp prefilter to tags.
	// +optional
	Regex *RegexFilter `json:"regex,omitempty"`

	// Semver is a semver constraint applied after the regex prefilter.
	// +optional
	Semver string `json:"semver,omitempty"`

	// SortBy selects the sort strategy: semver, alphabetical, or numerical.
	// +optional
	// +kubebuilder:validation:Enum=semver;alphabetical;numerical
	SortBy string `json:"sortBy,omitempty"`

	// Limit caps how many tags are mirrored. 0 means unlimited.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Limit *int `json:"limit,omitempty"`
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
	// Pattern is the Go regexp applied to tags.
	Pattern string `json:"pattern"`

	// Extract is the named capture group used as the sort/semver value.
	// +optional
	Extract string `json:"extract,omitempty"`
}
