#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the lab chaos runner, hack/lab-chaos.sh (#1395), against stub kubectl
# and openstack CLIs:
#   1. Without a subcommand it prints the usage and exits 2, and
#      `make -n lab-chaos-<subcommand>` calls it with the subcommand.
#   2. The settings and the context: an unset LAB_CHAOS_CONTEXT and a kind-
#      context each print one FAIL: line naming the cause before any kubectl
#      call; a malformed LAB_CHAOS_RUN or LAB_CHAOS_RANDOM_DURATION is refused.
#   3. preflight on a lab without the Cinder backend nfs1 prints one FAIL:
#      line naming it, and on a lab with a server of a previous session one
#      naming the server; a complete lab passes every check.
#   4. A LAB_CHAOS_GO_AHEAD that differs from the shoot refuses proving,
#      faults, random, cleanup and teardown-check before any write.
#   5. recovered, the gate before the next fault, holds on alive agents and
#      fails on an empty agent list, a failed agent call and a dead agent;
#      probes_ok holds on new ok lines of both probes and fails on a stalled
#      probe and on a console read that prints nothing (the cases the docs
#      test pinned while the helpers lived on the page).
#   6. The helpers of faults.tsv: lc_console_stats counts the lost and error
#      lines and the largest gaps inside the window alone, lc_state_diff
#      compares domain by its running_ prefix, agents by holding True alone,
#      nfs-server by phase and ready flag and every other field as text.
#   7. The user-data of lab-chaos-0 waits for /dev/vdb, pings the peer's
#      address and writes the counter at byte 4096 of the volume.
#   8. cleanup deletes the servers, the volumes, the subnet, the network, the
#      server group and the runner's chaos objects, and a second call deletes
#      nothing and exits 0.
#   9. record writes record.md from the run directory: the paragraph with the
#      date, the commit, the shoot, the nodes and the chart, the proving and
#      fault tables, and the random run's row.
#
# The runner is sourced in a subshell (its BASH_SOURCE guard keeps lc_main
# from running) with the stubs first on PATH, the waits shortened, and
# lc_clouds, which writes the clouds.yaml with hack/dizzy.sh, replaced.
#
# Usage: bash tests/unit/hack/lab_chaos_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
LAB_CHAOS_SH="$PROJECT_ROOT/hack/lab-chaos.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

MUTATING='^(kubectl (apply|create|delete|patch|scale|label|annotate)|openstack (server|volume|network|subnet) (create|delete|add|reboot|stop))'

# ---------------------------------------------------------------------------
# Stubs
# ---------------------------------------------------------------------------

# make_stubs <dir> — kubectl and openstack stubs in <dir>/bin that append
# their argv to $CALL_LOG. The lab state lives in <dir>/state:
#   kubectl: shoot (the shoot name), contexts (one context per line),
#            nfs1 (present: the backend is Ready), chaos (lines of the
#            runner's chaos objects)
#   openstack: servers, volumes (one name per line), network, subnet, group
#            (present: exists), agents (the Alive column; absent: the call
#            fails), console (the console log), console-step (lines a
#            console read adds)
make_stubs() {
  local dir="$1"
  mkdir -p "$dir/bin" "$dir/state"
  echo newforge >"$dir/state/shoot"
  printf 'garden-lab\nkind-cobaltcore\n' >"$dir/state/contexts"
  : >"$dir/state/nfs1"
  : >"$dir/state/servers"
  : >"$dir/state/volumes"
  cat >"$dir/bin/kubectl" <<'STUB'
#!/bin/bash
s="$(cd "$(dirname "$0")/.." && pwd)/state"
echo "kubectl $*" >>"$CALL_LOG"
case "$*" in
  "config view --raw --minify --flatten --context "*)
    ctx="${!#}"
    grep -qxF "$ctx" "$s/contexts" || { echo "error: context \"$ctx\" does not exist" >&2; exit 1; }
    echo "apiVersion: v1" ;;
  "get configmap shoot-info "*) cat "$s/shoot" | tr -d '\n' ;;
  "get helmrelease chaos-mesh "*) printf True ;;
  "rollout status daemonset/chaos-daemon "*) echo 'daemon set "chaos-daemon" successfully rolled out' ;;
  "get nodes -o name") printf 'node/a\nnode/b\n' ;;
  "logs -n chaos-mesh "*) printf '[pod/a/load] chaos-mesh-modules: ip_set are loaded\n[pod/b/load] chaos-mesh-modules: ip_set are loaded\n' ;;
  "get namespace dizzy "*) echo namespace/dizzy ;;
  "get controlplane controlplane "*) printf True ;;
  "get images.openstack.k-orc.cloud/hvo-cirros-kvm "*)
    echo '{"items":[{"status":{"conditions":[{"type":"Available","status":"True"}]}},{"status":{"conditions":[{"type":"Available","status":"True"}]}}]}' ;;
  "get cinderbackend nfs1 "*)
    [ -f "$s/nfs1" ] || { echo 'Error from server (NotFound): cinderbackends.cinder.openstack.c5c3.io "nfs1" not found' >&2; exit 1; }
    printf True ;;
  "get podchaos,networkchaos "*) cat "$s/chaos" 2>/dev/null ;;
  "delete podchaos,networkchaos "*) rm -f "$s/chaos" ;;
  "get deployment nfs-server "*) printf 1 ;;
