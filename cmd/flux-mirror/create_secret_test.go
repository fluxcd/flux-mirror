// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/fluxcd/flux-mirror/internal/registryauth"
)

func TestBuildDockerConfigSecret(t *testing.T) {
	g := NewWithT(t)
	secret, err := buildDockerConfigSecret("regcreds", "flux-system", map[string]registryauth.HostAuth{
		"user.example.com":  {Username: "robot", Password: "token-value"},
		"token.example.com": {RegistryToken: "bearer-token"},
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(secret.Name).To(Equal("regcreds"))
	g.Expect(secret.Namespace).To(Equal("flux-system"))
	g.Expect(secret.Type).To(Equal(corev1.SecretTypeDockerConfigJson))

	raw, ok := secret.Data[corev1.DockerConfigJsonKey]
	g.Expect(ok).To(BeTrue())
	var parsed struct {
		Auths map[string]dockerConfigEntry `json:"auths"`
	}
	g.Expect(json.Unmarshal(raw, &parsed)).To(Succeed())

	// Username host => username/password/auth, no registrytoken.
	up := parsed.Auths["user.example.com"]
	g.Expect(up.Username).To(Equal("robot"))
	g.Expect(up.Password).To(Equal("token-value"))
	g.Expect(up.RegistryToken).To(BeEmpty())
	dec, err := base64.StdEncoding.DecodeString(up.Auth)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(string(dec)).To(Equal("robot:token-value"))

	// Token host => registrytoken only, no username/password/auth.
	tok := parsed.Auths["token.example.com"]
	g.Expect(tok.RegistryToken).To(Equal("bearer-token"))
	g.Expect(tok.Auth).To(BeEmpty())
	g.Expect(tok.Username).To(BeEmpty())
	g.Expect(tok.Password).To(BeEmpty())
}

func TestApplySecret_CreateThenReplace(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	client := fake.NewClientset()

	s1, err := buildDockerConfigSecret("regcreds", "flux-system", map[string]registryauth.HostAuth{"a.example": {Username: "robot", Password: "v1"}})
	g.Expect(err).ToNot(HaveOccurred())
	created, err := applySecret(ctx, client, s1)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(created).To(BeTrue())

	// Second apply with new data replaces the existing secret.
	s2, err := buildDockerConfigSecret("regcreds", "flux-system", map[string]registryauth.HostAuth{"a.example": {Username: "robot", Password: "v2"}})
	g.Expect(err).ToNot(HaveOccurred())
	created, err = applySecret(ctx, client, s2)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(created).To(BeFalse())

	got, err := client.CoreV1().Secrets("flux-system").Get(ctx, "regcreds", metav1.GetOptions{})
	g.Expect(err).ToNot(HaveOccurred())
	var parsed struct {
		Auths map[string]dockerConfigEntry `json:"auths"`
	}
	g.Expect(json.Unmarshal(got.Data[corev1.DockerConfigJsonKey], &parsed)).To(Succeed())
	g.Expect(parsed.Auths["a.example"].Password).To(Equal("v2"))
}
