// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	craneLogs "github.com/google/go-containerregistry/pkg/logs"
	"github.com/spf13/cobra"
	"google.golang.org/api/idtoken"

	"github.com/fluxcd/pkg/auth/actionsoidc"
	"github.com/fluxcd/pkg/auth/utils/cijwt"

	"github.com/fluxcd/flux-mirror/internal/artifacts"
	"github.com/fluxcd/flux-mirror/internal/charts"
	"github.com/fluxcd/flux-mirror/internal/config"
	"github.com/fluxcd/flux-mirror/internal/flags"
	"github.com/fluxcd/flux-mirror/internal/jwkio"
	"github.com/fluxcd/flux-mirror/internal/oci"
	"github.com/fluxcd/flux-mirror/internal/sync"
)

const (
	envConfig                  = "FLUX_MIRROR_CONFIG"
	syncDefaultTimeout         = 5 * time.Minute
	syncDefaultDriftExitCode   = 2
	syncMaxCustomDriftExitCode = 255
)

var syncCmd = &cobra.Command{
	Use:   "sync [CONFIG|-]",
	Short: "Mirror Helm charts and OCI artifacts to a destination registry",
	Long: `Mirror Helm charts and OCI artifacts between registries based on a
declarative YAML config (apiVersion: mirror.fluxcd.io/v1alpha1, kind: Config).
OCI registry auth is read from the ambient Docker config (~/.docker/config.json,
$DOCKER_CONFIG, and configured credential helpers), or, for hosts listed in the
config's 'auth' section, from a per-host JWT. Helm HTTP/S repository auth is read
from Helm's default repositories.yaml path, or $HELM_REPOSITORY_CONFIG when set.

Exit codes:
  0  clean run (everything copied/skipped as expected)
  1  at least one tag job failed
  2  no failures, but at least one tag drifted (overwrite=false)

Use --drift-exit-code=0 to keep drift-only runs green in CI when the
destination registry is known to be immutable.`,
	Example: `  # Run a sync against a config file
  flux-mirror sync flux-mirror.yaml

  # Pass the config via env var
  FLUX_MIRROR_CONFIG=flux-mirror.yaml flux-mirror sync

  # Pass the config via stdin
  flux-mirror sync - < flux-mirror.yaml

  # Preview without writing to the destination
  flux-mirror sync flux-mirror.yaml --dry-run -o yaml

  # Force overwrite of every drifted tag
  flux-mirror sync flux-mirror.yaml --overwrite`,
	Args: cobra.MaximumNArgs(1),
	RunE: syncCmdRun,
}

type syncFlags struct {
	output        flags.Output
	concurrency   int
	retries       int
	overwrite     bool
	driftExitCode int
	dryRun        bool
	verbose       bool
	insecure      bool
	noProgress    bool
	maxChunkSize  int
}

var syncArgs = syncFlags{
	output:        "text",
	concurrency:   4,
	retries:       3,
	driftExitCode: syncDefaultDriftExitCode,
}

func init() {
	syncCmd.Flags().VarP(&syncArgs.output, "output", "o", syncArgs.output.Description())
	syncCmd.Flags().IntVar(&syncArgs.concurrency, "concurrency", syncArgs.concurrency,
		"Maximum number of copy operations to run in parallel per job")
	syncCmd.Flags().IntVar(&syncArgs.retries, "retries", syncArgs.retries,
		"Maximum number of retry attempts per job within timeout budget.")
	syncCmd.Flags().BoolVar(&syncArgs.overwrite, "overwrite", false,
		"Force overwrite when the destination artifact digest has drifted")
	syncCmd.Flags().IntVar(&syncArgs.driftExitCode, "drift-exit-code", syncArgs.driftExitCode,
		"Exit code to use when drift is detected without failures")
	syncCmd.Flags().BoolVar(&syncArgs.dryRun, "dry-run", false,
		"Run the plan and comparison pipeline without performing any writes.")
	syncCmd.Flags().BoolVar(&syncArgs.verbose, "verbose", false,
		"Log all operations and the involved digests as they are performed.")
	syncCmd.Flags().BoolVar(&syncArgs.insecure, "insecure", false,
		"Allow plaintext HTTP and skip TLS verification (test/dev only).")
	syncCmd.Flags().BoolVar(&syncArgs.noProgress, "no-progress", false,
		"Disable the live progress spinner.")
	syncCmd.Flags().IntVar(&syncArgs.maxChunkSize, "oci-max-chunk-size", 0,
		"Maximum size in KiB (1024 bytes) for an OCI blob upload PATCH; "+
			"larger blobs are split into chunked PATCH uploads. 0 disables "+
			"chunking (single monolithic PATCH per blob).")

	rootCmd.AddCommand(syncCmd)
}