esac
exit 0
STUB
  cat >"$dir/bin/openstack" <<'STUB'
#!/bin/bash
s="$(cd "$(dirname "$0")/.." && pwd)/state"
echo "openstack $*" >>"$CALL_LOG"
case "$*" in
  "token issue "*) echo gAAAA ;;
  "volume type show nfs "*) echo nfs ;;
  "compute service list --service nova-compute -c State -f value") printf 'up\nup\n' ;;
  "compute service list --service nova-compute --host "*) echo up ;;
  "server list "*) cat "$s/servers" ;;
  "server delete "*) n="${!#}"; grep -vxF "$n" "$s/servers" >"$s/servers.new"; mv "$s/servers.new" "$s/servers" ;;
  "volume list "*)
    n="$(echo "$*" | sed 's/.*--name \([^ ]*\).*/\1/')"
    grep -xF "$n" "$s/volumes" || true ;;
  "volume delete "*) n="${!#}"; grep -vxF "$n" "$s/volumes" >"$s/volumes.new"; mv "$s/volumes.new" "$s/volumes" ;;
  "subnet show "*) [ -f "$s/subnet" ] || exit 1 ;;
  "subnet delete "*) rm -f "$s/subnet" ;;
  "network show "*) [ -f "$s/network" ] || exit 1 ;;
  "network delete "*) rm -f "$s/network" ;;
  "server group show "*) [ -f "$s/group" ] || exit 1 ;;
  "server group delete "*) rm -f "$s/group" ;;
  "server show "*"-c status -f value") echo "${SERVER_STATUS:-ACTIVE}" ;;
  "network agent list "*)
    [ -f "$s/agents" ] || { echo "Unable to establish connection to https://neutron.127-0-0-1.nip.io:8443" >&2; exit 1; }
    cat "$s/agents" ;;
  "console log show --lines 120 "*)
    [ -f "$s/console" ] || exit 0
    cat "$s/console"
    if [ -f "$s/console-step" ]; then cat "$s/console-step" >>"$s/console"; fi ;;
esac
exit 0
STUB
  chmod +x "$dir/bin/kubectl" "$dir/bin/openstack"
}

# new_case — Fresh stubs and call log.
new_case() {
  rm -rf "$TMP_DIR/case"
  make_stubs "$TMP_DIR/case"
  export CALL_LOG="$TMP_DIR/case/calls.log"
  : >"$CALL_LOG"
}

# in_lc [env_var=value...] — Source the runner in the current (sub)shell with
# the stubs first on PATH, the given settings, short waits, its output under
# the case directory and lc_clouds replaced.
# shellcheck disable=SC2034 # the sourced runner reads them
in_lc() {
  local assignment
  unset LAB_CHAOS_CONTEXT LAB_CHAOS_GO_AHEAD LAB_CHAOS_RUN LAB_CHAOS_RANDOM_DURATION
  for assignment in "$@"; do
    export "${assignment?}"
  done
  PATH="$TMP_DIR/case/bin:$PATH"
  TMPDIR="$TMP_DIR/case"
  export PATH TMPDIR
  # shellcheck source=hack/lab-chaos.sh
  source "$LAB_CHAOS_SH"
  LC_OUTPUT_DIR="$TMP_DIR/case/out"
  LC_POLL=0
  LC_PROBE_WINDOW=0
  LC_HOST=worker-a
  lc_clouds() { :; }
  lc_sleep() { :; }
}

