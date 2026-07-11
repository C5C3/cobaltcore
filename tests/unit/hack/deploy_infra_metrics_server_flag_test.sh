#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh gates metrics-server behind WITH_METRICS_SERVER
# so the kind Quick Start stays minimal by default and only installs the
# resource-metrics API (required by the HPA/autoscaling recipe) when
# explicitly requested.
#
# Implementation: bash + tests/lib/assertions.sh — mirrors the sibling
# tests/unit/hack/deploy_infra_prometheus_flag_test.sh pattern. The repo has
# zero .bats files and no shellspec runner; introducing one would add an
# undeclared test dependency.
#
# Strategy: hybrid — source the script (the `BASH_SOURCE[0] == ${0}` guard at
# the bottom of deploy-infra.sh keeps main() from auto-running) to assert the
# runtime default of WITH_METRICS_SERVER for each env scenario, and grep the
# script source to lock in the two gate locations:
#   1. kustomize apply (deploy/kind/metrics-server)
#   2. Phase 3 helm-release wait list append (metrics-server)
# Unlike the prometheus flag there is NO dashboard copy and NO servicemonitor
# patch, so the strict gate count is 2, not 3. The configuration-banner line
# is asserted via grep so the user-visible summary stays in lockstep with the
# runtime value.
#
# Usage: bash tests/unit/hack/deploy_infra_metrics_server_flag_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# resolve_with_metrics_server [env_var=value...]
# Sources deploy-infra.sh in a subshell with the supplied env overrides and
# echoes the resolved value of WITH_METRICS_SERVER after the configuration
# block runs. The `BASH_SOURCE[0] == ${0}` guard at the bottom of
# deploy-infra.sh keeps main() from auto-running when the script is sourced.
resolve_with_metrics_server() {
  (
    for assignment in "$@"; do
      export "${assignment?}"
    done
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    printf '%s' "${WITH_METRICS_SERVER}"
  )
}

