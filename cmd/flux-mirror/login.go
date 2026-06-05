// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	dockercfg "github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/credentials"
	"github.com/docker/cli/cli/config/types"
	"github.com/spf13/cobra"

	"github.com/fluxcd/flux-mirror/internal/config"
	"github.com/fluxcd/flux-mirror/internal/jwkio"
)

// loginUsername is stored as the username of the Docker credential entry. The
// identity is carried entirely by the credential (the password), so the
// username is just a non-empty placeholder; token-auth registries ignore it.
const loginUsername = "flux-mirror"

type loginFlags struct {
	config       string
	hosts        []string
	dockerConfig string
	plaintext    bool
}

var loginArgs loginFlags

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Store configured credentials in the Docker config",
	Long: `Resolve the credentials configured under auth.hosts and store them in
the Docker config so subsequent registry requests authenticate as those
identities. These are the same credentials the sync command would attach, minted
once: an OIDC/cloud token for a provider, the value of fromEnv, the contents of
fromPath, or a freshly signed JWT for jwkPath. By default every host in the
config is logged in; restrict with one or more --host flags.

The config is read from --config, else $FLUX_MIRROR_CONFIG, else a path derived
from the executable location ('-' reads the config from stdin).

The credential is written through the Docker credential store, exactly like
'docker login': if a credential helper is configured (credsStore/credHelpers) —
or, on a fresh config, one is auto-detected for the platform — the secret goes
to the OS keychain rather than plaintext in config.json. Pass --plaintext to
force the base64 config.json entry and bypass any helper. The Docker config
location follows Docker's own discovery: --docker-config, else $DOCKER_CONFIG,
else ~/.docker — the same as 'docker --config'.`,
	Example: `  # Log in to every host in the config
  flux-mirror login

  # Specific hosts from a specific config file
  flux-mirror login --host registry.example.com --host other.example.com -f ./flux-mirror.yaml

  # Read the config from stdin
  flux-mirror login -f - < flux-mirror.yaml

  # Force a plaintext config.json entry instead of the OS keychain
  flux-mirror login --plaintext`,
	Args: cobra.NoArgs,
	RunE: loginCmdRun,
}

func loginCmdRun(cmd *cobra.Command, _ []string) error {
	cfgPath, err := resolveLoginConfigPath()
	if err != nil {
		return err
	}
	cfg, err := loadConfig(cmd, cfgPath, false)
	if err != nil {
		return err
	}
	hosts, err := selectAuthHosts(cfg, loginArgs.hosts)
	if err != nil {
		return err
	}

	// dockercfg.Load("") falls back to Docker's own config discovery
	// ($DOCKER_CONFIG, else ~/.docker); a non-empty dir overrides it, matching
	// 'docker --config <dir>'.
	dcf, err := dockercfg.Load(loginArgs.dockerConfig)
	if err != nil {
		return fmt.Errorf("load docker config: %w", err)
	}
	// Decide the keychain default once, from the original config state: when the
	// config has no helper configured yet, adopt the platform default if its
	// docker-credential-* binary is installed (what Docker Desktop does). Only
	// when no helper is available does a credential land base64-encoded in
	// config.json. An existing config is left as the user set it up.
	if !loginArgs.plaintext && !dcf.ContainsAuth() {
		dcf.CredentialsStore = credentials.DetectDefaultStore(dcf.CredentialsStore)
	}

	for _, h := range hosts {
		cred, err := resolveCredential(cmd.Context(), h)
		if err != nil {
			return fmt.Errorf("host %q: %w", h.Host, err)
		}

		var store credentials.Store
		if loginArgs.plaintext {
			// Force a base64 config.json entry, bypassing any helper.
			store = credentials.NewFileStore(dcf)
		} else {
			store = dcf.GetCredentialsStore(h.Host)
		}
		if err := store.Store(types.AuthConfig{
			Username:      loginUsername,
			Password:      cred,
			ServerAddress: h.Host,
		}); err != nil {
			return fmt.Errorf("store docker credential for %q: %w", h.Host, err)
		}

		helper := ""
		if !loginArgs.plaintext {
			if helper = dcf.CredentialHelpers[h.Host]; helper == "" {
				helper = dcf.CredentialsStore
			}
		}
		if helper != "" {
			cmd.Printf("✔ stored credential for %s via the %q credential helper\n", h.Host, helper)
		} else {
			cmd.Printf("✔ stored credential for %s in %s\n", h.Host, dcf.Filename)
		}
	}
	return nil
}

