#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the HELM_UPGRADE option of hack/ci-deploy-operator.sh with a stubbed
# PATH:
#   - HELM_UPGRADE=true runs `helm upgrade <op>-operator` with the --set list an
#     install gets and without --create-namespace, after the server-side CRD
#     apply;
#   - an unset HELM_UPGRADE and HELM_UPGRADE=false both run `helm install ...
#     --create-namespace`;
#   - HELM_UPGRADE=yes exits 1 with an ::error:: line before anything is
#     applied;
#   - an upgrade of barbican re-passes the TokenRequest grant of
#     BARBICAN_SECRET_STORE_GRANTS, which a hand-written `helm upgrade` would
#     drop;
#   - Helm's "has no deployed releases" failure of an upgrade fails the script.
#
# The operator upgrade suites under tests/e2e-operator-upgrade/ upgrade through
# this option, so a dropped --set or a stray --create-namespace would change
# what every suite upgrades to.
#
# Follows tests/unit/hack/ci_deploy_operator_replicas_test.sh.
#
# Usage: bash tests/unit/hack/ci_deploy_operator_helm_upgrade_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_SH="$PROJECT_ROOT/hack/ci-deploy-operator.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stubs <dir>
# helm logs its argv to $HELM_LOG and, like kubectl, to the shared $ORDER_LOG.
# With HELM_UPGRADE_FAIL=1 an upgrade prints Helm's error for a release that
# does not exist and exits 1. kubectl logs its argv to $KUBECTL_LOG, returns
# non-zero for `get mutatingwebhookconfigurations` so the deploy script skips
# the webhook readiness wait, and succeeds otherwise.
make_stubs() {
  local dir="$1"
  mkdir -p "$dir"

  cat >"$dir/helm" <<'STUB'
#!/bin/bash
echo "helm $*" >>"$HELM_LOG"
echo "helm $*" >>"$ORDER_LOG"
if [ "${1:-}" = "upgrade" ] && [ "${HELM_UPGRADE_FAIL:-}" = "1" ]; then
  echo "Error: UPGRADE FAILED: \"$2\" has no deployed releases" >&2
  exit 1
fi
exit 0
STUB
  chmod +x "$dir/helm"

  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
echo "kubectl $*" >>"$KUBECTL_LOG"
echo "kubectl $*" >>"$ORDER_LOG"
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

# run_deploy <tmp> [<operator>]
# Runs the deploy script against the stubs and the stub chart in <tmp>.
# HELM_UPGRADE, HELM_UPGRADE_FAIL and BARBICAN_SECRET_STORE_GRANTS are
# inherited from the caller's environment.
run_deploy() {
  local tmp="$1" operator="${2:-keystone}"
  (
    PATH="$tmp/bin:$PATH"
    export PATH
    export HELM_LOG="$tmp/helm.log"
    export KUBECTL_LOG="$tmp/kubectl.log"
    export ORDER_LOG="$tmp/order.log"
    export OPERATOR="$operator"
    export IMAGE_REPO="ghcr.io/c5c3/${operator}-operator"
    export CHART_DIR="$tmp/chart"
    bash "$DEPLOY_SH"
  ) 2>&1
}

new_tmp() {
  local tmp
  tmp="$(mktemp -d)"
  make_stubs "$tmp/bin"
  make_chart "$tmp/chart"
  touch "$tmp/helm.log" "$tmp/kubectl.log" "$tmp/order.log"
  echo "$tmp"
}

# release_line <log> <install|upgrade>
# Echoes the first `helm install` or `helm upgrade` line of a helm log.
release_line() {
  grep -m1 "^helm $2 " "$1" || true
}

# set_args <line>
# Echoes every "--set <value>" pair of a helm argv line, one per line.
set_args() {
  # shellcheck disable=SC2086 # deliberate: split the logged argv
  set -- $1
  while [ $# -gt 0 ]; do
    if [ "$1" = "--set" ] && [ $# -gt 1 ]; then
      echo "--set $2"
      shift
    fi
    shift
  done
}

# ---------------------------------------------------------------------------
# Test 1: HELM_UPGRADE=true upgrades with the install's --set list
# ---------------------------------------------------------------------------
test_upgrade_mode() {
  echo "Test: HELM_UPGRADE=true runs helm upgrade with the install's arguments"
  local tmp install_tmp output rc upgrade install
  tmp="$(new_tmp)"
  install_tmp="$(new_tmp)"
  trap 'rm -rf "$tmp" "$install_tmp"' RETURN

  output="$(HELM_UPGRADE=true run_deploy "$tmp")"
  rc=$?
  (unset HELM_UPGRADE; run_deploy "$install_tmp") >/dev/null

  assert_eq "deploy exits 0" "0" "$rc"
  assert_contains "the banner names the mode" "$output" "Helm upgrade        : true"
  upgrade="$(release_line "$tmp/helm.log" upgrade)"
  install="$(release_line "$install_tmp/helm.log" install)"
  assert_starts_with "helm upgrade targets the keystone-operator release" "$upgrade" \
    "helm upgrade keystone-operator $tmp/chart/ --namespace keystone-system"
  assert_not_contains "the upgrade passes no --create-namespace" "$upgrade" "--create-namespace"
  assert_file_not_contains "no helm install runs" "$tmp/helm.log" "^helm install"
  assert_not_empty "the install run passes --set arguments" "$(set_args "$install")"
  assert_eq "the upgrade passes the --set arguments the install passes" \
    "$(set_args "$install")" "$(set_args "$upgrade")"
  assert_contains "the upgrade waits like the install" "$upgrade" "--wait --timeout 120s"

  local apply_at upgrade_at
  apply_at="$(grep -n -m1 '^kubectl apply --server-side' "$tmp/order.log" | cut -d: -f1)"
  upgrade_at="$(grep -n -m1 '^helm upgrade ' "$tmp/order.log" | cut -d: -f1)"
  assert_not_empty "the CRDs are applied server-side" "$apply_at"
  if [ -n "$apply_at" ] && [ -n "$upgrade_at" ] && [ "$apply_at" -lt "$upgrade_at" ]; then
    assert_eq "the CRD apply runs before the helm upgrade" "yes" "yes"
  else
    assert_eq "the CRD apply runs before the helm upgrade" "yes" \
      "no (apply at line '${apply_at}', upgrade at line '${upgrade_at}')"
  fi
}

# ---------------------------------------------------------------------------
# Test 2: unset and false both install with --create-namespace
# ---------------------------------------------------------------------------
test_install_mode() {
  local form
  for form in unset false; do
    echo "Test: HELM_UPGRADE ${form} runs helm install --create-namespace"
    local tmp output rc
    tmp="$(new_tmp)"

    if [ "$form" = "unset" ]; then
      output="$(unset HELM_UPGRADE; run_deploy "$tmp")"
    else
      output="$(HELM_UPGRADE=false run_deploy "$tmp")"
    fi
    rc=$?

    assert_eq "deploy exits 0" "0" "$rc"
    assert_contains "the banner names the mode" "$output" "Helm upgrade        : false"
    assert_contains "helm install creates the namespace" \
      "$(release_line "$tmp/helm.log" install)" "--namespace keystone-system --create-namespace"
    assert_file_not_contains "no helm upgrade runs" "$tmp/helm.log" "^helm upgrade"
    rm -rf "$tmp"
  done
}

# ---------------------------------------------------------------------------
# Test 3: any other value exits 1 before anything is applied
# ---------------------------------------------------------------------------
test_invalid_value() {
  echo "Test: HELM_UPGRADE=yes is rejected before anything is applied"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(HELM_UPGRADE=yes run_deploy "$tmp")"
  rc=$?

  assert_eq "deploy exits 1" "1" "$rc"
  assert_contains "the ::error:: line names the value" "$output" \
    "::error::HELM_UPGRADE must be true or false (got: 'yes')"
  assert_eq "kubectl is never called" "" "$(cat "$tmp/kubectl.log")"
  assert_eq "helm is never called" "" "$(cat "$tmp/helm.log")"
}

# ---------------------------------------------------------------------------
# Test 4: a barbican upgrade keeps the TokenRequest grant
# ---------------------------------------------------------------------------
test_barbican_upgrade_keeps_the_grant() {
  echo "Test: a barbican upgrade re-passes BARBICAN_SECRET_STORE_GRANTS"
  local tmp rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  HELM_UPGRADE=true BARBICAN_SECRET_STORE_GRANTS=openstack=openbao-instance-provisioner \
    run_deploy "$tmp" barbican >/dev/null
  rc=$?

  assert_eq "deploy exits 0" "0" "$rc"
  assert_contains "the helm upgrade line carries the grant" \
    "$(release_line "$tmp/helm.log" upgrade)" \
    "--set rbac.secretStoreNamespaces.openstack={openbao-instance-provisioner}"
}

# ---------------------------------------------------------------------------
# Test 5: Helm's failure of an upgrade without a release fails the script
# ---------------------------------------------------------------------------
test_upgrade_without_a_release_fails() {
  echo "Test: an upgrade of a release that does not exist exits non-zero"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(HELM_UPGRADE=true HELM_UPGRADE_FAIL=1 run_deploy "$tmp")"
  rc=$?

  assert_nonzero_exit "deploy exits non-zero" "$rc"
  assert_contains "Helm's error reaches the log" "$output" \
    'Error: UPGRADE FAILED: "keystone-operator" has no deployed releases'
  assert_file_not_contains "the webhook wait never runs" "$tmp/kubectl.log" \
    "get mutatingwebhookconfigurations"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_upgrade_mode
test_install_mode
test_invalid_value
test_barbican_upgrade_keeps_the_grant
test_upgrade_without_a_release_fails

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
