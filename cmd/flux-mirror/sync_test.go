// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fluxcd/pkg/auth/utils/cijwt"

	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta1"
	"github.com/fluxcd/flux-mirror/internal/registryauth"
	"github.com/fluxcd/flux-mirror/internal/testregistry"
)

// writeJWK writes a fresh private Ed25519 JWK to a temp file and returns its path.
func writeJWK(t *testing.T) string {
	t.Helper()
	return writeJWKFile(t, mustMarshalJWK(t))
}

// writeJWKS writes a fresh private Ed25519 JWK wrapped in a {"keys":[...]} set
// of size n to a temp file and returns its path.
func writeJWKS(t *testing.T, n int) string {
	t.Helper()
	g := NewWithT(t)
	keys := make([]json.RawMessage, n)
	for i := range keys {
		keys[i] = mustMarshalJWK(t)
	}
	b, err := json.Marshal(struct {
		Keys []json.RawMessage `json:"keys"`
	}{Keys: keys})
	g.Expect(err).ToNot(HaveOccurred())
	return writeJWKFile(t, b)
}

func mustMarshalJWK(t *testing.T) []byte {
	t.Helper()
	g := NewWithT(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	g.Expect(err).ToNot(HaveOccurred())
	b, err := json.Marshal(jose.JSONWebKey{Key: priv, KeyID: "k", Algorithm: "EdDSA"})
	g.Expect(err).ToNot(HaveOccurred())
	return b
}

func writeJWKFile(t *testing.T, data []byte) string {
	t.Helper()
	g := NewWithT(t)
	path := filepath.Join(t.TempDir(), "jwk.json")
	g.Expect(os.WriteFile(path, data, 0o600)).To(Succeed())
	return path
}

func TestJWTTransportOptions(t *testing.T) {
	t.Run("provider audience builds a transport with host as default aud", func(t *testing.T) {
		g := NewWithT(t)
		hosts := []apiv1.RegistryHost{{
			Host:       "mint.example",
			Credential: &apiv1.RegistryCredential{Provider: apiv1.JWTProviderForgejo},
		}}
		opts, err := registryauth.JWTTransportOptions(http.DefaultTransport, hosts)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(opts).To(HaveLen(2)) // WithInner + 1 audience.
		_, err = cijwt.NewTransport(opts...)
		g.Expect(err).ToNot(HaveOccurred())
	})

	t.Run("value builds a static token option", func(t *testing.T) {
		g := NewWithT(t)
		hosts := []apiv1.RegistryHost{{
			Host:       "static.example",
			Credential: &apiv1.RegistryCredential{Value: "env-token"},
		}}
		opts, err := registryauth.JWTTransportOptions(http.DefaultTransport, hosts)
		g.Expect(err).ToNot(HaveOccurred())
		_, err = cijwt.NewTransport(opts...)
		g.Expect(err).ToNot(HaveOccurred())
	})

	t.Run("fromPath builds a token-file transport", func(t *testing.T) {
		g := NewWithT(t)
		hosts := []apiv1.RegistryHost{{
			Host:       "static.example",
			Credential: &apiv1.RegistryCredential{FromPath: "/run/secrets/token"},
		}}
		opts, err := registryauth.JWTTransportOptions(http.DefaultTransport, hosts)
		g.Expect(err).ToNot(HaveOccurred())
		_, err = cijwt.NewTransport(opts...)
		g.Expect(err).ToNot(HaveOccurred())
	})

	t.Run("jwkPath signs from the key file", func(t *testing.T) {
		g := NewWithT(t)
		hosts := []apiv1.RegistryHost{{
			Host: "registry.example",
			Credential: &apiv1.RegistryCredential{
				JWKPath: writeJWK(t),
				Iss:     "https://issuer.example",
				Sub:     "client-id",
				Aud:     "registry.example",
			},
		}}
		opts, err := registryauth.JWTTransportOptions(http.DefaultTransport, hosts)
		g.Expect(err).ToNot(HaveOccurred())
		_, err = cijwt.NewTransport(opts...)
		g.Expect(err).ToNot(HaveOccurred())
	})

	t.Run("jwkPath with unreadable file errors", func(t *testing.T) {
		g := NewWithT(t)
		hosts := []apiv1.RegistryHost{{
			Host: "registry.example",
			Credential: &apiv1.RegistryCredential{
				JWKPath: filepath.Join(t.TempDir(), "missing.json"),
				Iss:     "https://issuer.example",
				Sub:     "client-id",
			},
		}}
		_, err := registryauth.JWTTransportOptions(http.DefaultTransport, hosts)
		g.Expect(err).To(MatchError(ContainSubstring("read jwkPath")))
	})

	t.Run("jwkPath accepts a JWK set with exactly one key", func(t *testing.T) {
		g := NewWithT(t)
		hosts := []apiv1.RegistryHost{{
			Host: "registry.example",
			Credential: &apiv1.RegistryCredential{
				JWKPath: writeJWKS(t, 1),
				Iss:     "https://issuer.example",
				Sub:     "client-id",
			},
		}}
		opts, err := registryauth.JWTTransportOptions(http.DefaultTransport, hosts)
		g.Expect(err).ToNot(HaveOccurred())
		_, err = cijwt.NewTransport(opts...)
		g.Expect(err).ToNot(HaveOccurred())
	})

	t.Run("jwkPath rejects a JWK set with more than one key", func(t *testing.T) {
		g := NewWithT(t)
		hosts := []apiv1.RegistryHost{{
			Host: "registry.example",
			Credential: &apiv1.RegistryCredential{
				JWKPath: writeJWKS(t, 2),
				Iss:     "https://issuer.example",
				Sub:     "client-id",
			},
		}}
		_, err := registryauth.JWTTransportOptions(http.DefaultTransport, hosts)
		g.Expect(err).To(MatchError(ContainSubstring("exactly one key, got 2")))
	})

	t.Run("jwkPath rejects an empty JWK set", func(t *testing.T) {
		g := NewWithT(t)
		path := writeJWKFile(t, []byte(`{"keys":[]}`))
		hosts := []apiv1.RegistryHost{{
			Host: "registry.example",
			Credential: &apiv1.RegistryCredential{
				JWKPath: path,
				Iss:     "https://issuer.example",
				Sub:     "client-id",
			},
		}}
		_, err := registryauth.JWTTransportOptions(http.DefaultTransport, hosts)
		g.Expect(err).To(MatchError(ContainSubstring("exactly one key, got 0")))
	})

	t.Run("jwkPath rejects malformed JSON", func(t *testing.T) {
		g := NewWithT(t)
		path := writeJWKFile(t, []byte("not json"))
		hosts := []apiv1.RegistryHost{{
			Host: "registry.example",
			Credential: &apiv1.RegistryCredential{
				JWKPath: path,
				Iss:     "https://issuer.example",
				Sub:     "client-id",
			},
		}}
		_, err := registryauth.JWTTransportOptions(http.DefaultTransport, hosts)
		g.Expect(err).To(MatchError(ContainSubstring("parse JWK")))
	})

	t.Run("jwkValue signs from the inline value", func(t *testing.T) {
		g := NewWithT(t)
		hosts := []apiv1.RegistryHost{{
			Host: "registry.example",
			Credential: &apiv1.RegistryCredential{
				JWKValue: string(mustMarshalJWK(t)),
				Iss:      "https://issuer.example",
				Sub:      "client-id",
				Aud:      "registry.example",
			},
		}}
		opts, err := registryauth.JWTTransportOptions(http.DefaultTransport, hosts)
		g.Expect(err).ToNot(HaveOccurred())
		_, err = cijwt.NewTransport(opts...)
		g.Expect(err).ToNot(HaveOccurred())
	})

	t.Run("jwkValue with custom exp signs from the inline value", func(t *testing.T) {
		g := NewWithT(t)
		hosts := []apiv1.RegistryHost{{
			Host: "registry.example",
			Credential: &apiv1.RegistryCredential{
				JWKValue: string(mustMarshalJWK(t)),
				Iss:      "https://issuer.example",
				Sub:      "client-id",
				Exp:      &metav1.Duration{Duration: time.Hour},
			},
		}}
		opts, err := registryauth.JWTTransportOptions(http.DefaultTransport, hosts)
		g.Expect(err).ToNot(HaveOccurred())
		_, err = cijwt.NewTransport(opts...)
		g.Expect(err).ToNot(HaveOccurred())
	})

	t.Run("jwkValue rejects malformed JWK", func(t *testing.T) {
		g := NewWithT(t)
		hosts := []apiv1.RegistryHost{{
			Host: "registry.example",
			Credential: &apiv1.RegistryCredential{
				JWKValue: "not json",
				Iss:      "https://issuer.example",
				Sub:      "client-id",
			},
		}}
		_, err := registryauth.JWTTransportOptions(http.DefaultTransport, hosts)
		g.Expect(err).To(MatchError(ContainSubstring("parse JWK")))
	})

	t.Run("multiple hosts of mixed kinds build one transport", func(t *testing.T) {
		g := NewWithT(t)
		t.Setenv("MY_CI_TOKEN", "env-token")
		hosts := []apiv1.RegistryHost{
			{Host: "static.example", Credential: &apiv1.RegistryCredential{Value: "env-token"}},
			{Host: "mint.example", Credential: &apiv1.RegistryCredential{Provider: apiv1.JWTProviderGitHub}},
			{Host: "registry.example", Credential: &apiv1.RegistryCredential{
				JWKPath: writeJWK(t), Iss: "https://issuer.example", Sub: "client-id",
			}},
		}
		opts, err := registryauth.JWTTransportOptions(http.DefaultTransport, hosts)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(opts).To(HaveLen(4)) // WithInner + 3 hosts.
		_, err = cijwt.NewTransport(opts...)
		g.Expect(err).ToNot(HaveOccurred())
	})
}

var dockerReg string

func ensureRegistry(t *testing.T) {
	t.Helper()
	testregistry.UseEmptyDockerConfig(t)
	if dockerReg != "" {
		return
	}
	addr, err := testregistry.Start(context.Background())
	if err != nil {
		t.Fatalf("start registry: %s", err)
	}
	dockerReg = addr
}

func writeConfig(t *testing.T, src, dst string) string {
	t.Helper()
	g := NewWithT(t)
	body := configBody(src, dst)
	path := filepath.Join(t.TempDir(), "config.yaml")
	g.Expect(os.WriteFile(path, []byte(body), 0o600)).To(Succeed())
	return path
}

func configBody(src, dst string) string {
	return fmt.Sprintf(`apiVersion: mirror.plugin.fluxcd.io/v1beta1
kind: Config
artifacts:
  - source: %s
    destination: %s
    selector:
      semver: ">=0.0.0"
      limit: 5
`, src, dst)
}

func TestSync_NoConfigError(t *testing.T) {
	g := NewWithT(t)
	t.Setenv("FLUX_MIRROR_CONFIG", "")

	_, err := executeCommand([]string{"sync"})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("config required"))
}

