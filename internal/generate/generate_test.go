// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package generate

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	gojwt "github.com/golang-jwt/jwt/v5"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	"github.com/fluxcd/flux-mirror/internal/config"
)

func TestJWKPair(t *testing.T) {
	t.Run("creates dir and a usable pair", func(t *testing.T) {
		g := NewWithT(t)
		dir := filepath.Join(t.TempDir(), "nested", "keys")

		kid, err := JWKPair(dir, "my-kid")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(kid).To(Equal("my-kid"))

		// privkey.json parses as an Ed25519 private key with the kid.
		var priv jose.JSONWebKey
		readJSON(t, filepath.Join(dir, PrivKeyFile), &priv)
		g.Expect(priv.KeyID).To(Equal("my-kid"))
		g.Expect(priv.Key).To(BeAssignableToTypeOf(ed25519.PrivateKey{}))

		// pubkey.json parses as the matching public key, same kid, no "d".
		pubBytes, err := os.ReadFile(filepath.Join(dir, PubKeyFile))
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(pubBytes).ToNot(ContainSubstring(`"d"`))
		var pub jose.JSONWebKey
		g.Expect(json.Unmarshal(pubBytes, &pub)).To(Succeed())
		g.Expect(pub.KeyID).To(Equal("my-kid"))
		g.Expect(pub.Key).To(BeAssignableToTypeOf(ed25519.PublicKey{}))
	})

	t.Run("defaults kid to the thumbprint", func(t *testing.T) {
		g := NewWithT(t)
		dir := t.TempDir()
		kid, err := JWKPair(dir, "")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(kid).ToNot(BeEmpty())

		var priv jose.JSONWebKey
		readJSON(t, filepath.Join(dir, PrivKeyFile), &priv)
		tp, err := priv.Thumbprint(0x05) // crypto.SHA256
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(kid).To(Equal(base64.RawURLEncoding.EncodeToString(tp)))
	})

	t.Run("errors if a file already exists", func(t *testing.T) {
		g := NewWithT(t)
		dir := t.TempDir()
		g.Expect(os.WriteFile(filepath.Join(dir, PrivKeyFile), []byte("x"), 0o600)).To(Succeed())

		_, err := JWKPair(dir, "k")
		g.Expect(err).To(MatchError(ContainSubstring("already exists")))
		// pubkey.json must not have been written.
		_, statErr := os.Stat(filepath.Join(dir, PubKeyFile))
		g.Expect(statErr).To(HaveOccurred())
	})
}

// setupHost generates a key pair in a fresh dir and returns a config AuthHost
// whose jwkPath points at the private key, plus the public key for verification.
func setupHost(t *testing.T, host, iss, sub, aud string) (config.AuthHost, ed25519.PublicKey) {
	t.Helper()
	g := NewWithT(t)
	dir := t.TempDir()
	_, err := JWKPair(dir, host+"-kid")
	g.Expect(err).ToNot(HaveOccurred())

	var pub jose.JSONWebKey
	readJSON(t, filepath.Join(dir, PubKeyFile), &pub)

	return config.AuthHost{
		Host: host,
		JWT: &config.AuthJWT{
			JWKPath: filepath.Join(dir, PrivKeyFile),
			Iss:     iss,
			Sub:     sub,
			Aud:     aud,
		},
	}, pub.Key.(ed25519.PublicKey)
}

func TestDockerConfigFromJWK(t *testing.T) {
	g := NewWithT(t)

	host, pub := setupHost(t, "registry.example", "https://issuer", "client-id", "registry.example")
	cfg := &config.Config{Auth: &config.Auth{Hosts: []config.AuthHost{host}}}

	results, err := DockerConfigFromJWK(cfg, 10*time.Minute)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(results).To(HaveLen(1))

	dir := filepath.Dir(host.JWT.JWKPath)
	g.Expect(results[0].Path).To(Equal(filepath.Join(dir, DockerConfigFile)))

	var dc dockerConfigFile
	readJSON(t, results[0].Path, &dc)
	entry, ok := dc.Auths["registry.example"]
	g.Expect(ok).To(BeTrue())
	g.Expect(entry.Username).To(Equal("registry.example"))
	g.Expect(entry.Auth).To(Equal(base64.StdEncoding.EncodeToString(
		[]byte("registry.example:" + entry.Password))))

	// The password is a JWT verifiable with the public key and carries the claims.
	claims := verifyToken(t, entry.Password, pub)
	g.Expect(claims["iss"]).To(Equal("https://issuer"))
	g.Expect(claims["sub"]).To(Equal("client-id"))
	aud, _ := claims.GetAudience()
	g.Expect(aud).To(ContainElement("registry.example"))
}

