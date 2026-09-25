// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

// Package envelope renders an optional credential envelope: a Go template
// applied to a resolved registry credential to adapt it to the exact string the
// registry expects. The template data is a single Token field and the functions
// base64 and hex are available.
package envelope

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"text/template"
)

// Data is the template data exposed to an envelope template.
type Data struct {
	// Token is the resolved credential the envelope wraps.
	Token string
}

// funcs are the functions available to an envelope template.
var funcs = template.FuncMap{
	"base64": func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) },
	"hex":    func(s string) string { return hex.EncodeToString([]byte(s)) },
}

// Parse compiles an envelope template. An empty template is valid and returns a
// nil template, which Render treats as "send the credential unchanged".
func Parse(text string) (*template.Template, error) {
	if text == "" {
		return nil, nil
	}
	t, err := template.New("envelope").Funcs(funcs).Parse(text)
	if err != nil {
		return nil, fmt.Errorf("parse envelope: %w", err)
	}
	return t, nil
}

// Render applies t to token. A nil template returns token unchanged.
func Render(t *template.Template, token string) (string, error) {
	if t == nil {
		return token, nil
	}
	var b strings.Builder
	if err := t.Execute(&b, Data{Token: token}); err != nil {
		return "", fmt.Errorf("render envelope: %w", err)
	}
	return b.String(), nil
}

// Apply parses and renders text against token. An empty text returns token
// unchanged.
func Apply(text, token string) (string, error) {
	t, err := Parse(text)
	if err != nil {
		return "", err
	}
	return Render(t, token)
}

// Validate reports whether text is a usable envelope template. It parses the
// template and executes it against a sample token, so unknown fields and bad
// function calls are rejected up front. An empty text is valid.
func Validate(text string) error {
	_, err := Apply(text, "sample-token")
	return err
}
