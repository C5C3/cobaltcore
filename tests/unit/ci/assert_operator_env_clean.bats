#!/usr/bin/env bats
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
# Usage: bin/bats tests/unit/ci/assert_operator_env_clean.bats

load '../../lib/bats/common'
load '../../lib/bats/stubs'

# Each test function stays and is registered with bats_test_function in the
# order the old script called it: as @test blocks, git would no longer follow
# the history of this file (docs/contributing/shell-unit-tests.md).

SCRIPT="$PROJECT_ROOT/tests/e2e/lib/assert-operator-env-clean.sh"
IMAGE="ghcr.io/c5c3/keystone:2026.1"
POD="keystone-operator-7d9f8c6b5-abcde"

# The kubectl rules answer the five calls the script makes, and stub_calls
# kubectl lists every call. setup declares the answers of reset_stub, and a
# test declares the rules it changes after them, so that its own rule answers:
#   STUB_DEPLOY      the JSON `get deployment` prints
#   STUB_PODS        the JSON `get pods` prints
#   STUB_STATE       the ephemeral container's state `get pod` reports:
#                    terminated:<code>, waiting:<reason>, running, or
#                    running:<n>, which runs for <n> reads and then
#                    terminates with 0
#   STUB_ENVIRON     the lines `logs` prints
# A `get deployment` that fails with NotFound and a `logs` that fails are
# rules with --rc and --err; a `debug` that fails as on a cluster without
# ephemeral containers is a stub_debug. Every other call exits 0. A sleep
# stub beside it returns at once.

# stub_debug <answer>
# Declares the answer of `debug`: it writes the --container value to
# $STUB_DIR/state/container, where `get pod` reads it, and then runs the
# shell code <answer>.
stub_debug() {
  stub_on kubectl 'debug *' --exec 'prev=""
for arg in "$@"; do
  if [ "$prev" = "--container" ]; then
    printf "%s" "$arg" >"$STUB_DIR/state/container"
  fi
  prev="$arg"
done
'"$1"
}

# stub_state <state> [<stub_on option>...]
# Declares the state `get pod` reports, one of those of STUB_STATE, and
# reports the state under the name the last `debug` wrote. running:<n> is a
# lasting terminated:0 rule and a running rule with --times <n> after it.
stub_state() {
  local STUB_STATE="$1" state
  shift
  case "$STUB_STATE" in
    terminated:*) state="{\"terminated\":{\"exitCode\":${STUB_STATE#terminated:}}}" ;;
    waiting:*) state="{\"waiting\":{\"reason\":\"${STUB_STATE#waiting:}\",\"message\":\"stub message\"}}" ;;
    running:*)
      stub_state terminated:0
      stub_state running --times "${STUB_STATE#running:}"
      return
      ;;
    *) state='{"running":{"startedAt":"2026-10-20T10:00:00Z"}}' ;;
  esac
  stub_on kubectl 'get pod *' "$@" --exec "state='$state'"'
name="$(cat "$STUB_DIR/state/container" 2>/dev/null)"
printf "{\"status\":{\"ephemeralContainerStatuses\":[{\"name\":\"%s\",\"state\":%s}]}}\n" "$name" "$state"'
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
# CLEAN_ENVIRON. Called again inside a test, it declares these rules anew, so
# they answer again, and the kubectl call log and the container name start
# empty again, as before the first run.
reset_stub() {
  STUB_DEPLOY="$DEPLOY"
  STUB_PODS="$(pods_with "$CLEAN_SPEC")"
  STUB_STATE="terminated:0"
  STUB_ENVIRON="$CLEAN_ENVIRON"
  ALLOW_RE=""
  TIMEOUT=60
  rm -f "$STUB_DIR/calls/kubectl" "$STUB_DIR/state/container"
  stub_on kubectl '*'
  stub_on kubectl 'get deployment *' --out "$STUB_DEPLOY"
  stub_on kubectl 'get pods *' --out "$STUB_PODS"
  stub_state "$STUB_STATE"
  stub_debug 'echo "Targeting container \"manager\"." >&2'
  stub_on kubectl 'logs *' --out "$STUB_ENVIRON"
}

