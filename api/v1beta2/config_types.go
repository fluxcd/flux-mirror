// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package v1beta2

import (
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
	// verify it and read the caller's account/ARN. Each audience pins a target
	// registry via a signed header, not an OIDC audience claim.
	JWTProviderAWS = "aws"
	// JWTProviderSPIFFE fetches a JWT-SVID from the SPIFFE Workload API
	// (ambient SPIFFE_ENDPOINT_SOCKET) for the audiences (defaulting to the host)
	// and sends it as the registry credential. This is the HTTP-layer counterpart
	// to the transport-layer tls.clientAuth/serverAuth spiffe provider
	// (X.509-SVID mTLS); the two are independent.
	JWTProviderSPIFFE = "spiffe"

	// CredentialTypeJWT selects JSON Web Token material for a credential
	// (credential.type). It is the only type currently supported; other
	// material kinds (e.g. X.509) are reserved for a future release.
	CredentialTypeJWT = "jwt"

	// TrustDomainSelf used in tls.serverAuth.spiffe.trustDomain, authorizes any
	// server SVID in the client's own trust domain (read from its X.509-SVID).
	TrustDomainSelf = "self"

	// TLSProviderSPIFFE used in tls.clientAuth.provider and tls.serverAuth.provider,
	// uses a SPIFFE X.509-SVID: as the client certificate for clientAuth, and as the
	// expected server identity for serverAuth. The trust bundle comes from the
	// ambient Workload API (SPIFFE_ENDPOINT_SOCKET). It is the only provider value;
	// omitting provider selects the file-based path fields instead.
	TLSProviderSPIFFE = "spiffe"

	// RegistryProviderGeneric is the default registry provider for a host
	// (hosts[].provider): the host is an ordinary registry whose transport and
	// credential flux-mirror configures directly. It composes with credential and
	// tls, unlike the cloud providers below.
	RegistryProviderGeneric = "generic"
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

	// defaultJWKExp is the jwkPath/jwkValue JWT lifetime when expiration is unset.
	// It matches cijwt's per-request signing lifetime, keeping the default behavior
	// of short-lived, freshly signed tokens.
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

	// Charts lists Helm charts to mirror from an HTTP/S repository into an OCI
	// registry. Mirroring a chart that already lives in an OCI registry is an
	// OCI-to-OCI copy; configure it under Artifacts instead.
	// +optional
	Charts []ChartEntry `json:"charts,omitempty"`

	// Artifacts lists OCI artifacts to mirror.
	// +optional
	Artifacts []ArtifactEntry `json:"artifacts,omitempty"`
}

