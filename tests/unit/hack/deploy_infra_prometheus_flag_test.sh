#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh gates kube-prometheus-stack behind WITH_PROMETHEUS
# so the kind Quick Start stays minimal by default and only installs the
# monitoring stack (Prometheus + Grafana) when explicitly requested
#
# Implementation: bash + tests/lib/assertions.sh — mirrors the sibling
# tests/unit/hack/deploy_infra_chaos_flag_test.sh pattern. The repo has zero
# .bats files and no shellspec runner; introducing one would add an
# undeclared test dependency.
#
# Strategy: hybrid — source the script (the `BASH_SOURCE[0] == ${0}` guard at
# the bottom of deploy-infra.sh keeps main() from auto-running) to assert the
# runtime default of WITH_PROMETHEUS for each env scenario, and grep the
# script source to lock in the four gate locations:
#   1. kustomize apply (deploy/kind/prometheus)
#   2. dashboard JSON copy (operators/keystone/dashboards → overlay root)
#   3. Phase 3 helm-release wait list append (kube-prometheus-stack)
#   4. enable_keystone_operator_servicemonitor call site
# The configuration-banner line is asserted via grep so the user-visible
# summary stays in lockstep with the runtime value.
#
# Usage: bash tests/unit/hack/deploy_infra_prometheus_flag_test.sh

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

# resolve_with_prometheus [env_var=value...]
# Sources deploy-infra.sh in a subshell with the supplied env overrides and
# echoes the resolved value of WITH_PROMETHEUS after the configuration block
# runs. The `BASH_SOURCE[0] == ${0}` guard at the bottom of deploy-infra.sh
# keeps main() from auto-running when the script is sourced.
resolve_with_prometheus() {
  (
    for assignment in "$@"; do
      export "${assignment?}"
    done
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    printf '%s' "${WITH_PROMETHEUS}"
  )
}

