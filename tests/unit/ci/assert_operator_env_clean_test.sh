#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify tests/e2e/lib/assert-operator-env-clean.sh, the scan every
# env-key-material suite runs against its operator:
#   - a clean pod and a clean process environment pass with the count of
#     entries; SSL_CERT_FILE, which every operator image sets, and the service
#     links Kubernetes injects (KEYSTONE_OPERATOR_SERVICE_HOST) pass
#   - the ephemeral container targets the manager container, runs under the
#     restricted profile, never pulls, uses the debug image it was given, and
#     two runs against the same pod use two container names
#   - the read skips exactly the processes of its own mount namespace, and the
#     script polls until a container that runs for a while terminates
#   - the pod is the first Running pod by name; a terminating pod is skipped
#   - each check fails on its own evidence: a Deployment kubectl cannot read
#     or without a selector, no Running pod, a Secret read through env or
#     envFrom (in an init container too), a literal env entry named like key
#     material, a debug kubectl refuses, an ephemeral container that cannot
#     start, still runs at the timeout or exits non-zero, a log kubectl cannot
#     read, an empty process environment, a variable named like key material
#     (a name with - or . included), a value that looks like key material and
#     a continuation line of a multi-line value that does
#   - a finding never prints the value it found
#   - an allowlisted name passes when it carries a literal value, and fails in
#     the pod spec when it does not
#   - the runtime check still runs after the static check found something
#   - anything but three non-empty arguments exits 2 with the usage line, and
#     so does a timeout that is not a whole number, one with a leading zero
#     (which bash arithmetic reads as octal) or a regex jq cannot compile
#
# Usage: bash tests/unit/ci/assert_operator_env_clean_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

SCRIPT="$PROJECT_ROOT/tests/e2e/lib/assert-operator-env-clean.sh"
IMAGE="ghcr.io/c5c3/keystone:2026.1"
POD="keystone-operator-7d9f8c6b5-abcde"

STUBS="$(mktemp -d)"
trap 'rm -rf "$STUBS"' EXIT

# make_kubectl_stub <dir>
# Writes a kubectl stub that answers the five calls the script makes, appends
# every call to <dir>/calls.log and reads its behaviour from the environment:
#   STUB_DEPLOY      the JSON `get deployment` prints
#   STUB_DEPLOY_RC   non-zero: `get deployment` fails with NotFound
#   STUB_PODS        the JSON `get pods` prints
#   STUB_DEBUG_RC    non-zero: `debug` fails as on a cluster without ephemeral
#                    containers
#   STUB_STATE       the ephemeral container's state `get pod` reports:
#                    terminated:<code>, waiting:<reason>, running, or
#                    running:<n>, which runs for <n> reads and then
#                    terminates with 0
#   STUB_ENVIRON     the lines `logs` prints
#   STUB_LOGS_RC     non-zero: `logs` fails
# `debug` writes the --container value to <dir>/container, and `get pod`
# reports the state under that name.
make_kubectl_stub() {
  local dir="$1"

  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
dir="$(dirname "$0")"
printf '%s\n' "$*" >>"$dir/calls.log"
case "$1 $2" in
  "get deployment")
    if [ "${STUB_DEPLOY_RC:-0}" -ne 0 ]; then
      echo "Error from server (NotFound): deployments.apps \"$3\" not found" >&2
      exit 1
    fi
    printf '%s\n' "$STUB_DEPLOY"
    ;;
  "get pods")
    printf '%s\n' "$STUB_PODS"
    ;;
  "get pod")
    name="$(cat "$dir/container" 2>/dev/null)"
    case "$STUB_STATE" in
      terminated:*) state="{\"terminated\":{\"exitCode\":${STUB_STATE#terminated:}}}" ;;
      waiting:*) state="{\"waiting\":{\"reason\":\"${STUB_STATE#waiting:}\",\"message\":\"stub message\"}}" ;;
      running:*)
        polls="$(cat "$dir/polls" 2>/dev/null || echo 0)"
        polls=$((polls + 1))
        echo "$polls" >"$dir/polls"
        if [ "$polls" -le "${STUB_STATE#running:}" ]; then
          state='{"running":{"startedAt":"2026-10-20T10:00:00Z"}}'
        else
          state='{"terminated":{"exitCode":0}}'
        fi
        ;;
      *) state='{"running":{"startedAt":"2026-10-20T10:00:00Z"}}' ;;
    esac
    printf '{"status":{"ephemeralContainerStatuses":[{"name":"%s","state":%s}]}}\n' "$name" "$state"
    ;;
  "debug "*)
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--container" ]; then
        printf '%s' "$arg" >"$dir/container"
      fi
      prev="$arg"
    done
    if [ "${STUB_DEBUG_RC:-0}" -ne 0 ]; then
      echo "error: ephemeral containers are disabled for this cluster" >&2
      exit 1
    fi
    echo "Targeting container \"manager\"." >&2
    ;;
  "logs "*)
    if [ "${STUB_LOGS_RC:-0}" -ne 0 ]; then
      echo "Error from server (BadRequest): container not found" >&2
      exit 1
    fi
    if [ -n "$STUB_ENVIRON" ]; then printf '%s\n' "$STUB_ENVIRON"; fi
    ;;
