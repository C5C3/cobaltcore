#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the library of the libvirt guest data-plane suite
# (tests/e2e-nova-libvirt/guest-dataplane/guest-lib.sh, #1396) against a stub
# kubectl, each case asserting the exact message the suite logs:
#   1. gl_window passes on a contiguous all-ok window; fails with `says lost`
#      on a lost line, with `jumps from 7 to 9` on a missing seq, and with
#      `0 net lines after 12, want 3` on a window with no new line. Given
#      lost, it passes on a lost window and fails on an ok line in it.
#      gl_window and gl_last_seq fail with the "no line" message on empty
#      stdin, and gl_last_seq prints the highest seq of its kind.
#   2. gl_console fails naming the server when the console read fails and
#      when the log holds kernel lines only; it drops the last line of the
#      log, which a read can cut short, and every line outside the grammar.
#   3. gl_wait_window with a 2 s bound returns the last failure prefixed
#      `after 2 s:`, polls on through a failed read before it passes, and
#      stops at once on a line that is not ok and on a line a kernel message
#      garbled, which the parser drops and the window reads as a gap. Given
#      lost, it polls on a short lost window and stops at once on an ok line.
#   4. gl_wait_result prints the seq of the first matching line after the
#      given one, and times out with its message.
#   5. GL_PIDS_SCRIPT finds the one QEMU of a domain by its -name argument
#      and libvirtd by argv[0] in a proc tree, and counts neither a scanning
#      shell's argv nor its own PID; gl_pids prints both, and fails
#      on two QEMU processes, on none, and when no libvirt pod runs; gl_pid
#      reads one of them. gl_wait_status waits for a server or volume status
#      and fails at once on ERROR with the fault, and gl_wait_compute_up
#      waits for the host's nova-compute to read up with a report after the
#      given time, so an up from a report before it does not pass.
#   6. gl_pod_kill applies a pod-kill of mode one with the suite label and
#      every label selector, and on a missed AllInjected prints the describe
#      output and its message.
#   7. gl_wait_replaced passes on a new UID that turns Ready and ignores a
#      terminating pod; it fails with its message on the old UID alone.
#   8. gl_tempest_verdict passes on four passed tests and on a class whose
#      fixture failed and whose tests passed on the serial retry, and fails on
#      a skipped test, a failed test, a class skipped in its fixture, and a
#      missing test.
#   9. gl_teardown_guests deletes the servers, the available volume, the two
#      groups, the subnet and the network, and fails on a network left over.
#  10. gl_recall fails naming an absent key; gl_dump exits 0 with every
#      kubectl call failing; the library parses, make shellcheck lints it,
#      and sourcing it calls nothing.
#  11. The probe function of guest-user-data.sh writes lines GL_PROBE_RE
#      matches and gl_window reads, and the rendered user-data names the peer.
#  12. gl_last_ok_disk prints the seq of the last ok disk line of a raw log
#      with CRLF lines and kinds out of order, drops the line the EOF marker
#      joins, and fails without an ok disk line; gl_backend_holds passes on
#      probe-N and probe-N+1 and fails on any other value.
#
# Usage: bash tests/unit/ci/nova_libvirt_guest_lib_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
LIB="$PROJECT_ROOT/tests/e2e-nova-libvirt/guest-dataplane/guest-lib.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT
STUB="$TMP_DIR/stub"
mkdir -p "$STUB/bin" "$STUB/consoles"

