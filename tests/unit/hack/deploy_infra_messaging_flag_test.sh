#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh gates the kind message bus behind WITH_MESSAGING:
# the deploy/kind/messaging overlay apply and the AllReplicasReady wait on
# rabbitmqcluster/shared-rabbitmq. The default Quick Start must install
# neither, a typo like WITH_MESSAGING=yes must take the skip branch, and the
# two actions must run in that order after the infrastructure overlay.
#
# It also drives wait_for_rabbitmqcluster itself: the timeout path has to exit
# non-zero with the broker named in the error and the describe output dumped,
# and the success path has to return without asking for diagnostics.
#
# Implementation: bash + tests/lib/assertions.sh, matching the sibling
# tests/unit/hack/deploy_infra_nfs_flag_test.sh and
# tests/unit/hack/deploy_infra_gateway_wait_test.sh. The repo has zero .bats
# files and no bats binary on CI, so introducing one would add an undeclared
# dependency.
#
# Strategy: hybrid. Source the script in a subshell (the
# `BASH_SOURCE[0] == ${0}` guard at the bottom of deploy-infra.sh keeps main()
# from auto-running) to read the resolved flag default and to drive
# wait_for_rabbitmqcluster against shell functions that shadow kubectl, sleep
# and date; grep the script source for the strict gate and the gate-before-
# action order.
#
# Usage: bash tests/unit/hack/deploy_infra_messaging_flag_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"
SETUP_ACTION="$PROJECT_ROOT/.github/actions/setup-e2e-infra/action.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# resolve_with_messaging [env_var=value...]
# Sources deploy-infra.sh in a subshell with the supplied env overrides and
# echoes the resolved value of WITH_MESSAGING after the configuration block
# runs.
resolve_with_messaging() {
  (
    # Apply each env override in the subshell before sourcing.
    for assignment in "$@"; do
      export "${assignment?}"
    done
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    printf '%s' "${WITH_MESSAGING}"
  )
}

# run_wait SHIMS ARG...
# Sources deploy-infra.sh in a subshell, evaluates SHIMS (function definitions
# that shadow the commands the wait calls) and invokes
# wait_for_rabbitmqcluster with ARG.... Echoes combined stdout/stderr; returns
# the function's exit status, which is also the subshell's because the call is
# its last command.
run_wait() {
  local shims="$1"
  shift
  (
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    eval "$shims"
    wait_for_rabbitmqcluster "$@"
  ) 2>&1
}

# A `date` shim backed by a file, so the clock keeps advancing across the
# command substitutions the wait runs it in. Every call jumps 1000 seconds,
# which puts the second call past any deadline the first one computed.
STUB_CLOCK=""
stub_date_shim() {
  cat <<SHIM
date() {
  local now
  now=\$(( \$(cat "${STUB_CLOCK}") + 1000 ))
  printf '%s' "\$now" >"${STUB_CLOCK}"
  printf '%s' "\$now"
}
sleep() { :; }
SHIM
}

# assert_file_contains_literal DESCRIPTION FILE LITERAL
# Same contract as assert_file_contains, but matches with grep -F so the `${}`
# and `()` in the pinned lines need no escaping.
assert_file_contains_literal() {
  local description="$1" file="$2" literal="$3"
  if grep -qF -- "$literal" "$file"; then
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $description (literal not found in $file)"
    echo "    expected line: $literal"
    FAIL=$((FAIL + 1))
  fi
}

