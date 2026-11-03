#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the multinode entry of the e2e-chaos matrix in
# .github/workflows/ci.yaml (#1392): the entry and its suites in their order,
# the topology label that keeps those suites out of `make e2e-chaos`, the
# two-worker kind config, the step that prepares the leg and the one that
# restores its workers, the nova stack it shares with the nova entry, and its
# wall.
#
# Every signal here fails silently when it is missing. A suite absent from the
# entry is lint-checked and never applied to a cluster. On the single-node
# config every suite fails its own two-worker preflight. Without the prepare
# step the singletons land on a worker a suite kills, and without the restore
# step a worker a killed chainsaw left paused hides the diagnostics and the
# coverage counters of its node. Without the nova images, operators and
# opt-ins the compute and API suites have nothing to fault.
#
# Usage: bash tests/unit/ci/multinode_chaos_matrix_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
CHAOS_DIR="$PROJECT_ROOT/tests/e2e-chaos"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

# The suites of the multinode entry, in leg order: the cheap ones first.
MULTINODE_SUITES=(ovn-controller-pod-kill ovn-northd-pod-kill ovn-chassis-loss
  api-priority-preemption api-worker-loss novacompute-node-loss)
# The images the leg pulls beyond the keystone and OVN stacks: the nova
# entry's seven.
NOVA_REFS=(placement-operator:dev placement:2026.1 neutron-operator:dev neutron:2026.1
  nova-operator:dev nova:2026.1 nova-compute:2026.1)

# step_if <step-name>: the if: line of an e2e-chaos step, without its indent.
step_if() {
  job_step e2e-chaos "$1" | grep -E '^ *if:' | sed 's/^ *//'
}

# step_at <step-name>: the line of the step in the e2e-chaos job.
step_at() {
  job_block e2e-chaos | grep -nxF -- "      - name: $1" | cut -d: -f1
}

