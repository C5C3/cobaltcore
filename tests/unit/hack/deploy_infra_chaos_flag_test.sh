#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh gates chaos-mesh behind WITH_CHAOS_MESH so the
# kind Quick Start stays minimal by default and only installs Chaos Mesh when
# explicitly requested.
#
# Implementation: bash + tests/lib/assertions.sh (project-native, matches the
# sibling tests/unit/hack/deploy_infra_preflight_test.sh pattern). The repo
# has zero .bats files and no bats binary on CI, so introducing one would
# add an undeclared dependency.
#
# Strategy: hybrid — source the script (the `BASH_SOURCE[0] == ${0}` guard
# at the bottom of deploy-infra.sh keeps main() from auto-running) to assert
# the runtime default of WITH_CHAOS_MESH for each env scenario, and grep the
# script source to lock in the three gate locations (kernel-modules,
# kustomize apply, helm-releases array). Full main()-stubbing requires
# shimming ~10 wait_for_* helpers, which would obscure the contract under
# test.
#
# Usage: bash tests/unit/hack/deploy_infra_chaos_flag_test.sh

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

# resolve_with_chaos_mesh [env_var=value...]
# Sources deploy-infra.sh in a subshell with the supplied env overrides and
# echoes the resolved value of WITH_CHAOS_MESH after the configuration block
# runs. The `BASH_SOURCE[0] == ${0}` guard at the bottom of deploy-infra.sh
# keeps main() from auto-running when the script is sourced.
resolve_with_chaos_mesh() {
  (
    # Apply each env override in the subshell before sourcing.
    for assignment in "$@"; do
      export "${assignment?}"
    done
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    printf '%s' "${WITH_CHAOS_MESH}"
  )
}

