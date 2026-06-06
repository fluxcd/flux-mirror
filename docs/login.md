# Flux Mirror Login Command

The `flux-mirror login` command resolves the credentials configured under
[`hosts`](./config.md#hosts) and logs in to those registries. It mints the
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
`$DOCKER_CONFIG`, else `~/.docker`).

What gets written depends on the host:
- A cloud `provider` host, or a `credential` host with `username` set, writes
  `username`/`password`/`auth`.
- A `credential` host **without** `username` writes the bearer `registrytoken`
  field instead. Because credential helpers only store username/secret pairs, a
  `registrytoken` always goes to the config **file** (never a keychain helper).
  See [`credential.username`](./config.md#bearer-token-vs-usernamepassword-credentialusername).

Pass `--plaintext` to force the base64 `config.json` entry and bypass any
configured or auto-detected credential helper (applies to the username/password
case; registrytoken is always file-based).

## What the credential is

Exactly one source is set per host (enforced by config validation):

| Source     | Credential                                                                                     |
|------------|------------------------------------------------------------------------------------------------|
| `provider` | A freshly minted token for the provider (`github`/`forgejo`/`gcp`/`azure`, the `aws` SigV4 envelope, or a `jwt-svid` from the SPIFFE Workload API). Carries its own expiry. |
| `fromEnv`  | The value of the named environment variable, as-is.                                            |
| `fromPath` | The contents of the file at the path, with surrounding whitespace trimmed.                     |
| `jwkPath`  | A freshly signed JWT (`iss`/`sub`/`aud` from the config), valid for the credential's `exp` (default **60s**). Set `exp` in the config for a longer-lived login token. |

## Hosts-only config

`login` reads only the `hosts` section, so — unlike `sync` — it accepts a config
with no `artifacts` or `charts`:

```yaml
apiVersion: mirror.fluxcd.io/v1alpha1
kind: Config
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