// resolveLoginConfigPath picks the flux-mirror config path: --config if set,
// else $FLUX_MIRROR_CONFIG, else a path derived from the executable location.
func resolveLoginConfigPath() (string, error) {
	if loginArgs.config != "" {
		return loginArgs.config, nil
	}
	if env := os.Getenv(envConfig); env != "" {
		return env, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve default config path: %w", err)
	}
	return exe + ".config", nil
}

// selectAuthHosts returns the auth hosts to log in: the ones named in filter
// (erroring on any not present), or all configured hosts when filter is empty.
func selectAuthHosts(cfg *config.Config, filter []string) ([]config.AuthHost, error) {
	if cfg.Auth == nil || len(cfg.Auth.Hosts) == 0 {
		return nil, fmt.Errorf("config has no auth.hosts")
	}
	if len(filter) == 0 {
		return cfg.Auth.Hosts, nil
	}
	byHost := make(map[string]config.AuthHost, len(cfg.Auth.Hosts))
	for _, h := range cfg.Auth.Hosts {
		byHost[h.Host] = h
	}
	out := make([]config.AuthHost, 0, len(filter))
	for _, name := range filter {
		h, ok := byHost[name]
		if !ok {
			return nil, fmt.Errorf("host %q not found in auth.hosts", name)
		}
		out = append(out, h)
	}
	return out, nil
}

// resolveCredential mints or reads the credential for a single host, mirroring
// what jwtTransportOptions wires into the sync transport but returning the value
// directly. It assumes the config has passed validation (exactly one source).
func resolveCredential(ctx context.Context, h config.AuthHost) (string, error) {
	c := h.Credential
	aud := h.EffectiveAud()
	switch {
	case c.Provider != "":
		fn, err := providerTokenFunc(c.Provider, aud)
		if err != nil {
			return "", err
		}
		return fn(ctx)
	case c.FromEnv != "":
		token := os.Getenv(c.FromEnv)
		if token == "" {
			return "", fmt.Errorf("environment variable %q is not set or empty", c.FromEnv)
		}
		return token, nil
	case c.FromPath != "":
		b, err := os.ReadFile(c.FromPath)
		if err != nil {
			return "", fmt.Errorf("read fromPath: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	case c.JWKPath != "":
		raw, err := jwkio.ReadPrivateJWK(c.JWKPath)
		if err != nil {
			return "", fmt.Errorf("read jwkPath: %w", err)
		}
		fn, err := jwkTokenFunc(raw, c.Iss, c.Sub, aud, c.EffectiveExp())
		if err != nil {
			return "", fmt.Errorf("parse jwkPath: %w", err)
		}
		return fn(ctx)
	default:
		return "", fmt.Errorf("credential has no source")
	}
}

func init() {
	exeConfig := "<executable>.config"
	if exe, err := os.Executable(); err == nil {
		exeConfig = exe + ".config"
	}
	loginCmd.Flags().StringVarP(&loginArgs.config, "config", "f", "",
		fmt.Sprintf("Path to the flux-mirror config, or '-' for stdin "+
			"(default $FLUX_MIRROR_CONFIG, else %s)", exeConfig))
	loginCmd.Flags().StringArrayVar(&loginArgs.hosts, "host", nil,
		"Registry host from the config to log in; repeatable, defaults to all hosts")
	loginCmd.Flags().StringVar(&loginArgs.dockerConfig, "docker-config", "",
		"Location of the Docker client config files, like 'docker --config' "+
			"(default $DOCKER_CONFIG, else ~/.docker)")
	loginCmd.Flags().BoolVar(&loginArgs.plaintext, "plaintext", false,
		"Store the credential base64-encoded in config.json, bypassing any "+
			"OS keychain credential helper")
	rootCmd.AddCommand(loginCmd)
}