// RegistryHost binds an authentication method to a registry host. Credential and
// Provider configure the HTTP-layer registry credential (mutually exclusive for a
// cloud provider). TLS configures the transport-layer TLS/mTLS settings: it
// composes with Credential and with the default (generic) Provider, but is
// mutually exclusive with a cloud Provider — a cloud registry provider is a
// managed registry whose transport flux-mirror does not customize. At least one
// of Credential, a cloud Provider, TLS, or MaxChunkSize must be set.
type RegistryHost struct {
	// Host is the registry host (and optional port) the auth applies to.
	Host string `json:"host"`

	// Credential configures the HTTP-layer registry credential for the host.
	// +optional
	Credential *RegistryCredential `json:"credential,omitempty"`

	// Provider selects a cloud registry provider's workload-identity credentials
	// for the host: RegistryProviderECR, RegistryProviderACR, or
	// RegistryProviderGAR. Omitted (or RegistryProviderGeneric) means an ordinary
	// registry, for which flux-mirror configures the credential and/or TLS
	// directly. A cloud provider is mutually exclusive with Credential and TLS.
	// +optional
	// +kubebuilder:validation:Enum=generic;ecr;acr;gar
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

// IsCloudProvider reports whether the host selects a cloud registry provider
// (RegistryProviderECR, RegistryProviderACR, or RegistryProviderGAR) rather than
// the default (RegistryProviderGeneric) provider. A cloud provider authenticates
// the host itself and is mutually exclusive with Credential and TLS. The value is
// matched as written.
func (h RegistryHost) IsCloudProvider() bool {
	switch h.Provider {
	case RegistryProviderECR, RegistryProviderACR, RegistryProviderGAR:
		return true
	default:
		return false
	}
}

// RegistryCredential configures a per-host credential. Type selects the kind of
// material (CredentialTypeJWT) and is required. Exactly one of Provider, Value,
// FromPath, JWKPath, or JWKValue selects how the credential is obtained:
//
//   - Provider mints a per-request credential for Audiences (an OIDC token for
//     the OIDC providers, a JWT-SVID for spiffe, or a signed
//     sts:GetCallerIdentity envelope for aws; see JWTProviderGitHub,
//     JWTProviderForgejo, JWTProviderGCP, JWTProviderAzure, JWTProviderAWS,
//     JWTProviderSPIFFE).
//   - Value sends a static JWT read as-is from the config. Environment
//     substitution can fill this from an environment variable.
//   - FromPath sends a static JWT read from the file at the path, with leading
//     and trailing whitespace trimmed.
//   - JWKPath signs a fresh JWT with the private JSON Web Key at the path.
//   - JWKValue signs a fresh JWT with the private JSON Web Key read from the config.
//
// Issuer and Subject are required for, and may only be set with, JWKPath or
// JWKValue. Audiences is optional and may only be set with JWKPath, JWKValue, or
// Provider; it defaults to Host. Every source accepts multiple audiences except
// the gcp and azure providers, whose token carries a single aud. Expiration sets
// the JWT lifetime and may only be set with JWKPath or JWKValue, the sources
// whose lifetime flux-mirror controls; it defaults to a short 60s. Every other
// source's lifetime is fixed by an external issuer or is an opaque static token,
// so Expiration is rejected for them.
//
// Username presents the credential as a username/password pair. When unset, the
// credential is used as a bearer token.
//
// Envelope is an optional transform applied to the resolved credential on top of
// whichever source is selected; see its field comment.
type RegistryCredential struct {
	// Type is the kind of credential material (CredentialTypeJWT).
	// +kubebuilder:validation:Enum=jwt
	Type string `json:"type"`

	// Provider mints a per-request credential for the audiences, one of
	// JWTProviderGitHub, JWTProviderForgejo, JWTProviderGCP, JWTProviderAzure,
	// JWTProviderAWS, or JWTProviderSPIFFE.
	// +optional
	// +kubebuilder:validation:Enum=github;forgejo;gcp;azure;aws;spiffe
	Provider string `json:"provider,omitempty"`

	// Value sends a static JWT read from the config.
	// +optional
	Value string `json:"value,omitempty"`

	// FromPath sends a static JWT read from the file at the path.
	// +optional
	FromPath string `json:"fromPath,omitempty"`

	// JWKPath signs a fresh JWT with the private JSON Web Key at the path.
	// +optional
	JWKPath string `json:"jwkPath,omitempty"`

	// JWKValue signs a fresh JWT with the private JSON Web Key read from the config.
	// +optional
	JWKValue string `json:"jwkValue,omitempty"`

	// Issuer is the issuer claim for jwkPath/jwkValue-signed tokens.
	// +optional
	Issuer string `json:"issuer,omitempty"`

	// Subject is the subject claim for jwkPath/jwkValue-signed tokens.
	// +optional
	Subject string `json:"subject,omitempty"`

	// Audiences is the audience claim. Defaults to Host.
	// +optional
	Audiences []string `json:"audiences,omitempty"`

	// Expiration is the jwkPath/jwkValue JWT lifetime. Defaults to 60s.
	// +optional
	Expiration *metav1.Duration `json:"expiration,omitempty"`

	// Username presents the credential as the password of a username/password
	// pair. When unset, the credential is used as a bearer token.
	// +optional
	Username string `json:"username,omitempty"`

	// Envelope transforms the resolved credential before it is sent to the
	// registry. It is a Go template whose data is a single Token field holding
	// the credential, with two functions available: base64 (standard base64)
	// and hex (lowercase hexadecimal). For example, "token-{{ hex .Token }}"
	// sends the hex-encoded credential with a token- prefix. An unset envelope
	// sends the credential unchanged.
	// +optional
	Envelope string `json:"envelope,omitempty"`
}

// EffectiveExpiration returns the jwkPath/jwkValue JWT lifetime with the
// documented default (60s) applied. Only meaningful for jwkPath/jwkValue
// credentials.
func (c RegistryCredential) EffectiveExpiration() time.Duration {
	if c.Expiration != nil {
		return c.Expiration.Duration
	}
	return defaultJWKExp
}

// EffectiveAudiences returns the audiences with the documented default (the
// host) applied.
func (h RegistryHost) EffectiveAudiences() []string {
	if h.Credential == nil || len(h.Credential.Audiences) == 0 {
		return []string{h.Host}
	}
	return h.Credential.Audiences
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

// TLSServerAuth verifies the registry's server certificate. With provider unset,
// FromPath or Value supply a custom CA bundle (one or more concatenated PEM
// certificates); with provider TLSProviderSPIFFE, SPIFFE verifies the server's
// X.509-SVID against the SPIFFE trust bundle. When ServerAuth is unset entirely,
// the system trust pool is used.
type TLSServerAuth struct {
	// Provider selects SPIFFE server verification (TLSProviderSPIFFE). Unset
	// selects the file-based CA bundle from FromPath or Value.
	// +optional
	// +kubebuilder:validation:Enum=spiffe
	Provider string `json:"provider,omitempty"`

	// FromPath reads the CA bundle from the file at the path.
	// +optional
	FromPath string `json:"fromPath,omitempty"`

	// Value inlines the PEM-encoded CA bundle.
	// +optional
	Value string `json:"value,omitempty"`

	// SPIFFE verifies the server's X.509-SVID against the SPIFFE trust bundle.
	// +optional
	SPIFFE *SPIFFETLS `json:"spiffe,omitempty"`
}

// TLSData is a single PEM-encoded public value (a client cert chain or a CA
// bundle) obtained from exactly one of FromPath or Value.
type TLSData struct {
	// FromPath reads the value from the file at the path.
	// +optional
	FromPath string `json:"fromPath,omitempty"`

	// Value inlines the PEM-encoded value.
	// +optional
	Value string `json:"value,omitempty"`
}

// TLSKey is a private key source. Unlike the public certificate and CA values it
// is a secret: exactly one of FromPath or Value.
type TLSKey struct {
	// FromPath reads the private key from the file at the path.
	// +optional
	FromPath string `json:"fromPath,omitempty"`

	// Value reads the private key from the config.
	// +optional
	Value string `json:"value,omitempty"`
}

// TLSClientAuth presents a client certificate (mTLS). With provider unset, the
// static Certificate and Key pair is used; with provider TLSProviderSPIFFE, a
// SPIFFE X.509-SVID from the Workload API is presented.
type TLSClientAuth struct {
	// Provider selects SPIFFE X.509-SVID client authentication
	// (TLSProviderSPIFFE). Unset selects the static Certificate and Key pair.
	// +optional
	// +kubebuilder:validation:Enum=spiffe
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

// ChartEntry mirrors a Helm chart from an HTTP/S repository to an OCI
// destination. To mirror a chart that is already in an OCI registry, use an
// ArtifactEntry instead (an OCI Helm chart is mirrored as a plain OCI artifact).
type ChartEntry struct {
	// Source is the HTTP/S repository URL the chart is mirrored from.
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
	if c.Version == "" {
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
	if s.SortBy == "" {
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
