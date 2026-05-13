// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/fluxcd/flux-mirror/internal/testregistry"
)

// dockerReg is the address of the in-process registry shared across tests
// in this package. Each test builds refs with testregistry.RandSuffix to
// claim its own namespace.
var dockerReg string

func TestMain(m *testing.M) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, err := testregistry.Start(ctx)
	if err != nil {
		panic(fmt.Sprintf("start registry: %s", err))
	}
	dockerReg = addr
	os.Exit(m.Run())
}

func repo(stem string) string {
	return testregistry.Repo(dockerReg, stem)
}
