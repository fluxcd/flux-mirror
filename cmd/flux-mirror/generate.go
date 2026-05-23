// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/fluxcd/flux-mirror/internal/config"
	"github.com/fluxcd/flux-mirror/internal/generate"
)

const generateDefaultExp = time.Hour

var generateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate JWK pairs and registry credentials for JWK-based auth",
	Args:  cobra.NoArgs,
}

// generate jwk-pair

type jwkPairFlags struct {
	dir string
	kid string
}

var jwkPairArgs jwkPairFlags

var jwkPairCmd = &cobra.Command{
	Use:   "jwk-pair",
	Short: "Generate an EdDSA JWK pair (pubkey.json, privkey.json)",
	Long: `Generate an EdDSA JSON Web Key pair and write it to a directory as
pubkey.json and privkey.json. The directory is created if it does not exist. The
command errors without writing anything if either file already exists.`,
	Example: `  # Write a key pair into ./keys/registry (created if missing)
  flux-mirror generate jwk-pair -f ./keys/registry

  # Use an explicit key id instead of the JWK thumbprint
  flux-mirror generate jwk-pair -f ./keys/registry --kid my-key-1`,
	Args: cobra.NoArgs,
	RunE: jwkPairCmdRun,
}

func jwkPairCmdRun(cmd *cobra.Command, args []string) error {
	if jwkPairArgs.dir == "" {
		return fmt.Errorf("-f/--file is required")
	}
	kid, err := generate.JWKPair(jwkPairArgs.dir, jwkPairArgs.kid)
	if err != nil {
		return err
	}
	cmd.Printf("✓ wrote %s/%s and %s/%s (kid: %s)\n",
		jwkPairArgs.dir, generate.PrivKeyFile, jwkPairArgs.dir, generate.PubKeyFile, kid)
	return nil
}

// generate docker-config / pull-secret

type fromJWKFlags struct {
	exp time.Duration
}

var (
	dockerConfigArgs fromJWKFlags
	pullSecretArgs   fromJWKFlags
)

var dockerConfigCmd = &cobra.Command{
	Use:   "docker-config",
	Short: "Generate Docker config files",
	Args:  cobra.NoArgs,
}

var dockerConfigFromJWKCmd = &cobra.Command{
	Use:   "from-jwk <config>",
	Short: "Issue JWTs from each jwkPath host and write docker-config.json files",
	Long: `For every host in the config that uses jwkPath, read privkey.json from
the key directory, issue a JWT with the host's iss/sub/aud, and write a
docker-config.json next to the key with the host as the username and the JWT as
the password. Hosts sharing a key directory are upserted into the same file.`,
	Example: `  # Generate docker-config.json files with 1h tokens
  flux-mirror generate docker-config from-jwk flux-mirror.yaml

  # Use 24h tokens
  flux-mirror generate docker-config from-jwk flux-mirror.yaml --exp 24h`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runFromJWK(cmd, args[0], dockerConfigArgs.exp, generate.DockerConfigFromJWK)
	},
}

var pullSecretCmd = &cobra.Command{
	Use:   "pull-secret",
	Short: "Generate Kubernetes image pull secrets",
	Args:  cobra.NoArgs,
}

var pullSecretFromJWKCmd = &cobra.Command{
	Use:   "from-jwk <config>",
	Short: "Issue JWTs from each jwkPath host and write pull-secret.yaml files",
	Long: `Like 'docker-config from-jwk', but writes a Kubernetes dockerconfigjson
Secret (pull-secret.yaml, name "docker", namespace "default") wrapping the same
credentials.`,
	Example: `  # Generate pull-secret.yaml files with 1h tokens
  flux-mirror generate pull-secret from-jwk flux-mirror.yaml --exp 1h`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runFromJWK(cmd, args[0], pullSecretArgs.exp, generate.PullSecretFromJWK)
	},
}

// runFromJWK loads and validates the config, runs gen, and prints one line per
// generated file.
func runFromJWK(cmd *cobra.Command, cfgPath string, exp time.Duration,
	gen func(*config.Config, time.Duration) ([]generate.Result, error)) error {

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	results, err := gen(cfg, exp)
	if err != nil {
		return err
	}
	for _, r := range results {
		cmd.Printf("✓ %s → %s\n", r.Host, r.Path)
	}
	return nil
}

func init() {
	jwkPairCmd.Flags().StringVarP(&jwkPairArgs.dir, "file", "f", "",
		"Directory to write pubkey.json and privkey.json into; created if missing.")
	jwkPairCmd.Flags().StringVar(&jwkPairArgs.kid, "kid", "",
		"Key id for both keys. Defaults to the RFC 7638 JWK thumbprint.")
	generateCmd.AddCommand(jwkPairCmd)

	dockerConfigFromJWKCmd.Flags().DurationVar(&dockerConfigArgs.exp, "exp", generateDefaultExp,
		"Lifetime of the issued JWT (Go duration, e.g. 30m, 24h).")
	dockerConfigCmd.AddCommand(dockerConfigFromJWKCmd)
	generateCmd.AddCommand(dockerConfigCmd)

	pullSecretFromJWKCmd.Flags().DurationVar(&pullSecretArgs.exp, "exp", generateDefaultExp,
		"Lifetime of the issued JWT (Go duration, e.g. 30m, 24h).")
	pullSecretCmd.AddCommand(pullSecretFromJWKCmd)
	generateCmd.AddCommand(pullSecretCmd)

	rootCmd.AddCommand(generateCmd)
}
