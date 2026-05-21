// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	craneLogs "github.com/google/go-containerregistry/pkg/logs"
	"github.com/spf13/cobra"

	"github.com/fluxcd/pkg/auth/utils/cioidc"

	"github.com/fluxcd/flux-mirror/internal/artifacts"
	"github.com/fluxcd/flux-mirror/internal/charts"
	"github.com/fluxcd/flux-mirror/internal/config"
	"github.com/fluxcd/flux-mirror/internal/flags"
	"github.com/fluxcd/flux-mirror/internal/oci"
	"github.com/fluxcd/flux-mirror/internal/sync"
)

const (
	envConfig                  = "FLUX_MIRROR_CONFIG"
	syncDefaultTimeout         = 5 * time.Minute
	syncDefaultDriftExitCode   = 2
	syncMaxCustomDriftExitCode = 255

	// defaultJWTTokenEnvVar is the environment variable a --jwt-token value
	// reads its token from when its token part is empty (i.e. "@<host>").
	defaultJWTTokenEnvVar = "FLUX_MIRROR_SYNC_JWT_TOKEN"
)

// Supported --jwt-provider values. GitHub and Forgejo currently mint tokens the
// same way, but they are kept as distinct flag values so the CLI can adapt to
// each platform's breaking changes without changing its surface.
const (
	jwtProviderGitHub  = "github"
	jwtProviderForgejo = "forgejo"
)

var syncCmd = &cobra.Command{
	Use:   "sync [CONFIG|-]",
	Short: "Mirror Helm charts and OCI artifacts to a destination registry",
	Long: `Mirror Helm charts and OCI artifacts between registries based on a
declarative YAML config (apiVersion: mirror.fluxcd.io/v1alpha1, kind: Config).
OCI registry auth is read from the ambient Docker config (~/.docker/config.json,
$DOCKER_CONFIG, and configured credential helpers). Helm HTTP/S repository auth
is read from Helm's default repositories.yaml path, or $HELM_REPOSITORY_CONFIG
when set.

Exit codes:
  0  clean run (everything copied/skipped as expected)
  1  at least one tag job failed
  2  no failures, but at least one tag drifted (overwrite=false)

Use --drift-exit-code=0 to keep drift-only runs green in CI when the
destination registry is known to be immutable.`,
	Example: `  # Run a sync against a config file
  flux-mirror sync flux-mirror.yaml

  # Pass the config via env var
  FLUX_MIRROR_CONFIG=flux-mirror.yaml flux-mirror sync

  # Pass the config via stdin
  flux-mirror sync - < flux-mirror.yaml

  # Preview without writing to the destination
  flux-mirror sync flux-mirror.yaml --dry-run -o yaml

  # Force overwrite of every drifted tag
  flux-mirror sync flux-mirror.yaml --overwrite`,
	Args: cobra.MaximumNArgs(1),
	RunE: syncCmdRun,
}

type syncFlags struct {
	output        flags.Output
	concurrency   int
	retries       int
	overwrite     bool
	driftExitCode int
	dryRun        bool
	verbose       bool
	insecure      bool
	noProgress    bool
	maxChunkSize  int
	jwtProvider   string
	jwtTokens     []string
	jwtAudiences  []string
}

var syncArgs = syncFlags{
	output:        "text",
	concurrency:   4,
	retries:       3,
	driftExitCode: syncDefaultDriftExitCode,
}

