#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the BARBICAN_SECRET_STORE_GRANTS wiring in hack/ci-deploy-operator.sh
# with a stubbed PATH:
#   - for OPERATOR=barbican, each <namespace>=<serviceAccount> pair renders its
#     own --set rbac.secretStoreNamespaces.<namespace>={<serviceAccount>} (helm
#     brace-list), granting the operator the TokenRequest Role a managed
#     BarbicanSecretStore needs there, restricted to the provisioner account it
#     may mint for — and to nothing a *different* Namespace's pair named;
#   - a REPEATED Namespace accumulates its accounts into that one flag instead
#     of emitting the key twice, which helm --set resolves by replacement;
#   - an entry that is not a pair is REJECTED before helm runs: the chart
#     refuses to render an unrestricted Role, which would cover every
#     ServiceAccount in a shared tenant Namespace;
#   - for any other operator the flags are dropped even when the env is set, so
#     a chart without the value (additionalProperties:false values schema) is
#     not handed an unknown key;
#   - for OPERATOR=barbican with the env unset, no flag is passed and the chart
#     default (empty map) stands.
#
# Follows the project-native bash test pattern (tests/lib/assertions.sh),
# mirroring tests/unit/hack/ci_deploy_operator_federation_cidrs_test.sh.
#
# Usage: bash tests/unit/hack/ci_deploy_operator_secret_store_grants_test.sh

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
# Writes helm/kubectl stubs into <dir>. The helm stub logs its full argv to
# $HELM_LOG and always succeeds. The kubectl stub returns non-zero for
# `get mutatingwebhookconfigurations` so the deploy script skips the webhook
# readiness wait (and its sleeps), and succeeds for everything else.
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
if [ "${1:-}" = "get" ] && [ "${2:-}" = "mutatingwebhookconfigurations" ]; then
  exit 1
fi
exit 0
STUB
  chmod +x "$dir/kubectl"
}

# make_chart <dir>
# Materialises a minimal stub chart under <dir>: a crds/ directory (the deploy
# script runs `kubectl apply -f <chart>/crds/`), so the run never touches the
# real chart tree.
make_chart() {
  local dir="$1"
  mkdir -p "$dir/crds"
  printf 'apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n' \
    >"$dir/crds/dummy.yaml"
}

