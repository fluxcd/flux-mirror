# Flux Mirror Sync Command

The `flux-mirror sync` command mirrors Helm charts and OCI artifacts between registries based on a
declarative YAML config. The command is idempotent, re-running against the same config produces
the same destination state, copying only what's missing or drifted.

See the [config specification](./config.md) for the YAML schema.

## Synopsis

```
flux-mirror sync CONFIG|- [flags]
```

## Configuration source

The config file path is resolved in the following order:

1. First positional argument (`-` reads YAML from stdin).
2. `FLUX_MIRROR_CONFIG` environment variable.

```bash
flux-mirror sync examples/podinfo.yaml
flux-mirror sync - < examples/podinfo.yaml
FLUX_MIRROR_CONFIG=examples/podinfo.yaml flux-mirror sync
```

## Authentication

OCI registry auth is read from the ambient Docker config:

- `~/.docker/config.json`, or the `DOCKER_CONFIG` env var if set.
- Any configured credential helpers (e.g., `docker-credential-osxkeychain`, `docker-credential-ecr-login`, `docker-credential-gcloud`).

Log in once with `docker login`, `oras login`, etc. and `flux-mirror` picks up the credentials.

Helm HTTP/S repository auth is read from the ambient Helm repositories config:

- Helm's default `repositories.yaml` path, or the `HELM_REPOSITORY_CONFIG` env var if set.
- `username` / `password`, `certFile`, `keyFile`, `caFile`, `insecure_skip_tls_verify`, and
  `pass_credentials_all` are honored.

Log in or add repositories with `helm repo add` and `flux-mirror` picks up matching HTTP/S
repository credentials automatically.

## Flags

