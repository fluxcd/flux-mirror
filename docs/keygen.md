# Flux Mirror Keygen Command

The `flux-mirror keygen` command generates an EdDSA JSON Web Key pair for
JWK-based registry auth. The output files plug directly into the
[`auth.hosts[].credential.jwkPath`](./config.md#auth) config field, which
`flux-mirror sync` and [`flux-mirror login`](./login.md) use to sign JWTs.

```
flux-mirror keygen [flags]
```

Generates an EdDSA Ed25519 key pair and writes it to a directory as two JWK
sets:

| File          | Contents                | Mode |
|---------------|-------------------------|------|
| `pubkey.json` | Public JWK set, share this with the registry or publish it as JWKS. | `0644` |
| `privkey.json`| Private JWK set, keep it secret.                                    | `0600` |

Both files use the standard JWK set shape `{"keys":[...]}`. The two keys share
a UUIDv6 `kid` so the signature can be matched against the public set.

### Flags

| Flag                  | Default | Description                                                                |
|-----------------------|---------|----------------------------------------------------------------------------|
| `-o, --output-dir`    | `.`     | Directory to write `pubkey.json` and `privkey.json` into. Created if missing. |

### Behavior

- The directory is created with `mkdir -p` if it does not exist.
- The command refuses to overwrite either file if it already exists. Delete or
  point at a fresh directory to rotate.
- The private file is written with mode `0600`. If the public write fails, the
  private file is rolled back so a partial run never leaves a stray key.

### Examples

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

### Using the generated keys

- **In a sync config** — point `auth.hosts[].credential.jwkPath` at `privkey.json`.
  See the [`auth` section of the config spec](./config.md#auth).
- **To mint a one-shot JWT** — reference `privkey.json` from a host's
  `credential.jwkPath` and run [`flux-mirror login`](./login.md).
- **To grant access** — share `pubkey.json` with the registry operator, or
  publish it at an HTTPS URL the registry can fetch.
