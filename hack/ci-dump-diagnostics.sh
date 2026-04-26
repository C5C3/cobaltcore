#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-dump-diagnostics.sh — Dump diagnostic info after E2E failures.
# Feature: CC-0050
#
# Consolidates diagnostic dump logic shared across e2e-infra, e2e-operator,
# and tempest CI jobs into a single script. Usable locally against any
# kubeconfig.
#
# Usage:
#   hack/ci-dump-diagnostics.sh                   # infra-only diagnostics
#   OPERATOR=keystone hack/ci-dump-diagnostics.sh  # + operator-specific diagnostics
#
# REQ-001: Shared diagnostic dump script for all E2E jobs.
# REQ-007: set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

# Optional: operator name for operator-specific diagnostics.
OPERATOR="${OPERATOR:-}"
NAMESPACE="${NAMESPACE:-openstack}"

# ---------------------------------------------------------------------------
# Infrastructure diagnostics (always emitted)
# ---------------------------------------------------------------------------
echo "=== HelmReleases ==="
kubectl get helmrelease --all-namespaces || true

echo "=== Pods ==="
kubectl get pods --all-namespaces || true

echo "=== DaemonSets ==="
kubectl get daemonsets --all-namespaces -o wide || true

# Chaos Mesh is opt-in in the kind Quick Start (CC-0097): the chaos-mesh
# namespace only exists when the cluster was deployed with
# WITH_CHAOS_MESH=true. Guard the describe so the log emits an explicit
# SKIP line instead of swallowing the not-found error silently.
echo "=== DaemonSet chaos-daemon detail ==="
if kubectl get ns chaos-mesh >/dev/null 2>&1; then
  kubectl describe daemonset -n chaos-mesh chaos-daemon 2>/dev/null || true
else
  echo "SKIP: chaos-mesh namespace not present (install with WITH_CHAOS_MESH=true)"
fi

echo "=== Events (last 50) ==="
kubectl get events --all-namespaces --sort-by='.lastTimestamp' | tail -50 || true

# Diagnostics-only Flux CLI block. After the FluxInstance migration
# (CC-0085, REQ-004) the CLI is opt-in — `hack/install-test-deps.sh` only
# installs it when `WITH_FLUX_CLI=true`, so on the default CI path this
# branch is dormant. The `fluxinstance,fluxreport` dump below compensates.
if command -v flux >/dev/null 2>&1; then
  echo "=== Flux logs ==="
  flux logs --all-namespaces || true
fi

# flux-operator state (CC-0085): emit FluxInstance and FluxReport only when
# the flux-operator CRDs are registered on the cluster; otherwise a plain
# `kubectl get` would error loudly on clusters that have not been bootstrapped
# with flux-operator yet.
if kubectl api-resources --api-group=fluxcd.controlplane.io 2>/dev/null \
    | grep -q '^fluxinstances'; then
  echo "=== FluxInstance / FluxReport ==="
  kubectl get fluxinstance,fluxreport -A -o yaml || true
fi

# ---------------------------------------------------------------------------
# Operator-specific diagnostics (only when OPERATOR is set)
# ---------------------------------------------------------------------------
if [[ -n "${OPERATOR}" ]]; then
  echo "=== Operator pods ==="
  kubectl get pods -l "app.kubernetes.io/name=${OPERATOR}-operator" || true

  echo "=== Operator logs ==="
  kubectl logs -l "app.kubernetes.io/name=${OPERATOR}-operator" --tail=100 || true

  echo "=== Job descriptions ==="
  for job in $(kubectl get jobs -n "${NAMESPACE}" -o name 2>/dev/null); do
    echo "--- describe ${job} ---"
    kubectl describe -n "${NAMESPACE}" "${job}" 2>&1 | tail -30 || true
  done

  echo "=== Failed Job logs ==="
  for job in $(kubectl get jobs -n "${NAMESPACE}" -o name 2>/dev/null); do
    echo "--- logs for ${job} ---"
    kubectl logs -n "${NAMESPACE}" "${job}" --all-containers --tail=50 2>&1 || true
  done

  echo "=== All pod logs in ${NAMESPACE} ==="
  for pod in $(kubectl get pods -n "${NAMESPACE}" -o name 2>/dev/null); do
    echo "--- ${pod} (current) ---"
    kubectl logs -n "${NAMESPACE}" "${pod}" --all-containers --tail=30 2>&1 || true
    echo "--- ${pod} (previous) ---"
    kubectl logs -n "${NAMESPACE}" "${pod}" --all-containers --tail=30 --previous 2>&1 || true
  done

  echo "=== Operator CR status ==="
  kubectl get "${OPERATOR}" -n "${NAMESPACE}" -o yaml 2>/dev/null | grep -A30 "conditions:" || true

  echo "=== ConfigMaps in ${NAMESPACE} namespace ==="
  kubectl get cm -n "${NAMESPACE}" 2>/dev/null || true
fi
