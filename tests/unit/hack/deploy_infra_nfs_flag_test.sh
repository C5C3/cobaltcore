#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh gates the NFS storage stack behind WITH_NFS: the
# host-side nfsd/nfs/nfsv4 module load, the deploy/kind/nfs overlay apply with
# its rollout wait on the nfs-server Deployment, and the csi-driver-nfs
# HelmRelease wait. The default Quick Start must install none of it, and a
# typo like WITH_NFS=yes must take the skip branch at all three gates.
#
# It also pins the loader half: load_nfs_kernel_modules delegates to the shared
# load_host_kernel_modules with the three module names, and that loader keeps
# its best-effort posture (it warns and returns 0 on every host condition), so
# the rollout wait is what actually fails a broken install.
#
# Implementation: bash + tests/lib/assertions.sh, matching the sibling
# tests/unit/hack/deploy_infra_chaos_flag_test.sh and
# tests/unit/hack/deploy_infra_ovn_kernel_modules_flag_test.sh. The repo has
# zero .bats files and no bats binary on CI, so introducing one would add an
# undeclared dependency.
#
# Strategy: hybrid. Source the script in a subshell (the
# `BASH_SOURCE[0] == ${0}` guard at the bottom of deploy-infra.sh keeps main()
# from auto-running) to read the resolved flag default and to drive
# load_host_kernel_modules against shell functions that shadow uname, id, sudo,
# modprobe and apt-get; grep the script source for the three strict gates, the
# gate-before-action order, and the delegation line.
#
# Usage: bash tests/unit/hack/deploy_infra_nfs_flag_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"
SETUP_ACTION="$PROJECT_ROOT/.github/actions/setup-e2e-infra/action.yaml"

