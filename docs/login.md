# Flux Mirror Login Command

The `flux-mirror login` command resolves the credentials configured under
[`auth.hosts`](./config.md#auth) and logs in to those registries. It mints the
*same* credentials `flux-mirror sync` would attach, but once. By default it
stores them in the Docker config, exactly like `docker login`.

```
flux-mirror login [flags]
```

| Flag              | Description                                                                                          |
|-------------------|-----------------------------------------------------------------------------------------------------|
| `-f`, `--config`  | Path to the flux-mirror config, or `-` for stdin. Defaults to `$FLUX_MIRROR_CONFIG`, else a path derived from the executable location. |
| `--host`          | Registry host from the config to log in. Repeatable; defaults to **all** hosts.                     |
| `--docker-config` | Docker config directory, like `docker --config` (default `$DOCKER_CONFIG`, else `~/.docker`).       |
| `--plaintext`     | Store the credential base64-encoded in `config.json`, bypassing any OS keychain helper.             |

## Where the credential goes

By default the credential is written through the Docker credential store,
exactly like `docker login`:

- If a credential helper is configured (`credsStore`/`credHelpers`) — or, on a
  fresh config, one is auto-detected for the platform (`docker-credential-*` on
  `PATH`, e.g. `osxkeychain`, `secretservice`, `pass`, `wincred`) — the secret
  goes to the **OS keychain**, not the config file.
- Otherwise it falls back to a base64-encoded entry in `config.json` (the same
  plaintext fallback `docker login` uses when no helper is available).

The config location follows Docker's own discovery (`--docker-config`, else
`$DOCKER_CONFIG`, else `~/.docker`). The stored username is a placeholder
(`flux-mirror`) — the identity is carried entirely by the credential.

Pass `--plaintext` to force the base64 `config.json` entry and bypass any
configured or auto-detected credential helper.

## What the credential is

Exactly one source is set per host (enforced by config validation):

| Source     | Credential                                                                                     |
|------------|------------------------------------------------------------------------------------------------|
| `provider` | A freshly minted token for the provider (`github`/`forgejo`/`gcp`/`azure`, or the `aws` SigV4 envelope). Carries its own expiry. |
| `fromEnv`  | The value of the named environment variable, as-is.                                            |
| `fromPath` | The contents of the file at the path, with surrounding whitespace trimmed.                     |
| `jwkPath`  | A freshly signed JWT (`iss`/`sub`/`aud` from the config), valid for the credential's `exp` (default **60s**). Set `exp` in the config for a longer-lived login token. |

## Auth-only config

`login` reads only the `auth` section, so — unlike `sync` — it accepts a config
with no `artifacts` or `charts`:

```yaml
apiVersion: mirror.fluxcd.io/v1alpha1
kind: Config
auth:
  hosts:
    - host: registry.example.com
      credential:
        jwkPath: ./keys/registry/privkey.json
        iss: https://my-issuer.example
        sub: client-id
        # aud: registry.example.com   # optional, defaults to host
        # exp: 1h                      # optional, defaults to 60s
```

## Examples

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

- For `aws`, the credential is a JWT-shaped envelope wrapping a signed
  `sts:GetCallerIdentity` request, not an OIDC token. The destination registry
  must understand this scheme — see the [`provider` row](./config.md#token-sources).
- The credential is short-lived. Re-run `login` when it expires; for `provider`
  sources the registry re-validates on each request, so a stored credential
  stops working once it lapses.
```
