#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the controlplane entry of the e2e-chaos matrix in
# .github/workflows/ci.yaml (#1390): the entry and its suites, the
# c5c3-operator image it loads, the infrastructure it asks setup-e2e-infra for,
# the K-ORC and c5c3-operator deploys, its diagnostics, its wall and its place
# among the non-blocking legs.
#
# Every signal here fails silently when it is missing. The matrix enumerates
# test_dirs by hand, so a suite absent from the entry is lint-checked and never
# applied to a cluster. Without the image or the deploy the c5c3-operator never
# runs, and every ControlPlane of the leg waits for a reconciler until its
# bring-up times out. Without WITH_CONTROLPLANE the infrastructure overlay
# still carries the standalone MariaDB and Memcached of `openstack`. Without
# K-ORC no plane reaches KORCReady.
#
# Usage: bash tests/unit/ci/controlplane_chaos_matrix_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

# The suites of #1390 that bring up a ControlPlane of their own.
CONTROLPLANE_SUITES=(rabbitmq-operator-cascading-failure
  korc-provisioning-outage controlplane-keystone-degraded
  korc-network-partition c5c3-operator-pod-kill)
# The suites of #1390 that need only the standalone stack, on the pod leg.
POD_SUITES=(cert-manager-outage eso-down eso-pod-kill openbao-sealed
  openbao-tenant-policy-narrowed envoy-gateway-outage envoy-proxy-pod-kill
  mariadb-operator-cascading-failure)

# step_if <step-name>: the if: line of an e2e-chaos step, without its indent.
step_if() {
  job_step e2e-chaos "$1" | grep -E '^ *if:' | sed 's/^ *//'
}

test_the_entry_runs_the_controlplane_suites() {
  echo "Test: the controlplane entry runs on self-hosted runners and holds the ControlPlane suites"
  local job entry pod suite
  job="$(job_block e2e-chaos)"
  entry="$(e2e_chaos_matrix_entry controlplane)"
  pod="$(e2e_chaos_matrix_entry pod)"
  assert_not_empty "the matrix carries a controlplane entry" "$entry"
  assert_contains "it runs on the self-hosted runners" "$job" "runs-on: self-hosted"
  assert_eq "it health-checks the chaos-mesh install first" "tests/e2e/infrastructure/chaos-mesh-health" \
    "$(grep -m1 -oE 'tests/e2e[^ ]*' <<<"$entry")"
  for suite in ${CONTROLPLANE_SUITES[@]+"${CONTROLPLANE_SUITES[@]}"}; do
    assert_contains "it runs ${suite}" "$entry" "tests/e2e-chaos/${suite}"
    assert_not_contains "the pod entry does not run ${suite}" "$pod" "tests/e2e-chaos/${suite}"
  done
  for suite in "${POD_SUITES[@]}"; do
    assert_contains "the pod entry runs ${suite}" "$pod" "tests/e2e-chaos/${suite}"
    assert_not_contains "it does not run the pod suite ${suite}" "$entry" "tests/e2e-chaos/${suite}"
  done
  assert_eq "it runs nothing but the health check and the ControlPlane suites" \
    "$((1 + ${#CONTROLPLANE_SUITES[@]}))" "$(grep -cE '^ +tests/' <<<"$entry")"
}

test_the_leg_loads_the_c5c3_operator_image() {
  echo "Test: the controlplane leg pulls the c5c3-operator image and loads it onto the node"
  assert_contains "Load E2E images pulls it on the controlplane leg alone" \
    "$(job_step e2e-chaos "Load E2E images")" \
    "\${{ matrix.suite == 'controlplane' && format('{0}/c5c3-operator:dev', env.IMAGE_PREFIX) || '' }}"
  local load
  load="$(job_step e2e-chaos "Load controlplane images into kind")"
  assert_not_empty "a load step exists" "$load"
  assert_eq "it runs on the controlplane leg alone" "if: matrix.suite == 'controlplane'" \
    "$(step_if "Load controlplane images into kind")"
  assert_contains "it loads the c5c3-operator image" "$load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/c5c3-operator:dev --name \${{ env.KIND_CLUSTER }}"
}

test_the_leg_asks_for_the_controlplane_path() {
  echo "Test: setup-e2e-infra prepares the ControlPlane path on the controlplane leg"
  local setup
  setup="$(job_step e2e-chaos "Setup E2E infrastructure")"
  assert_contains "WITH_CONTROLPLANE on the controlplane leg alone" "$setup" \
    "WITH_CONTROLPLANE: \${{ matrix.suite == 'controlplane' && 'true' || '' }}"
  assert_contains "with the operators deployed out of Flux" "$setup" \
    "CONTROLPLANE_OPERATORS: \${{ matrix.suite == 'controlplane' && 'external' || '' }}"
  assert_contains "Chaos Mesh stays on for every leg" "$setup" 'WITH_CHAOS_MESH: "true"'
}

