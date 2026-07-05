#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-deploy-operator.sh — Deploy an operator into a kind cluster.
#
# operator runs in its own dedicated Namespace (default keystone-system);
# the Keystone workload (custom resources) remains in the openstack Namespace.
#
# Installs CRDs, waits for establishment, and deploys the operator via Helm
# with the specified container image into NAMESPACE (default: keystone-system).
#
# Required env vars:
#   OPERATOR    — Operator name (e.g. keystone)
#   IMAGE_REPO  — Full image repository (e.g. ghcr.io/c5c3/keystone-operator)
#
# Optional env vars:
#   IMAGE_TAG          — Image tag (default: dev)
#   NAMESPACE          — Release Namespace for the operator (default:
#                        keystone-system). The Namespace is created on demand
#                        via `helm install --create-namespace`.
#   WITH_PROMETHEUS    — When "true", enables the chart's gated ServiceMonitor
#                        template via --set monitoring.serviceMonitor.enabled=true
#                        (default: false). Used by the e2e-prometheus CI job
#                       .
#   CHART_DIR          — Chart directory to install from (default:
#                        operators/${OPERATOR}/helm/${OPERATOR}-operator). Set
#                        this to a chart pulled from GHCR (e.g. via
#                        hack/ci-fetch-released-operator.sh) to install a
#                        previously-released operator as the upgrade baseline.
#                        When CHART_DIR/charts/ already exists (a pulled chart
#                        tarball vendors its operator-library dependency), the
#                        `helm dependency build` step is skipped — the file://
#                        dependency path in a pulled chart does not resolve
#                        outside the repo.
#
# Reusable operator deployment script.
# set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

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
WITH_PROMETHEUS="${WITH_PROMETHEUS:-false}"
# dedicated release Namespace for the operator. The Keystone workload
# CRs themselves are still reconciled in the `openstack` Namespace.
NAMESPACE="${NAMESPACE:-keystone-system}"

# CHART_DIR defaults to the in-repo chart but may point at a chart pulled from
# GHCR (upgrade-baseline install). Trailing slash is normalised so path joins
# below are stable regardless of how the caller passes it.
CHART_PATH="${CHART_DIR:-operators/${OPERATOR}/helm/${OPERATOR}-operator}"
CHART_PATH="${CHART_PATH%/}"

# ---------------------------------------------------------------------------
# 1. Install CRDs (idempotent — kubectl apply succeeds if already present)
# ---------------------------------------------------------------------------
kubectl apply -f "${CHART_PATH}/crds/"
kubectl wait crd --all --for condition=Established --timeout=60s

# ---------------------------------------------------------------------------
# 1b. Vendor chart dependencies
# ---------------------------------------------------------------------------
# The operator chart depends on the operator-library library chart via a
# file:// path, so `helm install` needs it vendored into charts/ first.
# --skip-refresh: the dependency is local, so no chart-repository refresh is
# required (and it avoids failing on a developer's stale repo cache).
#
# A chart pulled from GHCR (CHART_DIR set) already vendors operator-library
# under charts/, and its file:// dependency path does not resolve outside the
# repo tree — so skip `helm dependency build` only for that pulled-chart case.
# For the in-repo chart (CHART_DIR unset) the file:// path resolves, so always
# rebuild: an unconditional `helm dependency build` self-corrects a stale
# charts/ left by a previous local run after the operator-library dependency
# version in Chart.yaml/Chart.lock has moved on. Gating the skip on a populated
# charts/ alone would silently reuse that stale vendored copy.
if [[ -n "${CHART_DIR:-}" ]] && [[ -d "${CHART_PATH}/charts" ]] && [[ -n "$(ls -A "${CHART_PATH}/charts" 2>/dev/null)" ]]; then
  echo "Pulled chart with vendored dependencies (${CHART_PATH}/charts) — skipping dependency build"
else
  helm dependency build --skip-refresh "${CHART_PATH}/"
fi

# ---------------------------------------------------------------------------
# 2. Deploy operator via Helm
# ---------------------------------------------------------------------------
# Build the Helm --set arguments. The ServiceMonitor template is gated on
# monitoring.serviceMonitor.enabled and only enabled when the caller opts in
# via WITH_PROMETHEUS=true. Without this flag the
# kube-prometheus-stack chainsaw suite cannot observe the operator's metrics
# because the ServiceMonitor never renders.
#
# Echo the resolved flag value into the CI log mirroring deploy-infra.sh's
# banner line ("Prometheus stack : ${WITH_PROMETHEUS} ...") so a reader can
# pinpoint at which step the gate flipped without grepping back to the
# workflow YAML.
echo "Prometheus stack    : ${WITH_PROMETHEUS} (set WITH_PROMETHEUS=true to enable ServiceMonitor)"
helm_args=(
  --set "image.repository=${IMAGE_REPO}"
  --set "image.tag=${IMAGE_TAG}"
  --set image.pullPolicy=Never
)
if [[ "${WITH_PROMETHEUS}" == "true" ]]; then
  helm_args+=(--set "monitoring.serviceMonitor.enabled=true")
fi

helm install "${OPERATOR}-operator" \
  "${CHART_PATH}/" \
  --namespace "${NAMESPACE}" \
  --create-namespace \
  "${helm_args[@]}" \
  --wait --timeout 120s

# ---------------------------------------------------------------------------
# 3. Wait for admission webhook to accept traffic
# ---------------------------------------------------------------------------
# The readiness probe checks :8081, but the admission webhook serves TLS on
# 9443.  After helm --wait returns the webhook listener may not yet be
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