esac
exit 0
STUB
  chmod +x "$dir/kubectl"
  printf '#!/bin/bash\nexit 0\n' >"$dir/sleep"
  chmod +x "$dir/sleep"
}

DEPLOY='{"spec":{"selector":{"matchLabels":{"app.kubernetes.io/instance":"keystone-operator","app.kubernetes.io/name":"keystone-operator"}}}}'

# pods_with <spec-json> [phase]
# One pod named $POD in <phase> (default Running) with <spec-json> as its spec.
pods_with() {
  printf '{"items":[{"metadata":{"name":"%s"},"status":{"phase":"%s"},"spec":%s}]}' \
    "$POD" "${2:-Running}" "$1"
}

CLEAN_SPEC='{"containers":[{"name":"manager","env":[{"name":"POD_NAMESPACE","valueFrom":{"fieldRef":{"fieldPath":"metadata.namespace"}}}]}]}'
CLEAN_ENVIRON='PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
HOSTNAME=keystone-operator-7d9f8c6b5-abcde
KEYSTONE_OPERATOR_SERVICE_HOST=10.96.12.34
KUBERNETES_SERVICE_HOST=10.96.0.1'
SECRET_VALUE="hunter2-probe-value"

# reset_stub
# A clean keystone-operator: one Running pod whose spec reads no Secret, an
# ephemeral container that terminates with 0, and the five entries of
# CLEAN_ENVIRON.
reset_stub() {
  STUB_DEPLOY="$DEPLOY"
  STUB_DEPLOY_RC=0
  STUB_PODS="$(pods_with "$CLEAN_SPEC")"
  STUB_DEBUG_RC=0
  STUB_STATE="terminated:0"
  STUB_ENVIRON="$CLEAN_ENVIRON"
  STUB_LOGS_RC=0
  ALLOW_RE=""
  TIMEOUT=60
  rm -f "$STUBS/calls.log" "$STUBS/container" "$STUBS/polls"
}

# run_check [args...]
# Runs the script with the kubectl stub first on PATH. Leaves its stdout in
# RUN_OUT and its exit code in RUN_RC, and writes its stderr to $STUBS/stderr.
# Without arguments it runs the keystone suite's call.
run_check() {
  if [ "$#" -eq 0 ]; then
    set -- keystone-system keystone-operator "$IMAGE"
  fi
  RUN_OUT="$(
    PATH="$STUBS:$PATH" \
      STUB_DEPLOY="$STUB_DEPLOY" STUB_DEPLOY_RC="$STUB_DEPLOY_RC" \
      STUB_PODS="$STUB_PODS" STUB_DEBUG_RC="$STUB_DEBUG_RC" \
      STUB_STATE="$STUB_STATE" STUB_ENVIRON="$STUB_ENVIRON" \
      STUB_LOGS_RC="$STUB_LOGS_RC" \
      ENV_SCAN_ALLOW_NAME_RE="$ALLOW_RE" ENV_SCAN_TIMEOUT="$TIMEOUT" \
      bash "$SCRIPT" "$@" 2>"$STUBS/stderr"
  )"
  RUN_RC=$?
}

