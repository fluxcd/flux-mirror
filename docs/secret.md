# Flux Mirror Secret Command

```
flux-mirror secret <name> [flags]
```

Resolve the credential configured under [`hosts`](./config.md#hosts) for each
selected host and write them into a Kubernetes Secret of type
`kubernetes.io/dockerconfigjson` — the same shape `kubectl create secret
docker-registry` produces.

By default the Secret is upserted: if one with the same name already exists it
is replaced in place. This is the right semantic for the command's main use
case — rotating short-lived pull Secrets from a `CronJob`. Pass `--create` to
instead fail if the Secret already exists, matching `kubectl create secret
docker-registry`.

These are the *same* credentials `flux-mirror sync` and `flux-mirror login`
resolve, so the Secret can be referenced as an `imagePullSecret` (or by Flux's
`OCIRepository`/`HelmRepository`) to pull from a registry that understands the
configured identity.

| Argument | Description                                |
|----------|--------------------------------------------|
| `<name>` | Name of the Secret to create or replace.   |

## Flags

| Flag             | Description                                                                                |
|------------------|-------------------------------------------------------------------------------------------|
| `-f`, `--config` | Path to the flux-mirror config, or `-` for stdin. Defaults to `$FLUX_MIRROR_CONFIG`, else a path derived from the executable location. |
| `--host`         | Registry host from the config to include. Repeatable; defaults to all hosts.          |
| `--create`       | Fail if the Secret already exists instead of replacing it (like `kubectl create secret docker-registry`). |

In addition, the command accepts the standard `kubectl` connection flags and
their env vars — `--kubeconfig` (`$KUBECONFIG`), `--context`, `--cluster`,
`-n`/`--namespace`, `--user`, `--server`, `--token`, `--request-timeout`,
`--as`, `--certificate-authority`, `--insecure-skip-tls-verify`, and so on. The
namespace defaults to the one from the active kubeconfig context (or the
in-cluster service-account namespace).

## Behavior

- Works both locally (kubeconfig) and in-cluster (falls back to the
  in-cluster config and service-account namespace).
- By default the Secret is upserted (created, or replaced if it exists). With
  `--create`, an existing Secret of the same name is an error.
- With no `--host`, every host in `hosts` is included. A `--host` that is
  not present in the config is an error.
- The Secret's `.dockerconfigjson` holds one `auths` entry per host. A cloud
  `provider` host, and a `credential` host with `username` set, write
  `username`/`password`/`auth`. A `credential` host without `username` writes
  the bearer `registrytoken` field (understood by go-containerregistry/Flux, not
  by `kubelet`). See [`credential.username`](./config.md#bearer-token-vs-usernamepassword-credentialusername).
- Credentials are short-lived (provider tokens, freshly signed JWTs). Re-run the
  command to refresh the Secret before they expire; by default it replaces the
  existing one in place.

## Examples

```bash
# Upsert a Secret for all hosts in the default config, in the current namespace.
flux-mirror secret regcreds

# Fail if the Secret already exists, like 'kubectl create secret docker-registry'.
flux-mirror secret regcreds --create

# Specific hosts, a specific namespace and kubeconfig context.
flux-mirror secret regcreds -n flux-system --context prod \
  --host registry.example.com --host other.example.com -f ./flux-mirror.yaml

# In-cluster (e.g. from a CronJob): kubeconfig and namespace are auto-detected.
flux-mirror secret regcreds -f /etc/flux-mirror/config.yaml
```
