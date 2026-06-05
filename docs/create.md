# Flux Mirror Create Command

The `flux-mirror create` command writes Kubernetes resources derived from a
flux-mirror config.

## Subcommands

| Command                            | Description                                                          |
|------------------------------------|---------------------------------------------------------------------|
| `flux-mirror create secret <name>` | Create or replace a `dockerconfigjson` Secret with host credentials. |

## `create secret`

```
flux-mirror create secret <name> [flags]
```

Resolve the credential configured under [`auth.hosts`](./config.md#auth) for each
selected host and write them into a Kubernetes Secret of type
`kubernetes.io/dockerconfigjson` — the same shape `kubectl create secret
docker-registry` produces. An existing Secret with the same name is **replaced**.

These are the *same* credentials `flux-mirror sync` and `flux-mirror login`
resolve, so the Secret can be referenced as an `imagePullSecret` (or by Flux's
`OCIRepository`/`HelmRepository`) to pull from a registry that understands the
configured identity.

| Argument | Description                                |
|----------|--------------------------------------------|
| `<name>` | Name of the Secret to create or replace.   |

### Flags

| Flag             | Description                                                                                |
|------------------|-------------------------------------------------------------------------------------------|
| `-f`, `--config` | Path to the flux-mirror config, or `-` for stdin. Defaults to `$FLUX_MIRROR_CONFIG`, else a path next to the binary (`<binary>.config`). |
| `--host`         | Registry host from the config to include. Repeatable; defaults to **all** hosts.          |

In addition, the command accepts the standard `kubectl` connection flags and
their env vars — `--kubeconfig` (`$KUBECONFIG`), `--context`, `--cluster`,
`-n`/`--namespace`, `--user`, `--server`, `--token`, `--request-timeout`,
`--as`, `--certificate-authority`, `--insecure-skip-tls-verify`, and so on. The
namespace defaults to the one from the active kubeconfig context (or the
in-cluster service-account namespace).

### Behavior

- Works both **locally** (kubeconfig) and **in-cluster** (falls back to the
  in-cluster config and service-account namespace).
- With no `--host`, every host in `auth.hosts` is included. A `--host` that is
  not present in the config is an error.
- The Secret's `.dockerconfigjson` holds one `auths` entry per host, each with a
  placeholder username (`flux-mirror`) and the resolved credential as the
  password — the identity is carried entirely by the credential.
- Credentials are short-lived (provider tokens, freshly signed JWTs). Re-run the
  command to refresh the Secret before they expire; it replaces the existing one
  in place.

### Examples

```bash
# Secret for all hosts in the default config, in the current namespace.
flux-mirror create secret regcreds

# Specific hosts, a specific namespace and kubeconfig context.
flux-mirror create secret regcreds -n flux-system --context prod \
  --host registry.example.com --host other.example.com -f ./flux-mirror.yaml

# In-cluster (e.g. from a CronJob): kubeconfig and namespace are auto-detected.
flux-mirror create secret regcreds -f /etc/flux-mirror/config.yaml
```
