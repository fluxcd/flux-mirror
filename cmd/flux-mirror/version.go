// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/fluxcd/flux-mirror/internal/flags"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version information",
	Example: `  # Print the version
  flux-mirror version

  # Print the full version information in YAML format
  flux-mirror version -o yaml

  # Print the full version information in JSON format
  flux-mirror version -o json`,
	Args: cobra.NoArgs,
	RunE: versionCmdRun,
}

type versionInfo struct {
	Version   string `json:"version"`
	GoVersion string `json:"goVersion"`
}

type versionFlags struct {
	output flags.Output
}

var versionArgs = versionFlags{
	output: "text",
}

func init() {
	versionCmd.Flags().VarP(&versionArgs.output, "output", "o", versionArgs.output.Description())
	rootCmd.AddCommand(versionCmd)
}

func versionCmdRun(cmd *cobra.Command, args []string) error {
	info := versionInfo{
		Version:   VERSION,
		GoVersion: runtime.Version(),
	}

	var output string
	switch versionArgs.output.String() {
	case "text":
		output = info.Version + "\n"
	case "yaml":
		output = fmt.Sprintf("version: %s\ngoVersion: %s\n", info.Version, info.GoVersion)
	case "json":
		data, err := json.MarshalIndent(info, "", "  ")
		if err != nil {
			return err
		}
		output = string(data) + "\n"
	}

	rootCmd.Print(output)
	return nil
}
