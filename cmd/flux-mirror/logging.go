// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
)

// plainHandler is a slog.Handler that writes lines matching the standard
// library's log.LstdFlags format ("YYYY/MM/DD HH:MM:SS msg [key=val ...]"),
// so output from the runner, the entry mirrors, and the underlying crane
// library all share a single visual style.
type plainHandler struct {
	w     io.Writer
	mu    *sync.Mutex
	level slog.Leveler
	attrs []slog.Attr
}

func newPlainHandler(w io.Writer, level slog.Leveler) *plainHandler {
	return &plainHandler{w: w, mu: &sync.Mutex{}, level: level}
}

func (h *plainHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *plainHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	t := r.Time
	fmt.Fprintf(&b, "%04d/%02d/%02d %02d:%02d:%02d %s",
		t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), r.Message)
	for _, a := range h.attrs {
		b.WriteByte(' ')
		writeAttr(&b, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		b.WriteByte(' ')
		writeAttr(&b, a)
		return true
	})
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *plainHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h2 := *h
	h2.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &h2
}

func (h *plainHandler) WithGroup(_ string) slog.Handler { return h }

func writeAttr(b *strings.Builder, a slog.Attr) {
	b.WriteString(a.Key)
	b.WriteByte('=')
	v := a.Value.String()
	if strings.ContainsAny(v, " \t\"=") {
		b.WriteString(strconv.Quote(v))
	} else {
		b.WriteString(v)
	}
}
