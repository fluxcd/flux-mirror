// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

// ---------- ChunkingTransport ----------

// fakeRT records every request it sees and replies with an optional
// per-request override; defaults to 202 Accepted with the request URL
// echoed back as Location so the chunking loop can exercise the
// "follow Location" path.
type fakeRT struct {
	calls    []recordedReq
	override func(*http.Request) *http.Response
}

type recordedReq struct {
	method      string
	url         string
	contentLen  int64
	body        []byte
	contentType string
	auth        string
	contentRng  string
}

func (f *fakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	_ = req.Body.Close()
	f.calls = append(f.calls, recordedReq{
		method:      req.Method,
		url:         req.URL.String(),
		contentLen:  req.ContentLength,
		body:        body,
		contentType: req.Header.Get("Content-Type"),
		auth:        req.Header.Get("Authorization"),
		contentRng:  req.Header.Get("Content-Range"),
	})
	if f.override != nil {
		if r := f.override(req); r != nil {
			return r, nil
		}
	}
	h := http.Header{}
	h.Set("Location", req.URL.String())
	return &http.Response{
		StatusCode: http.StatusAccepted,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader("")),
	}, nil
}

func newPATCH(t *testing.T, url string, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPatch, url, strings.NewReader(string(body)))
	NewWithT(t).Expect(err).ToNot(HaveOccurred())
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer abc.def.ghi")
	req.ContentLength = int64(len(body))
	return req
}

func TestChunkingTransport_PassThrough(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		path       string
		bodyLen    int
		chunkSize  int64
		wantChunks int
	}{
		{name: "chunk_size_zero", method: "PATCH", path: "/v2/repo/blobs/uploads/abc", bodyLen: 1024, chunkSize: 0, wantChunks: 1},
		{name: "non_patch_method", method: "POST", path: "/v2/repo/blobs/uploads/abc", bodyLen: 1024, chunkSize: 100, wantChunks: 1},
		{name: "non_blob_upload_path", method: "PATCH", path: "/v2/repo/manifests/latest", bodyLen: 1024, chunkSize: 100, wantChunks: 1},
		{name: "body_smaller_than_chunk", method: "PATCH", path: "/v2/repo/blobs/uploads/abc", bodyLen: 50, chunkSize: 100, wantChunks: 1},
		{name: "body_equal_to_chunk", method: "PATCH", path: "/v2/repo/blobs/uploads/abc", bodyLen: 100, chunkSize: 100, wantChunks: 1},
		{name: "v1_path_ignored", method: "PATCH", path: "/v1/repo/blobs/uploads/abc", bodyLen: 1024, chunkSize: 100, wantChunks: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			body := make([]byte, tc.bodyLen)
			for i := range body {
				body[i] = byte(i)
			}
			fake := &fakeRT{}
			tr := &ChunkingTransport{Inner: fake, ChunkSize: tc.chunkSize}
			req, _ := http.NewRequest(tc.method, "https://example.test"+tc.path, strings.NewReader(string(body)))
			req.ContentLength = int64(tc.bodyLen)
			resp, err := tr.RoundTrip(req)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(resp.StatusCode).To(Equal(http.StatusAccepted))
			g.Expect(fake.calls).To(HaveLen(tc.wantChunks))
			g.Expect(fake.calls[0].body).To(Equal(body))
		})
	}
}

func TestChunkingTransport_SplitsAtChunkBoundary(t *testing.T) {
	g := NewWithT(t)
	body := make([]byte, 250)
	for i := range body {
		body[i] = byte(i)
	}
	fake := &fakeRT{}
	tr := &ChunkingTransport{Inner: fake, ChunkSize: 100}
	resp, err := tr.RoundTrip(newPATCH(t, "https://example.test/v2/repo/blobs/uploads/abc", body))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(resp.StatusCode).To(Equal(http.StatusAccepted))

	// Three chunks: 0-99, 100-199, 200-249.
	g.Expect(fake.calls).To(HaveLen(3))
	g.Expect(fake.calls[0].contentRng).To(Equal("0-99"))
	g.Expect(fake.calls[0].contentLen).To(Equal(int64(100)))
	g.Expect(fake.calls[0].body).To(Equal(body[:100]))
	g.Expect(fake.calls[1].contentRng).To(Equal("100-199"))
	g.Expect(fake.calls[1].body).To(Equal(body[100:200]))
	g.Expect(fake.calls[2].contentRng).To(Equal("200-249"))
	g.Expect(fake.calls[2].contentLen).To(Equal(int64(50)))
	g.Expect(fake.calls[2].body).To(Equal(body[200:]))
}

