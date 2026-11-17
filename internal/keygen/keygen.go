// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

// Package keygen generates JSON Web Key pairs for flux-mirror's JWK-based
// registry auth, and writes them as a public/private JWKS pair.
package keygen

import (
	"crypto/ed25519"
	"crypto/mldsa"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// Algorithms lists the signing algorithms a key pair can be generated for, in
// the order the keygen command documents them. Values are the JOSE "alg" names
// and are written verbatim into the JWK.
var Algorithms = []jose.SignatureAlgorithm{
	jose.EdDSA,
	jose.MLDSA44,
	jose.MLDSA65,
	jose.MLDSA87,
}

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

// Sig generates a key pair for alg and writes the public and private JWKS into
// dir as PubKeyFile and PrivKeyFile.
//
// alg selects the signing algorithm: EdDSA (Ed25519), or ML-DSA-44/65/87 as
// specified in FIPS 204 and RFC 9964. ML-DSA keys are written as RFC 9964 "AKP"
// keys, whose "alg" member identifies the parameter set, and the private key is
// stored as the 32-byte seed.
//
// The directory is created if it does not exist. Both files are written with
// O_EXCL semantics: if either already exists, Sig returns an error without
// modifying anything. Permissions are 0600 for the private file and 0644 for
// the public file. The kid is a UUIDv6 shared by both keys.
func Sig(dir string, alg jose.SignatureAlgorithm) (*Result, error) {
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

	pubKey, privKey, err := generate(alg)
	if err != nil {
		return nil, err
	}

	kid, err := uuid.NewV6()
	if err != nil {
		return nil, fmt.Errorf("generate key id: %w", err)
	}
	kidStr := kid.String()

	pubSet := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       pubKey,
		KeyID:     kidStr,
		Algorithm: string(alg),
		Use:       useSig,
	}}}
	privSet := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       privKey,
		KeyID:     kidStr,
		Algorithm: string(alg),
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

// generate returns the public and private halves of a fresh key pair for alg.
func generate(alg jose.SignatureAlgorithm) (pub, priv any, err error) {
	switch alg {
	case jose.EdDSA:
		p, k, gerr := ed25519.GenerateKey(rand.Reader)
		if gerr != nil {
			return nil, nil, fmt.Errorf("generate Ed25519 key: %w", gerr)
		}
		return p, k, nil
	case jose.MLDSA44, jose.MLDSA65, jose.MLDSA87:
		k, gerr := mldsa.GenerateKey(mldsaParams(alg))
		if gerr != nil {
			return nil, nil, fmt.Errorf("generate %s key: %w", alg, gerr)
		}
		return k.Public(), k, nil
	default:
		return nil, nil, fmt.Errorf("unsupported algorithm %q: want one of %s",
			alg, SupportedAlgorithms())
	}
}

// mldsaParams maps a JOSE algorithm to its FIPS 204 parameter set.
func mldsaParams(alg jose.SignatureAlgorithm) mldsa.Parameters {
	switch alg {
	case jose.MLDSA44:
		return mldsa.MLDSA44()
	case jose.MLDSA65:
		return mldsa.MLDSA65()
	default:
		return mldsa.MLDSA87()
	}
}

// SupportedAlgorithms returns the accepted --alg values as a comma-separated
// list.
func SupportedAlgorithms() string {
	names := make([]string, len(Algorithms))
	for i, alg := range Algorithms {
		names[i] = string(alg)
	}
	return strings.Join(names, ", ")
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