# ---------------------------------------------------------------------------
# Test 1: WITH_MESSAGING defaults to false
# The Quick Start needs no broker, and one RabbitmqCluster on a 4-vCPU kind
# node is CPU nobody asked for.
# ---------------------------------------------------------------------------
test_default_is_false() {
  echo "Test: WITH_MESSAGING defaults to false"

  local resolved
  resolved="$(unset WITH_MESSAGING; resolve_with_messaging)"
  assert_eq "WITH_MESSAGING defaults to false" "false" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 2: defensive non-true value
# A typo like WITH_MESSAGING=yes must not install the broker, because the gate
# site uses the strict `== "true"` comparison.
# ---------------------------------------------------------------------------
test_non_true_value_does_not_trigger_install() {
  echo "Test: WITH_MESSAGING=yes passes through but does not trigger install"

  local resolved
  resolved="$(resolve_with_messaging WITH_MESSAGING=yes)"
  assert_eq "WITH_MESSAGING=yes is preserved verbatim" "yes" "$resolved"

  # Every condition that reads the flag has to be the strict form, otherwise
  # `yes` would reach the apply through a `!= "false"` style test.
  local condition_count strict_count
  condition_count="$(grep -cE '\[\[ *"\$\{WITH_MESSAGING\}"' "$DEPLOY_INFRA_SH" || true)"
  strict_count="$(grep -cF '"${WITH_MESSAGING}" == "true"' "$DEPLOY_INFRA_SH" || true)"
  assert_eq "every WITH_MESSAGING condition uses the strict == \"true\" form" \
    "$condition_count" "$strict_count"
}

# ---------------------------------------------------------------------------
# Test 3: one gate, not several
# The broker is applied in one place. A second gate would mean a second apply
# path nothing here covers.
# ---------------------------------------------------------------------------
test_single_strict_gate() {
  echo "Test: deploy-infra.sh has exactly one strict WITH_MESSAGING gate"

  local gate_count
  gate_count="$(grep -cF '"${WITH_MESSAGING}" == "true"' "$DEPLOY_INFRA_SH" || true)"
  assert_eq "deploy-infra.sh has exactly 1 strict WITH_MESSAGING==true gate" \
    "1" "$gate_count"
}

# ---------------------------------------------------------------------------
# Test 4: the configuration banner reports the flag
# The banner is the one place an operator sees which opt-ins a run resolved to.
# ---------------------------------------------------------------------------
test_banner_includes_messaging_line() {
  echo "Test: the configuration banner reports WITH_MESSAGING"

  assert_file_contains "the banner has a message bus line" \
    "$DEPLOY_INFRA_SH" 'Message bus'

  assert_file_contains "the banner names the variable that turns it on" \
    "$DEPLOY_INFRA_SH" 'set WITH_MESSAGING=true'
}

# ---------------------------------------------------------------------------
# Test 5: apply, then wait, and both after the infrastructure overlay
# The broker needs the rabbitmqclusters.rabbitmq.com CRD and the `openstack`
# namespace, which Step 5 has settled by the time it applies its own overlay.
# Waiting before applying would time out on an object that does not exist.
# ---------------------------------------------------------------------------
test_apply_and_wait_follow_step_5() {
  echo "Test: the messaging apply precedes its wait, and both follow Step 5"

  local infra_line apply_line wait_line
  infra_line="$(grep -nF 'kubectl apply -k "${REPO_ROOT}/deploy/kind/infrastructure"' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  apply_line="$(grep -nF 'kubectl apply -k "${REPO_ROOT}/deploy/kind/messaging"' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  wait_line="$(grep -nF 'wait_for_rabbitmqcluster shared-rabbitmq openstack' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"

  assert_not_empty "the Step 5 infrastructure apply is found" "$infra_line"
  assert_not_empty "the deploy/kind/messaging apply is found" "$apply_line"
  assert_not_empty "the wait_for_rabbitmqcluster call is found" "$wait_line"

  assert_gte "the messaging apply follows the infrastructure apply" \
    "${apply_line:-0}" "$((${infra_line:-0} + 1))"
  assert_gte "the AllReplicasReady wait follows the messaging apply" \
    "${wait_line:-0}" "$((${apply_line:-0} + 1))"

  # The gate has to enclose the apply, not sit somewhere after it.
  local gate_line
  gate_line="$(grep -nF '"${WITH_MESSAGING}" == "true"' "$DEPLOY_INFRA_SH" | awk -F: -v target="${apply_line:-0}" '$1 < target { last = $1 } END { print last }')"
  assert_not_empty "the WITH_MESSAGING gate precedes the messaging apply" "$gate_line"
}

# ---------------------------------------------------------------------------
# Test 6: the composite action threads the flag
# Without the passthrough a CI job could set WITH_MESSAGING on the job and
# still get the false default inside the deploy step.
# ---------------------------------------------------------------------------
test_setup_action_threads_the_flag() {
  echo "Test: setup-e2e-infra threads WITH_MESSAGING into deploy-infra.sh"

  assert_file_contains_literal "WITH_MESSAGING reaches deploy-infra.sh" \
    "$SETUP_ACTION" \
    'WITH_MESSAGING: ${{ env.WITH_MESSAGING }}'
}

# ---------------------------------------------------------------------------
# Test 7: the wait fails the run when the broker never reports ready
# A green deploy over a broker with no ready replica hands every Cinder suite
# a transport_url that connects to nothing, so the timeout has to exit 1 and
# leave the describe output behind.
# ---------------------------------------------------------------------------
test_wait_times_out_with_diagnostics() {
  echo "Test: wait_for_rabbitmqcluster exits 1 with diagnostics on timeout"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  STUB_CLOCK="$tmp/clock"
  printf '%s' "0" >"$STUB_CLOCK"

  # kubectl prints nothing for `get` (no condition yet) and a sentinel for
  # `describe`; `get events` is silent like the real command on a namespace
  # with no events.
  local shims
  shims="$(stub_date_shim)
kubectl() {
  if [[ \"\$1\" == \"describe\" ]]; then
    echo \"DESCRIBE-STUB\"
  fi
  return 0
}"

  local output exit_code
  output="$(run_wait "$shims" shared-rabbitmq openstack 300)"
  exit_code=$?

  assert_eq "wait_for_rabbitmqcluster exits 1 on timeout" "1" "$exit_code"
  assert_contains "the timeout names the broker and the budget" "$output" \
    "ERROR: RabbitmqCluster openstack/shared-rabbitmq did not report AllReplicasReady within 300s"
  assert_contains "the describe output is dumped" "$output" "DESCRIBE-STUB"
}

# ---------------------------------------------------------------------------
# Test 8: the wait returns as soon as AllReplicasReady is True
# ---------------------------------------------------------------------------
test_wait_returns_on_ready() {
  echo "Test: wait_for_rabbitmqcluster returns 0 when AllReplicasReady=True"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  STUB_CLOCK="$tmp/clock"
  printf '%s' "0" >"$STUB_CLOCK"

  local shims
  shims="$(stub_date_shim)
kubectl() {
  if [[ \"\$1\" == \"get\" ]]; then
    printf '%s' \"True\"
  elif [[ \"\$1\" == \"describe\" ]]; then
    echo \"DESCRIBE-STUB\"
  fi
  return 0
}"

  local output exit_code
  output="$(run_wait "$shims" shared-rabbitmq openstack 300)"
  exit_code=$?

  assert_eq "wait_for_rabbitmqcluster exits 0 on a ready broker" "0" "$exit_code"
  assert_contains "the success is logged" "$output" \
    "RabbitmqCluster/shared-rabbitmq reports AllReplicasReady."
  assert_not_contains "no diagnostics are dumped on the happy path" \
    "$output" "DESCRIBE-STUB"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_default_is_false
test_non_true_value_does_not_trigger_install
test_single_strict_gate
test_banner_includes_messaging_line
test_apply_and_wait_follow_step_5
test_setup_action_threads_the_flag
test_wait_times_out_with_diagnostics
test_wait_returns_on_ready

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
