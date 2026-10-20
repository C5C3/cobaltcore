#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/perf-token-baseline.sh — Keystone token issue and validate latency.
#
# Applies one Keystone CR to a running kind stack (see `make deploy-infra`),
# waits for it to become Ready, and runs the token stream of the chaos workload
# harness (tests/e2e-chaos/lib/workload.sh) against it for DURATION_SECONDS.
# `hack/perf-baseline.py token` then reads the stream's records and writes the
# p50/p99 of token issue and validate, with their sample and failure counts, to
# RESULT_JSON. The harness's client is the one the chaos suites drive the token
# path with, so the baseline measures that code path and no second one.
#
# The nightly Performance workflow (.github/workflows/performance.yaml) runs it
# and compares RESULT_JSON against tests/perf/baseline.json. See
# docs/reference/testing/reconcile-performance-benchmark.md.
#
# Exit codes: 0 measured; 1 a failed preflight, a CR that never became Ready, a
# workload that did not start, too few samples or a failed record; 2 a log
# without a token record. Once the preflight passed, the EXIT trap stops the
# workload pod and deletes the CR on every exit.
#
# Environment variables:
#   NAMESPACE             namespace of the CR and the workload pod (default openstack)
#   CR_NAME               Keystone CR name                   (default keystone-perf-token)
#   DURATION_SECONDS      how long the token stream runs     (default 600)
#   MIN_SAMPLES           ok records each of issue and validate needs (default 300)
#   READY_TIMEOUT         kubectl wait and delete timeout    (default 600s)
#   OPERATOR_NAMESPACE    namespace of the operator Deployment (default keystone-system)
#   OPERATOR_DEPLOY       operator Deployment name           (default keystone-operator)
#   DB_CLUSTER_REF        managed MariaDB cluster name       (default openstack-db)
#   CACHE_CLUSTER_REF     managed memcached cluster name     (default openstack-memcached)
#   IMAGE_REPOSITORY      keystone image repository   (default ghcr.io/c5c3/keystone)
#   IMAGE_TAG             keystone image tag, recorded as meta.release (default 2026.1)
#   RESULT_JSON           the JSON file written    (default _output/perf/token.json)
#   CHAOS_WORKLOAD_IMAGE  the workload pod image, read by the harness
#                         (its default is ghcr.io/c5c3/tempest:2026.1)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
# shellcheck source=hack/perf-keystone-cr.sh
source "${SCRIPT_DIR}/perf-keystone-cr.sh"

NAMESPACE="${NAMESPACE:-openstack}"
# workload.sh reads the namespace of its pod from the environment.
export NAMESPACE
CR_NAME="${CR_NAME:-keystone-perf-token}"
DURATION_SECONDS="${DURATION_SECONDS:-600}"
MIN_SAMPLES="${MIN_SAMPLES:-300}"
READY_TIMEOUT="${READY_TIMEOUT:-600s}"
OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE:-keystone-system}"
OPERATOR_DEPLOY="${OPERATOR_DEPLOY:-keystone-operator}"
DB_CLUSTER_REF="${DB_CLUSTER_REF:-openstack-db}"
CACHE_CLUSTER_REF="${CACHE_CLUSTER_REF:-openstack-memcached}"
IMAGE_REPOSITORY="${IMAGE_REPOSITORY:-ghcr.io/c5c3/keystone}"
IMAGE_TAG="${IMAGE_TAG:-2026.1}"
RESULT_JSON="${RESULT_JSON:-_output/perf/token.json}"

WORKLOAD_ID="perf-token"
WORKLOAD_POD="chaos-wl-${WORKLOAD_ID}"
WORKLOAD_SH="${REPO_ROOT}/tests/e2e-chaos/lib/workload.sh"
BASELINE_PY="${REPO_ROOT}/hack/perf-baseline.py"
WORKDIR=""

log() { printf '>> %s\n' "$*" >&2; }
err() { printf '::error:: %s\n' "$*" >&2; }

# cleanup stops the workload pod and deletes the CR, keeping the exit code of
# the script.
cleanup() {
  local rc=$?
  "${WORKLOAD_SH}" stop "${WORKLOAD_ID}" >/dev/null 2>&1 || true
  kubectl -n "${NAMESPACE}" delete keystone "${CR_NAME}" --ignore-not-found \
    --wait=true --timeout="${READY_TIMEOUT}" >/dev/null 2>&1 || true
  rm -rf "${WORKDIR}"
  exit "${rc}"
}

# preflight verifies kubectl can reach the cluster, the operator Deployment
# exists and the tools the measurement runs are present, before any CR is
# created. The failure modes are separated so the remediation message is
# actionable.
preflight() {
  if ! kubectl version --request-timeout=5s >/dev/null 2>&1; then
    err "kubectl cannot reach a cluster; deploy the stack first (make deploy-infra)"
    exit 1
  fi
  if ! kubectl -n "${OPERATOR_NAMESPACE}" get deploy "${OPERATOR_DEPLOY}" >/dev/null 2>&1; then
    err "operator Deployment ${OPERATOR_NAMESPACE}/${OPERATOR_DEPLOY} not found; deploy the operator first"
    exit 1
  fi
  if ! command -v python3 >/dev/null 2>&1; then
    err "python3 is required"
    exit 1
  fi
  if [[ ! -x "${WORKLOAD_SH}" ]]; then
    err "${WORKLOAD_SH} not found"
    exit 1
  fi
}

main() {
  preflight
  WORKDIR="$(mktemp -d)"
  trap cleanup EXIT

  log "applying Keystone ${NAMESPACE}/${CR_NAME}"
  perf_keystone_cr "${CR_NAME}" keystone_perf_token >"${WORKDIR}/cr.yaml"
  kubectl apply -f "${WORKDIR}/cr.yaml" >/dev/null
  log "waiting for ${NAMESPACE}/${CR_NAME} to become Ready (timeout ${READY_TIMEOUT})"
  if ! kubectl -n "${NAMESPACE}" wait --for=condition=Ready --timeout="${READY_TIMEOUT}" "keystone/${CR_NAME}" >/dev/null; then
    err "Keystone ${NAMESPACE}/${CR_NAME} did not become Ready within ${READY_TIMEOUT}"
    exit 1
  fi

  if ! "${WORKLOAD_SH}" start "${WORKLOAD_ID}" -- \
    --streams token --keystone "http://${CR_NAME}.${NAMESPACE}.svc:5000/v3"; then
    err "the token workload did not start"
    exit 1
  fi
  log "measuring token issue and validate for ${DURATION_SECONDS}s"
  sleep "${DURATION_SECONDS}"
  kubectl -n "${NAMESPACE}" logs "${WORKLOAD_POD}" >"${WORKDIR}/token.log"
  "${WORKLOAD_SH}" stop "${WORKLOAD_ID}" >/dev/null

  python3 "${BASELINE_PY}" token --log "${WORKDIR}/token.log" --release "${IMAGE_TAG}" \
    --min-samples "${MIN_SAMPLES}" --out "${RESULT_JSON}"
}

main "$@"
