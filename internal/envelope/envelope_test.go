// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package envelope

import (
	"encoding/base64"
	"encoding/hex"
	"testing"

	. "github.com/onsi/gomega"
)

func TestApply(t *testing.T) {
	tests := []struct {
		name  string
		tmpl  string
		token string
		want  string
	}{
		{name: "empty is a no-op", tmpl: "", token: "secret", want: "secret"},
		{name: "literal only", tmpl: "static", token: "secret", want: "static"},
		{name: "prefix and hex", tmpl: "token-{{ hex .Token }}", token: "abc", want: "token-" + hex.EncodeToString([]byte("abc"))},
		{name: "base64", tmpl: "{{ base64 .Token }}", token: "abc", want: base64.StdEncoding.EncodeToString([]byte("abc"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			got, err := Apply(tt.tmpl, tt.token)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(got).To(Equal(tt.want))
		})
	}
}

func TestParse_Errors(t *testing.T) {
	tests := []struct {
		name string
		tmpl string
	}{
		{name: "unclosed action", tmpl: "token-{{ hex .Token"},
		{name: "unknown function", tmpl: "{{ nope .Token }}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			_, err := Parse(tt.tmpl)
			g.Expect(err).To(HaveOccurred())
		})
	}
}

func TestValidate_UnknownField(t *testing.T) {
	g := NewWithT(t)
	g.Expect(Validate("{{ .Nope }}")).To(MatchError(ContainSubstring("render envelope")))
	g.Expect(Validate("token-{{ hex .Token }}")).To(Succeed())
	g.Expect(Validate("")).To(Succeed())
}