# The stub kubectl appends every call to $STUB_STATE/calls, fails every call
# when STUB_ALL_FAIL is set, and answers from the environment:
#   STUB_CONSOLES      the console reads in order, each a file name under
#                      $STUB/consoles or fail; the last one repeats
#   STUB_LIBVIRT_PODS  the libvirt-e2e pod list (default one Running pod
#                      libvirt-e2e-abc)
#   STUB_PROC          the proc tree the process scan reads
#   STUB_INJECTED      fail: the AllInjected wait fails
#   STUB_PODS_SEQ      the pod lists of a selector in order, separated by |
#   STUB_READY         fail: the Ready wait of a pod fails
#   STUB_SERVERS_SEQ   the server lists in order (default empty)
#   STUB_VOLUME_SEQ    the volume status reads in order, gone for a missing
#                      volume (default available)
#   STUB_NETWORK       the network list (default empty)
#   STUB_SERVER_SEQ    the server status reads in order (default ACTIVE)
#   STUB_COMPUTE_SEQ   the compute service State and Updated At in order
#                      (default up)
#   STUB_CM            the ConfigMap JSON (default no data)
cat >"$STUB/bin/kubectl" <<'SH'
#!/bin/bash
state="$STUB_STATE"
echo "$*" >>"$state/calls"
[ -z "${STUB_ALL_FAIL:-}" ] || { echo "error: the stub refuses" >&2; exit 1; }
# next <name> <list> <separator>: the next item of the list, the last one
# repeated.
next() {
  local n=0 sep="${3:-,}"
  [ -f "$state/$1" ] && n="$(cat "$state/$1")"
  echo $((n + 1)) >"$state/$1"
  IFS="$sep" read -ra items <<<"$2"
  echo "${items[$n]:-${items[${#items[@]}-1]}}"
}
libvirt_default='{"items":[{"metadata":{"name":"libvirt-e2e-old","uid":"u0","deletionTimestamp":"2026-11-06T10:00:00Z"},"status":{"phase":"Running"}},{"metadata":{"name":"libvirt-e2e-abc","uid":"u1"},"status":{"phase":"Running"}}]}'
case "$1" in
  exec)
    case "$*" in
      *" -- openstack console log show "*)
        item="$(next console "${STUB_CONSOLES:-good}")"
        if [ "$item" = fail ]; then
          echo "error: unable to upgrade connection" >&2
          exit 1
        fi
        cat "$STUB_CONSOLE_DIR/$item"
        ;;
      *" -- openstack server list "*)
        item="$(next servers "${STUB_SERVERS_SEQ:-none}")"
        [ "$item" = none ] || echo "$item"
        ;;
      *" -- openstack volume show "*)
        item="$(next volume "${STUB_VOLUME_SEQ:-available}")"
        if [ "$item" = gone ]; then
          echo "No volume found for nova-pool-guest-vol" >&2
          exit 1
        fi
        echo "$item"
        ;;
      *" -- openstack network list "*) echo "${STUB_NETWORK:-}" ;;
      *" -- openstack server show "*" -c status") next server "${STUB_SERVER_SEQ:-ACTIVE}" ;;
      *" -- openstack server show "*" -c fault") echo "{'code': 500, 'message': 'No valid host was found.'}" ;;
      *" -- openstack compute service list "*)
        item="$(next compute "${STUB_COMPUTE_SEQ:-up}")"
        [ "$item" = fail ] && exit 1
        echo "$item"
        ;;
      *" -- bash -c "*) bash -c "${10}" gl-pids "${12}" "$STUB_PROC" ;;
    esac
    ;;
  get)
    case "$*" in
      "get pod -n openstack -l app.kubernetes.io/name=libvirt-e2e -o json") echo "${STUB_LIBVIRT_PODS:-$libvirt_default}" ;;
      "get pod -n openstack -l "*" -o json") next pods "${STUB_PODS_SEQ:-{\"items\":[]\}}" '|' ;;
      "get configmap nova-pool-guest -n openstack -o json") echo "${STUB_CM:-{\"data\":{\}\}}" ;;
    esac
    ;;
  apply) cat >"$state/applied" ;;
  wait)
    case "$*" in
      *AllInjected*) [ "${STUB_INJECTED:-}" != fail ] || exit 1 ;;
      *"--for=condition=Ready"*) [ "${STUB_READY:-}" != fail ] || exit 1 ;;
    esac
    ;;
  describe) echo "Name: $3 (described)" ;;
esac
exit 0
SH
chmod +x "$STUB/bin/kubectl"

# run_lib <VAR=value...> -- <shell code>: run code under set -euo pipefail
# with the library sourced, the stub first on PATH, NAMESPACE=openstack and
# GL_POLL=0 unless given; GL_OUT and GL_RC hold the result.
run_lib() {
  local envs=()
  while [ "$1" != "--" ]; do
    envs+=("$1")
    shift
  done
  shift
  rm -rf "$STUB/state"
  mkdir -p "$STUB/state"
  : >"$STUB/state/calls"
  GL_RC=0
  GL_OUT="$(env STUB_STATE="$STUB/state" STUB_CONSOLE_DIR="$STUB/consoles" \
    NAMESPACE=openstack GL_POLL=0 PATH="$STUB/bin:$PATH" \
    ${envs[@]+"${envs[@]}"} bash -c "set -euo pipefail; . '$LIB'; $1" 2>&1)" || GL_RC=$?
}

calls() {
  cat "$STUB/state/calls"
}

# lines <kind> <from> <to> [<result>]: probe lines of the kind, seq from..to.
lines() {
  local i
  for i in $(seq "$2" "$3"); do
    printf 'guest-probe %s %d 10:00:%02d %s\n' "$1" "$i" "$((i % 60))" "${4:-ok}"
  done
}

# console <name> <content>: a console log the stub can serve; the serial
# console ends its lines in \r\n, and a login prompt follows the last probe.
console() {
  printf '%s\n' "$2" | sed 's/$/\r/' >"$STUB/consoles/$1"
  printf 'login: \r\n' >>"$STUB/consoles/$1"
}

# --- Test 1 ---------------------------------------------------------------
test_gl_window() {
  echo "Test: gl_window judges a window by its seqs, and gl_last_seq reads the highest"
  local ok
  ok="$(lines net 1 9; lines disk 1 30)"

  run_lib -- "gl_window net 5 3 <<'EOF'
$ok
EOF"
  assert_eq "a contiguous all-ok window passes" "0" "$GL_RC"
  assert_eq "it names the window" "OK: net 6..9 all ok" "$GL_OUT"

  run_lib -- "gl_window net 5 3 <<'EOF'
$(lines net 1 6; lines net 7 7 lost; lines net 8 9)
EOF"
  assert_eq "a lost line fails" "1" "$GL_RC"
  assert_eq "it names the line" "FAIL: net line 7 says lost: guest-probe net 7 10:00:07 lost" "$GL_OUT"

  run_lib -- "gl_window net 5 3 <<'EOF'
$(lines net 1 7; lines net 9 10)
EOF"
  assert_eq "a missing seq fails" "1" "$GL_RC"
  assert_eq "it names the gap" "FAIL: net sequence jumps from 7 to 9" "$GL_OUT"

  run_lib -- "gl_window net 12 3 <<'EOF'
$(lines net 1 12)
EOF"
  assert_eq "a window with no new line fails" "1" "$GL_RC"
  assert_eq "it counts the lines" "FAIL: 0 net lines after 12, want 3" "$GL_OUT"

  run_lib -- "gl_window disk 4 3 <<'EOF'
$(lines disk 6 9)
EOF"
  assert_eq "a window that starts after the next seq fails as a gap" \
    "FAIL: disk sequence jumps from 4 to 6" "$GL_OUT"

  run_lib -- "gl_window net 5 3 lost <<'EOF'
$(lines net 1 5; lines net 6 9 lost)
EOF"
  assert_eq "a window of the given result passes" "OK: net 6..9 all lost" "$GL_OUT"

  run_lib -- "gl_window net 5 3 lost <<'EOF'
$(lines net 1 5; lines net 6 7 lost; lines net 8 8; lines net 9 9 lost)
EOF"
  assert_eq "an ok line in it fails" "1" "$GL_RC"
  assert_eq "it names the line" "FAIL: net line 8 says ok: guest-probe net 8 10:00:08 ok" "$GL_OUT"

  run_lib -- "gl_window net 0 1 </dev/null"
  assert_eq "gl_window fails on empty stdin" "1" "$GL_RC"
  assert_eq "it says there is no line" "FAIL: no net line in the console log" "$GL_OUT"

  run_lib -- "gl_last_seq net </dev/null"
  assert_eq "gl_last_seq fails on empty stdin" "1" "$GL_RC"
  assert_eq "it says there is no line" "FAIL: no net line in the console log" "$GL_OUT"

  run_lib -- "gl_last_seq net <<'EOF'
$ok
EOF"
  assert_eq "gl_last_seq prints the highest seq of its kind" "9" "$GL_OUT"
}

