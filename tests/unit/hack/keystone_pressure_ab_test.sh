#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/keystone-pressure-ab.sh, the harness of the keystone-pressure CI
# job and of `make keystone-pressure-ab`:
#   1. The preflight names each missing setting, a profile that has no file,
#      a malformed duration and a release that is not under releases/, before
#      it calls kubectl; without KEYSTONE_PRESSURE_RELEASE it takes the newest
#      release.
#   2. ab_render_side gives each side a Keystone on its own image tag (the
#      release with a suffix, which still parses as the release), its own
#      database and its own Memcached, and a pull policy that keeps the
#      kind-loaded image.
#   3. ab_render_clouds names one cloud per side at that side's Service, on the
#      internal interface, and doubles a single quote of the password.
#   4. ab_render_client runs the dizzy image of the pin of hack/dizzy.sh,
#      non-root, with the scenarios and clouds.yaml mounted.
#   5. ab_run hands the profile's file, the side's cloud, the duration and the
#      extra flags to dizzy through kubectl exec, as arguments, not as shell
#      text; a profile's pair in KEYSTONE_PRESSURE_PROFILE_DURATIONS takes
#      precedence over KEYSTONE_PRESSURE_DURATION.
#   6. ab_write_summary puts each profile's comparison into summary.md, names
#      the runs that failed, marks an A/A run, and appends to
#      GITHUB_STEP_SUMMARY.
#   7. The default profiles exist, and none of them sends tokens to Nova,
#      Neutron, Cinder or Glance, which a standalone Keystone's catalog does
#      not list.
#
# The script is sourced in a subshell (its BASH_SOURCE guard keeps ab_main from
# running) with a stub kubectl first on PATH.
#
# Usage: bash tests/unit/hack/keystone_pressure_ab_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
AB_SH="$PROJECT_ROOT/hack/keystone-pressure-ab.sh"
DIZZY_SH="$PROJECT_ROOT/hack/dizzy.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stubs <dir> — a kubectl that appends its argv to $CALL_LOG and exits 0.
make_stubs() {
  mkdir -p "$1"
  cat >"$1/kubectl" <<'STUB'
#!/bin/bash
echo "kubectl $*" >>"$CALL_LOG"
STUB
  chmod +x "$1/kubectl"
}

# run_ab <tmp> <command> [env_var=value...]
# Sources the harness with the stubs of <tmp>/bin first on PATH and the given
# overrides, and evaluates <command>. Echoes combined output; returns its
# status.
run_ab() {
  local tmp="$1" cmd="$2"
  shift 2
  (
    unset KEYSTONE_PRESSURE_BASE_IMAGE KEYSTONE_PRESSURE_PATCHED_IMAGE KEYSTONE_PRESSURE_RELEASE \
      KEYSTONE_PRESSURE_PROFILES KEYSTONE_PRESSURE_DURATION KEYSTONE_PRESSURE_PROFILE_DURATIONS \
      KEYSTONE_PRESSURE_ARGS \
      KEYSTONE_PRESSURE_REPLICAS KEYSTONE_PRESSURE_OUTPUT KEYSTONE_PRESSURE_LOAD_IMAGES \
      KEYSTONE_PRESSURE_KEEP DIZZY_VERSION GITHUB_STEP_SUMMARY
    for assignment in "$@"; do
      export "${assignment?}"
    done
    PATH="$tmp/bin:$PATH"
    export PATH
    # shellcheck source=/dev/null
    source "$AB_SH"
    eval "$cmd"
  ) 2>&1
}

# The pin of hack/dizzy.sh.
pinned_dizzy() {
  sed -n 's/^DIZZY_VERSION="${DIZZY_VERSION:-\(v[0-9.]*\)}"$/\1/p' "$DIZZY_SH"
}