func syncCmdRun(cmd *cobra.Command, args []string) error {
	if syncArgs.driftExitCode < 0 || syncArgs.driftExitCode > syncMaxCustomDriftExitCode {
		return fmt.Errorf("--drift-exit-code must be between 0 and %d", syncMaxCustomDriftExitCode)
	}

	cfgPath, err := resolveConfigPath(args)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(cmd, cfgPath)
	if err != nil {
		return err
	}

	// Bump per-job timeout to 5m if the user didn't pass --timeout explicitly.
	// The root flag's declared default stays at 1m so other commands aren't
	// affected.
	if !cmd.Flags().Changed("timeout") {
		rootArgs.timeout = syncDefaultTimeout
	}

	// Verbose enables our own structured logs (sync started, entry started,
	// mirroring tag, tag done, entry summary, sync complete) AND wires
	// crane's package-global Progress / Warn loggers to stderr in the same
	// log.LstdFlags shape — that's where the per-blob digest lines, fallback
	// tag updates, and registry rejections come from. Without verbose, both
	// streams are silent and only the spinner + per-job lines show.
	var logger *slog.Logger
	if syncArgs.verbose {
		logger = slog.New(newPlainHandler(cmd.ErrOrStderr(), slog.LevelDebug))
		craneLogs.Progress = log.New(cmd.ErrOrStderr(), "", log.LstdFlags)
		craneLogs.Warn = log.New(cmd.ErrOrStderr(), "", log.LstdFlags)
	} else {
		logger = slog.New(slog.DiscardHandler)
	}

	var clientOpts []oci.ClientOption
	if syncArgs.insecure {
		clientOpts = append(clientOpts, oci.Insecure())
	}
	if t, err := buildClientTransport(cfg.Auth); err != nil {
		return err
	} else if t != nil {
		clientOpts = append(clientOpts, oci.WithTransport(t))
	}
	client := oci.NewClient(clientOpts...)

	runner := &sync.Runner{
		Concurrency:   syncArgs.concurrency,
		Retries:       syncArgs.retries,
		PerJobTimeout: rootArgs.timeout,
		Logger:        logger,
	}

	mirrors := make([]sync.EntryMirror, 0, len(cfg.Artifacts)+len(cfg.Charts))
	for _, e := range cfg.Artifacts {
		mirrors = append(mirrors, artifacts.New(client, e, artifacts.Options{
			Overwrite: syncArgs.overwrite,
			DryRun:    syncArgs.dryRun,
			Verbose:   syncArgs.verbose,
			CopyJobs:  syncArgs.concurrency,
			Logger:    logger,
		}))
	}
	for _, e := range cfg.Charts {
		m, err := charts.New(client, e, charts.Options{
			Overwrite: syncArgs.overwrite,
			DryRun:    syncArgs.dryRun,
			Verbose:   syncArgs.verbose,
			Logger:    logger,
		})
		if err != nil {
			return fmt.Errorf("build chart entry %s/%s: %w", e.Source, e.Name, err)
		}
		mirrors = append(mirrors, m)
	}

	// Pretty-print mode = text output AND no --verbose. In that case we
	// drive a spinner on stderr and print one completion line per tag on
	// stdout, followed by a one-line totals summary. The spinner is stopped
	// before the summary so it can't overwrite it. Verbose mode skips the
	// spinner entirely — the structured log stream already conveys progress,
	// and a spinner would compete with it.
	isText := syncArgs.output.String() == "text" || syncArgs.output.String() == ""
	prettyPrint := isText && !syncArgs.verbose

	var prog *progress
	if prettyPrint {
		var spinnerOut io.Writer
		if !syncArgs.noProgress {
			spinnerOut = cmd.ErrOrStderr()
		}
		prog = newProgress(cmd.OutOrStdout(), spinnerOut, len(mirrors))
		runner.OnJobFinished = prog.JobFinished
		runner.OnEntryFinished = prog.EntryFinished
		runner.OnPlanError = prog.PlanFailed
	}

	res, err := runner.Run(cmd.Context(), mirrors)
	if prog != nil {
		prog.Close()
	}
	if err != nil {
		return err
	}
	switch syncArgs.output.String() {
	case "text", "":
		res.LogSummary(logger)
		if prettyPrint {
			if err := res.PrettyPrint(cmd.OutOrStdout()); err != nil {
				return err
			}
		}
	default:
		if err := res.Render(cmd.OutOrStdout(), syncArgs.output.String()); err != nil {
			return err
		}
	}
	return classifyExit(res, syncArgs.driftExitCode)
}

// syncExitError is returned to make `main` exit with a code other than 1
// without printing the default error decoration.
type syncExitError struct {
	code int
	msg  string
}

