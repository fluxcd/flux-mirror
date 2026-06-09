# Flux Mirror Config

The `Config` API defines what `flux-mirror` mirrors and how it authenticates to
the registries involved. A single config lists the registry **hosts** to
authenticate, the **OCI artifacts** to copy between OCI registries, and the
**Helm charts** to pull from HTTP/S Helm repositories and publish to an OCI
registry.

Sources can be OCI registries or HTTP/S Helm repositories; destinations are
always OCI registries.

The config shape is published as a JSON Schema in
[`config-v1beta1.json`](config-v1beta1.json) and is consumed by
[`flux-mirror sync`](../guides/sync.md), [`flux-mirror login`](../guides/login.md),
and [`flux-mirror secret`](../guides/secret.md).

## Example

The following config mirrors a container image and a Helm chart, and
authenticates to the destination registry with a per-host credential:

```yaml
apiVersion: mirror.fluxcd.io/v1beta1
kind: Config
artifacts:
  - source: docker.io/stefanprodan/podinfo
    destination: quay.io/my-org/podinfo
    selector:
      semver: "6.x"
      limit: 2
    includeReferrers: true
    verify:
      provider: cosign
      minAge: 48h
      matchOIDCIdentity:
        - issuer: https://token.actions.githubusercontent.com
          subject: ^https://github\.com/stefanprodan/.*$
charts:
  - name: podinfo
    source: https://stefanprodan.github.io/podinfo
    destination: oci://quay.io/my-org/charts
    version: "6.x"
    limit: 2
hosts:
  - host: quay.io
    username: 'my-org+robot-user'
    credential:
      value: ${QUAY_TOKEN}
```

In the above example:

- The image at `docker.io/stefanprodan/podinfo` is mirrored to
  `quay.io/my-org/podinfo`. The two highest `6.x` tags are selected, their cosign
  signatures, SBOMs, and attestations are mirrored alongside them, and each tag
  is verified against the listed GitHub Actions OIDC identity (and required to be
  at least 48h old) before it is copied.
- The `podinfo` Helm chart is pulled from the HTTP/S repository at
  `https://stefanprodan.github.io/podinfo` and published as an OCI Helm chart to
  `quay.io/my-org/charts/podinfo`. The two highest `6.x` versions are mirrored.
- Requests to `quay.io` authenticate with the Quay robot token read from the
  `QUAY_TOKEN` environment variable, presented as a username/password pair
  because `username` is set.

You can run this with:

```bash
flux-mirror sync ./config.yaml
```

## Writing a Config spec

A config is a single YAML document with `apiVersion: mirror.fluxcd.io/v1beta1`,
`kind: Config`, and any of the `artifacts`, `charts`, and `hosts` lists.

At least one `artifacts` or `charts` entry is required, except for
[`flux-mirror login`](../guides/login.md) and
[`flux-mirror secret`](../guides/secret.md), which read only the `hosts` section
and accept a `hosts`-only config.

### Artifacts

`.artifacts` is an optional field that lists OCI artifacts to mirror between OCI
registries. The manifest and every referenced blob are copied byte-for-byte,
preserving digests; multi-arch images are mirrored faithfully as manifest lists,
with no platform filtering.