# --- Test 2 ---------------------------------------------------------------
test_gl_console() {
  echo "Test: gl_console keeps the whole probe lines and names a server without any"
  console kernel "[    0.000000] Linux version 5.15.0-71-generic
[    1.234567] Run /init as init process"
  console mixed "[    2.000000] random: crng init done
guest-probe dhcp 1 10:00:01 ok 10.241.0.5
noise guest-probe net 1 10:00:02 ok
guest-probe net 2 10:00:04 maybe
guest-probe disk 1 10:00:04 ok 1"
  console cut "guest-probe net 1 10:00:02 ok
guest-probe net 2 10:00:04 ok"

  run_lib STUB_CONSOLES=fail -- 'gl_console probe'
  assert_eq "a failed read returns 1" "1" "$GL_RC"
  assert_eq "it names the server" "FAIL: no guest-probe line in the console log of probe" "$GL_OUT"

  run_lib STUB_CONSOLES=kernel -- 'gl_console probe'
  assert_eq "a log of kernel lines returns 1" "1" "$GL_RC"
  assert_eq "it names the server" "FAIL: no guest-probe line in the console log of probe" "$GL_OUT"

  run_lib STUB_CONSOLES=mixed -- 'gl_console nova-pool-guest-probe'
  assert_eq "a log with probe lines passes" "0" "$GL_RC"
  assert_eq "it keeps the probe lines, without \\r and without what precedes them" \
    "guest-probe dhcp 1 10:00:01 ok 10.241.0.5
guest-probe net 1 10:00:02 ok
guest-probe disk 1 10:00:04 ok 1" "$GL_OUT"
  assert_contains "it reads the server's log in the client pod" "$(calls)" \
    "exec -n openstack nova-pool-guest-client -- openstack console log show nova-pool-guest-probe"

  printf 'guest-probe net 1 10:00:02 ok\r\nguest-probe net 2 10:00:04 ok\r\nguest-probe net 3 10:00:0\n' \
    >"$STUB/consoles/cut"
  run_lib STUB_CONSOLES=cut -- 'gl_console probe'
  assert_eq "the last line of the log, which a read can cut short, is dropped" \
    "guest-probe net 1 10:00:02 ok
guest-probe net 2 10:00:04 ok" "$GL_OUT"
}

# --- Test 3 ---------------------------------------------------------------
test_gl_wait_window() {
  echo "Test: gl_wait_window polls through a failed read and times out with the last failure"
  console upto12 "$(lines net 1 12)"
  console upto15 "$(lines net 1 15)"
  console lost "$(lines net 1 12; lines net 13 13 lost; lines net 14 16)"

  run_lib GL_POLL=1 STUB_CONSOLES=upto12 -- 'gl_wait_window probe net 12 3 2'
  assert_eq "a window that stays short fails at the bound" "1" "$GL_RC"
  assert_eq "with the last failure prefixed by the bound" \
    "FAIL: after 2 s: 0 net lines after 12, want 3" "$GL_OUT"

  run_lib STUB_CONSOLES=fail -- 'gl_wait_window probe net 0 3 0'
  assert_eq "a log that cannot be read fails at the bound too" \
    "FAIL: after 0 s: no guest-probe line in the console log of probe" "$GL_OUT"

  run_lib STUB_CONSOLES=fail,upto15 -- 'gl_wait_window probe net 12 3 60; echo after'
  assert_eq "it passes once the read works again" "0" "$GL_RC"
  assert_eq "and the caller goes on under set -e" "OK: net 13..15 all ok
after" "$GL_OUT"
  assert_eq "it read the console twice" "2" "$(grep -c 'console log show' "$STUB/state/calls")"

  run_lib STUB_CONSOLES=lost -- 'gl_wait_window probe net 12 3 60'
  assert_eq "a lost line ends the wait" "1" "$GL_RC"
  assert_eq "with its own message" "FAIL: net line 13 says lost: guest-probe net 13 10:00:13 lost" "$GL_OUT"
  assert_eq "after one read" "1" "$(grep -c 'console log show' "$STUB/state/calls")"

  console deny2 "$(lines net 1 12; lines net 13 14 lost)"
  console deny3 "$(lines net 1 12; lines net 13 15 lost)"
  run_lib STUB_CONSOLES=deny2,deny3 -- 'gl_wait_window probe net 12 3 60 lost'
  assert_eq "it waits for a window of the given result" "OK: net 13..15 all lost" "$GL_OUT"
  assert_eq "over two reads" "2" "$(grep -c 'console log show' "$STUB/state/calls")"

  run_lib STUB_CONSOLES=upto15 -- 'gl_wait_window probe net 12 3 60 lost'
  assert_eq "an ok line ends a wait for lost lines at once" \
    "FAIL: net line 13 says ok: guest-probe net 13 10:00:13 ok" "$GL_OUT"

  # A kernel message written to ttyS0 in the middle of a probe line.
  console garbled "$(lines net 1 13
    printf 'guest-probe net 14 10:00:1[   12.3] sh (99): drop_caches: 1\n4 ok\n'
    lines net 15 17)"
  run_lib STUB_CONSOLES=garbled -- 'gl_wait_window probe net 12 3 60'
  assert_eq "a line garbled by a kernel message ends the wait as a gap" \
    "FAIL: net sequence jumps from 13 to 15" "$GL_OUT"
  assert_eq "after one read too" "1" "$(grep -c 'console log show' "$STUB/state/calls")"
}

