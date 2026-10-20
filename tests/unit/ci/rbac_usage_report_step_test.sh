#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the `Report RBAC usage` post-step of the two CI jobs that create the
# audited kind cluster, e2e-operator and e2e-controlplane, and the artifact it
# fills.
#
# The step diffs the audit log against the operator's ClusterRole and must
# never decide the job's result: a path the suites skip is no reason to fail
# correct code. So it runs under always() with continue-on-error, after every
# diagnostics dump (which still needs the cluster) and before the teardown. The
# artifact name carries the operator and, on a nova shard, the shard, because
# two artifacts of one run cannot share a name.
#
# Usage: bash tests/unit/ci/rbac_usage_report_step_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
REPORT_SH="$PROJECT_ROOT/hack/ci-rbac-usage-report.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

# step_names <job> — the names of the job's steps, in order.
step_names() {
  job_block "$1" | sed -n 's/^      - name: //p'
}

# step_index <job> <step-name> — the 1-based position of the step, empty when absent.
step_index() {
  step_names "$1" | grep -nxF -- "$2" | cut -d: -f1
}

# check_step <job> <operator-env> <artifact-name>
check_step() {
  local job="$1" operator="$2" artifact="$3"
  echo "Test: $job reports RBAC usage without gating and uploads it"

  local step
  step="$(job_step "$job" "Report RBAC usage")"
  assert_not_empty "$job has a Report RBAC usage step" "$step"
  assert_contains "it runs after a failed suite" "$step" "if: always()"
  assert_contains "it never fails the job" "$step" "continue-on-error: true"
  assert_contains "it runs the report script" "$step" "run: hack/ci-rbac-usage-report.sh"
  assert_contains "it names the operator" "$step" "OPERATOR: $operator"

  local upload
  upload="$(job_step "$job" "Upload RBAC usage report")"
  assert_not_empty "$job has an Upload RBAC usage report step" "$upload"
  assert_contains "the upload runs after a failed suite" "$upload" "if: always()"
  assert_contains "it uploads the report directory" "$upload" "path: _output/rbac-usage/"
  assert_contains "the artifact name carries the operator" "$upload" "name: $artifact"
  assert_contains "it is kept for two weeks" "$upload" "retention-days: 14"
  assert_eq "it uses the upload-artifact pin of the JUnit upload" \
    "$(job_step "$job" "Upload JUnit report" | grep 'uses: actions/upload-artifact@')" \
    "$(grep 'uses: actions/upload-artifact@' <<<"$upload")"

  local report_at upload_at delete_at last_dump
  report_at="$(step_index "$job" "Report RBAC usage")"
  upload_at="$(step_index "$job" "Upload RBAC usage report")"
  delete_at="$(step_index "$job" "Delete kind cluster")"
  last_dump="$(step_names "$job" | grep -n '^Dump .*diagnostic info' | tail -1 | cut -d: -f1)"
  assert_not_empty "$job dumps diagnostics" "$last_dump"
  assert_eq "the report follows the last diagnostics dump" "$((last_dump + 1))" "$report_at"
  assert_eq "the upload follows the report" "$((report_at + 1))" "$upload_at"
  if [ -n "$delete_at" ] && [ "$upload_at" -lt "$delete_at" ]; then
    assert_eq "both run before the cluster is deleted" "yes" "yes"
  else
    assert_eq "both run before the cluster is deleted" "yes" "no"
  fi
}

test_e2e_operator() {
  check_step e2e-operator "\${{ matrix.operator }}" \
    "rbac-usage-\${{ matrix.operator }}\${{ matrix.shard && format('-{0}', matrix.shard) || '' }}"
}

test_e2e_controlplane() {
  check_step e2e-controlplane "c5c3" "rbac-usage-e2e-controlplane"
}

test_only_the_audited_jobs_report() {
  echo "Test: only the two jobs on the audited cluster run the report"

  # The c5c3 entry of the e2e-operator matrix already uploads rbac-usage-c5c3.
  # upload-artifact keeps a second artifact of the same name beside it, and
  # gh run download --name then returns only one of them.
  assert_not_contains "e2e-controlplane does not reuse a matrix artifact name" \
    "$(job_step e2e-controlplane "Upload RBAC usage report")" "name: rbac-usage-c5c3"

  assert_eq "two steps run hack/ci-rbac-usage-report.sh" "2" \
    "$(grep -c 'run: hack/ci-rbac-usage-report.sh$' "$CI_YAML" | tr -d ' ')"
  if [ -x "$REPORT_SH" ]; then
    assert_eq "hack/ci-rbac-usage-report.sh is executable" "yes" "yes"
  else
    assert_eq "hack/ci-rbac-usage-report.sh is executable" "yes" "no"
  fi
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_e2e_operator
test_e2e_controlplane
test_only_the_audited_jobs_report

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