# ---------------------------------------------------------------------------
# Test 1: the preflight
# ---------------------------------------------------------------------------
test_preflight() {
  echo "Test: the preflight names each bad setting before it calls kubectl"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"
  local images=(KEYSTONE_PRESSURE_BASE_IMAGE=base:1 KEYSTONE_PRESSURE_PATCHED_IMAGE=patched:1
    KEYSTONE_PRESSURE_LOAD_IMAGES=false)

  : >"$CALL_LOG"
  output="$(run_ab "$tmp" ab_preflight KEYSTONE_PRESSURE_LOAD_IMAGES=false)"
  rc=$?
  assert_eq "without a base image it exits 1" "1" "$rc"
  assert_contains "and names the variable" "$output" "KEYSTONE_PRESSURE_BASE_IMAGE is not set"

  output="$(run_ab "$tmp" ab_preflight KEYSTONE_PRESSURE_LOAD_IMAGES=false KEYSTONE_PRESSURE_BASE_IMAGE=base:1)"
  rc=$?
  assert_eq "without a patched image it exits 1" "1" "$rc"
  assert_contains "and names the variable" "$output" "KEYSTONE_PRESSURE_PATCHED_IMAGE is not set"

  output="$(run_ab "$tmp" ab_preflight "${images[@]}" KEYSTONE_PRESSURE_RELEASE=1999.1)"
  rc=$?
  assert_eq "a release that is not under releases/ exits 1" "1" "$rc"
  assert_contains "and names it" "$output" "KEYSTONE_PRESSURE_RELEASE '1999.1' names no release under releases/."

  output="$(run_ab "$tmp" ab_preflight "${images[@]}" "KEYSTONE_PRESSURE_PROFILES=warm-cache nope")"
  rc=$?
  assert_eq "a profile without a file exits 1" "1" "$rc"
  assert_contains "and names it" "$output" "no pressure profile 'nope' in $PROJECT_ROOT/tests/load/keystone-pressure."

  output="$(run_ab "$tmp" ab_preflight "${images[@]}" KEYSTONE_PRESSURE_DURATION=2min)"
  rc=$?
  assert_eq "a malformed duration exits 1" "1" "$rc"
  assert_contains "and says what a duration looks like" "$output" "such as 2m or 1m30s"

  output="$(run_ab "$tmp" ab_preflight "${images[@]}" "KEYSTONE_PRESSURE_PROFILE_DURATIONS=effective-fan=6m warm-cache")"
  rc=$?
  assert_eq "a per-profile duration without a value exits 1" "1" "$rc"
  assert_contains "and names the entry" "$output" \
    "KEYSTONE_PRESSURE_PROFILE_DURATIONS holds 'warm-cache'; each entry is <profile>=<duration>"

  output="$(run_ab "$tmp" ab_preflight "${images[@]}" KEYSTONE_PRESSURE_PROFILE_DURATIONS=effective-fan=6min)"
  rc=$?
  assert_eq "a malformed per-profile duration exits 1" "1" "$rc"

  output="$(run_ab "$tmp" ab_preflight "${images[@]}" KEYSTONE_PRESSURE_REPLICAS=0)"
  rc=$?
  assert_eq "zero replicas exit 1" "1" "$rc"

  assert_eq "no setting error reached kubectl" "" "$(cat "$CALL_LOG")"

  # Every check passes against the stub kubectl.
  local newest
  newest="$(find "$PROJECT_ROOT/releases" -mindepth 1 -maxdepth 1 -type d -exec basename {} \; | sort | tail -n 1)"
  output="$(run_ab "$tmp" 'ab_preflight && echo "release=${KEYSTONE_PRESSURE_RELEASE}"' "${images[@]}" \
    KEYSTONE_PRESSURE_DURATION=1m30s "KEYSTONE_PRESSURE_PROFILE_DURATIONS=effective-fan=6m warm-cache=1m")"
  rc=$?
  assert_eq "good settings pass" "0" "$rc"
  assert_contains "the release defaults to the newest" "$output" "release=${newest}"
  assert_contains "the stack is checked for openstack-db" "$(cat "$CALL_LOG")" \
    "kubectl get mariadbs.k8s.mariadb.com openstack-db -n openstack"
  assert_contains "and for the admin Secret" "$(cat "$CALL_LOG")" \
    "kubectl get secret keystone-admin -n openstack"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 2: the two sides
# ---------------------------------------------------------------------------
test_render_side() {
  echo "Test: each side gets its own image tag, database and Memcached"

  local tmp output
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"

  output="$(run_ab "$tmp" 'ab_render_side patched' KEYSTONE_PRESSURE_RELEASE=2026.2 KEYSTONE_PRESSURE_REPLICAS=2)"
  local keystone memcached
  keystone="$(yq -N 'select(.kind == "Keystone")' <<<"$output")"
  memcached="$(yq -N 'select(.kind == "Memcached")' <<<"$output")"
  assert_eq "the Keystone is named after the side" "keystone-pressure-patched" \
    "$(yq -r '.metadata.name' <<<"$keystone")"
  assert_eq "it runs the side's tag, which parses as the release" "2026.2-pressure-patched" \
    "$(yq -r '.spec.image.tag' <<<"$keystone")"
  assert_eq "of the keystone repository" "ghcr.io/c5c3/keystone" \
    "$(yq -r '.spec.image.repository' <<<"$keystone")"
  assert_eq "without pulling over the kind-loaded image" "IfNotPresent" \
    "$(yq -r '.spec.image.pullPolicy' <<<"$keystone")"
  assert_eq "with the requested replicas" "2" "$(yq -r '.spec.deployment.replicas' <<<"$keystone")"
  assert_eq "on a database of its own" "keystone_pressure_patched" \
    "$(yq -r '.spec.database.database' <<<"$keystone")"
  assert_eq "on the stack's MariaDB" "openstack-db" "$(yq -r '.spec.database.clusterRef.name' <<<"$keystone")"
  assert_eq "on a Memcached of its own" "keystone-pressure-patched-cache" \
    "$(yq -r '.spec.cache.clusterRef.name' <<<"$keystone")"
  assert_eq "which the side brings" "keystone-pressure-patched-cache" \
    "$(yq -r '.metadata.name' <<<"$memcached")"
  assert_eq "bootstrapped from the stack's admin Secret" "keystone-admin" \
    "$(yq -r '.spec.bootstrap.adminPasswordSecretRef.name' <<<"$keystone")"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 3: clouds.yaml
# ---------------------------------------------------------------------------
test_render_clouds() {
  echo "Test: clouds.yaml names one in-cluster cloud per side"

  local tmp output
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"

  output="$(run_ab "$tmp" "ab_render_clouds \"s3cr'et\"")"
  assert_eq "the base cloud is the base Keystone's Service" \
    "http://keystone-pressure-base.openstack.svc:5000/v3" "$(yq -r '.clouds.base.auth.auth_url' <<<"$output")"
  assert_eq "the patched cloud is the patched Keystone's Service" \
    "http://keystone-pressure-patched.openstack.svc:5000/v3" "$(yq -r '.clouds.patched.auth.auth_url' <<<"$output")"
  assert_eq "the catalog is read on the internal interface" "internal" \
    "$(yq -r '.clouds.base.interface' <<<"$output")"
  assert_eq "the password survives a single quote" "s3cr'et" \
    "$(yq -r '.clouds.patched.auth.password' <<<"$output")"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 4: the client pod
# ---------------------------------------------------------------------------
test_render_client() {
  echo "Test: the client pod runs the pinned dizzy image, non-root"

  local tmp output
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"

  output="$(run_ab "$tmp" ab_render_client)"
  assert_eq "the image is the pin of hack/dizzy.sh" "ghcr.io/b42labs/dizzy:$(pinned_dizzy)" \
    "$(yq -r '.spec.containers[0].image' <<<"$output")"
  assert_eq "it runs as non-root" "true" "$(yq -r '.spec.securityContext.runAsNonRoot' <<<"$output")"
  assert_eq "dizzy reads the mounted clouds.yaml" "/etc/openstack/clouds.yaml" \
    "$(yq -r '.spec.containers[0].env[] | select(.name == "OS_CLIENT_CONFIG_FILE") | .value' <<<"$output")"
  assert_eq "from the clouds Secret" "keystone-pressure-clouds" \
    "$(yq -r '.spec.volumes[] | select(.name == "clouds") | .secret.secretName' <<<"$output")"
  assert_eq "the profiles are mounted at /scenarios" "/scenarios" \
    "$(yq -r '.spec.containers[0].volumeMounts[] | select(.name == "scenarios") | .mountPath' <<<"$output")"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 5: one run
# ---------------------------------------------------------------------------
test_run() {
  echo "Test: ab_run hands its arguments to dizzy through kubectl exec"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"

  : >"$CALL_LOG"
  output="$(run_ab "$tmp" 'ab_run warm-cache base' KEYSTONE_PRESSURE_DURATION=2m \
    "KEYSTONE_PRESSURE_ARGS=--seed 7 --workers 4")"
  rc=$?
  assert_eq "it exits with kubectl's status" "0" "$rc"
  assert_contains "the run goes to the client pod" "$(cat "$CALL_LOG")" \
    "kubectl exec -n openstack keystone-pressure-client -- sh -c"
  assert_contains "the run directory and dizzy's arguments follow the inline script" "$(cat "$CALL_LOG")" \
    "sh /work/warm-cache/base keystone pressure --os-cloud base --scenario /scenarios/warm-cache.yaml --duration 2m --seed 7 --workers 4"

  : >"$CALL_LOG"
  output="$(run_ab "$tmp" 'ab_run revocation-lookup patched')"
  assert_contains "without a duration the profile's own applies" "$(cat "$CALL_LOG")" \
    "sh /work/revocation-lookup/patched keystone pressure --os-cloud patched --scenario /scenarios/revocation-lookup.yaml"
  assert_not_contains "and no --duration is passed" "$(cat "$CALL_LOG")" "--duration"

  : >"$CALL_LOG"
  output="$(run_ab "$tmp" 'ab_run effective-fan base; ab_run warm-cache base' KEYSTONE_PRESSURE_DURATION=2m \
    KEYSTONE_PRESSURE_PROFILE_DURATIONS=effective-fan=6m)"
  assert_contains "a profile's own pair wins" "$(cat "$CALL_LOG")" \
    "--scenario /scenarios/effective-fan.yaml --duration 6m"
  assert_contains "the other profiles keep the common duration" "$(cat "$CALL_LOG")" \
    "--scenario /scenarios/warm-cache.yaml --duration 2m"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 6: the summary
# ---------------------------------------------------------------------------
test_summary() {
  echo "Test: summary.md carries each comparison and names each failure"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"
  mkdir -p "$tmp/out/warm-cache"
  echo "validate   p50  1.84ms  1.42ms  -420µs  -22.8%" >"$tmp/out/warm-cache/comparison.txt"
  : >"$tmp/step-summary.md"

  output="$(run_ab "$tmp" "AB_OUTPUT_DIR=$tmp/out; AB_A_A=true; ab_write_summary \$'warm-cache 0 0 0\nrevocation-lookup 0 1 -\neffective-fan 0 0 1\n'" \
    KEYSTONE_PRESSURE_RELEASE=2026.2 KEYSTONE_PRESSURE_BASE_IMAGE=ghcr.io/c5c3/keystone:2026.2-main \
    KEYSTONE_PRESSURE_PATCHED_IMAGE=ghcr.io/c5c3/keystone:2026.2 KEYSTONE_PRESSURE_DURATION=2m \
    "GITHUB_STEP_SUMMARY=$tmp/step-summary.md")"
  rc=$?
  assert_eq "it exits 0" "0" "$rc"
  local summary="$tmp/out/summary.md"
  assert_file_contains_fixed "it names the release" "$summary" "## Keystone pressure 2026.2: base against patched"
  assert_file_contains_fixed "and both images" "$summary" "- patched: \`ghcr.io/c5c3/keystone:2026.2\`"
  assert_file_contains_fixed "it marks the A/A run" "$summary" "an A/A run"
  assert_file_contains_fixed "it carries the comparison" "$summary" "validate   p50  1.84ms  1.42ms  -420µs  -22.8%"
  assert_file_contains_fixed "it names a failed run" "$summary" \
    "The run failed (dizzy exit code base 0, patched 1); see the job log."
  assert_file_contains_fixed "and a failed comparison" "$summary" "The comparison failed (exit code 1)"
  assert_eq "it appends the summary to the job summary" "$(cat "$summary")" "$(cat "$tmp/step-summary.md")"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 7: the default profiles
# ---------------------------------------------------------------------------
test_default_profiles() {
  echo "Test: the default profiles exist and stay inside a standalone Keystone"

  local defaults profile file
  defaults="$(sed -n 's/^AB_DEFAULT_PROFILES="\(.*\)"$/\1/p' "$AB_SH")"
  assert_not_empty "the harness names its default profiles" "$defaults"
  for profile in $defaults; do
    file="$PROJECT_ROOT/tests/load/keystone-pressure/${profile}.yaml"
    assert_eq "${profile} has a file" "true" "$([[ -f "$file" ]] && echo true || echo false)"
    [[ -f "$file" ]] || continue
    assert_eq "${profile} sends no probe" "0" "$(yq -r '.pressure.mix.probe // 0' "$file")"
    assert_eq "${profile} sends no bad token" "0" "$(yq -r '.pressure.mix.bad_token // 0' "$file")"
    assert_eq "${profile} runs no propagation probe" "0" "$(yq -r '.pressure.propagation.every // 0' "$file")"
  done
  # revocation-lookup, whose revocation events stay in the database, runs last.
  assert_eq "revocation-lookup is the last default" "revocation-lookup" "${defaults##* }"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if ! command -v yq >/dev/null 2>&1; then
  echo "SKIP: yq not installed"
  exit 0
fi

test_preflight
test_render_side
test_render_clouds
test_render_client
test_run
test_summary
test_default_profiles

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
