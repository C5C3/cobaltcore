#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify how TEMPEST_KEYSTONE_LEAK_CHECK reaches hack/tempest/run-tests.sh:
#   - hack/ci-run-tempest.sh stages hack/tempest/keystone-inventory.py next to
#     tempest.conf and hands the Tempest container -e
#     TEMPEST_KEYSTONE_LEAK_CHECK=off when the variable is unset and the
#     caller's value when it is set; the external-network lookup docker run of
#     the neutron legs gets no such variable;
#   - run_tempest of hack/run-tempest.sh stages the helper too and hands the
#     container fail for SERVICE=keystone, off for any other service, and the
#     caller's value when the variable is set;
#   - hack/tempest/run-tests.sh exits 1 on a value other than off or fail
#     before it touches /tmp, on a host without tempest on PATH, and accepts
#     off, fail, an empty value and no value;
#   - the tempest job of ci.yaml sets fail on the legs whose service starts
#     with keystone and off on the others.
#
# hack/ci-run-tempest.sh runs with the stub kubectl, curl and docker of
# tests/unit/hack/ci_run_tempest_public_network_test.sh first on PATH: kubectl
# answers the admin-secret read and blocks on a sleeping port-forward, curl
# answers every readiness poll, and docker records each argv in the case's
# docker.log and answers a network lookup with one network id. run-tempest.sh
# is sourced in a subshell (its BASH_SOURCE guard keeps main() from running)
# with stub curl, docker, timeout and uname commands. run-tests.sh runs with
# PATH set to a directory that holds only a mkdir stub, which records its
# arguments and exits 1: the script stops at its first mkdir, so a value that
# passes the check is the one that reaches mkdir.
#
# Usage: bash tests/unit/hack/tempest_leak_check_wiring_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_SCRIPT="$PROJECT_ROOT/hack/ci-run-tempest.sh"
LOCAL_SCRIPT="$PROJECT_ROOT/hack/run-tempest.sh"
RUNNER="$PROJECT_ROOT/hack/tempest/run-tests.sh"
HELPER="$PROJECT_ROOT/hack/tempest/keystone-inventory.py"
BASE_PATH="$PATH"
BASH_BIN="$(command -v bash)"
# shellcheck disable=SC2034 # read by tests/lib/ci_yaml.sh
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

PASS=0
FAIL=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

PASSWORD="Tempest-Admin-Pw-1840"
NETWORK_ID="0b6a3c1e-4f2d-4a8b-9c7e-1d2f3a4b5c6d"

# --- Stubs of hack/ci-run-tempest.sh ---------------------------------------
CI_STUBS="$TMP_DIR/ci-stubs"
mkdir -p "$CI_STUBS"
cat >"$CI_STUBS/kubectl" <<STUB
#!/bin/bash
case "\$1" in
  get) printf '%s' "$(printf '%s' "$PASSWORD" | base64)" ;;
  # Absolute path: a real forward outlives the suite it serves.
  port-forward) exec /bin/sleep 600 ;;
esac
exit 0
STUB
cat >"$CI_STUBS/curl" <<'STUB'
#!/bin/bash
exit 0
STUB
cat >"$CI_STUBS/docker" <<STUB
#!/bin/bash
printf '%s\n' "\$*" >>"\$STUB_CASE_DIR/docker.log"
case " \$* " in
  *" network list "*) echo "$NETWORK_ID" ;;
esac
exit 0
STUB
chmod +x "$CI_STUBS/kubectl" "$CI_STUBS/curl" "$CI_STUBS/docker"

