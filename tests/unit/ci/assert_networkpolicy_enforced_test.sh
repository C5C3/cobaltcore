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
#
# The kubectl stub runs the program the script hands to kubectl exec with the
# local python3, behind a prelude that replaces socket.create_connection,
# socket.getaddrinfo and time.sleep. Without python3 those cases are skipped
# and counted.
#
# Usage: bash tests/unit/ci/assert_networkpolicy_enforced_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

SCRIPT="$PROJECT_ROOT/tests/e2e/lib/assert-networkpolicy-enforced.sh"
USAGE="usage: assert-networkpolicy-enforced.sh <namespace> <pod> <expect> <host> [<port>...]"

HAVE_PYTHON=0
if command -v python3 >/dev/null 2>&1; then
  HAVE_PYTHON=1
fi

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

# --- Run ---
test_wrong_arguments_exit_2
test_open
test_dropped
test_resolves
test_an_exec_failure_is_reported
test_host_and_ports_arrive_as_argv
test_the_script_parses_and_is_linted

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
