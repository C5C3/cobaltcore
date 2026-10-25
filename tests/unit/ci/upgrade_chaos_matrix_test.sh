#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the upgrade entry of the e2e-chaos matrix in
# .github/workflows/ci.yaml (#1394): the entry and its two Cinder release
# upgrade suites, the three cinder images it pulls and loads onto the node,
# the infrastructure it asks setup-e2e-infra for, the cinder-operator deploy,
# its diagnostics, its wall and its place among the non-blocking legs.
#
# Every signal here fails silently when it is missing. The matrix enumerates
# test_dirs by hand, so a suite absent from the entry is lint-checked and never
# applied to a cluster. Without the 2026.2 image on the node the upgrade's
# phase Jobs pull it from GHCR or sit in ImagePullBackOff, and without the
# cinder-operator no Cinder of the leg is reconciled. Without WITH_NFS no
# backend mounts, and without WITH_MESSAGING cinder-upgrade-db-partition has
# no broker to take its vhost on.
#
# Usage: bash tests/unit/ci/upgrade_chaos_matrix_test.sh

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

# The suites of #1394 that fault a Cinder release upgrade.
UPGRADE_SUITES=(cinder-upgrade-db-partition cinder-upgrade-broker-pod-kill)
# The images the leg pulls and loads beyond the keystone and OVN stacks.
UPGRADE_REFS=(cinder-operator:dev cinder:2026.1 cinder:2026.2)

# step_if <step-name>: the if: line of an e2e-chaos step, without its indent.
step_if() {
  job_step e2e-chaos "$1" | grep -E '^ *if:' | sed 's/^ *//'
}

test_the_entry_runs_the_upgrade_suites() {
  echo "Test: the upgrade entry runs on self-hosted runners and holds the two Cinder upgrade suites alone"
  local job entry suite leg other
  job="$(job_block e2e-chaos)"
  entry="$(e2e_chaos_matrix_entry upgrade)"
  assert_not_empty "the matrix carries an upgrade entry" "$entry"
  assert_contains "it runs on the self-hosted runners" "$job" "runs-on: self-hosted"
  assert_eq "it health-checks the chaos-mesh install first" "tests/e2e/infrastructure/chaos-mesh-health" \
    "$(grep -m1 -oE 'tests/e2e[^ ]*' <<<"$entry")"
  for suite in "${UPGRADE_SUITES[@]}"; do
    assert_contains "it runs ${suite}" "$entry" "tests/e2e-chaos/${suite}"
    for leg in pod network ovn nova controlplane; do
      other="$(e2e_chaos_matrix_entry "$leg")"
      assert_not_contains "the ${leg} entry does not run ${suite}" "$other" "tests/e2e-chaos/${suite}"
    done
  done
  assert_eq "it runs nothing but the health check and the two suites" \
    "$((1 + ${#UPGRADE_SUITES[@]}))" "$(grep -cE '^ +tests/' <<<"$entry")"
}

test_the_leg_loads_the_cinder_images() {
  echo "Test: the upgrade leg pulls the cinder-operator and both cinder images and loads them onto the node"
  local pull load ref
  pull="$(job_step e2e-chaos "Load E2E images")"
  load="$(job_step e2e-chaos "Load upgrade leg images into kind")"
  assert_not_empty "a load step exists" "$load"
  assert_eq "it runs on the upgrade leg alone" "if: matrix.suite == 'upgrade'" \
    "$(step_if "Load upgrade leg images into kind")"
  for ref in "${UPGRADE_REFS[@]}"; do
    assert_contains "Load E2E images pulls ${ref} on the upgrade leg alone" "$pull" \
      "\${{ matrix.suite == 'upgrade' && format('{0}/${ref}', env.IMAGE_PREFIX) || '' }}"
    assert_contains "and ${ref} reaches the kind node" "$load" \
      "kind load docker-image \${{ env.IMAGE_PREFIX }}/${ref} --name \${{ env.KIND_CLUSTER }}"
  done
  # The network leg runs no upgrade, so its cinder load stays on 2026.1.
  assert_not_contains "the network leg's cinder load names no 2026.2 image" \
    "$(job_step e2e-chaos "Load cinder images into kind")" "cinder:2026.2"
}