setup() {
  stub_on sleep '*'
  reset_stub
}

# run_check [args...]
# Runs the script with the stubs first on PATH, through run: its stdout in
# $output, its stderr in $stderr and its exit code in $status.
# Without arguments it runs the keystone suite's call.
run_check() {
  if [ "$#" -eq 0 ]; then
    set -- keystone-system keystone-operator "$IMAGE"
  fi
  run --separate-stderr with_stubs env \
      ENV_SCAN_ALLOW_NAME_RE="$ALLOW_RE" ENV_SCAN_TIMEOUT="$TIMEOUT" \
      bash "$SCRIPT" "$@"
}

# debug_call: the debug line of the call log.
debug_call() {
  stub_calls kubectl | grep '^debug ' || true
}

# --- Test 1: the happy path ---
test_a_clean_operator_passes() {
  run_check
  assert_eq "it exits 0" "0" "$status"
  assert_contains "it reads the pod of the Deployment" "$output" \
    "OK: deployment keystone-system/keystone-operator runs pod $POD"
  assert_contains "the pod spec passes" "$output" \
    "OK: the spec of pod keystone-system/$POD puts no Secret and no key-material name into env"
  assert_contains "the process environment passes with its count" "$output" \
    "OK: 5 process environment entries of keystone-system/$POD hold no key material"
  assert_not_contains "nothing fails, SSL_CERT_FILE and the service links included" "$output" "FAIL"
}
bats_test_function --description "a clean pod and a clean process environment pass" -- test_a_clean_operator_passes

# --- Test 2: the ephemeral container ---
test_the_debug_call_targets_the_manager() {
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
    "$(stub_calls kubectl | grep '^logs ')" "-c $(cat "$STUB_DIR/state/container")"
}
bats_test_function --description "the ephemeral container targets the manager under the restricted profile" -- test_the_debug_call_targets_the_manager

test_two_runs_use_two_container_names() {
  run_check
  local first second
  first="$(cat "$STUB_DIR/state/container")"
  run_check
  second="$(cat "$STUB_DIR/state/container")"
  assert_starts_with "the first name is env-scan-" "$first" "env-scan-"
  assert_starts_with "the second name is env-scan-" "$second" "env-scan-"
  if [ "$first" != "$second" ]; then
    check_passed "the names differ ($first, $second)"
  else
    fail "both runs named the container $first"
  fi
}
bats_test_function --description "two runs against the same pod add two ephemeral containers" -- test_two_runs_use_two_container_names

# --- Test 3: the pod ---
test_a_terminating_pod_is_skipped() {
  STUB_PODS='{"items":[
    {"metadata":{"name":"op-c"},"status":{"phase":"Running"},"spec":{"containers":[{"name":"manager"}]}},
    {"metadata":{"name":"op-a","deletionTimestamp":"2026-10-20T10:00:00Z"},"status":{"phase":"Running"},"spec":{"containers":[{"name":"manager"}]}},
    {"metadata":{"name":"op-b"},"status":{"phase":"Running"},"spec":{"containers":[{"name":"manager"}]}}]}'
  stub_on kubectl 'get pods *' --out "$STUB_PODS"
  run_check
  assert_eq "it exits 0" "0" "$status"
  assert_contains "it reads op-b" "$output" "runs pod op-b"
}
bats_test_function --description "the first Running pod by name is read, a terminating one skipped" -- test_a_terminating_pod_is_skipped

test_an_unreadable_deployment_fails() {
  stub_on kubectl 'get deployment *' --rc 1 --err 'Error from server (NotFound): deployments.apps "keystone-operator" not found'
  run_check
  assert_eq "it exits 1" "1" "$status"
  assert_contains "it names the Deployment and the error" "$output" \
    "FAIL: cannot read deployment keystone-system/keystone-operator: Error from server (NotFound)"
  assert_eq "it adds no ephemeral container" "" "$(debug_call)"
}
bats_test_function --description "a Deployment kubectl cannot read fails before any debug" -- test_an_unreadable_deployment_fails

