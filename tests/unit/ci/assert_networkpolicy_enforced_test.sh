#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify tests/e2e/lib/assert-networkpolicy-enforced.sh, the probe the service
# network-policy suites run from the pods of their fixtures (#1530):
#   - a wrong argument count, an unknown expect, a missing or surplus port, a
#     malformed host, port or override exits 2 with the usage line, calls no
#     kubectl, and never runs a command a port or an override carries
#   - open passes at the first attempt that connects and fails with the last
#     result once the attempts run out, port by port
#   - dropped passes once a timeout is followed by the confirming timeouts,
#     polls on through connects and refusals before the first timeout, and
#     fails when no attempt times out or a later attempt does not, port by
#     port, and the first port that fails ends the run
#   - resolves passes with the first address, retries a lookup error, and
#     fails with the lookup error once the attempts run out
#   - a kubectl exec that cannot run the probe fails with kubectl's last line
#   - host and ports reach the program as arguments after its text, never in
#     it, and the overrides reach it the same way
#   - the script is executable, parses, and make shellcheck lints it
#   - an edit to the script runs the e2e-operator jobs of the eight service
#     operators, through tests/e2e/lib/** for seven of them and by name for
#     neutron, and neither the c5c3 nor the ovn job
#   - each of the eight service network-policy suites applies the fixture of
#     the table below and calls the probe with its pods and ports: the
#     target's first drop before any other probe, the outside drop after the
#     inside open and before the update patch, the outside open after it; the
#     table's sink and API ports are the TCP ports the suite's own
#     NetworkPolicy assert names; the target carries the policy's two selector
#     labels and no other, only nova's console stand-in carries a component
#     label, the pods run the service image with the restricted pod spec, the
#     target and sink listen on the table's ports with one listener program
#     across all sixteen, the clients and the console stand-in sleep, the
#     update patch admits <cr>-outside, and no suite keeps its SCOPE CAVEAT
#
# The kubectl stub runs the program the script hands to kubectl exec with the
# local python3, behind a prelude that replaces socket.create_connection,
# socket.getaddrinfo and time.sleep. Without python3 those cases are skipped
# and counted. The structure of the fixtures is read with mikefarah yq; without
# it those checks are skipped and counted.
#
# Usage: bash tests/unit/ci/assert_networkpolicy_enforced_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC2034 # read by tests/lib/ci_yaml.sh
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

SCRIPT="$PROJECT_ROOT/tests/e2e/lib/assert-networkpolicy-enforced.sh"
USAGE="usage: assert-networkpolicy-enforced.sh <namespace> <pod> <expect> <host> [<port>...]"

HAVE_PYTHON=0
if command -v python3 >/dev/null 2>&1; then
  HAVE_PYTHON=1
fi
HAVE_YQ=0
if command -v yq >/dev/null 2>&1 && yq --version 2>&1 | grep -q mikefarah; then
  HAVE_YQ=1
fi

# The eight suites under tests/e2e/<dir>/network-policy, one per line:
#   dir|cr|fixture|target ports|sink ports admitted|sink ports denied|API ports
# The target listens on the API ports and 9999, which no ingress rule admits.
# The sink listens on the TCP egress ports the policy admits other than 53 and
# 443, then on the denied ones.
SUITES='keystone|keystone-netpol|03-probe-pods.yaml|5000 9999|3306 6443 11211|9999|5000
horizon|horizon-netpol|03-probe-pods.yaml|8080 9999|5000 11211|9999|8080
glance|glance-netpol|04-probe-pods.yaml|9292 9999|3306 3900 11211|9999|9292
placement|placement-netpol|03-probe-pods.yaml|8778 9999|3306 5000 11211|9999|8778
barbican|barbican-netpol|04-probe-pods.yaml|9311 9999|3306 5000 8200 11211|9999|9311
neutron|neutron-netpol|05-probe-pods.yaml|9696 9999|3306 5000 5672 6641 6642 11211|9999|9696
cinder|cinder-netpol|04-probe-pods.yaml|8776 9999|2049 3306 5672 11211|5000 9999|8776
nova|nova-netpol|04-probe-pods.yaml|8774 8775 6080 9999|3306 5000 5672 8778 9292 9696 11211|5900 9999|8774 8775 6080'