# --- Stubs of hack/run-tempest.sh ------------------------------------------
LOCAL_STUBS="$TMP_DIR/local-stubs"
mkdir -p "$LOCAL_STUBS"
# curl succeeds, so run_tempest reuses the forward on localhost:5000 and
# never calls kubectl.
cp "$CI_STUBS/curl" "$LOCAL_STUBS/curl"
cat >"$LOCAL_STUBS/docker" <<'STUB'
#!/bin/bash
printf '%s\n' "$*" >>"$STUB_CASE_DIR/docker.log"
exit 0
STUB
cat >"$LOCAL_STUBS/timeout" <<'STUB'
#!/bin/bash
shift
exec "$@"
STUB
cat >"$LOCAL_STUBS/uname" <<'STUB'
#!/bin/bash
echo Linux
STUB
chmod +x "$LOCAL_STUBS/curl" "$LOCAL_STUBS/docker" "$LOCAL_STUBS/timeout" \
  "$LOCAL_STUBS/uname"

# --- The only command run-tests.sh finds -----------------------------------
RUNNER_STUBS="$TMP_DIR/runner-stubs"
mkdir -p "$RUNNER_STUBS"
cat >"$RUNNER_STUBS/mkdir" <<'STUB'
#!/bin/bash
printf '%s\n' "$*" >>"$MKDIR_LOG"
exit 1
STUB
chmod +x "$RUNNER_STUBS/mkdir"

# run_ci_case <name> <value|unset> <with-neutron: yes|no>
# Runs hack/ci-run-tempest.sh as a keystone leg, or as a neutron leg whose
# tempest.conf names ${NEUTRON_PUBLIC_NETWORK_ID}, with
# TEMPEST_KEYSTONE_LEAK_CHECK set to <value> or unset. Sets DIR, OUTPUT and RC.
run_ci_case() {
  local value="$2" neutron="$3"
  DIR="$TMP_DIR/ci-$1"
  mkdir -p "$DIR/config"
  printf '%s\n' '[identity]' \
    'uri_v3 = http://keystone-tempest-2026-1.openstack.svc:5000/v3' '' \
    '[auth]' "admin_password = \${KEYSTONE_ADMIN_PASSWORD}" \
    >"$DIR/config/tempest.conf"
  if [ "$neutron" = yes ]; then
    printf '%s\n' '' '[network]' \
      "public_network_id = \${NEUTRON_PUBLIC_NETWORK_ID}" \
      >>"$DIR/config/tempest.conf"
  fi
  printf '%s\n' 'tempest.api.identity' 'keystone_tempest_plugin.tests' \
    >"$DIR/config/include-tests.txt"
  : >"$DIR/docker.log"
  OUTPUT=$(
    unset GLANCE_K8S_NAME BARBICAN_K8S_NAME CINDER_K8S_NAME NOVA_K8S_NAME \
      NOVA_CONSOLE_K8S_NAME PLACEMENT_K8S_NAME NEUTRON_K8S_NAME \
      TEMPEST_KEYSTONE_LEAK_CHECK
    if [ "$value" != unset ]; then
      export TEMPEST_KEYSTONE_LEAK_CHECK="$value"
    fi
    if [ "$neutron" = yes ]; then
      export NEUTRON_K8S_NAME=neutron-tempest-2026-1
    fi
    PATH="$CI_STUBS:$BASE_PATH"
    CONFIG_DIR="$DIR/config"
    OUTPUT_DIR="$DIR/output"
    GITHUB_WORKSPACE="$DIR"
    SERVICE_K8S_NAME=keystone-tempest-2026-1
    STUB_CASE_DIR="$DIR"
    export PATH CONFIG_DIR OUTPUT_DIR GITHUB_WORKSPACE SERVICE_K8S_NAME
    export STUB_CASE_DIR
    TEMPEST_IMAGE=ghcr.io/c5c3/tempest:2026.1 bash "$CI_SCRIPT" 2>&1
  )
  RC=$?
}

