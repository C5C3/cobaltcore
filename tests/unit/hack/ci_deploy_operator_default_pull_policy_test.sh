#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the DEFAULT_IMAGE_PULL_POLICY knob of hack/ci-deploy-operator.sh with
# a stubbed PATH:
#   - an unset knob adds --set controller.defaultImagePullPolicy=IfNotPresent
#     to the `helm install` argv and echoes the value in the banner;
#   - DEFAULT_IMAGE_PULL_POLICY=Always passes Always;
#   - DEFAULT_IMAGE_PULL_POLICY=bogus exits 1 with an ::error:: line before
#     kubectl or helm is called;
#   - a chart whose values.schema.json lacks the key, and a chart without a
#     values.schema.json, get no such --set, and the script does not fail;
#   - every chainsaw file that installs an operator chart with
#     image.pullPolicy=Never also sets controller.defaultImagePullPolicy to
#     IfNotPresent, so a second operator release that wins the leader election
#     keeps the workloads on the images CI loaded into kind.
#
# Follows tests/unit/hack/ci_deploy_operator_replicas_test.sh.
#
# Usage: bash tests/unit/hack/ci_deploy_operator_default_pull_policy_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_SH="$PROJECT_ROOT/hack/ci-deploy-operator.sh"
VALUES_FILE="$PROJECT_ROOT/tests/e2e/keystone-operator/network-policy-egress/00-install-operator.yaml"

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

# make_chart <dir> <schema>
# A minimal stub chart with a crds/ directory. <schema> selects its
# values.schema.json: "with-key" names controller.defaultImagePullPolicy,
# "without-key" is a schema that predates it, and "none" writes no file.
make_chart() {
  local dir="$1" schema="$2"
  mkdir -p "$dir/crds"
  printf 'apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n' \
    >"$dir/crds/dummy.yaml"
  case "$schema" in
    with-key)
      printf '{"properties":{"controller":{"properties":{"maxConcurrentReconciles":{},"defaultImagePullPolicy":{}}}}}\n' \
        >"$dir/values.schema.json"
      ;;
    without-key)
      printf '{"properties":{"controller":{"properties":{"maxConcurrentReconciles":{}}}}}\n' \
        >"$dir/values.schema.json"
      ;;
    none) ;;
  esac
}

# run_deploy <tmp>
# Runs the deploy script against the stubs and the stub chart in <tmp>.
# DEFAULT_IMAGE_PULL_POLICY is inherited from the caller's environment.
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

# new_tmp <schema>
new_tmp() {
  local tmp
  tmp="$(mktemp -d)"
  make_stubs "$tmp/bin"
  make_chart "$tmp/chart" "$1"
  touch "$tmp/helm.log" "$tmp/kubectl.log"
  echo "$tmp"
}

# ---------------------------------------------------------------------------
# Test 1: an unset knob passes IfNotPresent
# ---------------------------------------------------------------------------
test_pull_policy_unset() {
  echo "Test: unset DEFAULT_IMAGE_PULL_POLICY passes IfNotPresent"
  local tmp output rc
  tmp="$(new_tmp with-key)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(unset DEFAULT_IMAGE_PULL_POLICY; run_deploy "$tmp")"
  rc=$?

  assert_eq "deploy exits 0" "0" "$rc"
  assert_contains "the banner echoes the value" "$output" "Image pull policy   : IfNotPresent"
  assert_file_contains_fixed "helm install carries the IfNotPresent default" \
    "$tmp/helm.log" "--set controller.defaultImagePullPolicy=IfNotPresent"
}

# ---------------------------------------------------------------------------
# Test 2: an explicit value is passed through
# ---------------------------------------------------------------------------
test_pull_policy_always() {
  echo "Test: DEFAULT_IMAGE_PULL_POLICY=Always passes Always"
  local tmp output rc
  tmp="$(new_tmp with-key)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(DEFAULT_IMAGE_PULL_POLICY=Always run_deploy "$tmp")"
  rc=$?

  assert_eq "deploy exits 0" "0" "$rc"
  assert_contains "the banner echoes the value" "$output" "Image pull policy   : Always"
  assert_file_contains_fixed "helm install carries Always" \
    "$tmp/helm.log" "--set controller.defaultImagePullPolicy=Always"
  assert_file_not_contains "IfNotPresent is not passed" "$tmp/helm.log" "defaultImagePullPolicy=IfNotPresent"
}

