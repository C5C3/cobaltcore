#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh gates the VPA recommender behind WITH_VPA so the
# kind Quick Start stays minimal by default and only installs the recommender
# the sizing measurement reads (hack/ci-vpa-recommendations.sh) when
# explicitly requested, and that WITH_VPA=true implies WITH_METRICS_SERVER=true
# because the recommender reads the resource-metrics API.
#
# Implementation: bash + tests/lib/assertions.sh, mirroring the sibling
# tests/unit/hack/deploy_infra_metrics_server_flag_test.sh.
#
# Strategy: hybrid. Source the script (the `BASH_SOURCE[0] == ${0}` guard at
# the bottom of deploy-infra.sh keeps main() from auto-running) to assert the
# runtime values of WITH_VPA and WITH_METRICS_SERVER for each env scenario, and
# grep the script source to lock in the three gated locations:
#   1. kustomize apply (deploy/kind/vpa)
#   2. Phase 3 helm-release wait list append (vertical-pod-autoscaler)
#   3. the MariaDB CRD scale-subresource removal (subresources.scale), after
#      the CRD wait, so the recommender accepts a VPA on the database
#      StatefulSet
# The fourth strict gate is the metrics-server implication itself.
#
# Usage: bash tests/unit/hack/deploy_infra_vpa_flag_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"
SETUP_ACTION="$PROJECT_ROOT/.github/actions/setup-e2e-infra/action.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# resolve_flags [env_var=value...]
# Sources deploy-infra.sh in a subshell with WITH_VPA and WITH_METRICS_SERVER
# unset, applies the supplied env overrides, and echoes
# "<WITH_VPA>/<WITH_METRICS_SERVER>" after the configuration block runs.
resolve_flags() {
  (
    unset WITH_VPA WITH_METRICS_SERVER
    for assignment in "$@"; do
      export "${assignment?}"
    done
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    printf '%s/%s' "${WITH_VPA}" "${WITH_METRICS_SERVER}"
  )
}