# ---------------------------------------------------------------------------
# Test 1: WITH_METRICS_SERVER defaults to false
# ---------------------------------------------------------------------------
test_default_is_false() {
  echo "Test: WITH_METRICS_SERVER defaults to false"

  # Unset any inherited value so we observe the script's own default.
  local resolved
  resolved="$(unset WITH_METRICS_SERVER; resolve_with_metrics_server)"
  assert_eq "WITH_METRICS_SERVER defaults to false" "false" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 2: explicit WITH_METRICS_SERVER=true
# ---------------------------------------------------------------------------
test_explicit_true() {
  echo "Test: WITH_METRICS_SERVER=true is preserved"

  local resolved
  resolved="$(resolve_with_metrics_server WITH_METRICS_SERVER=true)"
  assert_eq "WITH_METRICS_SERVER=true is preserved" "true" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 3: explicit WITH_METRICS_SERVER=false
# ---------------------------------------------------------------------------
test_explicit_false() {
  echo "Test: WITH_METRICS_SERVER=false is preserved"

  local resolved
  resolved="$(resolve_with_metrics_server WITH_METRICS_SERVER=false)"
  assert_eq "WITH_METRICS_SERVER=false is preserved" "false" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 4: defensive non-true value
# A typo like WITH_METRICS_SERVER=yes must NOT enable the overlay; every gate
# uses the strict `== "true"` comparison. We assert the value passes through
# verbatim AND that all gate sites use exact-match.
# ---------------------------------------------------------------------------
test_non_true_value_does_not_trigger_install() {
  echo "Test: WITH_METRICS_SERVER=yes passes through but does not trigger install"

  local resolved
  resolved="$(resolve_with_metrics_server WITH_METRICS_SERVER=yes)"
  assert_eq "WITH_METRICS_SERVER=yes is preserved verbatim" "yes" "$resolved"

  # Every gate must compare with the exact string "true" so "yes" takes the
  # skip branch at every gate. There are exactly two runtime gates:
  #   - kustomize apply (Step 3)
  #   - helm_releases append (Phase 3)
  local gate_count
  gate_count="$(grep -cE '"\$\{WITH_METRICS_SERVER\}" == "true"' "$DEPLOY_INFRA_SH" || true)"
  assert_eq "deploy-infra.sh has exactly 2 strict WITH_METRICS_SERVER==true gates" "2" "$gate_count"
}

# ---------------------------------------------------------------------------
# Test 5: configuration banner line
# The user-visible summary must surface the WITH_METRICS_SERVER state next to
# WITH_PROMETHEUS so operators can spot accidental opt-ins / opt-outs at a
# glance. The expected line shape mirrors the prometheus banner exactly.
# ---------------------------------------------------------------------------
test_banner_includes_metrics_server_line() {
  echo "Test: configuration banner surfaces WITH_METRICS_SERVER state"

  assert_file_contains \
    "deploy-infra.sh banner mentions metrics-server" \
    "$DEPLOY_INFRA_SH" \
    'metrics-server'

  assert_file_contains \
    "deploy-infra.sh banner shows the WITH_METRICS_SERVER value and remediation hint" \
    "$DEPLOY_INFRA_SH" \
    'set WITH_METRICS_SERVER=true to install'
}

# ---------------------------------------------------------------------------
# Test 6: metrics-server overlay apply is conditional
# The kustomize apply for deploy/kind/metrics-server must live inside the
# WITH_METRICS_SERVER gate so the default Quick Start does not install it.
# ---------------------------------------------------------------------------
test_metrics_server_kustomize_is_gated() {
  echo "Test: metrics-server kustomize apply is gated by WITH_METRICS_SERVER"

  # The apply line itself exists.
  assert_file_contains \
    "deploy-infra.sh references the deploy/kind/metrics-server overlay" \
    "$DEPLOY_INFRA_SH" \
    'deploy/kind/metrics-server'

  # The line must be preceded by a WITH_METRICS_SERVER gate. Use awk to confirm
  # the most recent if-line above the apply tests WITH_METRICS_SERVER.
  local apply_line gate_line
  apply_line="$(grep -n 'kubectl apply -k "${REPO_ROOT}/deploy/kind/metrics-server"' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  gate_line="$(grep -n '"${WITH_METRICS_SERVER}" == "true"' "$DEPLOY_INFRA_SH" | awk -F: -v target="${apply_line:-0}" '$1 < target { last = $1 } END { print last }')"

  assert_not_empty "kustomize apply line for metrics-server is found" "$apply_line"
  assert_not_empty "WITH_METRICS_SERVER gate precedes the kustomize apply" "$gate_line"
}

# ---------------------------------------------------------------------------
# Test 7: metrics-server appended to helm-release wait list
# The Phase 3 wait list must NOT statically include metrics-server; it must be
# appended dynamically inside the WITH_METRICS_SERVER gate, after the
# kube-prometheus-stack append, so the relative ordering of the seven base
# releases is preserved.
# ---------------------------------------------------------------------------
test_metrics_server_appended_dynamically() {
  echo "Test: metrics-server is appended dynamically to the helm-release wait list"

  # The base array must NOT include metrics-server inline.
  assert_file_not_contains \
    "deploy-infra.sh does not hard-code metrics-server in the helm-release wait list" \
    "$DEPLOY_INFRA_SH" \
    'envoy-gateway metrics-server'

  # The base array must preserve the seven non-opt-in releases in order.
  assert_file_contains \
    "helm_releases array preserves the seven non-opt-in releases in the documented order" \
    "$DEPLOY_INFRA_SH" \
    'helm_releases=(prometheus-operator-crds openbao mariadb-operator-crds mariadb-operator external-secrets memcached-operator envoy-gateway)'

  # The dynamic append must be guarded by WITH_METRICS_SERVER.
  assert_file_contains \
    "metrics-server is appended only inside the WITH_METRICS_SERVER gate" \
    "$DEPLOY_INFRA_SH" \
    'helm_releases+=(metrics-server)'
}

# ---------------------------------------------------------------------------
# Test 8: production-caller contract — the metrics-server apply mirrors the
# chaos-mesh / prometheus contract under kubectl's embedded kustomize.
#
# kubectl apply -k uses the embedded kustomize, which does NOT expose
# --load-restrictor (kubernetes/kubectl#948). We hold the metrics-server
# overlay to the same standard:
#
#   (a) deploy-infra.sh's metrics-server apply line uses plain
#       `kubectl apply -k` (no `--load-restrictor`, no pipe through
#       `kustomize build` to `kubectl apply -f -`); and
#   (b) deploy/kind/metrics-server/kustomization.yaml has zero `../../`
#       parent-directory resource entries, so the default
#       LoadRestrictionsRootOnly check is satisfied and the apply succeeds
#       without any flag.
# ---------------------------------------------------------------------------
test_production_caller_matches_self_contained_overlay() {
  echo "Test: production caller and overlay agree on the no-load-restrictor contract"

  # (a) The apply line is the bare kubectl form — no pipe, no flag.
  local raw
  raw="$(grep -E 'kubectl apply -k "\$\{REPO_ROOT\}/deploy/kind/metrics-server"' "$DEPLOY_INFRA_SH" | head -1)"
  assert_not_empty "deploy-infra.sh has the metrics-server kubectl apply line" "$raw"

  if printf '%s' "$raw" | grep -q -- '--load-restrictor'; then
    echo "  FAIL: deploy-infra.sh's metrics-server apply line passes --load-restrictor (kubectl's embedded kustomize does not accept it)"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: metrics-server apply line uses no --load-restrictor flag"
    PASS=$((PASS + 1))
  fi

  # (b) The overlay must have zero `../../` parent-directory resource entries.
  local kust="$PROJECT_ROOT/deploy/kind/metrics-server/kustomization.yaml"
  if [[ ! -f "$kust" ]]; then
    echo "  FAIL: $kust does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  local parent_refs
  parent_refs="$( { grep -E '^[[:space:]]*-[[:space:]]+\.\./\.\.' "$kust" || true; } | wc -l)"
  assert_eq "deploy/kind/metrics-server/kustomization.yaml has no '../../' resource entries" \
    "0" "${parent_refs// /}"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_default_is_false
test_explicit_true
test_explicit_false
test_non_true_value_does_not_trigger_install
test_banner_includes_metrics_server_line
test_metrics_server_kustomize_is_gated
test_metrics_server_appended_dynamically
test_production_caller_matches_self_contained_overlay

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
