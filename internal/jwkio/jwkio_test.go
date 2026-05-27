// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package jwkio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadPrivateJWK(t *testing.T) {
	bareJWK := `{"kty":"OKP","crv":"Ed25519","kid":"k","x":"AAA","d":"BBB"}`
	cases := []struct {
		name   string
		body   string
		want   string // empty means expect error
		errSub string
	}{
		{
			name: "bare JWK passes through unchanged",
			body: bareJWK,
			want: bareJWK,
		},
		{
			name: "single-key set is unwrapped",
			body: `{"keys":[` + bareJWK + `]}`,
			want: bareJWK,
		},
		{
			name:   "empty set errors",
			body:   `{"keys":[]}`,
			errSub: "exactly one key, got 0",
		},
		{
			name:   "multi-key set errors",
			body:   `{"keys":[` + bareJWK + `,` + bareJWK + `]}`,
			errSub: "exactly one key, got 2",
		},
		{
			name:   "malformed JSON errors",
			body:   `not json`,
			errSub: "parse JWK",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "k.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := ReadPrivateJWK(path)
			if tc.errSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errSub) {
					t.Fatalf("err = %v, want substring %q", err, tc.errSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadPrivateJWK: %v", err)
			}
			if strings.TrimSpace(got) != strings.TrimSpace(tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("missing file errors", func(t *testing.T) {
		_, err := ReadPrivateJWK(filepath.Join(t.TempDir(), "absent.json"))
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}