# --- Test 4 ---------------------------------------------------------------
test_gl_wait_result() {
  echo "Test: gl_wait_result prints the seq of the first matching line"
  console deny "$(lines net 1 5; lines net 6 8 lost; lines net 9 10)"

  run_lib STUB_CONSOLES=deny -- 'gl_wait_result probe net lost 4 60'
  assert_eq "it returns 0" "0" "$GL_RC"
  assert_eq "it prints the first lost seq after 4" "6" "$GL_OUT"

  run_lib STUB_CONSOLES=deny -- 'gl_wait_result probe net ok 6 60'
  assert_eq "it prints the first ok seq after 6" "9" "$GL_OUT"

  run_lib STUB_CONSOLES=fail,deny -- 'gl_wait_result probe net lost 6 60'
  assert_eq "it polls through a failed read" "7" "$GL_OUT"

  run_lib STUB_CONSOLES=deny -- 'gl_wait_result probe net lost 8 0'
  assert_eq "no match within the bound returns 1" "1" "$GL_RC"
  assert_eq "it names the kind, the result and the bound" \
    "FAIL: no net lost line after 8 within 0 s" "$GL_OUT"
}

# --- Test 5 ---------------------------------------------------------------
# proc_entry <pid> <argv...>: a process of the fake proc tree.
proc_entry() {
  local pid="$1"
  shift
  mkdir -p "$TMP_DIR/proc/$pid"
  if [ "$#" -gt 0 ]; then
    printf '%s\0' "$@" >"$TMP_DIR/proc/$pid/cmdline"
  else
    : >"$TMP_DIR/proc/$pid/cmdline"
  fi
}

test_gl_pids() {
  echo "Test: the process scan finds QEMU by its -name and libvirtd by argv[0]"
  rm -rf "$TMP_DIR/proc"
  proc_entry 1 /sbin/init
  proc_entry 2
  proc_entry 100 /sbin/libvirtd
  proc_entry 200 /bin/qemu-system-x86_64 -name guest=instance-0000000a,debug-threads=on -S
  proc_entry 300 /bin/qemu-system-x86_64 -name guest=instance-0000000ab,debug-threads=on
  proc_entry 400 /bin/virsh domstate instance-0000000a
  proc_entry 500 /bin/libvirtd-helper guest=instance-0000000b,

  run_lib STUB_PROC="$TMP_DIR/proc" -- 'gl_pids instance-0000000a'
  assert_eq "it returns 0 on one QEMU and one libvirtd" "0" "$GL_RC"
  assert_eq "it prints both PIDs" "qemu=200 libvirtd=100" "$GL_OUT"
  assert_contains "it scans in the Running libvirt pod that is not terminating" "$(calls)" \
    "exec -n openstack libvirt-e2e-abc -c libvirtd -- bash -c"

  # The scan's own shell names the domain on its command line.
  local scan
  # shellcheck source=tests/e2e-nova-libvirt/guest-dataplane/guest-lib.sh
  scan="$(. "$LIB" && printf '%s' "$GL_PIDS_SCRIPT")"
  proc_entry 700 bash -c "$scan" gl-pids instance-0000000a
  run_lib STUB_PROC="$TMP_DIR/proc" -- 'gl_pids instance-0000000a'
  assert_eq "the argv of a scanning shell does not count" "qemu=200 libvirtd=100" "$GL_OUT"

  # The scan skips its own PID whatever that argv holds: it runs as the PID
  # of the only entry of a proc tree, an entry that would count as QEMU.
  GL_OUT="$(
    mkdir -p "$TMP_DIR/self/$BASHPID"
    printf '%s\0' bash -c guest=instance-0000000a, >"$TMP_DIR/self/$BASHPID/cmdline"
    exec bash -c "$scan" gl-pids instance-0000000a "$TMP_DIR/self"
  )"
  assert_eq "the scanning shell's own PID does not count" "qemu= libvirtd=" "$GL_OUT"

  proc_entry 600 /bin/qemu-system-x86_64 -name guest=instance-0000000a,debug-threads=on
  run_lib STUB_PROC="$TMP_DIR/proc" -- 'gl_pids instance-0000000a'
  assert_eq "two QEMU processes of the domain fail" "1" "$GL_RC"
  assert_eq "it names them" "FAIL: 2 QEMU processes of instance-0000000a in libvirt-e2e-abc: 200,600" "$GL_OUT"

  run_lib STUB_PROC="$TMP_DIR/proc" -- 'gl_pids instance-0000000c'
  assert_eq "no QEMU of the domain fails" "FAIL: 0 QEMU processes of instance-0000000c in libvirt-e2e-abc: none" "$GL_OUT"

  rm -rf "$TMP_DIR/proc/100"
  run_lib STUB_PROC="$TMP_DIR/proc" -- 'gl_pids instance-0000000b'
  assert_eq "no libvirtd fails" "FAIL: 0 libvirtd processes in libvirt-e2e-abc: none" "$GL_OUT"

  run_lib STUB_LIBVIRT_PODS='{"items":[]}' -- 'gl_pids instance-0000000a'
  assert_eq "no libvirt pod fails" "1" "$GL_RC"
  assert_eq "it says so" "FAIL: no Running libvirt-e2e pod" "$GL_OUT"

  run_lib -- 'gl_pid qemu "qemu=200 libvirtd=100"; gl_pid libvirtd "qemu=200 libvirtd=100"'
  assert_eq "gl_pid reads one PID each" "200