# The restricted pod spec every probe pod carries, as yq path=value.
RESTRICTED='.spec.automountServiceAccountToken=false
.spec.restartPolicy=Never
.spec.terminationGracePeriodSeconds=0
.spec.securityContext.runAsNonRoot=true
.spec.securityContext.runAsUser=42424
.spec.securityContext.runAsGroup=42424
.spec.securityContext.seccompProfile.type=RuntimeDefault
.spec.containers[0].imagePullPolicy=IfNotPresent
.spec.containers[0].securityContext.allowPrivilegeEscalation=false
.spec.containers[0].securityContext.readOnlyRootFilesystem=true
.spec.containers[0].securityContext.capabilities.drop[0]=ALL
.spec.containers[0].resources.requests.cpu=10m
.spec.containers[0].resources.requests.memory=64Mi
.spec.containers[0].resources.limits.memory=128Mi'

STUBS="$(mktemp -d "${TMPDIR:-/tmp}/netpol-probe-test.XXXXXX")"
trap 'rm -rf "$STUBS"' EXIT

# The kubectl stub logs every call to calls.log. It answers
# `exec -n <ns> <pod> -- python3 -c <program> <args...>` by saving the program
# to program and the arguments after it, one per line, to argv, and then
# running the prelude and the program with the local python3. Any other call
# exits 99. STUB_EXEC_RC, when not 0, makes it print STUB_EXEC_ERR on stderr
# and exit with that code instead.
cat >"$STUBS/kubectl" <<'STUB'
#!/bin/bash
stubs="$(dirname "$0")"
printf '%s\n' "$*" >>"$stubs/calls.log"
if [ "${STUB_EXEC_RC:-0}" -ne 0 ]; then
  if [ -n "${STUB_EXEC_ERR:-}" ]; then
    printf '%s\n' "$STUB_EXEC_ERR" >&2
  fi
  exit "$STUB_EXEC_RC"
fi
if [ "$1 $2 $5 $6 $7" != "exec -n -- python3 -c" ]; then
  echo "unexpected kubectl call: $*" >&2
  exit 99
fi
program="$8"
shift 8
printf '%s' "$program" >"$stubs/program"
printf '%s\n' "$@" >"$stubs/argv"
exec python3 -c "$(cat "$stubs/prelude.py")"$'\n'"$program" "$@"
STUB
chmod +x "$STUBS/kubectl"

# The prelude the stub runs before the program:
#   STUB_CONNECT  comma list of connected, timeout or refused; each
#                 create_connection takes the next entry and the last one
#                 repeats (default connected). Every call appends
#                 "<host> <port> <timeout>" to connects.log.
#   STUB_RESOLVE  comma list of ok, which returns 10.96.0.1, or fail, which
#                 raises gaierror; each getaddrinfo takes the next entry and
#                 the last one repeats (default ok). Every call appends
#                 "<host>" to resolves.log.
# time.sleep returns at once.
cat >"$STUBS/prelude.py" <<'PY'
import os as _os, socket as _socket, time as _time

_results = _os.environ.get("STUB_CONNECT", "connected").split(",")
_lookups = _os.environ.get("STUB_RESOLVE", "ok").split(",")
_log = _os.path.join(_os.environ["STUB_DIR"], "connects.log")
_resolve_log = _os.path.join(_os.environ["STUB_DIR"], "resolves.log")


class _Connection:
    def close(self):
        pass


def _create_connection(address, timeout=None):
    with open(_log, "a") as f:
        f.write("%s %s %s\n" % (address[0], address[1], timeout))
    result = _results.pop(0) if len(_results) > 1 else _results[0]
    if result == "timeout":
        raise _socket.timeout("timed out")
    if result == "refused":
        raise ConnectionRefusedError(111, "Connection refused")
    return _Connection()


def _getaddrinfo(host, port, *args, **kwargs):
    with open(_resolve_log, "a") as f:
        f.write("%s\n" % host)
    lookup = _lookups.pop(0) if len(_lookups) > 1 else _lookups[0]
    if lookup == "fail":
        raise _socket.gaierror(-2, "Name or service not known")
    return [(_socket.AF_INET, _socket.SOCK_STREAM, 6, "", ("10.96.0.1", 0))]


_socket.create_connection = _create_connection
_socket.getaddrinfo = _getaddrinfo
_time.sleep = lambda seconds: None
PY