test_a_deployment_without_a_selector_fails() {
  STUB_DEPLOY='{"spec":{}}'
  stub_on kubectl 'get deployment *' --out "$STUB_DEPLOY"
  run_check
  assert_eq "it exits 1" "1" "$status"
  assert_contains "it says so" "$output" \
    "FAIL: deployment keystone-system/keystone-operator has no spec.selector.matchLabels"
}
bats_test_function --description "a Deployment without matchLabels fails" -- test_a_deployment_without_a_selector_fails

test_no_running_pod_fails() {
  STUB_PODS="$(pods_with "$CLEAN_SPEC" Pending)"
  stub_on kubectl 'get pods *' --out "$STUB_PODS"
  run_check
  assert_eq "it exits 1" "1" "$status"
  assert_contains "it names the selector" "$output" \
    "FAIL: no Running pod of deployment keystone-system/keystone-operator (selector app.kubernetes.io/instance=keystone-operator,app.kubernetes.io/name=keystone-operator)"
  assert_eq "it adds no ephemeral container" "" "$(debug_call)"
}
bats_test_function --description "a Deployment without a Running pod fails" -- test_no_running_pod_fails

# --- Test 4: the pod spec ---
test_a_secret_key_ref_fails() {
  STUB_PODS="$(pods_with '{"containers":[{"name":"manager","env":[{"name":"DB_URL","valueFrom":{"secretKeyRef":{"name":"keystone-db","key":"url"}}}]}]}')"
  stub_on kubectl 'get pods *' --out "$STUB_PODS"
  run_check
  assert_eq "it exits 1" "1" "$status"
  assert_contains "it names the container, the entry and the Secret" "$output" \
    "FAIL: container manager: env DB_URL reads Secret keystone-db/url"
  assert_not_empty "it still adds the ephemeral container" "$(debug_call)"
  assert_contains "it still reports the clean process environment" "$output" \
    "OK: 5 process environment entries"
}
bats_test_function --description "env read from a Secret fails, and the runtime check still runs" -- test_a_secret_key_ref_fails

test_a_secret_named_like_key_material_is_reported_once() {
  STUB_PODS="$(pods_with '{"containers":[{"name":"manager","env":[{"name":"DB_PASSWORD","valueFrom":{"secretKeyRef":{"name":"keystone-db","key":"password"}}}]}]}')"
  stub_on kubectl 'get pods *' --out "$STUB_PODS"
  run_check
  assert_contains "it reports the Secret read" "$output" \
    "FAIL: container manager: env DB_PASSWORD reads Secret keystone-db/password"
  assert_eq "it reports it once" "1" "$(grep -c '^FAIL:' <<<"$output")"
}
bats_test_function --description "a Secret read into DB_PASSWORD is one finding" -- test_a_secret_named_like_key_material_is_reported_once

test_an_env_from_secret_ref_fails() {
  STUB_PODS="$(pods_with '{"containers":[{"name":"manager"}],"initContainers":[{"name":"setup","envFrom":[{"secretRef":{"name":"creds"}}]}]}')"
  stub_on kubectl 'get pods *' --out "$STUB_PODS"
  run_check
  assert_eq "it exits 1" "1" "$status"
  assert_contains "it names the init container and the Secret" "$output" \
    "FAIL: init container setup: envFrom reads Secret creds"
}
bats_test_function --description "envFrom a Secret fails, in an init container too" -- test_an_env_from_secret_ref_fails

test_a_literal_key_material_name_fails() {
  STUB_PODS="$(pods_with '{"containers":[{"name":"manager","env":[{"name":"OS_PASSWORD","value":"'"$SECRET_VALUE"'"}]}]}')"
  stub_on kubectl 'get pods *' --out "$STUB_PODS"
  run_check
  assert_eq "it exits 1" "1" "$status"
  assert_contains "it names the container and the entry" "$output" \
    "FAIL: container manager: env OS_PASSWORD is named like key material"
  assert_not_contains "it never prints the value" "$output" "$SECRET_VALUE"
}
bats_test_function --description "a literal env entry named like key material fails" -- test_a_literal_key_material_name_fails

