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

	"github.com/fluxcd/flux-mirror/internal/config"
)

func authHostsConfig(hosts ...string) *config.Config {
	cfg := &config.Config{Auth: &config.Auth{}}
	for _, h := range hosts {
		cfg.Auth.Hosts = append(cfg.Auth.Hosts, config.AuthHost{
			Host:       h,
			Credential: &config.AuthCredential{FromEnv: "X"},
		})
	}
	return cfg
}

func TestSelectAuthHosts_AllByDefault(t *testing.T) {
	g := NewWithT(t)
	hosts, err := selectAuthHosts(authHostsConfig("a.example", "b.example"), nil)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(hosts).To(HaveLen(2))
}

func TestSelectAuthHosts_Filter(t *testing.T) {
	g := NewWithT(t)
	cfg := authHostsConfig("a.example", "b.example", "c.example")
	hosts, err := selectAuthHosts(cfg, []string{"b.example", "c.example"})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(hosts).To(HaveLen(2))
	g.Expect(hosts[0].Host).To(Equal("b.example"))
	g.Expect(hosts[1].Host).To(Equal("c.example"))
}

func TestSelectAuthHosts_UnknownHost(t *testing.T) {
	g := NewWithT(t)
	_, err := selectAuthHosts(authHostsConfig("a.example"), []string{"missing.example"})
	g.Expect(err).To(MatchError(ContainSubstring(`host "missing.example" not found`)))
}

func TestSelectAuthHosts_NoAuth(t *testing.T) {
	g := NewWithT(t)
	_, err := selectAuthHosts(&config.Config{}, nil)
	g.Expect(err).To(MatchError(ContainSubstring("no auth.hosts")))
}

func TestBuildDockerConfigSecret(t *testing.T) {
	g := NewWithT(t)
	secret, err := buildDockerConfigSecret("regcreds", "flux-system", map[string]string{
		"registry.example.com": "token-value",
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
	entry, ok := parsed.Auths["registry.example.com"]
	g.Expect(ok).To(BeTrue())
	g.Expect(entry.Username).To(Equal(loginUsername))
	g.Expect(entry.Password).To(Equal("token-value"))
	dec, err := base64.StdEncoding.DecodeString(entry.Auth)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(string(dec)).To(Equal(loginUsername + ":token-value"))
}

func TestApplySecret_CreateThenReplace(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	client := fake.NewClientset()

	s1, err := buildDockerConfigSecret("regcreds", "flux-system", map[string]string{"a.example": "v1"})
	g.Expect(err).ToNot(HaveOccurred())
	created, err := applySecret(ctx, client, s1)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(created).To(BeTrue())

	// Second apply with new data replaces the existing secret.
	s2, err := buildDockerConfigSecret("regcreds", "flux-system", map[string]string{"a.example": "v2"})
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