# run_deploy <operator> <stub_dir> <helm_log> <chart_dir>
# Runs the deploy script for <operator> with the stub dir prepended to PATH and
# CHART_DIR pointing at the stub chart. BARBICAN_SECRET_STORE_GRANTS (optional)
# is inherited from the caller's environment so callers can select the set/unset
# path via a `BARBICAN_SECRET_STORE_GRANTS=... run_deploy ...` prefix.
run_deploy() {
  local operator="$1" stub_dir="$2" helm_log="$3" chart_dir="$4"
  (
    PATH="$stub_dir:$PATH"
    export PATH
    export HELM_LOG="$helm_log"
    export OPERATOR="$operator"
    export IMAGE_REPO="ghcr.io/c5c3/${operator}-operator"
    export CHART_DIR="$chart_dir"
    bash "$DEPLOY_SH"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test 1: barbican with the env set renders one brace-list --set flag per pair
# ---------------------------------------------------------------------------
# Two pairs, two DIFFERENT accounts: the assertion that each Namespace carries
# only its own account is what a flat namespace list plus a flat account list
# could not satisfy — that shape crosses every account into every Namespace.
test_barbican_renders_flag() {
  echo "Test: BARBICAN_SECRET_STORE_GRANTS renders one brace-list flag per pair"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_stubs "$tmp/bin"
  make_chart "$tmp/chart"

  local exit_code
  BARBICAN_SECRET_STORE_GRANTS="openstack=openbao-instance-provisioner,tenant-a=openbao-tenant-a-provisioner" \
    run_deploy barbican "$tmp/bin" "$tmp/helm.log" "$tmp/chart" >/dev/null
  exit_code=$?

  assert_eq "deploy exits 0 with both pairs set" "0" "$exit_code"
  # Braces are literal here: assert_contains matches the quoted needle as a
  # substring, so no grep-regex escaping of { or } is required (and a needle
  # starting with -- is not mistaken for a grep option).
  assert_contains "helm install grants openstack its own account" \
    "$(cat "$tmp/helm.log")" \
    "--set rbac.secretStoreNamespaces.openstack={openbao-instance-provisioner}"
  assert_contains "helm install grants tenant-a its own account" \
    "$(cat "$tmp/helm.log")" \
    "--set rbac.secretStoreNamespaces.tenant-a={openbao-tenant-a-provisioner}"
  assert_file_not_contains "tenant-a's account never lands in the openstack grant" \
    "$tmp/helm.log" "rbac.secretStoreNamespaces.openstack={openbao-tenant-a-provisioner}"
}

# ---------------------------------------------------------------------------
# Test 1a: two pairs for the SAME Namespace land in one flag
# ---------------------------------------------------------------------------
# A Namespace hosting two OpenBaoClusters needs both provisioner accounts in its
# grant. Emitting the Namespace key twice does not do that: helm --set assigns a
# brace-list by replacement, so the last pair would win and the store bound to
# the first instance would stall on a denied TokenRequest — silently, because
# the deploy still exits 0.
test_barbican_repeated_namespace_accumulates() {
  echo "Test: two pairs for one Namespace accumulate into a single flag"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_stubs "$tmp/bin"
  make_chart "$tmp/chart"

  local exit_code
  BARBICAN_SECRET_STORE_GRANTS="openstack=openbao-a-provisioner,openstack=openbao-b-provisioner" \
    run_deploy barbican "$tmp/bin" "$tmp/helm.log" "$tmp/chart" >/dev/null
  exit_code=$?

  assert_eq "deploy exits 0 with both pairs set" "0" "$exit_code"
  assert_contains "both accounts land in the openstack grant" \
    "$(cat "$tmp/helm.log")" \
    "--set rbac.secretStoreNamespaces.openstack={openbao-a-provisioner,openbao-b-provisioner}"
  assert_file_not_contains "the first account is not dropped by the second" \
    "$tmp/helm.log" "rbac.secretStoreNamespaces.openstack={openbao-b-provisioner}"
}

# ---------------------------------------------------------------------------
# Test 1b: an entry that is not a <namespace>=<serviceAccount> pair is rejected
# ---------------------------------------------------------------------------
# The account is what keeps the rendered Role from covering every ServiceAccount
# in the Namespace — in openstack that includes the accounts reading database
# credentials and tenant secrets out of OpenBao. Fail before helm runs so the
# misconfiguration is named, not surfaced as a template error.
test_barbican_bare_namespace_is_rejected() {
  echo "Test: a bare Namespace without its ServiceAccount fails"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_stubs "$tmp/bin"
  make_chart "$tmp/chart"
  # The helm stub never runs on this path, so seed the log the assertion reads.
  : >"$tmp/helm.log"

  local output exit_code
  output="$(
    BARBICAN_SECRET_STORE_GRANTS="openstack" \
      run_deploy barbican "$tmp/bin" "$tmp/helm.log" "$tmp/chart"
  )"
  exit_code=$?

  assert_eq "deploy fails when the grant would be unrestricted" "1" "$exit_code"
  assert_contains "the error names the malformed entry" \
    "$output" "is not a <namespace>=<serviceAccount> pair"
  assert_file_not_contains "helm never installs the unrestricted grant" \
    "$tmp/helm.log" "rbac.secretStoreNamespaces"
}

# ---------------------------------------------------------------------------
# Test 2: a non-barbican operator drops the flag even with the env set
# ---------------------------------------------------------------------------
test_other_operator_omits_flag() {
  echo "Test: a non-barbican operator ignores BARBICAN_SECRET_STORE_GRANTS"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_stubs "$tmp/bin"
  make_chart "$tmp/chart"

  local exit_code
  BARBICAN_SECRET_STORE_GRANTS="openstack=openbao-instance-provisioner" \
    run_deploy keystone "$tmp/bin" "$tmp/helm.log" "$tmp/chart" >/dev/null
  exit_code=$?

  assert_eq "deploy exits 0 on the keystone leg" "0" "$exit_code"
  assert_file_not_contains "no rbac.secretStoreNamespaces flag is passed" \
    "$tmp/helm.log" "rbac.secretStoreNamespaces"
}

# ---------------------------------------------------------------------------
# Test 3: barbican with the env unset omits the flag entirely
# ---------------------------------------------------------------------------
test_barbican_unset_omits_flag() {
  echo "Test: unset BARBICAN_SECRET_STORE_GRANTS omits the flag"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_stubs "$tmp/bin"
  make_chart "$tmp/chart"

  local exit_code
  (
    unset BARBICAN_SECRET_STORE_GRANTS
    run_deploy barbican "$tmp/bin" "$tmp/helm.log" "$tmp/chart" >/dev/null
  )
  exit_code=$?

  assert_eq "deploy exits 0 with the grant list unset" "0" "$exit_code"
  assert_file_not_contains "no rbac.secretStoreNamespaces flag is passed" \
    "$tmp/helm.log" "rbac.secretStoreNamespaces"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_barbican_renders_flag
test_barbican_repeated_namespace_accumulates
test_barbican_bare_namespace_is_rejected
test_other_operator_omits_flag
test_barbican_unset_omits_flag

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
