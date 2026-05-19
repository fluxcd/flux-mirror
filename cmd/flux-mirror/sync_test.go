// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/fluxcd/flux-mirror/internal/testregistry"
)

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