# debug_call: the debug line of the call log.
debug_call() {
  grep '^debug ' "$STUBS/calls.log" 2>/dev/null || true
}

make_kubectl_stub "$STUBS"

# --- Test 1: the happy path ---
test_a_clean_operator_passes() {
  echo "Test: a clean pod and a clean process environment pass"
  reset_stub
  run_check
  assert_eq "it exits 0" "0" "$RUN_RC"
  assert_contains "it reads the pod of the Deployment" "$RUN_OUT" \
    "OK: deployment keystone-system/keystone-operator runs pod $POD"
  assert_contains "the pod spec passes" "$RUN_OUT" \
    "OK: the spec of pod keystone-system/$POD puts no Secret and no key-material name into env"
  assert_contains "the process environment passes with its count" "$RUN_OUT" \
    "OK: 5 process environment entries of keystone-system/$POD hold no key material"
  assert_not_contains "nothing fails, SSL_CERT_FILE and the service links included" "$RUN_OUT" "FAIL"
}

# --- Test 2: the ephemeral container ---
test_the_debug_call_targets_the_manager() {
  echo "Test: the ephemeral container targets the manager under the restricted profile"
  reset_stub
  run_check
  local call
  call="$(debug_call)"
  assert_contains "it debugs the pod in the namespace" "$call" "debug $POD -n keystone-system"
  assert_contains "it uses the debug image it was given" "$call" "--image $IMAGE"
  assert_contains "it never pulls" "$call" "--image-pull-policy=Never"
  assert_contains "it joins the manager's process namespace" "$call" "--target=manager"
  assert_contains "it runs under the restricted profile" "$call" "--profile=restricted"
  assert_contains "it runs the read in sh" "$call" "-- sh -c "
  assert_contains "the read takes its own mount namespace" "$call" 'me=$(readlink /proc/self/ns/mnt);'
  assert_contains "the read skips exactly the processes of that namespace" "$call" \
    '[ "$(readlink "$p/ns/mnt" 2>/dev/null)" = "$me" ] && continue;'
  assert_contains "the read prints the environ of every other process" "$call" \
    '[ -r "$p/environ" ] && tr "\0" "\n" < "$p/environ";'
  assert_not_contains "the read does not use \$\$, which Kubernetes reduces to \$" "$call" '$$'
  assert_contains "it reads the log of its own container" \
    "$(grep '^logs ' "$STUBS/calls.log")" "-c $(cat "$STUBS/container")"
}

test_two_runs_use_two_container_names() {
  echo "Test: two runs against the same pod add two ephemeral containers"
  reset_stub
  run_check
  local first second
  first="$(cat "$STUBS/container")"
  run_check
  second="$(cat "$STUBS/container")"
  assert_starts_with "the first name is env-scan-" "$first" "env-scan-"
  assert_starts_with "the second name is env-scan-" "$second" "env-scan-"
  if [ "$first" != "$second" ]; then
    echo "  PASS: the names differ ($first, $second)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: both runs named the container $first"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 3: the pod ---
test_a_terminating_pod_is_skipped() {
  echo "Test: the first Running pod by name is read, a terminating one skipped"
  reset_stub
  STUB_PODS='{"items":[
    {"metadata":{"name":"op-c"},"status":{"phase":"Running"},"spec":{"containers":[{"name":"manager"}]}},
    {"metadata":{"name":"op-a","deletionTimestamp":"2026-10-20T10:00:00Z"},"status":{"phase":"Running"},"spec":{"containers":[{"name":"manager"}]}},
    {"metadata":{"name":"op-b"},"status":{"phase":"Running"},"spec":{"containers":[{"name":"manager"}]}}]}'
  run_check
  assert_eq "it exits 0" "0" "$RUN_RC"
  assert_contains "it reads op-b" "$RUN_OUT" "runs pod op-b"
}

test_an_unreadable_deployment_fails() {
  echo "Test: a Deployment kubectl cannot read fails before any debug"
  reset_stub
  STUB_DEPLOY_RC=1
  run_check
  assert_eq "it exits 1" "1" "$RUN_RC"
  assert_contains "it names the Deployment and the error" "$RUN_OUT" \
    "FAIL: cannot read deployment keystone-system/keystone-operator: Error from server (NotFound)"
  assert_eq "it adds no ephemeral container" "" "$(debug_call)"
}

