#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/keystone/eso-down/scale-eso.sh — Scale the external-secrets
# Deployment to a given replica count and wait for the rollout to converge
# (CC-0102, REQ-004, REQ-008).
#
# Usage:
#   scale-eso.sh <replicas>
#
# The eso-down E2E test scales ESO to 0 to prove the Keystone operator does
# NOT spuriously flip Ready=True while the upstream syncer is absent
# (reconcile_secrets.go:86 WaitForExternalSecret == false branch). The same
# helper is then re-invoked with the captured original replica count to
# restore ESO during cleanup.
#
# Idempotency (REQ-008):
#   `kubectl scale --replicas=N` is desired-state semantics — running it twice
#   with the same N is a no-op. The post-scale wait is also idempotent: if the
#   Deployment is already at the target state the loop exits on the first
#   sample. This matters because Chainsaw cleanup blocks may run twice in
#   soak conditions.
#
# Convergence semantics:
#   - For N == 0: wait until .status.replicas == 0 AND .status.availableReplicas
#     is absent (kubectl emits no field when the count is 0), confirming every
#     pod has actually terminated. A "scaled to 0 with one terminating pod
#     still serving traffic" window would let the operator's WaitForExternal-
#     Secret race observe a still-Ready ExternalSecret and produce a flaky
#     Ready=True flicker — exactly the regression REQ-004 guards against.
#   - For N >= 1: wait until .status.availableReplicas == N AND
#     .status.observedGeneration >= .metadata.generation, ensuring the
#     deployment controller has reacted to the scale and the pods are passing
#     their readiness probes.
#
# REQ-004, REQ-008 (CC-0102)

set -euo pipefail

REPLICAS="${1:?Usage: scale-eso.sh <replicas>}"

# Validate input early — a non-integer would surface as an opaque kubectl
# error 30 seconds into the wait loop.
if ! [[ "${REPLICAS}" =~ ^[0-9]+$ ]]; then
  echo "scale-eso: ERROR: replica count must be a non-negative integer (got: ${REPLICAS})" >&2
  exit 1
fi

# Use ESO_NAMESPACE (not NAMESPACE) because chainsaw injects $NAMESPACE into
# script env from the test's spec.namespace (openstack), which would silently
# override a NAMESPACE default and make us look for the deployment in the
# wrong namespace.
ESO_NAMESPACE="${ESO_NAMESPACE:-external-secrets}"
ESO_DEPLOYMENT="${ESO_DEPLOYMENT:-external-secrets}"

# Convergence bound: 120s is comfortably above the chart's terminationGracePeriod
# (default 30s) and the kubelet readiness retry interval at start-up. A
# deployment that has not converged in 120s is a real environmental failure,
# not a slow scheduler — bail out so chainsaw's catch block surfaces the
# diagnostics rather than letting the test hang.
WAIT_SECONDS="${WAIT_SECONDS:-120}"
POLL_INTERVAL="${POLL_INTERVAL:-2}"

log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] scale-eso: $*"
}

# Issue the scale. kubectl scale's --current-replicas is intentionally NOT
# used: enforcing a precondition would defeat idempotency on a re-run, and
# scaling 0->0 / N->N is already a no-op at the API server.
do_scale() {
  log "Scaling deployment.apps/${ESO_DEPLOYMENT} -n ${ESO_NAMESPACE} to ${REPLICAS} replicas..."
  kubectl scale "deployment.apps/${ESO_DEPLOYMENT}" \
    -n "${ESO_NAMESPACE}" \
    --replicas="${REPLICAS}" >/dev/null
}

# Wait for the deployment status to converge to the requested replica count.
# Reads .spec.replicas, .status.replicas, .status.availableReplicas and
# .status.observedGeneration vs .metadata.generation in a single call to
# minimise the API server hit rate.
wait_for_convergence() {
  local deadline=$(( $(date +%s) + WAIT_SECONDS ))
  local jsonpath='{.metadata.generation}{","}{.status.observedGeneration}{","}{.spec.replicas}{","}{.status.replicas}{","}{.status.availableReplicas}'
  while true; do
    # `2>/dev/null || true` swallows transient API errors during a controller
    # restart — the deadline below provides the hard timeout. We deliberately
    # require a non-empty `raw` AND a non-empty `gen` field below before
    # accepting any "converged" decision — otherwise an API blip when the
    # target is 0 would leave every field empty (defaulted to "0") and
    # spuriously report convergence on the very first iteration.
    local raw
    raw=$(kubectl get "deployment.apps/${ESO_DEPLOYMENT}" -n "${ESO_NAMESPACE}" \
      -o jsonpath="${jsonpath}" 2>/dev/null) || raw=""

    local gen observed spec_r status_r avail_r
    IFS=',' read -r gen observed spec_r status_r avail_r <<<"${raw}"
    # jsonpath returns empty for absent fields; normalise to 0 *only* for the
    # numeric comparisons below. `gen` stays unset/empty so the guard catches
    # a failed get (where every captured value is empty).
    observed="${observed:-0}"
    spec_r="${spec_r:-0}"
    status_r="${status_r:-0}"
    avail_r="${avail_r:-0}"

    # Require .metadata.generation to be present and non-empty; an empty
    # `gen` means the kubectl get either failed entirely or returned a
    # malformed object, neither of which can prove convergence.
    if [[ -n "${gen}" && "${observed}" -ge "${gen}" \
       && "${spec_r}" -eq "${REPLICAS}" \
       && "${status_r}" -eq "${REPLICAS}" \
       && "${avail_r}" -eq "${REPLICAS}" ]]; then
      log "  converged: spec=${spec_r} status=${status_r} available=${avail_r} (gen=${gen}/observed=${observed})"
      return 0
    fi

    if [[ $(date +%s) -ge ${deadline} ]]; then
      log "ERROR: deployment.apps/${ESO_DEPLOYMENT} did not converge to replicas=${REPLICAS} within ${WAIT_SECONDS}s"
      log "  last sample: gen=${gen:-?} observed=${observed} spec=${spec_r} status=${status_r} available=${avail_r}"
      kubectl get "deployment.apps/${ESO_DEPLOYMENT}" -n "${ESO_NAMESPACE}" -o wide >&2 2>&1 || true
      kubectl get pods -n "${ESO_NAMESPACE}" \
        -l app.kubernetes.io/name=external-secrets -o wide >&2 2>&1 || true
      exit 1
    fi

    log "  waiting: spec=${spec_r} status=${status_r} available=${avail_r} (target=${REPLICAS})"
    sleep "${POLL_INTERVAL}"
  done
}

main() {
  do_scale
  wait_for_convergence
  log "deployment.apps/${ESO_DEPLOYMENT} is at ${REPLICAS} replicas"
}

main "$@"
