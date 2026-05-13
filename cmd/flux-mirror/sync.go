// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log"
	"log/slog"
	"os"
	"time"

	craneLogs "github.com/google/go-containerregistry/pkg/logs"
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
		"Maximum number of tag jobs to run in parallel within an entry "+
			"(per-entry, not global; entries are processed sequentially).")
	syncCmd.Flags().IntVar(&syncArgs.retries, "retries", syncArgs.retries,
		"Maximum number of retry attempts per tag job (within --timeout budget).")
	syncCmd.Flags().BoolVar(&syncArgs.overwrite, "overwrite", false,
		"Force overwrite=true on every entry, regardless of per-entry config.")
	syncCmd.Flags().BoolVar(&syncArgs.dryRun, "dry-run", false,
		"Run the plan and comparison pipeline without performing any writes.")
	syncCmd.Flags().BoolVar(&syncArgs.verbose, "verbose", false,
		"Log selector debug output (excluded tags with reasons).")
	syncCmd.Flags().BoolVar(&syncArgs.insecure, "insecure", false,
		"Allow plaintext HTTP and skip TLS verification (test/dev only).")

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

	level := slog.LevelInfo
	if syncArgs.verbose {
		level = slog.LevelDebug
	}
	stderr := cmd.ErrOrStderr()
	logger := slog.New(newPlainHandler(stderr, level))

	// Wire crane's package-global loggers to the same stderr writer with
	// the matching log.LstdFlags format and no prefix — runner, artifacts,
	// and crane all share one uniform line shape.
	craneLogs.Progress = log.New(stderr, "", log.LstdFlags)
	craneLogs.Warn = log.New(stderr, "", log.LstdFlags)
	if syncArgs.verbose {
		craneLogs.Debug = log.New(stderr, "", log.LstdFlags)
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

	res, err := runner.Run(cmd.Context(), mirrors)
	if err != nil {
		return err
	}
	switch syncArgs.output.String() {
	case "text", "":
		res.LogSummary(logger)
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
	switch r.ExitCode() {
	case 0:
		return nil
	case 1:
		return &syncExitError{code: 1, msg: fmt.Sprintf("%d tag job(s) failed", r.TotalFailures())}
	case 2:
		// Empty message — main skips the "✗ ..." print since drift isn't a
		// failure to surface twice; the structured summary already shows it.
		return &syncExitError{code: 2, msg: ""}
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
