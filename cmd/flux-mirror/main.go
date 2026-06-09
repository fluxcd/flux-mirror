// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

var (
	VERSION = "0.0.0-dev.0"
)

var rootCmd = &cobra.Command{
	Use:               "flux-mirror",
	Version:           VERSION,
	SilenceUsage:      true,
	SilenceErrors:     true,
	DisableAutoGenTag: true,
	Long: `Flux CLI plugin for mirroring Helm charts and OCI artifacts across registries.
⚠️ Please note that this plugin is in preview and under development.
While we try our best to not introduce breaking changes, they may occur when
we adapt to new features and/or find better ways to facilitate what it does.`,
}

type rootFlags struct {
	timeout    time.Duration
	noEnvsubst bool
}

var rootArgs = rootFlags{
	timeout: time.Minute,
}

func init() {
	rootCmd.PersistentFlags().DurationVar(&rootArgs.timeout, "timeout", rootArgs.timeout,
		"The length of time to wait before giving up on the current operation.")
	rootCmd.PersistentFlags().BoolVar(&rootArgs.noEnvsubst, "no-envsubst", false,
		"Disable environment variable substitution in config files.")

	rootCmd.SetOut(os.Stdout)
}

// exitCoder lets a command return a non-default exit code (e.g. sync's
// distinct 0/1/2 mapping for clean / failure / drift-only).
type exitCoder interface{ ExitCode() int }

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	err := rootCmd.ExecuteContext(ctx)
	if err == nil {
		return
	}
	var ec exitCoder
	if errors.As(err, &ec) {
		if msg := err.Error(); msg != "" {
			rootCmd.PrintErrf("✗ %v\n", msg)
		}
		os.Exit(ec.ExitCode())
	}
	if err.Error() != "" {
		rootCmd.PrintErrf("✗ %v\n", err)
	}
	os.Exit(1)
}
