// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"

	"github.com/fluxcd/flux-mirror/internal/registryauth"
)

type secretFlags struct {
	config string
	hosts  []string
	create bool
}

var secretArgs secretFlags

// secretKubeFlags exposes the standard kubectl connection flags (--kubeconfig,
// --context, --cluster, --namespace/-n, --user, --server, --token,
// --request-timeout, --as, ...) and honors their env vars (KUBECONFIG, ...).
// NewConfigFlags(true) loads from a kubeconfig and falls back to the in-cluster
// config, so the command works both locally and inside a pod.
var secretKubeFlags = genericclioptions.NewConfigFlags(true)

var secretCmd = &cobra.Command{
	Use:   "secret <name>",
	Short: "Create or replace a dockerconfigjson Secret with per-host credentials",
	Long: `Resolve the credential configured under hosts for each selected
host and write them into a Kubernetes Secret of type
kubernetes.io/dockerconfigjson, the same shape 'kubectl create secret
docker-registry' produces.

By default the Secret is upserted: an existing Secret with the same name is
replaced in place. This is the right semantic for rotating pull Secrets from a
CronJob. Pass --create to instead fail if the Secret already exists, matching
'kubectl create secret docker-registry'.

--host flags. The config is read from --config, else $FLUX_MIRROR_CONFIG, else
{{CONFIG_DEFAULT}}
('-' reads the config from stdin). The cluster, namespace, and credentials are
resolved from the standard kubectl flags and env vars, working both with a local
kubeconfig and in-cluster.`,
	Example: `  # Upsert a Secret for all hosts in the default config, in the current namespace
  flux-mirror secret regcreds

  # Fail if the Secret already exists, like 'kubectl create secret docker-registry'
  flux-mirror secret regcreds --create

  # Specific hosts, a specific namespace and kubeconfig context
  flux-mirror secret regcreds -n flux-system --context prod \
    --host registry.example.com --host other.example.com -f ./flux-mirror.yaml`,
	Args: cobra.ExactArgs(1),
	RunE: secretCmdRun,
}

func secretCmdRun(cmd *cobra.Command, args []string) error {
	name := args[0]

	cfgPath, err := resolveConfigFlag(secretArgs.config)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(cmd, cfgPath, false)
	if err != nil {
		return err
	}
	hosts, err := registryauth.SelectAuthHosts(cfg, secretArgs.hosts)
	if err != nil {
		return err
	}

	// Resolve cluster + namespace before minting credentials so a broken
	// kubeconfig fails fast.
	ns, _, err := secretKubeFlags.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return fmt.Errorf("resolve namespace: %w", err)
	}
	restConfig, err := secretKubeFlags.ToRESTConfig()
	if err != nil {
		return fmt.Errorf("build kubernetes client config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}

	creds := make(map[string]registryauth.HostAuth, len(hosts))
	for _, h := range hosts {
		ha, err := registryauth.ResolveHostAuth(cmd.Context(), h)
		if err != nil {
			return fmt.Errorf("host %q: %w", h.Host, err)
		}
		creds[h.Host] = ha
	}

	secret, err := buildDockerConfigSecret(name, ns, creds)
	if err != nil {
		return err
	}
	created, err := applySecret(cmd.Context(), clientset, secret, secretArgs.create)
	if err != nil {
		return err
	}
	verb := "replaced"
	if created {
		verb = "created"
	}
	cmd.Printf("✔ secret %s/%s %s with %d host(s)\n", ns, name, verb, len(creds))
	return nil
}

// dockerConfigEntry is one registry's credentials in a dockerconfigjson. A
// username/password pair (with the base64 auth) matches what 'kubectl create
// secret docker-registry' writes; registrytoken is the non-standard bearer-token
// field understood by go-containerregistry (crane, Flux).
type dockerConfigEntry struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	Auth          string `json:"auth,omitempty"`
	RegistryToken string `json:"registrytoken,omitempty"`
}

// buildDockerConfigSecret assembles a kubernetes.io/dockerconfigjson Secret from
// per-host credentials.
func buildDockerConfigSecret(name, namespace string, creds map[string]registryauth.HostAuth) (*corev1.Secret, error) {
	auths := make(map[string]dockerConfigEntry, len(creds))
	for host, ha := range creds {
		if ha.RegistryToken != "" {
			auths[host] = dockerConfigEntry{RegistryToken: ha.RegistryToken}
			continue
		}
		auths[host] = dockerConfigEntry{
			Username: ha.Username,
			Password: ha.Password,
			Auth:     base64.StdEncoding.EncodeToString([]byte(ha.Username + ":" + ha.Password)),
		}
	}
	dockerCfg, err := json.Marshal(map[string]any{"auths": auths})
	if err != nil {
		return nil, fmt.Errorf("marshal dockerconfigjson: %w", err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: dockerCfg},
	}, nil
}

// applySecret writes the secret. By default it upserts: create, or replace if it
// already exists. When createOnly is true it only creates, returning the
// AlreadyExists error if a Secret with the same name is already present (matching
// 'kubectl create secret docker-registry'). Returns true when the secret was
// newly created.
func applySecret(ctx context.Context, client kubernetes.Interface, secret *corev1.Secret, createOnly bool) (bool, error) {
	secrets := client.CoreV1().Secrets(secret.Namespace)
	if _, err := secrets.Create(ctx, secret, metav1.CreateOptions{}); err == nil {
		return true, nil
	} else if createOnly || !apierrors.IsAlreadyExists(err) {
		return false, fmt.Errorf("create secret: %w", err)
	}
	existing, err := secrets.Get(ctx, secret.Name, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("get existing secret: %w", err)
	}
	secret.ResourceVersion = existing.ResourceVersion
	if _, err := secrets.Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return false, fmt.Errorf("replace secret: %w", err)
	}
	return false, nil
}

func init() {
	secretCmd.Long = strings.ReplaceAll(secretCmd.Long, configDefaultPlaceholder, defaultConfigDescription())
	secretCmd.Flags().StringVarP(&secretArgs.config, "config", "f", "", configFlagUsage())
	secretCmd.Flags().StringArrayVar(&secretArgs.hosts, "host", nil,
		"Registry host from the config to include; repeatable, defaults to all hosts")
	secretCmd.Flags().BoolVar(&secretArgs.create, "create", false,
		"Fail if the Secret already exists instead of replacing it")
	secretKubeFlags.AddFlags(secretCmd.Flags())
	rootCmd.AddCommand(secretCmd)
}