func TestChunkingTransport_ForwardsHeaders(t *testing.T) {
	g := NewWithT(t)
	body := make([]byte, 200)
	fake := &fakeRT{}
	tr := &ChunkingTransport{Inner: fake, ChunkSize: 100}
	_, err := tr.RoundTrip(newPATCH(t, "https://example.test/v2/repo/blobs/uploads/abc", body))
	g.Expect(err).ToNot(HaveOccurred())
	for _, c := range fake.calls {
		g.Expect(c.auth).To(Equal("Bearer abc.def.ghi"))
		g.Expect(c.contentType).To(Equal("application/octet-stream"))
	}
}

func TestChunkingTransport_DefaultsContentType(t *testing.T) {
	g := NewWithT(t)
	body := make([]byte, 200)
	fake := &fakeRT{}
	tr := &ChunkingTransport{Inner: fake, ChunkSize: 100}
	req, _ := http.NewRequest(http.MethodPatch, "https://example.test/v2/repo/blobs/uploads/abc", strings.NewReader(string(body)))
	req.ContentLength = int64(len(body))
	// No Content-Type set on caller; ChunkingTransport must default.
	_, err := tr.RoundTrip(req)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(fake.calls[0].contentType).To(Equal("application/octet-stream"))
}

func TestChunkingTransport_HonorsLocationFromPriorChunk(t *testing.T) {
	g := NewWithT(t)
	body := make([]byte, 250)
	var idx atomic.Int32
	fake := &fakeRT{
		override: func(req *http.Request) *http.Response {
			i := int(idx.Add(1))
			h := http.Header{}
			// Each chunk's response sends a different Location to verify
			// we follow it for the next chunk.
			h.Set("Location", fmt.Sprintf("/v2/repo/blobs/uploads/session-%d", i))
			return &http.Response{
				StatusCode: http.StatusAccepted,
				Header:     h,
				Body:       io.NopCloser(strings.NewReader("")),
			}
		},
	}
	tr := &ChunkingTransport{Inner: fake, ChunkSize: 100}
	resp, err := tr.RoundTrip(newPATCH(t, "https://example.test/v2/repo/blobs/uploads/start", body))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(resp.StatusCode).To(Equal(http.StatusAccepted))

	// Each chunk's request URL is the prior chunk's Location, resolved
	// against the original (absolute) upload URL.
	g.Expect(fake.calls[0].url).To(Equal("https://example.test/v2/repo/blobs/uploads/start"))
	g.Expect(fake.calls[1].url).To(Equal("https://example.test/v2/repo/blobs/uploads/session-1"))
	g.Expect(fake.calls[2].url).To(Equal("https://example.test/v2/repo/blobs/uploads/session-2"))
}

func TestChunkingTransport_AbortsOnNon202(t *testing.T) {
	g := NewWithT(t)
	body := make([]byte, 250)
	var idx atomic.Int32
	fake := &fakeRT{
		override: func(req *http.Request) *http.Response {
			if idx.Add(1) == 2 {
				return &http.Response{
					StatusCode: http.StatusInternalServerError,
					Header:     http.Header{},
					Body:       io.NopCloser(strings.NewReader("nope")),
				}
			}
			h := http.Header{}
			h.Set("Location", req.URL.String())
			return &http.Response{StatusCode: http.StatusAccepted, Header: h, Body: io.NopCloser(strings.NewReader(""))}
		},
	}
	tr := &ChunkingTransport{Inner: fake, ChunkSize: 100}
	resp, err := tr.RoundTrip(newPATCH(t, "https://example.test/v2/repo/blobs/uploads/abc", body))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(resp.StatusCode).To(Equal(http.StatusInternalServerError))
	// Stops mid-stream — third chunk is never sent.
	g.Expect(fake.calls).To(HaveLen(2))
}

