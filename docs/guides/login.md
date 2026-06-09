# Flux Mirror Login Command

The `flux-mirror login` command resolves the credentials configured under
[`hosts`](../config/README.md#hosts) and stores them in the Docker config, the
same way `docker login` does. It mints the *same* credentials
[`flux-mirror sync`](./sync.md) would attach, but once — so any tool that reads
the Docker config (`docker`, `crane`, `flux push artifact`, `helm`) can then
authenticate as those identities.

## Synopsis

```
flux-mirror login [flags]
```

## Configuration source

The config path is resolved as: the `--config`/`-f` flag, else
`$FLUX_MIRROR_CONFIG`, else a path next to the executable (`<executable>.config`).
`-f -` reads the config from stdin.

`login` reads only the [`hosts`](../config/README.md#hosts) section, so — unlike
`sync` — it accepts a config with no `artifacts` or `charts`.

## Flags

| Flag              | Default                          | Description                                                                                           |
|-------------------|----------------------------------|-------------------------------------------------------------------------------------------------------|
| `-f, --config`    | `$FLUX_MIRROR_CONFIG`, else `<executable>.config` | Path to the flux-mirror config, or `-` for stdin.                                    |
| `--host`          | all hosts                        | Registry host from the config to log in. Repeatable; defaults to all hosts.                           |
| `--docker-config` | `$DOCKER_CONFIG`, else `~/.docker` | Docker config directory, like `docker --config`.                                                   |
| `--plaintext`     | `false`                          | Store the credential base64-encoded in `config.json`, bypassing any OS keychain credential helper.    |

The global `--timeout` flag (default `1m`) bounds credential resolution, which
for `provider` sources involves a network call to mint the token. The global
`--no-envsubst` flag disables config environment substitution.

## Where the credential goes

By default, the credential is written through the Docker credential store,
exactly like `docker login`:

- If a credential helper is configured (`credsStore`/`credHelpers`) — or, on a
  fresh config, one is auto-detected for the platform (a `docker-credential-*`
  binary on `PATH`, e.g. `osxkeychain`, `secretservice`, `pass`, `wincred`) — the
  secret goes to the OS keychain, not the config file.
- Otherwise, it falls back to a base64-encoded entry in `config.json` (the same
  plaintext fallback `docker login` uses when no helper is available).

What gets written depends on the host:

- A cloud [`provider`](../config/README.md#cloud-registry-providers) host, or a
  [`credential`](../config/README.md#per-host-credential) host with `username` set,
  writes `username`/`password`/`auth`.
- A `credential` host without `username` writes the bearer `registrytoken` field
  instead. Because credential helpers only store username/secret pairs, a
  `registrytoken` always goes to the config file (never a keychain helper). See
  [Bearer token vs. username/password](../config/README.md#bearer-token-vs-usernamepassword).

Pass `--plaintext` to force the base64 `config.json` entry and bypass any
configured or auto-detected helper (this applies to the username/password case;
`registrytoken` is always file-based).

> **Note:** a TLS-only host (only `tls`, no `credential`/`provider`) has nothing
> to store and is skipped with a `• skipping <host>` message.

## Examples

### Log in to ECR, ACR, or GAR from GitHub Actions

Authenticate to the cloud provider with its GitHub Actions OIDC login action,
then `flux-mirror login` with a [`provider`](../config/README.md#cloud-registry-providers)
host. `login` mints the registry's native credentials from that ambient identity
and writes them into the Docker config. Afterwards `flux push artifact` (and any
other Docker-config-aware tool) authenticates **without** a `--provider` flag,
because the credentials are already in the config.

A hosts-only config — switch clouds by changing the host and `provider`:

```yaml
# hosts.yaml
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

```yaml
# .github/workflows/push.yaml
permissions:
  contents: read
  id-token: write          # required for GitHub Actions OIDC
jobs:
  push:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: fluxcd/flux-mirror/actions/setup@main
      # --- Amazon ECR ---
      - uses: aws-actions/configure-aws-credentials@v4
        with:
          role-to-assume: arn:aws:iam::123456789012:role/flux-push
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
      #     service_account: flux-push@my-project.iam.gserviceaccount.com
      - run: flux-mirror login -f ./hosts.yaml
      # No --provider needed: the credential is already in the Docker config.
      - run: |
          flux push artifact \
            oci://123456789012.dkr.ecr.us-east-1.amazonaws.com/manifests/app:v1.0.0 \
            --path=./manifests --source="$GITHUB_REPOSITORY" --revision="$GITHUB_SHA"
```

### Log in to GHCR from GitHub Actions

GHCR authorizes by the token and ignores the username value, but it still expects
a username/password login — so set `username` to any value (for example a
`github.*` context key) and pass `GITHUB_TOKEN` as the password via `value`:

```yaml
# hosts.yaml
apiVersion: mirror.plugin.fluxcd.io/v1beta1
kind: Config
hosts:
  - host: ghcr.io
    username: ${GITHUB_REPOSITORY_OWNER}   # ignored by GHCR; any value works
    credential:
      value: ${GH_TOKEN}
```

```yaml
# .github/workflows/push.yaml
permissions:
  contents: read
  packages: write
jobs:
  push:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: fluxcd/flux-mirror/actions/setup@main
      - run: flux-mirror login -f ./hosts.yaml
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
      - run: |
          flux push artifact oci://ghcr.io/$GITHUB_REPOSITORY_OWNER/manifests/app:v1.0.0 \
            --path=./manifests --source="$GITHUB_REPOSITORY" --revision="$GITHUB_SHA"
```

### Log in to Docker Hub from GitHub Actions

Docker Hub uses a standard username/password login. Set `username` to the Docker
Hub account and pass an access token as the password via `value`:

```yaml
# hosts.yaml
apiVersion: mirror.plugin.fluxcd.io/v1beta1
kind: Config
hosts:
  - host: docker.io
    username: ${DOCKERHUB_USERNAME}
    credential:
      value: ${DOCKERHUB_TOKEN}
```

```yaml
# .github/workflows/push.yaml
jobs:
  push:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: fluxcd/flux-mirror/actions/setup@main
      - run: envsubst '${DOCKERHUB_USERNAME}' < hosts.yaml > rendered.yaml
        env:
          DOCKERHUB_USERNAME: ${{ secrets.DOCKERHUB_USERNAME }}
      - run: flux-mirror login -f ./rendered.yaml
        env:
          DOCKERHUB_TOKEN: ${{ secrets.DOCKERHUB_TOKEN }}
      - run: |
          flux push artifact oci://docker.io/$DOCKERHUB_USERNAME/manifests/app:v1.0.0 \
            --path=./manifests --source="$GITHUB_REPOSITORY" --revision="$GITHUB_SHA"
        env:
          DOCKERHUB_USERNAME: ${{ secrets.DOCKERHUB_USERNAME }}
```

### Common invocations

```bash
# Log in to every host in the default config.
flux-mirror login

# Specific hosts from a specific config file.
flux-mirror login --host registry.example.com --host other.example.com -f ./flux-mirror.yaml

# Read the config from stdin.
flux-mirror login -f - < flux-mirror.yaml

# Use an alternate Docker config directory, like 'docker --config'.
flux-mirror login --docker-config /tmp/docker

# Force a plaintext config.json entry instead of the OS keychain.
flux-mirror login --plaintext
```

## Notes

- The credential is short-lived (a freshly minted provider token, a signed JWT,
  or whatever `value`/`fromPath` holds). Re-run `login` before it expires; for
  `provider` sources the registry re-validates each request, so a stored
  credential stops working once it lapses. To mint a longer-lived login token,
  use a `jwkPath`/`jwkValue` credential with a longer `exp` — see
  [keygen](./keygen.md).
- For `aws`, the credential is a JWT-shaped envelope wrapping a signed
  `sts:GetCallerIdentity` request, not an OIDC token. The destination registry
  must understand this scheme — see [Token providers](../config/README.md#token-providers).