func TestDockerConfigFromJWK_UpsertSameDir(t *testing.T) {
	g := NewWithT(t)

	// Two hosts whose keys live in the same directory → same output file.
	dir := t.TempDir()
	_, err := JWKPair(dir, "shared")
	g.Expect(err).ToNot(HaveOccurred())
	mkHost := func(name string) config.AuthHost {
		return config.AuthHost{Host: name, JWT: &config.AuthJWT{
			JWKPath: filepath.Join(dir, PrivKeyFile), Iss: "iss", Sub: "sub",
		}}
	}
	cfg := &config.Config{Auth: &config.Auth{Hosts: []config.AuthHost{
		mkHost("a.example"), mkHost("b.example"),
	}}}

	results, err := DockerConfigFromJWK(cfg, time.Minute)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(results).To(HaveLen(2))
	g.Expect(results[0].Path).To(Equal(results[1].Path))

	var dc dockerConfigFile
	readJSON(t, filepath.Join(dir, DockerConfigFile), &dc)
	g.Expect(dc.Auths).To(HaveKey("a.example"))
	g.Expect(dc.Auths).To(HaveKey("b.example"))
}

func TestPullSecretFromJWK(t *testing.T) {
	g := NewWithT(t)

	host, pub := setupHost(t, "registry.example", "https://issuer", "client-id", "")
	cfg := &config.Config{Auth: &config.Auth{Hosts: []config.AuthHost{host}}}

	results, err := PullSecretFromJWK(cfg, 10*time.Minute)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(results).To(HaveLen(1))

	var sec pullSecret
	readYAML(t, results[0].Path, &sec)
	g.Expect(sec.Kind).To(Equal("Secret"))
	g.Expect(sec.Type).To(Equal("kubernetes.io/dockerconfigjson"))
	g.Expect(sec.Metadata.Name).To(Equal("docker"))
	g.Expect(sec.Metadata.Namespace).To(Equal("default"))

	raw := sec.StringData[".dockerconfigjson"]
	g.Expect(raw).To(ContainSubstring("\n  ")) // pretty-printed with 2-space indent
	var dc dockerConfigFile
	g.Expect(json.Unmarshal([]byte(raw), &dc)).To(Succeed())
	entry := dc.Auths["registry.example"]
	g.Expect(entry.Username).To(Equal("registry.example"))
	g.Expect(entry.Password).ToNot(BeEmpty())
	g.Expect(entry.Auth).To(Equal(base64.StdEncoding.EncodeToString(
		[]byte("registry.example:" + entry.Password))))

	claims := verifyToken(t, entry.Password, pub)
	// aud was empty → defaults to host.
	aud, _ := claims.GetAudience()
	g.Expect(aud).To(ContainElement("registry.example"))
}

func TestFromJWK_NoJWKHosts(t *testing.T) {
	g := NewWithT(t)
	cfg := &config.Config{} // no auth at all
	_, err := DockerConfigFromJWK(cfg, time.Minute)
	g.Expect(err).To(MatchError(ContainSubstring("no hosts using jwkPath")))
}

func TestFromJWK_NonPositiveExp(t *testing.T) {
	host, _ := setupHost(t, "registry.example", "https://issuer", "client-id", "")
	cfg := &config.Config{Auth: &config.Auth{Hosts: []config.AuthHost{host}}}

	for _, exp := range []time.Duration{0, -time.Minute} {
		g := NewWithT(t)
		_, err := DockerConfigFromJWK(cfg, exp)
		g.Expect(err).To(MatchError(ContainSubstring("exp must be a positive duration")))
		_, err = PullSecretFromJWK(cfg, exp)
		g.Expect(err).To(MatchError(ContainSubstring("exp must be a positive duration")))
	}
}

func verifyToken(t *testing.T, token string, pub ed25519.PublicKey) gojwt.MapClaims {
	t.Helper()
	g := NewWithT(t)
	claims := gojwt.MapClaims{}
	_, err := gojwt.NewParser().ParseWithClaims(token, claims, func(*gojwt.Token) (any, error) {
		return pub, nil
	})
	g.Expect(err).ToNot(HaveOccurred())
	return claims
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	g := NewWithT(t)
	b, err := os.ReadFile(path)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(json.Unmarshal(b, v)).To(Succeed())
}

func readYAML(t *testing.T, path string, v any) {
	t.Helper()
	g := NewWithT(t)
	b, err := os.ReadFile(path)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(yaml.Unmarshal(b, v)).To(Succeed())
}