func TestChunkingTransport_NilBody(t *testing.T) {
	g := NewWithT(t)
	tr := &ChunkingTransport{Inner: &fakeRT{}, ChunkSize: 100}
	req, _ := http.NewRequest(http.MethodPatch, "https://example.test/v2/repo/blobs/uploads/abc", nil)
	req.ContentLength = 200
	_, err := tr.RoundTrip(req)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("nil body"))
}

// ---------- JWTBearerTransport ----------

// makeJWT returns a compact-serialized JWT with the given exp claim.
// Header + signature are stub bytes; the transport doesn't verify them.
func makeJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	header := enc([]byte(`{"alg":"none","typ":"JWT"}`))
	body, _ := json.Marshal(map[string]any{"exp": exp.Unix()})
	return header + "." + enc(body) + "." + enc([]byte("sig"))
}

// jwtServer issues a fresh JWT with the configured TTL on each call,
// counting requests so tests can assert cache behavior.
type jwtServer struct {
	server   *httptest.Server
	calls    atomic.Int32
	ttl      time.Duration
	wantAuth string
	wantAud  string
	t        *testing.T
}

func newJWTServer(t *testing.T, ttl time.Duration) *jwtServer {
	js := &jwtServer{ttl: ttl, wantAuth: "Bearer github-oidc-runtime-token", wantAud: "registry.example.test", t: t}
	js.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		js.calls.Add(1)
		g := NewWithT(t)
		g.Expect(r.Method).To(Equal(http.MethodGet))
		g.Expect(r.Header.Get("Authorization")).To(Equal(js.wantAuth))
		g.Expect(r.URL.Query().Get("audience")).To(Equal(js.wantAud))
		jwt := makeJWT(t, time.Now().Add(js.ttl))
		_ = json.NewEncoder(w).Encode(map[string]string{"value": jwt})
	}))
	t.Cleanup(js.server.Close)
	return js
}

// recordingRT lets us assert what made it past the auth-stamping step.
type recordingRT struct {
	headers []string
	hosts   []string
}

func (r *recordingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r.headers = append(r.headers, req.Header.Get("Authorization"))
	r.hosts = append(r.hosts, req.URL.Host)
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
}

func TestJWTBearerTransport_PassThroughOtherHosts(t *testing.T) {
	g := NewWithT(t)
	js := newJWTServer(t, time.Hour)
	rec := &recordingRT{}
	tr := &JWTBearerTransport{
		Inner: rec, RequestURL: js.server.URL, RequestToken: "github-oidc-runtime-token",
		Audience: "registry.example.test", Hosts: []string{"registry.example.test"},
	}
	req, _ := http.NewRequest(http.MethodGet, "https://other.example.test/v2/", nil)
	req.Header.Set("Authorization", "Basic abc")
	_, err := tr.RoundTrip(req)
	g.Expect(err).ToNot(HaveOccurred())
	// Untouched: did NOT mint, did NOT rewrite Authorization.
	g.Expect(js.calls.Load()).To(Equal(int32(0)))
	g.Expect(rec.headers).To(Equal([]string{"Basic abc"}))
}

func TestJWTBearerTransport_StampsBearerOnMatchingHost(t *testing.T) {
	g := NewWithT(t)
	js := newJWTServer(t, time.Hour)
	rec := &recordingRT{}
	tr := &JWTBearerTransport{
		Inner: rec, RequestURL: js.server.URL, RequestToken: "github-oidc-runtime-token",
		Audience: "registry.example.test", Hosts: []string{"registry.example.test"},
	}
	req, _ := http.NewRequest(http.MethodGet, "https://registry.example.test/v2/", nil)
	req.Header.Set("Authorization", "Basic should-be-overwritten")
	_, err := tr.RoundTrip(req)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(js.calls.Load()).To(Equal(int32(1)))
	g.Expect(rec.headers).To(HaveLen(1))
	g.Expect(rec.headers[0]).To(HavePrefix("Bearer "))
	g.Expect(rec.headers[0]).ToNot(ContainSubstring("Basic"))
}

