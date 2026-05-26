// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package keygen

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/go-jose/go-jose/v4"
)

func TestSig(t *testing.T) {
	t.Run("writes pubkey.json and privkey.json with matching kid", func(t *testing.T) {
		dir := t.TempDir()

		res, err := Sig(dir)
		if err != nil {
			t.Fatalf("Sig: %v", err)
		}
		if res.KeyID == "" {
			t.Fatalf("empty kid")
		}
		if res.PubPath != filepath.Join(dir, PubKeyFile) {
			t.Errorf("PubPath = %q, want %q", res.PubPath, filepath.Join(dir, PubKeyFile))
		}
		if res.PrivPath != filepath.Join(dir, PrivKeyFile) {
			t.Errorf("PrivPath = %q, want %q", res.PrivPath, filepath.Join(dir, PrivKeyFile))
		}

		pub := readJWKS(t, res.PubPath)
		priv := readJWKS(t, res.PrivPath)
		if len(pub.Keys) != 1 || len(priv.Keys) != 1 {
			t.Fatalf("expected 1 key in each set, got pub=%d priv=%d", len(pub.Keys), len(priv.Keys))
		}
		if pub.Keys[0].KeyID != res.KeyID || priv.Keys[0].KeyID != res.KeyID {
			t.Errorf("kid mismatch: pub=%q priv=%q want=%q",
				pub.Keys[0].KeyID, priv.Keys[0].KeyID, res.KeyID)
		}
		for _, jwk := range []jose.JSONWebKey{pub.Keys[0], priv.Keys[0]} {
			if jwk.Algorithm != string(jose.EdDSA) {
				t.Errorf("alg = %q, want EdDSA", jwk.Algorithm)
			}
			if jwk.Use != useSig {
				t.Errorf("use = %q, want sig", jwk.Use)
			}
		}
		if _, ok := pub.Keys[0].Key.(ed25519.PublicKey); !ok {
			t.Errorf("public key is not ed25519.PublicKey")
		}
		if _, ok := priv.Keys[0].Key.(ed25519.PrivateKey); !ok {
			t.Errorf("private key is not ed25519.PrivateKey")
		}

		// Public/private must agree: derived public from priv equals pub.
		privKey := priv.Keys[0].Key.(ed25519.PrivateKey)
		pubKey := pub.Keys[0].Key.(ed25519.PublicKey)
		derived := privKey.Public().(ed25519.PublicKey)
		if string(pubKey) != string(derived) {
			t.Errorf("public key does not match derived public key")
		}
	})

	t.Run("creates missing directories", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "a", "b", "c")
		if _, err := Sig(dir); err != nil {
			t.Fatalf("Sig: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, PubKeyFile)); err != nil {
			t.Errorf("pubkey not written: %v", err)
		}
	})

	t.Run("file permissions", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("unix permission bits not meaningful on windows")
		}
		dir := t.TempDir()
		res, err := Sig(dir)
		if err != nil {
			t.Fatalf("Sig: %v", err)
		}
		privStat, err := os.Stat(res.PrivPath)
		if err != nil {
			t.Fatalf("stat priv: %v", err)
		}
		if perm := privStat.Mode().Perm(); perm != 0o600 {
			t.Errorf("priv perm = %o, want 0600", perm)
		}
		pubStat, err := os.Stat(res.PubPath)
		if err != nil {
			t.Fatalf("stat pub: %v", err)
		}
		if perm := pubStat.Mode().Perm(); perm != 0o644 {
			t.Errorf("pub perm = %o, want 0644", perm)
		}
	})

	t.Run("refuses to overwrite existing private key", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, PrivKeyFile), []byte("existing"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Sig(dir); err == nil {
			t.Fatalf("expected error, got nil")
		}
		// Public key must not have been written either.
		if _, err := os.Stat(filepath.Join(dir, PubKeyFile)); !os.IsNotExist(err) {
			t.Errorf("pubkey.json was written despite error: %v", err)
		}
		// Existing private file must be untouched.
		data, err := os.ReadFile(filepath.Join(dir, PrivKeyFile))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "existing" {
			t.Errorf("privkey.json was overwritten: %q", data)
		}
	})

	t.Run("refuses to overwrite existing public key", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, PubKeyFile), []byte("existing"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Sig(dir); err == nil {
			t.Fatalf("expected error, got nil")
		}
		// Private key must not have been written.
		if _, err := os.Stat(filepath.Join(dir, PrivKeyFile)); !os.IsNotExist(err) {
			t.Errorf("privkey.json was written despite error: %v", err)
		}
	})

	t.Run("empty dir errors", func(t *testing.T) {
		if _, err := Sig(""); err == nil {
			t.Fatalf("expected error, got nil")
		}
	})

	t.Run("generates a distinct kid on each call", func(t *testing.T) {
		a, err := Sig(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		b, err := Sig(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if a.KeyID == b.KeyID {
			t.Errorf("expected different kids, got %q twice", a.KeyID)
		}
	})
}

func readJWKS(t *testing.T, path string) jose.JSONWebKeySet {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(data, &set); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return set
}
