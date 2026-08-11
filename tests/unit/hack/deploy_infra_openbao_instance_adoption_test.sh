#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the unseal-key adoption choreography in hack/deploy-infra.sh keeps its
# ordering contract. Every step of it only works in one order, and getting it
# wrong is silent: the openbao-operator blind-creates the fixed-name Secret
# openbao-instance-unseal-key (Immutable, random key) on its first reconcile and
# adopts a pre-existing one only against an ownership proof, so an instance that
# reconciles too early comes up on a key nobody custodies — and every e2e
# assertion still passes.
#
# The assertions are static line-ordering checks over the script text, in the
# style of the sibling deploy_infra_gateway_crds_test.sh wiring checks:
#
#   1. openbaoclusters.openbao.org is in the wait_for_crds argument list, so the
#      overlay apply cannot race the operator's CRD registration.
#   2. The ExternalSecret is waited for BEFORE the ownership proof is attached —
#      the patch targets a Secret that ESO has not necessarily materialized yet.
#   3. The proof lands BEFORE the un-pause — the operator adopts on its first
#      reconcile, and an unproven Secret is not adopted.
#   4. The un-pause lands BEFORE the blocking GarageCluster wait, and the
#      instance's own readiness wait is collected AFTER it. The instance depends
#      on nothing Garage provides, so serializing it behind that wait is pure
#      wall-clock on every deploy.
#   5. The proof is a controller ownerReference and NOT the operator's
#      openbao.org/owner-uid annotation. The operator accepts either, but the
#      openbao-lock-managed-resource-mutations ValidatingAdmissionPolicy the
#      chart ships reserves that annotation for the operator's own
#      ServiceAccounts and denies every other writer, clusterwide and at
#      failurePolicy: Fail. Writing it here aborts the whole deploy.
#
# Usage: bash tests/unit/hack/deploy_infra_openbao_instance_adoption_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# line_of <pattern>
# Prints the 1-based line number of the FIRST line matching <pattern>, or the
# empty string when there is no match.
line_of() {
  grep -n -- "$1" "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1
}

# assert_before <description> <earlier-line> <later-line>
assert_before() {
  local description="$1" earlier="$2" later="$3"
  if [[ -z "$earlier" || -z "$later" ]]; then
    echo "  FAIL: $description (a required call site is missing: earlier='$earlier' later='$later')"
    FAIL=$((FAIL + 1))
    return
  fi
  if [[ "$earlier" -lt "$later" ]]; then
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $description (line $earlier is not before line $later)"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 1: the OpenBaoCluster CRD is awaited before the overlay apply ---
test_openbaocluster_crd_is_awaited() {
  echo "Test: wait_for_crds covers openbaoclusters.openbao.org"

  assert_file_contains "the CRD the overlay's OpenBaoCluster needs is awaited" \
    "$DEPLOY_INFRA_SH" "openbaoclusters.openbao.org"

  # The name must sit inside the wait_for_crds argument list, not merely
  # somewhere in the file: the list is what blocks the apply.
  local in_arg_list
  in_arg_list="$(sed -n '/wait_for_crds "${POD_TIMEOUT}"/,/^$/p' "$DEPLOY_INFRA_SH" \
    | grep -c 'openbaoclusters.openbao.org' || true)"
  assert_eq "openbaoclusters.openbao.org is an argument of wait_for_crds" \
    "1" "${in_arg_list// /}"
}

# --- Test 2: sync → ownership proof → un-pause, in that order ---
test_adoption_choreography_order() {
  echo "Test: the unseal-key ExternalSecret is synced, then proven, then the CR is un-paused"

  local wait_es proof unpause
  wait_es="$(line_of '^    openbao-instance-unseal-key$')"
  proof="$(line_of 'kubectl patch secret openbao-instance-unseal-key')"
  # Located by the command rather than its payload: the same patch also carries
  # spec.network.apiServerEndpointIPs, so the payload's exact shape is not a stable
  # anchor. That the patch still clears spec.paused is asserted by
  # tests/unit/deploy/openbao_instance_overlay_test.sh.
  unpause="$(line_of 'kubectl patch openbaocluster openbao-instance')"

  assert_before "the ExternalSecret is waited for before the ownership proof" \
    "$wait_es" "$proof"
  assert_before "the ownership proof lands before the un-pause" \
    "$proof" "$unpause"
}

# --- Test 3: the instance is started before the Garage wait, collected after ---
test_instance_start_is_not_serialized_behind_garage() {
  echo "Test: the un-pause precedes the GarageCluster wait and the Available wait follows it"

  local unpause garage_wait available_wait
  # Located by the command rather than its payload: the same patch also carries
  # spec.network.apiServerEndpointIPs, so the payload's exact shape is not a stable
  # anchor. That the patch still clears spec.paused is asserted by
  # tests/unit/deploy/openbao_instance_overlay_test.sh.
  unpause="$(line_of 'kubectl patch openbaocluster openbao-instance')"
  garage_wait="$(line_of 'kubectl wait garagecluster/garage')"
  available_wait="$(line_of 'kubectl wait openbaocluster/openbao-instance')"

  assert_before "the instance is un-paused before the blocking GarageCluster wait" \
    "$unpause" "$garage_wait"
  assert_before "its readiness is collected after that wait, not before it" \
    "$garage_wait" "$available_wait"
}

# --- Test 4: the proof is an ownerReference, never the reserved annotation ---
test_proof_is_an_owner_reference() {
  echo "Test: the ownership proof is a controller ownerReference, not the owner-uid annotation"

  # The openbao-lock-managed-resource-mutations ValidatingAdmissionPolicy the
  # operator chart ships denies this annotation to every principal outside the
  # operator's own ServiceAccounts, clusterwide and at failurePolicy: Fail.
  # Writing it does not degrade the deploy, it aborts it.
  local stamp
  stamp="$(line_of 'openbao.org/owner-uid=')"
  if [[ -z "$stamp" ]]; then
    echo "  PASS: the deploy does not write the operator-reserved openbao.org/owner-uid annotation"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: hack/deploy-infra.sh writes openbao.org/owner-uid at line $stamp, which the openbao-operator ValidatingAdmissionPolicy denies"
    FAIL=$((FAIL + 1))
  fi

  # An object carries at most one controller ownerReference, so this only works
  # while the ExternalSecret leaves that slot free (creationPolicy: Orphan).
  assert_file_contains "the Secret is patched with an ownerReference to the OpenBaoCluster" \
    "$DEPLOY_INFRA_SH" 'ownerReferences.*OpenBaoCluster'
  assert_file_contains "that ownerReference claims the controller slot" \
    "$DEPLOY_INFRA_SH" 'ownerReferences.*controller.*true'
}

# --- Run ---
test_openbaocluster_crd_is_awaited
test_adoption_choreography_order
test_instance_start_is_not_serialized_behind_garage
test_proof_is_an_owner_reference

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
