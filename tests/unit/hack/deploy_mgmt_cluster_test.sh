#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-mgmt-cluster.sh brings up the management half of the
# two-cluster devstack the way the target half expects it.
#
# The script cannot be exercised against a cluster here, so the contract is
# pinned structurally, on the parts that fail silently or expensively:
#   - the flux-operator pin equals hack/deploy-infra.sh's, so the two clusters
#     bootstrap the same Flux and one Renovate bump moves both
#   - every deploy/flux-system/ manifest it applies exists (a renamed source or
#     release file otherwise surfaces as a failed deploy minutes in)
#   - cert-manager is waited for before the rest, since hack/ci-deploy-operator.sh
#     needs its CA injection and two releases declare a dependsOn on it
#   - the `openstack` namespace the placed CRs live in is created
#   - the script is syntactically valid and shellcheck-clean
#
# Usage: bash tests/unit/hack/deploy_mgmt_cluster_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
MGMT_SH="$PROJECT_ROOT/hack/deploy-mgmt-cluster.sh"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# The source/release pairs the script applies, in the order it applies them.
# Duplicated here on purpose: this list is the contract, and a reordering or a
# dropped pair has to fail the test rather than be read out of the script it
# tests.
EXPECTED_PAIRS=(
  "sources/cert-manager.yaml|releases/cert-manager.yaml|cert-manager/cert-manager"
  "sources/mariadb-operator.yaml|releases/mariadb-operator-crds.yaml|mariadb-system/mariadb-operator-crds"
  "sources/external-secrets.yaml|releases/external-secrets.yaml|external-secrets/external-secrets"
  "sources/openbao-operator.yaml|releases/openbao-operator.yaml|openbao-operator-system/openbao-operator"
  "sources/prometheus-community.yaml|releases/prometheus-operator-crds.yaml|monitoring/prometheus-operator-crds"
)

