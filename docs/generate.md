# Flux Mirror Generate Commands

The `flux-mirror generate` commands produce the artifacts that back JWK-based
registry authentication (see the [Auth section](./config.md#auth) of the config
specification): an EdDSA key pair, and Docker config / Kubernetes pull-secret
files holding a short-lived JWT signed with the private key.

## `generate jwk-pair`

Generates an EdDSA JSON Web Key pair and writes it to a directory as
`pubkey.json` and `privkey.json`.

```
flux-mirror generate jwk-pair -f <dir> [--kid <id>]
```

- The directory is created if it does not exist (like `mkdir -p`).
- The command errors **without writing anything** if either `pubkey.json` or
  `privkey.json` already exists.
- `privkey.json` is written with `0600` permissions; `pubkey.json` with `0644`.

| Flag           | Default              | Description                                                          |
|----------------|----------------------|---------------------------------------------------------------------|
| `-f`, `--file` |                      | Directory to write the pair into (required; created if missing).     |
| `--kid`        | JWK thumbprint       | Key id stamped on both keys. Defaults to the RFC 7638 thumbprint.    |

Publish `pubkey.json` at your issuer's JWKS endpoint; reference `privkey.json`
from a config host's `jwkPath`.

## `generate docker-config from-jwk`

Issues a JWT for every config host that uses `jwkPath` and writes a Docker config
file next to that host's private key.

```
flux-mirror generate docker-config from-jwk <config> [--exp <duration>]
```

For each `jwkPath` host, the command:

1. reads `privkey.json` from the key's directory (the base dir of `jwkPath`),
2. issues a JWT with the host's `iss`, `sub`, and `aud` (defaulting to the host),
3. writes `docker-config.json` in that directory, with the **host as the
   username** and the **JWT as the password**.

Hosts that share a key directory are upserted into the same `docker-config.json`
in config order. An existing file is preserved and upserted into.

## `generate pull-secret from-jwk`

Identical to `docker-config from-jwk`, but writes `pull-secret.yaml` — a
Kubernetes `kubernetes.io/dockerconfigjson` Secret named `docker` in namespace
`default`, wrapping the same credentials.

```
flux-mirror generate pull-secret from-jwk <config> [--exp <duration>]
```

### Flags

| Flag    | Default | Description                                                  |
|---------|---------|--------------------------------------------------------------|
| `--exp` | `1h`    | Lifetime of the issued JWT (Go duration, e.g. `30m`, `24h`). |

> The issued JWT is a **static** bearer credential baked into the generated
> file — unlike `flux-mirror sync`, which mints a fresh token per request. Choose
> `--exp` accordingly and regenerate before expiry.
