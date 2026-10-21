#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify that the two OpenBaoTenants of the stack carry one guardrail spec:
#
#   - deploy/kind/infrastructure/openbao-tenant.yaml admits the kind and lab
#     openstack namespace, and the c5c3 operator builds the tenant of a
#     dedicated Barbican store's namespace from the openBaoTenant* consts in
#     operators/c5c3/internal/controller/reconcile_barbican_openbao.go. The
#     five figures (pods, the two quota requests, the two default requests)
#     must be equal in both.
#   - The shape is pinned as well: the quota keys are exactly pods,
#     requests.cpu and requests.memory, so no limits.* key rejects a pod that
#     sets no limit, and the LimitRange is one Container item that sets only
#     defaultRequest, so no default limit caps a container that sets none.
#   - compare_guardrails reports a copy of the YAML whose figures or shape
#     differ, naming the const or the key.
#
# Usage: bash tests/unit/deploy/openbao_tenant_guardrails_contract_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TENANT_FILE="$PROJECT_ROOT/deploy/kind/infrastructure/openbao-tenant.yaml"
CONTROLLER_FILE="$PROJECT_ROOT/operators/c5c3/internal/controller/reconcile_barbican_openbao.go"

# go_const <file> <name> — Echo the string literal the Go const <name> in
# <file> is assigned. Empty when the const is absent.
go_const() {
  grep -E "^[[:space:]]*$2[[:space:]]+= \"" "$1" 2>/dev/null |
    head -n1 | sed 's/.*"\(.*\)"/\1/'
}

# tenant_field <yaml> <expression> — Evaluate a yq expression on the
# OpenBaoTenant of <yaml>.
tenant_field() {
  yq -r "select(.kind == \"OpenBaoTenant\") | $2" "$1" 2>/dev/null | head -n1
}

# compare_guardrails <yaml> <go> — Print one "mismatch:" line per difference
# between the OpenBaoTenant of <yaml> and the guardrail consts of <go>, and
# return 1 when there is any.
compare_guardrails() {
  local yaml="$1" go="$2" rc=0 pair name path want got
  for pair in \
    'openBaoTenantQuotaPods .spec.quota.hard.pods' \
    'openBaoTenantQuotaRequestsCPU .spec.quota.hard["requests.cpu"]' \
    'openBaoTenantQuotaRequestsMemory .spec.quota.hard["requests.memory"]' \
    'openBaoTenantDefaultRequestCPU .spec.limitRange.limits[0].defaultRequest.cpu' \
    'openBaoTenantDefaultRequestMemory .spec.limitRange.limits[0].defaultRequest.memory'; do
    name="${pair%% *}"
    path="${pair#* }"
    want="$(go_const "$go" "$name")"
    got="$(tenant_field "$yaml" "$path")"
    if [[ -z "$want" || "$want" != "$got" ]]; then
      echo "mismatch: ${name} is '${want}' in ${go}, ${path} is '${got}' in ${yaml}"
      rc=1
    fi
  done
  for pair in \
    '.spec.quota.hard | keys | sort | join(" ")=pods requests.cpu requests.memory' \
    '.spec.limitRange.limits | length=1' \
    '.spec.limitRange.limits[0].type=Container' \
    '.spec.limitRange.limits[0] | keys | sort | join(" ")=defaultRequest type' \
    '.spec.limitRange.limits[0].defaultRequest | keys | sort | join(" ")=cpu memory'; do
    path="${pair%%=*}"
    want="${pair#*=}"
    got="$(tenant_field "$yaml" "$path")"
    if [[ "$want" != "$got" ]]; then
      echo "mismatch: ${path} is '${got}' in ${yaml}, the guardrail spec has '${want}'"
      rc=1
    fi
  done
  return "$rc"
}

# --- Test 1: the kind tenant carries the operator's guardrail spec ----------
test_kind_tenant_matches_the_operator() {
  echo "Test: the kind tenant carries the guardrail spec of the c5c3 operator"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local out rc
  out="$(compare_guardrails "$TENANT_FILE" "$CONTROLLER_FILE")"
  rc=$?
  assert_eq "compare_guardrails passes on the committed files" "0" "$rc"
  assert_eq "it reports no mismatch" "" "$out"
}

# --- Test 2: a changed figure is reported, naming the const -----------------
test_changed_figure_is_reported() {
  echo "Test: a tenant copy with another pod figure is reported"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local tmp out rc want
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  want="$(go_const "$CONTROLLER_FILE" openBaoTenantQuotaPods)"
  yq '(select(.kind == "OpenBaoTenant") | .spec.quota.hard.pods) = "1"' "$TENANT_FILE" >"$tmp/tenant.yaml"
  out="$(compare_guardrails "$tmp/tenant.yaml" "$CONTROLLER_FILE")"
  rc=$?
  assert_eq "compare_guardrails fails on another pod figure" "1" "$rc"
  assert_contains "the mismatch names the const" "$out" \
    "mismatch: openBaoTenantQuotaPods is '${want}'"
}

# --- Test 3: a default limit is reported as a shape mismatch ----------------
test_default_limit_is_reported() {
  echo "Test: a tenant copy with a default limit is reported"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local tmp out rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  yq '(select(.kind == "OpenBaoTenant") | .spec.limitRange.limits[0].default.memory) = "512Mi"' \
    "$TENANT_FILE" >"$tmp/tenant.yaml"
  out="$(compare_guardrails "$tmp/tenant.yaml" "$CONTROLLER_FILE")"
  rc=$?
  assert_eq "compare_guardrails fails on a default limit" "1" "$rc"
  assert_contains "the mismatch names the item's keys" "$out" \
    "mismatch: .spec.limitRange.limits[0] | keys | sort | join(\" \") is 'default defaultRequest type'"
}

# --- Run ---
test_kind_tenant_matches_the_operator
test_changed_figure_is_reported
test_default_limit_is_reported

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
