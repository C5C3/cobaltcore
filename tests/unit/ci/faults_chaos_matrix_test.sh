#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the CI placement of the HTTPChaos, TimeChaos, IOChaos and StressChaos
# suites of #1391 in .github/workflows/ci.yaml:
#   1. The e2e-chaos matrix has a faults entry that health-checks Chaos Mesh
#      first and runs the six suites that need only the standalone stack,
#      none of which another entry lists.
#   2. The faults entry gets metrics-server and the VPA, Placement's images
#      and operator, its diagnostics step, a 130-minute wall and a place in
#      the non-blocking allowlist.
#
# Usage: bash tests/unit/ci/faults_chaos_matrix_test.sh

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

# The six suites of the faults entry, in the order the entry lists them.
FAULTS_SUITES=(
  keystone-validation-http-503
  keystone-api-clock-skew
  keystone-operator-clock-skew
  mariadb-io-fault
  glance-staging-io-fault
  keystone-api-stress-autoscaling
)

# step_if <job> <step>: the if: line of a step, without its indent.
step_if() {
  job_step "$1" "$2" | grep -E '^ *if:' | sed 's/^ *//'
}

# --- Test 1 ---------------------------------------------------------------
test_faults_entry_runs_its_six_suites() {
  echo "Test: the faults entry health-checks Chaos Mesh, then runs its six suites and only those"
  local entry tests
  entry="$(e2e_chaos_matrix_entry faults)"
  assert_not_empty "the matrix carries a faults entry" "$entry"
  # The last entry of the matrix runs on to the end of the job body, so the
  # test_dirs lines are read alone.
  tests="$(awk '/^            test_dirs:/ { on = 1; next }
    on && /^              tests\// { print $1; next }
    on { exit }' <<<"$entry")"
  assert_eq "the entry lists the health check and its suites, nothing else" \
    "$(printf '%s\n' tests/e2e/infrastructure/chaos-mesh-health ${FAULTS_SUITES[@]+"${FAULTS_SUITES[@]/#/tests/e2e-chaos/}"})" \
    "$tests"

  local leg suite other
  for leg in pod network ovn nova controlplane; do
    other="$(e2e_chaos_matrix_entry "$leg")"
    for suite in ${FAULTS_SUITES[@]+"${FAULTS_SUITES[@]}"}; do
      assert_not_contains "the ${leg} entry does not run ${suite}" "$other" "tests/e2e-chaos/${suite}"
    done
  done
  assert_contains "the faults entry comes after the controlplane entry" \
    "$(job_block e2e-chaos | grep -E '^          - suite:' | tail -2 | xargs)" "- suite: controlplane - suite: faults"
}

# --- Test 2 ---------------------------------------------------------------
test_faults_entry_gets_its_stack_wall_and_gate() {
  echo "Test: the faults entry gets metrics-server, the VPA, Placement, its diagnostics, its wall and no gate"
  local job setup load diag
  job="$(job_block e2e-chaos)"
  setup="$(job_step e2e-chaos "Setup E2E infrastructure")"
  assert_contains "metrics-server runs on the faults entry alone" "$setup" \
    "WITH_METRICS_SERVER: \${{ matrix.suite == 'faults' && 'true' || '' }}"
  assert_contains "the VPA runs on the faults entry alone" "$setup" \
    "WITH_VPA: \${{ matrix.suite == 'faults' && 'true' || '' }}"

  local three="(matrix.suite == 'pod' || matrix.suite == 'network' || matrix.suite == 'faults')"
  load="$(job_step e2e-chaos "Load E2E images")"
  assert_contains "the placement-operator image is pulled on the faults entry" "$load" \
    "\${{ ${three} && format('{0}/placement-operator:dev', env.IMAGE_PREFIX) || '' }}"
  assert_contains "the placement service image is pulled on the faults entry" "$load" \
    "\${{ ${three} && format('{0}/placement:2026.1', env.IMAGE_PREFIX) || '' }}"
  local name
  for name in "Load placement images into kind" "Deploy placement operator"; do
    assert_eq "${name}: runs on the pod, network and faults entries" \
      "if: matrix.suite == 'pod' || matrix.suite == 'network' || matrix.suite == 'faults'" \
      "$(step_if e2e-chaos "$name")"
  done

  diag="$(job_step e2e-chaos "Dump diagnostic info (faults leg)")"
  assert_not_empty "a faults diagnostics step exists" "$diag"
  assert_eq "it runs on every outcome of the faults entry" "if: always() && matrix.suite == 'faults'" \
    "$(step_if e2e-chaos "Dump diagnostic info (faults leg)")"
  assert_contains "it dumps the placement and glance operators" "$diag" "for op in placement glance; do"
  assert_contains "one operator per pass" "$diag" 'OPERATOR="${op}" OPERATOR_ONLY=1 hack/ci-dump-diagnostics.sh'

  assert_contains "the faults wall is 130 minutes" \
    "$(grep -E '^ +timeout-minutes:' <<<"$job")" "matrix.suite == 'faults' && 130"
  assert_contains "the faults entry is named in the non-blocking allowlist" \
    "$(grep -E '^ +continue-on-error:' <<<"$job")" "|| matrix.suite == 'faults' }}"
}

# --- Test 3 ---------------------------------------------------------------
# --- Run ---
test_faults_entry_runs_its_six_suites
test_faults_entry_gets_its_stack_wall_and_gate

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
