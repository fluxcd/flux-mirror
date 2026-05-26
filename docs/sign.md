# Flux Mirror Sign Command

The `flux-mirror sign` command signs credentials with a private JWK. Use it when
a pull path cannot run `flux-mirror sync` (for example, a Kubernetes cluster
pulling images directly or a pull-through cache in your own registry) and you
need a static, longer-lived JWT instead of the fresh 60-second tokens minted by
[`auth.hosts[].jwt.jwkPath`](./config.md#auth).

## Subcommands

| Command                | Description                                            |
|------------------------|--------------------------------------------------------|
| `flux-mirror sign jwt` | Sign a single JWT with a private JWK.                  |

## `sign jwt`

```
flux-mirror sign jwt --iss <iss> --aud <aud> --sub <sub> --exp <duration> \
                     -k <key-set> -o <output-file>
```

Reads the private JWK from `-k`, mints a compact-serialized JWT carrying the
given claims, and writes it to `-o`. The signing algorithm is derived from the
key type (EdDSA for Ed25519, ES256/384/512 for ECDSA P-256/P-384/P-521).

### Flags

| Flag                | Required | Description                                                                                                       |
|---------------------|----------|-------------------------------------------------------------------------------------------------------------------|
| `--iss`             | yes      | Issuer claim (`iss`).                                                                                              |
| `--aud`             | yes      | Audience claim (`aud`).                                                                                            |
| `--sub`             | yes      | Subject claim (`sub`).                                                                                             |
| `--exp`             | yes      | Token lifetime as a Go duration (e.g. `1h`, `24h`, `2160h`). Must be strictly positive.                            |
| `-k, --key-set`     | yes      | Path to the private JWK file. May be a bare JWK or a JWK set (`{"keys":[...]}`) with exactly one key.              |
| `-o, --output`      | yes      | Path to write the signed JWT to. Parent directories are created if missing. The command refuses to overwrite an existing file. |

### Token shape

The token carries all seven [RFC 7519](https://datatracker.ietf.org/doc/html/rfc7519)
registered claims, with `iat` at signing time, `nbf` backdated by 30 seconds to
absorb clock skew, `exp = iat + --exp`, and a random `jti`. The signing key's
`kid` is set in the JWS header.

### Behavior

- `--exp <= 0` is rejected before any key is read.
- The output file is checked for existence before signing, so a failure never
  leaves a stale token on disk. Parent directories are created with `mkdir -p`.
- The output file is written with mode `0600`.

### Examples

Mint a 1-hour token for a registry host using the key pair from
[`keygen sig`](./keygen.md):

```bash
flux-mirror sign jwt \
  --iss https://my-issuer.example \
  --aud registry.example.com \
  --sub client-id \
  --exp 1h \
  -k ./keys/registry/privkey.json \
  -o ./tokens/registry.jwt
```

Pipe directly into `docker login`:

```bash
flux-mirror sign jwt \
  --iss https://my-issuer.example \
  --aud registry.example.com \
  --sub client-id \
  --exp 24h \
  -k ./keys/registry/privkey.json \
  -o ./tokens/registry.jwt
cat ./tokens/registry.jwt | docker login registry.example.com -u registry.example.com --password-stdin
```
