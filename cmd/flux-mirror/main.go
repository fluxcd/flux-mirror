// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
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
	timeout time.Duration
}

var rootArgs = rootFlags{
	timeout: time.Minute,
}

func init() {
	rootCmd.PersistentFlags().DurationVar(&rootArgs.timeout, "timeout", rootArgs.timeout,
		"The length of time to wait before giving up on the current operation.")

	rootCmd.SetOut(os.Stdout)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		if err.Error() != "" {
			rootCmd.PrintErrf("✗ %v\n", err)
		}
		os.Exit(1)
	}
}