An entry handles any OCI-addressable content: container images, OCI Helm charts,
Flux OCI artifacts, or anything else stored in an OCI registry. To mirror a Helm
chart that is published to an HTTP/S Helm repository (and not yet in an OCI
registry), use [`.charts`](#charts) instead.

Each entry has two required fields:

- `.source`, the OCI repository the artifact is pulled from, written without a
  scheme (e.g. `ghcr.io/fluxcd/flux-cli`).
- `.destination`, the OCI repository the artifact is pushed to, written without a
  scheme. Selected tags are mirrored to the same tag at the destination.

It also has optional fields documented below: `.selector`
([Tag selection](#tag-selection)), `.includeReferrers` ([Referrers](#referrers)),
`.verify` ([Signature verification](#signature-verification)), and `.overwrite`
([Overwrite and drift behavior](#overwrite-and-drift-behavior)).

```yaml
artifacts:
  - source: ghcr.io/fluxcd/flux-cli
    destination: quay.io/example/flux-cli
    selector:
      semver: ">=2.7.0 <3.0.0"
      limit: 10
    includeReferrers: true
```

#### Tag selection

`.artifacts[].selector` is a required field that decides which tags of the
source repository are mirrored. It runs as a four-step pipeline — a regex
prefilter, a semver range filter, a sort, then a limit — always ordering highest
first and keeping the top N. The field offers the following subfields:

- `.regex`, an optional [Go regular expression](https://pkg.go.dev/regexp/syntax)
  prefilter applied to every tag before the sort and semver steps. It has a
  required `.pattern` (tags that do not match are dropped) and an optional
  `.extract` replacement string referencing named capture groups (e.g.
  `$version`). When `.extract` is set, the extracted value — not the raw tag — is
  used as the comparison key, so tags with prefixes or suffixes can still feed
  into a semver or numerical sort.
- `.semver`, an optional [semver range](https://github.com/Masterminds/semver#checking-version-constraints)
  (e.g. `>=2.7.0 <3.0.0` or `6.x`). Tags whose comparison key is not a valid
  semantic version are dropped.
- `.sortBy`, the sort strategy. The default is `semver`.
- `.limit`, how many tags to mirror, taken from the top of the sorted result. It
  defaults to `1`; `0` disables the cap and mirrors every matching tag.

The supported sort strategies are:

- `semver` parses each tag as a semantic version and orders by semver
  precedence. Tags that do not parse are silently dropped; use `.regex` to
  control what qualifies.
- `alphabetical` orders tags lexicographically. This suits tags shaped like
  `RELEASE.2024-11-12T08-30-15Z`, where lexical order matches chronological
  order.
- `numerical` parses each tag as a number and orders numerically. Tags that do
  not parse are silently dropped; this is typically paired with `.regex` to
  extract a numeric portion from a composite tag.

For example, to select the highest five tags by an embedded timestamp:

```yaml
selector:
  regex:
    pattern: '^.+-[0-9a-f]+-(?P<ts>\d+)$'
    extract: '$ts'
  sortBy: numerical
  limit: 5
```

#### Referrers

`.artifacts[].includeReferrers` is an optional field that, when `true`, also
mirrors the referrers of each selected tag — cosign signatures, SBOMs, and
attestations attached via the [OCI 1.1 referrers API](https://github.com/opencontainers/distribution-spec/blob/main/spec.md#listing-referrers).
It defaults to `false`.

#### Signature verification

`.artifacts[].verify` is an optional field that verifies the source artifact's
signature before any tag is copied. Every selected tag is verified at plan time;
if a selected tag has no matching valid signature, planning fails for that entry
and no copy job is scheduled. The field offers three subfields:

- `.provider`, the verification provider. It must be `cosign`. Verification uses
  [Cosign keyless bundles](https://docs.sigstore.dev) attached to the source
  artifact as OCI referrers, so the signing side must publish them as referrers.
- `.matchOIDCIdentity`, a list of accepted Fulcio certificate identities. Each
  entry provides an `.issuer` (the OIDC issuer URL, e.g.
  `https://token.actions.githubusercontent.com`) and a `.subject` (a Go regular
  expression matched against the certificate subject alternative name). The
  entries are evaluated in an OR fashion: a signature is accepted when any one
  identity matches.
- `.minAge`, an optional [duration](https://pkg.go.dev/time#ParseDuration) that
  requires a signature to be at least this old, measured from its verified
  transparency-log (Rekor) integrated timestamp. Tags whose signatures are valid
  but too recent are reported as `skipped` (reason `signature-too-new`) and not
  copied; signatures without an enforceable verified integrated timestamp fail
  verification. Use this to let signatures settle in the transparency log before
  mirroring.

```yaml
artifacts:
  - source: ghcr.io/stefanprodan/podinfo
    destination: quay.io/my-org/podinfo
    selector:
      semver: "*"
      limit: 1
    includeReferrers: true
    verify:
      provider: cosign
      minAge: 168h # 7 days
      matchOIDCIdentity:
        - issuer: https://token.actions.githubusercontent.com
          subject: ^https://github\.com/stefanprodan/.*$
```

### Charts

`.charts` is an optional field that lists Helm charts to mirror from an HTTP/S
Helm repository to an OCI destination. For each selected version, the chart
`.tgz` is downloaded from the source and re-published to the destination as a
Helm OCI artifact (a config blob with the chart metadata and a layer with the
tarball bytes). Charts use the same outcomes and overwrite/dry-run semantics as
artifacts; drift is detected by comparing the source tarball's content digest
against the destination chart-layer digest, so re-runs against an unchanged
source are idempotent.

Each entry offers the following subfields:

- `.name`, the chart name within the source repository. Required.
- `.source`, the HTTP/S Helm repository URL the chart is pulled from. Required;
  the scheme must be `http` or `https`, and the repository must serve a classic
  `index.yaml` plus chart tarballs.
- `.destination`, the OCI base URL the chart is published to. Required; the
  scheme must be `oci`.
- `.version`, an optional [semver range](https://github.com/Masterminds/semver#checking-version-constraints)
  (e.g. `>=2.7.0 <3.0.0`). Versions outside the range are dropped. It defaults to
  `*` (all versions).
- `.limit`, how many matching versions to mirror, highest first. It defaults to
  `1`; `0` disables the cap.
- `.overwrite`, see [Overwrite and drift behavior](#overwrite-and-drift-behavior).

Helm repository authentication is **not** configured under `hosts`. It is loaded
automatically from the ambient Helm repositories config — Helm's default
`repositories.yaml`, or `$HELM_REPOSITORY_CONFIG` when set — so adding the
repository with `helm repo add` is enough for `flux-mirror` to pick up matching
credentials. To mirror a chart that already lives in an OCI registry, list it
under [`.artifacts`](#artifacts) instead, where it is copied OCI-to-OCI as a
plain OCI artifact (authenticated through [`hosts`](#hosts)).

The chart `.name` is appended to `.destination` automatically: for `destination:
oci://quay.io/example/charts` and `name: nginx`, versions land at
`quay.io/example/charts/nginx:<version>`. Because OCI tags do not accept `+`,
semver build metadata (e.g. `1.2.3+meta`) is encoded with `_` in the destination
tag (`1.2.3_meta`); listing and comparison treat both forms as the same version.

```yaml
charts:
  - name: nginx
    source: https://charts.example.com
    destination: oci://quay.io/example/charts
    version: ">=15.0.0 <16.0.0"
    limit: 5
```

### Hosts

`.hosts` is an optional field that configures per-host authentication for OCI
registry requests, covering both source pulls and destination pushes. A host
listed here takes priority over the ambient Docker config; requests to hosts that
are not listed fall back to the Docker config and its credential helpers
(`~/.docker/config.json`, or `$DOCKER_CONFIG` when set).

> **Note:** the `hosts` section does not apply to HTTP/S Helm repositories
> (`.charts[].source`), which authenticate through the ambient Helm repositories
> config.

Each entry has one required field, `.host`, the registry host (with optional
port) the entry applies to (e.g. `registry.example.com` or `localhost:5000`),
unique across `hosts`. Beyond that, an entry configures **either** a
[`.credential`](#per-host-credential) **or** a
[`.provider`](#cloud-registry-providers) — the two are mutually exclusive. A
`credential` host (or a host with neither) may also set a
[`.tls`](#transport-tls) block; `tls` is not allowed with `provider`, because a
cloud registry is managed and its transport is not customized.
[`.maxChunkSize`](#blob-upload-chunking) tunes blob uploads independently of auth.
At least one of `credential`, `provider`, `tls`, or `maxChunkSize` must be set.

```yaml
hosts:
  # A per-host credential (HTTP-layer token).
  - host: registry.example.com
    credential:
      provider: github
  # A cloud registry provider, via ambient workload identity.
  - host: 123456789012.dkr.ecr.us-east-1.amazonaws.com
    provider: ecr
```

#### Per-host credential

`.hosts[].credential` configures an HTTP-layer registry credential for the host.
Exactly one **token source** subfield selects how the credential is obtained:

- `.provider`, mints a fresh, per-request credential for the audience using a
  cloud or CI identity, then caches and refreshes it on demand. One of `github`,
  `forgejo`, `gcp`, `azure`, `aws`, or `jwt-svid` — see
  [Token providers](#token-providers) for what each obtains and what the registry
  must accept.
- `.value`, sends the JSON Web Token configured inline as-is (e.g. a GitLab CI `id_token`). Use `${VAR}` to substitute it from the environment while loading the config.
- `.fromPath`, sends the token read from the file at the path, with surrounding
  whitespace trimmed. The file is re-read on every request, so the token can be
  rotated without restarting (useful for a projected ServiceAccount token).
- `.jwkPath`, signs a fresh JWT per request with the private JSON Web Key in the
  file at the path. The key may be a bare JWK or a single-key JWK set
  (`{"keys":[...]}`), and its `kid` is carried in the JWT header. Generate a key
  pair with [`flux-mirror keygen`](../guides/keygen.md).
- `.jwkValue`, signs a fresh JWT per request with the private JSON Web Key configured inline. Use `${VAR}` to substitute it from the environment while loading the config.

The signed and minted sources take additional claim subfields:

- `.iss` and `.sub`, the issuer and subject claims. Both are **required** with
  `.jwkPath` or `.jwkValue`, and not allowed with the other sources.
- `.aud`, the audience claim. Allowed with `.jwkPath`, `.jwkValue`, or `.provider`,
  and defaults to the host. The audience pins the credential to a specific
  registry, so it must match what the registry (or the cloud identity provider)
  expects.
- `.exp`, the signed JWT lifetime, as a
  [duration](https://pkg.go.dev/time#ParseDuration). Allowed only with `.jwkPath`
  or `.jwkValue` — the sources whose lifetime `flux-mirror` controls — and defaults
  to `60s`. A longer-lived token is cached and re-minted at half its lifetime.
  Every other source's lifetime is fixed by its issuer.
- `.hosts[].username`, controls how the resolved credential is transported, and therefore
  what the registry must accept — see
  [Bearer token vs. username/password](#bearer-token-vs-usernamepassword).

##### Token providers

When `.credential.provider` is set, the credential is obtained from the
provider's ambient identity for the audience (defaulting to the host). This is
what the registry on the other side has to accept:

- `github` and `forgejo` request an OIDC ID token from the CI Actions OIDC
  endpoint (`ACTIONS_ID_TOKEN_REQUEST_URL` / `ACTIONS_ID_TOKEN_REQUEST_TOKEN`).
  The registry must trust the GitHub/Forgejo Actions issuer and match the token's
  audience and subject (e.g. `repo:my-org/my-repo:ref:refs/heads/main`). The two
  mint tokens the same way today but are kept distinct so each platform can
  diverge.
- `gcp` obtains a Google ID token for the audience via Application Default
  Credentials (the GKE/GCE metadata server, a service account key, or workload
  identity federation). The registry must trust Google's OIDC issuer and the
  configured audience.
- `azure` obtains a Microsoft Entra ID access token via the default Azure
  credential chain (AKS/managed identity, workload identity federation,
  environment credentials). The audience is requested as the `<aud>/.default`
  scope, so `.aud` must be the application ID URI (or client ID) of a registered
  Entra application the registry validates tokens against.
- `aws` is not an OIDC token. AWS mints no JWT, so `flux-mirror` signs an
  `sts:GetCallerIdentity` request with the ambient role credentials (IRSA, EC2
  instance role, environment, ...) and wraps it in a JWT-shaped envelope whose
  header is `{"alg":"none","typ":"aws-sigv4-getcalleridentity"}`. The audience is
  carried as a signed `X-Audience` header that pins the target registry, not as
  an OIDC audience claim. The registry verifies the caller by replaying the
  signed request to AWS STS and reading the returned account/ARN, so the
  destination **must** understand this scheme — a generic OIDC registry will not.
- `jwt-svid` fetches a JWT-SVID for the audience from the SPIFFE Workload API
  (`SPIFFE_ENDPOINT_SOCKET`) and sends it as the credential. The registry must
  trust the SPIFFE trust domain's JWT bundle. This is the HTTP-layer counterpart
  to the transport-layer SPIFFE X.509-SVID mTLS configured under
  [`.tls`](#transport-tls); the two are independent.

##### Resolving file paths

`.fromPath`, `.jwkPath`, and the `.tls` path fields are resolved relative to the
config file's directory, and a config may only reference files under its own
directory tree. When the config is read from stdin (`-f -`), the process working
directory is used as the confinement root instead.

##### Bearer token vs. username/password

`.hosts[].username` controls how the resolved credential is
transported, and therefore what the registry on the other side must accept:

- **`username` unset (default)** — the credential is treated as a **bearer
  token**. `sync` sends it as an HTTP Bearer credential on every request, with no
  auth challenge, and `login`/`secret` write it to the Docker config's
  `registrytoken` field. This suits registries that natively validate an OIDC
  token (or another self-contained bearer credential). `registrytoken` is
  understood by [go-containerregistry](https://github.com/google/go-containerregistry)
  (crane, and Flux's `OCIRepository`/`HelmRepository`), but **not** by `kubelet`
  image pulls. Because credential helpers only store username/secret pairs, a
  `registrytoken` is always written to the config file, never to a keychain
  helper.
- **`username` set** — the credential becomes the **password** of a
  username/password pair. `sync` goes through the standard registry auth
  challenge (credentials are exchanged at the token endpoint, like the cloud
  providers), and `login`/`secret` write `username`/`password`/`auth`. Use this
  for registries such as Docker Hub, GHCR, and Quay, which expect a
  username/password login even when the username is a placeholder the registry
  ignores.

Choose `username` deliberately: set it when the registry expects a
username/password login (or the credential will be consumed by `kubelet`), and
leave it unset when the registry validates a self-contained bearer token.

#### Cloud registry providers

`.hosts[].provider` authenticates the host with a cloud registry provider's
ambient workload identity — the same mechanism the `flux push artifact` family
uses — and obtains the registry's native credentials directly (no
`credential`/JWT involved). It is mutually exclusive with `credential`. The
supported values are:

- `ecr`, for Amazon ECR. It uses the AWS credential chain (IRSA, EC2 instance
  role, environment, SSO), reading the region from the host.
- `acr`, for Azure ACR. It uses the default Azure credential chain (managed
  identity, workload identity, environment, ...).
- `gar`, for Google GAR. It uses Google Application Default Credentials.

The resolved credentials are presented to the registry as a username/password
pair through the standard registry auth challenge, and written as
`username`/`password`/`auth` by [`login`](../guides/login.md) and
[`secret`](../guides/secret.md).

#### Transport TLS

`.hosts[].tls` configures transport-layer TLS for the host's registry requests,
separately from the HTTP-layer `credential`. It is applied by `sync` (which
connects to the registry); `login` and `secret` do not open registry connections
and ignore it. It is not allowed on a `provider` host. The field has two
independent halves, of which at least one must be set:

- `.serverAuth`, how the registry's server certificate is verified. Set exactly
  one of `.fromPath` / `.value` to supply a custom CA bundle
  (one or more concatenated PEM certificates), or `.spiffe` to verify the
  server's X.509-SVID against the SPIFFE trust bundle. When `.serverAuth` is
  omitted entirely, the system trust pool is used.
- `.clientAuth`, the client certificate for mTLS. Set exactly one of `.provider:
  x509-svid` (present a SPIFFE X.509-SVID from the Workload API) or the static
  `.certificate` + `.key` pair. The `.certificate` is one of
  `.fromPath`/`.value`; the `.key` is one of `.fromPath`/`.value`.

Each half chooses SPIFFE or non-SPIFFE independently, so SPIFFE can authenticate
the client while a normal or custom CA verifies the server, or vice versa. Under
`.serverAuth.spiffe`, set exactly one of `.serverID` (authorize one exact SPIFFE
ID), `.trustDomain` (authorize any SVID in that trust domain; the value `self`
means the client's own trust domain, read from its X.509-SVID), or
`.authorizeAny: true` (accept any SVID the bundle can validate — discouraged).
With SPIFFE on either side, the client SVID and trust bundle come from the
ambient Workload API socket (`SPIFFE_ENDPOINT_SOCKET`) and rotate automatically.

```yaml
hosts:
  # Custom CA for server verification and a static client cert for mTLS.
  - host: registry.example.com
    tls:
      serverAuth:
        fromPath: ./certs/ca.crt
      clientAuth:
        certificate:
          fromPath: ./certs/client.crt
        key:
          fromPath: ./certs/client.key

  # Full SPIFFE: X.509-SVID client cert and SPIFFE server verification.
  - host: spiffe.example.com
    tls:
      clientAuth:
        provider: x509-svid
      serverAuth:
        spiffe:
          serverID: spiffe://example.org/registry
          # trustDomain: example.org   # or any SVID in this trust domain
          # trustDomain: self          # or any SVID in our own trust domain
          # authorizeAny: true         # or any SVID at all (discouraged)

  # Client-only SPIFFE: SPIFFE client cert, server verified by a public/custom CA.
  - host: public.example.com
    tls:
      clientAuth:
        provider: x509-svid
      # serverAuth omitted → system trust pool verifies the server.
```

#### Blob upload chunking

`.hosts[].maxChunkSize` is the maximum size, in KiB (1024 bytes), of a
blob-upload `PATCH` request to this host; larger blobs are split into chunked
uploads. It defaults to `0`, which disables chunking and sends one monolithic
`PATCH` per blob. Set it for registries or proxies that cap request body sizes.

## Working with Config

### Overwrite and drift behavior

`.artifacts[].overwrite` and `.charts[].overwrite` are optional fields that
control what happens when a tag or version already exists at the destination.
`flux-mirror` does not overwrite by default, which keeps the safe path the
default on immutable registries (ECR with `IMMUTABLE` tag mutability, GAR with
tag immutability, Harbor with retention rules) and avoids redundant writes on
mutable ones.

When a tag already exists at the destination, the source and destination digests
are compared:

- **Same digest** — skipped silently.
- **Different digest, `overwrite: false`** — a warning is logged and the tag is
  left alone, reported as `drifted`. The mirror has diverged from source but is
  not brought back in sync without explicit consent.
- **Different digest, `overwrite: true`** — the new digest is pushed, replacing
  the destination tag. This fails if the destination registry enforces tag
  immutability.

The drift warning is useful even on immutable registries where the divergence
cannot be resolved automatically: it surfaces in the run output and the
[exit code](../guides/sync.md#exit-codes), which audit and alerting can hook
into. The [`--overwrite`](../guides/sync.md#flags) CLI flag forces `overwrite:
true` for every entry, overriding per-entry values, for one-off resyncs.

With `includeReferrers: true`, the same rule applies to referrers. Referrers
missing at the destination are always mirrored (there is nothing to overwrite) —
the common case when an upstream signature is published after the artifact was
first mirrored. Referrers that exist with a different digest are skipped
(default) or replaced (`overwrite: true`).

### Defaults

| Field                            | Default  |
|----------------------------------|----------|
| `artifacts[].selector.sortBy`    | `semver` |
| `artifacts[].selector.limit`     | `1`      |
| `artifacts[].overwrite`          | `false`  |
| `artifacts[].includeReferrers`   | `false`  |
| `artifacts[].verify`             | unset    |
| `charts[].version`               | `*`      |
| `charts[].limit`                 | `1`      |
| `charts[].overwrite`             | `false`  |
| `hosts[].credential.aud`         | `host`   |
| `hosts[].credential.exp`         | `60s`    |
| `hosts[].maxChunkSize`           | `0`      |

A `limit` of `0` disables the cap and mirrors every matching tag or version. Use
it with care: unrestricted mirrors can consume significant bandwidth and storage
and may trip rate limits on upstream registries.