# run_probe [VAR=value...] -- <args...>
# Runs the script with the kubectl stub first on PATH and only the named
# overrides and stub settings set. Leaves its stdout in RUN_OUT and its exit
# code in RUN_RC, and writes its stderr to $STUBS/stderr. The logs and files of
# the previous run are removed first.
run_probe() {
  local vars=()
  while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do
    vars+=("$1")
    shift
  done
  shift
  rm -f "$STUBS/calls.log" "$STUBS/connects.log" "$STUBS/resolves.log" "$STUBS/program" \
    "$STUBS/argv" "$STUBS/marker"
  RUN_OUT="$(env -u NETPOL_PROBE_TIMEOUT -u NETPOL_PROBE_INTERVAL \
    -u NETPOL_PROBE_ATTEMPTS -u NETPOL_DROP_CONFIRM \
    -u STUB_CONNECT -u STUB_RESOLVE -u STUB_EXEC_RC -u STUB_EXEC_ERR \
    PATH="$STUBS:$PATH" STUB_DIR="$STUBS" ${vars[@]+"${vars[@]}"} \
    bash "$SCRIPT" "$@" 2>"$STUBS/stderr")"
  RUN_RC=$?
}

# skip_without_python <what>: count a skip and return 1 when python3 is absent.
skip_without_python() {
  if [ "$HAVE_PYTHON" -eq 0 ]; then
    echo "  SKIP: $1 (python3 not found)"
    SKIP=$((SKIP + 1))
    return 1
  fi
}

# expect_usage <description> [VAR=value...] -- <args...>
# Asserts that the run exits 2 with the usage line on stderr, calls no
# kubectl, and leaves no marker file behind.
expect_usage() {
  local description="$1"
  shift
  run_probe "$@"
  assert_eq "$description: exits 2" "2" "$RUN_RC"
  assert_contains "$description: prints the usage line on stderr" "$(cat "$STUBS/stderr")" "$USAGE"
  assert_eq "$description: calls no kubectl" "" "$(cat "$STUBS/calls.log" 2>/dev/null)"
  assert_eq "$description: runs no command it was handed" "absent" \
    "$([ -e "$STUBS/marker" ] && echo present || echo absent)"
}

# --- Test 1: the arguments ---
test_wrong_arguments_exit_2() {
  echo "Test: wrong arguments exit 2 with the usage line and call no kubectl"

  local inject="PATH[\$(touch $STUBS/marker)]"
  expect_usage "no argument" --
  expect_usage "one argument" -- openstack
  expect_usage "two arguments" -- openstack p
  expect_usage "three arguments" -- openstack p open
  expect_usage "an empty namespace" -- "" p open 10.0.0.1 5000
  expect_usage "expect blocked" -- openstack p blocked 10.0.0.1 5000
  expect_usage "open without a port" -- openstack p open 10.0.0.1
  expect_usage "dropped without a port" -- openstack p dropped 10.0.0.1
  expect_usage "resolves with a port" -- openstack p resolves 10.0.0.1 5000
  expect_usage "an empty host" -- openstack p open "" 5000
  expect_usage "host 'a b'" -- openstack p open "a b" 5000
  expect_usage "port 0" -- openstack p open 10.0.0.1 0
  expect_usage "port 65536" -- openstack p open 10.0.0.1 65536
  expect_usage "port 08" -- openstack p open 10.0.0.1 08
  expect_usage "port 5000x" -- openstack p open 10.0.0.1 5000x
  expect_usage "a port carrying a command" -- openstack p open 10.0.0.1 5000 "$inject"
  expect_usage "NETPOL_PROBE_ATTEMPTS=abc" NETPOL_PROBE_ATTEMPTS=abc -- openstack p open 10.0.0.1 5000
  expect_usage "NETPOL_PROBE_TIMEOUT=0" NETPOL_PROBE_TIMEOUT=0 -- openstack p open 10.0.0.1 5000
  expect_usage "an override carrying a command" "NETPOL_DROP_CONFIRM=$inject" -- \
    openstack p dropped 10.0.0.1 9999

  run_probe -- openstack p open 10.0.0.1 08
  assert_contains "the second stderr line names the bad port" "$(cat "$STUBS/stderr")" \
    "port must be a number from 1 to 65535, got '08'"
}