test_a_deployment_without_a_selector_fails() {
  echo "Test: a Deployment without matchLabels fails"
  reset_stub
  STUB_DEPLOY='{"spec":{}}'
  run_check
  assert_eq "it exits 1" "1" "$RUN_RC"
  assert_contains "it says so" "$RUN_OUT" \
    "FAIL: deployment keystone-system/keystone-operator has no spec.selector.matchLabels"
}

test_no_running_pod_fails() {
  echo "Test: a Deployment without a Running pod fails"
  reset_stub
  STUB_PODS="$(pods_with "$CLEAN_SPEC" Pending)"
  run_check
  assert_eq "it exits 1" "1" "$RUN_RC"
  assert_contains "it names the selector" "$RUN_OUT" \
    "FAIL: no Running pod of deployment keystone-system/keystone-operator (selector app.kubernetes.io/instance=keystone-operator,app.kubernetes.io/name=keystone-operator)"
  assert_eq "it adds no ephemeral container" "" "$(debug_call)"
}

# --- Test 4: the pod spec ---
test_a_secret_key_ref_fails() {
  echo "Test: env read from a Secret fails, and the runtime check still runs"
  reset_stub
  STUB_PODS="$(pods_with '{"containers":[{"name":"manager","env":[{"name":"DB_URL","valueFrom":{"secretKeyRef":{"name":"keystone-db","key":"url"}}}]}]}')"
  run_check
  assert_eq "it exits 1" "1" "$RUN_RC"
  assert_contains "it names the container, the entry and the Secret" "$RUN_OUT" \
    "FAIL: container manager: env DB_URL reads Secret keystone-db/url"
  assert_not_empty "it still adds the ephemeral container" "$(debug_call)"
  assert_contains "it still reports the clean process environment" "$RUN_OUT" \
    "OK: 5 process environment entries"
}

test_a_secret_named_like_key_material_is_reported_once() {
  echo "Test: a Secret read into DB_PASSWORD is one finding"
  reset_stub
  STUB_PODS="$(pods_with '{"containers":[{"name":"manager","env":[{"name":"DB_PASSWORD","valueFrom":{"secretKeyRef":{"name":"keystone-db","key":"password"}}}]}]}')"
  run_check
  assert_contains "it reports the Secret read" "$RUN_OUT" \
    "FAIL: container manager: env DB_PASSWORD reads Secret keystone-db/password"
  assert_eq "it reports it once" "1" "$(grep -c '^FAIL:' <<<"$RUN_OUT")"
}

test_an_env_from_secret_ref_fails() {
  echo "Test: envFrom a Secret fails, in an init container too"
  reset_stub
  STUB_PODS="$(pods_with '{"containers":[{"name":"manager"}],"initContainers":[{"name":"setup","envFrom":[{"secretRef":{"name":"creds"}}]}]}')"
  run_check
  assert_eq "it exits 1" "1" "$RUN_RC"
  assert_contains "it names the init container and the Secret" "$RUN_OUT" \
    "FAIL: init container setup: envFrom reads Secret creds"
}

test_a_literal_key_material_name_fails() {
  echo "Test: a literal env entry named like key material fails"
  reset_stub
  STUB_PODS="$(pods_with '{"containers":[{"name":"manager","env":[{"name":"OS_PASSWORD","value":"'"$SECRET_VALUE"'"}]}]}')"
  run_check
  assert_eq "it exits 1" "1" "$RUN_RC"
  assert_contains "it names the container and the entry" "$RUN_OUT" \
    "FAIL: container manager: env OS_PASSWORD is named like key material"
  assert_not_contains "it never prints the value" "$RUN_OUT" "$SECRET_VALUE"
}

test_an_ephemeral_container_is_checked() {
  echo "Test: an ephemeral container of an earlier debug session is checked too"
  reset_stub
  STUB_PODS="$(pods_with '{"containers":[{"name":"manager"}],"ephemeralContainers":[{"name":"debugger","env":[{"name":"API_TOKEN","value":"x"}]}]}')"
  run_check
  assert_contains "it names the ephemeral container" "$RUN_OUT" \
    "FAIL: ephemeral container debugger: env API_TOKEN is named like key material"
}