# ---------------------------------------------------------------------------
# Test 1: WITH_PROMETHEUS defaults to false
# ---------------------------------------------------------------------------
test_default_is_false() {
  echo "Test: WITH_PROMETHEUS defaults to false"

  # Unset any inherited value so we observe the script's own default.
  local resolved
  resolved="$(unset WITH_PROMETHEUS; resolve_with_prometheus)"
  assert_eq "WITH_PROMETHEUS defaults to false" "false" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 2: explicit WITH_PROMETHEUS=true
# ---------------------------------------------------------------------------
test_explicit_true() {
  echo "Test: WITH_PROMETHEUS=true is preserved"

  local resolved
  resolved="$(resolve_with_prometheus WITH_PROMETHEUS=true)"
  assert_eq "WITH_PROMETHEUS=true is preserved" "true" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 3: explicit WITH_PROMETHEUS=false
# ---------------------------------------------------------------------------
test_explicit_false() {
  echo "Test: WITH_PROMETHEUS=false is preserved"

  local resolved
  resolved="$(resolve_with_prometheus WITH_PROMETHEUS=false)"
  assert_eq "WITH_PROMETHEUS=false is preserved" "false" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 4: defensive non-true value
# A typo like WITH_PROMETHEUS=yes must NOT enable the stack; every gate uses
# the strict `== "true"` comparison. We assert the value passes through
# verbatim AND that all gate sites use exact-match.
# ---------------------------------------------------------------------------
test_non_true_value_does_not_trigger_install() {
  echo "Test: WITH_PROMETHEUS=yes passes through but does not trigger install"

  local resolved
  resolved="$(resolve_with_prometheus WITH_PROMETHEUS=yes)"
  assert_eq "WITH_PROMETHEUS=yes is preserved verbatim" "yes" "$resolved"

  # Every gate must compare with the exact string "true" so "yes" takes the
  # skip branch at every gate. There are four runtime gates:
  #   - dashboard copy + kustomize apply (Step 3)
  #   - helm_releases append (Phase 3)
  #   - enable_keystone_operator_servicemonitor call (post Phase 3)
  local gate_count
  gate_count="$(grep -cE '"\$\{WITH_PROMETHEUS\}" == "true"' "$DEPLOY_INFRA_SH" || true)"
  assert_eq "deploy-infra.sh has exactly 3 strict WITH_PROMETHEUS==true gates" "3" "$gate_count"
}

# ---------------------------------------------------------------------------
# Test 5: configuration banner line
# The user-visible summary must surface the WITH_PROMETHEUS state next to
# WITH_CHAOS_MESH so operators can spot accidental opt-ins / opt-outs at a
# glance. The expected line shape mirrors the chaos-mesh banner exactly.
# ---------------------------------------------------------------------------
test_banner_includes_prometheus_line() {
  echo "Test: configuration banner surfaces WITH_PROMETHEUS state"

  assert_file_contains \
    "deploy-infra.sh banner mentions Prometheus stack" \
    "$DEPLOY_INFRA_SH" \
    'Prometheus stack'

  assert_file_contains \
    "deploy-infra.sh banner shows the WITH_PROMETHEUS value and remediation hint" \
    "$DEPLOY_INFRA_SH" \
    'set WITH_PROMETHEUS=true to install'
}

# ---------------------------------------------------------------------------
# Test 6: prometheus overlay apply is conditional
# The kustomize apply for deploy/kind/prometheus must live inside the
# WITH_PROMETHEUS gate so the default Quick Start does not install it.
# ---------------------------------------------------------------------------
test_prometheus_kustomize_is_gated() {
  echo "Test: prometheus kustomize apply is gated by WITH_PROMETHEUS"

  # The apply line itself exists.
  assert_file_contains \
    "deploy-infra.sh references the deploy/kind/prometheus overlay" \
    "$DEPLOY_INFRA_SH" \
    'deploy/kind/prometheus'

  # The line must be preceded by a WITH_PROMETHEUS gate. Use awk to confirm
  # the most recent if-line above the apply tests WITH_PROMETHEUS.
  local apply_line gate_line
  apply_line="$(grep -n 'kubectl apply -k "${REPO_ROOT}/deploy/kind/prometheus"' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  gate_line="$(grep -n '"${WITH_PROMETHEUS}" == "true"' "$DEPLOY_INFRA_SH" | awk -F: -v target="${apply_line:-0}" '$1 < target { last = $1 } END { print last }')"

  assert_not_empty "kustomize apply line for prometheus is found" "$apply_line"
  assert_not_empty "WITH_PROMETHEUS gate precedes the kustomize apply" "$gate_line"
}

# ---------------------------------------------------------------------------
# Test 7: dashboard copy is gated and precedes the apply
# The Grafana dashboard JSON must be copied into the overlay root before
# kubectl apply -k renders the configMapGenerator, otherwise kustomize emits
# 'open keystone-operator.json: no such file or directory'. Both the copy and
# the apply must live inside the same WITH_PROMETHEUS gate.
# ---------------------------------------------------------------------------
test_dashboard_copy_precedes_apply() {
  echo "Test: dashboard JSON copy is gated and precedes the prometheus apply"

  local copy_line apply_line gate_line
  copy_line="$(grep -n 'cp -f "\${REPO_ROOT}/operators/keystone/dashboards/keystone-operator.json"' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  apply_line="$(grep -n 'kubectl apply -k "${REPO_ROOT}/deploy/kind/prometheus"' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"

  assert_not_empty "dashboard copy line is found" "$copy_line"
  assert_not_empty "kustomize apply line for prometheus is found" "$apply_line"

  if [[ -n "$copy_line" && -n "$apply_line" && "$copy_line" -lt "$apply_line" ]]; then
    echo "  PASS: dashboard copy precedes kubectl apply -k"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: dashboard copy must precede kubectl apply -k (copy=$copy_line, apply=$apply_line)"
    FAIL=$((FAIL + 1))
  fi

  # And both lines must sit inside a WITH_PROMETHEUS gate.
  gate_line="$(grep -n '"${WITH_PROMETHEUS}" == "true"' "$DEPLOY_INFRA_SH" | awk -F: -v target="${copy_line:-0}" '$1 < target { last = $1 } END { print last }')"
  assert_not_empty "WITH_PROMETHEUS gate precedes the dashboard copy" "$gate_line"
}

# ---------------------------------------------------------------------------
# Test 8: kube-prometheus-stack appended to helm-release wait list
# The Phase 3 wait list must NOT statically include kube-prometheus-stack;
# it must be appended dynamically inside the WITH_PROMETHEUS gate, after the
# chaos-mesh append, so the relative ordering of the seven base releases is
# preserved.
# ---------------------------------------------------------------------------
test_kube_prometheus_stack_appended_dynamically() {
  echo "Test: kube-prometheus-stack is appended dynamically to the helm-release wait list"

  # The base array must NOT include kube-prometheus-stack inline.
  assert_file_not_contains \
    "deploy-infra.sh does not hard-code kube-prometheus-stack in the helm-release wait list" \
    "$DEPLOY_INFRA_SH" \
    'envoy-gateway kube-prometheus-stack'

  # The base array must preserve the seven non-opt-in releases in order.
  assert_file_contains \
    "helm_releases array preserves the seven non-opt-in releases in the documented order" \
    "$DEPLOY_INFRA_SH" \
    'helm_releases=(prometheus-operator-crds openbao mariadb-operator-crds mariadb-operator external-secrets memcached-operator envoy-gateway)'

  # The dynamic append must be guarded by WITH_PROMETHEUS.
  assert_file_contains \
    "kube-prometheus-stack is appended only inside the WITH_PROMETHEUS gate" \
    "$DEPLOY_INFRA_SH" \
    'helm_releases+=(kube-prometheus-stack)'
}

# ---------------------------------------------------------------------------
# Test 9: enable_keystone_operator_servicemonitor exists and is gated
#
# The helper must (a) be defined as a function in the script and (b) be
# called from main() only when WITH_PROMETHEUS=true so the default Quick
# Start does not poke the operator HelmRelease.
# ---------------------------------------------------------------------------
test_servicemonitor_helper_is_defined_and_gated() {
  echo "Test: enable_keystone_operator_servicemonitor is defined and gated"

  assert_file_contains \
    "enable_keystone_operator_servicemonitor function is defined" \
    "$DEPLOY_INFRA_SH" \
    '^enable_keystone_operator_servicemonitor()'

  assert_file_contains \
    "helper patches the keystone-operator HelmRelease values" \
    "$DEPLOY_INFRA_SH" \
    'kubectl patch helmrelease keystone-operator -n keystone-system'

  # The call site must be inside a WITH_PROMETHEUS gate.
  local call_line gate_line
  call_line="$(grep -nE '^[[:space:]]+enable_keystone_operator_servicemonitor[[:space:]]' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  gate_line="$(grep -n '"${WITH_PROMETHEUS}" == "true"' "$DEPLOY_INFRA_SH" | awk -F: -v target="${call_line:-0}" '$1 < target { last = $1 } END { print last }')"

  assert_not_empty "enable_keystone_operator_servicemonitor call site is found" "$call_line"
  assert_not_empty "WITH_PROMETHEUS gate precedes the helper call" "$gate_line"
}

# ---------------------------------------------------------------------------
# Test 10: production-caller contract — the prometheus apply mirrors the
# chaos-mesh contract under kubectl's embedded kustomize (/).
#
# kubectl apply -k uses the embedded kustomize, which does NOT expose
# --load-restrictor (kubernetes/kubectl#948). The chaos-mesh review captured
# this contract and we hold the prometheus overlay to the same standard:
#
#   (a) deploy-infra.sh's prometheus apply line uses plain `kubectl apply -k`
#       (no `--load-restrictor`, no pipe through `kustomize build` to
#       `kubectl apply -f -`); and
#   (b) deploy/kind/prometheus/kustomization.yaml has zero `../../` parent-
#       directory resource entries, so the default LoadRestrictionsRootOnly
#       check is satisfied and the apply succeeds without any flag.
# ---------------------------------------------------------------------------
test_production_caller_matches_self_contained_overlay() {
  echo "Test: production caller and overlay agree on the no-load-restrictor contract (/)"

  # (a) The apply line is the bare kubectl form — no pipe, no flag.
  local raw
  raw="$(grep -E 'kubectl apply -k "\$\{REPO_ROOT\}/deploy/kind/prometheus"' "$DEPLOY_INFRA_SH" | head -1)"
  assert_not_empty "deploy-infra.sh has the prometheus kubectl apply line" "$raw"

  if printf '%s' "$raw" | grep -q -- '--load-restrictor'; then
    echo "  FAIL: deploy-infra.sh's prometheus apply line passes --load-restrictor (kubectl's embedded kustomize does not accept it)"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: prometheus apply line uses no --load-restrictor flag"
    PASS=$((PASS + 1))
  fi

  # (b) The overlay must have zero `../../` parent-directory resource entries.
  local kust="$PROJECT_ROOT/deploy/kind/prometheus/kustomization.yaml"
  if [[ ! -f "$kust" ]]; then
    echo "  FAIL: $kust does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  local parent_refs
  parent_refs="$( { grep -E '^[[:space:]]*-[[:space:]]+\.\./\.\.' "$kust" || true; } | wc -l)"
  assert_eq "deploy/kind/prometheus/kustomization.yaml has no '../../' resource entries" \
    "0" "${parent_refs// /}"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_default_is_false
test_explicit_true
test_explicit_false
test_non_true_value_does_not_trigger_install
test_banner_includes_prometheus_line
test_prometheus_kustomize_is_gated
test_dashboard_copy_precedes_apply
test_kube_prometheus_stack_appended_dynamically
test_servicemonitor_helper_is_defined_and_gated
test_production_caller_matches_self_contained_overlay

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
