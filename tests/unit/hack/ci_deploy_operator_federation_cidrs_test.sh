#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the FEDERATION_METADATA_ALLOW_CIDRS wiring in
# hack/ci-deploy-operator.sh with a stubbed PATH:
#   - when the env holds a CIDR list, the `helm install` argv carries
#     --set federation.metadataAllowCidrs={<cidrs>} (helm brace-list), enabling
#     the operator's federation-metadata SSRF allowlist for in-cluster IdP
#     discovery;
#   - when the env is unset, no federation.metadataAllowCidrs flag is passed, so
#     an older upgrade-baseline chart lacking the value stays undisturbed.
#
# Follows the project-native bash test pattern (tests/lib/assertions.sh),
# mirroring tests/unit/hack/ci_deploy_operator_dependency_build_test.sh.
#
# Usage: bash tests/unit/hack/ci_deploy_operator_federation_cidrs_test.sh

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

# run_deploy <stub_dir> <helm_log> <chart_dir>
# Runs the deploy script with the stub dir prepended to PATH and CHART_DIR
# pointing at the stub chart. FEDERATION_METADATA_ALLOW_CIDRS (optional) is
# inherited from the caller's environment so callers can select the set/unset
# path via a `FEDERATION_METADATA_ALLOW_CIDRS=... run_deploy ...` prefix.
run_deploy() {
  local stub_dir="$1" helm_log="$2" chart_dir="$3"
  (
    PATH="$stub_dir:$PATH"
    export PATH
    export HELM_LOG="$helm_log"
    export OPERATOR="keystone"
    export IMAGE_REPO="ghcr.io/c5c3/keystone-operator"
    export CHART_DIR="$chart_dir"
    bash "$DEPLOY_SH"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test 1: FEDERATION_METADATA_ALLOW_CIDRS set renders the brace-list --set flag
# ---------------------------------------------------------------------------
test_cidrs_set_renders_flag() {
  echo "Test: FEDERATION_METADATA_ALLOW_CIDRS renders the helm brace-list flag"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_stubs "$tmp/bin"
  make_chart "$tmp/chart"

  local output exit_code
  output="$(FEDERATION_METADATA_ALLOW_CIDRS="10.96.0.0/12" \
    run_deploy "$tmp/bin" "$tmp/helm.log" "$tmp/chart")"
  exit_code=$?

  assert_eq "deploy exits 0 with the allowlist set" "0" "$exit_code"
  assert_contains "the resolved-value banner echoes the allowlist" "$output" \
    "Metadata allow CIDRs: 10.96.0.0/12"
  # Braces/dots are literal here: assert_contains matches the quoted needle as a
  # substring, so no grep-regex escaping of {, } or . is required.
  assert_contains "helm install carries the brace-list --set flag" \
    "$(cat "$tmp/helm.log")" \
    "--set federation.metadataAllowCidrs={10.96.0.0/12}"
}

# ---------------------------------------------------------------------------
# Test 2: unset FEDERATION_METADATA_ALLOW_CIDRS omits the flag entirely
# ---------------------------------------------------------------------------
test_cidrs_unset_omits_flag() {
  echo "Test: unset FEDERATION_METADATA_ALLOW_CIDRS omits the flag"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_stubs "$tmp/bin"
  make_chart "$tmp/chart"

  local output exit_code
  output="$(unset FEDERATION_METADATA_ALLOW_CIDRS; \
    run_deploy "$tmp/bin" "$tmp/helm.log" "$tmp/chart")"
  exit_code=$?

  assert_eq "deploy exits 0 with the allowlist unset" "0" "$exit_code"
  assert_file_not_contains "no federation.metadataAllowCidrs flag is passed" \
    "$tmp/helm.log" "federation.metadataAllowCidrs"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_cidrs_set_renders_flag
test_cidrs_unset_omits_flag

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