func TestJWTBearerTransport_DoesNotMutateCallerRequest(t *testing.T) {
	g := NewWithT(t)
	js := newJWTServer(t, time.Hour)
	tr := &JWTBearerTransport{
		Inner: &recordingRT{}, RequestURL: js.server.URL, RequestToken: "github-oidc-runtime-token",
		Audience: "registry.example.test", Hosts: []string{"registry.example.test"},
	}
	req, _ := http.NewRequest(http.MethodGet, "https://registry.example.test/v2/", nil)
	req.Header.Set("Authorization", "Basic original")
	_, err := tr.RoundTrip(req)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(req.Header.Get("Authorization")).To(Equal("Basic original"))
}

func TestJWTBearerTransport_CachesTokenWithinHalfLife(t *testing.T) {
	g := NewWithT(t)
	js := newJWTServer(t, time.Hour)
	rec := &recordingRT{}
	tr := &JWTBearerTransport{
		Inner: rec, RequestURL: js.server.URL, RequestToken: "github-oidc-runtime-token",
		Audience: "registry.example.test", Hosts: []string{"registry.example.test"},
	}
	for range 5 {
		req, _ := http.NewRequest(http.MethodGet, "https://registry.example.test/v2/", nil)
		_, err := tr.RoundTrip(req)
		g.Expect(err).ToNot(HaveOccurred())
	}
	g.Expect(js.calls.Load()).To(Equal(int32(1)))
	for _, h := range rec.headers {
		g.Expect(h).To(Equal(rec.headers[0]))
	}
}

func TestJWTBearerTransport_RemintsAfterHalfLifeExpiry(t *testing.T) {
	g := NewWithT(t)
	// 4s TTL → cache for first ~2s (well above the 1s near-expiry guard
	// even with mint latency). Sleep 2.5s and the next call must remint.
	js := newJWTServer(t, 4*time.Second)
	tr := &JWTBearerTransport{
		Inner: &recordingRT{}, RequestURL: js.server.URL, RequestToken: "github-oidc-runtime-token",
		Audience: "registry.example.test", Hosts: []string{"registry.example.test"},
	}
	req1, _ := http.NewRequest(http.MethodGet, "https://registry.example.test/v2/", nil)
	_, err := tr.RoundTrip(req1)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(js.calls.Load()).To(Equal(int32(1)))

	time.Sleep(2500 * time.Millisecond)

	req2, _ := http.NewRequest(http.MethodGet, "https://registry.example.test/v2/", nil)
	_, err = tr.RoundTrip(req2)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(js.calls.Load()).To(Equal(int32(2)))
}

func TestJWTBearerTransport_ErrorsOnMintFailure(t *testing.T) {
	g := NewWithT(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	tr := &JWTBearerTransport{
		Inner: &recordingRT{}, RequestURL: srv.URL, RequestToken: "x",
		Audience: "registry.example.test", Hosts: []string{"registry.example.test"},
	}
	req, _ := http.NewRequest(http.MethodGet, "https://registry.example.test/v2/", nil)
	_, err := tr.RoundTrip(req)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("status 403"))
}

