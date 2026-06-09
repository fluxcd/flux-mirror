// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	. "github.com/onsi/gomega"
)

// writeLoginConfig writes a valid config whose single host carries the
// given credential block (already indented two levels under `credential:`) and
// returns its path.
func writeLoginConfig(t *testing.T, credBlock string) string {
	t.Helper()
	return writeLoginConfigIn(t, t.TempDir(), credBlock)
}

// writeLoginConfigIn writes the config into dir, so path fields in credBlock
// (resolved within the config's directory) can reference files placed in dir.
func writeLoginConfigIn(t *testing.T, dir, credBlock string) string {
	t.Helper()
	g := NewWithT(t)
	src := `apiVersion: mirror.fluxcd.io/v1beta1
kind: Config
artifacts:
  - source: ghcr.io/a/b
    destination: ghcr.io/c/d
    selector:
      semver: ">=1.0.0"
      limit: 1
hosts:
  - host: registry.example.com
    credential:
` + credBlock
	path := filepath.Join(dir, "config.yaml")
	g.Expect(os.WriteFile(path, []byte(src), 0o600)).To(Succeed())
	return path
}

// writeJWKIn writes a fresh private JWK into dir and returns its base name, for
// use as a config-relative jwkPath.
func writeJWKIn(t *testing.T, dir string) string {
	t.Helper()
	g := NewWithT(t)
	g.Expect(os.WriteFile(filepath.Join(dir, "jwk.json"), mustMarshalJWK(t), 0o600)).To(Succeed())
	return "jwk.json"
}

// jwkLoginConfig writes a JWK and a login config referencing it (config-relative)
// in the same dir, returning the config path. extra appends extra credential
// lines (already indented two levels under `credential:`).
func jwkLoginConfig(t *testing.T, extra string) string {
	t.Helper()
	dir := t.TempDir()
	jwk := writeJWKIn(t, dir)
	return writeLoginConfigIn(t, dir,
		"      jwkPath: "+jwk+"\n"+
			"      iss: https://issuer.example\n"+
			"      sub: client-id\n"+extra)
}

func parseUnverified(t *testing.T, token string) gojwt.MapClaims {
	t.Helper()
	g := NewWithT(t)
	tok, _, err := gojwt.NewParser(gojwt.WithoutClaimsValidation()).
		ParseUnverified(token, gojwt.MapClaims{})
	g.Expect(err).ToNot(HaveOccurred())
	claims, ok := tok.Claims.(gojwt.MapClaims)
	g.Expect(ok).To(BeTrue())
	return claims
}

