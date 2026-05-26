// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

// Package keygen generates EdDSA JSON Web Key pairs for flux-mirror's
// JWK-based registry auth, and writes them as a public/private JWKS pair.
package keygen

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
)

const (
	// PubKeyFile is the filename of the public JWKS within the output directory.
	PubKeyFile = "pubkey.json"
	// PrivKeyFile is the filename of the private JWKS within the output directory.
	PrivKeyFile = "privkey.json"

	useSig = "sig"
)

// Result describes a successful Sig run.
type Result struct {
	// Dir is the output directory the files were written to.
	Dir string
	// KeyID is the JWK kid used for both keys (UUIDv6).
	KeyID string
	// PubPath is the absolute or dir-relative path to the public JWKS.
	PubPath string
	// PrivPath is the absolute or dir-relative path to the private JWKS.
	PrivPath string
}

// Sig generates an EdDSA Ed25519 key pair and writes the public and private
// JWKS into dir as PubKeyFile and PrivKeyFile.
//
// The directory is created if it does not exist. Both files are written with
// O_EXCL semantics: if either already exists, Sig returns an error without
// modifying anything. Permissions are 0600 for the private file and 0644 for
// the public file. The kid is a UUIDv6 shared by both keys.
func Sig(dir string) (*Result, error) {
	if dir == "" {
		return nil, fmt.Errorf("output directory is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create output directory: %w", err)
	}

	pubPath := filepath.Join(dir, PubKeyFile)
	privPath := filepath.Join(dir, PrivKeyFile)
	for _, p := range []string{pubPath, privPath} {
		if _, err := os.Stat(p); err == nil {
			return nil, fmt.Errorf("%s already exists, refusing to overwrite", p)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat %s: %w", p, err)
		}
	}

	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519 key: %w", err)
	}

	kid, err := uuid.NewV6()
	if err != nil {
		return nil, fmt.Errorf("generate key id: %w", err)
	}
	kidStr := kid.String()

	pubSet := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       pubKey,
		KeyID:     kidStr,
		Algorithm: string(jose.EdDSA),
		Use:       useSig,
	}}}
	privSet := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       privKey,
		KeyID:     kidStr,
		Algorithm: string(jose.EdDSA),
		Use:       useSig,
	}}}

	if err := writeJSON(privPath, &privSet, 0o600); err != nil {
		return nil, fmt.Errorf("write private key set: %w", err)
	}
	if err := writeJSON(pubPath, &pubSet, 0o644); err != nil {
		// Best-effort rollback so a partial run doesn't leave a stray private
		// file behind without its public counterpart.
		_ = os.Remove(privPath)
		return nil, fmt.Errorf("write public key set: %w", err)
	}

	return &Result{
		Dir:      dir,
		KeyID:    kidStr,
		PubPath:  pubPath,
		PrivPath: privPath,
	}, nil
}

func writeJSON(path string, v any, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
