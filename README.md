# flux-mirror

[![release](https://img.shields.io/github/release/fluxcd/flux-mirror/all.svg)](https://github.com/fluxcd/flux-mirror/releases)
[![test](https://github.com/fluxcd/flux-mirror/actions/workflows/test.yaml/badge.svg)](https://github.com/fluxcd/flux-mirror/actions/workflows/test.yaml)
[![cve-scan](https://github.com/fluxcd/flux-mirror/workflows/cve-scan/badge.svg)](https://github.com/fluxcd/flux-mirror/actions/workflows/cve-scan.yml)
[![license](https://img.shields.io/github/license/fluxcd/flux-mirror.svg)](https://github.com/fluxcd/flux-mirror/blob/main/LICENSE)
[![slsa](https://slsa.dev/images/gh-badge-level2.svg)](https://github.com/fluxcd/flux-mirror/attestations)

**Flux Mirror** is a CLI for mirroring Helm charts, OCI artifacts and
container images between registries using a declarative approach.

The intended use case is feeding an internal mirror registry that backs
Flux OCIRepository and Kubernetes Deployments, so
clusters never reach out to upstream registries at reconcile time.

It also enables migration away from HTTP/S `HelmRepository` sources: chart
versions are republished as OCI Helm artifacts that `HelmRelease` consumes
via an `OCIRepository` in `spec.chartRef`, dropping the runtime dependency
on upstream chart repositories.

> [!NOTE]
> This repository is in early development and the plugin system is not yet
> available in a stable release of Flux. Instructions for installing and
> using `flux-mirror` as a Flux CLI plugin will be added here once
> [RFC-0013](https://github.com/fluxcd/flux2/blob/main/rfcs/0013-cli-plugin-system/README.md)
> ships in Flux 2.9 or later.

## Features

- **OCI artifacts** — mirror container images, OCI Helm charts, Flux OCI
  artifacts, and any other OCI-addressable artifact between registries.
  Manifests and blobs are copied byte-for-byte; multi-arch manifest lists
  are mirrored as a whole, no platform filtering.
- **Helm charts** — mirror charts from HTTP/S Helm repositories to an OCI registries.
  Chart bytes are re-published as a deterministic Helm-OCI artifact, so
  drift detection on re-runs is content-based and stable.
- **OCI 1.1 referrers** — opt-in mirror of signatures, SBOMs, and attestations attached to artifacts.
- **Cosign verification** — opt-in keyless signature verification for selected
  source artifacts before they are mirrored.
- **Selector pipeline** — for OCI artifacts, a four-step
  `regex → semver → sort → top-N` filter. For charts, a semver constraint
  plus top-N. Sort by `semver`, `alphabetical`, or `numerical`.
- **Idempotent** — destination digests are compared per tag/version. Re-runs
  copy only what's missing or drifted.
- **Drift gating** — destination drift (different content under the same tag) is reported
  as a distinct outcome and exit code, so audit pipelines can differentiate "out of date" from "mutated tags".
- **Ambient auth** — OCI credentials come from `~/.docker/config.json` and the
  configured credential helpers (ACR, ECR, GAR, etc.); Helm HTTP/S repository
  credentials come from Helm's `repositories.yaml`. Running `helm repo add` and
  `docker login` covers source and destination auth.
- **Structured output** — `text` and `yaml`/`json` for downstream
  tooling, plus a verbose mode that streams every blob and manifest digest
  for diagnosing TLS, auth, or push failures.

## Install

Download the binary for your platform from the [releases page](https://github.com/fluxcd/flux-mirror/releases),
or build from source:

```shell
go install github.com/fluxcd/flux-mirror/cmd/flux-mirror@latest
```

## Quickstart

Authenticate once against the destination and optionally source registries:

```shell
docker login ghcr.io
```

For private HTTP/S Helm repositories, login with Helm:

```shell
helm repo add private https://charts.example.com --username "$USER" --password "$TOKEN"
```

Write a config file describing what to mirror:

```yaml
# flux-mirror.yaml
apiVersion: mirror.fluxcd.io/v1alpha1
kind: Config
charts:
  - name: external-dns
    source: https://kubernetes-sigs.github.io/external-dns/
    destination: oci://ghcr.io/my-org/charts
    version: "*"
    limit: 3
artifacts:
  - source: registry.k8s.io/external-dns/external-dns
    destination: ghcr.io/my-org/external-dns
    selector:
      semver: ">=0.15.0"
      limit: 3
    includeReferrers: true
```

Run the sync:

```shell
flux-mirror sync flux-mirror.yaml
```

You can also read the config from stdin:

```shell
flux-mirror sync - < flux-mirror.yaml
```

Preview without writing:

```shell
flux-mirror sync flux-mirror.yaml --dry-run
```

Force a resync of drifted tags e.g. `latest`:

```shell
flux-mirror sync flux-mirror.yaml --overwrite
```

See [`examples/`](examples) for more configurations and
[`docs/sync.md`](docs/sync.md) for the full flag reference.

## Running in CI

`flux-mirror sync` is designed for unattended runs. The exit code separates
real failures from drift, so a CI gate can react to each independently:

| Code | Meaning                                                                                                    |
|------|------------------------------------------------------------------------------------------------------------|
| `0`  | Clean run, every tag was copied or skipped as expected.                                                    |
| `1`  | At least one tag job failed (network error, push rejected, retries exhausted).                             |
| `2`  | No failures, but at least one tag drifted with `overwrite: false` (configurable with `--drift-exit-code`). |

The `--no-progress` flag suppresses the live spinner so log output stays clean in CI:

```shell
flux-mirror sync flux-mirror.yaml --no-progress
```

When the destination registry is known to be immutable, drift can be reported
without failing the CI job:

```shell
flux-mirror sync flux-mirror.yaml --no-progress --drift-exit-code=0
```

For downstream tooling, emit a structured report:

```shell
flux-mirror sync flux-mirror.yaml -o json | jq '.entries[].outcomes'
```

### GitHub Actions

The [`fluxcd/flux-mirror/actions/setup`](actions/setup) composite action
installs the CLI on Ubuntu, macOS, and Windows runners.

Example workflow:

```yaml
name: mirror-charts

on:
  schedule:
    - cron: "0 */6 * * *"
  workflow_dispatch:

jobs:
  sync:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      packages: write
    steps:
      - name: Checkout
        uses: actions/checkout@v6
      - name: Setup Flux Mirror CLI
        uses: fluxcd/flux-mirror/actions/setup@main
      - name: Login to GHCR
        uses: docker/login-action@v4
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - name: Sync Kubernetes SIGs Charts
        run: flux-mirror sync kubernetes-sigs.yaml --no-progress
```

### Docker

The `ghcr.io/fluxcd/flux-mirror` image can be used in container-based CI pipelines:

```shell
docker run --rm \
  -e DOCKER_CONFIG=/.docker \
  -v "$PWD/flux-mirror.yaml:/config.yaml:ro" \
  -v "$HOME/.docker/config.json:/.docker/config.json:ro" \
  ghcr.io/fluxcd/flux-mirror:latest sync /config.yaml --no-progress
```

### Kubernetes

To run `flux-mirror sync` from inside a cluster on a schedule, see the
[`examples/cronjob.yaml`](examples/cronjob.yaml) manifest. It bundles a
`ConfigMap` with the sync config and a `CronJob` that mounts the
destination registry credentials from a `Secret` created via
`flux create secret oci`.

## Commands

| Command                     | Description                                                      |
|-----------------------------|------------------------------------------------------------------|
| `flux-mirror sync [CONFIG]` | Mirror Helm charts and OCI artifacts described by a YAML config. |
| `flux-mirror version`       | Print the CLI version.                                           |
| `flux-mirror completion`    | Generate shell completion for bash, fish, powershell and zsh.    |

Run `flux-mirror <command> --help` for the full flag list.

## Documentation

- [Sync command reference](docs/sync.md) — flags, output modes, outcomes,
  exit codes, and example invocations.
- [Config specification](docs/config.md) — YAML schema for `artifacts` and
  `charts` entries, selector pipeline, overwrite semantics, defaults.
- [Examples](examples/) — runnable configs for common mirror scenarios.

## License

The Flux Mirror project is [Apache 2.0 licensed](LICENSE) and accepts
contributions via GitHub pull requests.
