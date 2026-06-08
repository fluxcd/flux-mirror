# Flux Mirror Config

`flux-mirror` is configured via a YAML file listing the OCI artifacts and Helm charts to mirror.
Sources can be OCI registries or HTTP/S Helm repositories; destinations are OCI registries.

The config shape is published as a JSON Schema in [`config-v1beta1.json`](config-v1beta1.json).

## Example

```yaml
apiVersion: mirror.fluxcd.io/v1beta1
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
      fromEnv: 'QUAY_TOKEN'
```

## Specification

| Field        | Type   | Default | Description                                                                                 |
|--------------|--------|---------|---------------------------------------------------------------------------------------------|
| `apiVersion` | string |         | Must be `mirror.fluxcd.io/v1beta1`.                                                         |
| `kind`       | string |         | Must be `Config`.                                                                           |
| `artifacts`  | list   | `[]`    | OCI artifacts (images, OCI Helm charts, Flux artifacts, etc.). See [Artifacts](#artifacts). |
| `charts`     | list   | `[]`    | Helm charts from HTTP/S or OCI Helm repositories. See [Charts](#charts).                    |
| `hosts`      | list   | `[]`    | Per-host authentication for OCI registry requests. See [Hosts](#hosts).                     |

At least one of `artifacts` or `charts` must be set, except for
[`flux-mirror login`](../guides/login.md), which reads only the `hosts` section and
accepts a `hosts`-only config.

## Artifacts

The `artifacts` section mirrors OCI artifacts between OCI registries;
the manifest and all referenced blobs are copied byte-for-byte, preserving digests.
Multi-arch images are mirrored faithfully as manifest lists; no platform filtering is performed.

This section handles any OCI-addressable artifact: container images, OCI Helm
charts, Flux OCI artifacts, or anything else stored in an OCI registry.

### Artifact Fields

| Field              | Type   | Default | Description                                                                                                                                                                    |
|--------------------|--------|---------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `source`           | string |         | Source OCI reference, without scheme (e.g. `ghcr.io/fluxcd/flux-cli`).                                                                                                         |
| `destination`      | string |         | Destination OCI reference, without scheme.                                                                                                                                     |
| `selector`         | object |         | Tag selection policy. See [Selector](#selector).                                                                                                                               |
| `overwrite`        | bool   | `false` | If true, replace tags whose destination digest differs from source. See [Overwrite](#overwrite-behavior).                                                                      |
| `includeReferrers` | bool   | `false` | If true, also mirror referrers of each tag (cosign signatures, SBOMs, attestations) via the OCI 1.1 API.                                                                       |
| `verify`           | object |         | Optional source artifact signature verification. When set, selected tags are verified before any copy job is scheduled. See [Signature verification](#signature-verification). |

### Selector

The `selector` block decides which tags to mirror. It runs as a four-step
pipeline: regex prefilter, semver range filter, sort, then cap.

| Field    | Type   | Default  | Description                                                                                                    |
|----------|--------|----------|----------------------------------------------------------------------------------------------------------------|
| `regex`  | object |          | Optional regex prefilter applied before sort or version matching. See [Regex Filter](#regex-filter).           |
| `semver` | string |          | Optional semver range. Tags whose comparison key is not a valid semver are dropped.                            |
| `sortBy` | string | `semver` | Sort strategy: `semver`, `alphabetical`, or `numerical`.                                                       |
| `limit`  | int    | `1`      | Number of tags to mirror, taken from the top of the sorted result (highest first). `0` disables the cap.       |

The pipeline always orders highest first and takes the top N.

#### Sort strategies

- `semver`: tags are parsed as semantic versions and sorted by semver
  precedence. Tags that don't parse are silently dropped. Use `regex` to
  control what qualifies.
- `alphabetical`: tags are sorted lexicographically. Useful for tags shaped
  like `RELEASE.2024-11-12T08-30-15Z` where lexical order matches chronological.
- `numerical`: tags are parsed as numbers and sorted numerically. Tags that
  don't parse are silently dropped. Typically paired with `regex` to extract
  a numeric portion from a composite tag.

#### Regex filter

Applies a Go regular expression to each tag before sort. A named capture
group can extract a substring used as the comparison key, so tags with
prefixes or suffixes can still feed into semver or numerical sort.

| Field     | Type   | Description                                                                                                                                            |
|-----------|--------|--------------------------------------------------------------------------------------------------------------------------------------------------------|
| `pattern` | string | Go regular expression. Tags not matching the pattern are dropped.                                                                                      |
| `extract` | string | Replacement string referencing named capture groups (e.g. `$version`). The extracted value is used for sort and the `semver` filter, not the raw tag.  |

#### Image with Semver tags

Mirror the `flux-cli` container image, selecting versions in the `2.x` range:

```yaml
artifacts:
  - source: ghcr.io/fluxcd/flux-cli
    destination: quay.io/example/flux-cli
    selector:
      semver: ">=2.7.0 <3.0.0"
      limit: 10
    includeReferrers: true
```

`sortBy` defaults to `semver`. Up to 10 of the highest tags in the range are
mirrored, along with any cosign signatures, SBOMs, or attestations attached
via the referrers API.

#### Image with CI build tags

Some images use build tags of the form `<branch>-<short-sha>-<timestamp>`
(e.g. `main-a1b2c3d-1731420000`). To select by the timestamp portion,
extract it with `regex` and sort numerically:

```yaml
artifacts:
  - source: ghcr.io/example/nightly-build
    destination: quay.io/example/nightly-build
    selector:
      regex:
        pattern: '^.+-[0-9a-f]+-(?P<ts>\d+)$'
        extract: '$ts'
      sortBy: numerical
      limit: 5
```

The regex matches tags like `main-a1b2c3d-1731420000` and uses `1731420000`
as the sort key. The 5 most recent builds by timestamp are mirrored; tags
that don't match the pattern are dropped.

### Signature verification

Set `verify.provider: cosign` to verify selected source tags before they are
mirrored. Verification uses Cosign v3 bundles attached to the source
artifact as OCI referrers. If any selected tag has no matching valid signature,
planning fails for that artifact entry and no copy job is scheduled for it.

| Field               | Type     | Description                                                                                                 |
|---------------------|----------|-------------------------------------------------------------------------------------------------------------|
| `provider`          | string   | Must be `cosign`.                                                                                           |
| `minAge`            | duration | Optional minimum age since the verified Rekor integrated timestamp. Tags with newer signatures are skipped. |
| `matchOIDCIdentity` | list     | One or more OIDC identity matchers accepted for the signing certificate.                                    |
| `issuer`            | string   | OIDC issuer URL, e.g. `https://token.actions.githubusercontent.com`.                                        |
| `subject`           | string   | Go regexp matched against the signing certificate subject alternative name.                                 |

Multiple `matchOIDCIdentity` entries are treated as alternatives; a signature
is accepted when any identity matches.

When `minAge` is set, the signature must contain a verified transparency-log
integrated timestamp old enough to satisfy the duration. Tags with valid but
too-recent signatures are reported as `skipped` and are not copied; signatures
without an enforceable verified integrated timestamp fail verification.

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
      minAge: 168h # 7 days old
      matchOIDCIdentity:
        - issuer: https://token.actions.githubusercontent.com
          subject: ^https://github\.com/stefanprodan/.*$
```

## Charts

The `charts` section mirrors Helm charts from HTTP/S Helm repositories or OCI
Helm registries to an OCI destination. For each selected version, the chart
`.tgz` is downloaded from the source and re-published to the destination as
a Helm OCI artifact (config blob with the chart metadata, layer with the
tarball bytes).

Charts use the same outcomes (`copied`, `skipped`, `overwritten`, `drifted`)
and the same `--overwrite` / `--dry-run` semantics as artifacts. Drift is
detected by comparing the source tarball's content digest against the
destination chart-layer digest, so re-runs against an unchanged source are
idempotent.

### Chart Fields

| Field         | Type   | Default | Description                                                                                                                     |
|---------------|--------|---------|---------------------------------------------------------------------------------------------------------------------------------|
| `name`        | string |         | Chart name within the source repository.                                                                                        |
| `source`      | string |         | Source repository URL. Scheme must be `http`, `https`, or `oci`.                                                                |
| `destination` | string |         | Destination OCI base URL. Scheme must be `oci`. The chart `name` is appended automatically.                                     |
| `version`     | string | `*`     | Semver constraint (e.g. `>=2.7.0 <3.0.0`). Versions outside the range are dropped.                                              |
| `limit`       | int    | `1`     | Number of versions to mirror, taken from the highest matching versions. `0` disables the cap.                                   |
| `overwrite`   | bool   | `false` | If true, replace the destination tag when its chart-layer digest differs from the source. See [Overwrite](#overwrite-behavior). |

### Source scheme

- `http` / `https`: classic Helm repository serving `index.yaml` plus chart
  tarballs. Auth is loaded automatically from the ambient Helm repositories
  config (`repositories.yaml`, or `HELM_REPOSITORY_CONFIG` when set); no auth
  fields are needed in the flux-mirror YAML.
- `oci`: OCI Helm registry. Each chart name is its own OCI repository at
  `<source>/<name>`. Tags whose OCI manifest is not a Helm chart (cosign
  signatures, SBOMs, other artifacts in the same repository) are filtered
  out before the semver constraint is applied. Authentication uses the
  ambient Docker config.

### Destination convention

`destination` is the parent path; the chart `name` is appended automatically.
For `destination: oci://quay.io/example/charts` and `name: nginx`, versions
land at `quay.io/example/charts/nginx:<version>`.

OCI tags do not accept `+`, so semver build metadata (e.g. `1.2.3+meta`) is
encoded as `_` in the destination tag (`1.2.3_meta`). Listing and comparison
treat both forms as the same version.

#### HTTP Helm repository

```yaml
charts:
  - name: nginx
    source: https://charts.example.com
    destination: oci://quay.io/example/charts
    version: ">=15.0.0 <16.0.0"
    limit: 5
```

The 5 highest `15.x` versions of the `nginx` chart are mirrored to
`quay.io/example/charts/nginx:<version>`.

#### OCI Helm registry

```yaml
charts:
  - name: my-app
    source: oci://ghcr.io/example/charts
    destination: oci://quay.io/example/charts
    version: ">=1.0.0"
    limit: 3
```

The source repo is resolved to `ghcr.io/example/charts/my-app`; the
destination is `quay.io/example/charts/my-app:<version>`.

## Hosts

The optional `hosts` section configures per-host registry authentication. A host
listed here takes priority over Docker config. Requests to hosts that are not
listed fall back to that ambient Docker config.

**Note** that the `hosts` section does not apply to Helm HTTP/S repositories,
which use the ambient Helm repositories config.

Each host configures either a `credential` (a JWT-based token, below) or a
cloud registry `provider` — the two are mutually exclusive. A `credential` host
(or a host with neither) may also set [`tls`](#tls) for transport-layer TLS/mTLS;
`tls` is not allowed with `provider` (a cloud registry is managed and its
transport is not customized). The host-level [`maxChunkSize`](#host-fields) tunes
blob uploads independently of auth. At least one of `credential`, `provider`,
`tls`, or `maxChunkSize` is required.

```yaml
hosts:
  # Form 1: a per-host credential (JWT-based).
  - host: registry.example.com
    credential:
      # Exactly one of the following four selects how the credential is obtained.
      provider: github           # mint a token (github, forgejo, gcp, azure, aws, or jwt-svid)
      fromEnv: SOME_ENV_VAR      # send a static JWT read from this env var
      fromPath: /path/to/token   # send a static JWT read from this file
      jwkPath: /path/to/jwk.json # sign a fresh JWT per request with this key

      # Required for, and allowed only with, jwkPath.
      iss: https://example.com/issuer
      sub: client-id

      # Optional; allowed only with jwkPath or provider. Defaults to host.
      aud: registry.example.com

      # Optional; allowed only with jwkPath. JWT lifetime; defaults to 60s.
      exp: 1h

      # Optional. Changes how the credential is presented (see below).
      username: robot

  # Form 2: a cloud registry provider (ECR/ACR/GAR), via ambient workload identity.
  - host: 123456789012.dkr.ecr.us-east-1.amazonaws.com
    provider: ecr               # one of ecr, acr, gar
```

### Bearer token vs. username/password (`credential.username`)

`username` controls how the resolved credential is presented to the registry:

- **Unset (default)** — the credential is a bearer token. `sync` sends it as
  `Authorization: Bearer` on every request (no auth challenge), and
  `login`/`secret` write it to the Docker config's `registrytoken` field.
  This suits registries that validate an OIDC token natively. `registrytoken` is
  non-standard but understood by go-containerregistry (crane, Flux); it is not
  understood by `kubelet` image pulls.
- **Set** — the credential becomes the password of a `username`/`password`
  pair. `sync` goes through the standard registry auth challenge (like the
  cloud providers — credentials are exchanged at the token endpoint), and
  `login`/`secret` write `username`/`password`/`auth`.

> ⚠️ Breaking change: credentials without `username` now default to
> `registrytoken` in `login`/`secret` output (previously a placeholder
> `username`/`password`). Set `username` to restore `username`/`password`/`auth`.

### Registry providers

`provider` authenticates the host using the cloud provider's ambient workload
identity — the same mechanism the `flux push artifact` family uses — and obtains
the registry credentials directly (no JWT/`credential` involved). It is mutually
exclusive with `credential`.

| `provider` | Cloud registry    | Identity source                                                      |
|------------|-------------------|----------------------------------------------------------------------|
| `ecr`      | Amazon ECR (AWS)  | AWS credential chain (IRSA / EC2 / env / SSO, region from the host). |
| `acr`      | Azure ACR (Azure) | Default Azure credential chain (managed identity, env, ...).         |
| `gar`      | Google GAR (GCP)  | Google Application Default Credentials.                              |

### Token sources

| Source     | Behavior                                                                                                                                                                                                                                                                                                                                                                                                                   |
|------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `provider` | Mints a fresh, per-request token for `aud` using the named provider's ambient credentials, cached and refreshed on demand. One of `github`, `forgejo`, `gcp`, `azure`, `aws`, or `jwt-svid` — see [Token providers](#token-providers) for what each uses.                                                                                                                                                                  |
| `fromEnv`  | Sends the JWT read from the named environment variable as-is (e.g. a GitLab CI `id_token`). Errors at runtime if the variable is unset or empty.                                                                                                                                                                                                                                                                           |
| `fromPath` | Sends the JWT read from the file at the path, with surrounding whitespace trimmed. The file is re-read on every request.                                                                                                                                                                                                                                                                                                   |
| `jwkPath`  | Signs a fresh JWT with the private Ed25519/ECDSA key in the JWK file at this path. By default each request gets a new 60-second token; set `exp` to issue longer-lived tokens (then cached and reminted at half-life). The file may be a bare JWK or a JWK set (`{"keys":[...]}`) containing exactly one key. The key id is set in the `kid` header. Generate a key pair with [`flux-mirror keygen`](../guides/keygen.md). |

Path values (`fromPath`, `jwkPath`, and the `tls` path fields) are resolved relative to the config file's directory.
A config can only reference files under its own directory tree.
When the config is read from stdin (`-f -`), the process working directory is used as the confinement root instead.

### Token providers

When `credential.provider` is set, the token is minted from the provider's ambient
credentials for the audience (`aud`, defaulting to the host), then cached and
refreshed on demand. Each provider obtains it differently — consult the linked
platform docs for setup:

- `github`, `forgejo` — request an OIDC ID token from the CI Actions endpoint
  (`ACTIONS_ID_TOKEN_REQUEST_URL` / `ACTIONS_ID_TOKEN_REQUEST_TOKEN`).
- `gcp` — Google Application Default Credentials (GKE/GCE metadata server,
  service account key, or workload identity federation).
- `azure` — the default Azure credential chain (AKS/managed identity, workload
  identity federation, environment credentials), requesting the `<aud>/.default`
  scope.
- `aws` — not an OIDC token. It signs an `sts:GetCallerIdentity` request with the
  ambient role credentials (IMDS, environment, ...) and wraps it in a JWT-shaped
  envelope; `aud` is carried as a signed `X-Audience` header that pins the target
  registry. The registry verifies the caller by replaying the signed request to
  AWS STS, so the destination must understand this scheme — a generic OIDC
  registry will not. The envelope uses the JOSE header
  `{"alg":"none","typ":"aws-sigv4-getcalleridentity"}` so the registry routes it
  away from JWKS validation and derives identity only from the STS response.
- `jwt-svid` — fetches a JWT-SVID for `aud` from the SPIFFE Workload API
  (`SPIFFE_ENDPOINT_SOCKET`). This is the HTTP-layer counterpart to the
  transport-layer SPIFFE X.509-SVID mTLS under [`tls`](#tls).

### Host fields

A `hosts[]` entry. At least one of `credential`, `provider`, `tls`, or
`maxChunkSize` must be set. `credential` and `provider` are mutually exclusive;
`tls` composes with `credential` but is not allowed with `provider`.

| Field          | Type   | Default | Description                                                                                                                                                                                                |
|----------------|--------|---------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `host`         | string |         | Registry host (with optional port) the entry applies to. Required and unique across `hosts`.                                                                                                               |
| `credential`   | object |         | Per-host JWT-based credential. Mutually exclusive with `provider`. See [Credential fields](#credential-fields).                                                                                            |
| `provider`     | string |         | Cloud registry provider, one of `ecr`, `acr`, `gar`. Mutually exclusive with `credential`. See [Registry providers](#registry-providers).                                                                  |
| `tls`          | object |         | Transport-layer TLS/mTLS for this host. Composes with `credential`; not allowed with `provider`. See [TLS](#tls).                                                                                          |
| `maxChunkSize` | int    | `0`     | Maximum size in KiB for a blob-upload `PATCH` to this host; larger blobs are split into chunked uploads. `0` disables chunking (one monolithic `PATCH` per blob). Useful behind body-size-capping proxies. |

### Credential fields

The `credential` object on a host. It selects exactly one token source
(`provider`, `fromEnv`, `fromPath`, or `jwkPath`); see [Token sources](#token-sources)
for what each does.

| Field      | Type     | Default | Description                                                                                                                                                                                                                                                                                                     |
|------------|----------|---------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `provider` | string   |         | Token provider, one of `github`, `forgejo`, `gcp`, `azure`, `aws`, `jwt-svid`. Mutually exclusive with `fromEnv`, `fromPath`, and `jwkPath`. See [Token providers](#token-providers).                                                                                                                           |
| `fromEnv`  | string   |         | Environment variable holding a static JWT. Mutually exclusive with `provider`, `fromPath`, and `jwkPath`.                                                                                                                                                                                                       |
| `fromPath` | string   |         | Path to a file holding a static JWT. Mutually exclusive with `provider`, `fromEnv`, and `jwkPath`.                                                                                                                                                                                                              |
| `jwkPath`  | string   |         | Path to a private JSON Web Key, as a bare JWK or a single-key JWK set. Mutually exclusive with `provider`, `fromEnv`, and `fromPath`.                                                                                                                                                                           |
| `iss`      | string   |         | Token issuer. Required with `jwkPath`; not allowed otherwise.                                                                                                                                                                                                                                                   |
| `sub`      | string   |         | Token subject. Required with `jwkPath`; not allowed otherwise.                                                                                                                                                                                                                                                  |
| `aud`      | string   | `host`  | Token audience. Allowed only with `jwkPath` or `provider`; defaults to `host`.                                                                                                                                                                                                                                  |
| `exp`      | duration | `60s`   | JWT lifetime (e.g. `1h`). Allowed only with `jwkPath` — the one source whose lifetime flux-mirror controls. Must be positive. Other sources' lifetimes are fixed by their issuer.                                                                                                                               |
| `username` | string   |         | When unset, the credential is a bearer token (`registrytoken` / `Authorization: Bearer`). When set, the credential is the password of a `username`/`password` pair, using the standard registry auth challenge. See [Bearer token vs. username/password](#bearer-token-vs-usernamepassword-credentialusername). |

### TLS

`tls` configures the transport-layer TLS for a host's registry requests, separate
from the HTTP-layer `credential` above — a host may set both. It is not allowed
on a `provider` host (a cloud registry is managed and its transport is not
customized). It is
applied by `sync` (which connects to the registry); the `login` and `secret`
commands do not open registry connections and ignore it.

`tls` has two independent halves — `serverAuth` (how the registry's server
certificate is verified) and `clientAuth` (the client certificate presented for
mTLS) — and at least one must be set. Each half independently chooses SPIFFE
or non-SPIFFE, so SPIFFE can authenticate the client while a normal/custom CA
verifies the server, or vice versa.

- **`serverAuth`** — exactly one of `fromPath`/`fromEnv`/`fromBytes` (a custom CA
  bundle, possibly multiple concatenated PEM certs) or `spiffe` (verify the
  server's X.509-SVID against the SPIFFE trust bundle). When `serverAuth` is
  omitted, the system trust pool is used.
- **`clientAuth`** — exactly one of `provider: x509-svid` (present a SPIFFE
  X.509-SVID from the Workload API) or the static `certificate` + `key` pair.

```yaml
hosts:
  # Static: custom CA for server verification and a client cert for mTLS.
  - host: registry.example.com
    tls:
      serverAuth:
        # Exactly one of fromPath/fromEnv/fromBytes. May hold multiple
        # concatenated PEM certificates (a CA pool).
        fromPath: ./certs/ca.crt
      clientAuth:
        certificate:                  # exactly one of fromPath/fromEnv/fromBytes
          fromPath: ./certs/client.crt
        key:                          # exactly one of fromPath/fromEnv (no inline; it's a secret)
          fromPath: ./certs/client.key

  # Full SPIFFE: X.509-SVID client cert + SPIFFE server verification.
  - host: spiffe.example.com
    tls:
      clientAuth:
        provider: x509-svid           # present our X.509-SVID from the Workload API
      serverAuth:
        spiffe:
          # Exactly one of the following authorizes the server's SVID.
          serverID: spiffe://example.org/registry   # pin one exact SPIFFE ID, or
          # trustDomain: example.org                 # any SVID in this trust domain
          # trustDomain: self                        # any SVID in our own trust domain
          # authorizeAny: true                       # accept any SVID (discouraged)

  # Client-only SPIFFE: SPIFFE client cert, server verified by a public/custom CA.
  - host: public.example.com
    tls:
      clientAuth:
        provider: x509-svid
      # serverAuth omitted → system trust pool verifies the server.
```

With SPIFFE on either side, the client SVID and trust bundle come from the ambient
Workload API socket (`SPIFFE_ENDPOINT_SOCKET`) and rotate automatically.
`trustDomain: self` uses the client's own trust domain (read from its X.509-SVID),
so no value is needed in the common case.

| Field                                | Type   | Description                                                                                                                                                                        |
|--------------------------------------|--------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `tls.serverAuth`                     | object | Server-cert verification. Exactly one of `fromPath`/`fromEnv`/`fromBytes` (CA bundle, may contain multiple concatenated PEM certs) or `spiffe`. Omit to use the system trust pool. |
| `tls.serverAuth.spiffe.serverID`     | string | Authorize one exact server SPIFFE ID. Mutually exclusive with the other `spiffe` fields.                                                                                           |
| `tls.serverAuth.spiffe.trustDomain`  | string | Authorize any server SVID in this trust domain; `self` means the client's own. Mutually exclusive with the other `spiffe` fields.                                                  |
| `tls.serverAuth.spiffe.authorizeAny` | bool   | Accept any SVID the bundle can validate (discouraged). Mutually exclusive with the other `spiffe` fields.                                                                          |
| `tls.clientAuth`                     | object | Client certificate for mTLS. Exactly one of `provider: x509-svid` or the `certificate` + `key` pair.                                                                               |
| `tls.clientAuth.provider`            | string | `x509-svid` to present a SPIFFE X.509-SVID. Mutually exclusive with `certificate`/`key`.                                                                                           |
| `tls.clientAuth.certificate`         | object | Client certificate chain. One of `fromPath`/`fromEnv`/`fromBytes`. Requires `key`.                                                                                                 |
| `tls.clientAuth.key`                 | object | Client private key. One of `fromPath`/`fromEnv` (no `fromBytes` — a private key must not be inlined in the config). Requires `certificate`.                                        |

## Overwrite behavior

The Flux Mirror CLI does not overwrite existing destination tags by default. This
keeps the safe path the default on immutable registries (ECR with `imageTagMutability: IMMUTABLE`,
GAR with tag immutability, Harbor with retention rules) and avoids redundant writes on mutable ones.

When a tag exists at the destination, the source and destination digests are compared:

- Same digest: skip silently.
- Different digest, `overwrite: false`: log a warning and skip. The mirror
  has drifted from source but will not be brought back in sync without
  explicit consent.
- Different digest, `overwrite: true`: push the new digest, replacing the
  destination tag. Fails if the destination registry enforces tag immutability.

The drift warning is useful even on immutable registries where the divergence
cannot be resolved automatically; it shows up in the run output and exit
code, which audit and alerting can hook into.

The `--overwrite` CLI flag forces `overwrite: true` for every entry,
overriding per-entry values. Use it for one-off resyncs without editing
the config.

When combined with `includeReferrers: true`, the same rule applies to
referrers. Referrers that are missing at the destination are always mirrored
(there is nothing to overwrite); this is the common case when an upstream
signature is published after the artifact was first mirrored. Referrers
that exist with a different digest are skipped (default) or replaced
(`overwrite: true`).

## Defaults

| Field                          | Default  |
|--------------------------------|----------|
| `artifacts[].selector.sortBy`  | `semver` |
| `artifacts[].selector.limit`   | `1`      |
| `artifacts[].overwrite`        | `false`  |
| `artifacts[].includeReferrers` | `false`  |
| `artifacts[].verify`           | unset    |
| `artifacts[].verify.minAge`    | unset    |
| `charts[].version`             | `*`      |
| `charts[].limit`               | `1`      |
| `charts[].overwrite`           | `false`  |

`limit: 0` disables the cap and mirrors every matching tag. Use with care:
unrestricted mirrors can consume significant bandwidth and storage, and may
trip rate limits on upstream registries.