func (e *syncExitError) Error() string { return e.msg }
func (e *syncExitError) ExitCode() int { return e.code }

func classifyExit(r sync.Result, driftExitCode int) error {
	// Non-zero exit codes carry an empty message — failures and drift are
	// already surfaced inline (per-failure ✗ lines) and in the Summary
	// totals; printing a third "N failed" footer just adds noise.
	code := r.ExitCode()
	if code == syncDefaultDriftExitCode && r.HasDrift() && !r.HasFailures() {
		code = driftExitCode
	}
	switch code {
	case 0:
		return nil
	default:
		return &syncExitError{code: code, msg: ""}
	}
}

// buildClientTransport composes the optional transport stack from the config's
// auth hosts and --oci-max-chunk-size. Returns (nil, nil) if neither is
// requested. Stack order, outer first:
//
//	ChunkingTransport (split big PATCH bodies)
//	  → cijwt.Transport (stamp Authorization on each chunk request)
//	    → http.DefaultTransport
//
// JWT is the inner wrapper so that per-request token minting works per chunk;
// chunking is the outer wrapper so split chunks each get freshly stamped auth
// from the JWT layer.
func buildClientTransport(auth *config.Auth) (http.RoundTripper, error) {
	var (
		jwtSet   = auth != nil && len(auth.Hosts) > 0
		chunkSet = syncArgs.maxChunkSize > 0
	)
	if !jwtSet && !chunkSet {
		return nil, nil
	}
	t := http.DefaultTransport
	if jwtSet {
		opts, err := jwtTransportOptions(t, auth)
		if err != nil {
			return nil, err
		}
		jwt, err := cijwt.NewTransport(opts...)
		if err != nil {
			return nil, err
		}
		t = jwt
	}
	if chunkSet {
		t = &oci.ChunkingTransport{
			Inner:     t,
			ChunkSize: int64(syncArgs.maxChunkSize) * 1024,
		}
	}
	return t, nil
}

// jwtTransportOptions turns the validated auth.hosts config into cijwt options,
// reading FromEnv environment variables and JWKPath files at this point and
// wiring each Provider to its token-minting closure. FromPath is wired straight
// to cijwt, which re-reads the file on every request. It assumes the config
// has passed config validation (exactly one source per host and the required
// fields present), so it only reports errors that need runtime state: an unset
// env var or an unreadable key file.
func jwtTransportOptions(inner http.RoundTripper, auth *config.Auth) ([]cijwt.Option, error) {
	opts := []cijwt.Option{cijwt.WithInner(inner)}

	for _, h := range auth.Hosts {
		j := h.Credential
		aud := h.EffectiveAud()
		switch {
		case j.Provider != "":
			fn, err := providerTokenFunc(j.Provider, aud)
			if err != nil {
				return nil, fmt.Errorf("auth host %q: %w", h.Host, err)
			}
			opts = append(opts, cijwt.WithHostTokenFunc(h.Host, fn))
		case j.FromEnv != "":
			token := os.Getenv(j.FromEnv)
			if token == "" {
				return nil, fmt.Errorf("auth host %q: environment variable %q is not set or empty", h.Host, j.FromEnv)
			}
			opts = append(opts, cijwt.WithHostToken(h.Host, token))
		case j.FromPath != "":
			opts = append(opts, cijwt.WithHostTokenFile(h.Host, j.FromPath))
		case j.JWKPath != "":
			jwk, err := jwkio.ReadPrivateJWK(j.JWKPath)
			if err != nil {
				return nil, fmt.Errorf("auth host %q: read jwkPath: %w", h.Host, err)
			}
			opts = append(opts, cijwt.WithHostJWK(h.Host, jwk, j.Iss, aud, j.Sub))
		}
	}

	return opts, nil
}

