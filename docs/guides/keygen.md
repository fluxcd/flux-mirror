# Flux Mirror Keygen Command

The `flux-mirror keygen` command generates an EdDSA (Ed25519) JSON Web Key pair
for [JWK-based registry auth](../config/README.md#per-host-credential). The output
files plug directly into the
[`hosts[].credential.jwkPath`](../config/README.md#per-host-credential) config field,
which [`sync`](./sync.md) and [`login`](./login.md) use to sign JWTs.

## Synopsis

```
flux-mirror keygen [flags]
```

It writes the key pair into a directory as two JWK sets:

| File           | Contents                                                            | Mode   |
|----------------|---------------------------------------------------------------------|--------|
| `pubkey.json`  | Public JWK set — share with the registry or publish it as JWKS.     | `0644` |
| `privkey.json` | Private JWK set — keep it secret.                                   | `0600` |

Both files use the standard JWK set shape `{"keys":[...]}`. The two keys share a
UUIDv6 `kid` so a signature made with the private key can be matched against the
public set.

## Flags

| Flag               | Default | Description                                                                   |
|--------------------|---------|-------------------------------------------------------------------------------|
| `-o, --output-dir` | `.`     | Directory to write `pubkey.json` and `privkey.json` into. Created if missing. |

## Behavior

- The output directory is created (`mkdir -p`) if it does not exist.
- The command refuses to overwrite either file if it already exists. Delete them
  or point at a fresh directory to rotate.
- The private file is written with mode `0600`. If the public write fails, the
  private file is rolled back, so a partial run never leaves a stray key behind.

```bash
# Write a key pair into the current directory.
flux-mirror keygen

# Write a key pair into ./keys/registry (created if missing).
flux-mirror keygen -o ./keys/registry
```

Output:

```
✔ private key set written to: ./keys/registry/privkey.json
✔ public key set written to: ./keys/registry/pubkey.json
```

## Example: mint a long-lived login token

Unlike `provider`/`fromEnv` credentials — whose lifetime is fixed by an external
issuer — a `jwkPath` credential is signed locally, so you control its lifetime
through [`exp`](../config/README.md#per-host-credential). This makes a key pair plus
`login` a convenient way to mint a single, long-lived bearer token (for example,
a year-long token for a CI system or an air-gapped agent).

**1. Generate the key pair.**

```bash
flux-mirror keygen -o ./keys/registry
```

**2. Reference the private key from a host credential, with a long `exp`.** Set
`iss`/`sub` to the identity the registry expects, and `exp` to the desired
lifetime (here ~1 year). With no `username`, the signed JWT is stored as a bearer
`registrytoken`:

```yaml
# config.yaml
apiVersion: mirror.fluxcd.io/v1beta1
kind: Config
hosts:
  - host: registry.example.com
    credential:
      jwkPath: ./keys/registry/privkey.json
      iss: https://my-issuer.example
      sub: ci-pusher
      # aud: registry.example.com   # optional, defaults to the host
      exp: 8760h                     # ~1 year
```

**3. Log in once to mint and store the token.**

```bash
flux-mirror login -f ./config.yaml
```

`login` signs one JWT valid for `exp` and writes it to the Docker config, so
tools like `flux push artifact` authenticate as that identity until it expires.

**4. Grant access on the registry side.** Share `pubkey.json` with the registry
operator, or publish it at an HTTPS URL the registry can fetch as JWKS, so the
registry can verify tokens signed by the matching private key (matched by `kid`).

> Re-run `login` to mint a fresh token before the current one expires. Treat
> `privkey.json` as a secret: anyone holding it can mint tokens for `sub` until
> the public key is rotated out.