test_the_leg_deploys_korc_and_the_c5c3_operator() {
  echo "Test: the controlplane leg deploys K-ORC with the Placement CRD, then the c5c3-operator"
  local korc c5c3
  korc="$(job_step e2e-chaos "Deploy K-ORC (controlplane leg)")"
  c5c3="$(job_step e2e-chaos "Deploy c5c3 operator")"
  assert_not_empty "a K-ORC step exists" "$korc"
  assert_eq "the K-ORC step runs on the controlplane leg alone" "if: matrix.suite == 'controlplane'" \
    "$(step_if "Deploy K-ORC (controlplane leg)")"
  assert_contains "it authenticates the K-ORC clone" "$korc" "GITHUB_TOKEN: \${{ github.token }}"
  assert_contains "it applies the Placement CRD the c5c3-operator's cache needs" "$korc" \
    "kubectl apply -f operators/placement/helm/placement-operator/crds/"
  assert_contains "it runs the pinned K-ORC installer" "$korc" "hack/ci-deploy-korc.sh"

  assert_not_empty "a c5c3 deploy step exists" "$c5c3"
  assert_eq "the c5c3 deploy runs on the controlplane leg alone" "if: matrix.suite == 'controlplane'" \
    "$(step_if "Deploy c5c3 operator")"
  assert_contains "it goes through the shared deploy script" "$c5c3" "run: hack/ci-deploy-operator.sh"
  assert_contains "it deploys the c5c3 operator" "$c5c3" "OPERATOR: c5c3"
  assert_contains "from the run's image" "$c5c3" "IMAGE_REPO: \${{ env.IMAGE_PREFIX }}/c5c3-operator"
  assert_contains "into c5c3-system, where the suites select its pods" "$c5c3" "NAMESPACE: c5c3-system"
  assert_not_contains "at the chart default of two replicas" "$c5c3" "OPERATOR_REPLICAS"

  # The c5c3-operator projects Keystone children, so it starts after the
  # keystone-operator and after K-ORC, and both precede the suites.
  local job keystone_at korc_at c5c3_at run_at
  job="$(job_block e2e-chaos)"
  keystone_at="$(grep -n -- '- name: Deploy operator$' <<<"$job" | cut -d: -f1)"
  korc_at="$(grep -n -- '- name: Deploy K-ORC (controlplane leg)$' <<<"$job" | cut -d: -f1)"
  c5c3_at="$(grep -n -- '- name: Deploy c5c3 operator$' <<<"$job" | cut -d: -f1)"
  run_at="$(grep -n -- '- name: Run chaos E2E tests$' <<<"$job" | cut -d: -f1)"
  assert_gte "K-ORC deploys after the keystone-operator" "$korc_at" "$keystone_at"
  assert_gte "the c5c3-operator deploys after K-ORC" "$c5c3_at" "$korc_at"
  assert_gte "both precede the suites" "$run_at" "$c5c3_at"
}

test_the_leg_dumps_its_operators() {
  echo "Test: the controlplane leg dumps the c5c3-operator and the K-ORC log"
  local dump
  dump="$(job_step e2e-chaos "Dump diagnostic info (controlplane leg)")"
  assert_not_empty "a diagnostics step exists" "$dump"
  assert_eq "it runs on every outcome of the controlplane leg" "if: always() && matrix.suite == 'controlplane'" \
    "$(step_if "Dump diagnostic info (controlplane leg)")"
  assert_contains "it dumps the c5c3-operator sections" "$dump" \
    "OPERATOR=c5c3 OPERATOR_ONLY=1 hack/ci-dump-diagnostics.sh"
  assert_contains "and 500 lines of the K-ORC log" "$dump" \
    "kubectl logs -n orc-system deployment/orc-controller-manager --tail=500 || true"
}

test_the_leg_has_its_wall_and_does_not_gate() {
  echo "Test: the controlplane leg has a 120-minute wall and is named non-blocking"
  local job
  job="$(job_block e2e-chaos)"
  assert_contains "its arm of the wall reads 120 minutes" \
    "$(grep -E '^ +timeout-minutes:' <<<"$job")" "matrix.suite == 'controlplane' && 120"
  assert_contains "it is named in the non-blocking allowlist" \
    "$(grep -E '^ +continue-on-error:' <<<"$job")" "matrix.suite == 'controlplane'"
}

test_the_entry_runs_the_controlplane_suites
test_the_leg_loads_the_c5c3_operator_image
test_the_leg_asks_for_the_controlplane_path
test_the_leg_deploys_korc_and_the_c5c3_operator
test_the_leg_dumps_its_operators
test_the_leg_has_its_wall_and_does_not_gate

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