test_an_ephemeral_container_is_checked() {
  STUB_PODS="$(pods_with '{"containers":[{"name":"manager"}],"ephemeralContainers":[{"name":"debugger","env":[{"name":"API_TOKEN","value":"x"}]}]}')"
  stub_on kubectl 'get pods *' --out "$STUB_PODS"
  run_check
  assert_contains "it names the ephemeral container" "$output" \
    "FAIL: ephemeral container debugger: env API_TOKEN is named like key material"
}
bats_test_function --description "an ephemeral container of an earlier debug session is checked too" -- test_an_ephemeral_container_is_checked

test_an_allowlisted_name_passes() {
  STUB_PODS="$(pods_with '{"containers":[{"name":"manager","env":[{"name":"OS_PASSWORD","value":"not-a-secret"}]}]}')"
  STUB_ENVIRON="$CLEAN_ENVIRON"$'\n''OS_PASSWORD=not-a-secret'
  ALLOW_RE='^OS_PASSWORD$'
  stub_on kubectl 'get pods *' --out "$STUB_PODS"
  stub_on kubectl 'logs *' --out "$STUB_ENVIRON"
  run_check
  assert_eq "it exits 0" "0" "$status"
  assert_contains "the pod spec passes" "$output" "OK: the spec of pod"
  assert_contains "the process environment passes" "$output" \
    "OK: 6 process environment entries"

  STUB_PODS="$(pods_with '{"containers":[{"name":"manager","env":[{"name":"OS_PASSWORD","valueFrom":{"configMapKeyRef":{"name":"cm","key":"pw"}}}]}]}')"
  stub_on kubectl 'get pods *' --out "$STUB_PODS"
  run_check
  assert_eq "without a literal value it exits 1" "1" "$status"
  assert_contains "without a literal value the pod spec fails" "$output" \
    "FAIL: container manager: env OS_PASSWORD is named like key material"
}
bats_test_function --description "an allowlisted name passes with a literal value, not without" -- test_an_allowlisted_name_passes

# --- Test 5: the ephemeral container's lifecycle ---
test_a_refused_debug_fails() {
  stub_debug 'echo "error: ephemeral containers are disabled for this cluster" >&2
exit 1'
  run_check
  assert_eq "it exits 1" "1" "$status"
  assert_contains "it prints kubectl's error" "$output" \
    "ephemeral containers are disabled for this cluster"
  assert_contains "it says the debug failed" "$output" "FAIL: kubectl debug could not add env-scan-"
}
bats_test_function --description "a debug kubectl refuses fails with its output" -- test_a_refused_debug_fails

test_a_container_that_cannot_start_fails() {
  STUB_STATE="waiting:ErrImageNeverPull"
  stub_state "$STUB_STATE"
  run_check
  assert_eq "it exits 1" "1" "$status"
  assert_contains "it names the reason" "$output" "cannot start: ErrImageNeverPull (stub message)"
  assert_eq "it reads no log" "" "$(stub_calls kubectl | grep '^logs ' || true)"
}
bats_test_function --description "an ephemeral container whose image is not on the node fails at once" -- test_a_container_that_cannot_start_fails

test_a_container_still_running_at_the_timeout_fails() {
  STUB_STATE="running"
  stub_state "$STUB_STATE"
  TIMEOUT=0
  run_check
  assert_eq "it exits 1" "1" "$status"
  assert_contains "it names the timeout and the state" "$output" \
    "did not terminate within 0s; its state: {\"running\""
}
bats_test_function --description "an ephemeral container still running at the timeout fails" -- test_a_container_still_running_at_the_timeout_fails

test_a_container_that_runs_first_is_awaited() {
  STUB_STATE="running:2"
  stub_state "$STUB_STATE"
  run_check
  assert_eq "it exits 0" "0" "$status"
  assert_eq "it read the state three times" "3" "$(stub_calls kubectl | grep -c '^get pod ')"
  assert_contains "it reads the log after the container terminated" "$output" \
    "OK: 5 process environment entries"
}
bats_test_function --description "a container that runs for two reads is awaited until it terminates" -- test_a_container_that_runs_first_is_awaited

test_a_non_zero_exit_fails() {
  STUB_STATE="terminated:126"
  stub_state "$STUB_STATE"
  run_check
  assert_eq "it exits 1" "1" "$status"
  assert_contains "it names the exit code" "$output" "exited 126"
}
bats_test_function --description "an ephemeral container that exits non-zero fails" -- test_a_non_zero_exit_fails

