// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/fluxcd/flux-mirror/internal/keygen"
)

var keygenCmd = &cobra.Command{
	Use:   "keygen",
	Short: "Generate JSON Web Keys for JWK-based registry auth",
	Args:  cobra.NoArgs,
}

type keygenSigFlags struct {
	outputDir string
}

var keygenSigArgs keygenSigFlags

var keygenSigCmd = &cobra.Command{
	Use:   "sig",
	Short: "Generate an EdDSA JWK pair (pubkey.json, privkey.json) for signing",
	Long: `Generate an EdDSA JSON Web Key pair and write it to a directory as
pubkey.json (the public JWKS, mode 0644) and privkey.json (the private JWKS,
mode 0600). The directory is created if it does not exist. The command errors
without writing anything if either file already exists.`,
	Example: `  # Write a key pair into the current directory
  flux-mirror keygen sig

  # Write a key pair into ./keys/registry (created if missing)
  flux-mirror keygen sig -o ./keys/registry`,
	Args: cobra.NoArgs,
	RunE: keygenSigCmdRun,
}

func keygenSigCmdRun(cmd *cobra.Command, args []string) error {
	res, err := keygen.Sig(keygenSigArgs.outputDir)
	if err != nil {
		return err
	}
	cmd.Printf("✔ private key set written to: %s\n", res.PrivPath)
	cmd.Printf("✔ public key set written to: %s\n", res.PubPath)
	return nil
}

func init() {
	keygenSigCmd.Flags().StringVarP(&keygenSigArgs.outputDir, "output-dir", "o", ".",
		"path to output directory (defaults to current directory)")
	keygenCmd.AddCommand(keygenSigCmd)
	rootCmd.AddCommand(keygenCmd)
}