100" "$GL_OUT"
}

# --- Test 5b --------------------------------------------------------------
test_gl_wait_status() {
  echo "Test: gl_wait_status and gl_wait_compute_up poll until the wanted state"

  run_lib STUB_SERVER_SEQ="BUILD,BUILD,ACTIVE" -- 'gl_wait_status server nova-pool-guest-peer ACTIVE 60'
  assert_eq "a server that goes ACTIVE passes" "OK: server nova-pool-guest-peer is ACTIVE" "$GL_OUT"
  assert_eq "after three reads" "3" "$(grep -c 'server show nova-pool-guest-peer -f value -c status' "$STUB/state/calls")"

  run_lib STUB_SERVER_SEQ="BUILD,ERROR" -- 'gl_wait_status server nova-pool-guest-peer ACTIVE 60'
  assert_eq "a server in ERROR fails at once" "1" "$GL_RC"
  assert_eq "with its fault" \
    "FAIL: server nova-pool-guest-peer is ERROR: {'code': 500, 'message': 'No valid host was found.'}" "$GL_OUT"

  run_lib STUB_VOLUME_SEQ="creating" -- 'gl_wait_status volume nova-pool-guest-vol available 0'
  assert_eq "a volume that stays creating fails at the bound" \
    "FAIL: volume nova-pool-guest-vol is creating, not available, after 0 s" "$GL_OUT"

  run_lib STUB_COMPUTE_SEQ="fail,down 2026-11-06T09:59:58.000000,up 2026-11-06T10:00:08.000000,up 2026-11-06T10:00:18.000000" -- \
    'gl_wait_compute_up kind-control-plane 60 2026-11-06T10:00:10'
  assert_eq "a compute service that reports up after the given time passes" \
    "OK: the compute service of kind-control-plane is up, last report at 2026-11-06T10:00:18.000000" "$GL_OUT"
  assert_eq "after four reads" "4" "$(grep -c 'compute service list' "$STUB/state/calls")"
  assert_contains "it asks for the host's nova-compute" "$(calls)" \
    "openstack compute service list --service nova-compute --host kind-control-plane -f value -c State -c Updated At"

  # Nova reads a killed nova-compute as up for 60 s after its last report.
  run_lib STUB_COMPUTE_SEQ="up 2026-11-06T10:00:08.000000" -- \
    'gl_wait_compute_up kind-control-plane 0 2026-11-06T10:00:10'
  assert_eq "an up from a report before the given time fails" "1" "$GL_RC"
  assert_eq "naming the report" \
    "FAIL: the compute service of kind-control-plane is up with its last report at 2026-11-06T10:00:08.000000, not up with a report after 2026-11-06T10:00:10, after 0 s" "$GL_OUT"

  run_lib STUB_COMPUTE_SEQ="up 2026-11-06T10:00:10.000000" -- \
    'gl_wait_compute_up kind-control-plane 0 2026-11-06T10:00:10'
  assert_eq "a report in the second of the given time does not count" "1" "$GL_RC"

  run_lib STUB_COMPUTE_SEQ="down 2026-11-06T10:00:18.000000" -- \
    'gl_wait_compute_up kind-control-plane 0 2026-11-06T10:00:10'
  assert_eq "a compute service that stays down fails" \
    "FAIL: the compute service of kind-control-plane is down with its last report at 2026-11-06T10:00:18.000000, not up with a report after 2026-11-06T10:00:10, after 0 s" "$GL_OUT"
}

# --- Test 6 ---------------------------------------------------------------
test_gl_pod_kill() {
  echo "Test: gl_pod_kill applies a labelled pod-kill and waits for AllInjected"
  local applied

  run_lib -- 'gl_pod_kill guest-nova-compute-kill app.kubernetes.io/name=novacompute,app.kubernetes.io/instance=pool-libvirt'
  applied="$(cat "$STUB/state/applied")"
  assert_eq "it returns 0" "0" "$GL_RC"
  assert_eq "it says so" "OK: PodChaos guest-nova-compute-kill is AllInjected" "$GL_OUT"
  assert_contains "the object carries the suite label" "$applied" \
    "  labels:
    chaos.c5c3.io/suite: nova-libvirt-guest-dataplane"
  assert_contains "it kills one pod" "$applied" "  action: pod-kill
  mode: one"
  assert_contains "it selects by both labels in the namespace" "$applied" \
    "    namespaces: [openstack]
    labelSelectors:
      app.kubernetes.io/name: novacompute
      app.kubernetes.io/instance: pool-libvirt"
  assert_contains "it waits for AllInjected" "$(calls)" \
    "wait podchaos/guest-nova-compute-kill -n openstack --for=condition=AllInjected --timeout=120s"

  run_lib STUB_INJECTED=fail -- 'gl_pod_kill guest-libvirt-kill app.kubernetes.io/name=libvirt-e2e'
  assert_eq "a missed AllInjected fails" "1" "$GL_RC"
  assert_eq "it prints the describe output, then its message" \
    "Name: guest-libvirt-kill (described)
FAIL: PodChaos guest-libvirt-kill did not reach AllInjected within 120 s" "$GL_OUT"
}

