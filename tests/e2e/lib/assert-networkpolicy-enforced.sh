#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/lib/assert-networkpolicy-enforced.sh: prove from inside a pod that
# the cluster enforces a NetworkPolicy as its rules say.
#
# A network-policy suite asserts the policy object its operator renders, which
# says nothing about the packets. This script runs a probe in a pod with
# kubectl exec and reports what the network did with its connections:
#   open      every TCP port connects to <host>, retried until one attempt
#             connects;
#   dropped   every TCP port to <host> times out, polled until the first
#             timeout and then confirmed by NETPOL_DROP_CONFIRM more timeouts
#             in a row;
#   resolves  <host> resolves through the pod's resolver, retried on a lookup
#             error; it takes no port.
# Ports are checked in the order given, and the first that fails ends the run.
#
# Usage:
#   assert-networkpolicy-enforced.sh <namespace> <pod> <expect> <host> [<port>...]
#
# <namespace>/<pod> name the pod the connection starts from, which must ship
# python3 (every service image does). <expect> is open, dropped or resolves.
# <host> is an IPv4 address or a DNS name; a name with a trailing dot keeps
# the resolver off the search list. Each port is a number from 1 to 65535.
#
# Only a timeout counts as a drop. kindnet drops the SYN of a connection a
# policy denies, so the client hears nothing until its connect timeout. A
# refusal, an unreachable route or a reset means something answered or the
# route is broken, and is reported as the result it is. In dropped mode a
# connect or such an error before the first timeout means the CNI has not
# added the pod to its policy set yet, so polling goes on. A probe that never
# times out fails with "was never dropped": the CNI does not enforce
# NetworkPolicy, or the policy admits the port.
#
# Each probed port or name prints one OK: line, or a FAIL: line with the last
# result, and the script exits 1 at the first FAIL:. When kubectl exec cannot
# run the probe at all (no such pod, a container that is not running, no
# python3), the script prints "FAIL: cannot run the probe in
# <namespace>/<pod>:" with kubectl's last output line and exits 1. A wrong
# argument count, an unknown <expect>, a missing or surplus port, or a
# malformed host, port or override prints the usage line and the bad value on
# stderr and exits 2 without calling kubectl.
#
# Environment overrides, each a whole number:
#   NETPOL_PROBE_TIMEOUT   connect timeout in seconds (default 5, at least 1)
#   NETPOL_PROBE_INTERVAL  seconds between attempts (default 2)
#   NETPOL_PROBE_ATTEMPTS  attempts per port or name (default 15, at least 1)
#   NETPOL_DROP_CONFIRM    timeouts required after the first one (default 2)
#
# The probe is a Python program held below in a quoted heredoc. Host and ports
# reach it as arguments only and are never written into its text.
#
# Chainsaw runs script steps in the suite directory, so a suite calls this as
# "../../lib/assert-networkpolicy-enforced.sh". The script only reads.

set -uo pipefail

usage() {
  echo "usage: assert-networkpolicy-enforced.sh <namespace> <pod> <expect> <host> [<port>...]" >&2
  if [ -n "${1:-}" ]; then
    echo "$1" >&2
  fi
  exit 2
}

# check_whole <name> <value> <min>: exit 2 unless <value> is a whole number of
# at least <min>. The regex runs before the comparison, so a value such as
# PATH[$(cmd)] never reaches bash arithmetic, which would run the command in
# the subscript.
check_whole() {
  if ! [[ "$2" =~ ^(0|[1-9][0-9]*)$ ]]; then
    usage "$1 must be a whole number, got '$2'"
  fi
  if [ "$2" -lt "$3" ]; then
    usage "$1 must be at least $3, got '$2'"
  fi
}

if [ "$#" -lt 4 ]; then
  usage "expected at least 4 arguments, got $#"
fi

# Not NAMESPACE: chainsaw injects that variable into every script step.
ns="$1"
pod="$2"
expect="$3"
host="$4"
shift 4

if [ -z "$ns" ] || [ -z "$pod" ]; then
  usage "the namespace and the pod must not be empty"
fi
case "$expect" in
  open | dropped)
    if [ "$#" -eq 0 ]; then
      usage "$expect needs at least one port"
    fi
    ;;
  resolves)
    if [ "$#" -ne 0 ]; then
      usage "resolves takes no port, got '$*'"
    fi
    ;;
  *)
    usage "unknown expect '$expect': want open, dropped or resolves"
    ;;