# --- Test 2: open ---
test_open() {
  echo "Test: open passes at the first connect and fails with the last result"
  skip_without_python "open" || return 0

  run_probe STUB_CONNECT=connected -- openstack p open 10.0.0.1 5000
  assert_eq "a connect exits 0" "0" "$RUN_RC"
  assert_contains "it prints the OK line of attempt 1" "$RUN_OUT" \
    "OK: TCP 5000 to 10.0.0.1 is open from openstack/p (attempt 1)"

  run_probe STUB_CONNECT=timeout,timeout,connected -- openstack p open 10.0.0.1 5000
  assert_eq "a connect at the third attempt exits 0" "0" "$RUN_RC"
  assert_contains "it names attempt 3" "$RUN_OUT" \
    "OK: TCP 5000 to 10.0.0.1 is open from openstack/p (attempt 3)"

  run_probe STUB_CONNECT=refused NETPOL_PROBE_ATTEMPTS=3 -- openstack p open 10.0.0.1 5000
  assert_eq "a refusal on every attempt exits 1" "1" "$RUN_RC"
  assert_contains "the FAIL line names port, host, attempts and the refusal" "$RUN_OUT" \
    "FAIL: TCP 5000 to 10.0.0.1 did not open from openstack/p in 3 attempts; last result: [Errno 111] Connection refused"
  assert_not_contains "the program's own FAIL line is not reported as an exec failure" \
    "$RUN_OUT" "cannot run the probe"

  run_probe STUB_CONNECT=timeout NETPOL_PROBE_ATTEMPTS=3 -- openstack p open 10.0.0.1 5000
  assert_eq "a timeout on every attempt exits 1" "1" "$RUN_RC"
  assert_contains "the FAIL line names the timeout" "$RUN_OUT" \
    "in 3 attempts; last result: timed out"

  run_probe STUB_CONNECT=timeout -- openstack p open 10.0.0.1 5000
  assert_contains "without an override the FAIL line names 15 attempts" "$RUN_OUT" \
    "FAIL: TCP 5000 to 10.0.0.1 did not open from openstack/p in 15 attempts; last result: timed out"

  run_probe STUB_CONNECT=connected,timeout NETPOL_PROBE_ATTEMPTS=2 -- openstack p open 10.0.0.1 3306 11211
  assert_eq "a second port that never opens exits 1" "1" "$RUN_RC"
  assert_contains "the first port passes" "$RUN_OUT" \
    "OK: TCP 3306 to 10.0.0.1 is open from openstack/p (attempt 1)"
  assert_contains "the second port fails" "$RUN_OUT" \
    "FAIL: TCP 11211 to 10.0.0.1 did not open from openstack/p in 2 attempts; last result: timed out"
}

# --- Test 3: dropped ---
test_dropped() {
  echo "Test: dropped passes on confirmed timeouts and fails on anything else"
  skip_without_python "dropped" || return 0

  run_probe STUB_CONNECT=timeout -- openstack p dropped 10.0.0.1 9999
  assert_eq "three timeouts exit 0" "0" "$RUN_RC"
  assert_contains "it prints the OK line" "$RUN_OUT" \
    "OK: TCP 9999 to 10.0.0.1 is dropped from openstack/p (3 timeouts in a row)"

  run_probe STUB_CONNECT=connected,connected,timeout -- openstack p dropped 10.0.0.1 9999
  assert_eq "connects before the first timeout are polled through" "0" "$RUN_RC"
  assert_eq "two connects, the timeout and two confirmations" "5" \
    "$(wc -l <"$STUBS/connects.log" | tr -d ' ')"

  run_probe STUB_CONNECT=refused,refused,timeout -- openstack p dropped 10.0.0.1 9999
  assert_eq "refusals before the first timeout are polled through" "0" "$RUN_RC"
  assert_contains "it prints the OK line after the refusals" "$RUN_OUT" \
    "OK: TCP 9999 to 10.0.0.1 is dropped from openstack/p (3 timeouts in a row)"
  assert_eq "two refusals, the timeout and two confirmations" "5" \
    "$(wc -l <"$STUBS/connects.log" | tr -d ' ')"

  run_probe STUB_CONNECT=timeout -- openstack p dropped 10.0.0.1 5900 9999
  assert_eq "two dropped ports exit 0" "0" "$RUN_RC"
  assert_contains "the first port passes" "$RUN_OUT" \
    "OK: TCP 5900 to 10.0.0.1 is dropped from openstack/p (3 timeouts in a row)"
  assert_contains "the second port passes" "$RUN_OUT" \
    "OK: TCP 9999 to 10.0.0.1 is dropped from openstack/p (3 timeouts in a row)"

  run_probe STUB_CONNECT=connected NETPOL_PROBE_ATTEMPTS=2 -- openstack p dropped 10.0.0.1 5900 9999
  assert_eq "a first port that is never dropped exits 1" "1" "$RUN_RC"
  assert_contains "the FAIL line names the first port" "$RUN_OUT" \
    "FAIL: TCP 5900 to 10.0.0.1 was never dropped from openstack/p in 2 attempts"
  assert_eq "the run ends at the first failing port" "0" \
    "$(grep -c '^10.0.0.1 9999 ' "$STUBS/connects.log")"

  run_probe STUB_CONNECT=timeout NETPOL_DROP_CONFIRM=0 -- openstack p dropped 10.0.0.1 9999
  assert_contains "NETPOL_DROP_CONFIRM=0 passes on one timeout" "$RUN_OUT" "(1 timeouts in a row)"

  run_probe STUB_CONNECT=connected -- openstack p dropped 10.0.0.1 9999
  assert_eq "a connect on every attempt exits 1" "1" "$RUN_RC"
  assert_contains "the FAIL line says the port was never dropped" "$RUN_OUT" \
    "FAIL: TCP 9999 to 10.0.0.1 was never dropped from openstack/p in 15 attempts; last result: connected"
  assert_contains "the FAIL line names NetworkPolicy enforcement" "$RUN_OUT" \
    "the CNI does not enforce NetworkPolicy, or the policy admits this port"

  run_probe STUB_CONNECT=refused -- openstack p dropped 10.0.0.1 9999
  assert_eq "a refusal is never a drop" "1" "$RUN_RC"
  assert_contains "the FAIL line names the refusal" "$RUN_OUT" \
    "last result: [Errno 111] Connection refused"

  run_probe STUB_CONNECT=timeout,connected -- openstack p dropped 10.0.0.1 9999
  assert_eq "a connect after the first timeout exits 1" "1" "$RUN_RC"
  assert_contains "the FAIL line names the connect after the drop" "$RUN_OUT" \
    "FAIL: TCP 9999 to 10.0.0.1 returned connected from openstack/p after a drop"

  run_probe STUB_CONNECT=timeout,refused -- openstack p dropped 10.0.0.1 9999
  assert_eq "a refusal after the first timeout exits 1" "1" "$RUN_RC"
  assert_contains "the FAIL line names the refusal after the drop" "$RUN_OUT" \
    "returned [Errno 111] Connection refused from openstack/p after a drop"
}

