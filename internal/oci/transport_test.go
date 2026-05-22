// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

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

// ---------- helpers ----------

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