func init() {
	syncCmd.Flags().VarP(&syncArgs.output, "output", "o", syncArgs.output.Description())
	syncCmd.Flags().IntVar(&syncArgs.concurrency, "concurrency", syncArgs.concurrency,
		"Maximum number of copy operations to run in parallel per job")
	syncCmd.Flags().IntVar(&syncArgs.retries, "retries", syncArgs.retries,
		"Maximum number of retry attempts per job within timeout budget.")
	syncCmd.Flags().BoolVar(&syncArgs.overwrite, "overwrite", false,
		"Force overwrite when the destination artifact digest has drifted")
	syncCmd.Flags().IntVar(&syncArgs.driftExitCode, "drift-exit-code", syncArgs.driftExitCode,
		"Exit code to use when drift is detected without failures")
	syncCmd.Flags().BoolVar(&syncArgs.dryRun, "dry-run", false,
		"Run the plan and comparison pipeline without performing any writes.")
	syncCmd.Flags().BoolVar(&syncArgs.verbose, "verbose", false,
		"Log all operations and the involved digests as they are performed.")
	syncCmd.Flags().BoolVar(&syncArgs.insecure, "insecure", false,
		"Allow plaintext HTTP and skip TLS verification (test/dev only).")
	syncCmd.Flags().BoolVar(&syncArgs.noProgress, "no-progress", false,
		"Disable the live progress spinner.")
	syncCmd.Flags().IntVar(&syncArgs.maxChunkSize, "oci-max-chunk-size", 0,
		"Maximum size in KiB (1024 bytes) for an OCI blob upload PATCH; "+
			"larger blobs are split into chunked PATCH uploads. 0 disables "+
			"chunking (single monolithic PATCH per blob).")
	syncCmd.Flags().StringVar(&syncArgs.jwtProvider, "jwt-provider", "",
		fmt.Sprintf("OIDC provider that mints the tokens for --jwt-audience, one "+
			"of: %s, %s. Required when --jwt-audience is set.",
			jwtProviderGitHub, jwtProviderForgejo))
	syncCmd.Flags().StringArrayVar(&syncArgs.jwtTokens, "jwt-token", nil,
		fmt.Sprintf("Static JWT to send to a registry, in the format "+
			"<token>@<host>, used as-is (e.g. a GitLab CI id_token). When the "+
			"token part is empty (@<host>), it is read from the %s environment "+
			"variable, keeping the token off the command line. Requests to other "+
			"hosts pass through with their keychain auth intact. Repeatable.",
			defaultJWTTokenEnvVar))
	syncCmd.Flags().StringArrayVar(&syncArgs.jwtAudiences, "jwt-audience", nil,
		"Audience of an OIDC ID token minted from the GitHub/Forgejo Actions "+
			"endpoint (ACTIONS_ID_TOKEN_REQUEST_URL and "+
			"ACTIONS_ID_TOKEN_REQUEST_TOKEN) and sent to a registry, in the "+
			"format <audience>[@<host>]; the host defaults to the audience. The "+
			"token is cached for the first 50% of its lifetime and reminted on "+
			"demand. Repeatable.")

	rootCmd.AddCommand(syncCmd)
}