test_the_leg_asks_for_nfs_and_the_broker() {
  echo "Test: setup-e2e-infra installs the NFS export and the shared broker on the upgrade leg"
  local setup
  setup="$(job_step e2e-chaos "Setup E2E infrastructure")"
  assert_contains "WITH_NFS on the network and upgrade legs" "$setup" \
    "WITH_NFS: \${{ (matrix.suite == 'network' || matrix.suite == 'upgrade') && 'true' || '' }}"
  assert_contains "WITH_MESSAGING on the network, nova and upgrade legs" "$setup" \
    "WITH_MESSAGING: \${{ (matrix.suite == 'network' || matrix.suite == 'nova' || matrix.suite == 'upgrade') && 'true' || '' }}"
  assert_contains "Chaos Mesh stays on for every leg" "$setup" 'WITH_CHAOS_MESH: "true"'
  assert_not_contains "the leg asks for no ControlPlane path" "$setup" "matrix.suite == 'upgrade' && 'external'"
}

test_the_leg_deploys_the_cinder_operator() {
  echo "Test: the upgrade leg deploys the cinder-operator through a step of its own"
  local deploy job ovn_at deploy_at run_at
  deploy="$(job_step e2e-chaos "Deploy cinder operator (upgrade leg)")"
  assert_not_empty "a deploy step exists" "$deploy"
  # The whole condition, not a substring of it: an extra `|| always()` would
  # install the operator on every leg.
  assert_eq "it runs on the upgrade leg alone" "if: matrix.suite == 'upgrade'" \
    "$(step_if "Deploy cinder operator (upgrade leg)")"
  assert_contains "it goes through the shared deploy script" "$deploy" "run: hack/ci-deploy-operator.sh"
  assert_contains "it deploys the cinder operator" "$deploy" "OPERATOR: cinder"
  assert_contains "from the run's image" "$deploy" "IMAGE_REPO: \${{ env.IMAGE_PREFIX }}/cinder-operator"
  assert_contains "into cinder-system" "$deploy" "NAMESPACE: cinder-system"
  # The network leg's step keeps its own condition.
  assert_eq "the network leg's cinder deploy keeps its condition" "if: matrix.suite == 'network'" \
    "$(step_if "Deploy cinder operator")"

  job="$(job_block e2e-chaos)"
  ovn_at="$(grep -n -- '- name: Deploy ovn operator$' <<<"$job" | cut -d: -f1)"
  deploy_at="$(grep -n -- '- name: Deploy cinder operator (upgrade leg)$' <<<"$job" | cut -d: -f1)"
  run_at="$(grep -n -- '- name: Run chaos E2E tests$' <<<"$job" | cut -d: -f1)"
  assert_gte "it deploys after the ovn-operator" "$deploy_at" "$ovn_at"
  assert_gte "and before the suites run" "$run_at" "$deploy_at"
}

test_the_leg_dumps_the_cinder_operator() {
  echo "Test: the upgrade leg dumps the cinder-operator"
  local dump
  dump="$(job_step e2e-chaos "Dump diagnostic info (upgrade leg)")"
  assert_not_empty "a diagnostics step exists" "$dump"
  assert_eq "it runs on every outcome of the upgrade leg" "if: always() && matrix.suite == 'upgrade'" \
    "$(step_if "Dump diagnostic info (upgrade leg)")"
  assert_contains "it dumps the cinder-operator sections" "$dump" \
    "OPERATOR=cinder OPERATOR_ONLY=1 hack/ci-dump-diagnostics.sh"
}

test_the_leg_has_its_wall_and_does_not_gate() {
  echo "Test: the upgrade leg has an 80-minute wall and is named non-blocking"
  local job
  job="$(job_block e2e-chaos)"
  assert_contains "its arm of the wall reads 80 minutes" \
    "$(grep -E '^ +timeout-minutes:' <<<"$job")" "matrix.suite == 'upgrade' && 80"
  assert_contains "it is named in the non-blocking allowlist" \
    "$(grep -E '^ +continue-on-error:' <<<"$job")" "matrix.suite == 'upgrade'"
}

test_the_entry_runs_the_upgrade_suites
test_the_leg_loads_the_cinder_images
test_the_leg_asks_for_nfs_and_the_broker
test_the_leg_deploys_the_cinder_operator
test_the_leg_dumps_the_cinder_operator
test_the_leg_has_its_wall_and_does_not_gate

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
