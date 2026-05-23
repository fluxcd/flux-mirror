// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

// Package generate produces the artifacts that back JWK-based registry auth: an
// EdDSA JWK key pair, and Docker config / Kubernetes pull-secret files holding a
// JWT issued from a private key.
package generate

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	yaml "gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"

	"github.com/fluxcd/pkg/auth/jwt"

	"github.com/fluxcd/flux-mirror/internal/config"
)

const (
	// PubKeyFile and PrivKeyFile are the JWK pair filenames written into a key
	// directory. The from-jwk commands read PrivKeyFile from each host's key dir.
	PubKeyFile  = "pubkey.json"
	PrivKeyFile = "privkey.json"

	// DockerConfigFile and PullSecretFile are the output filenames written next
	// to each host's private key.
	DockerConfigFile = "docker-config.json"
	PullSecretFile   = "pull-secret.yaml"

	pullSecretName = "docker"
	pullSecretNS   = "default"
)

// Result records one host/output-file pair produced by a from-jwk command.
type Result struct {
	Host string
	Path string
}

// JWKPair generates an EdDSA key pair and writes it as pubkey.json and
// privkey.json under dir, creating dir (and parents) if needed. It returns an
// error without writing anything if either file already exists. kid sets the key
// id of both keys; when empty, it defaults to the RFC 7638 JWK thumbprint. The
// kid that was used is returned.
func JWKPair(dir, kid string) (string, error) {
	privPath := filepath.Join(dir, PrivKeyFile)
	pubPath := filepath.Join(dir, PubKeyFile)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create dir: %w", err)
	}
	for _, p := range []string{privPath, pubPath} {
		if _, err := os.Stat(p); err == nil {
			return "", fmt.Errorf("%s already exists", p)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}

	privJWK := jose.JSONWebKey{Key: priv, KeyID: kid, Algorithm: string(jose.EdDSA), Use: "sig"}
	if kid == "" {
		tp, err := privJWK.Thumbprint(crypto.SHA256)
		if err != nil {
			return "", fmt.Errorf("compute thumbprint: %w", err)
		}
		kid = base64.RawURLEncoding.EncodeToString(tp)
		privJWK.KeyID = kid
	}
	pubJWK := jose.JSONWebKey{Key: pub, KeyID: kid, Algorithm: string(jose.EdDSA), Use: "sig"}

	// privkey.json is 0600 (it is a secret); pubkey.json is world-readable.
	if err := writeJSON(privPath, privJWK, 0o600); err != nil {
		return "", err
	}
	if err := writeJSON(pubPath, pubJWK, 0o644); err != nil {
		return "", err
	}
	return kid, nil
}

// DockerConfigFromJWK issues a JWT for every host that uses jwkPath and writes a
// Docker config file (docker-config.json) next to that host's private key, with
// the host as the username and the JWT as the password. Multiple hosts sharing a
// key dir are upserted into the same file in iteration order; an existing file is
// preserved and upserted into.
func DockerConfigFromJWK(cfg *config.Config, exp time.Duration) ([]Result, error) {
	return forEachJWKHost(cfg, exp, DockerConfigFile, func(path, host, token string) error {
		dc, err := upsertDockerConfig(path, host, token)
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(dc, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(path, append(b, '\n'), 0o600)
	})
}

// PullSecretFromJWK behaves like DockerConfigFromJWK but writes a Kubernetes
// dockerconfigjson Secret (pull-secret.yaml, name "docker", namespace "default")
// wrapping the same credentials.
func PullSecretFromJWK(cfg *config.Config, exp time.Duration) ([]Result, error) {
	return forEachJWKHost(cfg, exp, PullSecretFile, func(path, host, token string) error {
		sec, err := upsertPullSecret(path, host, token)
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2) // yaml.v3 defaults to 4; match the 2-space manifest convention.
		if err := enc.Encode(sec); err != nil {
			return err
		}
		if err := enc.Close(); err != nil {
			return err
		}
		return os.WriteFile(path, buf.Bytes(), 0o600)
	})
}

// forEachJWKHost issues a JWT for each jwkPath host and invokes write with the
// output path (outName joined to the host's key dir), the host, and the token.
func forEachJWKHost(cfg *config.Config, exp time.Duration, outName string,
	write func(path, host, token string) error) ([]Result, error) {

	if exp <= 0 {
		return nil, fmt.Errorf("exp must be a positive duration, got %s", exp)
	}

	hosts := jwkHosts(cfg)
	if len(hosts) == 0 {
		return nil, fmt.Errorf("config has no hosts using jwkPath")
	}

	var results []Result
	for _, h := range hosts {
		dir := filepath.Dir(h.JWT.JWKPath)
		token, err := issueForHost(dir, h, exp)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, outName)
		if err := write(path, h.Host, token); err != nil {
			return nil, fmt.Errorf("host %q: write %s: %w", h.Host, path, err)
		}
		results = append(results, Result{Host: h.Host, Path: path})
	}
	return results, nil
}