# ---------------------------------------------------------------------------
# Test 1: WITH_CHAOS_MESH defaults to false
# ---------------------------------------------------------------------------
test_default_is_false() {
  echo "Test: WITH_CHAOS_MESH defaults to false"

  # Unset any inherited value so we observe the script's own default.
  local resolved
  resolved="$(unset WITH_CHAOS_MESH; resolve_with_chaos_mesh)"
  assert_eq "WITH_CHAOS_MESH defaults to false" "false" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 2: explicit WITH_CHAOS_MESH=true
# ---------------------------------------------------------------------------
test_explicit_true() {
  echo "Test: WITH_CHAOS_MESH=true is preserved"

  local resolved
  resolved="$(resolve_with_chaos_mesh WITH_CHAOS_MESH=true)"
  assert_eq "WITH_CHAOS_MESH=true is preserved" "true" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 3: explicit WITH_CHAOS_MESH=false
# ---------------------------------------------------------------------------
test_explicit_false() {
  echo "Test: WITH_CHAOS_MESH=false is preserved"

  local resolved
  resolved="$(resolve_with_chaos_mesh WITH_CHAOS_MESH=false)"
  assert_eq "WITH_CHAOS_MESH=false is preserved" "false" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 4: defensive non-true value
# A typo like WITH_CHAOS_MESH=yes should NOT enable chaos-mesh because every
# gate site uses the strict `== "true"` comparison. We assert the value
# passes through verbatim AND that the three gate sites use exact-match.
# ---------------------------------------------------------------------------
test_non_true_value_does_not_trigger_install() {
  echo "Test: WITH_CHAOS_MESH=yes passes through but does not trigger install"

  local resolved
  resolved="$(resolve_with_chaos_mesh WITH_CHAOS_MESH=yes)"
  assert_eq "WITH_CHAOS_MESH=yes is preserved verbatim" "yes" "$resolved"

  # Every gate compares with the exact string "true"; "yes" therefore takes
  # the skip branch at all three sites. Lock that in by counting matches.
  local gate_count
  gate_count="$(grep -cE '"\$\{WITH_CHAOS_MESH\}" == "true"' "$DEPLOY_INFRA_SH" || true)"
  assert_eq "deploy-infra.sh has exactly 3 strict WITH_CHAOS_MESH==true gates" "3" "$gate_count"
}

# ---------------------------------------------------------------------------
# Test 5: chaos-mesh overlay apply is conditional
# The kustomize apply for deploy/kind/chaos-mesh must live inside the
# WITH_CHAOS_MESH gate so the default Quick Start does not install it.
# ---------------------------------------------------------------------------
test_chaos_mesh_kustomize_is_gated() {
  echo "Test: chaos-mesh kustomize apply is gated by WITH_CHAOS_MESH"

  # The apply line itself exists.
  assert_file_contains \
    "deploy-infra.sh references the deploy/kind/chaos-mesh overlay" \
    "$DEPLOY_INFRA_SH" \
    'deploy/kind/chaos-mesh'

  # The line must be preceded by a WITH_CHAOS_MESH gate. Use awk to confirm
  # the most recent if-line above the apply tests WITH_CHAOS_MESH.
  local apply_line gate_line
  apply_line="$(grep -n 'kubectl apply -k "${REPO_ROOT}/deploy/kind/chaos-mesh"' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  gate_line="$(grep -n '"${WITH_CHAOS_MESH}" == "true"' "$DEPLOY_INFRA_SH" | awk -F: -v target="${apply_line:-0}" '$1 < target { last = $1 } END { print last }')"

  assert_not_empty "kustomize apply line for chaos-mesh is found" "$apply_line"
  assert_not_empty "WITH_CHAOS_MESH gate precedes the kustomize apply" "$gate_line"
}

# ---------------------------------------------------------------------------
# Test 6: kernel-module load is gated
# load_chaos_mesh_kernel_modules must only run when WITH_CHAOS_MESH=true so
# the default Quick Start does not require sudo / modprobe.
# ---------------------------------------------------------------------------
test_kernel_module_call_is_gated() {
  echo "Test: load_chaos_mesh_kernel_modules call is gated by WITH_CHAOS_MESH"

  local call_line gate_line
  # Find the call site (not the function definition). The call is invoked
  # with no arguments and at indentation, so we match a line that is exactly
  # the call (allowing leading whitespace).
  call_line="$(grep -nE '^[[:space:]]+load_chaos_mesh_kernel_modules$' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  gate_line="$(grep -n '"${WITH_CHAOS_MESH}" == "true"' "$DEPLOY_INFRA_SH" | awk -F: -v target="${call_line:-0}" '$1 < target { last = $1 } END { print last }')"

  assert_not_empty "load_chaos_mesh_kernel_modules call site is found" "$call_line"
  assert_not_empty "WITH_CHAOS_MESH gate precedes the kernel-module load" "$gate_line"
}

# ---------------------------------------------------------------------------
# Test 7: chaos-mesh dropped from the default helm-release wait list
# The Phase 3 wait must NOT statically include chaos-mesh; it must be
# appended dynamically inside the gate.
# ---------------------------------------------------------------------------
test_chaos_mesh_not_in_default_helm_releases() {
  echo "Test: chaos-mesh is appended dynamically to the helm-release wait list"

  # The static (legacy) one-liner that contained chaos-mesh inline must be
  # gone. Asserting on the surviving order is the strongest contract.
  assert_file_not_contains \
    "deploy-infra.sh no longer hard-codes chaos-mesh in the helm-release wait list" \
    "$DEPLOY_INFRA_SH" \
    'memcached-operator chaos-mesh envoy-gateway'

  # The new dynamic array must include the seven non-chaos releases in order
  # and append chaos-mesh inside the gate.
  assert_file_contains \
    "helm_releases array preserves the seven non-chaos releases in the documented order" \
    "$DEPLOY_INFRA_SH" \
    'helm_releases=(prometheus-operator-crds openbao mariadb-operator-crds mariadb-operator external-secrets memcached-operator envoy-gateway)'

  assert_file_contains \
    "chaos-mesh is appended only inside the WITH_CHAOS_MESH gate" \
    "$DEPLOY_INFRA_SH" \
    'helm_releases+=(chaos-mesh)'
}

# ---------------------------------------------------------------------------
# Test 8: production-caller contract — the chaos-mesh apply mirrors the
# unit-test render and works under kubectl's embedded kustomize.
#
# kubectl apply -k uses the embedded kustomize, which does NOT expose
# --load-restrictor (kubernetes/kubectl#948). The first review of
# caught a contract drift: the unit test rendered the overlay with
# `kustomize build --load-restrictor=LoadRestrictionsNone` while
# deploy-infra.sh used `kubectl apply -k` with no flag — a green unit test
# masked a broken production caller. This test pins both halves of the
# contract:
#
#   (a) deploy-infra.sh's chaos-mesh apply line uses plain `kubectl apply -k`
#       (no `--load-restrictor`, no pipe through `kustomize build` to
#       `kubectl apply -f -`); and
#   (b) deploy/kind/chaos-mesh/kustomization.yaml has zero `../../` parent-
#       directory resource entries, so the default LoadRestrictionsRootOnly
#       check is satisfied and the apply succeeds without any flag.
#
# Either half changing must update the other. If a future change re-introduces
# parent-dir references, this test fails until the caller is updated to a
# `kustomize build --load-restrictor=LoadRestrictionsNone | kubectl apply -f -`
# pipeline (and the standalone `kustomize` CLI is added to install-test-deps).
# ---------------------------------------------------------------------------
test_production_caller_matches_self_contained_overlay() {
  echo "Test: production caller and overlay agree on the no-load-restrictor contract"

  # (a) The apply line is the bare kubectl form — no pipe, no flag.
  local raw
  raw="$(grep -E 'kubectl apply -k "\$\{REPO_ROOT\}/deploy/kind/chaos-mesh"' "$DEPLOY_INFRA_SH" | head -1)"
  assert_not_empty "deploy-infra.sh has the chaos-mesh kubectl apply line" "$raw"

  if printf '%s' "$raw" | grep -q -- '--load-restrictor'; then
    echo "  FAIL: deploy-infra.sh's chaos-mesh apply line passes --load-restrictor (kubectl's embedded kustomize does not accept it; switch to a 'kustomize build | kubectl apply -f -' pipeline if you really need that flag)"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: chaos-mesh apply line uses no --load-restrictor flag"
    PASS=$((PASS + 1))
  fi

  # Also reject a covert pipeline form that bypasses the contract: piping
  # `kustomize build` into `kubectl apply -f -` would silently re-introduce
  # the standalone-kustomize dependency without updating install-test-deps.
  if grep -E 'kustomize build.*\| *kubectl apply -f -' "$DEPLOY_INFRA_SH" >/dev/null 2>&1; then
    echo "  FAIL: deploy-infra.sh pipes kustomize build into kubectl apply (introduces undeclared kustomize CLI dependency; remove or wire kustomize into install-test-deps)"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: deploy-infra.sh does not pipe kustomize build into kubectl apply"
    PASS=$((PASS + 1))
  fi

  # (b) The overlay must have zero `../../` parent-directory resource entries
  # so the default LoadRestrictionsRootOnly check is satisfied.
  local kust="$PROJECT_ROOT/deploy/kind/chaos-mesh/kustomization.yaml"
  if [[ ! -f "$kust" ]]; then
    echo "  FAIL: $kust does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  local parent_refs
  parent_refs="$( { grep -E '^[[:space:]]*-[[:space:]]+\.\./\.\.' "$kust" || true; } | wc -l)"
  assert_eq "deploy/kind/chaos-mesh/kustomization.yaml has no '../../' resource entries" \
    "0" "${parent_refs// /}"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_default_is_false
test_explicit_true
test_explicit_false
test_non_true_value_does_not_trigger_install
test_chaos_mesh_kustomize_is_gated
test_kernel_module_call_is_gated
test_chaos_mesh_not_in_default_helm_releases
test_production_caller_matches_self_contained_overlay

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