func TestJWTBearerTransport_ErrorsOnEmptyTokenInResponse(t *testing.T) {
	g := NewWithT(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"value":""}`))
	}))
	t.Cleanup(srv.Close)
	tr := &JWTBearerTransport{
		Inner: &recordingRT{}, RequestURL: srv.URL, RequestToken: "x",
		Audience: "registry.example.test", Hosts: []string{"registry.example.test"},
	}
	req, _ := http.NewRequest(http.MethodGet, "https://registry.example.test/v2/", nil)
	_, err := tr.RoundTrip(req)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("empty token"))
}

func TestJWTBearerTransport_ErrorsOnNearExpiredJWT(t *testing.T) {
	g := NewWithT(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// exp in 500ms → half-life 250ms → < 1s threshold → reject.
		jwt := makeJWT(t, time.Now().Add(500*time.Millisecond))
		_, _ = fmt.Fprintf(w, `{"value":%q}`, jwt)
	}))
	t.Cleanup(srv.Close)
	tr := &JWTBearerTransport{
		Inner: &recordingRT{}, RequestURL: srv.URL, RequestToken: "x",
		Audience: "registry.example.test", Hosts: []string{"registry.example.test"},
	}
	req, _ := http.NewRequest(http.MethodGet, "https://registry.example.test/v2/", nil)
	_, err := tr.RoundTrip(req)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("near-expired"))
}

func TestJWTBearerTransport_MatchesAnyOfMultipleHosts(t *testing.T) {
	g := NewWithT(t)
	js := newJWTServer(t, time.Hour)
	rec := &recordingRT{}
	tr := &JWTBearerTransport{
		Inner: rec, RequestURL: js.server.URL, RequestToken: "github-oidc-runtime-token",
		Audience: "registry.example.test",
		Hosts:    []string{"registry.example.test", "alt.example.test"},
	}
	for _, host := range []string{"registry.example.test", "alt.example.test"} {
		req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/v2/", nil)
		_, err := tr.RoundTrip(req)
		g.Expect(err).ToNot(HaveOccurred())
	}
	for _, h := range rec.headers {
		g.Expect(h).To(HavePrefix("Bearer "))
	}
}

// ---------- helpers ----------

func TestJwtExp(t *testing.T) {
	g := NewWithT(t)
	want := time.Now().Add(2 * time.Hour).Truncate(time.Second).UTC()
	got, err := jwtExp(makeJWT(t, want))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(got).To(Equal(want))

	cases := []struct {
		name, jwt, want string
	}{
		{"only_two_parts", "a.b", "expected 3 parts"},
		{"non_base64_payload", "a.@@@.c", "decode JWT payload"},
		{"non_json_payload", "a." + base64.RawURLEncoding.EncodeToString([]byte("not-json")) + ".c", "parse JWT claims"},
		{"missing_exp_claim", "a." + base64.RawURLEncoding.EncodeToString([]byte(`{}`)) + ".c", "no exp claim"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			_, err := jwtExp(tc.jwt)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.want))
		})
	}
}

func TestIsBlobUploadPath(t *testing.T) {
	g := NewWithT(t)
	cases := map[string]bool{
		"/v2/foo/blobs/uploads/abc":  true,
		"/v2/foo/bar/blobs/uploads/": true,
		"/v2/foo/blobs/sha256:abc":   false,
		"/v2/foo/manifests/latest":   false,
		"/v1/foo/blobs/uploads/abc":  false,
		"/blobs/uploads/abc":         false,
		"":                           false,
	}
	for path, want := range cases {
		g.Expect(isBlobUploadPath(path)).To(Equal(want), "path %q", path)
	}
}

func TestCopyHeader(t *testing.T) {
	g := NewWithT(t)
	src := http.Header{}
	src.Set("Authorization", "Bearer x")
	src.Set("Content-Type", "application/octet-stream")
	src.Set("X-Skip", "yes")
	dst := http.Header{}
	copyHeader(dst, src, "Authorization", "Content-Type", "Missing")
	g.Expect(dst.Get("Authorization")).To(Equal("Bearer x"))
	g.Expect(dst.Get("Content-Type")).To(Equal("application/octet-stream"))
	g.Expect(dst.Get("X-Skip")).To(BeEmpty())
	g.Expect(dst.Get("Missing")).To(BeEmpty())
}

func TestResolveLocation(t *testing.T) {
	g := NewWithT(t)
	cases := []struct{ base, loc, want string }{
		{"https://r.example/v2/foo/blobs/uploads/abc", "/v2/foo/blobs/uploads/xyz", "https://r.example/v2/foo/blobs/uploads/xyz"},
		{"https://r.example/v2/foo/blobs/uploads/abc", "https://other.example/path", "https://other.example/path"},
		{"https://r.example/v2/foo/blobs/uploads/abc", "next", "https://r.example/v2/foo/blobs/uploads/next"},
	}
	for _, tc := range cases {
		got, err := resolveLocation(tc.base, tc.loc)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got).To(Equal(tc.want))
	}
}