func syncCmdRun(cmd *cobra.Command, args []string) error {
	if syncArgs.driftExitCode < 0 || syncArgs.driftExitCode > syncMaxCustomDriftExitCode {
		return fmt.Errorf("--drift-exit-code must be between 0 and %d", syncMaxCustomDriftExitCode)
	}

	cfgPath, err := resolveConfigPath(args)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(cmd, cfgPath)
	if err != nil {
		return err
	}

	// Bump per-job timeout to 5m if the user didn't pass --timeout explicitly.
	// The root flag's declared default stays at 1m so other commands aren't
	// affected.
	if !cmd.Flags().Changed("timeout") {
		rootArgs.timeout = syncDefaultTimeout
	}

	// Verbose enables our own structured logs (sync started, entry started,
	// mirroring tag, tag done, entry summary, sync complete) AND wires
	// crane's package-global Progress / Warn loggers to stderr in the same
	// log.LstdFlags shape — that's where the per-blob digest lines, fallback
	// tag updates, and registry rejections come from. Without verbose, both
	// streams are silent and only the spinner + per-job lines show.
	var logger *slog.Logger
	if syncArgs.verbose {
		logger = slog.New(newPlainHandler(cmd.ErrOrStderr(), slog.LevelDebug))
		craneLogs.Progress = log.New(cmd.ErrOrStderr(), "", log.LstdFlags)
		craneLogs.Warn = log.New(cmd.ErrOrStderr(), "", log.LstdFlags)
	} else {
		logger = slog.New(slog.DiscardHandler)
	}

	var clientOpts []oci.ClientOption
	if syncArgs.insecure {
		clientOpts = append(clientOpts, oci.Insecure())
	}
	if t, err := buildClientTransport(); err != nil {
		return err
	} else if t != nil {
		clientOpts = append(clientOpts, oci.WithTransport(t))
	}
	client := oci.NewClient(clientOpts...)

	runner := &sync.Runner{
		Concurrency:   syncArgs.concurrency,
		Retries:       syncArgs.retries,
		PerJobTimeout: rootArgs.timeout,
		Logger:        logger,
	}

	mirrors := make([]sync.EntryMirror, 0, len(cfg.Artifacts)+len(cfg.Charts))
	for _, e := range cfg.Artifacts {
		mirrors = append(mirrors, artifacts.New(client, e, artifacts.Options{
			Overwrite: syncArgs.overwrite,
			DryRun:    syncArgs.dryRun,
			Verbose:   syncArgs.verbose,
			CopyJobs:  syncArgs.concurrency,
			Logger:    logger,
		}))
	}
	for _, e := range cfg.Charts {
		m, err := charts.New(client, e, charts.Options{
			Overwrite: syncArgs.overwrite,
			DryRun:    syncArgs.dryRun,
			Verbose:   syncArgs.verbose,
			Logger:    logger,
		})
		if err != nil {
			return fmt.Errorf("build chart entry %s/%s: %w", e.Source, e.Name, err)
		}
		mirrors = append(mirrors, m)
	}

	// Pretty-print mode = text output AND no --verbose. In that case we
	// drive a spinner on stderr and print one completion line per tag on
	// stdout, followed by a one-line totals summary. The spinner is stopped
	// before the summary so it can't overwrite it. Verbose mode skips the
	// spinner entirely — the structured log stream already conveys progress,
	// and a spinner would compete with it.
	isText := syncArgs.output.String() == "text" || syncArgs.output.String() == ""
	prettyPrint := isText && !syncArgs.verbose

	var prog *progress
	if prettyPrint {
		var spinnerOut io.Writer
		if !syncArgs.noProgress {
			spinnerOut = cmd.ErrOrStderr()
		}
		prog = newProgress(cmd.OutOrStdout(), spinnerOut, len(mirrors))
		runner.OnJobFinished = prog.JobFinished
		runner.OnEntryFinished = prog.EntryFinished
		runner.OnPlanError = prog.PlanFailed
	}

	res, err := runner.Run(cmd.Context(), mirrors)
	if prog != nil {
		prog.Close()
	}
	if err != nil {
		return err
	}
	switch syncArgs.output.String() {
	case "text", "":
		res.LogSummary(logger)
		if prettyPrint {
			if err := res.PrettyPrint(cmd.OutOrStdout()); err != nil {
				return err
			}
		}
	default:
		if err := res.Render(cmd.OutOrStdout(), syncArgs.output.String()); err != nil {
			return err
		}
	}
	return classifyExit(res, syncArgs.driftExitCode)
}

// syncExitError is returned to make `main` exit with a code other than 1
// without printing the default error decoration.
type syncExitError struct {
	code int
	msg  string
}

func (e *syncExitError) Error() string { return e.msg }
func (e *syncExitError) ExitCode() int { return e.code }

func classifyExit(r sync.Result, driftExitCode int) error {
	// Non-zero exit codes carry an empty message — failures and drift are
	// already surfaced inline (per-failure ✗ lines) and in the Summary
	// totals; printing a third "N failed" footer just adds noise.
	code := r.ExitCode()
	if code == syncDefaultDriftExitCode && r.HasDrift() && !r.HasFailures() {
		code = driftExitCode
	}
	switch code {
	case 0:
		return nil
	default:
		return &syncExitError{code: code, msg: ""}
	}
}

