// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

// Package registryauth resolves OCI registry credentials for the hosts in a
// flux-mirror config's hosts section: JWT credentials (provider/fromEnv/fromPath/
// jwkPath) and cloud registry providers (ecr/acr/gar). It is the auth layer
// shared by the sync, login, and create commands.
package registryauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/idtoken"

	"github.com/fluxcd/pkg/auth/actionsoidc"
	"github.com/fluxcd/pkg/auth/jwt"
	"github.com/fluxcd/pkg/auth/utils/cijwt"

	"github.com/fluxcd/flux-mirror/internal/config"
	"github.com/fluxcd/flux-mirror/internal/jwkio"
)

// resolveCredential mints or reads the credential for a single credential host,
// mirroring what jwtTransportOptions wires into the sync transport but returning
// the value directly. It assumes the config has passed validation (exactly one
// source).
func resolveCredential(ctx context.Context, h config.AuthHost) (string, error) {
	c := h.Credential
	aud := h.EffectiveAud()
	switch {
	case c.Provider != "":
		fn, err := providerTokenFunc(c.Provider, aud)
		if err != nil {
			return "", err
		}
		return fn(ctx)
	case c.FromEnv != "":
		token := os.Getenv(c.FromEnv)
		if token == "" {
			return "", fmt.Errorf("environment variable %q is not set or empty", c.FromEnv)
		}
		return token, nil
	case c.FromPath != "":
		b, err := os.ReadFile(c.FromPath)
		if err != nil {
			return "", fmt.Errorf("read fromPath: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	case c.JWKPath != "":
		raw, err := jwkio.ReadPrivateJWK(c.JWKPath)
		if err != nil {
			return "", fmt.Errorf("read jwkPath: %w", err)
		}
		fn, err := jwkTokenFunc(raw, c.Iss, c.Sub, aud, c.EffectiveExp())
		if err != nil {
			return "", fmt.Errorf("parse jwkPath: %w", err)
		}
		return fn(ctx)
	default:
		return "", fmt.Errorf("credential has no source")
	}
}

// jwkTokenFunc parses a private JWK once and returns a cijwt.TokenFunc that
// signs a fresh JWT with the given claims and lifetime on each call. Used for
// jwkPath credentials that set a custom exp; the default 60s lifetime is served
// by cijwt.WithHostJWK instead.
func jwkTokenFunc(jwk, iss, sub, aud string, ttl time.Duration) (cijwt.TokenFunc, error) {
	key, err := jwt.ParseJWK(jwk)
	if err != nil {
		return nil, err
	}
	return func(context.Context) (string, error) {
		return key.Issue(iss, sub, aud, ttl)
	}, nil
}

// gcpUserIDToken returns the default-audience OIDC ID token carried in the
// Application Default Credentials token response. It is the fallback for user
// credentials (authorized_user), which Google forbids from minting a
// custom-audience ID token. The returned token's aud is fixed to the gcloud
// OAuth client ID and its identity is the signed-in user (email/sub) — it is a
// real Google-signed ID token (iss=accounts.google.com), just not for the
// configured audience.
func gcpUserIDToken(ctx context.Context) (string, error) {
	creds, err := google.FindDefaultCredentials(ctx,
		"openid", "https://www.googleapis.com/auth/userinfo.email")
	if err != nil {
		return "", fmt.Errorf("find GCP default credentials: %w", err)
	}
	tok, err := creds.TokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("get GCP token: %w", err)
	}
	idTok, ok := tok.Extra("id_token").(string)
	if !ok || idTok == "" {
		return "", fmt.Errorf("GCP credential response carried no id_token")
	}
	return idTok, nil
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
				// User credentials (authorized_user, e.g. `gcloud auth
				// application-default login`) cannot mint a custom-audience ID
				// token. Fall back to the default-audience id_token from ADC,
				// whose aud is the gcloud OAuth client ID (not aud) and whose
				// identity is the human's email/sub.
				if strings.Contains(err.Error(), "unsupported credentials type") {
					return gcpUserIDToken(ctx)
				}
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
	case config.JWTProviderJWTSVID:
		return func(ctx context.Context) (string, error) {
			// Fetch a JWT-SVID for the audience from the ambient Workload API
			// (SPIFFE_ENDPOINT_SOCKET). The source is opened per cache-miss call
			// and closed immediately; cijwt caches the returned token's exp, so
			// this does not dial the socket on every request.
			src, err := workloadapi.NewJWTSource(ctx)
			if err != nil {
				return "", fmt.Errorf("create SPIFFE JWT source: %w", err)
			}
			defer src.Close()
			svid, err := src.FetchJWTSVID(ctx, jwtsvid.Params{Audience: aud})
			if err != nil {
				return "", fmt.Errorf("fetch JWT-SVID: %w", err)
			}
			return svid.Marshal(), nil
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
