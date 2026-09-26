#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e-autoscaling/check-connection-cap.sh: prove that a service child
# running at its HPA maximum fits the SQL connection caps its operator sized.
#
# Usage:
#   check-connection-cap.sh <child-cr> <sql-user>...
#
# <child-cr> is the service child the ControlPlane projects (for example
# cp-autoscaling-keystone), and each <sql-user> is a Static-mode MariaDB user it
# connects as. The helper checks, in this order, and exits 1 with a named
# reason on the first failure:
#
#   1. exactly one HPA in the namespace carries the child's instance label;
#   2. the HPA's scale-target Deployment has as many ready replicas as the HPA
#      maximum;
#   3. every pod of that Deployment has all containers ready (restart counts
#      are printed, not asserted: a crash loop on MySQL error 1226 is what
#      check 6 catches);
#   4. every such pod holds at least one ESTABLISHED connection to port 3306;
#   5. every <sql-user> has a User CR with a positive spec.maxUserConnections,
#      MariaDB enforces the same figure, and the user's live connections do
#      not exceed it;
#   6. no pod of the child logged MySQL error 1226, in its current or its
#      previous container.
#
# The SQL of check 5 runs as root through mariadb-sql.sh, which keeps the
# root password off every command line.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NS="openstack"

# Counts the ESTABLISHED (state 01) TCP connections to remote port 3306
# (0CEA) in the pod's network namespace. /proc/net/tcp6 is absent on a node
# without IPv6.
readonly COUNT_MARIADB_CONNECTIONS='
n = 0
for path in ("/proc/net/tcp", "/proc/net/tcp6"):
    try:
        with open(path) as f:
            next(f)
            for line in f:
                cols = line.split()
                if cols[2].rsplit(":", 1)[1] == "0CEA" and cols[3] == "01":
                    n += 1
    except FileNotFoundError:
        pass
print(n)
'

fail() {
  echo "ERROR: $*" >&2
  exit 1
}