func TestSync_ConfigViaEnv(t *testing.T) {
	g := NewWithT(t)
	ensureRegistry(t)

	src := dockerReg + "/env-src-" + testregistry.RandSuffix()
	dst := dockerReg + "/env-dst-" + testregistry.RandSuffix()
	testregistry.PushImage(t, src+":1.0.0")

	cfgPath := writeConfig(t, src, dst)
	t.Setenv("FLUX_MIRROR_CONFIG", cfgPath)

	out, err := executeCommand([]string{"sync", "--insecure", "-o", "json"})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(out).To(ContainSubstring(`"status": "copied"`))
	g.Expect(out).To(ContainSubstring(`"tag": "1.0.0"`))
}

func TestSync_ConfigViaStdin(t *testing.T) {
	g := NewWithT(t)
	ensureRegistry(t)

	src := dockerReg + "/stdin-src-" + testregistry.RandSuffix()
	dst := dockerReg + "/stdin-dst-" + testregistry.RandSuffix()
	testregistry.PushImage(t, src+":1.0.0")
	t.Setenv("FLUX_MIRROR_CONFIG", "")

	out, err := executeCommandWithInput([]string{"sync", "-", "--insecure", "-o", "json"}, configBody(src, dst))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(out).To(ContainSubstring(`"status": "copied"`))
	g.Expect(out).To(ContainSubstring(`"tag": "1.0.0"`))
}

