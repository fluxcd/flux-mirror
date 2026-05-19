# Flux Mirror Config Specification

`flux-mirror` is configured via a YAML file listing the OCI artifacts and Helm charts to
mirror. Sources can be OCI registries or HTTP/S Helm repositories; destinations are OCI
registries.

## File format

The config uses a Kubernetes-style `apiVersion`/`kind` fields. It is not a
Kubernetes resource; it is a CLI config consumed by `flux-mirror`, the same
way `kustomization.yaml` is consumed by `kustomize`.

`flux-mirror sync` reads the file path from the first positional argument,
falling back to `FLUX_MIRROR_CONFIG`. Use `-` as the argument to read YAML
from stdin. The argument wins when both are set.

## Top-level fields

| Field        | Type   | Default | Description                                                                                 |
|--------------|--------|---------|---------------------------------------------------------------------------------------------|
| `apiVersion` | string |         | Must be `mirror.fluxcd.io/v1alpha1`.                                                        |
| `kind`       | string |         | Must be `Config`.                                                                           |
| `artifacts`  | list   | `[]`    | OCI artifacts (images, OCI Helm charts, Flux artifacts, etc.). See [Artifacts](#artifacts). |
| `charts`     | list   | `[]`    | Helm charts from HTTP/S or OCI Helm repositories. See [Charts](#charts).                    |

At least one of `artifacts` or `charts` must be set.

## Artifacts

The `artifacts` section mirrors OCI artifacts between OCI registries using
`crane copy` semantics: the manifest and all referenced blobs are copied
byte-for-byte, preserving digests. Multi-arch images are mirrored faithfully
as manifest lists; no platform filtering is performed.

This section handles any OCI-addressable artifact: container images, OCI Helm
charts, Flux OCI artifacts, or anything else stored in an OCI registry.

### Fields

| Field              | Type   | Default | Description                                                                                               |
|--------------------|--------|---------|-----------------------------------------------------------------------------------------------------------|
| `source`           | string |         | Source OCI reference, without scheme (e.g. `ghcr.io/fluxcd/flux-cli`).                                    |
| `destination`      | string |         | Destination OCI reference, without scheme.                                                                |
| `selector`         | object |         | Tag selection policy. See [Selector](#selector).                                                          |
| `overwrite`        | bool   | `false` | If true, replace tags whose destination digest differs from source. See [Overwrite](#overwrite-behavior). |
| `includeReferrers` | bool   | `false` | If true, also mirror referrers of each tag (cosign signatures, SBOMs, attestations) via the OCI 1.1 API.  |

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

### Examples

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
  tarballs. Assumed public; no auth fields in the YAML.
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

### Examples

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

## Full example

```yaml
apiVersion: mirror.fluxcd.io/v1alpha1
kind: Config

artifacts:
  - source: ghcr.io/fluxcd/flux-cli
    destination: quay.io/example/flux-cli
    selector:
      semver: "2.x"
      limit: 10
    includeReferrers: true
  - source: ghcr.io/example/nightly-build
    destination: quay.io/example/nightly-build
    selector:
      regex:
        pattern: '^.+-[0-9a-f]+-(?P<ts>\d+)$'
        extract: '$ts'
      sortBy: numerical
      limit: 5

charts:
  - name: nginx
    source: https://charts.example.com
    destination: oci://quay.io/example/charts
    version: ">=15.0.0 <16.0.0"
    limit: 5
```

## Defaults

| Field                          | Default  |
|--------------------------------|----------|
| `artifacts[].selector.sortBy`  | `semver` |
| `artifacts[].selector.limit`   | `1`      |
| `artifacts[].overwrite`        | `false`  |
| `artifacts[].includeReferrers` | `false`  |
| `charts[].version`             | `*`      |
| `charts[].limit`               | `1`      |
| `charts[].overwrite`           | `false`  |

`limit: 0` disables the cap and mirrors every matching tag. Use with care:
unrestricted mirrors can consume significant bandwidth and storage, and may
trip rate limits on upstream registries.