# --- Test 7 ---------------------------------------------------------------
test_gl_wait_replaced() {
  echo "Test: gl_wait_replaced waits for a Ready pod with another UID"
  local old new terminating
  old='{"items":[{"metadata":{"name":"p-old","uid":"u-old"}}]}'
  terminating='{"items":[{"metadata":{"name":"p-old","uid":"u-old","deletionTimestamp":"2026-11-06T10:00:00Z"}},{"metadata":{"name":"p-gone","uid":"u-gone","deletionTimestamp":"2026-11-06T10:00:00Z"}}]}'
  new='{"items":[{"metadata":{"name":"p-new","uid":"u-new"}}]}'

  run_lib STUB_PODS_SEQ="$old|$terminating|$new" -- 'gl_wait_replaced app.kubernetes.io/name=novacompute,app.kubernetes.io/instance=pool-libvirt u-old 60'
  assert_eq "a new UID that turns Ready passes" "0" "$GL_RC"
  assert_starts_with "it names the new pod" "$GL_OUT" "OK: p-new (u-new) is Ready after "
  assert_contains "it waits for that pod" "$(calls)" "wait pod p-new -n openstack --for=condition=Ready"
  assert_not_contains "it never waits for a terminating pod" "$(calls)" "wait pod p-gone"

  run_lib STUB_PODS_SEQ="$old" -- 'gl_wait_replaced app.kubernetes.io/name=novacompute,app.kubernetes.io/instance=pool-libvirt u-old 0'
  assert_eq "the old UID alone fails" "1" "$GL_RC"
  assert_eq "it names the selector, the UID and the bound" \
    "FAIL: no Ready pod of app.kubernetes.io/name=novacompute,app.kubernetes.io/instance=pool-libvirt other than u-old within 0 s" "$GL_OUT"

  run_lib STUB_PODS_SEQ="$new" STUB_READY=fail -- 'gl_wait_replaced app.kubernetes.io/name=novacompute,app.kubernetes.io/instance=pool-libvirt u-old 0'
  assert_eq "a new pod that never turns Ready fails" "1" "$GL_RC"
}

# --- Test 8 ---------------------------------------------------------------
# junit <path> <testcase xml...>: a JUnit report of the given testcases.
junit() {
  local path="$1"
  shift
  {
    echo '<testsuite errors="0" failures="0" name="" tests="4" time="1.0">'
    printf '%s\n' "$@"
    echo '</testsuite>'
  } >"$path"
}

