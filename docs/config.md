---
linkTitle: Configuration
weight: 10
---

# Flux Mirror Config

The `Config` API defines what `flux mirror` mirrors and how it authenticates to
the registries involved. A single config lists the registry **hosts** to
authenticate, the **OCI artifacts** to copy between OCI registries, and the
**Helm charts** to pull from HTTP/S Helm repositories and publish to an OCI
registry. Config files support `${VAR}` environment substitution in field keys
and values; every referenced environment variable must be set when the config
is loaded. Use the global `--no-envsubst` flag to disable substitution and parse `${VAR}` strings literally.

Sources can be OCI registries or HTTP/S Helm repositories; destinations are
always OCI registries.

The config shape is published as a JSON Schema in
[`config-v1beta2.json`](config-v1beta2.json); the deprecated
[`config-v1beta1.json`](config-v1beta1.json) remains published for existing
configs. It is consumed by
[`flux mirror sync`](sync.md), [`flux mirror login`](login.md),
and [`flux mirror secret`](secret.md).

## Example

The following config mirrors a container image and a Helm chart, and
authenticates to the destination registry with a per-host credential:

```yaml
apiVersion: mirror.plugin.fluxcd.io/v1beta2
kind: Config
artifacts:
  - source: docker.io/stefanprodan/podinfo
    destination: quay.io/my-org/podinfo
    selector:
      semver: "6.x"
      limit: 2
    includeReferrers: true
    verify:
      provider: cosign
      minAge: 48h
      matchOIDCIdentity:
        - issuer: https://token.actions.githubusercontent.com
          subject: ^https://github\.com/stefanprodan/.*$
charts:
  - name: podinfo
    source: https://stefanprodan.github.io/podinfo
    destination: oci://quay.io/my-org/charts
    version: "6.x"
    limit: 2
hosts:
  - host: quay.io
    credential:
      username: 'my-org+robot-user'
      value: ${QUAY_TOKEN}
```

In the above example:

- The image at `docker.io/stefanprodan/podinfo` is mirrored to
  `quay.io/my-org/podinfo`. The two highest `6.x` tags are selected, their cosign
  signatures, SBOMs, and attestations are mirrored alongside them, and each tag
  is verified against the listed GitHub Actions OIDC identity (and required to be
  at least 48h old) before it is copied.
- The `podinfo` Helm chart is pulled from the HTTP/S repository at
  `https://stefanprodan.github.io/podinfo` and published as an OCI Helm chart to
  `quay.io/my-org/charts/podinfo`. The two highest `6.x` versions are mirrored.
- Requests to `quay.io` authenticate with the Quay robot token read from the
  `QUAY_TOKEN` environment variable, presented as a username/password pair
  because `credential.username` is set.

You can run this with:

```bash
flux mirror sync ./config.yaml
```

## Writing a Config spec

A config is a single YAML document with `apiVersion: mirror.plugin.fluxcd.io/v1beta2`,
`kind: Config`, and any of the `artifacts`, `charts`, and `hosts` lists.

At least one `artifacts` or `charts` entry is required, except for
[`flux mirror login`](login.md) and
[`flux mirror secret`](secret.md), which read only the `hosts` section
and accept a `hosts`-only config.

### API version

`mirror.plugin.fluxcd.io/v1beta2` is the current `apiVersion`. The previous
`mirror.plugin.fluxcd.io/v1beta1` is deprecated: a config that still declares it
still loads (it is converted to v1beta2 in memory), but prints a migration
warning. v1beta2 is a breaking change that adds an optional `credential.type`
(defaults to `jwt`), renames the credential claims (`aud`→`audiences`,
`iss`→`issuer`, `sub`→`subject`, `exp`→`expiration`), moves
`hosts[].username` under `credential`, renames the SPIFFE credential provider
(`jwt-svid`→`spiffe`), and replaces the `.tls` serverAuth/clientAuth blocks with
`.tls.path` (a directory),
inline `.ca.crt`, `.tls.crt`, and `.tls.key` values, or `.tls.spiffe.serverIDs`
(SPIFFE mutual TLS). Update the `apiVersion` and the
renamed fields to silence the warning. Inline v1beta1 `.tls` values and SPIFFE
`serverAuth` with `clientAuth: x509-svid` convert automatically; per-file paths,
one-sided SPIFFE, and `authorizeAny` fail with a migration error and must be
rewritten as `.tls.path`, inline certificate fields, or `.tls.spiffe`.

### Artifacts