test_the_entry_runs_the_multinode_suites() {
  echo "Test: the multinode entry runs the health check, then its suites in leg order"
  local entry want got suite leg other
  entry="$(e2e_chaos_matrix_entry multinode)"
  assert_not_empty "the matrix carries a multinode entry" "$entry"
  want="$(printf '%s\n' tests/e2e/infrastructure/chaos-mesh-health \
    ${MULTINODE_SUITES[@]+"${MULTINODE_SUITES[@]/#/tests/e2e-chaos/}"})"
  # The entry is the last of the matrix, so its block runs on to the end of
  # the job; its suites are the lines that hold nothing but a path.
  got="$(grep -E '^ +tests/[^ ]+$' <<<"$entry" | sed 's/^ *//')"
  assert_eq "it runs exactly the health check and the suites, in order" "$want" "$got"
  for suite in ${MULTINODE_SUITES[@]+"${MULTINODE_SUITES[@]}"}; do
    for leg in pod network ovn nova controlplane upgrade; do
      other="$(e2e_chaos_matrix_entry "$leg")"
      assert_not_contains "the ${leg} entry does not run ${suite}" "$other" "tests/e2e-chaos/${suite}"
    done
  done
}

test_only_the_multinode_suites_carry_the_topology_label() {
  echo "Test: the multinode suites carry chaos.c5c3.io/topology: multinode, and no other suite does"
  local file suite labelled="" want
  for file in "$CHAOS_DIR"/*/chainsaw-test.yaml; do
    suite="$(basename "$(dirname "$file")")"
    # The label sits under metadata.labels of the Test; make e2e-chaos
    # selects on it.
    if awk '/^metadata:$/ { m = 1; next } m && /^[^ ]/ { m = 0 }
            m && /^  labels:$/ { l = 1; next } l && /^  [^ ]/ { l = 0 }
            m && l && /^    chaos\.c5c3\.io\/topology: multinode$/ { found = 1 }
            END { exit !found }' "$file"; then
      labelled="${labelled} ${suite}"
    fi
  done
  want="$(printf '%s\n' ${MULTINODE_SUITES[@]+"${MULTINODE_SUITES[@]}"} | sed '/^$/d' | LC_ALL=C sort | xargs)"
  assert_eq "exactly the multinode suites carry the label" "$want" "$(xargs <<<"$labelled")"
}

test_the_leg_creates_the_two_worker_cluster() {
  echo "Test: the multinode leg creates its cluster from the two-worker config and prepares it"
  local create prepare
  create="$(job_step e2e-chaos "Create kind cluster")"
  assert_contains "the config follows the entry" "$create" \
    "config: \${{ matrix.suite == 'multinode' && 'hack/kind-config-multinode.yaml' || 'hack/kind-config.yaml' }}"

  prepare="$(job_step e2e-chaos "Prepare the multinode leg")"
  assert_not_empty "a prepare step exists" "$prepare"
  assert_eq "it runs on the multinode leg alone" "if: matrix.suite == 'multinode'" \
    "$(step_if "Prepare the multinode leg")"
  assert_contains "it untaints the control plane and cordons the workers" "$prepare" \
    "run: tests/e2e-chaos/lib/nodes.sh prepare"
  assert_gte "it follows the cluster creation" "$(step_at "Prepare the multinode leg")" \
    "$(step_at "Create kind cluster")"
  assert_gte "and precedes the infrastructure, which would schedule onto the workers" \
    "$(step_at "Setup E2E infrastructure")" "$(step_at "Prepare the multinode leg")"
}

test_the_leg_restores_its_workers() {
  echo "Test: the multinode leg restores its workers after the suites, before the diagnostics"
  local restore run_at restore_at
  restore="$(job_step e2e-chaos "Restore the workers")"
  assert_not_empty "a restore step exists" "$restore"
  # The whole condition: without always() a red suite skips it, and without
  # the leg gate it runs nodes.sh on single-node clusters.
  assert_eq "it runs on every outcome of the multinode leg" "if: always() && matrix.suite == 'multinode'" \
    "$(step_if "Restore the workers")"
  assert_contains "it never fails the job" "$restore" "continue-on-error: true"
  assert_contains "it restores every worker" "$restore" "run: tests/e2e-chaos/lib/nodes.sh restore-all"
  assert_contains "a cancelled run waits less, to leave the cluster delete its grace window" "$restore" \
    "CHAOS_NODES_RESTORE_SECONDS: \${{ job.status == 'cancelled' && '60' || '300' }}"
  run_at="$(step_at "Run chaos E2E tests")"
  restore_at="$(step_at "Restore the workers")"
  assert_gte "it follows the suites" "$restore_at" "$run_at"
  assert_gte "and precedes the diagnostics" "$(step_at "Dump diagnostic info")" "$restore_at"
  assert_gte "and the coverage counters" "$(step_at "Collect operator coverage")" "$restore_at"
}

test_the_leg_brings_up_the_nova_stack() {
  echo "Test: the multinode leg pulls, loads and deploys the nova entry's stack"
  local pull load ref name setup
  pull="$(job_step e2e-chaos "Load E2E images")"
  load="$(job_step e2e-chaos "Load nova leg images into kind")"
  for ref in "${NOVA_REFS[@]}"; do
    assert_contains "Load E2E images pulls ${ref} on both legs" "$pull" \
      "\${{ (matrix.suite == 'nova' || matrix.suite == 'multinode') && format('{0}/${ref}', env.IMAGE_PREFIX) || '' }}"
    assert_contains "and ${ref} reaches the kind nodes" "$load" \
      "kind load docker-image \${{ env.IMAGE_PREFIX }}/${ref} --name \${{ env.KIND_CLUSTER }}"
  done
  for name in "Load nova leg images into kind" "Deploy placement operator (nova leg)" \
    "Deploy neutron operator (nova leg)" "Deploy nova operator"; do
    assert_eq "${name}: runs on the nova and multinode legs" "if: matrix.suite == 'nova' || matrix.suite == 'multinode'" \
      "$(step_if "$name")"
  done
  assert_eq "the nova dump covers the leg" "if: always() && (matrix.suite == 'nova' || matrix.suite == 'multinode')" \
    "$(step_if "Dump diagnostic info (nova leg)")"
  # The keystone stack and the ovn-operator reach the leg through the
  # `!= 'ovn'` and `!= 'pod'` gates.
  assert_eq "the keystone stack loads on every leg but ovn" "if: matrix.suite != 'ovn'" \
    "$(step_if "Load images into kind")"
  assert_eq "the ovn-operator deploys on every leg but pod" "if: matrix.suite != 'pod'" \
    "$(step_if "Deploy ovn operator")"

  setup="$(job_step e2e-chaos "Setup E2E infrastructure")"
  assert_contains "the chassis of the leg get the OVN kernel modules" "$setup" \
    "WITH_OVN_KERNEL_MODULES: \${{ (matrix.suite == 'ovn' || matrix.suite == 'nova' || matrix.suite == 'multinode') && 'true' || '' }}"
  assert_contains "its Nova gets the shared broker" "$setup" \
    "WITH_MESSAGING: \${{ (matrix.suite == 'network' || matrix.suite == 'nova' || matrix.suite == 'upgrade' || matrix.suite == 'multinode') && 'true' || '' }}"
  assert_contains "Chaos Mesh stays on for every leg" "$setup" 'WITH_CHAOS_MESH: "true"'
}

test_the_leg_has_its_wall() {
  echo "Test: the multinode leg has a 160-minute wall"
  assert_contains "its arm of the wall reads 160 minutes" \
    "$(job_block e2e-chaos | grep -E '^    timeout-minutes: ')" "matrix.suite == 'multinode' && 160"
  # Gating is job-wide; e2e_chaos_job_test.sh pins it (#1388).
}

test_the_entry_runs_the_multinode_suites
test_only_the_multinode_suites_carry_the_topology_label
test_the_leg_creates_the_two_worker_cluster
test_the_leg_restores_its_workers
test_the_leg_brings_up_the_nova_stack
test_the_leg_has_its_wall

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