test_an_allowlisted_name_passes() {
  echo "Test: an allowlisted name passes with a literal value, not without"
  reset_stub
  STUB_PODS="$(pods_with '{"containers":[{"name":"manager","env":[{"name":"OS_PASSWORD","value":"not-a-secret"}]}]}')"
  STUB_ENVIRON="$CLEAN_ENVIRON"$'\n''OS_PASSWORD=not-a-secret'
  ALLOW_RE='^OS_PASSWORD$'
  run_check
  assert_eq "it exits 0" "0" "$RUN_RC"
  assert_contains "the pod spec passes" "$RUN_OUT" "OK: the spec of pod"
  assert_contains "the process environment passes" "$RUN_OUT" \
    "OK: 6 process environment entries"

  STUB_PODS="$(pods_with '{"containers":[{"name":"manager","env":[{"name":"OS_PASSWORD","valueFrom":{"configMapKeyRef":{"name":"cm","key":"pw"}}}]}]}')"
  run_check
  assert_eq "without a literal value it exits 1" "1" "$RUN_RC"
  assert_contains "without a literal value the pod spec fails" "$RUN_OUT" \
    "FAIL: container manager: env OS_PASSWORD is named like key material"
}

# --- Test 5: the ephemeral container's lifecycle ---
test_a_refused_debug_fails() {
  echo "Test: a debug kubectl refuses fails with its output"
  reset_stub
  STUB_DEBUG_RC=1
  run_check
  assert_eq "it exits 1" "1" "$RUN_RC"
  assert_contains "it prints kubectl's error" "$RUN_OUT" \
    "ephemeral containers are disabled for this cluster"
  assert_contains "it says the debug failed" "$RUN_OUT" "FAIL: kubectl debug could not add env-scan-"
}

test_a_container_that_cannot_start_fails() {
  echo "Test: an ephemeral container whose image is not on the node fails at once"
  reset_stub
  STUB_STATE="waiting:ErrImageNeverPull"
  run_check
  assert_eq "it exits 1" "1" "$RUN_RC"
  assert_contains "it names the reason" "$RUN_OUT" "cannot start: ErrImageNeverPull (stub message)"
  assert_eq "it reads no log" "" "$(grep '^logs ' "$STUBS/calls.log" || true)"
}

test_a_container_still_running_at_the_timeout_fails() {
  echo "Test: an ephemeral container still running at the timeout fails"
  reset_stub
  STUB_STATE="running"
  TIMEOUT=0
  run_check
  assert_eq "it exits 1" "1" "$RUN_RC"
  assert_contains "it names the timeout and the state" "$RUN_OUT" \
    "did not terminate within 0s; its state: {\"running\""
}

test_a_container_that_runs_first_is_awaited() {
  echo "Test: a container that runs for two reads is awaited until it terminates"
  reset_stub
  STUB_STATE="running:2"
  run_check
  assert_eq "it exits 0" "0" "$RUN_RC"
  assert_eq "it read the state three times" "3" "$(grep -c '^get pod ' "$STUBS/calls.log")"
  assert_contains "it reads the log after the container terminated" "$RUN_OUT" \
    "OK: 5 process environment entries"
}

test_a_non_zero_exit_fails() {
  echo "Test: an ephemeral container that exits non-zero fails"
  reset_stub
  STUB_STATE="terminated:126"
  run_check
  assert_eq "it exits 1" "1" "$RUN_RC"
  assert_contains "it names the exit code" "$RUN_OUT" "exited 126"
}

test_an_unreadable_log_fails() {
  echo "Test: a log kubectl cannot read fails"
  reset_stub
  STUB_LOGS_RC=1
  run_check
  assert_eq "it exits 1" "1" "$RUN_RC"
  assert_contains "it prints kubectl's error" "$RUN_OUT" \
    "FAIL: cannot read the log of env-scan-"
  assert_contains "the error is kubectl's" "$RUN_OUT" "container not found"
}

