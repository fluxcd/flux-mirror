// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var (
	timeout = 30 * time.Second
)

func TestMain(m *testing.M) {
	code := m.Run()
	os.Exit(code)
}

// executeCommand executes a command with the given args
// and returns the output and error. It resets the command
// arguments to their default values after execution to
// ensure test isolation.
func executeCommand(args []string) (string, error) {
	defer resetCmdArgs()

	buf := new(bytes.Buffer)
	cmd := rootCmd
	cmd.SetArgs(args)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	err := cmd.Execute()
	return buf.String(), err
}

func resetCmdArgs() {
	rootArgs.timeout = timeout

	versionArgs = versionFlags{output: "text"}
	syncArgs = syncFlags{output: "text", concurrency: 4, retries: 3}

	// pflag.Flag.Changed persists across Execute calls on the shared rootCmd,
	// which breaks MarkFlagRequired validation in subsequent tests. Clear it
	// for every flag in the command tree.
	resetFlagChanged(rootCmd)
}

func resetFlagChanged(cmd *cobra.Command) {
	cmd.Flags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
	cmd.PersistentFlags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
	for _, sub := range cmd.Commands() {
		resetFlagChanged(sub)
	}
}
