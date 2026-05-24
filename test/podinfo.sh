#!/usr/bin/env bash
# Copyright 2026 The Flux Authors
# SPDX-License-Identifier: Apache-2.0

# End-to-end test for the podinfo mirror config.
# Prerequisites: a local OCI registry on port 5050 (make registry-up)
# and the binary built at ./bin/flux-mirror (make build).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${REPO_ROOT}/bin/flux-mirror"
CONFIG="${REPO_ROOT}/test/podinfo.yaml"
REGISTRY="localhost:5050"

if [[ ! -x "${BIN}" ]]; then
  echo "FAIL: binary not found at ${BIN}, run 'make build'" >&2
  exit 1
fi

echo "==> Running sync"
"${BIN}" sync "${CONFIG}" --insecure --verbose

echo ""
echo "==> Verifying Helm charts (mediaType)"
HELM_MEDIA_TYPE="application/vnd.cncf.helm.chart.content.v1.tar+gzip"
for tag in 6.11.2 6.11.1; do
  echo -n "  charts/podinfo:${tag} ... "
  MANIFEST=$(docker manifest inspect "${REGISTRY}/charts/podinfo:${tag}" --insecure)
  MATCH=$(echo "${MANIFEST}" | jq -r --arg mt "${HELM_MEDIA_TYPE}" '[.layers[] | select(.mediaType == $mt)] | length')
  if [[ "${MATCH}" -eq 0 ]]; then
    echo "FAIL (no ${HELM_MEDIA_TYPE} layer)" >&2
    echo "${MANIFEST}" | jq . >&2
    exit 1
  fi
  echo "OK"
done

echo ""
echo "==> Verifying OCI manifests (mediaType)"
FLUX_MEDIA_TYPE="application/vnd.cncf.flux.content.v1.tar+gzip"
for tag in 6.11.2 6.11.1; do
  echo -n "  manifests/podinfo:${tag} ... "
  MANIFEST=$(docker manifest inspect "${REGISTRY}/manifests/podinfo:${tag}" --insecure)
  MATCH=$(echo "${MANIFEST}" | jq -r --arg mt "${FLUX_MEDIA_TYPE}" '[.layers[] | select(.mediaType == $mt)] | length')
  if [[ "${MATCH}" -eq 0 ]]; then
    echo "FAIL (no ${FLUX_MEDIA_TYPE} layer)" >&2
    echo "${MANIFEST}" | jq . >&2
    exit 1
  fi
  echo "OK"
done

echo ""
echo "==> Verifying container images (cosign)"
for tag in 6.11.2 6.11.1; do
  echo -n "  podinfo:${tag} ... "
  if ! COSIGN_OUT=$(cosign verify "${REGISTRY}/podinfo:${tag}" \
    --certificate-identity-regexp '^https://github\.com/stefanprodan/.*$' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --allow-http-registry 2>&1); then
    echo "FAIL" >&2
    echo "${COSIGN_OUT}" >&2
    exit 1
  fi
  echo "OK"
done

echo ""
echo "==> Re-running sync (idempotency check)"
SYNC_OUT=$("${BIN}" sync "${CONFIG}" --insecure -o json)

NON_SKIPPED=$(echo "${SYNC_OUT}" | jq '[.entries[] | .outcomes | to_entries[] | select(.key != "skipped")] | length')
if [[ "${NON_SKIPPED}" -ne 0 ]]; then
  echo "FAIL: expected all outcomes to be skipped on re-sync" >&2
  echo "${SYNC_OUT}" | jq . >&2
  exit 1
fi

SKIPPED=$(echo "${SYNC_OUT}" | jq '[.entries[].outcomes.skipped[]] | length')
echo "  all ${SKIPPED} tags skipped"

echo ""
echo "PASS: all artifacts verified"
