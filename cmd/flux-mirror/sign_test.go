// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	gojwt "github.com/golang-jwt/jwt/v5"
	. "github.com/onsi/gomega"
	"github.com/spf13/pflag"

	"github.com/fluxcd/flux-mirror/internal/keygen"
)

func freshKeyDir(t *testing.T) (*keygen.Result, string) {
	t.Helper()
	g := NewWithT(t)
	dir := t.TempDir()
	res, err := keygen.Sig(filepath.Join(dir, "keys"))
	g.Expect(err).ToNot(HaveOccurred())
	return res, dir
}

// resetSignJWTArgs clears the cobra-bound globals so a previous invocation in
// the same test binary does not leak default values into the next one.
func resetSignJWTArgs(t *testing.T) {
	t.Helper()
	signJWTArgs = signJWTFlags{}
	signJWTCmd.Flags().VisitAll(func(f *pflag.Flag) { _ = f.Value.Set(f.DefValue) })
}

func parseUnverified(t *testing.T, token string) (gojwt.MapClaims, map[string]any) {
	t.Helper()
	g := NewWithT(t)
	parser := gojwt.NewParser(gojwt.WithoutClaimsValidation())
	tok, _, err := parser.ParseUnverified(token, gojwt.MapClaims{})
	g.Expect(err).ToNot(HaveOccurred())
	claims, ok := tok.Claims.(gojwt.MapClaims)
	g.Expect(ok).To(BeTrue())
	return claims, tok.Header
}

func TestSignJWT_Success(t *testing.T) {
	g := NewWithT(t)
	resetSignJWTArgs(t)
	res, base := freshKeyDir(t)
	out := filepath.Join(base, "nested", "dir", "out.jwt")

	_, err := executeCommand([]string{
		"sign", "jwt",
		"--iss", "https://issuer.example",
		"--aud", "registry.example",
		"--sub", "team-platform",
		"--exp", "1h",
		"-k", res.PrivPath,
		"-o", out,
	})
	g.Expect(err).ToNot(HaveOccurred())

	data, err := os.ReadFile(out)
	g.Expect(err).ToNot(HaveOccurred())
	token := string(data)
	g.Expect(strings.Count(token, ".")).To(Equal(2), "compact JWT has three dot-separated segments")

	claims, header := parseUnverified(t, token)
	g.Expect(claims["iss"]).To(Equal("https://issuer.example"))
	g.Expect(claims["sub"]).To(Equal("team-platform"))
	g.Expect(claims["aud"]).To(ContainElement("registry.example"))
	g.Expect(claims).To(HaveKey("exp"))
	g.Expect(claims).To(HaveKey("iat"))
	g.Expect(claims).To(HaveKey("nbf"))
	g.Expect(claims).To(HaveKey("jti"))
	g.Expect(header["alg"]).To(Equal("EdDSA"))
	g.Expect(header["kid"]).To(Equal(res.KeyID))

	if runtime.GOOS != "windows" {
		st, err := os.Stat(out)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(st.Mode().Perm()).To(Equal(os.FileMode(0o600)))
	}
}

func TestSignJWT_AcceptsBareJWK(t *testing.T) {
	g := NewWithT(t)
	resetSignJWTArgs(t)
	base := t.TempDir()

	// Extract the single JWK from the JWK set produced by keygen.
	res, err := keygen.Sig(filepath.Join(base, "keys"))
	g.Expect(err).ToNot(HaveOccurred())
	setData, err := os.ReadFile(res.PrivPath)
	g.Expect(err).ToNot(HaveOccurred())
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	g.Expect(json.Unmarshal(setData, &set)).To(Succeed())
	g.Expect(set.Keys).To(HaveLen(1))
	bare := filepath.Join(base, "bare.jwk")
	g.Expect(os.WriteFile(bare, set.Keys[0], 0o600)).To(Succeed())

	out := filepath.Join(base, "out.jwt")
	_, err = executeCommand([]string{
		"sign", "jwt",
		"--iss", "https://issuer.example",
		"--aud", "registry.example",
		"--sub", "s",
		"--exp", "10m",
		"-k", bare,
		"-o", out,
	})
	g.Expect(err).ToNot(HaveOccurred())

	data, err := os.ReadFile(out)
	g.Expect(err).ToNot(HaveOccurred())
	claims, _ := parseUnverified(t, string(data))
	g.Expect(claims["sub"]).To(Equal("s"))
}

