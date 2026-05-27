// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

// Package jwkio reads JWK files in either of the two shapes flux-mirror
// accepts: a bare JSON Web Key, or a JWK Set wrapping exactly one key.
package jwkio

import (
	"encoding/json"
	"fmt"
	"os"
)

// ReadPrivateJWK reads a JWK file used to sign JWTs. The file may be either a
// bare JSON Web Key or a JWK Set wrapping exactly one key. The returned string
// is always a single JWK.
func ReadPrivateJWK(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return parsePrivateJWK(data)
}

func parsePrivateJWK(data []byte) (string, error) {
	var probe struct {
		Keys []json.RawMessage `json:"keys"`
	}
	// A bare JWK has no "keys" field, so probe.Keys stays nil and we return
	// the bytes unchanged. A JWK Set has a "keys" array and we require exactly
	// one entry so the right private key is unambiguous.
	if err := json.Unmarshal(data, &probe); err != nil {
		return "", fmt.Errorf("parse JWK: %w", err)
	}
	if probe.Keys == nil {
		return string(data), nil
	}
	if len(probe.Keys) != 1 {
		return "", fmt.Errorf("JWK set must contain exactly one key, got %d", len(probe.Keys))
	}
	return string(probe.Keys[0]), nil
}