# run_lc <subcommand> [env_var=value...] — Run lc_main in a subshell; OUT holds
# stdout and stderr, RC the exit code.
run_lc() {
  local sub="$1"
  shift
  RC=0
  OUT="$( (in_lc "$@"; lc_main "$sub") 2>&1)" || RC=$?
}

# mutations — The mutating calls of the call log.
mutations() {
  grep -E "$MUTATING" "$CALL_LOG" || true
}

# console_lines <net status> <disk status> <first counter> <n> — n lines of
# each probe, two seconds apart from 10:00:00.
console_lines() {
  awk -v net="$1" -v disk="$2" -v c="$3" -v n="$4" 'BEGIN {
    for (i = 0; i < n; i++) {
      t = 36000 + 2 * (c + i)
      printf "lab-probe net %02d:%02d:%02d %s\r\n", t / 3600, (t % 3600) / 60, t % 60, net
      printf "lab-probe disk %02d:%02d:%02d %s %d\r\n", t / 3600, (t % 3600) / 60, t % 60, disk, c + i
    }
  }'
}

# ---------------------------------------------------------------------------
# Test 1: usage and make
# ---------------------------------------------------------------------------
test_usage() {
  echo "Test: without a subcommand the usage goes to stderr with exit 2, and make calls each subcommand"
  local sub rc=0 out
  out="$(bash "$LAB_CHAOS_SH" 2>&1)" || rc=$?
  assert_eq "no subcommand exits 2" "2" "$rc"
  for sub in preflight proving faults random cleanup teardown-check record; do
    assert_contains "the usage names $sub" "$out" "  $sub "
    assert_eq "make -n lab-chaos-$sub calls hack/lab-chaos.sh $sub" "hack/lab-chaos.sh $sub" \
      "$(make -s -n -C "$PROJECT_ROOT" "lab-chaos-$sub" 2>/dev/null)"
  done
}

# ---------------------------------------------------------------------------
# Test 2: the settings and the context
# ---------------------------------------------------------------------------
test_settings_and_context() {
  echo "Test: an unset or a kind context fails with its own FAIL line before any kubectl call"
  new_case
  run_lc preflight
  assert_eq "an unset LAB_CHAOS_CONTEXT is a usage error" "2" "$RC"
  assert_eq "naming it" "FAIL: LAB_CHAOS_CONTEXT is not set; name the kube context of the lab" "$OUT"
  assert_eq "no kubectl call" "" "$(cat "$CALL_LOG")"

  new_case
  run_lc preflight LAB_CHAOS_CONTEXT=kind-cobaltcore
  assert_eq "a kind context fails" "1" "$RC"
  assert_contains "naming it" "$OUT" "FAIL: context kind-cobaltcore is a kind cluster; the lab chaos runs are for the lab only"
  assert_eq "no kubectl call" "" "$(cat "$CALL_LOG")"

  new_case
  run_lc preflight LAB_CHAOS_CONTEXT=garden-gone
  assert_contains "a context the kubeconfig lacks fails" "$OUT" "FAIL: context garden-gone is not in the kubeconfig"

  new_case
  run_lc preflight LAB_CHAOS_CONTEXT=garden-lab LAB_CHAOS_RUN=yesterday
  assert_eq "a malformed run id is a usage error" "2 FAIL: LAB_CHAOS_RUN must be a run id such as 20261030T080000Z, not 'yesterday'" "$RC $OUT"
  run_lc random LAB_CHAOS_CONTEXT=garden-lab LAB_CHAOS_RANDOM_DURATION=six
  assert_eq "a malformed duration is a usage error" "2 FAIL: LAB_CHAOS_RANDOM_DURATION must be a duration such as 6h, not 'six'" "$RC $OUT"
}

