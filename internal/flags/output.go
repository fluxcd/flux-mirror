// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package flags

import (
	"fmt"
	"slices"
	"strings"
)

var supportedOutputs = []string{"text", "yaml", "json"}

type Output string

func (o *Output) String() string {
	return string(*o)
}

func (o *Output) Set(str string) error {
	if strings.TrimSpace(str) == "" {
		return fmt.Errorf("no output format given, must be one of: %s",
			strings.Join(supportedOutputs, ", "))
	}
	if !slices.Contains(supportedOutputs, str) {
		return fmt.Errorf("unsupported output format '%s', must be one of: %s",
			str, strings.Join(supportedOutputs, ", "))
	}
	*o = Output(str)
	return nil
}

func (o *Output) Type() string {
	return strings.Join(supportedOutputs, "|")
}

func (o *Output) Description() string {
	return fmt.Sprintf("output format, can be one of: %s", strings.Join(supportedOutputs, ", "))
}
