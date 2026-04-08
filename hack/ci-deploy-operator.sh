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