# ---------------------------------------------------------------------------
# Test 3: preflight
# ---------------------------------------------------------------------------
test_preflight() {
  echo "Test: preflight reads the lab and names what it lacks"
  new_case
  run_lc preflight LAB_CHAOS_CONTEXT=garden-lab
  assert_eq "a complete lab passes" "0" "$RC"
  local line
  for line in "OK: kubectl jq yq python3 openstack on PATH" "OK: context garden-lab" "OK: Gardener shoot newforge" \
    "OK: openstack token issue through the port-forward" "OK: HelmRelease chaos-mesh/chaos-mesh is Ready" \
    "OK: one 'are loaded' line per node (2)" "OK: namespace dizzy" "OK: ControlPlane openstack/controlplane is Ready=True" \
    "OK: image hvo-cirros-kvm and flavor hvo-smoke-test-flavor are Available" "OK: the Cinder backend nfs1 is Ready" \
    "OK: volume type nfs" "OK: 2 nova-compute services are up" "OK: no server named lab-chaos-*"; do
    assert_contains "it prints '$line'" "$OUT" "$line"
  done
  assert_eq "and changes nothing" "" "$(mutations)"

  new_case
  rm -f "$TMP_DIR/case/state/nfs1"
  run_lc preflight LAB_CHAOS_CONTEXT=garden-lab
  assert_eq "a lab without nfs1 fails" "1" "$RC"
  assert_eq "with one FAIL line naming the backend" \
    "FAIL: the Cinder backend nfs1 is missing or not Ready (read 'Error from server (NotFound): cinderbackends.cinder.openstack.c5c3.io \"nfs1\" not found'); apply deploy/lab/metal-stack/controlplane-nfs as in Part 3, Step 1" \
    "$(grep '^FAIL:' <<<"$OUT")"

  new_case
  echo lab-chaos-0 >"$TMP_DIR/case/state/servers"
  run_lc preflight LAB_CHAOS_CONTEXT=garden-lab
  assert_contains "a server of a previous session fails" "$OUT" \
    "FAIL: servers of a previous session exist: lab-chaos-0; run make lab-chaos-cleanup"
}

# ---------------------------------------------------------------------------
# Test 4: the go-ahead
# ---------------------------------------------------------------------------
test_go_ahead() {
  echo "Test: a go-ahead that differs from the shoot refuses every changing subcommand before any write"
  local sub
  for sub in proving faults random cleanup teardown-check; do
    new_case
    run_lc "$sub" LAB_CHAOS_CONTEXT=garden-lab LAB_CHAOS_GO_AHEAD=oldforge
    assert_eq "$sub with the wrong go-ahead exits 1" "1" "$RC"
    assert_contains "$sub names the shoot" "$OUT" \
      "ERROR: no go-ahead for shoot 'newforge': LAB_CHAOS_GO_AHEAD is 'oldforge'; set it to newforge once the shared lab is yours"
    assert_eq "$sub writes nothing" "" "$(mutations)"
    assert_eq "$sub creates no run directory" "no" "$([[ -d "$TMP_DIR/case/out" ]] && echo yes || echo no)"
  done
}

# ---------------------------------------------------------------------------
# Test 5: recovered and probes_ok
# ---------------------------------------------------------------------------
test_gate_helpers() {
  echo "Test: recovered and probes_ok judge the gate as the fault runs need"
  local s="$TMP_DIR/case/state" rc
  new_case
  console_lines ok ok 1 10 >"$s/console"
  console_lines ok ok 11 1 >"$s/console-step"
  printf 'True\nTrue\n' >"$s/agents"
  rc=0
  ( in_lc; recovered 5 ) >/dev/null 2>&1 || rc=$?
  assert_eq "recovered holds on alive agents and advancing ok lines" "0" "$rc"

  : >"$s/agents"
  rc=0
  ( in_lc; recovered 1 ) >"$TMP_DIR/out" 2>&1 || rc=$?
  assert_eq "recovered fails on an empty agent list" "1" "$rc"
  assert_contains "with its GATE FAILED line" "$(cat "$TMP_DIR/out")" "GATE FAILED: lab-chaos-0, nova-compute, the agents of worker-a or the guest probes did not recover in 1 s"
  rm -f "$s/agents"
  rc=0
  ( in_lc; recovered 1 ) >/dev/null 2>&1 || rc=$?
  assert_eq "recovered fails on a failed agent call" "1" "$rc"
  printf 'True\nFalse\n' >"$s/agents"
  rc=0
  ( in_lc; recovered 1 ) >/dev/null 2>&1 || rc=$?
  assert_eq "recovered fails on a dead agent" "1" "$rc"

  printf 'True\n' >"$s/agents"
  rm -f "$s/console-step"
  rc=0
  ( in_lc; probes_ok ) >/dev/null 2>&1 || rc=$?
  assert_eq "probes_ok fails on a stalled probe" "1" "$rc"
  console_lines lost ok 20 1 >"$s/console-step"
  rc=0
  ( in_lc; probes_ok ) >/dev/null 2>&1 || rc=$?
  assert_eq "probes_ok fails on a new line that is not ok" "1" "$rc"
  rm -f "$s/console" "$s/console-step"
  rc=0
  ( in_lc; probes_ok ) >/dev/null 2>&1 || rc=$?
  assert_eq "probes_ok fails on a console read that prints nothing" "1" "$rc"
}

