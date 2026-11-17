// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/fluxcd/flux-mirror/internal/keygen"
)

type keygenFlags struct {
	outputDir string
	alg       string
}

var keygenArgs keygenFlags

var keygenCmd = &cobra.Command{
	Use:   "keygen",
	Short: "Generate a JWK pair for JWK-based registry auth",
	Long: `Generate a JSON Web Key pair and write it to a directory as
pubkey.json (the public JWKS, mode 0644) and privkey.json (the private JWKS,
mode 0600). The directory is created if it does not exist. The command errors
without writing anything if either file already exists.

The --alg flag selects the signing algorithm. It defaults to EdDSA (Ed25519).
The ML-DSA values select the FIPS 204 post-quantum parameter sets, which produce
much larger signatures: 2420, 3309 and 4627 bytes for ML-DSA-44, ML-DSA-65 and
ML-DSA-87 respectively, against 64 bytes for EdDSA. A registry must support the
selected algorithm to verify the resulting tokens.`,
	Example: `  # Write an EdDSA key pair into the current directory
  flux-mirror keygen

  # Write an EdDSA key pair into ./keys/registry (created if missing)
  flux-mirror keygen -o ./keys/registry

  # Write an ML-DSA-65 key pair into ./keys/registry
  flux-mirror keygen -o ./keys/registry --alg ML-DSA-65`,
	Args: cobra.NoArgs,
	RunE: keygenCmdRun,
}

func keygenCmdRun(cmd *cobra.Command, args []string) error {
	res, err := keygen.Sig(keygenArgs.outputDir, jose.SignatureAlgorithm(keygenArgs.alg))
	if err != nil {
		return err
	}
	cmd.Printf("✔ private key set written to: %s\n", res.PrivPath)
	cmd.Printf("✔ public key set written to: %s\n", res.PubPath)
	return nil
}

func init() {
	keygenCmd.Flags().StringVarP(&keygenArgs.outputDir, "output-dir", "o", ".",
		"path to output directory (defaults to current directory)")
	keygenCmd.Flags().StringVar(&keygenArgs.alg, "alg", string(jose.EdDSA),
		"signing algorithm: "+keygen.SupportedAlgorithms())
	rootCmd.AddCommand(keygenCmd)
}
