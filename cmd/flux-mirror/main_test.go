// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"strings"
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
	return executeCommandWithInput(args, "")
}

func executeCommandWithInput(args []string, input string) (string, error) {
	defer resetCmdArgs()

	buf := new(bytes.Buffer)
	cmd := rootCmd
	cmd.SetArgs(args)
	cmd.SetIn(strings.NewReader(input))
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	err := cmd.Execute()
	return buf.String(), err
}

func resetCmdArgs() {
	rootArgs = rootFlags{timeout: timeout}
	rootCmd.SetIn(os.Stdin)

	versionArgs = versionFlags{output: "text"}
	syncArgs = syncFlags{
		output:        "text",
		concurrency:   4,
		timeout:       syncDefaultTimeout,
		retries:       3,
		driftExitCode: syncDefaultDriftExitCode,
	}
	loginArgs = loginFlags{}
	secretArgs = secretFlags{}

	// pflag.Flag.Changed persists across Execute calls on the shared rootCmd,
	// and some bool flag values (notably --help=true) also persist. Reset the
	// bool flags in the command tree to their default state for test isolation;
	// the command arg structs above handle the non-bool flag values.
	resetFlags(rootCmd)
}

func resetFlags(cmd *cobra.Command) {
	resetFlagSet(cmd.Flags())
	resetFlagSet(cmd.PersistentFlags())
	for _, sub := range cmd.Commands() {
		resetFlags(sub)
	}
}

func resetFlagSet(fs *pflag.FlagSet) {
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Value.Type() == "bool" {
			_ = f.Value.Set(f.DefValue)
		}
		f.Changed = false
	})
}
