---
weight: 20
linkTitle: Sync Command
---

# Flux Mirror Sync Command

The `flux mirror sync` command mirrors Helm charts and OCI artifacts between
registries based on a declarative [`Config`](config.md). It is
idempotent: re-running against the same config produces the same destination
state, copying only what is missing or drifted.

## Synopsis

```
flux mirror sync [CONFIG|-] [flags]
```

## Configuration source

The config path is resolved in the following order:

1. The first positional argument (`-` reads the config from stdin).
2. The `FLUX_MIRROR_CONFIG` environment variable.

If neither is set, the command errors. Unlike [`login`](login.md) and
[`secret`](secret.md), `sync` has no executable-relative default path.

```bash
flux mirror sync ./flux-mirror.yaml
flux mirror sync - < ./flux-mirror.yaml
FLUX_MIRROR_CONFIG=./flux-mirror.yaml flux mirror sync
```

## Authentication

`sync` authenticates OCI registry requests — `artifacts` source/destination and
`charts` `oci://` destinations — per registry host:

1. Hosts listed under [`hosts`](config.md#hosts) use their configured
   authentication:
   - [`provider: ecr|acr|gar`](config.md#cloud-registry-providers) obtains the
     registry's native credentials from the cloud provider's workload identity.
   - [`credential`](config.md#per-host-credential) resolves a per-host
     token from a cloud/CI identity, a JWK signature, or an environment variable
     or file. With no `username` it is sent as an HTTP Bearer credential; with a
     `username` it is the password in the standard registry auth challenge.
2. Hosts not listed under `hosts` use the ambient Docker config and credential
   helpers (`~/.docker/config.json`, or `$DOCKER_CONFIG` when set).

A non-`provider` host may also set [`tls`](config.md#transport-tls) to
configure transport-layer TLS for its registry connections: a custom CA, a client
certificate (mTLS), or SPIFFE X.509-SVID mTLS.

HTTP/S Helm repository authentication (for `charts` sources) is separate from OCI
auth and always comes from the ambient Helm repositories config:

- Helm's default `repositories.yaml` path, or `$HELM_REPOSITORY_CONFIG` when set.
- The `username`/`password`, `certFile`, `keyFile`, `caFile`,
  `insecure_skip_tls_verify`, and `pass_credentials_all` fields are honored.

Add repositories with `helm repo add` and `flux mirror` picks up matching HTTP/S
repository credentials automatically.

## Flags

| Flag                            | Default | Description                                                                                                                          |
|---------------------------------|---------|------------------------------------------------------------------------------------------------------------------------------------|
| `-o, --output text\|yaml\|json` | `text`  | Output format. `text` is human-friendly; `yaml` and `json` print the structured [sync report](report.md) to stdout.       |
| `--concurrency N`               | `4`     | Maximum number of copy operations to run in parallel per job. Must be greater than `0`.                                            |
| `--retries N`                   | `3`     | Maximum number of retry attempts per job, within the `--timeout` budget. Must be greater than or equal to `0`.                     |
| `--timeout DURATION`            | `5m`    | Per-job total budget covering all retry attempts. Must be greater than `0`.                                                         |
| `--overwrite`                   | `false` | Force `overwrite: true` on every entry, regardless of per-entry config. See [Overwrite and drift behavior](config.md#overwrite-and-drift-behavior). |
| `--drift-exit-code N`           | `2`     | Exit code to use when drift is detected without failures (0–255). Set to `0` for immutable destinations that should not fail CI on drift. |
| `--dry-run`                     | `false` | Run the plan and comparison pipeline without performing any writes. Reported as `would-copy` / `would-overwrite`.                   |
| `--verbose`                     | `false` | Log every operation and the involved digests on stderr. Suppresses the spinner.                                                     |
| `--no-progress`                 | `false` | Disable the live progress spinner. Per-job lines and the summary still print.                                                       |
| `--insecure`                    | `false` | Allow plaintext HTTP and skip TLS verification. Test/dev only.                                                                      |

Global flags: `--timeout` sets the default operation timeout, and `--no-envsubst` disables config environment substitution.

## Output

### Text mode (default)

```
✓ ghcr.io/stefanprodan/charts/podinfo:6.10.2 (skipped)
→ localhost:5050/charts/podinfo:6.10.2
✓ ghcr.io/stefanprodan/charts/podinfo:6.11.0 (copied)
→ localhost:5050/charts/podinfo:6.11.0
✗ ghcr.io/stefanprodan/charts/podinfo2 — plan failed: NAME_UNKNOWN
Summary: 1 copied, 1 skipped, 1 failed in 4.2s.
```

### Verbose mode

`--verbose` replaces the pretty output with a full diagnostic log stream on
stderr — every layer push, blob check, manifest digest, referrer fallback-tag
update, and registry-side warning. Reach for it when diagnosing TLS, auth,
missing-blob, or push-rejection issues.

```
2026/05/13 09:00:00 sync started entries=3 concurrency=4 retries=3 timeout=5m0s
2026/05/13 09:00:00 mirroring tag src=ghcr.io/foo/bar:1.0 dst=localhost:5050/bar:1.0
2026/05/13 09:00:00 Copying from ghcr.io/foo/bar:1.0 to localhost:5050/bar:1.0
2026/05/13 09:00:00 pushed blob: sha256:fdf53ef8e04176eedbd42713efb2d002f1741c310627b38f444c6f6d92a598f7
2026/05/13 09:00:01 tag done src=ghcr.io/foo/bar:1.0 status=copied elapsed=812ms
…
2026/05/13 09:00:04 sync complete entries=3 copied=1 overwritten=0 skipped=1 drifted=0 failed=1 duration=4.213s
```

### Structured output

`-o yaml` and `-o json` print a versioned report to stdout: a top-level envelope
(`apiVersion`, `kind`, `$schema`, `report`) wrapping run metadata (`reporter`,
`timestamp`, `durationMs`), an aggregate `summary` of per-status tag counts, and
`results[]` where each entry carries a `status` (`completed`/`failed`) and a
`tags[]` array of per-tag rows. Every row has a `status` and, when known, a
source `digest`; skipped rows carry a `reason`; verified rows carry a
`verification` block; and, with `includeReferrers: true`, a `referrers[]` array.
The envelope is documented by the [sync report schema](report.md).

```bash
# Status of every tag, per entry
flux mirror sync config.yaml -o json | jq '.report.results[].tags[] | {tag, status}'

# Tags that were copied
flux mirror sync config.yaml -o json | jq '.report.results[].tags[] | select(.status == "copied") | .tag'

# Aggregate counts
flux mirror sync config.yaml -o json | jq '.report.summary'
```

## Status

Each tag row (and each referrer row) lands in exactly one of these statuses:

| Status            | Meaning                                                                                       |
|-------------------|-----------------------------------------------------------------------------------------------|
| `copied`          | Destination did not have the tag; mirrored from source.                                       |
| `overwritten`     | Destination had a different digest; replaced (only with `overwrite: true`).                   |
| `skipped`         | Nothing was copied; the row's `reason` says why.                                              |
| `drifted`         | Destination has a different digest, `overwrite: false` — left alone, surfaced in the summary. |
| `would-copy`      | Dry-run forecast: would have been copied.                                                     |
| `would-overwrite` | Dry-run forecast: would have been overwritten.                                                |
| `failed`          | The operation errored; the row's `error` carries the message.                                 |

A `skipped` row's `reason` is either `up-to-date` (the destination already has
the same digest) or `signature-too-new` (a valid signature deferred by
`verify.minAge`).

A failed signature verification (bad/missing signature, OIDC mismatch) is
recorded as a `failed` tag row carrying the verify error; verification runs at
plan time, so the first failure stops the entry (tags verified before it still
run) while the entry itself stays `completed`. A true plan-time failure (e.g. the
source registry rejected a `ListTags`) surfaces as an entry with `status:
"failed"`, an `error`, and an empty `tags` array, reported live as a `✗ <entry> —
plan failed: <err>` line. Both kinds count toward `failed`.

### Referrers

When an entry sets `includeReferrers: true`, each tag row gains a `referrers[]`
array — one row per mirrored sub-artifact (cosign signature bundle, SBOM,
attestation), each with its own `digest`, `artifactType`, `status`, and (when
skipped) `reason`. Referrers are evaluated under `--dry-run` too.

## Exit codes

| Code | Meaning                                                                                                                |
|------|-----------------------------------------------------------------------------------------------------------------------|
| `0`  | Clean run — every tag was copied or skipped as expected, no drift, no failures.                                        |
| `1`  | At least one tag job failed (network error, push rejected, retries exhausted, plan failure).                           |
| `2`  | No failures, but at least one tag drifted with `overwrite: false`. The destination is out of date relative to source. |

Failures take precedence over drift. `--dry-run` does not bump the exit code for
`would-copy` / `would-overwrite`, but drift detection still produces `2` by
default. Use `--drift-exit-code=0` when the destination registry is known to be
immutable and drift should be logged without failing CI.

## Examples

### Mirror HTTP/S Helm charts into OCI Helm charts

Many common Kubernetes ecosystem components are still published only to classic
HTTP/S Helm repositories and do not offer OCI Helm charts. The
[`charts`](config.md#charts) section pulls those charts over HTTP/S and
re-publishes them as OCI Helm charts so a cluster can consume them from a single
OCI registry.

This config mirrors a few such charts into GitHub Container Registry (GHCR):

```yaml
# flux-mirror.yaml
apiVersion: mirror.plugin.fluxcd.io/v1beta1
kind: Config
charts:
  - name: ingress-nginx
    source: https://kubernetes.github.io/ingress-nginx
    destination: oci://ghcr.io/my-org/charts
    version: ">=4.11.0"
    limit: 3
  - name: cert-manager
    source: https://charts.jetstack.io
    destination: oci://ghcr.io/my-org/charts
    version: ">=1.15.0"
    limit: 3
  - name: metrics-server
    source: https://kubernetes-sigs.github.io/metrics-server
    destination: oci://ghcr.io/my-org/charts
    version: ">=3.12.0"
    limit: 3
  - name: kube-prometheus-stack
    source: https://prometheus-community.github.io/helm-charts
    destination: oci://ghcr.io/my-org/charts
    version: ">=60.0.0"
    limit: 3
hosts:
  # GHCR is the push destination. It expects a username/password login (the
  # username is any non-empty value; GHCR authorizes by the token), so `username`
  # is set and GITHUB_TOKEN is the password.
  - host: ghcr.io
    username: my-org
    credential:
      value: ${GH_TOKEN}
```

Each chart lands at `ghcr.io/my-org/charts/<name>:<version>`. Run it from GitHub
Actions with `packages: write` so the workflow's `GITHUB_TOKEN` can push:

```yaml
# .github/workflows/mirror-charts.yaml
name: mirror-charts
on:
  schedule:
    - cron: "0 */6 * * *"
  workflow_dispatch:
permissions:
  contents: read
  packages: write
jobs:
  sync:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: fluxcd/flux-mirror/actions/setup@main
      - run: flux-mirror sync ./flux-mirror.yaml --no-progress
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

### Mirror OCI Helm charts into OCI Helm charts

A Helm chart that already lives in an OCI registry is mirrored OCI-to-OCI as a
plain OCI artifact. The [`artifacts`](config.md#artifacts) section is
the only way to mirror an OCI Helm chart between OCI repositories — the
[`charts`](config.md#charts) section is exclusively for HTTP/S sources.

```yaml
apiVersion: mirror.plugin.fluxcd.io/v1beta1
kind: Config
artifacts:
  - source: ghcr.io/stefanprodan/charts/podinfo
    destination: ghcr.io/my-org/charts/podinfo
    selector:
      semver: ">=6.0.0"
      limit: 5
```

### Mirror images from GHCR into a private registry

This mirrors `ghcr.io/fluxcd` controller images into a private registry from
GitHub Actions. The destination authenticates with a username and password from
GitHub Actions secrets. The GHCR source is authenticated with `GITHUB_TOKEN`
(rather than pulled anonymously) to avoid anonymous pull rate limits — GHCR
expects a username/password login, so `username` is set there too.

```yaml
# flux-mirror.yaml
apiVersion: mirror.plugin.fluxcd.io/v1beta1
kind: Config
artifacts:
  - source: ghcr.io/fluxcd/source-controller
    destination: registry.internal.example.com/fluxcd/source-controller
    selector:
      semver: ">=1.0.0"
      limit: 5
    includeReferrers: true
  - source: ghcr.io/fluxcd/kustomize-controller
    destination: registry.internal.example.com/fluxcd/kustomize-controller
    selector:
      semver: ">=1.0.0"
      limit: 5
    includeReferrers: true
hosts:
  # Source: authenticate to GHCR to avoid anonymous rate limits.
  - host: ghcr.io
    username: my-org
    credential:
      value: ${GH_TOKEN}
  # Destination: a private registry expecting a username/password login. The
  # username and password are substituted from the environment while loading the
  # config.
  - host: registry.internal.example.com
    username: ${REGISTRY_USERNAME}
    credential:
      value: ${REGISTRY_PASSWORD}
```

```yaml
# .github/workflows/mirror-images.yaml
name: mirror-images
on:
  schedule:
    - cron: "0 */6 * * *"
permissions:
  contents: read
  packages: read
jobs:
  sync:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: fluxcd/flux-mirror/actions/setup@main
      - name: Substitute the destination username into the config
        run: envsubst '${REGISTRY_USERNAME}' < flux-mirror.yaml > rendered.yaml
        env:
          REGISTRY_USERNAME: ${{ secrets.REGISTRY_USERNAME }}
      - run: flux-mirror sync ./rendered.yaml --no-progress
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          REGISTRY_PASSWORD: ${{ secrets.REGISTRY_PASSWORD }}
```

### Mirror into ECR, ACR, or GAR from GitHub Actions

To mirror container images, OCI Helm charts, and Flux OCI artifacts into a cloud
registry, authenticate to the cloud provider with its GitHub Actions OIDC login
action, then set [`hosts[].provider`](config.md#cloud-registry-providers) on the
destination host so `flux-mirror` reuses that ambient identity. The source here
is GHCR, authenticated with `GITHUB_TOKEN`.

The config is the same for all three clouds except for the destination host and
`provider`:

```yaml
# flux-mirror.yaml — Amazon ECR destination
apiVersion: mirror.plugin.fluxcd.io/v1beta1
kind: Config
artifacts:
  - source: ghcr.io/fluxcd/source-controller   # container image
    destination: 123456789012.dkr.ecr.us-east-1.amazonaws.com/fluxcd/source-controller
    selector: { semver: ">=1.0.0", limit: 5 }
    includeReferrers: true
  - source: ghcr.io/stefanprodan/charts/podinfo # OCI Helm chart
    destination: 123456789012.dkr.ecr.us-east-1.amazonaws.com/charts/podinfo
    selector: { semver: ">=6.0.0", limit: 5 }
  - source: ghcr.io/stefanprodan/manifests/podinfo # Flux OCI artifact
    destination: 123456789012.dkr.ecr.us-east-1.amazonaws.com/manifests/podinfo
    selector: { regex: { pattern: "^latest$" }, sortBy: alphabetical }
hosts:
  - host: ghcr.io
    username: my-org
    credential:
      value: ${GH_TOKEN}
  - host: 123456789012.dkr.ecr.us-east-1.amazonaws.com
    provider: ecr
```

For ACR or GAR, change only the destination host and `provider`:

```yaml
# Azure ACR
  - host: myregistry.azurecr.io
    provider: acr
# Google GAR
  - host: us-docker.pkg.dev
    provider: gar
```

The workflow differs only in the cloud login step, all using GitHub Actions OIDC
(no long-lived cloud keys):

```yaml
# .github/workflows/mirror-to-ecr.yaml
name: mirror-to-ecr
on:
  schedule:
    - cron: "0 */6 * * *"
permissions:
  contents: read
  packages: read
  id-token: write          # required for GitHub Actions OIDC
jobs:
  sync:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: fluxcd/flux-mirror/actions/setup@main
      # --- Amazon ECR ---
      - uses: aws-actions/configure-aws-credentials@v4
        with:
          role-to-assume: arn:aws:iam::123456789012:role/flux-mirror
          aws-region: us-east-1
      # --- Azure ACR (instead of the AWS step) ---
      # - uses: azure/login@v2
      #   with:
      #     client-id: ${{ secrets.AZURE_CLIENT_ID }}
      #     tenant-id: ${{ secrets.AZURE_TENANT_ID }}
      #     subscription-id: ${{ secrets.AZURE_SUBSCRIPTION_ID }}
      # --- Google GAR (instead of the AWS step) ---
      # - uses: google-github-actions/auth@v2
      #   with:
      #     workload_identity_provider: projects/123/locations/global/workloadIdentityPools/gh/providers/gh
      #     service_account: flux-mirror@my-project.iam.gserviceaccount.com
      - run: flux-mirror sync ./flux-mirror.yaml --no-progress
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

### Mirror into ECR, ACR, or GAR from a CronJob with workload identity

Inside a cluster, a `CronJob` can mirror on a schedule using the pod's workload
identity. The destination cloud registry is authenticated with
[`hosts[].provider`](config.md#cloud-registry-providers) (which uses the ambient
cloud credential chain — IRSA, AKS Workload Identity, or GKE Workload Identity).
The source registry here accepts the cluster's ServiceAccount OIDC tokens, so it
is authenticated with a projected ServiceAccount token sent as an HTTP Bearer
credential (no `username`).

First, the ServiceAccount, annotated for each provider's workload identity. The
projected token's audience must be the **source** registry host:

```yaml
# EKS with IRSA (annotation mandatory)
apiVersion: v1
kind: ServiceAccount
metadata:
  name: flux-mirror
  namespace: flux-system
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/flux-mirror
# EKS Pod Identity needs no annotation (the association is created via the EKS
# API); the same ServiceAccount works.
---
# AKS Workload Identity (annotation mandatory; the pod also needs the
# azure.workload.identity/use: "true" label)
apiVersion: v1
kind: ServiceAccount
metadata:
  name: flux-mirror
  namespace: flux-system
  annotations:
    azure.workload.identity/client-id: 00000000-0000-0000-0000-000000000000
---
# GKE Workload Identity Federation. With direct KSA-to-IAM bindings no
# annotation is needed; when impersonating a GCP service account, annotate it:
apiVersion: v1
kind: ServiceAccount
metadata:
  name: flux-mirror
  namespace: flux-system
  annotations:
    iam.gke.io/gcp-service-account: flux-mirror@my-project.iam.gserviceaccount.com
```

The config authenticates the destination with `provider` and the source with the
projected token. Because [file-path fields are confined to the config's own
directory](config.md#resolving-file-paths), the config and the token
are mounted together (below) so a relative `fromPath` resolves:

```yaml
# mirror.yaml
apiVersion: mirror.plugin.fluxcd.io/v1beta1
kind: Config
artifacts:
  - source: source-registry.example.com/library/app
    destination: 123456789012.dkr.ecr.us-east-1.amazonaws.com/library/app
    selector: { semver: ">=1.0.0", limit: 5 }
hosts:
  # Source: a registry that validates the cluster's ServiceAccount OIDC token.
  # No username → the token is sent as an HTTP Bearer credential. Its audience
  # (the source host) is set by the projected-token volume, not here.
  - host: source-registry.example.com
    credential:
      fromPath: registry-token
  # Destination: ECR via the pod's IRSA identity (use acr/gar to switch clouds).
  - host: 123456789012.dkr.ecr.us-east-1.amazonaws.com
    provider: ecr
```

The `CronJob` runs as the annotated ServiceAccount and projects a token whose
audience is the source registry, combined with the config in one volume so the
relative `fromPath` stays within the config directory:

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: flux-mirror
  namespace: flux-system
spec:
  schedule: "0 */6 * * *"
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
      template:
        metadata:
          labels:
            azure.workload.identity/use: "true"   # AKS Workload Identity only
        spec:
          serviceAccountName: flux-mirror
          restartPolicy: OnFailure
          containers:
            - name: flux-mirror
              image: ghcr.io/fluxcd/flux-mirror:latest
              args: ["sync", "/config/flux/mirror.yaml", "--no-progress"]
              volumeMounts:
                - name: flux
                  mountPath: /config/flux
                  readOnly: true
          volumes:
            # One projected volume holds both the config and the registry token,
            # so /config/flux/mirror.yaml can reference ./registry-token.
            - name: flux
              projected:
                sources:
                  - configMap:
                      name: flux-mirror-config   # holds mirror.yaml
                  - serviceAccountToken:
                      path: registry-token
                      audience: source-registry.example.com
                      expirationSeconds: 3600
```

### Mirror with the AWS credential provider into a SPIFFE mTLS registry

This `CronJob` pulls container images from a registry that authenticates the
caller's AWS identity, and pushes them to a registry that requires SPIFFE
X.509-SVID mTLS. The source host uses the
[`aws` credential provider](config.md#token-providers), which signs an
`sts:GetCallerIdentity` request the source registry replays to AWS STS, so the
pod needs AWS credentials via IRSA. The destination host uses
[`tls`](config.md#transport-tls) with a SPIFFE client certificate and
SPIFFE server verification, and no HTTP credential — the registry authorizes the
client by its X.509-SVID. The SPIFFE SVIDs and trust bundle come from the
Workload API, which the SPIFFE CSI driver exposes to the pod; go-spiffe locates
the socket through `SPIFFE_ENDPOINT_SOCKET`.

The ServiceAccount is annotated for IRSA so the `aws` provider can sign as the
mapped AWS role:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: flux-mirror
  namespace: flux-system
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/flux-mirror
```

```yaml
# mirror.yaml
apiVersion: mirror.plugin.fluxcd.io/v1beta1
kind: Config
artifacts:
  - source: source-registry.example.com/library/app
    destination: registry.example.org/library/app
    selector: { semver: ">=1.0.0", limit: 5 }
hosts:
  # Source: a registry that validates the caller's AWS identity. No username →
  # the signed sts:GetCallerIdentity envelope is sent as an HTTP Bearer credential.
  - host: source-registry.example.com
    credential:
      provider: aws
  # Destination: SPIFFE mTLS in both directions, no HTTP credential. Our client
  # X.509-SVID authenticates us; the server's SVID is verified against our own
  # trust domain.
  - host: registry.example.org
    tls:
      clientAuth:
        provider: x509-svid
      serverAuth:
        spiffe:
          trustDomain: self
```

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: flux-mirror
  namespace: flux-system
spec:
  schedule: "0 */6 * * *"
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
      template:
        spec:
          serviceAccountName: flux-mirror
          restartPolicy: OnFailure
          containers:
            - name: flux-mirror
              image: ghcr.io/fluxcd/flux-mirror:latest
              args: ["sync", "/config/flux/mirror.yaml", "--no-progress"]
              env:
                # go-spiffe reads the Workload API socket from this variable.
                - name: SPIFFE_ENDPOINT_SOCKET
                  value: unix:///spiffe-workload-api/spire-agent.sock
              volumeMounts:
                - name: config
                  mountPath: /config/flux
                  readOnly: true
                - name: spiffe-workload-api
                  mountPath: /spiffe-workload-api
                  readOnly: true
          volumes:
            - name: config
              configMap:
                name: flux-mirror-config   # holds mirror.yaml
            # The SPIFFE CSI driver exposes the SPIRE Agent Workload API socket.
            - name: spiffe-workload-api
              csi:
                driver: csi.spiffe.io
                readOnly: true
```

### Preview, resync, and CI invocations

```bash
# Preview the plan without writing to the destination.
flux mirror sync ./flux-mirror.yaml --dry-run -o yaml

# Force-resync every drifted tag.
flux mirror sync ./flux-mirror.yaml --overwrite

# CI-friendly: no spinner. For immutable destinations, keep CI green on drift.
flux mirror sync ./flux-mirror.yaml --no-progress --drift-exit-code=0
```