func TestSignJWT_RequiredFlags(t *testing.T) {
	res, base := freshKeyDir(t)
	out := filepath.Join(base, "out.jwt")
	full := []string{
		"sign", "jwt",
		"--iss", "https://i.example",
		"--aud", "a",
		"--sub", "s",
		"--exp", "1h",
		"-k", res.PrivPath,
		"-o", out,
	}

	cases := []struct {
		name   string
		strip  string
		errSub string
	}{
		{"missing iss", "--iss", "--iss is required"},
		{"missing aud", "--aud", "--aud is required"},
		{"missing sub", "--sub", "--sub is required"},
		{"missing key-set", "-k", "--key-set is required"},
		{"missing output", "-o", "--output is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			resetSignJWTArgs(t)
			args := stripFlag(full, tc.strip)
			_, err := executeCommand(args)
			g.Expect(err).To(MatchError(ContainSubstring(tc.errSub)))
		})
	}
}

func TestSignJWT_NonPositiveExp(t *testing.T) {
	res, base := freshKeyDir(t)
	for _, exp := range []string{"0s", "-1h"} {
		t.Run("exp="+exp, func(t *testing.T) {
			g := NewWithT(t)
			resetSignJWTArgs(t)
			_, err := executeCommand([]string{
				"sign", "jwt",
				"--iss", "https://i.example",
				"--aud", "a",
				"--sub", "s",
				"--exp", exp,
				"-k", res.PrivPath,
				"-o", filepath.Join(base, "out-"+exp+".jwt"),
			})
			g.Expect(err).To(MatchError(ContainSubstring("--exp must be a positive duration")))
		})
	}
}

func TestSignJWT_RefusesToOverwrite(t *testing.T) {
	g := NewWithT(t)
	resetSignJWTArgs(t)
	res, base := freshKeyDir(t)
	out := filepath.Join(base, "existing.jwt")
	g.Expect(os.WriteFile(out, []byte("old"), 0o600)).To(Succeed())

	_, err := executeCommand([]string{
		"sign", "jwt",
		"--iss", "https://i.example",
		"--aud", "a",
		"--sub", "s",
		"--exp", "1h",
		"-k", res.PrivPath,
		"-o", out,
	})
	g.Expect(err).To(MatchError(ContainSubstring("refusing to overwrite")))

	data, err := os.ReadFile(out)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(string(data)).To(Equal("old"))
}

func TestSignJWT_MultiKeyJWKSet(t *testing.T) {
	g := NewWithT(t)
	base := t.TempDir()

	// Build a JWK set with two keys.
	mk := func() json.RawMessage {
		_, priv, err := ed25519.GenerateKey(nil)
		g.Expect(err).ToNot(HaveOccurred())
		// Hand-rolled minimal JWK so we don't pull jose here.
		b, err := json.Marshal(map[string]any{
			"kty": "OKP",
			"crv": "Ed25519",
			"alg": "EdDSA",
			"use": "sig",
			"kid": "k",
			"d":   priv.Seed(),
			"x":   priv.Public().(ed25519.PublicKey),
		})
		g.Expect(err).ToNot(HaveOccurred())
		return b
	}
	body, err := json.Marshal(struct {
		Keys []json.RawMessage `json:"keys"`
	}{Keys: []json.RawMessage{mk(), mk()}})
	g.Expect(err).ToNot(HaveOccurred())
	keyPath := filepath.Join(base, "multi.jwks")
	g.Expect(os.WriteFile(keyPath, body, 0o600)).To(Succeed())

	_, err = executeCommand([]string{
		"sign", "jwt",
		"--iss", "https://i.example",
		"--aud", "a",
		"--sub", "s",
		"--exp", "1h",
		"-k", keyPath,
		"-o", filepath.Join(base, "out.jwt"),
	})
	g.Expect(err).To(MatchError(ContainSubstring("exactly one key, got 2")))
}

// stripFlag returns args with the first occurrence of flag and its value removed.
func stripFlag(args []string, flag string) []string {
	out := make([]string, 0, len(args))
	skip := false
	dropped := false
	for _, a := range args {
		if skip {
			skip = false
			continue
		}
		if !dropped && a == flag {
			skip = true
			dropped = true
			continue
		}
		out = append(out, a)
	}
	return out
}
