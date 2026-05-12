// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"runtime"
	"testing"

	. "github.com/onsi/gomega"
)

func TestVersionCmd(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		expectError    bool
		expectedOutput []string
	}{
		{
			name: "default text output",
			args: []string{"version"},
			expectedOutput: []string{
				VERSION + "\n",
			},
		},
		{
			name: "explicit text output",
			args: []string{"version", "-o", "text"},
			expectedOutput: []string{
				VERSION + "\n",
			},
		},
		{
			name: "yaml output",
			args: []string{"version", "-o", "yaml"},
			expectedOutput: []string{
				"version: " + VERSION,
				"goVersion: " + runtime.Version(),
			},
		},
		{
			name: "json output",
			args: []string{"version", "-o", "json"},
			expectedOutput: []string{
				`"version": "` + VERSION + `"`,
				`"goVersion": "` + runtime.Version() + `"`,
			},
		},
		{
			name:        "invalid output format",
			args:        []string{"version", "-o", "xml"},
			expectError: true,
		},
		{
			name:        "unexpected positional arg",
			args:        []string{"version", "extra"},
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

func TestVersionCmd_JSON(t *testing.T) {
	g := NewWithT(t)

	output, err := executeCommand([]string{"version", "-o", "json"})
	g.Expect(err).ToNot(HaveOccurred())

	var info versionInfo
	g.Expect(json.Unmarshal([]byte(output), &info)).To(Succeed())
	g.Expect(info.Version).To(Equal(VERSION))
	g.Expect(info.GoVersion).To(Equal(runtime.Version()))
}