// providerTokenFunc returns a cijwt.TokenFunc that mints a per-request bearer
// credential for aud using the given provider: an OIDC ID/access token for the
// OIDC providers, or a signed sts:GetCallerIdentity envelope for aws. cijwt
// parses each returned token's exp claim and caches it for the first 50% of its
// lifetime, so the closure runs only on a cache miss and any ctx-scoped setup
// happens lazily under the request's own context.
func providerTokenFunc(provider, aud string) (cijwt.TokenFunc, error) {
	switch provider {
	case config.JWTProviderGitHub, config.JWTProviderForgejo:
		return func(ctx context.Context) (string, error) {
			return actionsoidc.FetchToken(ctx, aud)
		}, nil
	case config.JWTProviderGCP:
		return func(ctx context.Context) (string, error) {
			ts, err := idtoken.NewTokenSource(ctx, aud)
			if err != nil {
				return "", fmt.Errorf("create GCP ID token source: %w", err)
			}
			tok, err := ts.Token()
			if err != nil {
				return "", err
			}
			return tok.AccessToken, nil
		}, nil
	case config.JWTProviderAzure:
		cred, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, fmt.Errorf("create Azure credential: %w", err)
		}
		// aud must be the App ID URI (or client ID) of a registered Entra
		// application configured for v2 access tokens; the .default suffix
		// requests a token whose aud claim is that application, signed by
		// Entra and verifiable through its OIDC discovery document.
		scopes := []string{aud + "/.default"}
		return func(ctx context.Context) (string, error) {
			tok, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: scopes})
			if err != nil {
				return "", err
			}
			return tok.Token, nil
		}, nil
	case config.JWTProviderAWS:
		signer := v4.NewSigner()
		return func(ctx context.Context) (string, error) {
			cfg, err := awsconfig.LoadDefaultConfig(ctx)
			if err != nil {
				return "", fmt.Errorf("load AWS config: %w", err)
			}
			region := cfg.Region
			if region == "" {
				region = "us-east-1"
			}
			return mintAWSSTSToken(ctx, cfg.Credentials, signer, region, aud)
		}, nil
	default:
		return nil, fmt.Errorf("unknown provider %q", provider)
	}
}

// awsSTSTokenTTL bounds how long flux-mirror reuses a signed GetCallerIdentity
// request before re-signing. cijwt re-mints at 50% of this, so a given signed
// request is replayed for at most half of it — kept well inside the few-minute
// window AWS STS accepts a SigV4 request's timestamp.
const awsSTSTokenTTL = 2 * time.Minute

// awsAudienceHeader carries aud, signed into the GetCallerIdentity request to
// pin the intended registry, so a request minted for one host cannot be
// replayed against another. The registry must require it and match it to its
// own identity. It plays the role Vault's X-Vault-AWS-IAM-Server-ID guard does.
const awsAudienceHeader = "X-Audience"

// awsSTSTokenType is the JOSE "typ" header the registry uses to tell this
// envelope apart from a real OIDC JWT. Together with "alg":"none" it signals
// "do not validate me as a JWT — replay my sts claim to STS instead". The
// registry must route on this and must never trust the envelope's claims; the
// only source of identity is the STS response, which sidesteps the classic
// alg=none confusion attack.
const awsSTSTokenType = "aws-sigv4-getcalleridentity"

// mintAWSSTSToken builds a SigV4-signed sts:GetCallerIdentity request and wraps
// it in a JWT-shaped envelope. AWS issues no JWT of its own, so the registry
// verifies identity out-of-band: it replays the signed request to STS (which
// validates the signature) and reads the returned account/ARN. The envelope is
// not a real JWT — it carries the signed request under the "sts" claim and an
// unverified exp that only tells the cijwt transport when to re-mint; nothing
// checks its (empty) signature segment.
func mintAWSSTSToken(ctx context.Context, creds aws.CredentialsProvider, signer *v4.Signer, region, serverID string) (string, error) {
	c, err := creds.Retrieve(ctx)
	if err != nil {
		return "", fmt.Errorf("retrieve AWS credentials: %w", err)
	}

	const body = "Action=GetCallerIdentity&Version=2011-06-15"
	url := fmt.Sprintf("https://sts.%s.amazonaws.com/", region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(awsAudienceHeader, serverID)

	sum := sha256.Sum256([]byte(body))
	now := time.Now()
	if err := signer.SignHTTP(ctx, c, req, hex.EncodeToString(sum[:]), "sts", region, now); err != nil {
		return "", fmt.Errorf("sign GetCallerIdentity request: %w", err)
	}

	sts, err := json.Marshal(map[string]any{
		"method":  http.MethodPost,
		"url":     url,
		"headers": req.Header,
		"body":    body,
	})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"exp": now.Add(awsSTSTokenTTL).Unix(),
		"aud": serverID,
		"sts": json.RawMessage(sts),
	})
	if err != nil {
		return "", err
	}

	// A JWT-shaped, unsigned envelope: header.claims with an empty signature
	// segment. The header's alg=none + typ let the registry route this away from
	// JWKS validation. cijwt ParseUnverified-reads only the exp; the registry
	// decodes the "sts" claim and replays the signed request.
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"none","typ":"` + awsSTSTokenType + `"}`))
	return header + "." + enc.EncodeToString(claims) + ".", nil
}

func resolveConfigPath(args []string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	if env := os.Getenv(envConfig); env != "" {
		return env, nil
	}
	return "", fmt.Errorf("config required: pass the config path as the first argument, pass '-' for stdin, or set %s", envConfig)
}

func loadConfig(cmd *cobra.Command, path string) (*config.Config, error) {
	if path != "-" {
		return config.Load(path)
	}
	cfg, err := config.Decode(cmd.InOrStdin())
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}