test_gl_tempest_verdict() {
  echo "Test: gl_tempest_verdict wants every test passed and none skipped"
  local ids basic advanced detach old
  ids="tempest.scenario.test_server_basic_ops.TestServerBasicOps.test_server_basic_ops tempest.scenario.test_server_advanced_ops.TestServerAdvancedOps.test_server_sequence_suspend_resume tempest.scenario.test_server_volume_attachment.TestServerVolumeAttachmentScenario.test_server_detach_rules tempest.scenario.test_server_volume_attachment.TestServerVolumeAttachScenarioOldVersion.test_old_versions_reject"
  basic='<testcase classname="tempest.scenario.test_server_basic_ops.TestServerBasicOps" name="test_server_basic_ops[compute,id-7fff3fb3-91d8-4fd0-bd7d-0204f1f180ba,network,smoke]" time="60.0" />'
  advanced='<testcase classname="tempest.scenario.test_server_advanced_ops.TestServerAdvancedOps" name="test_server_sequence_suspend_resume[compute,id-949da7d5-72c8-4808-8802-e3d70df98e2c,slow]" time="90.0" />'
  detach='<testcase classname="tempest.scenario.test_server_volume_attachment.TestServerVolumeAttachmentScenario" name="test_server_detach_rules[compute,id-be615530-f105-437a-8afe-ce998c9535d9,image,network,slow,volume]" time="120.0" />'
  old='<testcase classname="tempest.scenario.test_server_volume_attachment.TestServerVolumeAttachScenarioOldVersion" name="test_old_versions_reject[compute,id-6f4d2144-99f4-495c-8b0b-c6a537971418,image,network,slow,volume]" time="80.0" />'

  junit "$TMP_DIR/pass.xml" "$basic" "$advanced" "$detach" "$old"
  run_lib -- "gl_tempest_verdict '$TMP_DIR/pass.xml' $ids"
  assert_eq "four passed tests pass" "0" "$GL_RC"
  assert_eq "it says so" "OK: Tempest passed the 4 scenario tests, none skipped" "$GL_OUT"

  junit "$TMP_DIR/skip.xml" "$basic" \
    '<testcase classname="tempest.scenario.test_server_advanced_ops.TestServerAdvancedOps" name="test_server_sequence_suspend_resume[compute,slow]" time="0.0"><skipped>Suspend is not available.</skipped></testcase>' \
    "$detach" "$old"
  run_lib -- "gl_tempest_verdict '$TMP_DIR/skip.xml' $ids"
  assert_eq "a skipped test fails" "1" "$GL_RC"
  assert_eq "it names the test and the reason" \
    "FAIL: Tempest skipped tempest.scenario.test_server_advanced_ops.TestServerAdvancedOps.test_server_sequence_suspend_resume: Suspend is not available." "$GL_OUT"

  junit "$TMP_DIR/class-skip.xml" "$basic" "$advanced" "$detach" \
    '<testcase classname="" name="setUpClass (tempest.scenario.test_server_volume_attachment.TestServerVolumeAttachScenarioOldVersion)" time="0.0"><skipped>Cinder is not available</skipped></testcase>'
  run_lib -- "gl_tempest_verdict '$TMP_DIR/class-skip.xml' $ids"
  assert_eq "a class skipped in its fixture fails" \
    "FAIL: Tempest skipped tempest.scenario.test_server_volume_attachment.TestServerVolumeAttachScenarioOldVersion.test_old_versions_reject: Cinder is not available" "$GL_OUT"

  junit "$TMP_DIR/fail.xml" "$basic" "$advanced" \
    '<testcase classname="tempest.scenario.test_server_volume_attachment.TestServerVolumeAttachmentScenario" name="test_server_detach_rules[compute,slow]" time="1.0"><failure message="409 Conflict">tb</failure></testcase>' \
    "$old"
  run_lib -- "gl_tempest_verdict '$TMP_DIR/fail.xml' $ids"
  assert_eq "a failed test fails" "1" "$GL_RC"
  assert_eq "it names the test and the failure" \
    "FAIL: Tempest failed tempest.scenario.test_server_volume_attachment.TestServerVolumeAttachmentScenario.test_server_detach_rules: 409 Conflict" "$GL_OUT"

  # The row hack/tempest/merge-retry-junit.py leaves of a class fixture that
  # failed on the first run while every test of the class passed on retry.
  junit "$TMP_DIR/flake.xml" "$basic" "$advanced" "$detach" \
    '<testcase classname="" name="setUpClass (tempest.scenario.test_server_volume_attachment.TestServerVolumeAttachScenarioOldVersion)" time="0.0"><system-out>flaky: class fixture failed on first run, all tests of the class passed on retry</system-out></testcase>'
  run_lib -- "gl_tempest_verdict '$TMP_DIR/flake.xml' $ids"
  assert_eq "a class fixture resolved on the retry counts as passed" "0" "$GL_RC"

  junit "$TMP_DIR/missing.xml" "$basic" "$advanced" "$detach"
  run_lib -- "gl_tempest_verdict '$TMP_DIR/missing.xml' $ids"
  assert_eq "a missing test fails" "1" "$GL_RC"
  assert_eq "it names the report and the test" \
    "FAIL: $TMP_DIR/missing.xml has no testcase tempest.scenario.test_server_volume_attachment.TestServerVolumeAttachScenarioOldVersion.test_old_versions_reject" "$GL_OUT"
}

# --- Test 9 ---------------------------------------------------------------
test_gl_teardown_guests() {
  echo "Test: gl_teardown_guests removes the servers before the volume and the network"

  run_lib STUB_SERVERS_SEQ="id-1 id-2,id-1,none" STUB_VOLUME_SEQ="detaching,available" -- 'gl_teardown_guests'
  assert_eq "it returns 0" "0" "$GL_RC"
  assert_contains "it reports the servers gone" "$GL_OUT" \
    "OK: nova-pool-guest-probe and nova-pool-guest-peer are gone"
  assert_contains "it deletes both servers in one call" "$(calls)" \
    "openstack server delete nova-pool-guest-probe nova-pool-guest-peer"
  assert_contains "it lists the two servers by name" "$(calls)" \
    "openstack server list --name ^(nova-pool-guest-probe|nova-pool-guest-peer)\$ -f value -c ID"
  assert_eq "it deletes the volume once it is available" "1" \
    "$(grep -c 'openstack volume delete nova-pool-guest-vol$' "$STUB/state/calls")"
  assert_eq "the volume goes after the servers" \
    "$(grep -n 'openstack server list' "$STUB/state/calls" | tail -n 1 | cut -d: -f1)" \
    "$(($(grep -n 'openstack volume show' "$STUB/state/calls" | head -n 1 | cut -d: -f1) - 1))"
  assert_contains "it deletes the network last" "$(tail -n 2 "$STUB/state/calls")" \
    "openstack network delete nova-pool-guest-net"

  run_lib STUB_VOLUME_SEQ=gone STUB_NETWORK=net-1 -- 'gl_teardown_guests'
  assert_eq "a network left over fails" "1" "$GL_RC"
  assert_contains "it says so" "$GL_OUT" "FAIL: nova-pool-guest-net is left after the teardown"
  assert_not_contains "a volume already gone is not deleted" "$(calls)" "volume delete"
}

