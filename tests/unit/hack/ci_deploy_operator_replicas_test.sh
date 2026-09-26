#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the OPERATOR_REPLICAS knob of hack/ci-deploy-operator.sh with a
# stubbed PATH:
#   - OPERATOR_REPLICAS=1 adds --set replicas=1 to the `helm install` argv and
#     echoes the count in the banner;
#   - an unset knob adds no replicas flag, so the chart default of 2 applies;
#   - OPERATOR_REPLICAS=0 and OPERATOR_REPLICAS=two exit 1 with an ::error::
#     line before anything is applied;
#   - the e2e-controlplane job sets OPERATOR_REPLICAS=1 for its operator
#     deploy steps.
#
# Follows tests/unit/hack/ci_deploy_operator_federation_cidrs_test.sh.
#
# Usage: bash tests/unit/hack/ci_deploy_operator_replicas_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_SH="$PROJECT_ROOT/hack/ci-deploy-operator.sh"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stubs <dir>
# helm logs its argv to $HELM_LOG and succeeds. kubectl logs its argv to
# $KUBECTL_LOG, returns non-zero for `get mutatingwebhookconfigurations` so the
# deploy script skips the webhook readiness wait, and succeeds otherwise.
make_stubs() {
  local dir="$1"
  mkdir -p "$dir"

  cat >"$dir/helm" <<'STUB'
#!/bin/bash
echo "helm $*" >>"$HELM_LOG"
exit 0
STUB
  chmod +x "$dir/helm"

  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
echo "kubectl $*" >>"$KUBECTL_LOG"
if [ "${1:-}" = "get" ] && [ "${2:-}" = "mutatingwebhookconfigurations" ]; then
  exit 1
fi
exit 0
STUB
  chmod +x "$dir/kubectl"
}

# make_chart <dir>
# A minimal stub chart with a crds/ directory, so the run never touches the
# real chart tree.
make_chart() {
  local dir="$1"
  mkdir -p "$dir/crds"
  printf 'apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n' \
    >"$dir/crds/dummy.yaml"
}

# run_deploy <tmp>
# Runs the deploy script against the stubs and the stub chart in <tmp>.
# OPERATOR_REPLICAS is inherited from the caller's environment.
run_deploy() {
  local tmp="$1"
  (
    PATH="$tmp/bin:$PATH"
    export PATH
    export HELM_LOG="$tmp/helm.log"
    export KUBECTL_LOG="$tmp/kubectl.log"
    export OPERATOR="keystone"
    export IMAGE_REPO="ghcr.io/c5c3/keystone-operator"
    export CHART_DIR="$tmp/chart"
    bash "$DEPLOY_SH"
  ) 2>&1
}

new_tmp() {
  local tmp
  tmp="$(mktemp -d)"
  make_stubs "$tmp/bin"
  make_chart "$tmp/chart"
  touch "$tmp/helm.log" "$tmp/kubectl.log"
  echo "$tmp"
}

# ---------------------------------------------------------------------------
# Test 1: OPERATOR_REPLICAS=1 renders --set replicas=1
# ---------------------------------------------------------------------------
test_replicas_set() {
  echo "Test: OPERATOR_REPLICAS=1 adds --set replicas=1"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(OPERATOR_REPLICAS=1 run_deploy "$tmp")"
  rc=$?

  assert_eq "deploy exits 0" "0" "$rc"
  assert_contains "the banner echoes the count" "$output" "Operator replicas   : 1"
  assert_file_contains_fixed "helm install carries --set replicas=1" \
    "$tmp/helm.log" "--set replicas=1"
}

# ---------------------------------------------------------------------------
# Test 2: an unset knob keeps the chart default
# ---------------------------------------------------------------------------
test_replicas_unset() {
  echo "Test: unset OPERATOR_REPLICAS adds no replicas flag"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(unset OPERATOR_REPLICAS; run_deploy "$tmp")"
  rc=$?

  assert_eq "deploy exits 0" "0" "$rc"
  assert_contains "the banner names the chart default" "$output" \
    "Operator replicas   : <chart default>"
  assert_file_contains "helm install still runs" "$tmp/helm.log" "helm install"
  assert_file_not_contains "no replicas flag is passed" "$tmp/helm.log" "replicas="
}

# ---------------------------------------------------------------------------
# Test 3: a count that is not a positive integer exits 1 before any apply
# ---------------------------------------------------------------------------
test_replicas_invalid() {
  local value
  for value in 0 two; do
    echo "Test: OPERATOR_REPLICAS=${value} is rejected"
    local tmp output rc
    tmp="$(new_tmp)"

    output="$(OPERATOR_REPLICAS="$value" run_deploy "$tmp")"
    rc=$?

    assert_eq "deploy exits 1" "1" "$rc"
    assert_contains "the ::error:: line names the value" "$output" \
      "::error::OPERATOR_REPLICAS='${value}' is not a positive integer"
    assert_file_not_contains "helm install never runs" "$tmp/helm.log" "helm install"
    assert_file_not_contains "no CRD is applied" "$tmp/kubectl.log" "apply"
    rm -rf "$tmp"
  done
}

# ---------------------------------------------------------------------------
# Test 4: the e2e-controlplane job runs one replica per operator
# ---------------------------------------------------------------------------
test_ci_wiring() {
  echo "Test: e2e-controlplane sets OPERATOR_REPLICAS=1"
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi
  assert_eq "the job-level env sets OPERATOR_REPLICAS to 1" "1" \
    "$(yq '.jobs["e2e-controlplane"].env.OPERATOR_REPLICAS' "$CI_YAML")"
  assert_eq "no other job sets OPERATOR_REPLICAS" "e2e-controlplane" \
    "$(yq '[.jobs | to_entries[] | select(.value.env.OPERATOR_REPLICAS != null) | .key] | join(",")' "$CI_YAML")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_replicas_set
test_replicas_unset
test_replicas_invalid
test_ci_wiring

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
