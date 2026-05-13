// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

// Package testregistry spins up an in-process distribution/v3 registry on an
// ephemeral port for use in tests. Centralizes what would otherwise be
// duplicated TestMain + helper boilerplate across every package that exercises
// real OCI traffic. Pattern follows fluxcd/pkg/oci/suite_test.go.
package testregistry

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/distribution/distribution/v3/configuration"
	dockerregistry "github.com/distribution/distribution/v3/registry"
	_ "github.com/distribution/distribution/v3/registry/auth/htpasswd"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/inmemory"
	"github.com/sirupsen/logrus"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Start launches an in-memory registry bound to localhost on an
// OS-assigned port and returns its host:port address. The registry runs for
// the lifetime of ctx — pass a cancelable context from TestMain.
func Start(ctx context.Context) (string, error) {
	// Bind to an ephemeral port — never hardcode 5000 (taken by macOS
	// Control Center / AirPlay Receiver).
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}
	addr := lis.Addr().String()
	parts := strings.Split(addr, ":")
	port, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return "", fmt.Errorf("parse port from %q: %w", addr, err)
	}
	if err := lis.Close(); err != nil {
		return "", fmt.Errorf("close probe listener: %w", err)
	}

	cfg := &configuration.Configuration{}
	cfg.Log.AccessLog.Disabled = true
	cfg.Log.Level = "error"
	logrus.SetOutput(io.Discard)
	host := fmt.Sprintf("localhost:%d", port)
	cfg.HTTP.Addr = fmt.Sprintf("127.0.0.1:%d", port)
	cfg.HTTP.DrainTimeout = 10 * time.Second
	cfg.Storage = map[string]configuration.Parameters{
		"inmemory": map[string]any{},
		"delete":   map[string]any{"enabled": true},
	}
	reg, err := dockerregistry.NewRegistry(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("new registry: %w", err)
	}
	go func() { _ = reg.ListenAndServe() }()
	return host, nil
}

var letterRunes = []rune("abcdefghijklmnopqrstuvwxyz1234567890")

// RandSuffix returns a 6-rune lowercase-alnum string. Use to give each test
// its own repository namespace inside the shared registry.
func RandSuffix() string {
	b := make([]rune, 6)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}

// Repo builds a unique repository path inside the registry at addr.
func Repo(addr, stem string) string {
	return fmt.Sprintf("%s/%s-%s", addr, stem, RandSuffix())
}

// PushImage pushes a small random image to ref over plaintext HTTP and
// returns the resulting digest.
func PushImage(t testing.TB, ref string) string {
	t.Helper()
	img, err := random.Image(128, 1)
	if err != nil {
		t.Fatalf("random.Image: %s", err)
	}
	if err := crane.Push(img, ref, crane.Insecure); err != nil {
		t.Fatalf("push %s: %s", ref, err)
	}
	dig, err := crane.Digest(ref, crane.Insecure)
	if err != nil {
		t.Fatalf("digest %s: %s", ref, err)
	}
	return dig
}

// PushIndex pushes a small random multi-arch image index to ref and returns
// the manifest-list digest.
func PushIndex(t testing.TB, ref string) string {
	t.Helper()
	idx, err := random.Index(256, 1, 3)
	if err != nil {
		t.Fatalf("random.Index: %s", err)
	}
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatalf("parse %s: %s", ref, err)
	}
	if err := remote.Push(parsed, idx); err != nil {
		t.Fatalf("push index %s: %s", ref, err)
	}
	dig, err := crane.Digest(ref, crane.Insecure)
	if err != nil {
		t.Fatalf("digest %s: %s", ref, err)
	}
	return dig
}

// PushReferrer pushes a manifest with a `subject` field pointing at subject,
// of the given artifact type, into the repo at repoAddr (e.g. "host/repo").
// distribution/v3 indexes the result via the OCI 1.1 referrers API.
func PushReferrer(t testing.TB, repoAddr string, subject v1.Descriptor, artifactType string) v1.Hash {
	t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatalf("random.Image: %s", err)
	}
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.MediaType(artifactType))
	img = mutate.Subject(img, subject).(v1.Image)
	dig, err := img.Digest()
	if err != nil {
		t.Fatalf("digest referrer: %s", err)
	}
	ref, err := name.NewDigest(repoAddr+"@"+dig.String(), name.Insecure)
	if err != nil {
		t.Fatalf("parse referrer ref: %s", err)
	}
	if err := remote.Push(ref, img); err != nil {
		t.Fatalf("push referrer %s: %s", ref, err)
	}
	return dig
}