# ---------------------------------------------------------------------------
# Test 1: the cluster name defaults to cobaltcore-mgmt and honours an override
# ---------------------------------------------------------------------------
test_cluster_name_default() {
  echo "Test: CLUSTER_NAME defaults to cobaltcore-mgmt"

  local resolved
  resolved="$(unset CLUSTER_NAME; bash -c 'source "$1"; printf "%s" "$CLUSTER_NAME"' _ "$MGMT_SH")"
  assert_eq "CLUSTER_NAME defaults to cobaltcore-mgmt" "cobaltcore-mgmt" "$resolved"

  resolved="$(CLUSTER_NAME=other bash -c 'source "$1"; printf "%s" "$CLUSTER_NAME"' _ "$MGMT_SH")"
  assert_eq "an explicit CLUSTER_NAME is preserved" "other" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 2: the flux-operator pin matches hack/deploy-infra.sh
# Two clusters bootstrapping different flux-operator releases is a difference
# nothing else in the flow surfaces.
# ---------------------------------------------------------------------------
test_flux_operator_pin_matches_deploy_infra() {
  echo "Test: the flux-operator pin equals hack/deploy-infra.sh's"

  local mgmt_pin infra_pin
  mgmt_pin="$(grep -E '^FLUX_OPERATOR_VERSION=' "$MGMT_SH" | head -1 | sed -E 's/^FLUX_OPERATOR_VERSION="([^"]+)".*/\1/')"
  infra_pin="$(grep -E '^FLUX_OPERATOR_VERSION=' "$DEPLOY_INFRA_SH" | head -1 | sed -E 's/^FLUX_OPERATOR_VERSION="([^"]+)".*/\1/')"

  assert_not_empty "hack/deploy-mgmt-cluster.sh pins FLUX_OPERATOR_VERSION" "$mgmt_pin"
  assert_eq "the two pins are equal" "$infra_pin" "$mgmt_pin"

  # The pin has to be reachable by the customManagers regex, which anchors on the
  # bare assignment form — a `${FLUX_OPERATOR_VERSION:-...}` default would leave
  # the pin unbumped without any error.
  assert_file_contains "the pin uses the bare assignment form Renovate matches" \
    "$MGMT_SH" "FLUX_OPERATOR_VERSION=\"${mgmt_pin}\""
  # Coverage is asserted in detail (manager pattern plus the paired
  # packageRules) by tests/unit/renovate/fluxoperator_custommanager_test.sh;
  # here it is only the presence of the path in the Renovate config.
  assert_file_contains "renovate.json names the file" \
    "$PROJECT_ROOT/renovate.json" 'hack/deploy-mgmt-cluster'
}

# ---------------------------------------------------------------------------
# Test 3: every applied manifest exists, in the documented order
# ---------------------------------------------------------------------------
test_applied_manifests_exist_in_order() {
  echo "Test: the source/release pairs are declared in order and all files exist"

  local declared
  declared="$(awk '/^FLUX_RELEASES=\(/ { in_block = 1; next }
                   in_block && /^\)/ { exit }
                   in_block { gsub(/^[[:space:]]*"|"[[:space:]]*$/, ""); print }' "$MGMT_SH")"

  local expected
  expected="$(printf '%s\n' "${EXPECTED_PAIRS[@]}")"
  assert_eq "FLUX_RELEASES lists the expected pairs in order" "$expected" "$declared"

  local entry source_file release_file rest
  for entry in "${EXPECTED_PAIRS[@]}"; do
    source_file="${entry%%|*}"
    rest="${entry#*|}"
    release_file="${rest%%|*}"
    for file in "$source_file" "$release_file"; do
      if [ -f "$PROJECT_ROOT/deploy/flux-system/$file" ]; then
        echo "  PASS: deploy/flux-system/$file exists"
        PASS=$((PASS + 1))
      else
        echo "  FAIL: deploy/flux-system/$file does not exist (the script would abort mid-deploy)"
        FAIL=$((FAIL + 1))
      fi
    done
  done

  # The bootstrap pair is applied outside the loop and has to exist too.
  for file in namespaces.yaml fluxinstance.yaml; do
    if [ -f "$PROJECT_ROOT/deploy/flux-system/$file" ]; then
      echo "  PASS: deploy/flux-system/$file exists"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: deploy/flux-system/$file does not exist"
      FAIL=$((FAIL + 1))
    fi
  done
}

# ---------------------------------------------------------------------------
# Test 4: the release names match the HelmRelease objects they wait on
# A wait entry naming a release that no manifest creates never goes Ready, and
# the run burns the full timeout before saying so.
# ---------------------------------------------------------------------------
test_wait_entries_match_the_release_manifests() {
  echo "Test: each wait entry names the HelmRelease its manifest creates"

  local entry release_file rest expected_ns expected_name actual_ns actual_name
  for entry in "${EXPECTED_PAIRS[@]}"; do
    rest="${entry#*|}"
    release_file="${rest%%|*}"
    expected_ns="${entry##*|}"
    expected_name="${expected_ns##*/}"
    expected_ns="${expected_ns%/*}"

    actual_name="$(awk '/^kind: HelmRelease$/ { hr = 1 }
                        hr && /^  name: / { print $2; exit }' \
      "$PROJECT_ROOT/deploy/flux-system/$release_file")"
    actual_ns="$(awk '/^kind: HelmRelease$/ { hr = 1 }
                      hr && /^  namespace: / { print $2; exit }' \
      "$PROJECT_ROOT/deploy/flux-system/$release_file")"

    assert_eq "$release_file declares HelmRelease $expected_name" "$expected_name" "$actual_name"
    assert_eq "$release_file declares namespace $expected_ns" "$expected_ns" "$actual_ns"
  done
}

# ---------------------------------------------------------------------------
# Test 5: cert-manager is waited for on its own, before the rest
# ---------------------------------------------------------------------------
test_cert_manager_is_waited_for_first() {
  echo "Test: cert-manager is waited for before the remaining releases"

  local first_wait_line remaining_wait_line
  first_wait_line="$(grep -n 'wait_for_helmreleases "${HELMRELEASE_TIMEOUT}" cert-manager/cert-manager' \
    "$MGMT_SH" | head -1 | cut -d: -f1)"
  remaining_wait_line="$(grep -n 'wait_for_helmreleases "${HELMRELEASE_TIMEOUT}" "${remaining\[@\]}"' \
    "$MGMT_SH" | head -1 | cut -d: -f1)"

  assert_not_empty "cert-manager has a wait of its own" "$first_wait_line"
  assert_not_empty "the remaining releases have a wait of their own" "$remaining_wait_line"

  if [ "${remaining_wait_line:-0}" -gt "${first_wait_line:-0}" ]; then
    echo "  PASS: the cert-manager wait precedes the remaining-releases wait"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the cert-manager wait does not precede the remaining-releases wait (hack/ci-deploy-operator.sh needs its CA injection)"
    FAIL=$((FAIL + 1))
  fi
}

# ---------------------------------------------------------------------------
# Test 6: the openstack namespace exists after the run
# The placed CRs live in it on the management cluster. It comes with
# deploy/flux-system/namespaces.yaml, so both halves of that have to hold.
# ---------------------------------------------------------------------------
test_openstack_namespace_is_created() {
  echo "Test: the openstack namespace is created"

  assert_file_contains "the script applies deploy/flux-system/namespaces.yaml" \
    "$MGMT_SH" 'deploy/flux-system/namespaces.yaml'

  local declared
  declared="$(awk '/^kind: Namespace$/ { ns = 1; next }
                   ns && /^  name: / { print $2; ns = 0 }' \
    "$PROJECT_ROOT/deploy/flux-system/namespaces.yaml" | grep -cx openstack)"
  assert_eq "deploy/flux-system/namespaces.yaml declares the openstack namespace" \
    "1" "${declared// /}"
}

# ---------------------------------------------------------------------------
# Test 7: the script is syntactically valid and shellcheck-clean
# ---------------------------------------------------------------------------
test_script_sanity() {
  echo "Test: the script parses and passes shellcheck"

  if bash -n "$MGMT_SH" 2>/dev/null; then
    echo "  PASS: hack/deploy-mgmt-cluster.sh parses"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: hack/deploy-mgmt-cluster.sh has a syntax error"
    FAIL=$((FAIL + 1))
  fi

  if ! command -v shellcheck >/dev/null 2>&1; then
    echo "  SKIP: shellcheck not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local output status=0
  output="$(shellcheck --severity=warning "$MGMT_SH" 2>&1)" || status=$?
  if [ "$status" -eq 0 ]; then
    echo "  PASS: shellcheck reports no warnings"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: shellcheck reported findings"
    echo "$output" | head -20
    FAIL=$((FAIL + 1))
  fi
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_cluster_name_default
test_flux_operator_pin_matches_deploy_infra
test_applied_manifests_exist_in_order
test_wait_entries_match_the_release_manifests
test_cert_manager_is_waited_for_first
test_openstack_namespace_is_created
test_script_sanity

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