test_an_unreadable_log_fails() {
  stub_on kubectl 'logs *' --rc 1 --err 'Error from server (BadRequest): container not found'
  run_check
  assert_eq "it exits 1" "1" "$status"
  assert_contains "it prints kubectl's error" "$output" \
    "FAIL: cannot read the log of env-scan-"
  assert_contains "the error is kubectl's" "$output" "container not found"
}
bats_test_function --description "a log kubectl cannot read fails" -- test_an_unreadable_log_fails

# --- Test 6: the process environment ---
test_an_empty_environment_fails() {
  STUB_ENVIRON=""
  stub_on kubectl 'logs *' --out "$STUB_ENVIRON"
  run_check
  assert_eq "it exits 1" "1" "$status"
  assert_contains "it says it read nothing" "$output" \
    "FAIL: read no process environment of pod keystone-system/$POD"
}
bats_test_function --description "an empty process environment fails" -- test_an_empty_environment_fails

test_a_key_material_variable_fails_without_its_value() {
  STUB_ENVIRON="$CLEAN_ENVIRON"$'\n'"DB_PASSWORD=$SECRET_VALUE"
  stub_on kubectl 'logs *' --out "$STUB_ENVIRON"
  run_check
  assert_eq "it exits 1" "1" "$status"
  assert_contains "it names the variable" "$output" \
    "FAIL: the process environment holds DB_PASSWORD, which is named like key material"
  assert_not_contains "it never prints the value" "$output" "$SECRET_VALUE"
  assert_not_contains "stderr does not print it either" "$stderr" "$SECRET_VALUE"
  assert_not_contains "no OK line for the process environment" "$output" "hold no key material"
}
bats_test_function --description "a variable named like key material fails, its value unprinted" -- test_a_key_material_variable_fails_without_its_value

test_names_with_a_dash_or_a_dot_are_checked() {
  STUB_ENVIRON="$CLEAN_ENVIRON"$'\n'"CACHE-REDIS_PASSWORD=$SECRET_VALUE"$'\n'"redis.auth_TOKEN=$SECRET_VALUE"
  stub_on kubectl 'logs *' --out "$STUB_ENVIRON"
  run_check
  assert_eq "it exits 1" "1" "$status"
  assert_contains "it names the dashed variable" "$output" \
    "FAIL: the process environment holds CACHE-REDIS_PASSWORD, which is named like key material"
  assert_contains "it names the dotted variable" "$output" \
    "FAIL: the process environment holds redis.auth_TOKEN, which is named like key material"
  assert_not_contains "it never prints the value" "$output" "$SECRET_VALUE"

  reset_stub
  STUB_ENVIRON="$CLEAN_ENVIRON"$'\n''log.level=debug'
  stub_on kubectl 'logs *' --out "$STUB_ENVIRON"
  run_check
  assert_eq "a clean dotted name passes" "0" "$status"
  assert_contains "a clean dotted name is counted" "$output" "OK: 6 process environment entries"
}
bats_test_function --description "names with - or . are counted and checked like the pod spec checks them" -- test_names_with_a_dash_or_a_dot_are_checked

test_a_pem_value_fails() {
  STUB_ENVIRON="$CLEAN_ENVIRON"$'\n''TLS_BUNDLE=-----BEGIN PRIVATE KEY-----'$'\n'"$SECRET_VALUE"$'\n''-----END PRIVATE KEY-----'$'\n''CA_BUNDLE=x'$'\n''-----BEGIN CERTIFICATE-----'
  stub_on kubectl 'logs *' --out "$STUB_ENVIRON"
  run_check
  assert_eq "it exits 1" "1" "$status"
  assert_contains "it names the variable whose value starts the PEM block" "$output" \
    "FAIL: the value of TLS_BUNDLE in the process environment looks like key material"
  assert_contains "it reports a continuation line without a name" "$output" \
    "FAIL: a continuation line of a multi-line value in the process environment looks like key material"
  assert_not_contains "it never prints the value" "$output" "$SECRET_VALUE"
  assert_eq "the two BEGIN lines are the only findings" "2" "$(grep -c '^FAIL:' <<<"$output")"
}
bats_test_function --description "a PEM value fails, on its first line and on a continuation line" -- test_a_pem_value_fails

