// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/fluxcd/flux-mirror/internal/keygen"
)

func TestKeygenCmd(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		expectError bool
		check       func(g Gomega, output, dir string)
	}{
		{
			name: "writes keys to specified directory",
			check: func(g Gomega, output, dir string) {
				args := []string{"keygen", "-o", dir}
				out, err := executeCommand(args)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(out).To(ContainSubstring("private key set written to:"))
				g.Expect(out).To(ContainSubstring("public key set written to:"))
				g.Expect(filepath.Join(dir, keygen.PrivKeyFile)).To(BeAnExistingFile())
				g.Expect(filepath.Join(dir, keygen.PubKeyFile)).To(BeAnExistingFile())
			},
		},
		{
			name: "errors when output file already exists",
			check: func(g Gomega, output, dir string) {
				// Pre-create the private key file to trigger the overwrite guard.
				g.Expect(os.WriteFile(filepath.Join(dir, keygen.PrivKeyFile), []byte("{}"), 0o600)).To(Succeed())
				_, err := executeCommand([]string{"keygen", "-o", dir})
				g.Expect(err).To(HaveOccurred())
			},
		},
		{
			name: "rejects positional arguments",
			check: func(g Gomega, output, dir string) {
				_, err := executeCommand([]string{"keygen", "extra"})
				g.Expect(err).To(HaveOccurred())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			dir := t.TempDir()
			tt.check(g, "", dir)
		})
	}
}

func TestKeygenCmd_FlagNotLeaked(t *testing.T) {
	g := NewWithT(t)

	dir := t.TempDir()
	_, err := executeCommand([]string{"keygen", "-o", dir})
	g.Expect(err).ToNot(HaveOccurred())

	// resetCmdArgs must restore outputDir to "."; without the fix, a
	// subsequent call without -o would reuse dir and fail with "already exists".
	g.Expect(keygenArgs.outputDir).To(Equal("."))
}