`.artifacts` is an optional field that lists OCI artifacts to mirror between OCI
registries. The manifest and every referenced blob are copied byte-for-byte,
preserving digests; multi-arch images are mirrored faithfully as manifest lists,
with no platform filtering.

An entry handles any OCI-addressable content: container images, OCI Helm charts,
Flux OCI artifacts, or anything else stored in an OCI registry. To mirror a Helm
chart that is published to an HTTP/S Helm repository (and not yet in an OCI
registry), use [`.charts`](#charts) instead.

Each entry has two required fields:

- `.source`, the OCI repository the artifact is pulled from, written without a
  scheme (e.g. `ghcr.io/fluxcd/flux-cli`).
- `.destination`, the OCI repository the artifact is pushed to, written without a
  scheme. Selected tags are mirrored to the same tag at the destination.

It also has optional fields documented below: `.selector`
([Tag selection](#tag-selection)), `.includeReferrers` ([Referrers](#referrers)),
`.verify` ([Signature verification](#signature-verification)), `.overwrite` and
`.ignoreDriftForExitCode` ([Overwrite and drift behavior](#overwrite-and-drift-behavior)).

```yaml
artifacts:
  - source: ghcr.io/fluxcd/flux-cli
    destination: quay.io/example/flux-cli
    selector:
      semver: ">=2.7.0 <3.0.0"
      limit: 10
    includeReferrers: true
    ignoreDriftForExitCode: true
```

#### Tag selection

`.artifacts[].selector` is a required field that decides which tags of the
source repository are mirrored. It runs as a four-step pipeline — a regex
prefilter, a semver range filter, a sort, then a limit — always ordering highest
first and keeping the top N. The field offers the following subfields:

- `.regex`, an optional [Go regular expression](https://pkg.go.dev/regexp/syntax)
  prefilter applied to every tag before the sort and semver steps. It has a
  required `.pattern` (tags that do not match are dropped) and an optional
  `.extract` replacement string referencing named capture groups (e.g.
  `$version`). When `.extract` is set, the extracted value — not the raw tag — is
  used as the comparison key, so tags with prefixes or suffixes can still feed
  into a semver or numerical sort.
- `.semver`, an optional [semver range](https://github.com/Masterminds/semver#checking-version-constraints)
  (e.g. `>=2.7.0 <3.0.0` or `6.x`). Tags whose comparison key is not a valid
  semantic version are dropped.
- `.sortBy`, the sort strategy. The default is `semver`.
- `.limit`, how many tags to mirror, taken from the top of the sorted result. It
  defaults to `1`; `0` disables the cap and mirrors every matching tag.
- `.tags`, an optional list of exact tags to mirror. It replaces the pipeline
  and cannot be combined with `.regex`, `.semver`, `.sortBy` or `.limit`.

The supported sort strategies are:

- `semver` parses each tag as a semantic version and orders by semver
  precedence. Tags that do not parse are silently dropped; use `.regex` to
  control what qualifies.
- `alphabetical` orders tags lexicographically. This suits tags shaped like
  `RELEASE.2024-11-12T08-30-15Z`, where lexical order matches chronological
  order.
- `numerical` parses each tag as a number and orders numerically. Tags that do
  not parse are silently dropped; this is typically paired with `.regex` to
  extract a numeric portion from a composite tag.

For example, to select the highest five tags by an embedded timestamp:

```yaml
selector:
  regex:
    pattern: '^.+-[0-9a-f]+-(?P<ts>\d+)$'
    extract: '$ts'
  sortBy: numerical
  limit: 5
```

To mirror a fixed set of tags, list them in `.tags` instead:

```yaml
selector:
  tags: ["9.5.6", "9.5.5"]
```

With `.tags`, the source repository's tags are not listed, so planning cost
does not grow with the size of the repository. A listed tag that does not exist
at the source fails that tag's job.

#### Referrers

`.artifacts[].includeReferrers` is an optional field that, when `true`, also
mirrors the referrers of each selected tag — cosign signatures, SBOMs, and
attestations attached via the [OCI 1.1 referrers API](https://github.com/opencontainers/distribution-spec/blob/main/spec.md#listing-referrers).
It defaults to `false`.

#### Signature verification

`.artifacts[].verify` is an optional field that verifies the source artifact's
signature before any tag is copied. Every selected tag is verified at plan time;
if a selected tag has no matching valid signature, planning fails for that entry
and no copy job is scheduled. The field offers three subfields:

- `.provider`, the verification provider. It must be `cosign`. Verification uses
  [Cosign keyless bundles](https://docs.sigstore.dev) attached to the source
  artifact as OCI referrers, so the signing side must publish them as referrers.
- `.matchOIDCIdentity`, a list of accepted Fulcio certificate identities. Each
  entry provides an `.issuer` (the OIDC issuer URL, e.g.
  `https://token.actions.githubusercontent.com`) and a `.subject` (a Go regular
  expression matched against the certificate subject alternative name). The
  entries are evaluated in an OR fashion: a signature is accepted when any one
  identity matches.
- `.minAge`, an optional [duration](https://pkg.go.dev/time#ParseDuration) that
  requires a signature to be at least this old, measured from its verified
  transparency-log (Rekor) integrated timestamp. Tags whose signatures are valid
  but too recent are reported as `skipped` (reason `signature-too-new`) and not
  copied; signatures without an enforceable verified integrated timestamp fail
  verification. Use this to let signatures settle in the transparency log before
  mirroring.

```yaml
artifacts:
  - source: ghcr.io/stefanprodan/podinfo
    destination: quay.io/my-org/podinfo
    selector:
      semver: "*"
      limit: 1
    includeReferrers: true
    verify:
      provider: cosign
      minAge: 168h # 7 days
      matchOIDCIdentity:
        - issuer: https://token.actions.githubusercontent.com
          subject: ^https://github\.com/stefanprodan/.*$
```

### Charts

`.charts` is an optional field that lists Helm charts to mirror from an HTTP/S
Helm repository to an OCI destination. For each selected version, the chart
`.tgz` is downloaded from the source and re-published to the destination as a
Helm OCI artifact (a config blob with the chart metadata and a layer with the
tarball bytes). Charts use the same outcomes and overwrite/dry-run semantics as
artifacts; drift is detected by comparing the source tarball's content digest
against the destination chart-layer digest, so re-runs against an unchanged
source are idempotent.

Each entry offers the following subfields:

- `.name`, the chart name within the source repository. Required.
- `.source`, the HTTP/S Helm repository URL the chart is pulled from. Required;
  the scheme must be `http` or `https`, and the repository must serve a classic
  `index.yaml` plus chart tarballs.
- `.destination`, the OCI base URL the chart is published to. Required; the
  scheme must be `oci`.
- `.version`, an optional [semver range](https://github.com/Masterminds/semver#checking-version-constraints)
  (e.g. `>=2.7.0 <3.0.0`). Versions outside the range are dropped. It defaults to
  `*` (all versions).
- `.limit`, how many matching versions to mirror, highest first. It defaults to
  `1`; `0` disables the cap.
- `.overwrite`, see [Overwrite and drift behavior](#overwrite-and-drift-behavior).

Helm repository authentication is **not** configured under `hosts`. It is loaded
automatically from the ambient Helm repositories config — Helm's default
`repositories.yaml`, or `$HELM_REPOSITORY_CONFIG` when set — so adding the
repository with `helm repo add` is enough for `flux mirror` to pick up matching
credentials. To mirror a chart that already lives in an OCI registry, list it
under [`.artifacts`](#artifacts) instead, where it is copied OCI-to-OCI as a
plain OCI artifact (authenticated through [`hosts`](#hosts)).

The chart `.name` is appended to `.destination` automatically: for `destination:
oci://quay.io/example/charts` and `name: nginx`, versions land at
`quay.io/example/charts/nginx:<version>`. Because OCI tags do not accept `+`,
semver build metadata (e.g. `1.2.3+meta`) is encoded with `_` in the destination
tag (`1.2.3_meta`); listing and comparison treat both forms as the same version.

```yaml
charts:
  - name: nginx
    source: https://charts.example.com
    destination: oci://quay.io/example/charts
    version: ">=15.0.0 <16.0.0"
    limit: 5
```

### Hosts

`.hosts` is an optional field that configures per-host authentication for OCI
registry requests, covering both source pulls and destination pushes. A host
listed here takes priority over the ambient Docker config; requests to hosts that
are not listed fall back to the Docker config and its credential helpers
(`~/.docker/config.json`, or `$DOCKER_CONFIG` when set).

> **Note:** the `hosts` section does not apply to HTTP/S Helm repositories
> (`.charts[].source`), which authenticate through the ambient Helm repositories
> config.

Each entry has one required field, `.host`, the registry host (with optional
port) the entry applies to (e.g. `registry.example.com` or `localhost:5000`).
A host may appear more than once only when the entries are split by
[`.direction`](#host-direction); overlapping entries are rejected. Beyond that,
an entry configures a
[`.credential`](#per-host-credential) and/or a [`.tls`](#transport-tls) block, or
a cloud [`.provider`](#cloud-registry-providers). A cloud `provider` is mutually
exclusive with `tls`, because a cloud registry is managed and its transport is
not customized; the default `generic` provider is an ordinary registry and
composes with both. A cloud `provider` may also carry a `credential`: its
identity is then exchanged for the cloud provider's registry credentials
instead of using the ambient cloud identity. On a cloud `provider` host the
credential must use `value`, `fromPath`, `jwkPath`, `jwkValue`, or
`provider: github|forgejo|spiffe`; `username`, `envelope`, and
`provider: gcp|azure|aws` are rejected — see
[Cloud registry providers](#cloud-registry-providers).
[`.maxChunkSize`](#blob-upload-chunking) tunes
blob uploads independently of auth. At least one of `credential`, a cloud
`provider`, `tls`, or `maxChunkSize` must be set.

```yaml
hosts:
  # A per-host credential (HTTP-layer token).
  - host: registry.example.com
    credential:
      provider: github
  # A cloud registry provider, via ambient workload identity.
  - host: 123456789012.dkr.ecr.us-east-1.amazonaws.com
    provider: ecr
  # A cloud registry provider authenticated with a GitHub credential.
  - host: myregistry.azurecr.io
    provider: acr
    acr:
      tenantID: ${AZURE_TENANT_ID}
      clientID: ${AZURE_CLIENT_ID}
    credential:
      provider: github
      audiences:
        - api://AzureADTokenExchange
```

#### Host direction

`.hosts[].direction` limits an entry to one side of a mirror:

- `both` (default) applies the entry to source pulls and destination pushes.
- `source` applies it only to source pulls.
- `destination` applies it only to destination pushes.

This is what lets the same registry host carry different credentials for the
source and destination, for example when copying between two repositories on
`ghcr.io`.

```yaml
hosts:
  - host: ghcr.io
    direction: source
    credential:
      value: ${LEGACY_GHCR_TOKEN}
  - host: ghcr.io
    direction: destination
    credential:
      provider: github
```

#### Per-host credential

`.hosts[].credential` configures an HTTP-layer registry credential for the host.
`.type` is optional and selects the credential material. It defaults to `jwt`;
only `jwt` is supported today, with `x509` reserved for a future release.
Exactly one **token source** subfield selects how the credential is obtained:

- `.provider`, mints a fresh, per-request credential for the audiences using a
  cloud, CI, or SPIFFE identity, then caches and refreshes it on demand. One of
  `github`, `forgejo`, `gcp`, `azure`, `aws`, or `spiffe` — see
  [Credential providers](#credential-providers) for what each obtains and what the registry
  must accept.
- `.value`, sends the JSON Web Token configured inline as-is (e.g. a GitLab CI/CD `id_token`). Use `${VAR}` to substitute it from the environment while loading the config.
- `.fromPath`, sends the token read from the file at the path, with surrounding
  whitespace trimmed. On a `generic` host the file is re-read on every request,
  so the token can be rotated without restarting (useful for a projected
  ServiceAccount token); on a cloud `provider` host it is cached until near
  expiration.
- `.jwkPath`, signs a JWT with the private JSON Web Key in the file at the path.
  On a `generic` host a fresh JWT is signed per request; on a cloud `provider`
  host it is cached until near expiration. The key may be a bare JWK or a
  single-key JWK set (`{"keys":[...]}`), and its `kid` is carried in the JWT
  header. Generate a key pair with [`flux mirror keygen`](keygen.md).
- `.jwkValue`, signs a JWT with the private JSON Web Key configured inline, with
  the same per-request versus cached behavior as `.jwkPath`. Use `${VAR}` to
  substitute it from the environment while loading the config.

> **Cloud provider hosts:** `value`, `fromPath`, `jwkPath`, `jwkValue`, and
> `provider: github|forgejo|spiffe` are exchanged for the cloud registry
> credentials. `provider: gcp|azure|aws`, `username`, and `envelope` are rejected
> during config validation — see
> [Cloud registry providers](#cloud-registry-providers).

The signed and minted sources take additional claim subfields:

- `.issuer` and `.subject`, the issuer and subject claims. Both are **required**
  with `.jwkPath` or `.jwkValue`, and not allowed with the other sources.
- `.audiences`, the audience claim as a list. Allowed with `.jwkPath`,
  `.jwkValue`, or `.provider`, and defaults to the host. The audience pins the
  credential to a specific registry, so it must match what the registry (or the
  cloud identity provider) expects. On a cloud `provider` host, an unset
  `.audiences` uses the cloud provider's own default (`sts.amazonaws.com` for
  ECR, `api://AzureADTokenExchange` for ACR, and the workload identity provider's
  default audience for GAR) instead of the host. Every source accepts multiple
  values except the `gcp` and `azure` providers, which mint a token with a single
  `aud` and therefore reject a list longer than one.
- `.expiration`, the signed JWT lifetime, as a
  [duration](https://pkg.go.dev/time#ParseDuration). Allowed only with `.jwkPath`
  or `.jwkValue` — the sources whose lifetime `flux mirror` controls — and defaults
  to `60s`. A longer-lived token is cached and re-minted at half its lifetime.
  Every other source's lifetime is fixed by its issuer.

Two subfields apply to every token source:

- `.envelope`, an optional Go template applied to the resolved credential just
  before it is sent to the registry, for registries that expect the token in a
  specific serialized form. The template data is a single `.Token` field holding
  the credential, and two functions are available: `base64` (standard base64)
  and `hex` (lowercase hexadecimal). For example,
  `envelope: "token-{{ hex .Token }}"` sends the hex-encoded credential with a
  `token-` prefix. The envelope applies to both
  [transport styles](#bearer-token-vs-usernamepassword): it transforms the bearer
  token, or the password when `.credential.username` is set. An unset envelope
  sends the credential unchanged. It is only valid on a `generic` (or unset)
  `hosts[].provider`; it is rejected with a cloud `provider`.
- `.username`, controls how the resolved credential is transported, and therefore
  what the registry must accept — see
  [Bearer token vs. username/password](#bearer-token-vs-usernamepassword). It is
  only valid on a `generic` (or unset) `hosts[].provider`; it is rejected with a
  cloud `provider`.

##### Credential providers

When `.credential.provider` is set, the credential is obtained from the
provider's ambient identity for the audiences (defaulting to the host). This is
what the registry on the other side has to accept. On a cloud `provider` host,
only `github`, `forgejo`, and `spiffe` can be exchanged; `gcp`, `azure`, and
`aws` are rejected during config validation.

- `github` and `forgejo` request an OIDC ID token from the CI Actions OIDC
  endpoint (`ACTIONS_ID_TOKEN_REQUEST_URL` / `ACTIONS_ID_TOKEN_REQUEST_TOKEN`).
  The registry must trust the GitHub/Forgejo Actions issuer and match the token's
  audience and subject (e.g. `repo:my-org/my-repo:ref:refs/heads/main`). On a
  cloud `provider` host (`ecr`, `acr`, or `gar`), the same token is instead
  exchanged with the cloud provider for registry credentials, so the cloud
  identity provider must trust the GitHub/Forgejo Actions issuer. The two mint
  tokens the same way today but are kept distinct so each platform can diverge.
  Each audience is sent as a repeated `audience` query parameter; how the
  endpoint folds them into the token is up to the provider.
- `gcp` obtains a Google ID token for the audience via Application Default
  Credentials (the GKE/GCE metadata server, a service account key, or workload
  identity federation). The registry must trust Google's OIDC issuer and the
  configured audience. User credentials (`authorized_user`, e.g. from
  `gcloud auth application-default login`) cannot mint a token for a custom
  audience: with `.audiences` set the run fails, and with `.audiences` unset it
  falls back to the default-audience ID token carried in the ADC response, whose
  audience is the gcloud OAuth client ID and whose identity is the signed-in
  user.
- `azure` obtains a Microsoft Entra ID access token via the default Azure
  credential chain (AKS/managed identity, workload identity federation,
  environment credentials). The audience is requested as the `<aud>/.default`
  scope, so `.audiences` must contain a single application ID URI (or client ID)
  of a registered Entra application the registry validates tokens against.
- `aws` is not an OIDC token. AWS mints no JWT, so `flux mirror` signs an
  `sts:GetCallerIdentity` request with the ambient role credentials (IRSA, EC2
  instance role, environment, ...) and wraps it in a JWT-shaped envelope whose
  header is `{"alg":"none","typ":"aws-sigv4-getcalleridentity"}`. The audiences
  are carried as signed `X-Audience` header values, not as an OIDC audience
  claim: the registry accepts the request only if the signed list contains an
  identity it recognizes, so a single audience ties the request to that one
  registry while several authorize each registry listed. The registry verifies
  the caller by replaying the signed request to AWS STS and reading the returned
  account/ARN, so the destination **must** understand this scheme — a plain OIDC
  registry will not.
- `spiffe` fetches a JWT-SVID from the SPIFFE Workload API
  (`SPIFFE_ENDPOINT_SOCKET`) for the audiences and sends it as the credential.
  The extra audiences are carried as additional JWT-SVID audiences. The registry
  must trust the SPIFFE trust domain's JWT bundle. On a cloud provider host, the
  JWT-SVID is exchanged with the cloud provider, which must trust the SPIFFE
  trust domain. This is the HTTP-layer counterpart to the transport-layer SPIFFE
  X.509-SVID mTLS configured under [`.tls`](#transport-tls); the two are
  independent.

##### Resolving file paths

`.fromPath`, `.jwkPath`, and the `.tls.path` directory are resolved relative to
the config file's directory, and a config may only reference files under its own
directory tree. When the config is read from stdin (`-f -`), the process working
directory is used as the confinement root instead.

##### Bearer token vs. username/password

`.credential.username` controls how the resolved credential is
transported, and therefore what the registry on the other side must accept:

- **`credential.username` unset (default)** — the credential is treated as a **bearer
  token**. `sync` sends it as an HTTP Bearer credential on every request, with no
  auth challenge, and `login`/`secret` write it to the Docker config's
  `registrytoken` field. This suits registries that natively validate an OIDC
  token (or another self-contained bearer credential). `registrytoken` is
  understood by [go-containerregistry](https://github.com/google/go-containerregistry)
  (crane, and Flux's `OCIRepository`/`HelmRepository`), but **not** by `kubelet`
  image pulls. Because credential helpers only store username/secret pairs, a
  `registrytoken` is always written to the config file, never to a keychain
  helper.
- **`credential.username` set** — the credential becomes the **password** of a
  username/password pair. `sync` goes through the standard registry auth
  challenge (credentials are exchanged at the token endpoint, like the cloud
  providers), and `login`/`secret` write `username`/`password`/`auth`. Use this
  for registries such as Docker Hub, GHCR, and Quay, which expect a
  username/password login even when the username is a placeholder the registry
  ignores.

Choose `credential.username` deliberately: set it when the registry expects a
username/password login (or the credential will be consumed by `kubelet`), and
leave it unset when the registry validates a self-contained bearer token.

#### Cloud registry providers

`.hosts[].provider` selects how the host's registry credentials are obtained. It
defaults to `generic`, an ordinary registry for which `credential` and/or `tls`
configure the connection directly. The other values authenticate the host with a
cloud registry provider's workload identity — the same mechanism the
`flux push artifact` family uses — and obtain the registry's native credentials
directly. A cloud provider is mutually exclusive with `tls`; it composes with a
`credential`, whose identity is exchanged for the cloud provider's registry
credentials instead of using the ambient cloud identity.

##### Credential combinations

A cloud `provider` accepts only an OIDC/JWT credential source, because the
credential is exchanged with the cloud provider. The allowed combinations are:

| `hosts[].credential` | With `hosts[].provider: ecr`, `acr`, or `gar` |
| --- | --- |
| unset | Allowed. Uses the ambient cloud identity. |
| `provider: github` or `forgejo` | Allowed. Exchanged via the CI Actions OIDC token. |
| `provider: spiffe` | Allowed. Exchanged as a JWT-SVID. |
| `value` | Allowed. Exchanged as an OIDC token. |
| `fromPath` | Allowed. Exchanged as an OIDC token. |
| `jwkPath` or `jwkValue` | Allowed with the required `issuer` and `subject`; optional `audiences` and `expiration`. The signed JWT is exchanged as an OIDC token. |
| `provider: gcp`, `azure`, or `aws` | Rejected. |
| `username` | Rejected; the cloud provider determines the registry username. |
| `envelope` | Rejected; the exchange requires the raw JWT. |
| any other combination | Rejected during config validation. |

`credential.type`, when set, must still be `jwt`. Tokens from `value`,
`fromPath`, `jwkPath`, and `jwkValue` must be JWTs with an `exp` claim. When
`audiences` is unset, the credential is minted for the cloud provider's
federation audience instead of the host: `sts.amazonaws.com` for ECR,
`api://AzureADTokenExchange` for ACR, and the workload identity provider's
default audience for GAR.

`flux-mirror` caches the resolved token for a cloud host and refreshes it when
it approaches expiration (80% of its lifetime, capped by
`--token-cache-max-duration`), so a `fromPath` file or JWK is not re-read or
re-signed on every request. Set `--token-cache-max-size 0` to disable the cache.

The supported `hosts[].provider` values are:

- `generic` (default), an ordinary registry whose authentication is configured
  through `credential` and/or `tls`. It is the implicit value when `provider` is
  omitted, and composes with both.
- `ecr`, for Amazon ECR. It uses the AWS credential chain (IRSA, EC2 instance
  role, environment, SSO), reading the region from the host. With a
  `credential`, the identity is exchanged via `AssumeRoleWithWebIdentity` (set
  `.audiences` to the role's expected audience, typically `sts.amazonaws.com`,
  and grant the role trust to the credential issuer).
- `acr`, for Azure ACR. It uses the default Azure credential chain (managed
  identity, workload identity, environment, ...). With a `credential`, the
  identity is exchanged as a client assertion, typically with the
  `api://AzureADTokenExchange` audience.
- `gar`, for Google GAR. It uses Google Application Default Credentials. With a
  `credential`, the identity is exchanged through Google Workload Identity
  Federation.

When a cloud `provider` carries a `credential`, the matching `.ecr`, `.acr`, or
`.gar` block is required:

- `.ecr.roleARN` is the IAM role assumed with the credential identity via
  `AssumeRoleWithWebIdentity`. The region is derived from the ECR host, as in
  the rest of Flux. `.ecr.stsEndpoint` optionally overrides the STS endpoint.
- `.acr.tenantID` and `.acr.clientID` identify the Microsoft Entra application
  used as the client assertion.
- `.gar.workloadIdentityProvider` is the workload identity pool provider
  resource name; `.gar.gcpServiceAccount` optionally impersonates a service
  account. The built-in GKE workload identity pool only trusts GKE
  ServiceAccount tokens, so an external credential provider always needs an
  explicit `.gar` block.

```yaml
hosts:
  - host: 123456789012.dkr.ecr.us-east-1.amazonaws.com
    provider: ecr
    ecr:
      roleARN: arn:aws:iam::123456789012:role/flux-mirror
      # stsEndpoint: https://sts.us-east-1.amazonaws.com
    credential:
      provider: github
      audiences:
        - sts.amazonaws.com
  - host: myregistry.azurecr.io
    provider: acr
    acr:
      tenantID: ${AZURE_TENANT_ID}
      clientID: ${AZURE_CLIENT_ID}
    credential:
      provider: forgejo
  - host: us-docker.pkg.dev
    provider: gar
    gar:
      workloadIdentityProvider: projects/123/locations/global/workloadIdentityPools/gh/providers/gh
      # gcpServiceAccount: flux-mirror@my-project.iam.gserviceaccount.com
    credential:
      provider: github
```

The cloud providers' resolved credentials are presented to the registry as a
username/password pair through the standard registry auth challenge, and written
as `username`/`password`/`auth` by [`login`](login.md) and
[`secret`](secret.md).

#### Transport TLS

`.hosts[].tls` configures transport-layer TLS for the host's registry requests,
separately from the HTTP-layer `credential`. It is applied by `sync` (which
connects to the registry); `login` and `secret` do not open registry connections
and ignore it. It is not allowed on a cloud `provider` host. Exactly one of the
following must be set:

- `.path`, a directory laid out like a mounted Flux `certSecretRef` Secret:
  - `ca.crt` supplies the CA bundle used to verify the registry's server
    certificate (one or more concatenated PEM certificates).
  - `tls.crt` and `tls.key` supply a client certificate and key for mTLS.

  At least one of `ca.crt` or the `tls.crt`/`tls.key` pair must be present, and
  the two files of the pair must be set together. Legacy Flux key names
  (`caFile`, `certFile`, `keyFile`) are accepted as fallbacks, with the standard
  names taking precedence. The directory is read with the same key names and
  validation as the Flux controllers; relative paths are resolved against the
  config file's directory and confined within it, exactly like the credential
  file paths.
- inline PEM values, set directly as `.ca.crt`, `.tls.crt`, and `.tls.key`.
  Use these when the PEM material is already available to the config, for
  example through `${VAR}` substitution, and cannot be mounted as a directory.
  `tls.crt` and `tls.key` must be set together, and at least one of `ca.crt` or
  the pair must be present.
- `.spiffe`, SPIFFE mutual TLS. `.spiffe.serverIDs` is required and lists the
  registry server identities to authorize. The client X.509-SVID and trust
  bundle come from the ambient Workload API socket (`SPIFFE_ENDPOINT_SOCKET`) and
  rotate automatically.

SPIFFE is always mutual TLS: when `.spiffe` is set, flux-mirror presents its own
X.509-SVID as the client certificate as part of the handshake. The independent
server/client halves of earlier revisions are gone, as is client-only SPIFFE.

Each `.spiffe.serverIDs` entry accepts the
[SPIFFE ID formats defined by Flux RFC-0014](/flux/integrations/spiffe-mtls/).

```yaml
hosts:
  # Custom CA for server verification and a static client cert for mTLS.
  - host: registry.example.com
    tls:
      path: ./certs # contains ca.crt, tls.crt, and tls.key

  # CA-only: verify the server with a custom CA, no client certificate.
  - host: ca-only.example.com
    tls:
      path: ./certs/ca-only # contains ca.crt

  # Inline PEM values, e.g. injected from CI secrets.
  - host: inline.example.com
    tls:
      ca.crt: ${REGISTRY_CA_PEM}
      tls.crt: ${REGISTRY_CLIENT_CERT_PEM}
      tls.key: ${REGISTRY_CLIENT_KEY_PEM}

  # SPIFFE mutual TLS: X.509-SVID client cert and SPIFFE server verification.
  - host: spiffe.example.com
    tls:
      spiffe:
        serverIDs:
          - spiffe://example.org/registry       # exact SPIFFE ID
          # - spiffe://example.org/registry/    # any ID under this prefix
          # - spiffe://example.org              # any SVID in this trust domain
          # - spiffe://self                     # any SVID in our own trust domain
```

#### Blob upload chunking

`.hosts[].maxChunkSize` is the maximum size, in KiB (1024 bytes), of a
blob-upload `PATCH` request to this host; larger blobs are split into chunked
uploads. It defaults to `0`, which disables chunking and sends one monolithic
`PATCH` per blob. Set it for registries or proxies that cap request body sizes.

## Working with Config

### Overwrite and drift behavior

`.artifacts[].overwrite` and `.charts[].overwrite` are optional fields that
control what happens when a tag or version already exists at the destination.
`flux mirror` does not overwrite by default, which keeps the safe path the
default on immutable registries (ECR with `IMMUTABLE` tag mutability, GAR with
tag immutability, Harbor with retention rules) and avoids redundant writes on
mutable ones.

When a tag already exists at the destination, the source and destination digests
are compared:

- **Same digest** — skipped silently.
- **Different digest, `overwrite: false`** — a warning is logged and the tag is
  left alone, reported as `drifted`. The mirror has diverged from source but is
  not brought back in sync without explicit consent.
- **Different digest, `overwrite: true`** — the new digest is pushed, replacing
  the destination tag. This fails if the destination registry enforces tag
  immutability.

The drift warning is useful even on immutable registries where the divergence
cannot be resolved automatically: it surfaces in the run output and the
[exit code](sync.md#exit-codes), which audit and alerting can hook
into. The [`--overwrite`](sync.md#flags) CLI flag forces `overwrite:
true` for every entry, overriding per-entry values, for one-off resyncs.

`.artifacts[].ignoreDriftForExitCode` is an optional boolean, `false` by default. When
`true`, drifted tags for that entry are still logged and reported as `drifted`,
but they do not contribute to the drift exit code. If every drifted tag belongs
to an entry with `ignoreDriftForExitCode: true`, the run exits `0` and
`--drift-exit-code` is not applied. Use it for an artifact whose destination is
expected to diverge, without muting drift for the rest of the config.

With `includeReferrers: true`, the same rule applies to referrers. Referrers
missing at the destination are always mirrored (there is nothing to overwrite) —
the common case when an upstream signature is published after the artifact was
first mirrored. Referrers that exist with a different digest are skipped
(default) or replaced (`overwrite: true`).

### Defaults

| Field                                | Default    |
|--------------------------------------|------------|
| `artifacts[].selector.sortBy`        | `semver`   |
| `artifacts[].selector.limit`         | `1`        |
| `artifacts[].overwrite`              | `false`    |
| `artifacts[].includeReferrers`       | `false`    |
| `artifacts[].ignoreDriftForExitCode` | `false`    |
| `artifacts[].verify`                 | unset      |
| `charts[].version`                   | `*`        |
| `charts[].limit`                     | `1`        |
| `charts[].overwrite`                 | `false`    |
| `hosts[].direction`                  | `both`     |
| `hosts[].provider`                   | `generic`  |
| `hosts[].ecr`                        | unset      |
| `hosts[].acr`                        | unset      |
| `hosts[].gar`                        | unset      |
| `hosts[].credential.audiences`       | `host`     |
| `hosts[].credential.expiration`      | `60s`      |
| `hosts[].credential.envelope`        | unset      |
| `hosts[].maxChunkSize`               | `0`        |

A `limit` of `0` disables the cap and mirrors every matching tag or version. Use
it with care: unrestricted mirrors can consume significant bandwidth and storage
and may trip rate limits on upstream registries.