# run_local_case <name> <service> <value|unset>
# Sources hack/run-tempest.sh with SERVICE=<service> and
# TEMPEST_KEYSTONE_LEAK_CHECK set to <value> or unset, then runs run_tempest.
# The substitution is not tested by || or if, which would switch off the
# script's set -e inside it. Sets DIR, OUTPUT and RC.
run_local_case() {
  local service="$2" value="$3"
  DIR="$TMP_DIR/local-$1"
  mkdir -p "$DIR"
  : >"$DIR/docker.log"
  OUTPUT=$(
    unset TEMPEST_KEYSTONE_LEAK_CHECK SERVICE_NAME RELEASE
    if [ "$value" != unset ]; then
      TEMPEST_KEYSTONE_LEAK_CHECK="$value"
    fi
    PATH="$LOCAL_STUBS:$BASE_PATH"
    SERVICE="$service"
    OUTPUT_DIR="$DIR/output"
    STUB_CASE_DIR="$DIR"
    export STUB_CASE_DIR
    # shellcheck source=hack/run-tempest.sh
    source "$LOCAL_SCRIPT"
    run_tempest "$PASSWORD" 2>&1
  )
  RC=$?
}

# run_runner <env assignment>...
# Runs hack/tempest/run-tests.sh with nothing but the given variables, a
# PATH that holds only the mkdir stub, and TEMPEST_CONCURRENCY=1. Sets OUTPUT,
# RC and MKDIRS (the recorded mkdir calls).
run_runner() {
  local log="$TMP_DIR/mkdir.log"
  : >"$log"
  OUTPUT=$(cd "$TMP_DIR" && env -i PATH="$RUNNER_STUBS" MKDIR_LOG="$log" \
    TEMPEST_CONCURRENCY=1 "$@" "$BASH_BIN" "$RUNNER" 2>&1)
  RC=$?
  MKDIRS="$(cat "$log")"
}

