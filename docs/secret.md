---
weight: 40
linkTitle: Secret Command
---

# Flux Mirror Secret Command

The `flux mirror secret <name>` command resolves the credentials configured under
[`hosts`](config.md#hosts) for each selected host and writes them into a
Kubernetes Secret of type `kubernetes.io/dockerconfigjson` — the same shape
`kubectl create secret docker-registry` produces. These are the *same*
credentials [`sync`](sync.md) and [`login`](login.md) resolve, so the Secret
can be referenced as an `imagePullSecret`, or by a Flux
`OCIRepository`/`HelmRepository` `.spec.secretRef`, to pull from a registry that
understands the configured identity.

Its main use case is rotating short-lived pull Secrets from a `CronJob`.

## Synopsis

```
flux mirror secret <name> [flags]
```

`<name>` is the name of the Secret to create or replace.

By default the Secret is **upserted**: if one with the same name already exists it
is replaced in place. Pass `--create` to instead fail if the Secret already
exists, matching `kubectl create secret docker-registry`.

## Configuration source

The config path is resolved as: the `--config`/`-f` flag, else
`$FLUX_MIRROR_CONFIG`, else `<executable>.config`. `-f -` reads the config from
stdin. Like `login`, `secret` reads only the [`hosts`](config.md#hosts)
section and accepts a `hosts`-only config.

## Flags

| Flag             | Default     | Description                                                                                  |
|------------------|-------------|----------------------------------------------------------------------------------------------|
| `-f, --config`   | `$FLUX_MIRROR_CONFIG`, else `<executable>.config` | Path to the flux-mirror config, or `-` for stdin.      |
| `--host`         | all hosts   | Registry host from the config to include. Repeatable; defaults to all hosts.                  |
| `--create`       | `false`     | Fail if the Secret already exists instead of replacing it.                                    |

In addition, the command accepts the standard `kubectl` connection flags and
their env vars — `--kubeconfig` (`$KUBECONFIG`), `--context`, `--cluster`,
`-n`/`--namespace`, `--user`, `--server`, `--token`, `--request-timeout`, `--as`,
`--certificate-authority`, `--insecure-skip-tls-verify`, and so on. The namespace
defaults to the one from the active kubeconfig context, or the in-cluster
ServiceAccount namespace when running inside a pod.

The global `--timeout` flag (default `1m`) bounds credential resolution, which
for `provider` sources involves a network call to mint the token. The global
`--no-envsubst` flag disables config environment substitution.

## Behavior

- Works both locally (kubeconfig) and in-cluster (falls back to the in-cluster
  config and ServiceAccount namespace).
- By default the Secret is upserted (created, or replaced if it exists). With
  `--create`, an existing Secret of the same name is an error. On replace,
  existing labels and annotations are preserved.
- With no `--host`, every host in `hosts` is included. A `--host` that is not
  present in the config is an error.
- The Secret's `.dockerconfigjson` holds one `auths` entry per host. A cloud
  [`provider`](config.md#cloud-registry-providers) host, and a
  [`credential`](config.md#per-host-credential) host with `username` set,
  write `username`/`password`/`auth` (understood by `kubelet` and Flux). A
  `credential` host without `username` writes the bearer `registrytoken` field
  (understood by go-containerregistry and Flux, **not** by `kubelet`). See
  [Bearer token vs. username/password](config.md#bearer-token-vs-usernamepassword).
- A TLS-only host (only `tls`, no `credential`/`provider`) has nothing to put in
  the Secret and is skipped with a `• skipping <host>` message.
- Credentials are short-lived (provider tokens, freshly signed JWTs). Re-run the
  command to refresh the Secret before they expire; by default it replaces the
  existing one in place.

## Examples

### Rotate ECR, ACR, or GAR pull credentials from a CronJob

A cloud registry such as ECR, ACR, or GAR issues only short-lived credentials, so
a `CronJob` regenerates the pull Secret on a schedule using the pod's workload
identity. A [`provider`](config.md#cloud-registry-providers) host mints the
registry's native username/password from the ambient cloud identity, and `secret`
writes it as a `dockerconfigjson` Secret usable as an `imagePullSecret`.

The ServiceAccount carries each provider's workload-identity annotations:

```yaml
# EKS with IRSA (annotation mandatory; EKS Pod Identity needs none)
apiVersion: v1
kind: ServiceAccount
metadata:
  name: flux-mirror
  namespace: flux-system
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/flux-mirror
---
# AKS Workload Identity (annotation mandatory; pod also needs the
# azure.workload.identity/use: "true" label)
apiVersion: v1
kind: ServiceAccount
metadata:
  name: flux-mirror
  namespace: flux-system
  annotations:
    azure.workload.identity/client-id: 00000000-0000-0000-0000-000000000000
---
# GKE Workload Identity Federation (annotation only when impersonating a GSA)
apiVersion: v1
kind: ServiceAccount
metadata:
  name: flux-mirror
  namespace: flux-system
  annotations:
    iam.gke.io/gcp-service-account: flux-mirror@my-project.iam.gserviceaccount.com
```

The ServiceAccount also needs RBAC to manage the Secret it writes:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: flux-mirror-secret
  namespace: flux-system
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "create", "update"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: flux-mirror-secret
  namespace: flux-system
subjects:
  - kind: ServiceAccount
    name: flux-mirror
    namespace: flux-system
roleRef:
  kind: Role
  name: flux-mirror-secret
  apiGroup: rbac.authorization.k8s.io
```

A hosts-only config selecting the cloud registry (switch clouds via the host and
`provider`), stored in a `ConfigMap`:

```yaml
# mirror.yaml
apiVersion: mirror.plugin.fluxcd.io/v1beta1
kind: Config
hosts:
  - host: 123456789012.dkr.ecr.us-east-1.amazonaws.com
    provider: ecr
  # - host: myregistry.azurecr.io
  #   provider: acr
  # - host: us-docker.pkg.dev
  #   provider: gar
```

The `CronJob` rotates the Secret in its own namespace:

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: flux-mirror-regcreds
  namespace: flux-system
spec:
  schedule: "0 */6 * * *"          # rotate well within the credential lifetime
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
              # In-cluster: kubeconfig and namespace are auto-detected.
              args: ["secret", "regcreds", "-f", "/config/flux/mirror.yaml"]
              volumeMounts:
                - name: config
                  mountPath: /config/flux
                  readOnly: true
          volumes:
            - name: config
              configMap:
                name: flux-mirror-config   # holds mirror.yaml
```

The resulting `regcreds` Secret holds `username`/`password`/`auth`, so it works
both as a pod `imagePullSecret` and as a Flux `OCIRepository`/`HelmRepository`
`.spec.secretRef`.

### Rotate credentials for a registry that accepts ServiceAccount OIDC tokens

When the target registry validates the cluster's ServiceAccount OIDC tokens
directly, project a ServiceAccount token whose audience is the registry host and
feed it to a [`credential`](config.md#per-host-credential) host via
`fromPath`. Setting `username` writes the token as the password of a
username/password pair (`username`/`password`/`auth`), so the resulting Secret
works as a pod `imagePullSecret` (which `kubelet` can use) as well as a Flux
`OCIRepository`/`HelmRepository` `.spec.secretRef`. The username value is
whatever the registry expects alongside the token — many token-validating
registries ignore it but require it to be present.

Reuse the ServiceAccount and RBAC from the previous example (here the
ServiceAccount needs no cloud annotations — the registry trusts the cluster
issuer directly). The config references the token by a relative path, and because
[file-path fields are confined to the config's own
directory](config.md#resolving-file-paths), the config and the token
are mounted together:

```yaml
# mirror.yaml
apiVersion: mirror.plugin.fluxcd.io/v1beta1
kind: Config
hosts:
  - host: registry.example.com
    username: sa-oidc          # value the registry expects; often ignored
    credential:
      fromPath: registry-token   # the audience is set by the projected volume
```

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: flux-mirror-regcreds
  namespace: flux-system
spec:
  schedule: "*/30 * * * *"       # rotate ahead of the token's expirationSeconds
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
              args: ["secret", "regcreds", "-f", "/config/flux/mirror.yaml"]
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
                      audience: registry.example.com
                      expirationSeconds: 3600
```

### Rotate credentials using a SPIFFE JWT-SVID

When the registry accepts SPIFFE JWT-SVIDs, use the
[`jwt-svid` credential provider](config.md#token-providers): it fetches
a JWT-SVID for the audience (the registry host) from the SPIFFE Workload API.
Setting `username` writes the JWT-SVID as the password of a username/password
pair, so the resulting Secret works as a pod `imagePullSecret` (which `kubelet`
can use) as well as a Flux `OCIRepository`/`HelmRepository` `.spec.secretRef`.
The Workload API is exposed to the pod by the SPIFFE CSI driver, and go-spiffe
locates it through the `SPIFFE_ENDPOINT_SOCKET` environment variable.

Reuse the ServiceAccount and secret-management RBAC from the first example (no
cloud annotations are needed; the registry trusts the SPIFFE trust domain).

```yaml
# mirror.yaml
apiVersion: mirror.plugin.fluxcd.io/v1beta1
kind: Config
hosts:
  - host: registry.example.com
    username: spiffe           # value the registry expects; often ignored
    credential:
      provider: jwt-svid
      # aud defaults to the host (registry.example.com)
```

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: flux-mirror-regcreds
  namespace: flux-system
spec:
  schedule: "*/30 * * * *"       # rotate ahead of the JWT-SVID lifetime
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
              args: ["secret", "regcreds", "-f", "/config/flux/mirror.yaml"]
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

The resulting Secret holds `username`/`password`/`auth`, so it works as a pod
`imagePullSecret` as well as a Flux `OCIRepository`/`HelmRepository`
`.spec.secretRef`.

### Common invocations

```bash
# Upsert a Secret for all hosts in the default config, in the current namespace.
flux mirror secret regcreds

# Fail if the Secret already exists, like 'kubectl create secret docker-registry'.
flux mirror secret regcreds --create

# Specific hosts, a specific namespace and kubeconfig context.
flux mirror secret regcreds -n flux-system --context prod \
  --host registry.example.com --host other.example.com -f ./flux-mirror.yaml
```
