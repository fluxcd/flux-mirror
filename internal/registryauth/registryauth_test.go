// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta1"
)

func authHostsConfig(hosts ...string) *apiv1.Config {
	cfg := &apiv1.Config{}
	for _, h := range hosts {
		cfg.Hosts = append(cfg.Hosts, apiv1.RegistryHost{
			Host:       h,
			Credential: &apiv1.RegistryCredential{FromEnv: "X"},
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
	_, err := SelectAuthHosts(&apiv1.Config{}, nil)
	g.Expect(err).To(MatchError(ContainSubstring("no hosts")))
}

func TestUsernameSwitch(t *testing.T) {
	g := NewWithT(t)

	bearer := []apiv1.RegistryHost{{
		Host: "bearer.example", Credential: &apiv1.RegistryCredential{FromEnv: "X"},
	}}
	userpass := []apiv1.RegistryHost{{
		Host: "userpass.example", Credential: &apiv1.RegistryCredential{FromEnv: "X", Username: "robot"},
	}}

	// cijwt (bearer-stamp) transport is needed only when a credential host has
	// no username.
	g.Expect(NeedsCredentialTransport(bearer)).To(BeTrue())
	g.Expect(NeedsCredentialTransport(userpass)).To(BeFalse())

	// A username credential is skipped by the cijwt options (goes via keychain).
	t.Setenv("X", "tok")
	opts, err := JWTTransportOptions(nil, userpass)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(opts).To(HaveLen(1)) // only WithInner; the host is skipped

	// ...and is served by the keychain instead.
	kc, err := BuildKeychain(context.Background(), userpass)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(kc).ToNot(BeNil())
}

func TestPkgAuthProviderName(t *testing.T) {
	g := NewWithT(t)
	cases := map[string]string{
		apiv1.RegistryProviderECR: "aws",
		apiv1.RegistryProviderACR: "azure",
		apiv1.RegistryProviderGAR: "gcp",
	}
	for in, want := range cases {
		got, err := pkgAuthProviderName(in)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got).To(Equal(want))
	}

	_, err := pkgAuthProviderName("dockerhub")
	g.Expect(err).To(MatchError(ContainSubstring("unknown registry provider")))
}

func TestHasCredentialAndTLSOnly(t *testing.T) {
	g := NewWithT(t)

	tlsOnly := apiv1.RegistryHost{Host: "tls.example", TLS: &apiv1.TLS{
		ServerAuth: &apiv1.TLSServerAuth{FromBytes: "x"},
	}}
	withCred := apiv1.RegistryHost{Host: "c.example", Credential: &apiv1.RegistryCredential{FromEnv: "X"}}
	withProvider := apiv1.RegistryHost{Host: "p.example", Provider: apiv1.RegistryProviderECR}

	g.Expect(HasCredential(tlsOnly)).To(BeFalse())
	g.Expect(HasCredential(withCred)).To(BeTrue())
	g.Expect(HasCredential(withProvider)).To(BeTrue())

	// A TLS-only host returns a clear error instead of panicking.
	_, err := ResolveHostAuth(context.Background(), tlsOnly)
	g.Expect(err).To(MatchError(ContainSubstring("no credential or provider configured")))
}
