#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/perf-reconcile-benchmark.sh — reconcile-loop performance benchmark.
#
# Applies N Keystone CRs to a running kind stack (see `make deploy-infra`),
# waits for them to become Ready, lets the operator settle, and reports p50/p95/
# p99 reconcile latency from the operator's Prometheus metrics — both the
# per-sub-reconciler histogram (keystone_operator_reconcile_duration_seconds)
# and the built-in end-to-end histogram
# (controller_runtime_reconcile_time_seconds{controller="keystone"}).
#
# It is the regression gate for the reconcile-performance work (issue #361).
# The nightly Performance workflow (.github/workflows/performance.yaml) runs it
# with CR_COUNTS="1 3" and RESULT_JSON, and hack/perf-baseline.py compares the
# file against tests/perf/baseline.json. The 25-CR default spawns 25
# Deployments plus their Jobs and MariaDB objects, which does not fit a CI kind
# runner: run it locally or on a dedicated cluster. See
# docs/reference/testing/reconcile-performance-benchmark.md.
#
# Environment variables:
#   CR_COUNTS           space-separated CR counts to benchmark (default "1 5 25")
#   NAMESPACE           namespace for the Keystone CRs         (default openstack)
#   OPERATOR_NAMESPACE  namespace of the operator Deployment   (default keystone-system)
#   OPERATOR_DEPLOY     operator Deployment name               (default keystone-operator)
#   METRICS_PORT        operator metrics container port        (default 8080)
#   SETTLE_SECONDS      steady-state sampling window           (default 60)
#   READY_TIMEOUT       kubectl wait timeout for readiness     (default 600s)
#   DB_CLUSTER_REF      managed MariaDB cluster name           (default openstack-db)
#   CACHE_CLUSTER_REF   managed memcached cluster name         (default openstack-memcached)
#   IMAGE_REPOSITORY    keystone image repository   (default ghcr.io/c5c3/keystone)
#   IMAGE_TAG           keystone image tag                     (default 2026.1)
#   GATE_P95_SECONDS    if set, exit non-zero when the steady-state end-to-end
#                       p95 exceeds this many seconds (the D3 SLO as a gate)
#   RESULT_JSON         if set, also write the six quantiles of every count to
#                       this JSON file, a nan as null, with IMAGE_TAG as
#                       meta.release; the report on stdout stays the same

set -euo pipefail
# awk follows the locale: a comma-decimal one reads the bucket bound 0.025 as 0
# and prints 0,0350, which RESULT_JSON cannot hold as a number.
export LC_ALL=C

CR_COUNTS="${CR_COUNTS:-1 5 25}"
NAMESPACE="${NAMESPACE:-openstack}"
OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE:-keystone-system}"
OPERATOR_DEPLOY="${OPERATOR_DEPLOY:-keystone-operator}"
METRICS_PORT="${METRICS_PORT:-8080}"
SETTLE_SECONDS="${SETTLE_SECONDS:-60}"
READY_TIMEOUT="${READY_TIMEOUT:-600s}"
DB_CLUSTER_REF="${DB_CLUSTER_REF:-openstack-db}"
CACHE_CLUSTER_REF="${CACHE_CLUSTER_REF:-openstack-memcached}"
IMAGE_REPOSITORY="${IMAGE_REPOSITORY:-ghcr.io/c5c3/keystone}"
IMAGE_TAG="${IMAGE_TAG:-2026.1}"
GATE_P95_SECONDS="${GATE_P95_SECONDS:-}"
RESULT_JSON="${RESULT_JSON:-}"

CR_PREFIX="keystone-perf"
WORKDIR="$(mktemp -d)"
PORT_FORWARD_PID=""
PF_LOCAL_PORT=""
GATE_FAILED=0

log() { printf '>> %s\n' "$*" >&2; }
err() { printf '::error:: %s\n' "$*" >&2; }