// loginStore runs `login` for the standard test host storing into a fresh,
// plaintext Docker config dir, and returns the credential stored for it: the
// registrytoken when present (credential hosts without a username), otherwise
// the password from the decoded auth entry. --plaintext keeps the test off any
// OS keychain helper.
func loginStore(t *testing.T, configRef, input string) (string, error) {
	t.Helper()
	const host = "registry.example.com"
	dir := t.TempDir()
	args := []string{"login", "--host", host, "--config", configRef, "--docker-config", dir, "--plaintext"}
	if _, err := executeCommandWithInput(args, input); err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return "", err
	}
	var parsed struct {
		Auths map[string]struct {
			Auth          string `json:"auth"`
			RegistryToken string `json:"registrytoken"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", err
	}
	entry, ok := parsed.Auths[host]
	if !ok {
		return "", fmt.Errorf("no auth entry for %q", host)
	}
	if entry.RegistryToken != "" {
		return entry.RegistryToken, nil
	}
	dec, err := base64.StdEncoding.DecodeString(entry.Auth)
	if err != nil {
		return "", err
	}
	_, pass, _ := strings.Cut(string(dec), ":")
	return pass, nil
}

func TestLogin_Value(t *testing.T) {
	g := NewWithT(t)
	// value supports ${VAR} substitution from the environment (done at decode).
	t.Setenv("MY_LOGIN_TOKEN", "static-token-value")
	cfg := writeLoginConfig(t, "      value: ${MY_LOGIN_TOKEN}\n")

	cred, err := loginStore(t, cfg, "")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(cred).To(Equal("static-token-value"))
}

func TestLogin_FromPath(t *testing.T) {
	g := NewWithT(t)
	dir := t.TempDir()
	g.Expect(os.WriteFile(filepath.Join(dir, "token"), []byte("  file-token\n"), 0o600)).To(Succeed())
	cfg := writeLoginConfigIn(t, dir, "      fromPath: token\n")

	cred, err := loginStore(t, cfg, "")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(cred).To(Equal("file-token"))
}

func TestLogin_JWKPath(t *testing.T) {
	g := NewWithT(t)
	cfg := jwkLoginConfig(t, "      aud: custom-aud\n")

	cred, err := loginStore(t, cfg, "")
	g.Expect(err).ToNot(HaveOccurred())

	claims := parseUnverified(t, cred)
	g.Expect(claims["iss"]).To(Equal("https://issuer.example"))
	g.Expect(claims["sub"]).To(Equal("client-id"))
	g.Expect(claims["aud"]).To(ContainElement("custom-aud"))
	g.Expect(claims["exp"]).ToNot(BeNil())
}

func TestLogin_JWKPathExp(t *testing.T) {
	g := NewWithT(t)

	// Default exp (~60s): exp claim is within a couple minutes of now.
	defCred, err := loginStore(t, jwkLoginConfig(t, ""), "")
	g.Expect(err).ToNot(HaveOccurred())
	defExp, err := parseUnverified(t, defCred).GetExpirationTime()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(time.Until(defExp.Time)).To(BeNumerically("<", 5*time.Minute))

	// Explicit exp (1h): exp claim is far in the future.
	expCred, err := loginStore(t, jwkLoginConfig(t, "      exp: 1h\n"), "")
	g.Expect(err).ToNot(HaveOccurred())
	longExp, err := parseUnverified(t, expCred).GetExpirationTime()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(time.Until(longExp.Time)).To(BeNumerically(">", 50*time.Minute))
}

func TestLogin_AudDefaultsToHost(t *testing.T) {
	g := NewWithT(t)
	cred, err := loginStore(t, jwkLoginConfig(t, ""), "")
	g.Expect(err).ToNot(HaveOccurred())

	claims := parseUnverified(t, cred)
	g.Expect(claims["aud"]).To(ContainElement("registry.example.com"))
}

func TestLogin_ConfigFromStdin(t *testing.T) {
	g := NewWithT(t)
	t.Setenv("MY_LOGIN_TOKEN", "stdin-token")
	src := `apiVersion: mirror.fluxcd.io/v1beta1
kind: Config
hosts:
  - host: registry.example.com
    credential:
      value: ${MY_LOGIN_TOKEN}
`
	cred, err := loginStore(t, "-", src)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(cred).To(Equal("stdin-token"))
}

func TestLogin_AuthOnlyConfig(t *testing.T) {
	g := NewWithT(t)
	t.Setenv("MY_LOGIN_TOKEN", "auth-only-token")
	// No charts or artifacts — valid for login, rejected by sync.
	src := `apiVersion: mirror.fluxcd.io/v1beta1
kind: Config
hosts:
  - host: registry.example.com
    credential:
      value: ${MY_LOGIN_TOKEN}
`
	path := filepath.Join(t.TempDir(), "auth-only.yaml")
	g.Expect(os.WriteFile(path, []byte(src), 0o600)).To(Succeed())

	cred, err := loginStore(t, path, "")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(cred).To(Equal("auth-only-token"))

	// The same config is rejected by sync, which requires entries.
	_, err = executeCommand([]string{"sync", path})
	g.Expect(err).To(MatchError(ContainSubstring("config has no entries")))
}

func TestLogin_StoresRegistryToken(t *testing.T) {
	g := NewWithT(t)
	t.Setenv("MY_LOGIN_TOKEN", "static-token-value")
	cfg := writeLoginConfig(t, "      value: ${MY_LOGIN_TOKEN}\n")
	dockerDir := t.TempDir()

	out, err := executeCommand([]string{
		"login", "--host", "registry.example.com", "--config", cfg, "--docker-config", dockerDir, "--plaintext",
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(out).To(ContainSubstring("registry.example.com"))

	data, err := os.ReadFile(filepath.Join(dockerDir, "config.json"))
	g.Expect(err).ToNot(HaveOccurred())
	var parsed struct {
		Auths map[string]struct {
			Auth          string `json:"auth"`
			Username      string `json:"username"`
			Password      string `json:"password"`
			RegistryToken string `json:"registrytoken"`
		} `json:"auths"`
	}
	g.Expect(json.Unmarshal(data, &parsed)).To(Succeed())
	entry, ok := parsed.Auths["registry.example.com"]
	g.Expect(ok).To(BeTrue())
	// No username => bearer registrytoken, no username/password/auth.
	g.Expect(entry.RegistryToken).To(Equal("static-token-value"))
	g.Expect(entry.Auth).To(BeEmpty())
	g.Expect(entry.Username).To(BeEmpty())
	g.Expect(entry.Password).To(BeEmpty())
}

func TestLogin_UsernameStoresUserPassword(t *testing.T) {
	g := NewWithT(t)
	t.Setenv("MY_LOGIN_TOKEN", "static-token-value")
	// username is a host-level field, sitting alongside (not under) credential.
	src := `apiVersion: mirror.fluxcd.io/v1beta1
kind: Config
hosts:
  - host: registry.example.com
    username: robot
    credential:
      value: ${MY_LOGIN_TOKEN}
`
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	g.Expect(os.WriteFile(cfg, []byte(src), 0o600)).To(Succeed())
	dockerDir := t.TempDir()

	_, err := executeCommand([]string{
		"login", "--host", "registry.example.com", "--config", cfg, "--docker-config", dockerDir, "--plaintext",
	})
	g.Expect(err).ToNot(HaveOccurred())

	data, err := os.ReadFile(filepath.Join(dockerDir, "config.json"))
	g.Expect(err).ToNot(HaveOccurred())
	var parsed struct {
		Auths map[string]struct {
			Auth          string `json:"auth"`
			RegistryToken string `json:"registrytoken"`
		} `json:"auths"`
	}
	g.Expect(json.Unmarshal(data, &parsed)).To(Succeed())
	entry := parsed.Auths["registry.example.com"]
	// Username set => standard username/password/auth, no registrytoken.
	g.Expect(entry.RegistryToken).To(BeEmpty())
	dec, err := base64.StdEncoding.DecodeString(entry.Auth)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(string(dec)).To(Equal("robot:static-token-value"))
}

func TestLogin_AllHostsByDefault(t *testing.T) {
	g := NewWithT(t)
	t.Setenv("TOKEN_A", "cred-a")
	t.Setenv("TOKEN_B", "cred-b")
	src := `apiVersion: mirror.fluxcd.io/v1beta1
kind: Config
hosts:
  - host: a.example.com
    credential:
      value: ${TOKEN_A}
  - host: b.example.com
    credential:
      value: ${TOKEN_B}
`
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	g.Expect(os.WriteFile(cfgPath, []byte(src), 0o600)).To(Succeed())
	dockerDir := t.TempDir()

	_, err := executeCommand([]string{"login", "--config", cfgPath, "--docker-config", dockerDir, "--plaintext"})
	g.Expect(err).ToNot(HaveOccurred())

	data, err := os.ReadFile(filepath.Join(dockerDir, "config.json"))
	g.Expect(err).ToNot(HaveOccurred())
	var parsed struct {
		Auths map[string]struct {
			RegistryToken string `json:"registrytoken"`
		} `json:"auths"`
	}
	g.Expect(json.Unmarshal(data, &parsed)).To(Succeed())
	g.Expect(parsed.Auths).To(HaveLen(2))
	g.Expect(parsed.Auths["a.example.com"].RegistryToken).To(Equal("cred-a"))
	g.Expect(parsed.Auths["b.example.com"].RegistryToken).To(Equal("cred-b"))
}

func TestLogin_HostNotFound(t *testing.T) {
	g := NewWithT(t)
	cfg := writeLoginConfig(t, "      value: static-token\n")

	_, err := executeCommand([]string{"login", "--host", "other.example.com", "--config", cfg})
	g.Expect(err).To(MatchError(ContainSubstring(`host "other.example.com" not found`)))
}

func TestLogin_NoHosts(t *testing.T) {
	g := NewWithT(t)
	src := `apiVersion: mirror.fluxcd.io/v1beta1
kind: Config
artifacts:
  - source: ghcr.io/a/b
    destination: ghcr.io/c/d
    selector:
      semver: ">=1.0.0"
      limit: 1
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	g.Expect(os.WriteFile(path, []byte(src), 0o600)).To(Succeed())

	_, err := executeCommand([]string{"login", "--config", path})
	g.Expect(err).To(MatchError(ContainSubstring("no hosts")))
}

func TestLogin_ValueUnsetEnv(t *testing.T) {
	g := NewWithT(t)
	// Referencing an unset variable fails at decode (strict substitution).
	cfg := writeLoginConfig(t, "      value: ${DEFINITELY_UNSET_LOGIN_TOKEN}\n")

	_, err := executeCommand([]string{"login", "--config", cfg})
	g.Expect(err).To(MatchError(ContainSubstring("substitute environment variables")))
}

func TestLogin_SkipsTLSOnlyHost(t *testing.T) {
	g := NewWithT(t)
	// A TLS-only host has no credential to store: login skips it without error
	// (and without panicking).
	src := `apiVersion: mirror.fluxcd.io/v1beta1
kind: Config
hosts:
  - host: tls.example.com
    tls:
      serverAuth:
        value: dummy
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	g.Expect(os.WriteFile(path, []byte(src), 0o600)).To(Succeed())
	dockerDir := t.TempDir()

	out, err := executeCommand([]string{"login", "--config", path, "--docker-config", dockerDir, "--plaintext"})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(out).To(ContainSubstring("skipping tls.example.com"))
}
