#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh gates the host-side openvswitch/geneve module
# load behind WITH_OVN_KERNEL_MODULES, and that load_host_kernel_modules (the
# generic loader both the OVN and the chaos-mesh entry points delegate to)
# keeps its best-effort posture: it warns and returns 0 on every host
# condition, and returns non-zero only when a caller passes bad arguments.
#
# Implementation: bash + tests/lib/assertions.sh, matching the sibling
# tests/unit/hack/deploy_infra_chaos_flag_test.sh. The repo has zero .bats
# files and no bats binary on CI, so introducing one would add an undeclared
# dependency.
#
# Strategy: hybrid. Source the script in a subshell (the
# `BASH_SOURCE[0] == ${0}` guard at the bottom of deploy-infra.sh keeps main()
# from auto-running) to read the resolved flag default and to drive
# load_host_kernel_modules against shell functions that shadow uname, id, sudo,
# modprobe and apt-get; grep the script source for the single strict gate, the
# gate-before-call order, and the two delegation call lines. Shadowing needs no
# stub directory on PATH because bash resolves function names before it
# searches PATH.
#
# Usage: bash tests/unit/hack/deploy_infra_ovn_kernel_modules_flag_test.sh

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

# resolve_with_ovn_kernel_modules [env_var=value...]
# Sources deploy-infra.sh in a subshell with the supplied env overrides and
# echoes the resolved value of WITH_OVN_KERNEL_MODULES after the configuration
# block runs.
resolve_with_ovn_kernel_modules() {
  (
    # Apply each env override in the subshell before sourcing.
    for assignment in "$@"; do
      export "${assignment?}"
    done
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    printf '%s' "${WITH_OVN_KERNEL_MODULES}"
  )
}

