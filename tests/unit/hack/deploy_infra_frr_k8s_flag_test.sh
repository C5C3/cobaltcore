#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh gates frr-k8s behind WITH_FRR_K8S, so the kind
# Quick Start stays minimal by default and only installs the per-node FRR
# DaemonSet the BGP fabric suite peers from when explicitly requested.
#
# Strategy: the same hybrid as
# tests/unit/hack/deploy_infra_metrics_server_flag_test.sh. Source the script
# (the `BASH_SOURCE[0] == ${0}` guard at the bottom of deploy-infra.sh keeps
# main() from auto-running) to assert the runtime value of WITH_FRR_K8S for
# each env scenario, and grep the script source to lock in the two gates:
#   1. kustomize apply (deploy/overlays/kind/frr-k8s), Step 3
#   2. Phase 3 helm-release wait list append (frr-k8s)
# The second gate is what fails a run whose release never becomes Ready: the
# release is in the list wait_for_helmreleases waits on. The EXTERNAL_CLUSTER
# refusal reads the flag by indirect expansion and is not a literal gate;
# tests/unit/hack/deploy_infra_external_cluster_test.sh covers it.
#
# Usage: bash tests/unit/hack/deploy_infra_frr_k8s_flag_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"
OVERLAY_KUSTOMIZATION="$PROJECT_ROOT/deploy/overlays/kind/frr-k8s/kustomization.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# resolve_with_frr_k8s [env_var=value...]
# Sources deploy-infra.sh in a subshell with the supplied env overrides and
# echoes the resolved value of WITH_FRR_K8S after the configuration block runs.
resolve_with_frr_k8s() {
  (
    unset WITH_FRR_K8S
    for assignment in "$@"; do
      export "${assignment?}"
    done
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    printf '%s' "${WITH_FRR_K8S}"
  )
}

test_default_is_false() {
  echo "Test: WITH_FRR_K8S defaults to false"

  assert_eq "WITH_FRR_K8S unset resolves to false" "false" "$(resolve_with_frr_k8s)"
  # The e2e-infra job threads the flag through setup-e2e-infra as an empty
  # string; that has to install nothing either.
  assert_eq "WITH_FRR_K8S='' resolves to false" "false" "$(resolve_with_frr_k8s WITH_FRR_K8S=)"
}

test_explicit_values_are_preserved() {
  echo "Test: explicit WITH_FRR_K8S values are preserved"

  assert_eq "WITH_FRR_K8S=true is preserved" "true" "$(resolve_with_frr_k8s WITH_FRR_K8S=true)"
  assert_eq "WITH_FRR_K8S=false is preserved" "false" "$(resolve_with_frr_k8s WITH_FRR_K8S=false)"
}

# A typo like WITH_FRR_K8S=yes must not install the overlay. The value passes
# through verbatim, and every gate compares with the exact string "true".
test_non_true_value_does_not_trigger_install() {
  echo "Test: WITH_FRR_K8S=yes passes through but every gate needs the literal true"

  assert_eq "WITH_FRR_K8S=yes is preserved verbatim" "yes" "$(resolve_with_frr_k8s WITH_FRR_K8S=yes)"

  local gate_count
  gate_count="$(grep -cE '"\$\{WITH_FRR_K8S\}" == "true"' "$DEPLOY_INFRA_SH" || true)"
  assert_eq "deploy-infra.sh has exactly 2 strict WITH_FRR_K8S==true gates" "2" "$gate_count"
}

test_banner_line() {
  echo "Test: configuration banner surfaces the WITH_FRR_K8S state"

  assert_file_contains_fixed "deploy-infra.sh prints the frr-k8s banner line" \
    "$DEPLOY_INFRA_SH" \
    'log "frr-k8s             : ${WITH_FRR_K8S} (set WITH_FRR_K8S=true to install)"'
}

test_apply_is_gated() {
  echo "Test: the frr-k8s overlay apply sits inside the WITH_FRR_K8S gate"

  local apply_line gate_line raw
  apply_line="$(grep -n 'kubectl apply -k "${REPO_ROOT}/deploy/overlays/kind/frr-k8s"' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  assert_not_empty "the kubectl apply line for deploy/overlays/kind/frr-k8s exists" "$apply_line"
  # The line right above the apply is the gate, so no other branch can reach it.
  gate_line="$(sed -n "$((${apply_line:-1} - 1))p" "$DEPLOY_INFRA_SH")"
  assert_eq "the line above the apply is the WITH_FRR_K8S gate" \
    '  if [[ "${WITH_FRR_K8S}" == "true" ]]; then' "$gate_line"

  # kubectl's embedded kustomize does not accept --load-restrictor
  # (kubernetes/kubectl#948), so the apply is the bare form and the overlay
  # has no parent-directory entry.
  raw="$(sed -n "${apply_line:-1}p" "$DEPLOY_INFRA_SH")"
  assert_not_contains "the apply passes no --load-restrictor" "$raw" "--load-restrictor"
  local parent_refs
  parent_refs="$( { grep -E '^[[:space:]]*-[[:space:]]+\.\./' "$OVERLAY_KUSTOMIZATION" || true; } | wc -l)"
  assert_eq "the overlay kustomization has no '../' resource entries" "0" "${parent_refs// /}"
}

test_release_appended_after_vpa() {
  echo "Test: frr-k8s is appended to the Phase 3 wait list after vertical-pod-autoscaler"

  assert_file_contains_fixed "the base helm_releases array is unchanged" \
    "$DEPLOY_INFRA_SH" \
    'helm_releases=(prometheus-operator-crds shared-services/openbao mariadb-operator-crds mariadb-operator external-secrets memcached-operator envoy-gateway garage-operator openbao-operator)'
  assert_file_not_contains "the base array does not name frr-k8s" "$DEPLOY_INFRA_SH" \
    'helm_releases=(.*frr-k8s'

  local append_line vpa_line gate_line
  append_line="$(grep -n 'helm_releases+=(frr-k8s)' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  vpa_line="$(grep -n 'helm_releases+=(vertical-pod-autoscaler)' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  assert_not_empty "helm_releases+=(frr-k8s) exists" "$append_line"
  if [[ -n "$append_line" && -n "$vpa_line" && "$append_line" -gt "$vpa_line" ]]; then
    echo "  PASS: the frr-k8s append follows the vertical-pod-autoscaler append"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the frr-k8s append (line ${append_line:-none}) does not follow the vertical-pod-autoscaler append (line ${vpa_line:-none})"
    FAIL=$((FAIL + 1))
  fi
  gate_line="$(sed -n "$((${append_line:-1} - 1))p" "$DEPLOY_INFRA_SH")"
  assert_eq "the line above the append is the WITH_FRR_K8S gate" \
    '  if [[ "${WITH_FRR_K8S}" == "true" ]]; then' "$gate_line"
}

test_default_is_false
test_explicit_values_are_preserved
test_non_true_value_does_not_trigger_install
test_banner_line
test_apply_is_gated
test_release_appended_after_vpa

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