# The purpose string load_nfs_kernel_modules hands the shared loader. Kept in
# one place so the delegation-line assertion and the loader drives agree.
NFS_PURPOSE='NFS server and client (kernel nfsd for the in-cluster server, nfs and nfsv4 for the csi-driver-nfs node plugin)'

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# resolve_with_nfs [env_var=value...]
# Sources deploy-infra.sh in a subshell with the supplied env overrides and
# echoes the resolved value of WITH_NFS after the configuration block runs.
resolve_with_nfs() {
  (
    # Apply each env override in the subshell before sourcing.
    for assignment in "$@"; do
      export "${assignment?}"
    done
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    printf '%s' "${WITH_NFS}"
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
# Test 1: WITH_NFS defaults to false
# The Quick Start must install neither the server nor the mounter, and must
# not modprobe anything on a developer's host.
# ---------------------------------------------------------------------------
test_default_is_false() {
  echo "Test: WITH_NFS defaults to false"

  local resolved
  resolved="$(unset WITH_NFS; resolve_with_nfs)"
  assert_eq "WITH_NFS defaults to false" "false" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 2: explicit WITH_NFS=true
# ---------------------------------------------------------------------------
test_explicit_true() {
  echo "Test: WITH_NFS=true is preserved"

  local resolved
  resolved="$(resolve_with_nfs WITH_NFS=true)"
  assert_eq "WITH_NFS=true is preserved" "true" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 3: explicit WITH_NFS=false
# ---------------------------------------------------------------------------
test_explicit_false() {
  echo "Test: WITH_NFS=false is preserved"

  local resolved
  resolved="$(resolve_with_nfs WITH_NFS=false)"
  assert_eq "WITH_NFS=false is preserved" "false" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 4: defensive non-true value
# A typo like WITH_NFS=yes must not install anything, because every gate site
# uses the strict `== "true"` comparison. Assert the value passes through
# verbatim AND that all three gate sites are exact-match.
# ---------------------------------------------------------------------------
test_non_true_value_does_not_trigger_install() {
  echo "Test: WITH_NFS=yes passes through but does not trigger install"

  local resolved
  resolved="$(resolve_with_nfs WITH_NFS=yes)"
  assert_eq "WITH_NFS=yes is preserved verbatim" "yes" "$resolved"

  local gate_count
  gate_count="$(grep -cE '"\$\{WITH_NFS\}" == "true"' "$DEPLOY_INFRA_SH" || true)"
  assert_eq "deploy-infra.sh has exactly 3 strict WITH_NFS==true gates" "3" "$gate_count"
}

# ---------------------------------------------------------------------------
# Test 5: the configuration banner reports the flag
# The banner is the one place an operator sees which opt-ins a run resolved to.
# ---------------------------------------------------------------------------
test_banner_includes_nfs_line() {
  echo "Test: the configuration banner reports WITH_NFS"

  assert_file_contains "the banner has an NFS storage stack line" \
    "$DEPLOY_INFRA_SH" 'NFS storage stack'

  assert_file_contains "the banner names the variable that turns it on" \
    "$DEPLOY_INFRA_SH" 'set WITH_NFS=true'
}

# ---------------------------------------------------------------------------
# Test 6: the module load is gated
# load_nfs_kernel_modules must only run when WITH_NFS=true, so the default
# Quick Start needs neither sudo nor modprobe access.
# ---------------------------------------------------------------------------
test_kernel_module_call_is_gated() {
  echo "Test: load_nfs_kernel_modules call is gated by WITH_NFS"

  # Match the call site, not the function definition: the call carries no
  # arguments and is indented inside main().
  local call_line gate_line
  call_line="$(grep -nE '^[[:space:]]+load_nfs_kernel_modules$' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  gate_line="$(grep -n '"${WITH_NFS}" == "true"' "$DEPLOY_INFRA_SH" | awk -F: -v target="${call_line:-0}" '$1 < target { last = $1 } END { print last }')"

  assert_not_empty "load_nfs_kernel_modules call site is found" "$call_line"
  assert_not_empty "WITH_NFS gate precedes the kernel-module load" "$gate_line"
}

# ---------------------------------------------------------------------------
# Test 7: the overlay apply is gated
# The kustomize apply for deploy/kind/nfs must live inside the WITH_NFS gate.
# ---------------------------------------------------------------------------
test_nfs_kustomize_is_gated() {
  echo "Test: the deploy/kind/nfs apply is gated by WITH_NFS"

  local apply_line gate_line
  apply_line="$(grep -n 'kubectl apply -k "${REPO_ROOT}/deploy/kind/nfs"' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  gate_line="$(grep -n '"${WITH_NFS}" == "true"' "$DEPLOY_INFRA_SH" | awk -F: -v target="${apply_line:-0}" '$1 < target { last = $1 } END { print last }')"

  assert_not_empty "kustomize apply line for the NFS overlay is found" "$apply_line"
  assert_not_empty "WITH_NFS gate precedes the kustomize apply" "$gate_line"
}

# ---------------------------------------------------------------------------
# Test 8: the rollout wait follows the apply and fails the run
# The module load above is best-effort, so a host without nfsd leaves the
# server CrashLooping. Applying the overlay and walking on would hand the
# Cinder suites a share nothing serves, so the wait aborts deploy-infra.
# ---------------------------------------------------------------------------
test_rollout_guard_follows_the_apply() {
  echo "Test: the nfs-server rollout wait follows the overlay apply"

  local apply_line guard_line
  apply_line="$(grep -n 'kubectl apply -k "${REPO_ROOT}/deploy/kind/nfs"' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  guard_line="$(grep -n 'kubectl rollout status deployment/nfs-server -n openstack' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"

  assert_not_empty "the rollout wait on nfs-server is found" "$guard_line"
  assert_gte "the rollout wait follows the apply" "${guard_line:-0}" "$((${apply_line:-0} + 1))"

  assert_file_contains "a failed rollout is reported as an error" \
    "$DEPLOY_INFRA_SH" 'ERROR: the NFS server did not roll out'
}

# ---------------------------------------------------------------------------
# Test 9: csi-driver-nfs is appended dynamically to the helm-release wait list
# Waiting for it unconditionally would hang every default run, since the
# HelmRelease only exists once the overlay is applied.
# ---------------------------------------------------------------------------
test_nfs_appended_dynamically() {
  echo "Test: csi-driver-nfs is appended dynamically to the helm-release wait list"

  assert_file_contains \
    "helm_releases array keeps the base releases in the documented order" \
    "$DEPLOY_INFRA_SH" \
    'helm_releases=(prometheus-operator-crds shared-services/openbao mariadb-operator-crds mariadb-operator external-secrets memcached-operator envoy-gateway garage-operator openbao-operator)'

  assert_file_not_contains \
    "the base helm_releases array does not hard-code csi-driver-nfs" \
    "$DEPLOY_INFRA_SH" \
    'helm_releases=(prometheus-operator-crds.*csi-driver-nfs'

  assert_file_contains \
    "csi-driver-nfs is appended to the wait list" \
    "$DEPLOY_INFRA_SH" \
    'helm_releases+=(csi-driver-nfs)'

  local append_line gate_line
  append_line="$(grep -n 'helm_releases+=(csi-driver-nfs)' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  gate_line="$(grep -n '"${WITH_NFS}" == "true"' "$DEPLOY_INFRA_SH" | awk -F: -v target="${append_line:-0}" '$1 < target { last = $1 } END { print last }')"

  assert_not_empty "WITH_NFS gate precedes the csi-driver-nfs append" "$gate_line"
}

# ---------------------------------------------------------------------------
# Test 10: the entry point delegates to the shared loader
# The module list lives in the named wrapper; the loader takes it as arguments.
# Pin the call line so a future edit cannot drop a module or re-fork the
# implementation.
# ---------------------------------------------------------------------------
test_entry_point_delegates_to_the_shared_loader() {
  echo "Test: load_nfs_kernel_modules delegates to load_host_kernel_modules"

  assert_file_contains_literal \
    "load_nfs_kernel_modules asks for nfsd, nfs and nfsv4" \
    "$DEPLOY_INFRA_SH" \
    "load_host_kernel_modules \"${NFS_PURPOSE}\" nfsd nfs nfsv4"
}

# ---------------------------------------------------------------------------
# Test 11: a non-Linux host is skipped, not failed
# macOS developers run kind in a Linux VM whose kernel the script cannot reach.
# ---------------------------------------------------------------------------
test_loader_skips_non_linux() {
  echo "Test: the NFS module load skips a non-Linux host"

  local output rc
  output="$(run_loader 'uname() { echo Darwin; }' "$NFS_PURPOSE" nfsd nfs nfsv4)"
  rc=$?

  assert_eq "the loader returns 0 on a non-Linux host" "0" "$rc"
  assert_contains "the skip is logged" "$output" "Non-Linux host"
  assert_not_contains "no module load is attempted" "$output" "Loading kernel modules"
}

# ---------------------------------------------------------------------------
# Test 12: no root and no passwordless sudo warns and continues
# deploy-infra runs the loader under `set -e`, so a return of 1 here would
# abort a run that the rollout wait is meant to judge.
# ---------------------------------------------------------------------------
test_loader_warns_without_sudo() {
  echo "Test: the NFS module load warns when it cannot become root"

  local output rc
  output="$(run_loader 'uname() { echo Linux; }; id() { echo 1000; }; sudo() { return 1; }' "$NFS_PURPOSE" nfsd nfs nfsv4)"
  rc=$?

  assert_eq "the loader returns 0 without root" "0" "$rc"
  assert_contains "the missing privileges are logged" \
    "$output" "WARNING: not root and no passwordless sudo"
  assert_contains "the warning names the NFS purpose" "$output" "NFS server and client"
}

# ---------------------------------------------------------------------------
# Test 13: a failing modprobe warns and continues
# Fake module names keep the check host-independent: `nfs` is already loaded on
# plenty of Linux runners, and the loader short-circuits on /sys/module/${mod},
# so a real name would prove nothing. Test 10 pins the real names.
# ---------------------------------------------------------------------------
test_loader_survives_failing_modprobe() {
  echo "Test: the NFS module load survives a failing modprobe"

  local output rc
  output="$(run_loader 'uname() { echo Linux; }; id() { echo 0; }; modprobe() { return 1; }; apt-get() { return 1; }' \
    "$NFS_PURPOSE" cobaltcore_fake_nfsd cobaltcore_fake_nfsv4)"
  rc=$?

  assert_eq "the loader returns 0 after a failed modprobe" "0" "$rc"
  assert_contains "the first failed module is named" "$output" "modprobe cobaltcore_fake_nfsd failed"
  assert_contains "the second failed module is named" "$output" "modprobe cobaltcore_fake_nfsv4 failed"
  assert_contains "the failure is reported as a warning" "$output" "WARNING"
}

# ---------------------------------------------------------------------------
# Test 14: production-caller contract — the NFS apply mirrors a self-contained
# overlay and works under kubectl's embedded kustomize.
#
# kubectl apply -k uses the embedded kustomize, which does NOT expose
# --load-restrictor (kubernetes/kubectl#948). Both halves are pinned here:
#
#   (a) deploy-infra.sh's NFS apply line uses plain `kubectl apply -k` (no
#       `--load-restrictor`, no pipe through `kustomize build` to
#       `kubectl apply -f -`); and
#   (b) deploy/kind/nfs/kustomization.yaml has zero parent-directory resource
#       entries, so the default LoadRestrictionsRootOnly check is satisfied.
#
# Either half changing must update the other.
# ---------------------------------------------------------------------------
test_production_caller_matches_self_contained_overlay() {
  echo "Test: production caller and NFS overlay agree on the no-load-restrictor contract"

  # (a) The apply line is the bare kubectl form — no pipe, no flag.
  local raw
  raw="$(grep -E 'kubectl apply -k "\$\{REPO_ROOT\}/deploy/kind/nfs"' "$DEPLOY_INFRA_SH" | head -1)"
  assert_not_empty "deploy-infra.sh has the NFS kubectl apply line" "$raw"

  if printf '%s' "$raw" | grep -q -- '--load-restrictor'; then
    echo "  FAIL: deploy-infra.sh's NFS apply line passes --load-restrictor (kubectl's embedded kustomize does not accept it; switch to a 'kustomize build | kubectl apply -f -' pipeline if you really need that flag)"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: NFS apply line uses no --load-restrictor flag"
    PASS=$((PASS + 1))
  fi

  # Reject the covert pipeline form too: piping `kustomize build` into
  # `kubectl apply -f -` re-introduces the standalone-kustomize dependency
  # without updating install-test-deps.
  if grep -E 'kustomize build.*\| *kubectl apply -f -' "$DEPLOY_INFRA_SH" >/dev/null 2>&1; then
    echo "  FAIL: deploy-infra.sh pipes kustomize build into kubectl apply (introduces undeclared kustomize CLI dependency; remove or wire kustomize into install-test-deps)"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: deploy-infra.sh does not pipe kustomize build into kubectl apply"
    PASS=$((PASS + 1))
  fi

  # (b) The overlay must have zero parent-directory resource entries so the
  # default LoadRestrictionsRootOnly check is satisfied.
  local kust="$PROJECT_ROOT/deploy/kind/nfs/kustomization.yaml"
  if [[ ! -f "$kust" ]]; then
    echo "  FAIL: $kust does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  local parent_refs
  parent_refs="$( { grep -E '^[[:space:]]*-[[:space:]]+\.\./' "$kust" || true; } | wc -l)"
  assert_eq "deploy/kind/nfs/kustomization.yaml has no parent-directory resource entries" \
    "0" "${parent_refs// /}"
}

# ---------------------------------------------------------------------------
# Test 15: the composite action threads the flag
# Without the passthrough a CI job could set WITH_NFS on the job and still get
# the false default inside the deploy step.
# ---------------------------------------------------------------------------
test_setup_action_threads_the_flag() {
  echo "Test: setup-e2e-infra threads WITH_NFS into deploy-infra.sh"

  assert_file_contains_literal "WITH_NFS reaches deploy-infra.sh" \
    "$SETUP_ACTION" \
    'WITH_NFS: ${{ env.WITH_NFS }}'
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_default_is_false
test_explicit_true
test_explicit_false
test_non_true_value_does_not_trigger_install
test_banner_includes_nfs_line
test_kernel_module_call_is_gated
test_nfs_kustomize_is_gated
test_rollout_guard_follows_the_apply
test_nfs_appended_dynamically
test_entry_point_delegates_to_the_shared_loader
test_loader_skips_non_linux
test_loader_warns_without_sudo
test_loader_survives_failing_modprobe
test_production_caller_matches_self_contained_overlay
test_setup_action_threads_the_flag

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
