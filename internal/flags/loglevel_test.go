// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package flags

import (
	"log/slog"
	"testing"

	. "github.com/onsi/gomega"
)

func TestLogLevel_Set(t *testing.T) {
	tests := []struct {
		name      string
		str       string
		expect    string
		level     slog.Level
		expectErr bool
	}{
		{"debug", "debug", "debug", slog.LevelDebug, false},
		{"info", "info", "info", slog.LevelInfo, false},
		{"warn", "warn", "warn", slog.LevelWarn, false},
		{"error", "error", "error", slog.LevelError, false},
		{"unsupported", "trace", "", slog.LevelInfo, true},
		{"empty", "", "", slog.LevelInfo, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			var l LogLevel
			err := l.Set(tt.str)
			if tt.expectErr {
				g.Expect(err).To(HaveOccurred())
			} else {
				g.Expect(err).ToNot(HaveOccurred())
			}
			g.Expect(l.String()).To(Equal(tt.expect))
			g.Expect(l.Level()).To(Equal(tt.level))
		})
	}
}