# gating_if_of <fixed string>
# For every non-comment line of deploy-infra.sh that contains the fixed
# string, print the most recent `if [[ ... ]]` line above it. Prints one line
# per match, so the caller can count the matches and check every gate.
gating_if_of() {
  awk -v needle="$1" '
    /^[[:space:]]*#/ { next }
    /^[[:space:]]*if \[\[/ { gate = $0 }
    index($0, needle) > 0 { print gate }
  ' "$DEPLOY_INFRA_SH"
}

# ---------------------------------------------------------------------------
# Test 1: WITH_VPA defaults to false and leaves WITH_METRICS_SERVER at false
# ---------------------------------------------------------------------------
test_default_is_false() {
  echo "Test: WITH_VPA defaults to false without touching WITH_METRICS_SERVER"

  assert_eq "WITH_VPA/WITH_METRICS_SERVER default to false/false" \
    "false/false" "$(resolve_flags)"
}

# ---------------------------------------------------------------------------
# Test 2: WITH_VPA=true implies WITH_METRICS_SERVER=true
# ---------------------------------------------------------------------------
test_true_implies_metrics_server() {
  echo "Test: WITH_VPA=true sets WITH_METRICS_SERVER=true"

  assert_eq "WITH_VPA=true resolves to true/true" \
    "true/true" "$(resolve_flags WITH_VPA=true)"
}

# ---------------------------------------------------------------------------
# Test 3: the implication wins over an explicit WITH_METRICS_SERVER=false
# The recommender has nothing to recommend without the resource-metrics API,
# so a caller cannot switch metrics-server off underneath it.
# ---------------------------------------------------------------------------
test_true_overrides_metrics_server_false() {
  echo "Test: WITH_VPA=true WITH_METRICS_SERVER=false still ends with WITH_METRICS_SERVER=true"

  assert_eq "WITH_VPA=true WITH_METRICS_SERVER=false resolves to true/true" \
    "true/true" "$(resolve_flags WITH_VPA=true WITH_METRICS_SERVER=false)"
}

# ---------------------------------------------------------------------------
# Test 4: a non-true value passes through and implies nothing
# Every gate compares with the exact string "true", so WITH_VPA=yes takes the
# skip branch everywhere. The strict gates are the implication, the apply,
# the wait-list append and the MariaDB CRD patch.
# ---------------------------------------------------------------------------
test_non_true_value_does_not_trigger_install() {
  echo "Test: WITH_VPA=yes passes through but installs nothing"

  assert_eq "WITH_VPA=yes resolves to yes/false" \
    "yes/false" "$(resolve_flags WITH_VPA=yes)"
  assert_eq "WITH_VPA= (empty, as CI passes it unlabelled) resolves to false/false" \
    "false/false" "$(resolve_flags WITH_VPA=)"

  local gate_count
  gate_count="$(grep -cE '"\$\{WITH_VPA\}" == "true"' "$DEPLOY_INFRA_SH" || true)"
  assert_eq "deploy-infra.sh has exactly 4 strict WITH_VPA==true gates" "4" "$gate_count"
}

# ---------------------------------------------------------------------------
# Test 5: exactly three gated locations reference the overlay, the release
# and the MariaDB scale subresource
# ---------------------------------------------------------------------------
test_overlay_and_release_are_gated() {
  echo "Test: deploy/kind/vpa, vertical-pod-autoscaler and subresources.scale appear once each, under a WITH_VPA gate"

  local needle gates count ungated
  for needle in 'deploy/kind/vpa' 'vertical-pod-autoscaler' 'subresources.scale'; do
    gates="$(gating_if_of "$needle")"
    count="$(printf '%s' "$gates" | grep -c . || true)"
    ungated="$(printf '%s\n' "$gates" | grep -vcF '"${WITH_VPA}" == "true"' || true)"
    assert_eq "exactly one code line references $needle" "1" "$count"
    assert_eq "every code line referencing $needle sits under a WITH_VPA gate" "0" "$ungated"
  done

  assert_file_contains_fixed "the overlay is applied with plain kubectl apply -k" \
    "$DEPLOY_INFRA_SH" 'kubectl apply -k "${REPO_ROOT}/deploy/kind/vpa"'
  assert_file_contains_fixed "the release joins the Phase 3 wait list" \
    "$DEPLOY_INFRA_SH" 'helm_releases+=(vertical-pod-autoscaler)'

  local apply_line
  apply_line="$(grep -F 'deploy/kind/vpa"' "$DEPLOY_INFRA_SH" | grep -v '^[[:space:]]*#' | head -1)"
  if grep -q -- '--load-restrictor' <<<"$apply_line"; then
    echo "  FAIL: the VPA apply line passes --load-restrictor (kubectl's embedded kustomize does not accept it)"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: the VPA apply line uses no --load-restrictor flag"
    PASS=$((PASS + 1))
  fi
}

# ---------------------------------------------------------------------------
# Test 6: the MariaDB CRD loses its scale subresource after the CRD wait
# The recommender rejects a VPA on the database StatefulSet while its MariaDB
# owner is scalable, so the patch must run once the CRD is established.
# ---------------------------------------------------------------------------
test_mariadb_scale_subresource_is_removed() {
  echo "Test: WITH_VPA removes the MariaDB CRD scale subresource after the CRD wait"

  assert_file_contains_fixed "the patch reads the MariaDB CRD" \
    "$DEPLOY_INFRA_SH" 'kubectl get crd mariadbs.k8s.mariadb.com -o json'
  assert_file_contains_fixed "the patch deletes the scale subresource of every version" \
    "$DEPLOY_INFRA_SH" "jq 'del(.spec.versions[].subresources.scale)'"
  assert_file_contains_fixed "the patch replaces the CRD" \
    "$DEPLOY_INFRA_SH" 'kubectl replace -f - > /dev/null'

  local wait_line patch_line
  wait_line="$(grep -n '^[[:space:]]*mariadbs\.k8s\.mariadb\.com \\$' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  patch_line="$(grep -n 'subresources\.scale' "$DEPLOY_INFRA_SH" | grep -v '^[0-9]*:[[:space:]]*#' | head -1 | cut -d: -f1)"
  if [[ -n "$wait_line" && -n "$patch_line" && "$patch_line" -gt "$wait_line" ]]; then
    echo "  PASS: the patch (line $patch_line) follows the MariaDB CRD wait (line $wait_line)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the patch (line ${patch_line:-none}) must follow the MariaDB CRD wait (line ${wait_line:-none})"
    FAIL=$((FAIL + 1))
  fi
}

# ---------------------------------------------------------------------------
# Test 7: the configuration banner and the composite action
# ---------------------------------------------------------------------------
test_banner_and_setup_action() {
  echo "Test: the banner shows WITH_VPA and setup-e2e-infra threads it"

  assert_file_contains_fixed "deploy-infra.sh banner shows the WITH_VPA value and remediation hint" \
    "$DEPLOY_INFRA_SH" \
    'VPA recommender    : ${WITH_VPA} (set WITH_VPA=true to install the recommender and metrics-server)'
  assert_file_contains_fixed "WITH_VPA reaches deploy-infra.sh through setup-e2e-infra" \
    "$SETUP_ACTION" 'WITH_VPA: ${{ env.WITH_VPA }}'
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_default_is_false
test_true_implies_metrics_server
test_true_overrides_metrics_server_false
test_non_true_value_does_not_trigger_install
test_overlay_and_release_are_gated
test_mariadb_scale_subresource_is_removed
test_banner_and_setup_action

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