# --- Test 4: resolves ---
test_resolves() {
  echo "Test: resolves passes with the first address, retries, and fails with the lookup error"
  skip_without_python "resolves" || return 0

  run_probe STUB_RESOLVE=ok -- openstack p resolves kubernetes.default.svc.cluster.local.
  assert_eq "a resolvable name exits 0" "0" "$RUN_RC"
  assert_contains "it prints the first address" "$RUN_OUT" \
    "OK: kubernetes.default.svc.cluster.local. resolves from openstack/p to 10.96.0.1"

  run_probe STUB_RESOLVE=fail,fail,ok -- openstack p resolves kubernetes.default.svc.cluster.local.
  assert_eq "a lookup error is retried" "0" "$RUN_RC"
  assert_eq "two failed lookups and the one that resolves" "3" \
    "$(wc -l <"$STUBS/resolves.log" | tr -d ' ')"

  run_probe STUB_RESOLVE=fail -- openstack p resolves kubernetes.default.svc.cluster.local.
  assert_eq "an unresolvable name exits 1" "1" "$RUN_RC"
  assert_contains "the FAIL line carries the gaierror text" "$RUN_OUT" \
    "FAIL: kubernetes.default.svc.cluster.local. does not resolve from openstack/p in 15 attempts; last error: [Errno -2] Name or service not known"
  assert_eq "it looks the name up 15 times" "15" \
    "$(grep -cx 'kubernetes.default.svc.cluster.local.' "$STUBS/resolves.log")"
}

# --- Test 5: kubectl exec fails ---
test_an_exec_failure_is_reported() {
  echo "Test: a kubectl exec that cannot run the probe fails with its last line"

  run_probe STUB_EXEC_RC=1 'STUB_EXEC_ERR=error: pods "p" not found' -- openstack p open 10.0.0.1 5000
  assert_eq "a missing pod exits 1" "1" "$RUN_RC"
  assert_contains "it names the pod and kubectl's error" "$RUN_OUT" \
    'FAIL: cannot run the probe in openstack/p: error: pods "p" not found'

  run_probe STUB_EXEC_RC=126 -- openstack p open 10.0.0.1 5000
  assert_eq "an exec failure without output exits 1" "1" "$RUN_RC"
  assert_contains "it names the exit code" "$RUN_OUT" \
    "FAIL: cannot run the probe in openstack/p: kubectl exec exited 126"
}

