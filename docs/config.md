# Flux Mirror Config Specification

`flux-mirror` is configured via a YAML file listing the OCI artifacts and Helm charts to
mirror. Sources can be OCI registries or HTTP/S Helm repositories; destinations are OCI
registries.

## Example

```yaml
apiVersion: mirror.fluxcd.io/v1alpha1
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
```

## Top-level fields

| Field        | Type   | Default | Description                                                                                 |
|--------------|--------|---------|---------------------------------------------------------------------------------------------|
| `apiVersion` | string |         | Must be `mirror.fluxcd.io/v1alpha1`.                                                        |
| `kind`       | string |         | Must be `Config`.                                                                           |
| `auth`       | object | `null`  | Per-host JWT authentication for outbound OCI registry requests. See [Auth](#auth).         |
| `artifacts`  | list   | `[]`    | OCI artifacts (images, OCI Helm charts, Flux artifacts, etc.). See [Artifacts](#artifacts). |
| `charts`     | list   | `[]`    | Helm charts from HTTP/S or OCI Helm repositories. See [Charts](#charts).                    |

At least one of `artifacts` or `charts` must be set.

## Auth

The optional `auth` section attaches a JWT credential to specific OCI registry
hosts. On each request to a listed host, the JWT is sent as the
`Authorization: Bearer <jwt>` credential. Requests to hosts that are **not**
listed use the ambient keychain auth (Docker config `~/.docker/config.json` /
`$DOCKER_CONFIG` and any configured credential helpers) instead.

`auth` and the ambient credential files work together fine — **as long as each
registry host is served by one or the other, not both.** Listing a host in
`auth` *and* relying on ambient credentials for that same host is unsupported:
the JWT and the keychain layer on top of each other in a registry-dependent way,
and the result is undefined. Pick one mechanism per host.

> This applies only to OCI registry requests — the `artifacts` section and
> `oci://` Helm sources. HTTP/S Helm repositories are never affected by `auth`;
> their credentials always come from the Helm repositories config
> (`repositories.yaml` / `$HELM_REPOSITORY_CONFIG`). See [Source scheme](#source-scheme).

```yaml
auth:
  hosts:
    - host: registry.example.com
      jwt:
        # Exactly one of the following three selects how the token is obtained.
        provider: github           # mint an OIDC ID token (github or forgejo)
        fromEnv: SOME_ENV_VAR      # send a static JWT read from this env var
        jwkPath: /path/to/jwk.json # sign a fresh JWT per request with this key

        # Required for, and allowed only with, jwkPath.
        iss: https://example.com/issuer
        sub: client-id

        # Optional; allowed only with jwkPath or provider. Defaults to host.
        aud: registry.example.com
```

### Token sources

| Source     | Behavior                                                                                                                                                                                                  |
|------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `provider` | Mints an OIDC ID token for `aud` from the GitHub/Forgejo Actions endpoint (`ACTIONS_ID_TOKEN_REQUEST_URL`/`_TOKEN`). Cached for the first 50% of its lifetime, then reminted. One of: `github`, `forgejo`. |
| `fromEnv`  | Sends the JWT read from the named environment variable as-is (e.g. a GitLab CI `id_token`). Errors at runtime if the variable is unset or empty.                                                          |
| `jwkPath`  | Signs a fresh, 60-second JWT on **every** request with the private Ed25519/ECDSA key in the JWK file at this path. The key id is set in the `kid` header.                                                  |

### Fields

| Field      | Type   | Default | Description                                                                                  |
|------------|--------|---------|---------------------------------------------------------------------------------------------|
| `host`     | string |         | Registry host to authenticate. Required and unique across `hosts`.                           |
| `provider` | string |         | OIDC provider, one of `github`, `forgejo`. Mutually exclusive with `fromEnv` and `jwkPath`.  |
| `fromEnv`  | string |         | Environment variable holding a static JWT. Mutually exclusive with `provider` and `jwkPath`. |
| `jwkPath`  | string |         | Path to a private JSON Web Key. Mutually exclusive with `provider` and `fromEnv`.            |
| `iss`      | string |         | Token issuer. Required with `jwkPath`; not allowed otherwise.                                |
| `sub`      | string |         | Token subject. Required with `jwkPath`; not allowed otherwise.                               |
| `aud`      | string | `host`  | Token audience. Allowed only with `jwkPath` or `provider`; defaults to `host`.               |

## Artifacts

The `artifacts` section mirrors OCI artifacts between OCI registries using
`crane copy` semantics: the manifest and all referenced blobs are copied
byte-for-byte, preserving digests. Multi-arch images are mirrored faithfully
as manifest lists; no platform filtering is performed.

This section handles any OCI-addressable artifact: container images, OCI Helm
charts, Flux OCI artifacts, or anything else stored in an OCI registry.

### Fields

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

### Regex filter

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

### Fields

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