# --- Test 6: the process environment ---
test_an_empty_environment_fails() {
  echo "Test: an empty process environment fails"
  reset_stub
  STUB_ENVIRON=""
  run_check
  assert_eq "it exits 1" "1" "$RUN_RC"
  assert_contains "it says it read nothing" "$RUN_OUT" \
    "FAIL: read no process environment of pod keystone-system/$POD"
}

test_a_key_material_variable_fails_without_its_value() {
  echo "Test: a variable named like key material fails, its value unprinted"
  reset_stub
  STUB_ENVIRON="$CLEAN_ENVIRON"$'\n'"DB_PASSWORD=$SECRET_VALUE"
  run_check
  assert_eq "it exits 1" "1" "$RUN_RC"
  assert_contains "it names the variable" "$RUN_OUT" \
    "FAIL: the process environment holds DB_PASSWORD, which is named like key material"
  assert_not_contains "it never prints the value" "$RUN_OUT" "$SECRET_VALUE"
  assert_not_contains "stderr does not print it either" "$(cat "$STUBS/stderr")" "$SECRET_VALUE"
  assert_not_contains "no OK line for the process environment" "$RUN_OUT" "hold no key material"
}

test_names_with_a_dash_or_a_dot_are_checked() {
  echo "Test: names with - or . are counted and checked like the pod spec checks them"
  reset_stub
  STUB_ENVIRON="$CLEAN_ENVIRON"$'\n'"CACHE-REDIS_PASSWORD=$SECRET_VALUE"$'\n'"redis.auth_TOKEN=$SECRET_VALUE"
  run_check
  assert_eq "it exits 1" "1" "$RUN_RC"
  assert_contains "it names the dashed variable" "$RUN_OUT" \
    "FAIL: the process environment holds CACHE-REDIS_PASSWORD, which is named like key material"
  assert_contains "it names the dotted variable" "$RUN_OUT" \
    "FAIL: the process environment holds redis.auth_TOKEN, which is named like key material"
  assert_not_contains "it never prints the value" "$RUN_OUT" "$SECRET_VALUE"

  reset_stub
  STUB_ENVIRON="$CLEAN_ENVIRON"$'\n''log.level=debug'
  run_check
  assert_eq "a clean dotted name passes" "0" "$RUN_RC"
  assert_contains "a clean dotted name is counted" "$RUN_OUT" "OK: 6 process environment entries"
}

test_a_pem_value_fails() {
  echo "Test: a PEM value fails, on its first line and on a continuation line"
  reset_stub
  STUB_ENVIRON="$CLEAN_ENVIRON"$'\n''TLS_BUNDLE=-----BEGIN PRIVATE KEY-----'$'\n'"$SECRET_VALUE"$'\n''-----END PRIVATE KEY-----'$'\n''CA_BUNDLE=x'$'\n''-----BEGIN CERTIFICATE-----'
  run_check
  assert_eq "it exits 1" "1" "$RUN_RC"
  assert_contains "it names the variable whose value starts the PEM block" "$RUN_OUT" \
    "FAIL: the value of TLS_BUNDLE in the process environment looks like key material"
  assert_contains "it reports a continuation line without a name" "$RUN_OUT" \
    "FAIL: a continuation line of a multi-line value in the process environment looks like key material"
  assert_not_contains "it never prints the value" "$RUN_OUT" "$SECRET_VALUE"
  assert_eq "the two BEGIN lines are the only findings" "2" "$(grep -c '^FAIL:' <<<"$RUN_OUT")"
}

test_a_password_in_a_value_fails() {
  echo "Test: a value carrying password= fails"
  reset_stub
  STUB_ENVIRON="$CLEAN_ENVIRON"$'\n'"DATABASE_URL=mysql+pymysql://keystone@db/keystone?password=$SECRET_VALUE"
  run_check
  assert_eq "it exits 1" "1" "$RUN_RC"
  assert_contains "it names the variable" "$RUN_OUT" \
    "FAIL: the value of DATABASE_URL in the process environment looks like key material"
  assert_not_contains "it never prints the value" "$RUN_OUT" "$SECRET_VALUE"
}