# assert_helper_staged <case> <config dir>
assert_helper_staged() {
  local staged="$2/keystone-inventory.py"
  assert_eq "$1: keystone-inventory.py is staged next to tempest.conf" "yes" \
    "$([ -f "$staged" ] && cmp -s "$staged" "$HELPER" && echo yes || echo no)"
  assert_eq "$1: the staged helper is executable" "yes" \
    "$([ -x "$staged" ] && echo yes || echo no)"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_ci_wrapper_defaults_to_off() {
  echo "Test: hack/ci-run-tempest.sh hands the container off when the variable is unset"

  run_ci_case unset unset no

  assert_eq "it exits 0" "0" "$RC"
  assert_not_contains "it prints no ::error::" "$OUTPUT" "::error::"
  assert_contains "the Tempest container gets =off" \
    "$(grep -F run-tests.sh "$DIR/docker.log")" \
    "-e TEMPEST_KEYSTONE_LEAK_CHECK=off "
  assert_helper_staged "unset" "$DIR/output/config"
}

test_ci_wrapper_passes_fail_on() {
  echo "Test: hack/ci-run-tempest.sh hands the container the caller's fail"

  run_ci_case fail fail no

  assert_eq "it exits 0" "0" "$RC"
  assert_contains "the Tempest container gets =fail" \
    "$(grep -F run-tests.sh "$DIR/docker.log")" \
    "-e TEMPEST_KEYSTONE_LEAK_CHECK=fail "
  assert_helper_staged "fail" "$DIR/output/config"
}

test_ci_wrapper_keeps_the_lookup_free_of_it() {
  echo "Test: the external-network lookup docker run gets no TEMPEST_KEYSTONE_LEAK_CHECK"

  run_ci_case neutron fail yes

  assert_eq "it exits 0" "0" "$RC"
  assert_eq "it makes two docker calls" "2" "$(grep -c . "$DIR/docker.log")"
  assert_not_contains "the network lookup gets no leak check variable" \
    "$(grep -F "network list" "$DIR/docker.log")" "TEMPEST_KEYSTONE_LEAK_CHECK"
  assert_eq "one docker call carries the variable, the Tempest run" "1" \
    "$(grep -c TEMPEST_KEYSTONE_LEAK_CHECK "$DIR/docker.log")"
  assert_contains "and that call runs the suites" \
    "$(grep -F TEMPEST_KEYSTONE_LEAK_CHECK "$DIR/docker.log")" "run-tests.sh"
}

test_local_wrapper_defaults_by_service() {
  echo "Test: run_tempest of hack/run-tempest.sh defaults by SERVICE and keeps a set value"

  local service value want
  while read -r service value want; do
    run_local_case "$service-$value" "$service" "$value"
    assert_eq "SERVICE=$service, variable $value: run_tempest exits 0" "0" "$RC"
    assert_contains "SERVICE=$service, variable $value: the container gets =$want" \
      "$(grep -F run-tests.sh "$DIR/docker.log")" \
      "-e TEMPEST_KEYSTONE_LEAK_CHECK=$want "
    assert_helper_staged "SERVICE=$service, variable $value" \
      "$DIR/output/config"
  done <<'CASES'
keystone unset fail
glance unset off
glance fail fail
keystone off off
CASES
}

test_runner_rejects_an_unknown_value() {
  echo "Test: hack/tempest/run-tests.sh exits 1 on a value other than off or fail"

  assert_eq "the runner's PATH has no tempest" "no" \
    "$(PATH="$RUNNER_STUBS" command -v tempest >/dev/null && echo yes || echo no)"

  local value
  for value in warn FAIL; do
    run_runner "TEMPEST_KEYSTONE_LEAK_CHECK=$value"
    assert_eq "$value: it exits 1" "1" "$RC"
    assert_eq "$value: it prints the one error line" \
      "ERROR: TEMPEST_KEYSTONE_LEAK_CHECK must be off or fail, got '$value'." \
      "$OUTPUT"
    assert_eq "$value: it creates no /tmp/tempest-workspace" "" "$MKDIRS"
  done

  run_runner TEMPEST_KEYSTONE_LEAK_CHECK=warn TEMPEST_ERROR_PREFIX=::error::
  assert_eq "the error line takes TEMPEST_ERROR_PREFIX" \
    "::error::TEMPEST_KEYSTONE_LEAK_CHECK must be off or fail, got 'warn'." \
    "$OUTPUT"
}

test_runner_accepts_off_fail_empty_and_unset() {
  echo "Test: hack/tempest/run-tests.sh passes off, fail, empty and unset to its first mkdir"

  local setting
  for setting in TEMPEST_KEYSTONE_LEAK_CHECK=off TEMPEST_KEYSTONE_LEAK_CHECK=fail \
    TEMPEST_KEYSTONE_LEAK_CHECK= UNRELATED=1; do
    run_runner "$setting"
    assert_not_contains "$setting: the value check passes" "$OUTPUT" \
      "must be off or fail"
    assert_eq "$setting: the script reaches its first mkdir" \
      "-p /tmp/tempest-workspace /tmp/tempest-logs" "$MKDIRS"
  done
}

test_ci_yaml_sets_fail_on_the_keystone_legs() {
  echo "Test: the Run Tempest API tests step sets the variable from matrix.service"

  # The literal ${{ }} expression is the expected text.
  # shellcheck disable=SC2016
  assert_contains "fail where matrix.service starts with keystone, off elsewhere" \
    "$(job_step tempest "Run Tempest API tests")" \
    'TEMPEST_KEYSTONE_LEAK_CHECK: ${{ startsWith(matrix.service, '"'keystone'"') && '"'fail'"' || '"'off'"' }}'
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_ci_wrapper_defaults_to_off
test_ci_wrapper_passes_fail_on
test_ci_wrapper_keeps_the_lookup_free_of_it
test_local_wrapper_defaults_by_service
test_runner_rejects_an_unknown_value
test_runner_accepts_off_fail_empty_and_unset
test_ci_yaml_sets_fail_on_the_keystone_legs

echo ""
echo "Results: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
