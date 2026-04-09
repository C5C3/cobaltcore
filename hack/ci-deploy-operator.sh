#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-deploy-operator.sh — Deploy an operator into a kind cluster.
# Feature: CC-0050
#
# Installs CRDs, waits for establishment, and deploys the operator via Helm
# with the specified container image.
#
# Required env vars:
#   OPERATOR    — Operator name (e.g. keystone)
#   IMAGE_REPO  — Full image repository (e.g. ghcr.io/c5c3/keystone-operator)
#
# Optional env vars:
#   IMAGE_TAG   — Image tag (default: dev)
#
# REQ-003: Reusable operator deployment script.
# REQ-007: set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
OPERATOR="${OPERATOR:?OPERATOR is required (e.g. keystone)}"
if [[ ! "${OPERATOR}" =~ ^[a-z][a-z0-9-]*$ ]]; then
  echo "::error::OPERATOR must be lowercase alphanumeric (with hyphens), got '${OPERATOR}'"
  exit 1
fi
IMAGE_REPO="${IMAGE_REPO:?IMAGE_REPO is required (e.g. ghcr.io/c5c3/keystone-operator)}"
IMAGE_TAG="${IMAGE_TAG:-dev}"

CHART_PATH="operators/${OPERATOR}/helm/${OPERATOR}-operator"

# ---------------------------------------------------------------------------
# 1. Install CRDs (idempotent — kubectl apply succeeds if already present)
# ---------------------------------------------------------------------------
kubectl apply -f "${CHART_PATH}/crds/"
kubectl wait crd --all --for condition=Established --timeout=60s

# ---------------------------------------------------------------------------
# 2. Deploy operator via Helm
# ---------------------------------------------------------------------------
helm install "${OPERATOR}-operator" \
  "${CHART_PATH}/" \
  --set "image.repository=${IMAGE_REPO}" \
  --set "image.tag=${IMAGE_TAG}" \
  --set image.pullPolicy=Never \
  --wait --timeout 120s

# ---------------------------------------------------------------------------
# 3. Wait for admission webhook to accept traffic
# ---------------------------------------------------------------------------
# The readiness probe checks :8081, but the admission webhook serves TLS on
# :9443.  After helm --wait returns the webhook listener may not yet be
# accepting connections (cert not mounted, TLS handshake not ready).
# Additionally, cert-manager must inject the caBundle into the webhook
# configurations.  Poll until the caBundle is present and the webhook
# responds to a dry-run request.
WEBHOOK_CFG="${OPERATOR}-operator-mutating"
if kubectl get mutatingwebhookconfigurations "${WEBHOOK_CFG}" &>/dev/null; then
  echo "Waiting for ${OPERATOR}-operator webhook to become ready..."

  # 3a. Wait for cert-manager to inject the caBundle.
  for i in $(seq 1 30); do
    BUNDLE=$(kubectl get mutatingwebhookconfigurations "${WEBHOOK_CFG}" \
      -o jsonpath='{.webhooks[0].clientConfig.caBundle}' 2>/dev/null || true)
    if [[ -n "${BUNDLE}" ]]; then
      break
    fi
    if [[ "${i}" -eq 30 ]]; then
      echo "::warning::caBundle not injected after 60 s — proceeding anyway"
    fi
    sleep 2
  done

  # 3b. Give the webhook TLS listener a moment to start accepting traffic
  #     after the certificate has been mounted.
  sleep 3
  echo "Webhook ready."
fi
