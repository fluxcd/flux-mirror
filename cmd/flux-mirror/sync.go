// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	craneLogs "github.com/google/go-containerregistry/pkg/logs"
	"github.com/spf13/cobra"

	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta1"
	"github.com/fluxcd/flux-mirror/internal/artifacts"
	"github.com/fluxcd/flux-mirror/internal/charts"
	"github.com/fluxcd/flux-mirror/internal/config"
	"github.com/fluxcd/flux-mirror/internal/flags"
	"github.com/fluxcd/flux-mirror/internal/oci"
	"github.com/fluxcd/flux-mirror/internal/registryauth"
	"github.com/fluxcd/flux-mirror/internal/sync"
)

const (
	envConfig                  = "FLUX_MIRROR_CONFIG"
	syncDefaultTimeout         = 5 * time.Minute
	syncDefaultDriftExitCode   = 2
	syncMaxCustomDriftExitCode = 255
)

var syncCmd = &cobra.Command{
	Use:   "sync [CONFIG|-]",
	Short: "Mirror Helm charts and OCI artifacts to a destination registry",
	Long: `Mirror Helm charts and OCI artifacts between registries based on a
declarative YAML config (apiVersion: mirror.fluxcd.io/v1beta1, kind: Config).
OCI registry auth is read from the ambient Docker config (~/.docker/config.json,
$DOCKER_CONFIG, and configured credential helpers), or, for hosts listed in the
config's 'hosts' section, from a per-host JWT. Helm HTTP/S repository auth is read
from Helm's default repositories.yaml path, or $HELM_REPOSITORY_CONFIG when set.

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
	timeout       time.Duration
	retries       int
	overwrite     bool
	driftExitCode int
	dryRun        bool
	verbose       bool
	insecure      bool
	noProgress    bool
	maxChunkSize  int
}

var syncArgs = syncFlags{
	output:        "text",
	concurrency:   4,
	timeout:       syncDefaultTimeout,
	retries:       3,
	driftExitCode: syncDefaultDriftExitCode,
}

func init() {
	syncCmd.Flags().VarP(&syncArgs.output, "output", "o", syncArgs.output.Description())
	syncCmd.Flags().IntVar(&syncArgs.concurrency, "concurrency", syncArgs.concurrency,
		"Maximum number of copy operations to run in parallel per job")
	syncCmd.Flags().DurationVar(&syncArgs.timeout, "timeout", syncArgs.timeout,
		"The length of time to wait before giving up on the current operation.")
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
	cfg, err := loadConfig(cmd, cfgPath, true)
	if err != nil {
		return err
	}

	timeout := resolveSyncTimeout(cmd)

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
	t, closeTransport, err := buildClientTransport(cmd.Context(), cfg.Hosts)
	if err != nil {
		return err
	}
	defer closeTransport()
	if t != nil {
		clientOpts = append(clientOpts, oci.WithTransport(t))
	}
	if kc, err := registryauth.BuildKeychain(cmd.Context(), cfg.Hosts); err != nil {
		return err
	} else if kc != nil {
		clientOpts = append(clientOpts, oci.WithKeychain(kc))
	}
	client := oci.NewClient(clientOpts...)

	runner := &sync.Runner{
		Concurrency:   syncArgs.concurrency,
		Retries:       syncArgs.retries,
		PerJobTimeout: timeout,
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
		report := sync.NewReport("flux-mirror/"+VERSION, time.Now(), res)
		if err := sync.RenderReport(cmd.OutOrStdout(), syncArgs.output.String(), report); err != nil {
			return err
		}
	}
	return classifyExit(res, syncArgs.driftExitCode)
}

func resolveSyncTimeout(cmd *cobra.Command) time.Duration {
	timeout := syncArgs.timeout
	if !cmd.Flags().Changed("timeout") {
		if f := rootCmd.PersistentFlags().Lookup("timeout"); f != nil && f.Changed {
			timeout = rootArgs.timeout
		}
	}
	return timeout
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

// buildClientTransport composes the optional transport stack from the config's
// hosts and --oci-max-chunk-size. Returns (nil, noop, nil) if none is requested.
// Stack order, outer first:
//
//	ChunkingTransport (split big PATCH bodies)
//	  → cijwt.Transport (stamp Authorization on each chunk request)
//	    → tls dispatch (per-host TLS/mTLS) | http.DefaultTransport
//
// TLS is the inner-most layer (the transport handshake); JWT sits above it so
// per-request token minting works per chunk; chunking is the outer wrapper so
// split chunks each get freshly stamped auth from the JWT layer. The returned
// closer releases any SPIFFE Workload API sources and must be called when done.
func buildClientTransport(ctx context.Context, hosts []apiv1.AuthHost) (http.RoundTripper, func() error, error) {
	noop := func() error { return nil }

	// Only credential hosts use the cijwt transport; provider hosts authenticate
	// through the keychain instead.
	credSet := registryauth.NeedsCredentialTransport(hosts)
	chunkSet := syncArgs.maxChunkSize > 0
	tlsSet := registryauth.NeedsTLS(hosts)
	if !credSet && !chunkSet && !tlsSet {
		return nil, noop, nil
	}

	// Inner-most: per-host TLS dispatch, or the default transport.
	base, closeTLS, err := registryauth.NewTLSTransport(ctx, http.DefaultTransport, hosts)
	if err != nil {
		return nil, noop, err
	}
	t := base
	if credSet {
		ct, err := registryauth.NewCredentialTransport(t, hosts)
		if err != nil {
			_ = closeTLS()
			return nil, noop, err
		}
		t = ct
	}
	if chunkSet {
		t = &oci.ChunkingTransport{
			Inner:     t,
			ChunkSize: int64(syncArgs.maxChunkSize) * 1024,
		}
	}
	return t, closeTLS, nil
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

// loadConfig decodes the config from path ('-' reads stdin) and validates it.
// requireEntries enforces that at least one charts/artifacts entry is present;
// pass false for commands that consume only the hosts section (login). File-path
// fields are resolved (via SecureJoin) within the config file's directory, or the
// process working directory when the config is read from stdin.
func loadConfig(cmd *cobra.Command, path string, requireEntries bool) (*apiv1.Config, error) {
	cfg, err := decodeConfig(cmd, path)
	if err != nil {
		return nil, err
	}
	if requireEntries {
		err = config.Validate(cfg)
	} else {
		err = config.ValidateNoEntriesOK(cfg)
	}
	if err != nil {
		return nil, err
	}
	baseDir, err := configBaseDir(path)
	if err != nil {
		return nil, err
	}
	if err := config.ResolvePaths(cfg, baseDir); err != nil {
		return nil, err
	}
	return cfg, nil
}

// configBaseDir returns the directory that file-path fields are resolved within:
// the config file's directory, or the process working directory for stdin ('-').
func configBaseDir(path string) (string, error) {
	if path == "-" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working directory: %w", err)
		}
		return wd, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve config path: %w", err)
	}
	return filepath.Dir(abs), nil
}

func decodeConfig(cmd *cobra.Command, path string) (*apiv1.Config, error) {
	if path == "-" {
		return config.Decode(cmd.InOrStdin())
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	return config.Decode(f)
}