esac
if ! [[ "$host" =~ ^[A-Za-z0-9.:-]+$ ]]; then
  usage "host must be an IPv4 address or a DNS name, got '$host'"
fi
for port in "$@"; do
  # The regex first: bash arithmetic reads 08 as bad octal and would run
  # $(...) in an array subscript.
  if ! [[ "$port" =~ ^[1-9][0-9]{0,4}$ ]] || [ "$port" -gt 65535 ]; then
    usage "port must be a number from 1 to 65535, got '$port'"
  fi
done

timeout="${NETPOL_PROBE_TIMEOUT:-5}"
interval="${NETPOL_PROBE_INTERVAL:-2}"
attempts="${NETPOL_PROBE_ATTEMPTS:-15}"
confirm="${NETPOL_DROP_CONFIRM:-2}"
check_whole NETPOL_PROBE_TIMEOUT "$timeout" 1
check_whole NETPOL_PROBE_INTERVAL "$interval" 0
check_whole NETPOL_PROBE_ATTEMPTS "$attempts" 1
check_whole NETPOL_DROP_CONFIRM "$confirm" 0

# argv: <expect> <src> <host> <timeout> <interval> <attempts> <confirm> [<port>...]
IFS= read -r -d '' PROBE <<'PY' || true
import socket, sys, time

expect, src, host = sys.argv[1:4]
timeout, interval, attempts, confirm = (int(a) for a in sys.argv[4:8])
ports = [int(p) for p in sys.argv[8:]]


def attempt(port):
    # A timeout is the drop: kindnet discards a denied SYN. Any other OSError
    # (refused, unreachable, reset) is reported verbatim and is never a drop.
    try:
        socket.create_connection((host, port), timeout=timeout).close()
    except socket.timeout:
        return "timed out"
    except OSError as exc:
        return str(exc)
    return "connected"


def fail(message):
    print("FAIL: " + message, flush=True)
    sys.exit(1)


def resolves():
    last = ""
    for k in range(1, attempts + 1):
        try:
            infos = socket.getaddrinfo(host, None)
        except socket.gaierror as exc:
            last = str(exc)
            if k < attempts:
                time.sleep(interval)
            continue
        print("OK: %s resolves from %s to %s" % (host, src, infos[0][4][0]), flush=True)
        return
    fail("%s does not resolve from %s in %d attempts; last error: %s" % (host, src, attempts, last))


def is_open(port):
    for k in range(1, attempts + 1):
        result = attempt(port)
        if result == "connected":
            print("OK: TCP %d to %s is open from %s (attempt %d)" % (port, host, src, k), flush=True)
            return
        if k < attempts:
            time.sleep(interval)
    fail("TCP %d to %s did not open from %s in %d attempts; last result: %s" % (port, host, src, attempts, result))


def is_dropped(port):
    for k in range(1, attempts + 1):
        result = attempt(port)
        if result == "timed out":
            break
        if k < attempts:
            time.sleep(interval)
    else:
        fail("TCP %d to %s was never dropped from %s in %d attempts; last result: %s; "
             "the CNI does not enforce NetworkPolicy, or the policy admits this port"
             % (port, host, src, attempts, result))
    for _ in range(confirm):
        time.sleep(interval)
        result = attempt(port)
        if result != "timed out":
            fail("TCP %d to %s returned %s from %s after a drop" % (port, host, result, src))
    print("OK: TCP %d to %s is dropped from %s (%d timeouts in a row)" % (port, host, src, confirm + 1), flush=True)


if expect == "resolves":
    resolves()
else:
    for port in ports:
        (is_open if expect == "open" else is_dropped)(port)
PY

out="$(kubectl exec -n "$ns" "$pod" -- python3 -c "$PROBE" \
  "$expect" "$ns/$pod" "$host" "$timeout" "$interval" "$attempts" "$confirm" "$@" 2>&1)"
rc=$?
if [ -n "$out" ]; then
  echo "$out"
fi
# The probe prints a FAIL: line before it exits 1. Without one, kubectl exec
# itself failed.
if [ "$rc" -ne 0 ] && ! grep -q '^FAIL:' <<<"$out"; then
  last="$(tail -n 1 <<<"$out")"
  echo "FAIL: cannot run the probe in $ns/$pod: ${last:-kubectl exec exited $rc}"
  exit 1
fi
exit "$rc"