cleanup() {
  stop_port_forward
  delete_crs
  rm -rf "${WORKDIR}"
}
trap cleanup EXIT

# preflight verifies kubectl can reach the cluster and the operator Deployment
# exists before any CRs are created. The two failure modes are separated so the
# remediation message is actionable.
preflight() {
  if ! kubectl version --request-timeout=5s >/dev/null 2>&1; then
    err "kubectl cannot reach a cluster; deploy the stack first (make deploy-infra)"
    exit 1
  fi
  if ! kubectl -n "${OPERATOR_NAMESPACE}" get deploy "${OPERATOR_DEPLOY}" >/dev/null 2>&1; then
    err "operator Deployment ${OPERATOR_NAMESPACE}/${OPERATOR_DEPLOY} not found; deploy the operator first"
    exit 1
  fi
}

# cr_names prints the resource refs (keystone/<name>) of every benchmark CR
# currently present, one per line.
cr_names() {
  kubectl -n "${NAMESPACE}" get keystone -o name 2>/dev/null | grep "/${CR_PREFIX}-" || true
}

# gen_crs writes a manifest of $1 Keystone CRs to $2. Each CR gets a unique name
# and a unique database name so parallel CRs never share a schema.
gen_crs() {
  local count="$1" out="$2" i
  : >"${out}"
  for ((i = 1; i <= count; i++)); do
    cat >>"${out}" <<EOF
---
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: Keystone
metadata:
  name: ${CR_PREFIX}-${i}
  namespace: ${NAMESPACE}
spec:
  deployment:
    replicas: 1
  image:
    repository: ${IMAGE_REPOSITORY}
    tag: "${IMAGE_TAG}"
  database:
    clusterRef:
      name: ${DB_CLUSTER_REF}
    database: keystone_perf_${i}
    secretRef:
      name: keystone-db
  cache:
    clusterRef:
      name: ${CACHE_CLUSTER_REF}
    backend: dogpile.cache.pymemcache
  fernet:
    rotationSchedule: "0 0 * * 0"
    maxActiveKeys: 3
  bootstrap:
    adminUser: admin
    adminPasswordSecretRef:
      name: keystone-admin
    region: RegionOne
EOF
  done
}

# delete_crs removes every benchmark CR and waits for finalizers to complete so
# the next count starts from a clean slate.
delete_crs() {
  local names
  names="$(cr_names)"
  if [[ -n "${names}" ]]; then
    # shellcheck disable=SC2086 # names is a newline list of resource refs
    kubectl -n "${NAMESPACE}" delete ${names} --wait=true --timeout="${READY_TIMEOUT}" >/dev/null 2>&1 || true
  fi
}

# start_port_forward opens a background port-forward to the operator metrics
# port and records the chosen local port in PF_LOCAL_PORT.
start_port_forward() {
  PF_LOCAL_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
  kubectl -n "${OPERATOR_NAMESPACE}" port-forward "deploy/${OPERATOR_DEPLOY}" \
    "${PF_LOCAL_PORT}:${METRICS_PORT}" >/dev/null 2>&1 &
  PORT_FORWARD_PID="$!"
  local i
  for ((i = 0; i < 30; i++)); do
    if curl -sf "http://127.0.0.1:${PF_LOCAL_PORT}/metrics" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  err "metrics port-forward to ${OPERATOR_NAMESPACE}/${OPERATOR_DEPLOY} never became ready"
  exit 1
}

stop_port_forward() {
  if [[ -n "${PORT_FORWARD_PID}" ]]; then
    kill "${PORT_FORWARD_PID}" >/dev/null 2>&1 || true
    wait "${PORT_FORWARD_PID}" 2>/dev/null || true
    PORT_FORWARD_PID=""
  fi
}

# scrape writes the operator's /metrics output to $1.
scrape() {
  curl -sf "http://127.0.0.1:${PF_LOCAL_PORT}/metrics" >"$1"
}