if [[ $# -lt 2 ]]; then
  echo "usage: $0 <child-cr> <sql-user>..." >&2
  exit 2
fi
CHILD="$1"
shift

ERR="$(mktemp)"
trap 'rm -f "${ERR}"' EXIT

# ── 1. The child's HPA ─────────────────────────────────────────────────────
mapfile -t HPAS < <(kubectl get hpa -n "${NS}" -l "app.kubernetes.io/instance=${CHILD}" \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
if [[ ${#HPAS[@]} -ne 1 ]]; then
  fail "found ${#HPAS[@]} HPAs for ${CHILD}, want 1"
fi
HPA="${HPAS[0]}"
DEPLOY="$(kubectl get hpa "${HPA}" -n "${NS}" -o jsonpath='{.spec.scaleTargetRef.name}')"
MAX="$(kubectl get hpa "${HPA}" -n "${NS}" -o jsonpath='{.spec.maxReplicas}')"

# ── 2. The Deployment at the HPA maximum ───────────────────────────────────
READY="$(kubectl get deployment "${DEPLOY}" -n "${NS}" -o jsonpath='{.status.readyReplicas}')"
READY="${READY:-0}"
if [[ "${READY}" -ne "${MAX}" ]]; then
  fail "${DEPLOY} has ${READY} ready replicas, want ${MAX} (the HPA maximum)"
fi

# ── 3. Every pod of the Deployment ready ───────────────────────────────────
SELECTOR="$(kubectl get deployment "${DEPLOY}" -n "${NS}" -o json \
  | jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")')"
# A pod on its way out (after a rollout or a scale-in) is not part of the
# fleet any more, so it is left out rather than counted as not ready. Checks 3
# and 4 read the pods from this one listing.
PODS_JSON="$(kubectl get pods -n "${NS}" -l "${SELECTOR}" -o json \
  | jq '[.items[] | select(.metadata.deletionTimestamp == null)]')"
mapfile -t PODS < <(jq -r '.[].metadata.name' <<<"${PODS_JSON}")
pod_json() { # <pod>
  jq --arg name "$1" '.[] | select(.metadata.name == $name)' <<<"${PODS_JSON}"
}
for pod in "${PODS[@]}"; do
  p="$(pod_json "${pod}")"
  statuses="$(jq -r '[.status.containerStatuses // [] | .[] | "\(.name) ready=\(.ready) restarts=\(.restartCount)"] | join(", ")' <<<"${p}")"
  echo "pod ${pod}: ${statuses}"
  not_ready="$(jq -r '[.status.containerStatuses // [] | .[] | select(.ready != true)] | length' <<<"${p}")"
  if [[ -z "${statuses}" || "${not_ready}" -ne 0 ]]; then
    fail "pod ${pod} is not ready"
  fi
done

# ── 4. Every pod holds a MariaDB connection ────────────────────────────────
for pod in "${PODS[@]}"; do
  container="$(pod_json "${pod}" | jq -r '.spec.containers[0].name')"
  if ! conns="$(kubectl exec -n "${NS}" "${pod}" -c "${container}" -- \
    python3 -c "${COUNT_MARIADB_CONNECTIONS}" 2>"${ERR}")"; then
    fail "cannot read the connections of pod ${pod}: $(cat "${ERR}")"
  fi
  echo "pod ${pod}: ${conns} ESTABLISHED connection(s) to port 3306"
  if [[ "${conns}" -lt 1 ]]; then
    fail "pod ${pod} holds no MariaDB connection"
  fi
done

# ── 5. Each SQL user within its cap ────────────────────────────────────────
sql() { # <statement>
  local out
  if ! out="$("${HERE}/mariadb-sql.sh" "$1" 2>"${ERR}")"; then
    fail "MariaDB query failed: $(cat "${ERR}")"
  fi
  printf '%s\n' "${out}" | head -n 1
}

SUMMARY=()
for user in "$@"; do
  if ! kubectl get users.k8s.mariadb.com "${user}" -n "${NS}" >/dev/null 2>&1; then
    fail "User ${user} not found in ${NS}"
  fi
  cap="$(kubectl get users.k8s.mariadb.com "${user}" -n "${NS}" -o jsonpath='{.spec.maxUserConnections}')"
  if [[ -z "${cap}" || "${cap}" -le 0 ]]; then
    fail "User ${user} carries no maxUserConnections"
  fi
  enforced="$(sql "SELECT max_user_connections FROM mysql.user WHERE User='${user}'")"
  if [[ "${enforced}" != "${cap}" ]]; then
    fail "${user}: MariaDB enforces ${enforced:-nothing}, the User CR says ${cap}"
  fi
  live="$(sql "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE USER='${user}'")"
  echo "${user}: ${live} live connection(s), cap ${cap}"
  if [[ "${live}" -gt "${cap}" ]]; then
    fail "${user} holds ${live} connections, above its cap of ${cap}"
  fi
  SUMMARY+=("${user}=${live}/${cap}")
done

# ── 6. No pod of the child hit MySQL error 1226 ────────────────────────────
HITS=""
while IFS= read -r pod; do
  [[ -n "${pod}" ]] || continue
  for previous in false true; do
    logs="$(kubectl logs -n "${NS}" "${pod}" --all-containers --prefix --previous="${previous}" 2>/dev/null || true)"
    matches="$(grep -E 'max_user_connections|\(1226,' <<<"${logs}" || true)"
    if [[ -n "${matches}" ]]; then
      HITS+="${matches}"$'\n'
    fi
  done
done < <(kubectl get pods -n "${NS}" -l "app.kubernetes.io/instance=${CHILD}" \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
if [[ -n "${HITS}" ]]; then
  printf '%s' "${HITS}"
  fail "${CHILD} hit MySQL error 1226"
fi

echo "OK: ${CHILD} at its HPA maximum (${#PODS[@]} pods): ${SUMMARY[*]}"