// issueForHost reads privkey.json from dir and signs a JWT with the host's
// iss/sub/aud and the given lifetime.
func issueForHost(dir string, h config.AuthHost, exp time.Duration) (string, error) {
	privPath := filepath.Join(dir, PrivKeyFile)
	data, err := os.ReadFile(privPath)
	if err != nil {
		return "", fmt.Errorf("host %q: read %s: %w", h.Host, privPath, err)
	}
	key, err := jwt.ParseJWK(string(data))
	if err != nil {
		return "", fmt.Errorf("host %q: %w", h.Host, err)
	}
	token, err := key.Issue(h.JWT.Iss, h.JWT.Sub, h.EffectiveAud(), exp)
	if err != nil {
		return "", fmt.Errorf("host %q: issue JWT: %w", h.Host, err)
	}
	return token, nil
}

// jwkHosts returns the auth hosts that use jwkPath, in config order.
func jwkHosts(cfg *config.Config) []config.AuthHost {
	if cfg.Auth == nil {
		return nil
	}
	var hosts []config.AuthHost
	for _, h := range cfg.Auth.Hosts {
		if h.JWT != nil && strings.TrimSpace(h.JWT.JWKPath) != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

type dockerConfigFile struct {
	Auths map[string]dockerAuthEntry `json:"auths"`
}

type dockerAuthEntry struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Auth     string `json:"auth,omitempty"`
}

func newAuthEntry(user, pass string) dockerAuthEntry {
	return dockerAuthEntry{
		Username: user,
		Password: pass,
		Auth:     base64.StdEncoding.EncodeToString([]byte(user + ":" + pass)),
	}
}

func upsertDockerConfig(path, host, token string) (*dockerConfigFile, error) {
	dc := &dockerConfigFile{Auths: map[string]dockerAuthEntry{}}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, dc); err != nil {
			return nil, fmt.Errorf("parse existing %s: %w", path, err)
		}
		if dc.Auths == nil {
			dc.Auths = map[string]dockerAuthEntry{}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	dc.Auths[host] = newAuthEntry(host, token)
	return dc, nil
}

// pullSecret is marshaled with gopkg.in/yaml.v3, which preserves field order, so
// the fields are declared in the conventional manifest order. The yaml tags are
// required because yaml.v3 ignores json tags; the json tags are kept so the type
// stays usable by JSON-based consumers (e.g. sigs.k8s.io/yaml in tests).
type pullSecret struct {
	APIVersion string            `json:"apiVersion" yaml:"apiVersion"`
	Kind       string            `json:"kind" yaml:"kind"`
	Metadata   secretMetadata    `json:"metadata" yaml:"metadata"`
	Type       string            `json:"type" yaml:"type"`
	StringData map[string]string `json:"stringData" yaml:"stringData"`
}

type secretMetadata struct {
	Name      string `json:"name" yaml:"name"`
	Namespace string `json:"namespace" yaml:"namespace"`
}

func upsertPullSecret(path, host, token string) (*pullSecret, error) {
	dc := &dockerConfigFile{Auths: map[string]dockerAuthEntry{}}
	if b, err := os.ReadFile(path); err == nil {
		var existing pullSecret
		if err := yaml.Unmarshal(b, &existing); err != nil {
			return nil, fmt.Errorf("parse existing %s: %w", path, err)
		}
		if raw := existing.StringData[corev1.DockerConfigJsonKey]; raw != "" {
			if err := json.Unmarshal([]byte(raw), dc); err != nil {
				return nil, fmt.Errorf("parse %s in %s: %w", corev1.DockerConfigJsonKey, path, err)
			}
			if dc.Auths == nil {
				dc.Auths = map[string]dockerAuthEntry{}
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	dc.Auths[host] = newAuthEntry(host, token)
	raw, err := json.MarshalIndent(dc, "", "  ")
	if err != nil {
		return nil, err
	}
	return &pullSecret{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata:   secretMetadata{Name: pullSecretName, Namespace: pullSecretNS},
		Type:       string(corev1.SecretTypeDockerConfigJson),
		StringData: map[string]string{corev1.DockerConfigJsonKey: string(raw)},
	}, nil
}

func writeJSON(path string, v any, perm fs.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), perm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