test_a_password_in_a_value_fails() {
  STUB_ENVIRON="$CLEAN_ENVIRON"$'\n'"DATABASE_URL=mysql+pymysql://keystone@db/keystone?password=$SECRET_VALUE"
  stub_on kubectl 'logs *' --out "$STUB_ENVIRON"
  run_check
  assert_eq "it exits 1" "1" "$status"
  assert_contains "it names the variable" "$output" \
    "FAIL: the value of DATABASE_URL in the process environment looks like key material"
  assert_not_contains "it never prints the value" "$output" "$SECRET_VALUE"
}
bats_test_function --description "a value carrying password= fails" -- test_a_password_in_a_value_fails

test_an_allowlisted_variable_passes_at_runtime() {
  STUB_ENVIRON="$CLEAN_ENVIRON"$'\n''FEATURE_KEY=on'
  ALLOW_RE='^FEATURE_KEY$'
  stub_on kubectl 'logs *' --out "$STUB_ENVIRON"
  run_check
  assert_eq "it exits 0" "0" "$status"
  assert_contains "it counts the variable" "$output" "OK: 6 process environment entries"
}
bats_test_function --description "an allowlisted variable passes the process environment check" -- test_an_allowlisted_variable_passes_at_runtime

# --- Test 7: arguments and settings ---
test_wrong_arguments_exit_2() {
  local args
  for args in "three empty" "two" "an empty deployment" "four"; do
    case "$args" in
      "three empty") run_check "" "" "" ;;
      two) run_check keystone-system keystone-operator ;;
      "an empty deployment") run_check keystone-system "" "$IMAGE" ;;
      four) run_check keystone-system keystone-operator "$IMAGE" extra ;;
    esac
    soft assert_eq "$args arguments exit 2" "2" "$status"
    soft assert_contains "$args arguments print the usage line" "$stderr" \
      "usage: assert-operator-env-clean.sh <namespace> <deployment> <debug-image>"
  done
  assert_eq "no kubectl call was made" "" "$(stub_calls kubectl)"
}
bats_test_function --description "anything but three non-empty arguments exits 2 with the usage line" -- test_wrong_arguments_exit_2

test_bad_settings_exit_2() {
  TIMEOUT='1; touch /tmp/pwned'
  run_check
  assert_eq "a non-numeric timeout exits 2" "2" "$status"
  assert_contains "it names the setting" "$stderr" "ENV_SCAN_TIMEOUT must be a whole number"

  local leading
  for leading in 08 010; do
    reset_stub
    TIMEOUT="$leading"
    run_check
    soft assert_eq "a timeout of $leading exits 2" "2" "$status"
    soft assert_eq "a timeout of $leading adds no ephemeral container" "" "$(debug_call)"
  done

  reset_stub
  ALLOW_RE='('
  run_check
  assert_eq "a regex jq cannot compile exits 2" "2" "$status"
  assert_contains "it names the setting" "$stderr" \
    "ENV_SCAN_ALLOW_NAME_RE is not a regex jq can compile"
  assert_eq "no kubectl call was made" "" "$(stub_calls kubectl)"
}
bats_test_function --description "a timeout that is not a whole number or a broken regex exits 2" -- test_bad_settings_exit_2

test_the_script_parses() {
  if bash -n "$SCRIPT"; then
    check_passed "bash -n accepts the script"
  else
    fail "bash -n rejects the script"
  fi
  assert_file_contains_fixed "it carries the license header" "$SCRIPT" \
    "SPDX-License-Identifier: Apache-2.0"
  assert_file_contains_fixed "the default deny regex leaves CERT out" "$SCRIPT" \
    "default_deny_name='(^|_)(PASSWORD|PASSWD|SECRET|TOKEN|KEY|CREDENTIALS?|FERNET)(_|\$)'"
}
bats_test_function --description "the script parses and carries the SPDX header" -- test_the_script_parses