# run_loader SHIMS ARG...
# Sources deploy-infra.sh in a subshell, evaluates SHIMS (a snippet of function
# definitions that shadow the host commands the loader calls) and invokes
# load_host_kernel_modules with ARG.... Echoes combined stdout/stderr; returns
# the loader's exit status, which is also the subshell's because the call is
# its last command.
run_loader() {
  local shims="$1"
  shift
  (
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    eval "$shims"
    load_host_kernel_modules "$@"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test 1: WITH_OVN_KERNEL_MODULES defaults to false
# The Quick Start must not modprobe anything on a developer's host.
# ---------------------------------------------------------------------------
test_default_is_false() {
  echo "Test: WITH_OVN_KERNEL_MODULES defaults to false"

  local resolved
  resolved="$(unset WITH_OVN_KERNEL_MODULES; resolve_with_ovn_kernel_modules)"
  assert_eq "WITH_OVN_KERNEL_MODULES defaults to false" "false" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 2: explicit WITH_OVN_KERNEL_MODULES=true
# ---------------------------------------------------------------------------
test_explicit_true() {
  echo "Test: WITH_OVN_KERNEL_MODULES=true is preserved"

  local resolved
  resolved="$(resolve_with_ovn_kernel_modules WITH_OVN_KERNEL_MODULES=true)"
  assert_eq "WITH_OVN_KERNEL_MODULES=true is preserved" "true" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 3: a non-true value does not load anything
# A typo like WITH_OVN_KERNEL_MODULES=yes takes the skip branch because the
# single gate site uses the strict `== "true"` comparison.
# ---------------------------------------------------------------------------
test_non_true_value_does_not_trigger_load() {
  echo "Test: WITH_OVN_KERNEL_MODULES=yes passes through but does not trigger the load"

  local resolved
  resolved="$(resolve_with_ovn_kernel_modules WITH_OVN_KERNEL_MODULES=yes)"
  assert_eq "WITH_OVN_KERNEL_MODULES=yes is preserved verbatim" "yes" "$resolved"

  local gate_count
  gate_count="$(grep -cE '"\$\{WITH_OVN_KERNEL_MODULES\}" == "true"' "$DEPLOY_INFRA_SH" || true)"
  assert_eq "deploy-infra.sh has exactly 1 strict WITH_OVN_KERNEL_MODULES==true gate" "1" "$gate_count"
}

# ---------------------------------------------------------------------------
# Test 4: the module load is gated
# load_ovn_kernel_modules must only run when WITH_OVN_KERNEL_MODULES=true, so
# the default Quick Start needs neither sudo nor modprobe access.
# ---------------------------------------------------------------------------
test_kernel_module_call_is_gated() {
  echo "Test: load_ovn_kernel_modules call is gated by WITH_OVN_KERNEL_MODULES"

  # Match the call site, not the function definition: the call carries no
  # arguments and is indented inside main().
  local call_line gate_line
  call_line="$(grep -nE '^[[:space:]]+load_ovn_kernel_modules$' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  gate_line="$(grep -n '"${WITH_OVN_KERNEL_MODULES}" == "true"' "$DEPLOY_INFRA_SH" | awk -F: -v target="${call_line:-0}" '$1 < target { last = $1 } END { print last }')"

  assert_not_empty "load_ovn_kernel_modules call site is found" "$call_line"
  assert_not_empty "WITH_OVN_KERNEL_MODULES gate precedes the kernel-module load" "$gate_line"
}

# ---------------------------------------------------------------------------
# Test 5: both entry points delegate to the shared loader
# The module lists live in the two named wrappers; the loader itself takes
# them as arguments. Pin both call lines so a future edit cannot drop a module
# or re-fork the implementation.
# ---------------------------------------------------------------------------
test_entry_points_delegate_to_the_shared_loader() {
  echo "Test: the OVN and chaos-mesh entry points delegate to load_host_kernel_modules"

  assert_file_contains \
    "load_ovn_kernel_modules asks for openvswitch and geneve" \
    "$DEPLOY_INFRA_SH" \
    'load_host_kernel_modules "OVN chassis (Open vSwitch datapath and Geneve tunnels)" openvswitch geneve'

  assert_file_contains \
    "load_chaos_mesh_kernel_modules keeps its ipset/tc module list" \
    "$DEPLOY_INFRA_SH" \
    'load_host_kernel_modules "chaos-mesh NetworkChaos" ip_set ip_set_hash_ip ip_set_hash_net xt_set sch_netem sch_tbf'
}

# ---------------------------------------------------------------------------
# Test 6: the argument-count guard is the one non-zero return
# A purpose without a module list is a caller bug, and main() runs the loader
# under `set -e`, so this is the only case that may abort the deployment.
# ---------------------------------------------------------------------------
test_arg_count_guard_returns_non_zero() {
  echo "Test: load_host_kernel_modules rejects a call without modules"

  local output rc
  output="$(run_loader "" "only-a-purpose")"
  rc=$?

  assert_eq "the guard returns 1" "1" "$rc"
  assert_contains "the guard names the missing arguments" \
    "$output" "ERROR: load_host_kernel_modules needs a purpose and at least one module."
}

# ---------------------------------------------------------------------------
# Test 7: a non-Linux host is skipped, not failed
# macOS developers run kind in a Linux VM whose kernel the script cannot reach.
# ---------------------------------------------------------------------------
test_non_linux_host_is_skipped() {
  echo "Test: load_host_kernel_modules skips a non-Linux host"

  local output rc
  output="$(run_loader 'uname() { echo Darwin; }' "x" openvswitch)"
  rc=$?

  assert_eq "the loader returns 0 on a non-Linux host" "0" "$rc"
  assert_contains "the skip is logged" "$output" "Non-Linux host"
  assert_not_contains "no module load is attempted" "$output" "Loading kernel modules"
}

# ---------------------------------------------------------------------------
# Test 8: no root and no passwordless sudo warns and continues
# ---------------------------------------------------------------------------
test_missing_sudo_warns_and_continues() {
  echo "Test: load_host_kernel_modules warns when it cannot become root"

  local output rc
  output="$(run_loader 'uname() { echo Linux; }; id() { echo 1000; }; sudo() { return 1; }' "x" openvswitch)"
  rc=$?

  assert_eq "the loader returns 0 without root" "0" "$rc"
  assert_contains "the missing privileges are logged" \
    "$output" "WARNING: not root and no passwordless sudo"
}

# ---------------------------------------------------------------------------
# Test 9: a failing modprobe warns and continues
# cobaltcore_fake_mod exists in no kernel, so /sys/module lookup misses and the
# shimmed modprobe fails. With apt-get failing too, every recovery path is
# exhausted and the loader must still return 0.
# ---------------------------------------------------------------------------
test_failing_modprobe_is_best_effort() {
  echo "Test: load_host_kernel_modules survives a failing modprobe"

  local output rc
  output="$(run_loader 'uname() { echo Linux; }; id() { echo 0; }; modprobe() { return 1; }; apt-get() { return 1; }' "x" cobaltcore_fake_mod)"
  rc=$?

  assert_eq "the loader returns 0 after a failed modprobe" "0" "$rc"
  assert_contains "the failed module is named" "$output" "modprobe cobaltcore_fake_mod failed"
  assert_contains "the failure is reported as a warning" "$output" "WARNING"
}

# ---------------------------------------------------------------------------
# Test 10: every module in the list reaches modprobe
# `local modules=("${@:2}")` plus the loop over it is the whole of the refactor,
# and both production callers pass more than one module — OVN two, chaos-mesh
# six. Narrowing it to `("$2")` would leave every other test green while
# load_ovn_kernel_modules silently skipped geneve.
#
# Fake module names keep the check host-independent: a real name already loaded
# on the runner would be short-circuited by the /sys/module/${mod} test.
# ---------------------------------------------------------------------------
test_every_module_is_attempted() {
  echo "Test: load_host_kernel_modules attempts every module it is given"

  local output rc
  output="$(run_loader 'uname() { echo Linux; }; id() { echo 0; }; modprobe() { echo "modprobe-called:$1"; return 1; }; apt-get() { return 1; }' \
    "x" cobaltcore_fake_a cobaltcore_fake_b)"
  rc=$?

  assert_eq "the loader returns 0" "0" "$rc"
  assert_contains "the first module reaches modprobe" "$output" "modprobe-called:cobaltcore_fake_a"
  assert_contains "the second module reaches modprobe" "$output" "modprobe-called:cobaltcore_fake_b"
}

# ---------------------------------------------------------------------------
# Test 11: the retry after a successful apt-get is exercised
# Test 9 fails apt-get, so it returns before the install branch and never
# reaches the retry loop or the still_missing warning — the very lines the
# ${purpose} refactor rewrote. Let apt-get succeed so the loader runs the retry,
# fails it, and has to report rather than claim success.
# ---------------------------------------------------------------------------
test_retry_after_apt_get_reports_still_missing() {
  echo "Test: load_host_kernel_modules reports a module still missing after the retry"

  local output rc
  output="$(run_loader 'uname() { echo Linux; }; id() { echo 0; }; modprobe() { return 1; }; apt-get() { return 0; }' \
    "x" cobaltcore_fake_mod)"
  rc=$?

  assert_eq "the loader returns 0 after the retry fails" "0" "$rc"
  assert_contains "the retry is attempted" "$output" "still failed after installing"
  assert_contains "the still-missing module is reported" "$output" "still missing after retry"
  assert_contains "the warning names the module" "$output" "cobaltcore_fake_mod"
}

# ---------------------------------------------------------------------------
# Test 12: the composite action threads the flag
# Without the passthrough a future OVN chassis job (issue #905) could set the
# flag on the job and still get the false default inside the step.
# ---------------------------------------------------------------------------
test_setup_action_threads_the_flag() {
  echo "Test: setup-e2e-infra threads WITH_OVN_KERNEL_MODULES into deploy-infra.sh"

  assert_file_contains "WITH_OVN_KERNEL_MODULES reaches deploy-infra.sh" \
    "$SETUP_ACTION" \
    'WITH_OVN_KERNEL_MODULES: ${{ env.WITH_OVN_KERNEL_MODULES }}'
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_default_is_false
test_explicit_true
test_non_true_value_does_not_trigger_load
test_kernel_module_call_is_gated
test_entry_points_delegate_to_the_shared_loader
test_arg_count_guard_returns_non_zero
test_non_linux_host_is_skipped
test_missing_sudo_warns_and_continues
test_failing_modprobe_is_best_effort
test_every_module_is_attempted
test_retry_after_apt_get_reports_still_missing
test_setup_action_threads_the_flag

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