# --- Test 10 --------------------------------------------------------------
test_state_dump_and_sourcing() {
  echo "Test: gl_recall, gl_dump and sourcing the library"

  run_lib STUB_CM='{"data":{"node":"kind-control-plane"}}' -- 'gl_recall node'
  assert_eq "gl_recall prints a value" "kind-control-plane" "$GL_OUT"
  run_lib -- 'gl_recall instance-name'
  assert_eq "an absent key fails" "1" "$GL_RC"
  assert_eq "naming the key" "FAIL: ConfigMap nova-pool-guest holds no instance-name" "$GL_OUT"

  run_lib STUB_ALL_FAIL=1 -- 'gl_dump >/dev/null; gl_dump_chaos >/dev/null; echo done'
  assert_eq "gl_dump and gl_dump_chaos exit 0 with every kubectl call failing" "0" "$GL_RC"
  assert_eq "and the caller goes on" "done" "$GL_OUT"

  assert_eq "bash -n passes" "0" "$(bash -n "$LIB" >/dev/null 2>&1 && echo 0 || echo 1)"
  assert_file_contains_fixed "make shellcheck lints the library and the user-data" \
    "$PROJECT_ROOT/Makefile" "shellcheck --severity=warning tests/e2e-nova-libvirt/*/*.sh"
  run_lib -- 'true'
  assert_eq "sourcing it returns 0" "0" "$GL_RC"
  assert_eq "sourcing it calls no kubectl" "" "$(calls)"
}

# --- Test 11 --------------------------------------------------------------
test_user_data_matches_parser() {
  echo "Test: the lines guest-user-data.sh writes are the lines the library reads"
  local ud="$PROJECT_ROOT/tests/e2e-nova-libvirt/guest-dataplane/guest-user-data.sh" probe

  assert_eq "the rendered user-data names the peer" "PEER=10.241.0.9" \
    "$(sed 's|@PEER@|10.241.0.9|' "$ud" | grep '^PEER=')"

  # The user-data's own probe function, appending to a file in place of ttyS0.
  probe="$(sed -n '/^probe() {/,/^}/p' "$ud" | sed "s|>/dev/ttyS0|>>$TMP_DIR/tty|")"
  : >"$TMP_DIR/tty"
  sh -c "$probe
probe dhcp 1 ok 10.241.0.5; probe net 1 ok; probe net 2 ok; probe disk 1 ok 1"
  run_lib -- "grep -cE \"\$GL_PROBE_RE\" '$TMP_DIR/tty'"
  assert_eq "GL_PROBE_RE matches every line" "4" "$GL_OUT"
  run_lib -- "grep -oE \"\$GL_PROBE_RE\" '$TMP_DIR/tty' | gl_window net 0 2"
  assert_eq "gl_window reads the net lines" "OK: net 1..2 all ok" "$GL_OUT"
  run_lib -- "grep -oE \"\$GL_PROBE_RE\" '$TMP_DIR/tty' | gl_window disk 0 1"
  assert_eq "and the disk line" "OK: disk 1..1 all ok" "$GL_OUT"
}

# --- Test 12 --------------------------------------------------------------
# raw <name> <content> <tail>: a console log file as step 9 reads it, its
# lines ending in \r\n, then tail, then the EOF marker step 9 appends.
raw() {
  {
    printf '%s\n' "$2" | sed 's/$/\r/'
    printf '%sEOF\n' "$3"
  } >"$TMP_DIR/$1"
}

test_backend_read() {
  echo "Test: gl_last_ok_disk reads the last ok disk line, gl_backend_holds the volume"
  raw newline "$(lines disk 1 7; lines net 1 9; lines disk 8 8)" ""
  raw cut "$(lines disk 1 8; lines net 1 9)" "guest-probe disk 9 10:0"
  raw joined "$(lines disk 1 8)" "guest-probe disk 9 10:00:09 ok 9"
  raw error "$(lines disk 1 5; lines net 1 9; lines disk 6 6 error)" ""
  raw none "$(lines net 1 9)" ""

  run_lib -- "gl_last_ok_disk <'$TMP_DIR/newline'"
  assert_eq "a log that ends in a newline keeps its last line" "8" "$GL_OUT"
  run_lib -- "gl_last_ok_disk <'$TMP_DIR/cut'"
  assert_eq "a line the stop cut short is dropped" "8" "$GL_OUT"
  run_lib -- "gl_last_ok_disk <'$TMP_DIR/joined'"
  assert_eq "a last line without its newline is dropped" "8" "$GL_OUT"
  run_lib -- "gl_last_ok_disk <'$TMP_DIR/error'"
  assert_eq "a disk line that is not ok does not count" "5" "$GL_OUT"
  run_lib -- "gl_last_ok_disk <'$TMP_DIR/none'"
  assert_eq "a log without an ok disk line fails" "1" "$GL_RC"
  assert_eq "it says so" "FAIL: no ok disk line in the console log" "$GL_OUT"

  run_lib -- 'gl_backend_holds probe-8 8'
  assert_eq "the counter of the last ok line passes" \
    "OK: the volume file holds probe-8, the last ok disk line counted 8" "$GL_OUT"
  run_lib -- 'gl_backend_holds probe-9 8'
  assert_eq "the next counter passes" "0" "$GL_RC"
  run_lib -- 'gl_backend_holds probe-10 8'
  assert_eq "a counter two ahead fails" "1" "$GL_RC"
  run_lib -- "gl_backend_holds '' 8"
  assert_eq "an empty read fails" "1" "$GL_RC"
  assert_eq "naming the value" "FAIL: the volume file holds '', the last ok disk line counted 8" "$GL_OUT"
}

test_gl_window
test_gl_console
test_gl_wait_window
test_gl_wait_result
test_gl_pids
test_gl_wait_status
test_gl_pod_kill
test_gl_wait_replaced
test_gl_tempest_verdict
test_gl_teardown_guests
test_state_dump_and_sourcing
test_user_data_matches_parser
test_backend_read

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
