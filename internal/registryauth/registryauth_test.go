// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"testing"

	. "github.com/onsi/gomega"

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
	hosts, err := SelectAuthHosts(authHostsConfig("a.example", "b.example"), nil)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(hosts).To(HaveLen(2))
}

func TestSelectAuthHosts_Filter(t *testing.T) {
	g := NewWithT(t)
	cfg := authHostsConfig("a.example", "b.example", "c.example")
	hosts, err := SelectAuthHosts(cfg, []string{"b.example", "c.example"})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(hosts).To(HaveLen(2))
	g.Expect(hosts[0].Host).To(Equal("b.example"))
	g.Expect(hosts[1].Host).To(Equal("c.example"))
}

func TestSelectAuthHosts_UnknownHost(t *testing.T) {
	g := NewWithT(t)
	_, err := SelectAuthHosts(authHostsConfig("a.example"), []string{"missing.example"})
	g.Expect(err).To(MatchError(ContainSubstring(`host "missing.example" not found`)))
}

func TestSelectAuthHosts_NoAuth(t *testing.T) {
	g := NewWithT(t)
	_, err := SelectAuthHosts(&config.Config{}, nil)
	g.Expect(err).To(MatchError(ContainSubstring("no auth.hosts")))
}

func TestPkgAuthProviderName(t *testing.T) {
	g := NewWithT(t)
	cases := map[string]string{
		config.RegistryProviderECR: "aws",
		config.RegistryProviderACR: "azure",
		config.RegistryProviderGAR: "gcp",
	}
	for in, want := range cases {
		got, err := pkgAuthProviderName(in)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got).To(Equal(want))
	}

	_, err := pkgAuthProviderName("dockerhub")
	g.Expect(err).To(MatchError(ContainSubstring("unknown registry provider")))
}
