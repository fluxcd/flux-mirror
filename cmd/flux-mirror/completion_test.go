// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	. "github.com/onsi/gomega"
)

func TestCompletionCmd(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		expectError    bool
		expectedOutput []string
	}{
		{
			name: "bash",
			args: []string{"completion", "bash"},
			expectedOutput: []string{
				"bash completion V2 for flux-mirror",
				"__flux-mirror_get_completion_results",
			},
		},
		{
			name: "fish",
			args: []string{"completion", "fish"},
			expectedOutput: []string{
				"complete -c flux-mirror",
				"__flux_mirror_perform_completion",
			},
		},
		{
			name: "powershell",
			args: []string{"completion", "powershell"},
			expectedOutput: []string{
				"Register-ArgumentCompleter -CommandName 'flux-mirror'",
				"__flux_mirrorCompleterBlock",
			},
		},
		{
			name: "zsh",
			args: []string{"completion", "zsh"},
			expectedOutput: []string{
				"#compdef flux-mirror",
				"_flux-mirror",
			},
		},
		{
			name:        "missing shell",
			args:        []string{"completion"},
			expectError: true,
		},
		{
			name:        "unsupported shell",
			args:        []string{"completion", "tcsh"},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			output, err := executeCommand(tt.args)

			if tt.expectError {
				g.Expect(err).To(HaveOccurred())
				return
			}

			g.Expect(err).ToNot(HaveOccurred())
			for _, expected := range tt.expectedOutput {
				g.Expect(output).To(ContainSubstring(expected))
			}
		})
	}
}

func TestCompletionCmd_CompleteShells(t *testing.T) {
	g := NewWithT(t)

	output, err := executeCommand([]string{"__complete", "completion", ""})

	g.Expect(err).ToNot(HaveOccurred())
	for _, shell := range []string{"bash", "fish", "powershell", "zsh"} {
		g.Expect(output).To(ContainSubstring(shell))
	}
	g.Expect(output).To(ContainSubstring(":4"))
}