# --- Test 6: the arguments the program receives ---
test_host_and_ports_arrive_as_argv() {
  echo "Test: host and ports reach the program as arguments, never in its text"
  skip_without_python "the program's arguments" || return 0

  run_probe STUB_CONNECT=connected -- openstack p open 10.0.0.1 5000
  assert_starts_with "kubectl execs python3 -c in the pod" \
    "$(head -n 1 "$STUBS/calls.log")" "exec -n openstack p -- python3 -c "
  assert_not_contains "the program text does not carry the host" "$(cat "$STUBS/program")" "10.0.0.1"
  assert_not_contains "the program text does not carry the port" "$(cat "$STUBS/program")" "5000"
  assert_eq "the arguments after the program are expect, source, host, defaults and port" \
    "open openstack/p 10.0.0.1 5 2 15 2 5000" "$(paste -s -d ' ' "$STUBS/argv")"
  assert_eq "the probe connects to the host and port with the connect timeout" \
    "10.0.0.1 5000 5" "$(cat "$STUBS/connects.log")"

  run_probe STUB_CONNECT=timeout NETPOL_PROBE_TIMEOUT=7 NETPOL_PROBE_INTERVAL=0 \
    NETPOL_PROBE_ATTEMPTS=1 NETPOL_DROP_CONFIRM=4 -- openstack p dropped 10.0.0.1 9999
  assert_eq "the overrides reach the program as arguments" \
    "dropped openstack/p 10.0.0.1 7 0 1 4 9999" "$(paste -s -d ' ' "$STUBS/argv")"
  assert_eq "a drop takes the first timeout and the confirmations" "5" \
    "$(grep -c '^10.0.0.1 9999 7$' "$STUBS/connects.log")"
}

# --- Test 7: the script file ---
test_the_script_parses_and_is_linted() {
  echo "Test: the script is executable, parses, and make shellcheck lints it"

  assert_eq "the script is executable" "yes" "$([ -x "$SCRIPT" ] && echo yes || echo no)"
  bash -n "$SCRIPT"
  assert_eq "bash -n passes" "0" "$?"
  assert_file_contains_fixed "make shellcheck lints tests/e2e/lib/" "$PROJECT_ROOT/Makefile" \
    "@shellcheck --severity=warning tests/e2e/lib/*.sh"
}

# --- Test 8: the path filters ---
test_the_filters_name_the_probe() {
  echo "Test: an edit to the probe runs the leg of every service operator and no other"

  local op block
  for op in keystone horizon glance placement barbican cinder nova; do
    block="$(filter_block "tests_e2e_$op")"
    assert_contains "tests_e2e_$op lists the shared directory" "$block" "'tests/e2e/lib/**'"
  done
  assert_contains "tests_e2e_neutron names the probe" "$(filter_block tests_e2e_neutron)" \
    "'tests/e2e/lib/assert-networkpolicy-enforced.sh'"
  # c5c3 and ovn run no network-policy suite.
  for op in c5c3 ovn; do
    block="$(filter_block "tests_e2e_$op")"
    assert_not_empty "tests_e2e_$op exists" "$block"
    assert_not_contains "tests_e2e_$op leaves the probe out" "$block" "assert-networkpolicy-enforced.sh"
    assert_not_contains "tests_e2e_$op leaves the shared directory out" "$block" "tests/e2e/lib/**"
  done
}

# pod_field <file> <pod> <yq expression>: the expression on the named Pod of
# the multi-document file.
pod_field() {
  POD="$2" yq ea '[select(.kind == "Pod" and .metadata.name == strenv(POD))] | .[0] | '"$3" "$1"
}

# policy_tcp_ports <suite> <cr> <egress|ingress>: the TCP ports other than 53
# and 443 that the suite's assert steps name for that direction of the
# NetworkPolicy <cr>, sorted and without duplicates.
policy_tcp_ports() {
  CR="$2" yq '[.spec.steps[].try[] | select(has("assert")) | .assert.resource
    | select(.kind == "NetworkPolicy" and .metadata.name == strenv(CR))
    | .spec.'"$3"'[]?.ports[] | select(.protocol == "TCP" and .port != 53 and .port != 443)
    | .port] | unique | sort | join(" ")' "$1"
}

# ascending <n>...: print yes when every argument is a whole number above the
# one before it, and no otherwise. The regex runs before the comparison.
ascending() {
  local prev=0 n
  for n in "$@"; do
    if ! [[ "$n" =~ ^[1-9][0-9]*$ ]] || [ "$n" -le "$prev" ]; then
      echo no
      return
    fi
    prev="$n"
  done
  echo yes
}

