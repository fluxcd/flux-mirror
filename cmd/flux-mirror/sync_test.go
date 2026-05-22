// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/fluxcd/pkg/auth/utils/cioidc"

	"github.com/fluxcd/flux-mirror/internal/testregistry"
)

func TestJWTTransportOptions(t *testing.T) {
	saved := syncArgs
	t.Cleanup(func() { syncArgs = saved })

	reset := func() {
		syncArgs.jwtProvider = ""
		syncArgs.jwtTokens = nil
		syncArgs.jwtAudiences = nil
	}

	t.Run("token requires host", func(t *testing.T) {
		g := NewWithT(t)
		reset()
		syncArgs.jwtTokens = []string{"just-a-token"}
		_, err := jwtTransportOptions(http.DefaultTransport)
		g.Expect(err).To(MatchError(ContainSubstring("<token>@<host>")))
	})

	t.Run("token with empty host", func(t *testing.T) {
		g := NewWithT(t)
		reset()
		syncArgs.jwtTokens = []string{"tok@"}
		_, err := jwtTransportOptions(http.DefaultTransport)
		g.Expect(err).To(MatchError(ContainSubstring("<token>@<host>")))
	})

	t.Run("empty token reads from env var", func(t *testing.T) {
		g := NewWithT(t)
		reset()
		t.Setenv(defaultJWTTokenEnvVar, "env-token")
		syncArgs.jwtTokens = []string{"@static.example"}
		opts, err := jwtTransportOptions(http.DefaultTransport)
		g.Expect(err).ToNot(HaveOccurred())
		_, err = cioidc.NewTransport(opts...)
		g.Expect(err).ToNot(HaveOccurred())
	})

	t.Run("empty token with unset env var", func(t *testing.T) {
		g := NewWithT(t)
		reset()
		t.Setenv(defaultJWTTokenEnvVar, "")
		syncArgs.jwtTokens = []string{"@static.example"}
		_, err := jwtTransportOptions(http.DefaultTransport)
		g.Expect(err).To(MatchError(ContainSubstring("environment variable is not set")))
	})

	t.Run("audience requires provider", func(t *testing.T) {
		g := NewWithT(t)
		reset()
		syncArgs.jwtAudiences = []string{"aud@mint.example"}
		_, err := jwtTransportOptions(http.DefaultTransport)
		g.Expect(err).To(MatchError(ContainSubstring("--jwt-provider is required")))
	})

	t.Run("provider requires audience", func(t *testing.T) {
		g := NewWithT(t)
		reset()
		syncArgs.jwtProvider = jwtProviderGitHub
		syncArgs.jwtTokens = []string{"tok@static.example"}
		_, err := jwtTransportOptions(http.DefaultTransport)
		g.Expect(err).To(MatchError(ContainSubstring("--jwt-audience is required")))
	})

	t.Run("invalid provider", func(t *testing.T) {
		g := NewWithT(t)
		reset()
		syncArgs.jwtProvider = "gitlab"
		syncArgs.jwtAudiences = []string{"aud@mint.example"}
		_, err := jwtTransportOptions(http.DefaultTransport)
		g.Expect(err).To(MatchError(ContainSubstring("invalid --jwt-provider")))
	})

	t.Run("audience with empty value", func(t *testing.T) {
		g := NewWithT(t)
		reset()
		syncArgs.jwtProvider = jwtProviderGitHub
		syncArgs.jwtAudiences = []string{"@host.example"}
		_, err := jwtTransportOptions(http.DefaultTransport)
		g.Expect(err).To(MatchError(ContainSubstring("<audience>[@<host>]")))
	})

	t.Run("valid token and audience build a transport", func(t *testing.T) {
		g := NewWithT(t)
		reset()
		syncArgs.jwtProvider = jwtProviderForgejo
		syncArgs.jwtTokens = []string{"tok@static.example"}
		syncArgs.jwtAudiences = []string{"aud@mint.example", "self.example"}
		opts, err := jwtTransportOptions(http.DefaultTransport)
		g.Expect(err).ToNot(HaveOccurred())
		// WithInner + 1 token + 2 audiences.
		g.Expect(opts).To(HaveLen(4))
		// The options are valid input to cioidc.NewTransport (distinct hosts).
		_, err = cioidc.NewTransport(opts...)
		g.Expect(err).ToNot(HaveOccurred())
	})

	t.Run("duplicate host across flags is rejected by the lib", func(t *testing.T) {
		g := NewWithT(t)
		reset()
		syncArgs.jwtProvider = jwtProviderGitHub
		syncArgs.jwtTokens = []string{"tok@dup.example"}
		syncArgs.jwtAudiences = []string{"aud@dup.example"}
		opts, err := jwtTransportOptions(http.DefaultTransport)
		g.Expect(err).ToNot(HaveOccurred())
		_, err = cioidc.NewTransport(opts...)
		g.Expect(err).To(MatchError(ContainSubstring("configured more than once")))
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
	return fmt.Sprintf(`apiVersion: mirror.fluxcd.io/v1alpha1
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
	g.Expect(out).To(ContainSubstring(`"copied": [`))
	g.Expect(out).To(ContainSubstring(`"1.0.0"`))
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
	g.Expect(out).To(ContainSubstring(`"copied": [`))
	g.Expect(out).To(ContainSubstring(`"1.0.0"`))
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
	g.Expect(out).To(MatchRegexp(`would-copy:\s*\n\s*- 1\.0\.0`))
}

func TestSync_BadOutputFormat(t *testing.T) {
	g := NewWithT(t)
	cfgPath := writeConfig(t, "ghcr.io/a/b", "ghcr.io/c/d")
	_, err := executeCommand([]string{"sync", cfgPath, "-o", "xml"})
	g.Expect(err).To(HaveOccurred())
}