// buildClientTransport composes the optional transport stack from
// --oci-max-chunk-size and the --jwt-* flags. Returns (nil, nil)
// if neither feature is requested. Stack order, outer first:
//
//	ChunkingTransport (split big PATCH bodies)
//	  → cioidc.Transport (stamp Authorization on each chunk request)
//	    → http.DefaultTransport
//
// JWT is the inner wrapper so that mid-upload token refresh works
// per chunk; chunking is the outer wrapper so split chunks each get
// freshly stamped auth from the JWT layer.
func buildClientTransport() (http.RoundTripper, error) {
	var (
		jwtSet = syncArgs.jwtProvider != "" ||
			len(syncArgs.jwtTokens) > 0 || len(syncArgs.jwtAudiences) > 0
		chunkSet = syncArgs.maxChunkSize > 0
	)
	if !jwtSet && !chunkSet {
		return nil, nil
	}
	var t http.RoundTripper
	t = http.DefaultTransport
	if jwtSet {
		opts, err := jwtTransportOptions(t)
		if err != nil {
			return nil, err
		}
		jwt, err := cioidc.NewTransport(opts...)
		if err != nil {
			return nil, err
		}
		t = jwt
	}
	if chunkSet {
		t = &oci.ChunkingTransport{
			Inner:     t,
			ChunkSize: int64(syncArgs.maxChunkSize) * 1024,
		}
	}
	return t, nil
}

// jwtTransportOptions parses the --jwt-provider, --jwt-token and --jwt-audience
// flags into cioidc options. --jwt-token is <token>@<host>; --jwt-audience is
// <audience>[@<host>], where the host defaults to the audience. --jwt-provider
// is required when any --jwt-audience is set.
func jwtTransportOptions(inner http.RoundTripper) ([]cioidc.Option, error) {
	opts := []cioidc.Option{cioidc.WithInner(inner)}

	for _, v := range syncArgs.jwtTokens {
		token, host, ok := cutLast(v, "@")
		if !ok || host == "" {
			return nil, fmt.Errorf("invalid --jwt-token %q, must be in the format <token>@<host>", v)
		}
		if token == "" {
			token = os.Getenv(defaultJWTTokenEnvVar)
			if token == "" {
				return nil, fmt.Errorf("--jwt-token %q has an empty token but the %s environment variable is not set", v, defaultJWTTokenEnvVar)
			}
		}
		opts = append(opts, cioidc.WithHostToken(host, token))
	}

	if syncArgs.jwtProvider != "" && len(syncArgs.jwtAudiences) == 0 {
		return nil, fmt.Errorf("--jwt-audience is required when --jwt-provider is set")
	}
	if len(syncArgs.jwtAudiences) > 0 {
		switch syncArgs.jwtProvider {
		case jwtProviderGitHub, jwtProviderForgejo:
		case "":
			return nil, fmt.Errorf("--jwt-provider is required when --jwt-audience is set")
		default:
			return nil, fmt.Errorf("invalid --jwt-provider %q, must be one of: %s, %s",
				syncArgs.jwtProvider, jwtProviderGitHub, jwtProviderForgejo)
		}
	}

	for _, v := range syncArgs.jwtAudiences {
		audience, host, ok := cutLast(v, "@")
		if audience == "" {
			return nil, fmt.Errorf("invalid --jwt-audience %q, must be in the format <audience>[@<host>]", v)
		}
		if !ok {
			host = audience
		}
		if host == "" {
			return nil, fmt.Errorf("invalid --jwt-audience %q, the host after '@' must not be empty", v)
		}
		opts = append(opts, cioidc.WithHostAudience(host, audience))
	}

	return opts, nil
}

// cutLast slices s around the last instance of sep, like strings.Cut but from
// the end, so the host (which contains no '@') is split off correctly even when
// the token or audience itself contains '@'.
func cutLast(s, sep string) (before, after string, found bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}

func resolveConfigPath(args []string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	if env := os.Getenv(envConfig); env != "" {
		return env, nil
	}
	return "", fmt.Errorf("config required: pass the config path as the first argument, pass '-' for stdin, or set %s", envConfig)
}

func loadConfig(cmd *cobra.Command, path string) (*config.Config, error) {
	if path != "-" {
		return config.Load(path)
	}
	cfg, err := config.Decode(cmd.InOrStdin())
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}