# --- Test 9: the eight suites ---
test_each_suite_probes_its_policy() {
  echo "Test: each service network-policy suite probes its policy with the fixture of the table"

  local found dir cr fixture target sink_open sink_denied api suite fx patch patches path value
  local listener="" program pod vnc pods namespaces step first
  local order=()
  found="$(for suite in "$PROJECT_ROOT"/tests/e2e/*/network-policy/chainsaw-test.yaml; do
    suite="${suite%/network-policy/chainsaw-test.yaml}"
    echo "${suite##*/}"
  done | LC_ALL=C sort | paste -s -d ' ' -)"
  assert_eq "the network-policy suites on disk are the table's" \
    "$(cut -d '|' -f 1 <<<"$SUITES" | LC_ALL=C sort | paste -s -d ' ' -)" "$found"

  while IFS='|' read -r dir cr fixture target sink_open sink_denied api; do
    suite="$PROJECT_ROOT/tests/e2e/$dir/network-policy/chainsaw-test.yaml"
    fx="$PROJECT_ROOT/tests/e2e/$dir/network-policy/$fixture"
    assert_eq "the $dir fixture $fixture exists" "yes" "$([ -f "$fx" ] && echo yes || echo no)"
    assert_file_not_contains "the $dir suite has no SCOPE CAVEAT" "$suite" "SCOPE CAVEAT"
    assert_file_contains_fixed "the $dir suite applies its fixture" "$suite" "file: $fixture"
    assert_file_contains_fixed "the $dir suite runs the probe" "$suite" \
      "PROBE=../../lib/assert-networkpolicy-enforced.sh"
    assert_file_contains_fixed "the $dir target is dropped on the denied sink ports" "$suite" \
      "\"\${PROBE}\" openstack $cr-probe-target dropped \"\${SINK}\" $sink_denied"
    assert_file_contains_fixed "the $dir target resolves a cluster name" "$suite" \
      "\"\${PROBE}\" openstack $cr-probe-target resolves kubernetes.default.svc.cluster.local."
    assert_file_contains_fixed "the $dir target reaches the admitted sink ports" "$suite" \
      "\"\${PROBE}\" openstack $cr-probe-target open \"\${SINK}\" $sink_open"
    assert_file_contains_fixed "the $dir inside client is dropped on 9999" "$suite" \
      "\"\${PROBE}\" $dir-system $cr-probe-inside dropped \"\${TARGET}\" 9999"
    assert_file_contains_fixed "the $dir inside client reaches the API ports" "$suite" \
      "\"\${PROBE}\" $dir-system $cr-probe-inside open \"\${TARGET}\" $api"
    assert_file_contains_fixed "the $dir outside client is dropped on the API ports" "$suite" \
      "\"\${PROBE}\" $cr-outside $cr-probe-outside dropped \"\${TARGET}\" $api"
    assert_file_contains_fixed "the $dir outside client reaches the API ports after the patch" "$suite" \
      "\"\${PROBE}\" $cr-outside $cr-probe-outside open \"\${TARGET}\" $api"
    patches=("$PROJECT_ROOT"/tests/e2e/"$dir"/network-policy/0*-patch-update-ingress.yaml)
    patch="${patches[0]}"
    assert_file_contains_fixed "the $dir update patch admits $cr-outside" "$patch" \
      "kubernetes.io/metadata.name: $cr-outside"
    # The suite's reasoning rests on this order: the target's first drop waits
    # for kindnet, the inside open shows kindnet knows the clients before the
    # outside drop, and the outside open after the patch shows that drop came
    # from the policy.
    first="$(grep -nF -m 1 "\"\${PROBE}\" " "$suite" | cut -d: -f1)"
    order=()
    for step in "\"\${PROBE}\" openstack $cr-probe-target dropped" \
      "\"\${PROBE}\" $dir-system $cr-probe-inside open" \
      "\"\${PROBE}\" $cr-outside $cr-probe-outside dropped" \
      "file: ${patch##*/}" \
      "\"\${PROBE}\" $cr-outside $cr-probe-outside open"; do
      order+=("$(grep -nF -m 1 -- "$step" "$suite" | cut -d: -f1)")
    done
    assert_eq "the $dir target's drop is the first probe" "$first" "${order[0]}"
    assert_eq "the $dir inside open, outside drop, patch and outside open come in order" yes \
      "$(ascending "${order[@]}")"
    # Nova's console proxy has a policy of its own, which a fifth pod with the
    # proxy's component label probes.
    vnc=""
    if [ "$dir" = nova ]; then
      vnc="$cr-probe-vnc"
      assert_file_contains_fixed "the $dir console stand-in reaches the display range" "$suite" \
        "\"\${PROBE}\" openstack $vnc open \"\${SINK}\" 5900 9999"
    fi

    if [ "$HAVE_YQ" -eq 0 ]; then
      echo "  SKIP: the structure of $fixture of $dir (mikefarah yq not found)"
      SKIP=$((SKIP + 1))
      continue
    fi
    assert_eq "the $dir sink ports are the asserted TCP egress ports but 53 and 443" "$sink_open" \
      "$(policy_tcp_ports "$suite" "$cr" egress)"
    assert_eq "the $dir API ports are the asserted TCP ingress ports" \
      "$(tr ' ' '\n' <<<"$api" | sort -n | paste -s -d ' ' -)" "$(policy_tcp_ports "$suite" "$cr" ingress)"
    assert_eq "the $dir fixture creates the Namespace $cr-outside" "$cr-outside" \
      "$(yq ea '[select(.kind == "Namespace")] | map(.metadata.name) | join(",")' "$fx")"
    pods="$cr-probe-target $cr-probe-sink $cr-probe-inside $cr-probe-outside${vnc:+ $vnc}"
    namespaces="openstack openstack $dir-system $cr-outside${vnc:+ openstack}"
    assert_eq "the $dir fixture holds the probe pods" "$pods" \
      "$(yq ea '[select(.kind == "Pod")] | map(.metadata.name) | join(" ")' "$fx")"
    assert_eq "the $dir probe pods run in openstack, $dir-system and $cr-outside" "$namespaces" \
      "$(yq ea '[select(.kind == "Pod")] | map(.metadata.namespace) | join(" ")' "$fx")"
    assert_eq "the $dir target carries the policy's two selector labels and no other" \
      "app.kubernetes.io/instance=$cr,app.kubernetes.io/name=$dir" \
      "$(pod_field "$fx" "$cr-probe-target" '.metadata.labels | to_entries | map(.key + "=" + .value) | sort | join(",")')"
    assert_eq "only the console stand-in among the $dir probe pods carries a component label" "$vnc" \
      "$(yq ea '[select(.kind == "Pod") | select(.metadata.labels | has("app.kubernetes.io/component"))] | map(.metadata.name) | join(" ")' "$fx")"
    if [ -n "$vnc" ]; then
      assert_eq "the $dir console stand-in carries the console policy's three selector labels" \
        "app.kubernetes.io/component=novncproxy,app.kubernetes.io/instance=$cr,app.kubernetes.io/name=$dir" \
        "$(pod_field "$fx" "$vnc" '.metadata.labels | to_entries | map(.key + "=" + .value) | sort | join(",")')"
    fi
    assert_eq "the $dir probe pods run the $dir image" "ghcr.io/c5c3/$dir:2026.1" \
      "$(yq ea '[select(.kind == "Pod")] | map(.spec.containers[0].image) | unique | join(",")' "$fx")"
    # The pod count is pinned above, so one value per field covers every pod;
    # a pod without the field adds null.
    while IFS='=' read -r path value; do
      assert_eq "the $dir probe pods set $path" "$value" \
        "$(yq ea '[select(.kind == "Pod")] | map('"$path"') | unique | join(",")' "$fx")"
    done <<<"$RESTRICTED"
    assert_eq "the $dir target listens on the table's ports" "$target" \
      "$(pod_field "$fx" "$cr-probe-target" '.spec.containers[0].command | .[3:] | join(" ")')"
    assert_eq "the $dir sink listens on the table's ports" "$sink_open $sink_denied" \
      "$(pod_field "$fx" "$cr-probe-sink" '.spec.containers[0].command | .[3:] | join(" ")')"
    for pod in "$cr-probe-inside" "$cr-probe-outside" ${vnc:+"$vnc"}; do
      assert_eq "$pod sleeps" "sleep infinity" \
        "$(pod_field "$fx" "$pod" '.spec.containers[0].command | join(" ")')"
    done
    for pod in "$cr-probe-target" "$cr-probe-sink"; do
      assert_eq "$pod runs python3 -c" "python3 -c" \
        "$(pod_field "$fx" "$pod" '.spec.containers[0].command | .[0:2] | join(" ")')"
      program="$(pod_field "$fx" "$pod" '.spec.containers[0].command[2]')"
      if [ -z "$listener" ]; then
        listener="$program"
        assert_contains "the listener binds each port it is given" "$listener" \
          'srv.bind(("0.0.0.0", int(port)))'
      fi
      assert_eq "$pod runs the listener of the first target" "$listener" "$program"
    done
  done <<<"$SUITES"
}

# --- Run ---
test_wrong_arguments_exit_2
test_open
test_dropped
test_resolves
test_an_exec_failure_is_reported
test_host_and_ports_arrive_as_argv
test_the_script_parses_and_is_linted
test_the_filters_name_the_probe
test_each_suite_probes_its_policy

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
