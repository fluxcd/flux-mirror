// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/fluxcd/pkg/auth/jwt"

	"github.com/fluxcd/flux-mirror/internal/jwkio"
)

var signCmd = &cobra.Command{
	Use:   "sign",
	Short: "Sign credentials with a JWK",
	Args:  cobra.NoArgs,
}

type signJWTFlags struct {
	iss        string
	aud        string
	sub        string
	exp        time.Duration
	keySetPath string
	output     string
}

var signJWTArgs signJWTFlags

var signJWTCmd = &cobra.Command{
	Use:   "jwt",
	Short: "Sign a JWT with a private JWK and write it to a file",
	Long: `Sign a single JWT with the private key from --key-set and write the
compact-serialized token to --output. The key file may be a bare JWK or a JWK
Set wrapping exactly one key. The output file's parent directories are created
if missing, and the command refuses to overwrite an existing file.`,
	Example: `  # Sign a 1h token for a registry host
  flux-mirror sign jwt \
    --iss https://my-issuer.example \
    --aud registry.example.com \
    --sub client-id \
    --exp 1h \
    -k ./keys/registry/privkey.json \
    -o ./tokens/registry.jwt`,
	Args: cobra.NoArgs,
	RunE: signJWTCmdRun,
}

func signJWTCmdRun(cmd *cobra.Command, args []string) error {
	if signJWTArgs.iss == "" {
		return fmt.Errorf("--iss is required")
	}
	if signJWTArgs.aud == "" {
		return fmt.Errorf("--aud is required")
	}
	if signJWTArgs.sub == "" {
		return fmt.Errorf("--sub is required")
	}
	if signJWTArgs.exp <= 0 {
		return fmt.Errorf("--exp must be a positive duration, got %s", signJWTArgs.exp)
	}
	if signJWTArgs.keySetPath == "" {
		return fmt.Errorf("--key-set is required")
	}
	if signJWTArgs.output == "" {
		return fmt.Errorf("--output is required")
	}

	// Refuse to overwrite an existing output file. Checked before signing so
	// we don't generate a token only to throw it away.
	if _, err := os.Stat(signJWTArgs.output); err == nil {
		return fmt.Errorf("%s already exists, refusing to overwrite", signJWTArgs.output)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", signJWTArgs.output, err)
	}

	jwk, err := jwkio.ReadPrivateJWK(signJWTArgs.keySetPath)
	if err != nil {
		return fmt.Errorf("read --key-set: %w", err)
	}
	key, err := jwt.ParseJWK(jwk)
	if err != nil {
		return fmt.Errorf("parse --key-set: %w", err)
	}
	token, err := key.Issue(signJWTArgs.iss, signJWTArgs.sub, signJWTArgs.aud, signJWTArgs.exp)
	if err != nil {
		return fmt.Errorf("sign JWT: %w", err)
	}

	if dir := filepath.Dir(signJWTArgs.output); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create output directory: %w", err)
		}
	}
	f, err := os.OpenFile(signJWTArgs.output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}
	if _, err := f.WriteString(token); err != nil {
		_ = f.Close()
		return fmt.Errorf("write output file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close output file: %w", err)
	}

	cmd.Printf("✔ JWT written to: %s\n", signJWTArgs.output)
	return nil
}

func init() {
	signJWTCmd.Flags().StringVar(&signJWTArgs.iss, "iss", "",
		"issuer claim (iss); required")
	signJWTCmd.Flags().StringVar(&signJWTArgs.aud, "aud", "",
		"audience claim (aud); required")
	signJWTCmd.Flags().StringVar(&signJWTArgs.sub, "sub", "",
		"subject claim (sub); required")
	signJWTCmd.Flags().DurationVar(&signJWTArgs.exp, "exp", 0,
		"token lifetime as a Go duration (e.g. 1h, 24h, 90d-equivalent 2160h); required, must be > 0")
	signJWTCmd.Flags().StringVarP(&signJWTArgs.keySetPath, "key-set", "k", "",
		"path to the private JWK file (bare JWK or single-key JWK set); required")
	signJWTCmd.Flags().StringVarP(&signJWTArgs.output, "output", "o", "",
		"path to write the signed JWT to; required, refuses to overwrite, creates parent dirs")
	signCmd.AddCommand(signJWTCmd)
	rootCmd.AddCommand(signCmd)
}
