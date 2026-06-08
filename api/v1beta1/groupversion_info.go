// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

// Package v1beta1 contains the API Schema definitions for the flux-mirror
// config and report types.
// +kubebuilder:object:generate=true
// +groupName=mirror.fluxcd.io
// +versionName=v1beta1
package v1beta1

import "k8s.io/apimachinery/pkg/runtime/schema"

// GroupVersion identifies the flux-mirror API group and version.
var GroupVersion = schema.GroupVersion{Group: "mirror.fluxcd.io", Version: "v1beta1"}
