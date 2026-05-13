// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/fluxcd/flux-mirror/internal/artifacts"
	"github.com/fluxcd/flux-mirror/internal/config"
	"github.com/fluxcd/flux-mirror/internal/flags"
	"github.com/fluxcd/flux-mirror/internal/oci"
	"github.com/fluxcd/flux-mirror/internal/sync"
)

const (
	envConfig          = "FLUX_MIRROR_CONFIG"
	syncDefaultTimeout = 5 * time.Minute
)

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Mirror Helm charts and OCI artifacts to a destination registry",
	Long: `Mirror Helm charts and OCI artifacts between registries based on a
declarative YAML config (apiVersion: mirror.fluxcd.io/v1alpha1, kind: Config).
Auth is read from the ambient Docker config (~/.docker/config.json,
$DOCKER_CONFIG, and configured credential helpers).

Exit codes:
  0  clean run (everything copied/skipped as expected)
  1  at least one tag job failed
  2  no failures, but at least one tag drifted (overwrite=false)`,
	Example: `  # Run a sync against a config file
  flux-mirror sync -c flux-mirror.yaml

  # Pass the config via env var
  FLUX_MIRROR_CONFIG=flux-mirror.yaml flux-mirror sync

  # Preview without writing to the destination
  flux-mirror sync -c flux-mirror.yaml --dry-run -o yaml

  # Force overwrite of every drifted tag
  flux-mirror sync -c flux-mirror.yaml --overwrite`,
	Args: cobra.NoArgs,
	RunE: syncCmdRun,
}

type syncFlags struct {
	config      string
	output      flags.Output
	concurrency int
	retries     int
	overwrite   bool
	dryRun      bool
	verbose     bool
	insecure    bool
	noProgress  bool
}

var syncArgs = syncFlags{
	output:      "text",
	concurrency: 4,
	retries:     3,
}

func init() {
	syncCmd.Flags().StringVarP(&syncArgs.config, "config", "c", "",
		"Path to the YAML config file (or set "+envConfig+").")
	syncCmd.Flags().VarP(&syncArgs.output, "output", "o", syncArgs.output.Description())
	syncCmd.Flags().IntVar(&syncArgs.concurrency, "concurrency", syncArgs.concurrency,
		"Maximum number of copy operations to run in parallel per job")
	syncCmd.Flags().IntVar(&syncArgs.retries, "retries", syncArgs.retries,
		"Maximum number of retry attempts per job within timeout budget.")
	syncCmd.Flags().BoolVar(&syncArgs.overwrite, "overwrite", false,
		"Force overwrite when the destination artifact digest has drifted")
	syncCmd.Flags().BoolVar(&syncArgs.dryRun, "dry-run", false,
		"Run the plan and comparison pipeline without performing any writes.")
	syncCmd.Flags().BoolVar(&syncArgs.verbose, "verbose", false,
		"Log all operations and the involved digests as they are performed.")
	syncCmd.Flags().BoolVar(&syncArgs.insecure, "insecure", false,
		"Allow plaintext HTTP and skip TLS verification (test/dev only).")
	syncCmd.Flags().BoolVar(&syncArgs.noProgress, "no-progress", false,
		"Disable the live progress spinner.")

	rootCmd.AddCommand(syncCmd)
}

func syncCmdRun(cmd *cobra.Command, _ []string) error {
	cfgPath, err := resolveConfigPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	// Bump per-job timeout to 5m if the user didn't pass --timeout explicitly.
	// The root flag's declared default stays at 1m so other commands aren't
	// affected.
	if !cmd.Flags().Changed("timeout") {
		rootArgs.timeout = syncDefaultTimeout
	}

	// Verbose enables our own progress logs (entry started, mirroring tag,
	// entry summary, etc.). When off, the run is silent on stderr and the
	// only stdout signal in text mode is the start/end markers below.
	// Crane's package-global loggers are intentionally left at their default
	// (discard) — its line shape is too low-level to surface to prettyPrints.
	var logger *slog.Logger
	if syncArgs.verbose {
		logger = slog.New(newPlainHandler(cmd.ErrOrStderr(), slog.LevelDebug))
	} else {
		logger = slog.New(slog.DiscardHandler)
	}

	var clientOpts []oci.ClientOption
	if syncArgs.insecure {
		clientOpts = append(clientOpts, oci.Insecure())
	}
	client := oci.NewClient(clientOpts...)

	runner := &sync.Runner{
		Concurrency:   syncArgs.concurrency,
		Retries:       syncArgs.retries,
		PerJobTimeout: rootArgs.timeout,
		Logger:        logger,
	}

	mirrors := make([]sync.EntryMirror, 0, len(cfg.Artifacts))
	for _, e := range cfg.Artifacts {
		mirrors = append(mirrors, artifacts.New(client, e, artifacts.Options{
			Overwrite: syncArgs.overwrite,
			DryRun:    syncArgs.dryRun,
			Verbose:   syncArgs.verbose,
			CopyJobs:  syncArgs.concurrency,
			Logger:    logger,
		}))
	}
	if len(cfg.Charts) > 0 {
		return fmt.Errorf("chart entries are not implemented yet")
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
	return classifyExit(res)
}

// syncExitError is returned to make `main` exit with a code other than 1
// without printing the default error decoration.
type syncExitError struct {
	code int
	msg  string
}

func (e *syncExitError) Error() string { return e.msg }
func (e *syncExitError) ExitCode() int { return e.code }

func classifyExit(r sync.Result) error {
	// Non-zero exit codes carry an empty message — failures and drift are
	// already surfaced inline (per-failure ✗ lines) and in the Summary
	// totals; printing a third "N failed" footer just adds noise.
	switch r.ExitCode() {
	case 0:
		return nil
	default:
		return &syncExitError{code: r.ExitCode(), msg: ""}
	}
}

func resolveConfigPath() (string, error) {
	if syncArgs.config != "" {
		return syncArgs.config, nil
	}
	if env := os.Getenv(envConfig); env != "" {
		return env, nil
	}
	return "", fmt.Errorf("config required: pass --config/-c or set %s", envConfig)
}
