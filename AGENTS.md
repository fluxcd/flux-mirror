# AGENTS.md

Guidance for AI coding assistants working in `fluxcd/flux-mirror`. Read this file before making changes.

## Contribution workflow for AI agents

These rules come from [`fluxcd/flux2/CONTRIBUTING.md`](https://github.com/fluxcd/flux2/blob/main/CONTRIBUTING.md) and apply to every Flux repository.

- **Do not add `Signed-off-by` or `Co-authored-by` trailers with your agent name.** Only a human can legally certify the DCO.
- **Disclose AI assistance** with an `Assisted-by` trailer naming your agent and model:
  ```sh
  git commit -s -m "Add feature X" --trailer "Assisted-by: <agent-name>/<model-id>"
  ```
  The `-s` flag adds the human's `Signed-off-by` from their git config - do not remove it.
- **Commit message format:** Subject in imperative mood ("Add feature X" instead of "Adding feature X"), capitalized, no trailing period, <=50 characters.
- **Commit body:** Add a succinct explanation explaining what and why, wrap at 72 characters.
- **Trim verbiage:** in PR descriptions, commit messages, and code comments. No marketing prose, no restating the diff, no emojis.
- **Rebase, don't merge:** Never merge `main` into the feature branch; rebase onto the latest `main` and push with `--force-with-lease`. Squash before merge when asked.
- **Tests:** New features, improvements and fixes must have test coverage.

## Project

`flux-mirror` is a Flux CLI plugin for declaratively mirroring Helm charts, OCI artifacts, and container images between registries. It is a single Go binary, cobra-based.

Read the [README](README.md) for an overview of the project and its features.

### Code Structure

- `cmd/flux-mirror/` - the `main` package. One file per cobra command or command concern (`sync.go`, `version.go`, `completion.go`, `progress.go`, `logging.go`). `main.VERSION` is overridden at build time by the Makefile.
- `cmd/flux-mirror/main_test.go` - hosts `TestMain`, shared `executeCommand(...)` helpers, and `resetCmdArgs()`, which restores global cobra flag state between tests. New commands or flags must update this reset path so tests do not leak state across subtests.
- `internal/config/` - YAML config model and validation for `apiVersion: mirror.fluxcd.io/v1alpha1`, `kind: Config`, `artifacts`, `charts`, selector defaults, overwrite behavior, and cosign verification options.
- `internal/selector/` - tag selection pipeline for OCI artifacts: regex prefilter, semver filter, sort strategy (`semver`, `alphabetical`, `numerical`), then top-N limit.
- `internal/sync/` - sync runner, retry/timeout behavior, per-entry execution, summaries, outcomes, and exit-code aggregation.
- `internal/artifacts/` - OCI artifact mirroring from source repository tags to destination repository tags, including drift handling, dry-run outcomes, referrers, verification, and concurrency fan-out.
- `internal/charts/` - Helm chart mirroring to OCI destinations, including version selection, deterministic Helm-OCI publication, drift handling, and dry-run outcomes.
- `internal/oci/` - OCI client wrapper, auth/keychain setup, transport customization, digest checks, blob copy, Helm artifact helpers, referrers, and cosign verification.
- `internal/helmrepo/` - HTTP/S and OCI Helm repository access, index/chart resolution, and ambient Helm credential handling.
- `internal/flags/` - reusable `pflag.Value` implementations for CLI flags with constrained values, such as output format.
- `internal/testregistry/` - test helpers for registry-backed mirror tests.
- `actions/setup/` - composite GitHub Action for installing the CLI on CI runners.

### Build, Test, and Lint

All development goes through the Makefile - do not invoke `go build` directly, because the Makefile stamps `main.VERSION` via `-ldflags` and runs `tidy`/`fmt`/`vet` as prerequisites.

- `make build` - build `./bin/flux-mirror` with VERSION stamped from git
- `make test` - runs `tidy`, `fmt`, `vet`, then `go test ./... -coverprofile cover.out`
  - Single test pattern: `make test GO_TEST_ARGS="-run TestVersionCmd"`
- `make lint` - runs golangci-lint with revive, staticcheck, goimports, errcheck, misspell, and related checks
- `make run GO_RUN_ARGS="version -o json"` - build then run the CLI with args
- `make docker-build` - build the container image
- `make registry-up` / `make registry-down` - start/stop the local `registry:3` instance used by smoke and integration-style testing

CI (`.github/workflows/test.yaml`) runs `make test` and `make lint` and fails if generated formatting or module files leave the working tree dirty, so always run `make test` before committing.

### Code Conventions

- File header: every `.go` file must start with the two-line Apache-2.0 header - enforced by golangci-lint's `revive.file-header` rule.
- Struct tags: only `json` and `inline` are permitted on struct fields (revive `struct-tag` rule).
- Flag wiring: for flags with a fixed set of accepted values, add a type under `internal/flags/` and register it with `cmd.Flags().VarP(&args.x, "name", "n", args.x.Description())` rather than a plain string flag. This gives validation, an `a|b|c` type hint in `--help`, and consistent error messages.
- Cobra commands keep flag state in package-level `*Args` variables. When adding a command or flag, update `resetCmdArgs()` in `cmd/flux-mirror/main_test.go`.
- Command output should go through cobra command streams (`cmd.Printf`, `cmd.PrintErrf`, `cmd.OutOrStdout()`, `cmd.ErrOrStderr()`) rather than writing to process-wide stdout/stderr directly. Tests rely on command-local streams.
- Error handling should wrap context with `%w` and preserve the distinct sync exit-code behavior (`0` clean, `1` failed jobs, `2` drift-only by default).
- Config validation lives in `internal/config`; do not defer schema or semantic validation to mirror execution if it can be rejected before network operations.
- Tests use Gomega (`. "github.com/onsi/gomega"` dot-import is accepted - staticcheck ST1001 is disabled project-wide). Table-driven tests are the norm.

## Writing Documentation

User-facing changes (flags, commands, config fields, report shape, GitHub Action inputs) must be reflected in the docs. The tree is:

- `README.md` - features list, install, quickstart, CI usage, GitHub Action example, and doc links.
- `docs/config.md` - YAML config specification, defaults, selectors, overwrite behavior, referrers, and signature verification.
- `docs/sync.md` - `sync` command reference, config source resolution, authentication, flags, output, outcomes, exit codes, and dry-run behavior.
- `actions/setup/README.md` + `actions/setup/action.yaml` - GitHub Action inputs and example workflows.
- `examples/` - runnable config examples that should stay aligned with supported config fields and defaults.

Apply these rules:

- New or changed CLI flag: update the flag table in `docs/sync.md` and any relevant examples in `README.md`.
- New or changed config field: update `internal/config` tests, `docs/config.md`, and examples when appropriate.
- New command: add or update README command guidance and create a dedicated reference doc under `docs/` if the command has meaningful flags or output.
- Output or outcome shape change: update `docs/sync.md`, README examples, and tests that assert text/YAML/JSON output.
- GitHub Action change: when `actions/setup/action.yaml` inputs or behavior change, refresh `actions/setup/README.md`.
- Registry auth, Helm auth, OIDC/JWT, or cosign verification behavior changes must be documented in the authentication or verification sections, not only in flag help.
