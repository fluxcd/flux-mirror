// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package flags

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

var supportedLogLevels = []string{"debug", "info", "warn", "error"}

// LogLevel is a pflag.Value holding a log verbosity level. The zero value
// means unset.
type LogLevel string

func (l *LogLevel) String() string {
	return string(*l)
}

func (l *LogLevel) Set(str string) error {
	if !slices.Contains(supportedLogLevels, str) {
		return fmt.Errorf("unsupported log level '%s', must be one of: %s",
			str, strings.Join(supportedLogLevels, ", "))
	}
	*l = LogLevel(str)
	return nil
}

func (l *LogLevel) Type() string {
	return strings.Join(supportedLogLevels, "|")
}

func (l *LogLevel) Description() string {
	return fmt.Sprintf("log level, can be one of: %s", strings.Join(supportedLogLevels, ", "))
}

// Level returns the slog level. An unset value yields slog.LevelInfo.
func (l *LogLevel) Level() slog.Level {
	switch *l {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