# ---------------------------------------------------------------------------
# Test 6: the helpers of faults.tsv
# ---------------------------------------------------------------------------
test_fault_row_helpers() {
  echo "Test: the console statistics and the state comparison of a fault row"
  new_case
  {
    console_lines ok ok 1 3 | tr -d '\r'
    echo "lab-probe net 10:00:20 lost"
    echo "lab-probe disk 10:01:00 error 4"
    echo "lab-probe net 10:01:02 ok"
    echo "lab-probe disk 10:01:04 ok 5"
    echo "lab-probe net 11:00:00 lost"
  } >"$TMP_DIR/console"
  assert_eq "lost lines, the largest net gap, error lines and the largest disk gap inside the window" "1 42 1 54" \
    "$( (in_lc; lc_console_stats "$TMP_DIR/console" 10:00:00 10:59:59) )"
  local base now
  base="server=ACTIVE domain=running_booted agents=True_True_ volume=in-use nfs-server=w1/Running/true,"
  now="server=ACTIVE domain=running_unpaused agents=True_ volume=in-use nfs-server=w2/Running/true,"
  assert_eq "a running domain, alive agents and a moved NFS server move nothing" "no field moved" \
    "$( (in_lc; lc_state_diff "$base" "$now") )"
  now="server=ERROR domain=shut_off agents=True_False_ volume=in-use nfs-server=w1/Pending/false,"
  assert_eq "every other change is named" "server=ERROR, domain=shut_off, agents=True_False_, nfs-server=Pending/false," \
    "$( (in_lc; lc_state_diff "$base" "$now") )"
}

# ---------------------------------------------------------------------------
# Test 7: the user-data
# ---------------------------------------------------------------------------
test_user_data() {
  echo "Test: the user-data of lab-chaos-0 starts both probes once the volume is there"
  local ud
  new_case
  ud="$( (in_lc; lc_user_data 10.239.0.7) )"
  assert_eq "it is a #!/bin/sh script" "#!/bin/sh" "$(head -n 1 <<<"$ud")"
  assert_contains "it waits for /dev/vdb" "$ud" 'while [ ! -b /dev/vdb ]'
  assert_contains "it pings the peer" "$ud" 'ping -c 1 -W 1 10.239.0.7'
  assert_contains "it writes the counter at byte 4096 of the volume" "$ud" 'dd of=/dev/vdb bs=512 seek=8'
  assert_contains "both probes write to the serial console" "$ud" '>/dev/ttyS0 2>&1) &'
  local rc=0
  sh -n <<<"$ud" || rc=$?
  assert_eq "it parses as sh" "0" "$rc"
}

