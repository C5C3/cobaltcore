#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-rbac-usage-report.sh — Report which RBAC grants an operator used.
#
# Reads the kube-apiserver audit log of a kind cluster created with
# hack/kind-config-audit.yaml, keeps the events of the operator's ServiceAccount
# (<op>-operator in <op>-system, as hack/ci-deploy-operator.sh installs it), and
# runs hack/audit-rbac-usage.py on them: the triples the ServiceAccount used
# that its ClusterRole does not grant, and the granted verbs nothing used.
#
# The e2e-operator and e2e-controlplane jobs run it after their suites as the
# non-gating `Report RBAC usage` step and upload OUT_DIR as the
# rbac-usage-<operator> artifact; `make audit-rbac-usage` runs it against a
# local cluster. See docs/reference/testing/rbac-usage-audit.md.
#
# OUT_DIR receives:
#   audit.jsonl      the operator's audit events, the input of the diff; the
#                    files of several jobs can be diffed together by passing
#                    each to hack/audit-rbac-usage.py with --audit-log
#   <operator>.json  the findings, as hack/audit-rbac-usage.py --json writes them
#
# Required env vars:
#   OPERATOR             — operator to report on (keystone, c5c3, ...)
#
# Optional env vars:
#   KIND_CLUSTER         — kind cluster whose control-plane node holds the log
#                          (default: cobaltcore)
#   OPERATOR_NAMESPACE   — namespace of the operator's ServiceAccount
#                          (default: <operator>-system)
#   OUT_DIR              — report directory (default: _output/rbac-usage)
#
# Exit codes: 0 after a report, also one with findings; 1 when the node holds
# no audit log or python3 lacks PyYAML; otherwise the exit code of
# hack/audit-rbac-usage.py (2 when no event carries the operator's username).
#
# Usage:
#   OPERATOR=keystone hack/ci-rbac-usage-report.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

OPERATOR="${OPERATOR:-}"
KIND_CLUSTER="${KIND_CLUSTER:-cobaltcore}"
OUT_DIR="${OUT_DIR:-_output/rbac-usage}"

# audit-log-path and audit-log-maxbackup of hack/kind-config-audit.yaml.
AUDIT_LOG_DIR="/var/log/kubernetes"
AUDIT_LOG_MAXBACKUP=10

if [[ -z "${OPERATOR}" ]]; then
  echo "::error::OPERATOR must be set (e.g. OPERATOR=keystone)" >&2
  exit 1
fi
OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE:-${OPERATOR}-system}"

if ! python3 -c 'import yaml' 2>/dev/null; then
  echo "::error::hack/audit-rbac-usage.py needs python3 with PyYAML" >&2
  exit 1
fi

NODE="${KIND_CLUSTER}-control-plane"
USERNAME="system:serviceaccount:${OPERATOR_NAMESPACE}:${OPERATOR}-operator"

mkdir -p "${OUT_DIR}"
raw="$(mktemp)"
trap 'rm -f "${raw}"' EXIT

# The current file and every rotated one (kube-apiserver-audit-<time>.log).
docker exec "${NODE}" sh -c "cat ${AUDIT_LOG_DIR}/kube-apiserver-audit*.log 2>/dev/null" >"${raw}" || true
if [[ ! -s "${raw}" ]]; then
  echo "::error::no audit event: was the cluster created with hack/kind-config-audit.yaml?" >&2
  exit 1
fi

# kube-apiserver deletes the oldest rotated file once audit-log-maxbackup are
# kept. An informer lists its kind once, when it starts, so a lost file shows
# that list as unused.
rotated="$(docker exec "${NODE}" sh -c "ls ${AUDIT_LOG_DIR}/kube-apiserver-audit-*.log 2>/dev/null | wc -l" || echo 0)"
if ((rotated >= AUDIT_LOG_MAXBACKUP)); then
  echo "::warning::the audit log of ${NODE} rotated ${rotated} times, the most it keeps; the oldest events may be gone and show up as UNUSED"
fi

# The diff reads only the operator's events; keeping just those makes the
# artifact a few MB instead of the whole cluster's log.
grep -F "\"username\":\"${USERNAME}\"" "${raw}" >"${OUT_DIR}/audit.jsonl" || true

status=0
python3 "${SCRIPT_DIR}/audit-rbac-usage.py" \
  --audit-log "${OUT_DIR}/audit.jsonl" \
  --operator "${OPERATOR}" \
  --namespace "${OPERATOR_NAMESPACE}" \
  --json "${OUT_DIR}/${OPERATOR}.json" || status=$?
if ((status != 0)); then
  echo "::error::hack/audit-rbac-usage.py exited ${status} for ${USERNAME}" >&2
  exit "${status}"
fi

read -r used unused_verbs unused_rows not_granted < <(python3 -c '
import json, sys
with open(sys.argv[1], encoding="utf-8") as handle:
    result = json.load(handle)[sys.argv[2]]
print(len(result["used"]), sum(len(row["verbs"]) for row in result["unused"]),
      len(result["unused"]), len(result["used_not_granted"]))
' "${OUT_DIR}/${OPERATOR}.json" "${OPERATOR}")

echo "::notice::RBAC usage of ${OPERATOR}-operator: ${used} triples used, ${unused_verbs} granted verbs unused in ${unused_rows} rows, ${not_granted} used but not granted (${OUT_DIR}/${OPERATOR}.json)"
if ((not_granted > 0)); then
  echo "::warning::${OPERATOR}-operator used ${not_granted} triples its ClusterRole does not grant; see USED, NOT GRANTED above"
fi
