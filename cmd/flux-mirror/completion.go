// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

var completionCmd = &cobra.Command{
	Use:   "completion SHELL",
	Short: "Generate a shell completion script",
	Long: `Generate a shell completion script for flux-mirror.

Supported shells are bash, fish, powershell, and zsh.`,
	Example: `  # Load completions in the current bash session
  source <(flux-mirror completion bash)

  # Install zsh completions on macOS with Homebrew
  flux-mirror completion zsh > "$(brew --prefix)/share/zsh/site-functions/_flux-mirror"

  # Load completions in the current fish session
  flux-mirror completion fish | source

  # Load completions in the current PowerShell session
  flux-mirror completion powershell | Out-String | Invoke-Expression`,
	Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
	ValidArgs: []string{"bash", "fish", "powershell", "zsh"},
	RunE:      completionCmdRun,
}

func init() {
	rootCmd.AddCommand(completionCmd)
}

func completionCmdRun(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	switch args[0] {
	case "bash":
		return cmd.Root().GenBashCompletionV2(out, true)
	case "fish":
		return cmd.Root().GenFishCompletion(out, true)
	case "powershell":
		return cmd.Root().GenPowerShellCompletionWithDesc(out)
	case "zsh":
		return cmd.Root().GenZshCompletion(out)
	default:
		return fmt.Errorf("unsupported shell %q", args[0])
	}
}