# ---------------------------------------------------------------------------
# Test 8: cleanup
# ---------------------------------------------------------------------------
test_cleanup_twice() {
  echo "Test: cleanup deletes what faults created, and a second call deletes nothing"
  local s="$TMP_DIR/case/state"
  new_case
  printf 'lab-chaos-0\nlab-chaos-1\n' >"$s/servers"
  printf 'lab-chaos-nfs\nlab-chaos-vol-after\n' >"$s/volumes"
  touch "$s/network" "$s/subnet" "$s/group"
  echo "podchaos.chaos-mesh.org/lab-libvirt-kill" >"$s/chaos"
  run_lc cleanup LAB_CHAOS_CONTEXT=garden-lab LAB_CHAOS_GO_AHEAD=newforge
  assert_eq "the first cleanup exits 0" "0" "$RC"
  assert_contains "and deletes the eight objects" "$OUT" "cleanup: deleted 8 object(s)"
  assert_eq "no server, volume, network, subnet, group or chaos object is left" "" \
    "$(cat "$s/servers" "$s/volumes"; for f in network subnet group chaos; do [[ -e "$s/$f" ]] && echo "$f"; done)"
  run_lc cleanup LAB_CHAOS_CONTEXT=garden-lab LAB_CHAOS_GO_AHEAD=newforge
  assert_eq "the second cleanup exits 0" "0" "$RC"
  assert_contains "and deletes nothing" "$OUT" "cleanup: deleted 0 object(s)"
}

# ---------------------------------------------------------------------------
# Test 9: record
# ---------------------------------------------------------------------------
test_record() {
  echo "Test: record writes record.md from the run directory"
  local d="$TMP_DIR/case/out/20261030T080000Z" r="$TMP_DIR/case/random"
  new_case
  mkdir -p "$d/proving" "$r"
  echo newforge >"$d/shoot"
  printf 'chart 2.8.6\nworker-a v1.35.6 Garden Linux 1592.4\nworker-b v1.35.6 Garden Linux 1592.4\nworker-c v1.35.6 Garden Linux 1592.4\n' >"$d/proving/step1.log"
  printf '1\tpass\tchart 2.8.6\n2\tpass\tbaseline 0.3 s\n' >"$d/proving.tsv"
  printf 'fault\tname\tback\tqemu\tnet\tdisk\tconsole\tapis\tissue\nF1\tlab-libvirt-kill\t5 s\tACTIVE; QEMU PID 1 kept\tno lost line, largest gap 2 s\tno error line, largest gap 2 s\tthe log kept growing (90 probe lines)\tno field moved; 1 state changes; the gate held at 60 s\t-\n' >"$d/faults.tsv"
  echo '{"verdict": "FAIL", "run": "20261030T090000Z", "seed": 77, "window": "6h", "checks": [{"name": "workflow", "pass": true}, {"name": "quiet-tail", "pass": false}]}' >"$r/random-verdict.json"
  echo '{"steps": [{}, {}, {}]}' >"$r/plan.json"
  ln -s "$r" "$d/random"
  echo 20261030T080000Z >"$TMP_DIR/case/out/current"
  run_lc record
  assert_eq "record exits 0 without a context" "0" "$RC"
  local md
  md="$(cat "$d/record.md")"
  assert_contains "the paragraph names the date, the shoot, the nodes, the chart and no hand action" "$md" \
    "The run of 2026-10-30 ran \`make lab-chaos-proving\`, \`make lab-chaos-faults\` and \`make lab-chaos-random\` from commit \`"
  assert_contains "on the shoot with its nodes and chart" "$md" "on shoot \`newforge\`, 3 workers on Kubernetes v1.35.6, with Chaos Mesh chart 2.8.6. Hand actions: none."
  assert_contains "the proving table" "$md" "| 2 | pass | baseline 0.3 s |"
  assert_contains "the fault table" "$md" "| F1 \`lab-libvirt-kill\` | 5 s | ACTIVE; QEMU PID 1 kept |"
  assert_contains "the random row" "$md" "| 20261030T090000Z | 77 | 6h | 3 | FAIL | quiet-tail |"
  echo "read the console with virsh console after F1" >"$d/hand-actions.txt"
  run_lc record
  assert_contains "a hand-actions.txt is named" "$(cat "$d/record.md")" "Hand actions: read the console with virsh console after F1."
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if ! command -v jq >/dev/null 2>&1 || ! command -v yq >/dev/null 2>&1 || ! command -v python3 >/dev/null 2>&1; then
  echo "SKIP: jq, yq or python3 not installed (every check of this file skipped)"
  echo ""
  echo "Results: 0 passed, 0 failed, 1 skipped"
  exit 0
fi

test_usage
test_settings_and_context
test_preflight
test_go_ahead
test_gate_helpers
test_fault_row_helpers
test_user_data
test_cleanup_twice
test_record

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