func TestSync_ArgOverridesEnv(t *testing.T) {
	g := NewWithT(t)
	ensureRegistry(t)

	src := dockerReg + "/ovr-src-" + testregistry.RandSuffix()
	dst := dockerReg + "/ovr-dst-" + testregistry.RandSuffix()
	testregistry.PushImage(t, src+":1.0.0")

	cfgPath := writeConfig(t, src, dst)
	// Env points at a bogus path — the positional argument must win.
	t.Setenv("FLUX_MIRROR_CONFIG", "/nonexistent/path.yaml")

	out, err := executeCommand([]string{"sync", cfgPath, "--insecure", "--verbose"})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(out).To(ContainSubstring(src))
}

func TestSync_DriftExitCode(t *testing.T) {
	g := NewWithT(t)
	ensureRegistry(t)

	src := dockerReg + "/dr-src-" + testregistry.RandSuffix()
	dst := dockerReg + "/dr-dst-" + testregistry.RandSuffix()
	testregistry.PushImage(t, src+":1.0.0")
	testregistry.PushImage(t, dst+":1.0.0") // independent push → different digest

	cfgPath := writeConfig(t, src, dst)
	t.Setenv("FLUX_MIRROR_CONFIG", "")

	_, err := executeCommand([]string{"sync", cfgPath, "--insecure"})
	g.Expect(err).To(HaveOccurred())
	var ec interface{ ExitCode() int }
	g.Expect(errors.As(err, &ec)).To(BeTrue())
	g.Expect(ec.ExitCode()).To(Equal(2))

	out, err := executeCommand([]string{"sync", cfgPath, "--insecure", "--drift-exit-code", "0"})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(out).To(ContainSubstring("drifted"))
}