# ---------------------------------------------------------------------------
# Test 3: an unknown value exits 1 before anything is applied
# ---------------------------------------------------------------------------
test_pull_policy_invalid() {
  local value
  for value in bogus always; do
    echo "Test: DEFAULT_IMAGE_PULL_POLICY=${value} is rejected"
    local tmp output rc
    tmp="$(new_tmp with-key)"

    output="$(DEFAULT_IMAGE_PULL_POLICY="$value" run_deploy "$tmp")"
    rc=$?

    assert_eq "deploy exits 1" "1" "$rc"
    assert_contains "the ::error:: line names the value" "$output" \
      "::error::DEFAULT_IMAGE_PULL_POLICY='${value}' is not one of Always, IfNotPresent, Never"
    assert_eq "helm is never called" "" "$(cat "$tmp/helm.log")"
    assert_eq "kubectl is never called" "" "$(cat "$tmp/kubectl.log")"
    rm -rf "$tmp"
  done
}

# ---------------------------------------------------------------------------
# Test 4: a chart without the key installs unchanged
# ---------------------------------------------------------------------------
test_chart_without_key() {
  local schema
  for schema in without-key none; do
    echo "Test: a chart with schema '${schema}' gets no defaultImagePullPolicy"
    local tmp output rc
    tmp="$(new_tmp "$schema")"

    output="$(unset DEFAULT_IMAGE_PULL_POLICY; run_deploy "$tmp")"
    rc=$?

    assert_eq "deploy exits 0" "0" "$rc"
    assert_contains "the banner says the chart has no such value" "$output" \
      "Image pull policy   : <chart has no such value>"
    assert_file_contains "helm install still runs" "$tmp/helm.log" "helm install"
    assert_file_not_contains "no defaultImagePullPolicy is passed" "$tmp/helm.log" "defaultImagePullPolicy"
    assert_not_contains "no error about the missing file" "$output" "No such file"
    rm -rf "$tmp"
  done
}

# ---------------------------------------------------------------------------
# Test 5: the chainsaw install sites carry the default too
# ---------------------------------------------------------------------------
test_chainsaw_install_sites() {
  echo "Test: every chainsaw operator install sets defaultImagePullPolicy IfNotPresent"
  local files file count=0
  files="$(cd "$PROJECT_ROOT" && grep -rlE 'image\.pullPolicy=Never' tests/e2e* | sort)"
  for file in $files; do
    count=$((count + 1))
    assert_file_contains_fixed "$file sets the operator default" \
      "$PROJECT_ROOT/$file" "--set controller.defaultImagePullPolicy=IfNotPresent"
  done
  assert_gte "the scan finds chainsaw files that install an operator chart with --set" "$count" "1"

  assert_file_contains "the network-policy-egress values file names image.pullPolicy" \
    "$VALUES_FILE" "^  pullPolicy: Never"
  assert_file_contains "the network-policy-egress values file sets the operator default" \
    "$VALUES_FILE" "^  defaultImagePullPolicy: IfNotPresent"
  # A Helm values file nests pullPolicy two spaces under the top-level image
  # key; a CR fixture nests spec.image.pullPolicy deeper.
  assert_eq "no other values file names an operator image.pullPolicy" \
    "tests/e2e/keystone-operator/network-policy-egress/00-install-operator.yaml" \
    "$(cd "$PROJECT_ROOT" && grep -rlE '^  pullPolicy: Never$' tests/e2e* | sort | tr '\n' ' ' | sed 's/ $//')"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_pull_policy_unset
test_pull_policy_always
test_pull_policy_invalid
test_chart_without_key
test_chainsaw_install_sites

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
