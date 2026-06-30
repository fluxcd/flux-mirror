// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/cli-runtime/pkg/genericclioptions"
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
	resetSecretKubeFlags()
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

func resetSecretKubeFlags() {
	fresh := genericclioptions.NewConfigFlags(true)

	resetStringPtr(secretKubeFlags.CacheDir, fresh.CacheDir)
	resetStringPtr(secretKubeFlags.KubeConfig, fresh.KubeConfig)
	resetStringPtr(secretKubeFlags.ClusterName, fresh.ClusterName)
	resetStringPtr(secretKubeFlags.AuthInfoName, fresh.AuthInfoName)
	resetStringPtr(secretKubeFlags.Context, fresh.Context)
	resetStringPtr(secretKubeFlags.Namespace, fresh.Namespace)
	resetStringPtr(secretKubeFlags.APIServer, fresh.APIServer)
	resetStringPtr(secretKubeFlags.TLSServerName, fresh.TLSServerName)
	resetBoolPtr(secretKubeFlags.Insecure, fresh.Insecure)
	resetStringPtr(secretKubeFlags.CertFile, fresh.CertFile)
	resetStringPtr(secretKubeFlags.KeyFile, fresh.KeyFile)
	resetStringPtr(secretKubeFlags.CAFile, fresh.CAFile)
	resetStringPtr(secretKubeFlags.BearerToken, fresh.BearerToken)
	resetStringPtr(secretKubeFlags.Impersonate, fresh.Impersonate)
	resetStringPtr(secretKubeFlags.ImpersonateUID, fresh.ImpersonateUID)
	resetStringSlicePtr(secretKubeFlags.ImpersonateGroup, fresh.ImpersonateGroup)
	resetStringSlicePtr(secretKubeFlags.ImpersonateUserExtra, fresh.ImpersonateUserExtra)
	resetStringPtr(secretKubeFlags.Username, fresh.Username)
	resetStringPtr(secretKubeFlags.Password, fresh.Password)
	resetStringPtr(secretKubeFlags.Timeout, fresh.Timeout)
	resetBoolPtr(secretKubeFlags.DisableCompression, fresh.DisableCompression)
}

func resetStringPtr(dst, src *string) {
	if dst != nil && src != nil {
		*dst = *src
	}
}

func resetBoolPtr(dst, src *bool) {
	if dst != nil && src != nil {
		*dst = *src
	}
}

func resetStringSlicePtr(dst, src *[]string) {
	if dst != nil && src != nil {
		*dst = append((*dst)[:0], (*src)...)
	}
}

func TestResetCmdArgs_ResetsSecretKubeFlags(t *testing.T) {
	g := NewWithT(t)

	g.Expect(secretCmd.Flags().Set("namespace", "flux-system")).To(Succeed())
	g.Expect(secretCmd.Flags().Set("context", "prod")).To(Succeed())
	g.Expect(secretCmd.Flags().Set("request-timeout", "45s")).To(Succeed())
	g.Expect(secretCmd.Flags().Set("insecure-skip-tls-verify", "true")).To(Succeed())

	resetCmdArgs()

	g.Expect(secretCmd.Flags().Lookup("namespace").Value.String()).To(BeEmpty())
	g.Expect(secretCmd.Flags().Lookup("context").Value.String()).To(BeEmpty())
	g.Expect(secretCmd.Flags().Lookup("request-timeout").Value.String()).To(Equal("0"))
	g.Expect(secretCmd.Flags().Lookup("insecure-skip-tls-verify").Value.String()).To(Equal("false"))
}