func TestSync_DriftExitCodeValidation(t *testing.T) {
	g := NewWithT(t)
	cfgPath := writeConfig(t, "ghcr.io/a/b", "ghcr.io/c/d")

	_, err := executeCommand([]string{"sync", cfgPath, "--drift-exit-code", "-1"})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("--drift-exit-code must be between 0 and 255"))
}

func TestSync_FlagValidation(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		setup     func(*testing.T)
		wantError string
	}{
		{
			name:      "rejects non-positive concurrency",
			args:      []string{"sync", "config.yaml", "--concurrency", "0"},
			wantError: "--concurrency must be greater than 0",
		},
		{
			name:      "rejects negative retries",
			args:      []string{"sync", "config.yaml", "--retries", "-1"},
			wantError: "--retries must be greater than or equal to 0",
		},
		{
			name:      "rejects non-positive sync timeout",
			args:      []string{"sync", "config.yaml", "--timeout", "0s"},
			wantError: "--timeout must be greater than 0",
		},
		{
			name: "rejects non-positive inherited root timeout",
			args: []string{"sync", "config.yaml"},
			setup: func(t *testing.T) {
				t.Helper()
				_ = rootCmd.PersistentFlags().Set("timeout", "0s")
			},
			wantError: "--timeout must be greater than 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			defer resetCmdArgs()

			if tt.setup != nil {
				tt.setup(t)
			}

			_, err := executeCommand(tt.args)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tt.wantError))
		})
	}
}

func TestSyncHelp_UsesEffectiveTimeoutDefault(t *testing.T) {
	g := NewWithT(t)

	out, err := executeCommand([]string{"sync", "--help"})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(out).To(ContainSubstring("config's 'hosts' section"))
	g.Expect(out).ToNot(ContainSubstring("config's 'auth' section"))
	g.Expect(out).To(ContainSubstring("--timeout duration"))
	g.Expect(out).To(ContainSubstring("(default 5m0s)"))
}

func TestResolveSyncTimeout(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(*testing.T)
		wantTimeout time.Duration
	}{
		{
			name:        "sync default stays at five minutes",
			wantTimeout: 5 * time.Minute,
		},
		{
			name: "sync local timeout overrides default",
			setup: func(t *testing.T) {
				t.Helper()
				_ = syncCmd.Flags().Set("timeout", "7m")
			},
			wantTimeout: 7 * time.Minute,
		},
		{
			name: "root timeout before subcommand is still honored",
			setup: func(t *testing.T) {
				t.Helper()
				_ = rootCmd.PersistentFlags().Set("timeout", "9m")
			},
			wantTimeout: 9 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			defer resetCmdArgs()

			if tt.setup != nil {
				tt.setup(t)
			}
			g.Expect(resolveSyncTimeout(syncCmd)).To(Equal(tt.wantTimeout))
		})
	}
}

func TestSync_DryRun(t *testing.T) {
	g := NewWithT(t)
	ensureRegistry(t)

	src := dockerReg + "/dry-src-" + testregistry.RandSuffix()
	dst := dockerReg + "/dry-dst-" + testregistry.RandSuffix()
	testregistry.PushImage(t, src+":1.0.0")

	cfgPath := writeConfig(t, src, dst)
	t.Setenv("FLUX_MIRROR_CONFIG", "")

	out, err := executeCommand([]string{"sync", cfgPath, "--insecure", "--dry-run", "-o", "yaml"})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(out).To(ContainSubstring("status: would-copy"))
	g.Expect(out).To(ContainSubstring("tag: 1.0.0"))
}

func TestSync_BadOutputFormat(t *testing.T) {
	g := NewWithT(t)
	cfgPath := writeConfig(t, "ghcr.io/a/b", "ghcr.io/c/d")
	_, err := executeCommand([]string{"sync", cfgPath, "-o", "xml"})
	g.Expect(err).To(HaveOccurred())
}