# quantile computes the histogram_quantile of a bucket metric family between two
# scrapes, using the same "sum by (le)" aggregation and linear interpolation as
# Prometheus. Args: <before-file> <after-file> <metric> <label-filter> <q>.
# label-filter is a substring required on each bucket line (empty matches the
# whole family). Prints the quantile in seconds, or "nan" when no samples fell
# in the window.
quantile() {
  local before="$1" after="$2" metric="$3" filter="$4" q="$5"
  awk -v metric="${metric}" -v filter="${filter}" -v q="${q}" '
    function le_of(line) {
      if (match(line, /le="[^"]*"/)) return substr(line, RSTART + 4, RLENGTH - 5)
      return ""
    }
    function is_row(line) {
      return (index(line, metric "_bucket{") == 1) && (filter == "" || index(line, filter) > 0)
    }
    FNR == NR { if (is_row($0)) before[le_of($0)] += $NF; next }
    { if (is_row($0)) after[le_of($0)] += $NF }
    END {
      total = 0; n = 0
      for (le in after) {
        d = after[le] - (le in before ? before[le] : 0)
        if (le == "+Inf") { total = d; continue }
        bound[++n] = le + 0
        cum[le + 0] = d
      }
      if (total <= 0 || n == 0) { print "nan"; exit }
      # ascending sort of finite boundaries
      for (i = 1; i <= n; i++)
        for (j = i + 1; j <= n; j++)
          if (bound[j] < bound[i]) { t = bound[i]; bound[i] = bound[j]; bound[j] = t }
      rank = q * total
      prevBound = 0; prevCum = 0
      for (i = 1; i <= n; i++) {
        b = bound[i]; cc = cum[b]
        if (cc >= rank) {
          span = cc - prevCum
          frac = (span > 0) ? (rank - prevCum) / span : 0
          printf "%.4f\n", prevBound + frac * (b - prevBound)
          exit
        }
        prevBound = b; prevCum = cc
      }
      # rank falls in the +Inf bucket: report the highest finite boundary
      printf "%.4f\n", bound[n]
    }
  ' "${before}" "${after}"
}

# write_count_json writes the six quantiles of one count to
# ${WORKDIR}/count-<count>.json, keyed reconcile.<count>.<name>, a nan as null.
# Args: <count> <e2e_p50> <e2e_p95> <e2e_p99> <sub_p50> <sub_p95> <sub_p99>.
write_count_json() {
  local count="$1" name value sep="{"
  shift
  for name in e2e_p50_s e2e_p95_s e2e_p99_s sub_p50_s sub_p95_s sub_p99_s; do
    value="$1"
    shift
    if [[ "${value}" == "nan" ]]; then
      value=null
    fi
    printf '%s"reconcile.%s.%s": %s' "${sep}" "${count}" "${name}" "${value}"
    sep=", "
  done >"${WORKDIR}/count-${count}.json"
  printf '}\n' >>"${WORKDIR}/count-${count}.json"
}

# merge_results merges the count files into RESULT_JSON under the meta block
# hack/perf-baseline.py compare reads the release from.
merge_results() {
  mkdir -p "$(dirname "${RESULT_JSON}")"
  python3 - "${RESULT_JSON}" "${IMAGE_TAG}" "${CR_COUNTS}" "${SETTLE_SECONDS}" "${WORKDIR}"/count-*.json <<'PY'
import datetime
import json
import sys

out, release, cr_counts, settle, *parts = sys.argv[1:]
metrics = {}
for part in parts:
    with open(part, encoding="utf-8") as handle:
        metrics.update(json.load(handle))
measured_at = datetime.datetime.now(datetime.timezone.utc)
result = {
    "meta": {
        "release": release,
        "measured_at": measured_at.isoformat(timespec="seconds"),
        "cr_counts": cr_counts,
        "settle_seconds": int(settle),
    },
    "metrics": metrics,
}
with open(out, "w", encoding="utf-8") as handle:
    json.dump(result, handle, indent=2, sort_keys=True)
    handle.write("\n")
PY
  log "wrote ${RESULT_JSON}"
}

run_count() {
  local count="$1"
  log "benchmarking ${count} Keystone CR(s)"

  gen_crs "${count}" "${WORKDIR}/crs-${count}.yaml"
  kubectl apply -f "${WORKDIR}/crs-${count}.yaml" >/dev/null

  local names
  names="$(cr_names | paste -sd' ' -)"
  log "waiting for ${count} CR(s) to become Ready (timeout ${READY_TIMEOUT})"
  # shellcheck disable=SC2086 # names is a space-separated list of resource refs
  kubectl -n "${NAMESPACE}" wait --for=condition=Ready --timeout="${READY_TIMEOUT}" ${names} >/dev/null

  start_port_forward
  log "sampling steady-state reconcile latency over ${SETTLE_SECONDS}s"
  scrape "${WORKDIR}/before-${count}.txt"
  sleep "${SETTLE_SECONDS}"
  scrape "${WORKDIR}/after-${count}.txt"
  stop_port_forward

  local before="${WORKDIR}/before-${count}.txt" after="${WORKDIR}/after-${count}.txt"
  local e2e_metric="controller_runtime_reconcile_time_seconds"
  local e2e_filter='controller="keystone"'
  local sub_metric="keystone_operator_reconcile_duration_seconds"

  local e2e_p50 e2e_p95 e2e_p99 sub_p50 sub_p95 sub_p99
  e2e_p50="$(quantile "${before}" "${after}" "${e2e_metric}" "${e2e_filter}" 0.50)"
  e2e_p95="$(quantile "${before}" "${after}" "${e2e_metric}" "${e2e_filter}" 0.95)"
  e2e_p99="$(quantile "${before}" "${after}" "${e2e_metric}" "${e2e_filter}" 0.99)"
  sub_p50="$(quantile "${before}" "${after}" "${sub_metric}" "" 0.50)"
  sub_p95="$(quantile "${before}" "${after}" "${sub_metric}" "" 0.95)"
  sub_p99="$(quantile "${before}" "${after}" "${sub_metric}" "" 0.99)"

  printf '\n=== Reconcile latency for %s CR(s) (settle %ss) ===\n' "${count}" "${SETTLE_SECONDS}"
  printf '  end-to-end (controller_runtime_reconcile_time_seconds): p50=%ss p95=%ss p99=%ss\n' \
    "${e2e_p50}" "${e2e_p95}" "${e2e_p99}"
  printf '  sub-reconciler (keystone_operator_reconcile_duration_seconds): p50=%ss p95=%ss p99=%ss\n\n' \
    "${sub_p50}" "${sub_p95}" "${sub_p99}"

  if [[ -n "${RESULT_JSON}" ]]; then
    write_count_json "${count}" "${e2e_p50}" "${e2e_p95}" "${e2e_p99}" "${sub_p50}" "${sub_p95}" "${sub_p99}"
  fi

  if [[ -n "${GATE_P95_SECONDS}" && "${e2e_p95}" != "nan" ]]; then
    if awk -v v="${e2e_p95}" -v g="${GATE_P95_SECONDS}" 'BEGIN { exit !(v + 0 > g + 0) }'; then
      err "end-to-end p95 ${e2e_p95}s exceeds gate ${GATE_P95_SECONDS}s for ${count} CR(s)"
      GATE_FAILED=1
    fi
  fi

  delete_crs
}

main() {
  preflight
  local count
  for count in ${CR_COUNTS}; do
    run_count "${count}"
  done
  # Before the gate's exit, so a red gate still leaves the file behind.
  if [[ -n "${RESULT_JSON}" ]]; then
    merge_results
  fi
  if [[ "${GATE_FAILED}" == "1" ]]; then
    exit 1
  fi
}

main "$@"
