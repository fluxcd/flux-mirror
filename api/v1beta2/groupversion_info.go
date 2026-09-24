// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

// Package v1beta2 contains the API Schema definitions for the flux-mirror
// config and report types.
// +kubebuilder:object:generate=true
// +groupName=mirror.plugin.fluxcd.io
// +versionName=v1beta2
package v1beta2

import "k8s.io/apimachinery/pkg/runtime/schema"

// GroupVersion identifies the flux-mirror API group and version.
var GroupVersion = schema.GroupVersion{Group: "mirror.plugin.fluxcd.io", Version: "v1beta2"}
