// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ChunkingTransport intercepts blob-upload PATCH requests whose body
// is larger than ChunkSize and replays them as a sequence of OCI
// dist-spec chunked PATCHes (Content-Range: <start>-<end>). Anything
// else passes through. The final chunk's response is returned so
// go-containerregistry's commit step uses the correct upload Location.
//
// go-containerregistry sends one monolithic PATCH per blob and exposes
// no chunking knob, so wrapping at the HTTP layer is the only seam
// that doesn't fork the library.
type ChunkingTransport struct {
	Inner     http.RoundTripper
	ChunkSize int64
}

func (t *ChunkingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.ChunkSize <= 0 ||
		req.Method != http.MethodPatch ||
		!isBlobUploadPath(req.URL.Path) ||
		req.ContentLength <= 0 ||
		req.ContentLength <= t.ChunkSize {
		return t.Inner.RoundTrip(req)
	}
	return t.chunkedUpload(req)
}

func (t *ChunkingTransport) chunkedUpload(req *http.Request) (*http.Response, error) {
	if req.Body == nil {
		return nil, errors.New("oci.ChunkingTransport: PATCH with nil body")
	}
	body := req.Body
	defer body.Close()

	total := req.ContentLength
	uploadURL := req.URL.String()
	var lastResp *http.Response

	for offset := int64(0); offset < total; {
		remaining := total - offset
		size := t.ChunkSize
		if remaining < size {
			size = remaining
		}

		// PATCH bodies need a known length and must be replayable for
		// retries. The library hands us one reader for the original
		// monolithic PATCH; we own framing for each chunk.
		buf := make([]byte, size)
		if _, err := io.ReadFull(body, buf); err != nil {
			return nil, fmt.Errorf("read chunk at offset %d: %w", offset, err)
		}

		chunkReq, err := http.NewRequestWithContext(req.Context(), http.MethodPatch, uploadURL, bytes.NewReader(buf))
		if err != nil {
			return nil, err
		}
		copyHeader(chunkReq.Header, req.Header, "Authorization", "Content-Type", "User-Agent", "Accept")
		if chunkReq.Header.Get("Content-Type") == "" {
			chunkReq.Header.Set("Content-Type", "application/octet-stream")
		}
		chunkReq.Header.Set("Content-Range", fmt.Sprintf("%d-%d", offset, offset+size-1))
		chunkReq.ContentLength = size

		resp, err := t.Inner.RoundTrip(chunkReq)
		if err != nil {
			return nil, fmt.Errorf("PATCH chunk %d-%d: %w", offset, offset+size-1, err)
		}
		if resp.StatusCode != http.StatusAccepted {
			return resp, nil
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		if loc := resp.Header.Get("Location"); loc != "" {
			next, err := resolveLocation(uploadURL, loc)
			if err != nil {
				return nil, fmt.Errorf("resolve next Location %q: %w", loc, err)
			}
			uploadURL = next
		}
		offset += size
		lastResp = resp
	}

	if lastResp == nil {
		return nil, errors.New("oci.ChunkingTransport: zero-length PATCH")
	}
	lastResp.Body = io.NopCloser(strings.NewReader(""))
	if lastResp.Header.Get("Location") == "" {
		lastResp.Header.Set("Location", uploadURL)
	}
	return lastResp, nil
}

// JWTBearerTransport stamps Authorization: Bearer <jwt> on outbound
// requests whose URL host matches one of Hosts, where <jwt> is
// fetched from a generic endpoint configured at construction:
//
//	GET  <RequestURL>?audience=<Audience>
//	     Authorization: Bearer <RequestToken>
//	→ {"value": "<jwt>"}
//
// The token is mutex-cached for the FIRST 50% of its remaining JWT
// lifetime (read from the exp claim) and reminted on demand. Any
// upstream Authorization header on a matched request is overwritten;
// requests to other hosts pass through untouched (so the keychain's
// auth for source registries still works during a sync).
//
// Hosts must be non-empty — pass the destination registry hostname(s)
// the JWT is intended for. A typical setup uses Hosts = [Audience].
type JWTBearerTransport struct {
	Inner        http.RoundTripper
	RequestURL   string
	RequestToken string
	Audience     string
	Hosts        []string

	mu    sync.Mutex
	token string
	exp   time.Time
}

func (t *JWTBearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.matchesHost(req.URL.Host) {
		return t.Inner.RoundTrip(req)
	}
	tok, err := t.fetchToken(req.Context())
	if err != nil {
		return nil, fmt.Errorf("mint JWT bearer: %w", err)
	}
	// Don't mutate the caller's request — clone so the Authorization
	// edit is request-scoped.
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+tok)
	return t.Inner.RoundTrip(cloned)
}

func (t *JWTBearerTransport) matchesHost(host string) bool {
	for _, h := range t.Hosts {
		if h == host {
			return true
		}
	}
	return false
}

func (t *JWTBearerTransport) fetchToken(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.token != "" && now.Before(t.exp) {
		return t.token, nil
	}
	u, err := url.Parse(t.RequestURL)
	if err != nil {
		return "", fmt.Errorf("parse request URL: %w", err)
	}
	q := u.Query()
	q.Set("audience", t.Audience)
	u.RawQuery = q.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Authorization", "Bearer "+t.RequestToken)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if out.Value == "" {
		return "", errors.New("empty token in response")
	}
	exp, err := jwtExp(out.Value)
	if err != nil {
		return "", err
	}
	half := exp.Sub(now) / 2
	if half < time.Second {
		return "", fmt.Errorf("minted JWT already near-expired (exp=%s)", exp)
	}
	t.token = out.Value
	t.exp = now.Add(half)
	return out.Value, nil
}

// jwtExp pulls the exp claim out of a compact-serialized JWT without
// verifying the signature — this token came from a trusted endpoint
// one HTTP call ago and the only thing we need from it is the
// expiration for cache scheduling.
func jwtExp(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("malformed JWT (expected 3 parts)")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return time.Time{}, fmt.Errorf("parse JWT claims: %w", err)
	}
	if claims.Exp == 0 {
		return time.Time{}, errors.New("JWT has no exp claim")
	}
	return time.Unix(claims.Exp, 0).UTC(), nil
}

func isBlobUploadPath(p string) bool {
	if !strings.HasPrefix(p, "/v2/") {
		return false
	}
	return strings.Contains(p, "/blobs/uploads/")
}

func copyHeader(dst, src http.Header, keys ...string) {
	for _, k := range keys {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}

func resolveLocation(base, loc string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(loc)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(u).String(), nil
}
