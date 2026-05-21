// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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