| Flag                            | Default | Description                                                                                                                                        |
|---------------------------------|---------|----------------------------------------------------------------------------------------------------------------------------------------------------|
| `-o, --output text\|yaml\|json` | `text`  | Output format. `text` is human-friendly; `yaml` and `json` print the structured `Result` to stdout.                                                |
| `--concurrency N`               | `4`     | Maximum number of copy operations to run in parallel within a single config entry. Entries themselves are processed sequentially.                  |
| `--retries N`                   | `3`     | Maximum number of retry attempts per job, bounded by `--timeout`.                                                                                  |
| `--overwrite`                   | `false` | Force `overwrite: true` on every entry, regardless of per-entry config. See [Overwrite Behavior](./config.md#overwrite-behavior).                  |
| `--drift-exit-code N`           | `2`     | Exit code to use when drift is detected without failures. Set to `0` for immutable destination registries that should not fail CI on drift.        |
| `--dry-run`                     | `false` | Run the plan and comparison pipeline without performing any writes. Reported as `would-copy` / `would-overwrite` in the output.                    |
| `--verbose`                     | `false` | Emit a structured log line per operation (entry started, mirroring tag, tag done, entry summary, sync complete) on stderr. Suppresses the spinner. |
| `--no-progress`                 | `false` | Disable the live progress spinner. Per-job lines and the Summary still print.                                                                      |
| `--insecure`                    | `false` | Allow plaintext HTTP and skip TLS verification. **Test/dev only.**                                                                                 |
| `--timeout DURATION`            | `5m`    | Per-job total budget covering all retry attempts.                                                                                                  |

## Output

### Text mode (default)

Pretty-print mode:

```
✓ ghcr.io/stefanprodan/charts/podinfo:6.10.2 (skipped)
→ localhost:5050/charts/podinfo:6.10.2
✓ ghcr.io/stefanprodan/charts/podinfo:6.11.0 (copied)
→ localhost:5050/charts/podinfo:6.11.0
✗ ghcr.io/stefanprodan/charts/podinfo2 — plan failed: NAME_UNKNOWN
Summary: 1 copied, 1 skipped, 1 failed in 4.2s.
```

### Verbose mode

`--verbose` replaces the pretty output with a full diagnostic log stream on stderr.

Every layer push, blob check (existing vs pushed), manifest digest, fallback-tag update for referrers,
and registry-side warning is logged. Reach for this when diagnosing TLS, auth, missing-blob, or push-rejection issues.

```
2026/05/13 09:00:00 sync started entries=3 concurrency=4 retries=3 timeout=5m0s
2026/05/13 09:00:00 mirroring tag src=ghcr.io/foo/bar:1.0 dst=localhost:5050/bar:1.0
2026/05/13 09:00:00 Copying from ghcr.io/foo/bar:1.0 to localhost:5050/bar:1.0
2026/05/13 09:00:00 pushed blob: sha256:fdf53ef8e04176eedbd42713efb2d002f1741c310627b38f444c6f6d92a598f7
2026/05/13 09:00:00 existing blob: sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a
2026/05/13 09:00:00 localhost:5050/bar:1.0: digest: sha256:455d6a04fe43df4a93b40e1f92d65b2a9befaa0348e5a461295d961161ebf475 size: 829
2026/05/13 09:00:00 updating fallback tag sha256-455d6a04... with new referrer
2026/05/13 09:00:01 tag done src=ghcr.io/foo/bar:1.0 outcome=copied elapsed=812ms
…
2026/05/13 09:00:04 sync complete entries=3 copied=1 overwritten=0 skipped=1 drifted=0 failed=1 duration=4.213s
```

### Structured output

`-o yaml` and `-o json` print the full `Result` to stdout — entries with their per-outcome tag lists and any failures
Suitable for piping into another tool.

```bash
flux-mirror sync config.yaml -o json | jq '.entries[].outcomes.copied'
```

## Outcomes

Each tag job lands in exactly one of these buckets:

| Outcome           | Meaning                                                                                       |
|-------------------|-----------------------------------------------------------------------------------------------|
| `copied`          | Destination did not have the tag; mirrored from source.                                       |
| `overwritten`     | Destination had a different digest; replaced (only with `overwrite: true`).                   |
| `skipped`         | Destination already had the same digest; nothing to do.                                       |
| `drifted`         | Destination has a different digest, `overwrite: false` — left alone, surfaced in the summary. |
| `would-copy`      | Dry-run forecast: would have been copied.                                                     |
| `would-overwrite` | Dry-run forecast: would have been overwritten.                                                |

Plan-time failures (e.g., source registry rejected a `ListTags`) are reported as a
single `✗ <entry> — plan failed: <err>` line and counted toward `failed`.

## Exit codes

| Code | Meaning                                                                                                               |
|------|-----------------------------------------------------------------------------------------------------------------------|
| `0`  | Clean run — every tag was copied or skipped as expected, no drift, no failures.                                       |
| `1`  | At least one tag job failed (network error, push rejected, retries exhausted, plan failure).                          |
| `2`  | No failures, but at least one tag drifted with `overwrite: false`. The destination is out of date relative to source. |

Failures take precedence over drift. `--dry-run` does not bump the exit code for `would-copy` / `would-overwrite`,
but drift detection still produces `2` by default. Use `--drift-exit-code=0` when the destination registry is known to be
immutable and drift should be logged without failing CI.

## Examples

### Mirror a Flux artifact with its image

A typical `Kustomization` workload pulls two artifacts: the manifests from an
`OCIRepository`, and the container images those manifests reference (often
rewritten via `spec.images` patches). A single config can mirror both so the
destination registry is self-sufficient.

```yaml
# config.yaml
apiVersion: mirror.fluxcd.io/v1alpha1
kind: Config
artifacts:
  - source: ghcr.io/stefanprodan/manifests/podinfo
    destination: localhost:5050/manifests/podinfo
    selector:
      regex:
        pattern: "^latest$"
      sortBy: alphabetical
      limit: 1
    includeReferrers: true
    overwrite: true
  - source: ghcr.io/stefanprodan/podinfo
    destination: localhost:5050/podinfo
    selector:
      semver: "*"
      limit: 1
    includeReferrers: true
```

```bash
flux-mirror sync config.yaml
```

### Mirror a Helm chart with its image

A typical `HelmRelease` workload pulls two artifacts: the chart from a Helm
repository, and the container image referenced by the chart's `values.yaml`.
A single config can mirror both so the destination registry is self-sufficient.

```yaml
# config.yaml
apiVersion: mirror.fluxcd.io/v1alpha1
kind: Config
charts:
  - source: https://kubernetes-sigs.github.io/external-dns/
    destination: oci://localhost:5050/charts
    name: external-dns
    version: ">=1.15.0"
    limit: 3
artifacts:
  - source: registry.k8s.io/external-dns/external-dns
    destination: localhost:5050/external-dns
    selector:
      semver: ">=0.15.0"
      limit: 3
    includeReferrers: true
```

```bash
flux-mirror sync config.yaml
```

The chart lands at `localhost:5050/charts/external-dns:<version>` (the chart
name is appended to the destination); the image lands at
`localhost:5050/external-dns:<version>`. Override the image reference in the
consuming `HelmRelease.spec.values` to point at the mirror.

### Preview without writing

```bash
flux-mirror sync config.yaml --dry-run -o yaml
```

### Force-resync drifted tags

```bash
flux-mirror sync config.yaml --overwrite
```

### CI-friendly invocation

```bash
flux-mirror sync config.yaml --no-progress
```

For immutable destination registries, keep CI green while still logging drift:

```bash
flux-mirror sync config.yaml --no-progress --drift-exit-code=0
```

### Read config from stdin

```bash
cat config.yaml | flux-mirror sync -
```
