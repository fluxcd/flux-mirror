# Setup Flux Mirror CLI GitHub Action

This GitHub Action can be used to install the Flux Mirror CLI on GitHub
runners for usage in workflows. All GitHub runners are supported, including
Ubuntu, Windows, and macOS.

## Usage

Example workflow that runs `flux-mirror sync` on a schedule against a
config checked into the repository:

```yaml
name: flux-mirror

on:
  schedule:
    - cron: "0 */6 * * *"
  workflow_dispatch:

jobs:
  sync:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      packages: write
    steps:
      - name: Checkout
        uses: actions/checkout@v6
      - name: Setup Flux Mirror CLI
        uses: fluxcd/flux-mirror/actions/setup@main
      - name: Login to GHCR
        uses: docker/login-action@v4
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - name: Sync
        run: flux-mirror sync -c .flux-mirror.yaml --no-progress
```

`flux-mirror sync` exits with `1` on a pull/push failure and `2` when drift is detected.
Use `--overwrite` if mirroring mutable tags such as `latest`.

## Action Inputs

| Name      | Description                      | Default                   |
|-----------|----------------------------------|---------------------------|
| `version` | Flux Mirror version              | The latest stable release |
| `bindir`  | Alternative location for the CLI | `$RUNNER_TOOL_CACHE`      |

## Action Outputs

| Name      | Description                                                |
|-----------|------------------------------------------------------------|
| `version` | The Flux Mirror CLI version that was effectively installed |
