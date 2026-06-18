#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Shared chaos test diagnostics helper.
# Called from Chainsaw catch blocks to collect uniform diagnostic output
# when a test step fails.
#
# Usage:
#   diagnostics.sh baseline <cr-name> [options]
#   diagnostics.sh chaos    <cr-name> [options]
#
# Modes:
#   baseline  Pre-chaos diagnostics: CR status, all pod logs, events.
#   chaos     Post-chaos / recovery diagnostics: CR status, dependency
#             pod status, Chaos Mesh status, pod logs, events.
#
# Options:
#   --dep-label=LABEL     Label selector for dependency pods (chaos mode).
#   --dep-ns=NAMESPACE    Namespace for dependency pods (default: $NAMESPACE).
#   --log-label=LABEL     Label selector for log collection. Defaults to
#                         --dep-label if not specified.
#   --eso                 Include ESO ExternalSecret condition diagnostics.

set -euo pipefail

MODE="${1:?Usage: diagnostics.sh <baseline|chaos> <cr-name> [options]}"
CR_NAME="${2:?Usage: diagnostics.sh <baseline|chaos> <cr-name> [options]}"
shift 2

# Parse options.
DEP_LABEL=""
DEP_NS="${NAMESPACE:-openstack}"
LOG_LABEL=""
INCLUDE_ESO=false

for arg in "$@"; do
  case "$arg" in
    --dep-label=*) DEP_LABEL="${arg#--dep-label=}" ;;
    --dep-ns=*)    DEP_NS="${arg#--dep-ns=}" ;;
    --log-label=*) LOG_LABEL="${arg#--log-label=}" ;;
    --eso)         INCLUDE_ESO=true ;;
    *) echo "Unknown option: $arg" >&2; exit 1 ;;
  esac
done

# Default log label to dependency label.
LOG_LABEL="${LOG_LABEL:-$DEP_LABEL}"

# ── Common: Keystone CR status ──────────────────────────────────────────────
echo "=== Keystone CR status ==="
kubectl get keystone "$CR_NAME" -n "$NAMESPACE" -o yaml 2>&1 || true

case "$MODE" in
  baseline)
    # ── Baseline: all pod logs in namespace ──────────────────────────────────
    echo "=== All pod logs ==="
    for pod in $(kubectl get pods -n "$NAMESPACE" -o name 2>/dev/null || true); do
      echo "--- $pod ---"
      kubectl logs -n "$NAMESPACE" "$pod" --all-containers --tail=60 2>&1 || true
    done

    # ── Baseline: dependency pod status (optional, e.g. cross-namespace) ─────
    if [ -n "$DEP_LABEL" ]; then
      echo "=== Dependency pod status ==="
      kubectl get pods -n "$DEP_NS" -l "$DEP_LABEL" -o wide 2>&1 || true
    fi
    ;;

  chaos)
    # ── Chaos: dependency pod status ─────────────────────────────────────────
    if [ -n "$DEP_LABEL" ]; then
      echo "=== Dependency pod status ==="
      kubectl get pods -n "$DEP_NS" -l "$DEP_LABEL" -o wide 2>&1 || true
    fi

    # ── Chaos: Chaos Mesh experiment status ──────────────────────────────────
    # Report both PodChaos and NetworkChaos experiments.
    echo "=== Chaos Mesh experiment status ==="
    kubectl get podchaos,networkchaos -n "$NAMESPACE" -o wide 2>&1 || true

    # ── Chaos: ESO ExternalSecret conditions (optional) ──────────────────────
    if [ "$INCLUDE_ESO" = true ]; then
      echo "=== ESO ExternalSecret conditions ==="
      kubectl get externalsecrets -n "$NAMESPACE" -o wide 2>&1 || true
      for es in $(kubectl get externalsecrets -n "$NAMESPACE" -o name 2>/dev/null || true); do
        echo "--- $es ---"
        kubectl get -n "$NAMESPACE" "$es" -o jsonpath='{.status.conditions}' 2>&1 || true
        echo
      done
    fi

    # ── Chaos: pod logs with --previous ──────────────────────────────────────
    if [ -n "$LOG_LABEL" ]; then
      echo "=== Pod logs (current + previous) ==="
      for pod in $(kubectl get pods -n "$NAMESPACE" -l "$LOG_LABEL" -o name 2>/dev/null || true); do
        echo "--- $pod ---"
        kubectl logs -n "$NAMESPACE" "$pod" --all-containers --tail=60 2>&1 || true
        kubectl logs -n "$NAMESPACE" "$pod" --all-containers --tail=60 --previous 2>&1 || true
      done
    fi
    ;;

  *)
    echo "Unknown mode: $MODE (expected 'baseline' or 'chaos')" >&2
    exit 1
    ;;
esac

# ── Common: namespace events ────────────────────────────────────────────────
echo "=== Events ==="
kubectl get events -n "$NAMESPACE" --sort-by='.lastTimestamp' 2>&1 | tail -30 || true