test_an_allowlisted_variable_passes_at_runtime() {
  echo "Test: an allowlisted variable passes the process environment check"
  reset_stub
  STUB_ENVIRON="$CLEAN_ENVIRON"$'\n''FEATURE_KEY=on'
  ALLOW_RE='^FEATURE_KEY$'
  run_check
  assert_eq "it exits 0" "0" "$RUN_RC"
  assert_contains "it counts the variable" "$RUN_OUT" "OK: 6 process environment entries"
}

# --- Test 7: arguments and settings ---
test_wrong_arguments_exit_2() {
  echo "Test: anything but three non-empty arguments exits 2 with the usage line"
  reset_stub
  local args
  for args in "three empty" "two" "an empty deployment" "four"; do
    case "$args" in
      "three empty") run_check "" "" "" ;;
      two) run_check keystone-system keystone-operator ;;
      "an empty deployment") run_check keystone-system "" "$IMAGE" ;;
      four) run_check keystone-system keystone-operator "$IMAGE" extra ;;
    esac
    assert_eq "$args arguments exit 2" "2" "$RUN_RC"
    assert_contains "$args arguments print the usage line" "$(cat "$STUBS/stderr")" \
      "usage: assert-operator-env-clean.sh <namespace> <deployment> <debug-image>"
  done
  assert_eq "no kubectl call was made" "" "$(cat "$STUBS/calls.log" 2>/dev/null)"
}

test_bad_settings_exit_2() {
  echo "Test: a timeout that is not a whole number or a broken regex exits 2"
  reset_stub
  TIMEOUT='1; touch /tmp/pwned'
  run_check
  assert_eq "a non-numeric timeout exits 2" "2" "$RUN_RC"
  assert_contains "it names the setting" "$(cat "$STUBS/stderr")" "ENV_SCAN_TIMEOUT must be a whole number"

  local leading
  for leading in 08 010; do
    reset_stub
    TIMEOUT="$leading"
    run_check
    assert_eq "a timeout of $leading exits 2" "2" "$RUN_RC"
    assert_eq "a timeout of $leading adds no ephemeral container" "" "$(debug_call)"
  done

  reset_stub
  ALLOW_RE='('
  run_check
  assert_eq "a regex jq cannot compile exits 2" "2" "$RUN_RC"
  assert_contains "it names the setting" "$(cat "$STUBS/stderr")" \
    "ENV_SCAN_ALLOW_NAME_RE is not a regex jq can compile"
  assert_eq "no kubectl call was made" "" "$(cat "$STUBS/calls.log" 2>/dev/null)"
}

test_the_script_parses() {
  echo "Test: the script parses and carries the SPDX header"
  if bash -n "$SCRIPT"; then
    echo "  PASS: bash -n accepts the script"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: bash -n rejects the script"
    FAIL=$((FAIL + 1))
  fi
  assert_file_contains_fixed "it carries the license header" "$SCRIPT" \
    "SPDX-License-Identifier: Apache-2.0"
  assert_file_contains_fixed "the default deny regex leaves CERT out" "$SCRIPT" \
    "default_deny_name='(^|_)(PASSWORD|PASSWD|SECRET|TOKEN|KEY|CREDENTIALS?|FERNET)(_|\$)'"
}

# --- Run ---
test_a_clean_operator_passes
test_the_debug_call_targets_the_manager
test_two_runs_use_two_container_names
test_a_terminating_pod_is_skipped
test_an_unreadable_deployment_fails
test_a_deployment_without_a_selector_fails
test_no_running_pod_fails
test_a_secret_key_ref_fails
test_a_secret_named_like_key_material_is_reported_once
test_an_env_from_secret_ref_fails
test_a_literal_key_material_name_fails
test_an_ephemeral_container_is_checked
test_an_allowlisted_name_passes
test_a_refused_debug_fails
test_a_container_that_cannot_start_fails
test_a_container_still_running_at_the_timeout_fails
test_a_container_that_runs_first_is_awaited
test_a_non_zero_exit_fails
test_an_unreadable_log_fails
test_an_empty_environment_fails
test_a_key_material_variable_fails_without_its_value
test_names_with_a_dash_or_a_dot_are_checked
test_a_pem_value_fails
test_a_password_in_a_value_fails
test_an_allowlisted_variable_passes_at_runtime
test_wrong_arguments_exit_2
test_bad_settings_exit_2
test_the_script_parses

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
