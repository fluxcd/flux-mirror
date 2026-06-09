// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"strings"

	dockercfg "github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/credentials"
	"github.com/docker/cli/cli/config/types"
	"github.com/spf13/cobra"

	"github.com/fluxcd/flux-mirror/internal/registryauth"
)

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
	Long: `Resolve the credentials configured under hosts and store them in
the Docker config so subsequent registry requests authenticate as those
identities. These are the same credentials the sync command would attach, minted
once: an OIDC/cloud token for a provider, the inline credential value, the contents of
fromPath, or a freshly signed JWT for jwkPath. By default every host in the
config is logged in; restrict with one or more --host flags.

The config is read from --config, else $FLUX_MIRROR_CONFIG, else
{{CONFIG_DEFAULT}}
('-' reads the config from stdin).

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
	cfgPath, err := resolveConfigFlag(loginArgs.config)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(cmd, cfgPath, false)
	if err != nil {
		return err
	}
	hosts, err := registryauth.SelectAuthHosts(cfg, loginArgs.hosts)
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
		// A TLS-only host (only transport settings, no credential) has nothing to
		// store; skip it.
		if !registryauth.HasCredential(h) {
			cmd.Printf("• skipping %s: no credential configured (TLS-only host)\n", h.Host)
			continue
		}
		ha, err := registryauth.ResolveHostAuth(cmd.Context(), h)
		if err != nil {
			return fmt.Errorf("host %q: %w", h.Host, err)
		}

		// A bearer RegistryToken can't live in a credential helper (those store
		// username/secret pairs), so it always goes to the config file. A
		// username/password pair respects --plaintext and any configured helper.
		if ha.RegistryToken != "" {
			if err := credentials.NewFileStore(dcf).Store(types.AuthConfig{
				ServerAddress: h.Host,
				RegistryToken: ha.RegistryToken,
			}); err != nil {
				return fmt.Errorf("store docker credential for %q: %w", h.Host, err)
			}
			cmd.Printf("✔ stored registry token for %s in %s\n", h.Host, dcf.Filename)
			continue
		}

		var store credentials.Store
		if loginArgs.plaintext {
			// Force a base64 config.json entry, bypassing any helper.
			store = credentials.NewFileStore(dcf)
		} else {
			store = dcf.GetCredentialsStore(h.Host)
		}
		if err := store.Store(types.AuthConfig{
			Username:      ha.Username,
			Password:      ha.Password,
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

// resolveConfigFlag picks the flux-mirror config path for the flag-based
// commands (login, create): the --config value if set, else
// $FLUX_MIRROR_CONFIG, else a path next to the executable (<binary>.config).
func resolveConfigFlag(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
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

// configDefaultPlaceholder marks where the executable-derived default config
// path is templated into command Long help text at startup.
const configDefaultPlaceholder = "{{CONFIG_DEFAULT}}"

// defaultConfigDescription returns the executable-derived default config path
// for help text, resolved at startup, or a generic placeholder when the
// executable path can't be determined.
func defaultConfigDescription() string {
	if exe, err := os.Executable(); err == nil {
		return exe + ".config"
	}
	return "<executable>.config"
}

// configFlagUsage is the shared --config flag help, with the executable-derived
// default resolved at startup.
func configFlagUsage() string {
	return "Path to the flux-mirror config, or '-' for stdin " +
		"(default $FLUX_MIRROR_CONFIG, else " + defaultConfigDescription() + ")"
}

func init() {
	loginCmd.Long = strings.ReplaceAll(loginCmd.Long, configDefaultPlaceholder, defaultConfigDescription())
	loginCmd.Flags().StringVarP(&loginArgs.config, "config", "f", "", configFlagUsage())
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
