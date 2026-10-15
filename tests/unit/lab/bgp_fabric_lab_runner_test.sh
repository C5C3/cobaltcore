#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the verdicts and the teardown of tests/lab/bgp-fabric/run.sh against
# a fake lab. A docker stub first on PATH answers the containerlab and
# inspect calls and runs every "docker exec" on this host inside a fake box:
# its vtysh and ip answer from fixtures, its tc keeps the state of leaf1's
# eth1, and the in-box scripts of A3 run for real.
#   - a healthy lab passes A1 to A6, prints the record, silences leaf1 only
#     after spine1's withdraw sampler took its first sample, restores leaf1's
#     eth1 and is torn down
#   - a node container that is not running and a failed deploy end the run
#     non-zero and the lab is still torn down; KEEP_LAB=true keeps it
#   - a kernel without sch_netem or without the ingress modules makes A3
#     SKIP; tc missing from the image or any other tc error makes it FAIL;
#     a BFD session that stays up, a spine that keeps both paths or one at
#     one path before the silence fails it, and so does a failed mktemp
#   - A4 fails while a bait is missing, A6 when vtysh answers nothing
#
# No Docker, containerlab or network access is needed.
#
# Usage: bash tests/unit/lab/bgp_fabric_lab_runner_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
RUNNER="$PROJECT_ROOT/tests/lab/bgp-fabric/run.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# The stubs read these from the environment.
export HOST_PATH="$PATH"
export BIN="$TMP_ROOT/bin"
export BOX="$TMP_ROOT/box"
export BOX_NO_TC="$TMP_ROOT/box-no-tc"
export FIX="$TMP_ROOT/fixtures"
export STATE="$TMP_ROOT/state"
export DOCKER_LOG="$TMP_ROOT/docker.log"

OUT=""
RC=0

# ---------------------------------------------------------------------------
# Stubs
# ---------------------------------------------------------------------------

make_stubs() {
  mkdir -p "$BIN" "$BOX" "$BOX_NO_TC"

  # docker: records its argv, one line per call.
  #   run ... containerlab deploy    exits STUB_DEPLOY_RC
  #   run ... containerlab destroy   marks the lab destroyed
  #   inspect                        every node running but STUB_STOPPED
  #   exec <container> <cmd...>      runs <cmd...> in the fake box of that
  #                                  node, without tc when STUB_NO_TC=true;
  #                                  the A5 probe answers a passing reading
  # spine1's withdraw sampler starts 0.2 s late, as a busy dockerd may start
  # it, so only run.sh's wait for its ready line keeps the order. A silence
  # that starts before the sampler's first sample says so in
  # $STATE/silence-early.
  cat >"$BIN/docker" <<'STUB'
#!/bin/bash
echo "docker $*" >>"$DOCKER_LOG"
case "$1" in
  info) exit 0 ;;
  run)
    while [ "$#" -gt 0 ] && [ "$1" != containerlab ]; do shift; done
    case "${2:-}" in
      deploy) exit "${STUB_DEPLOY_RC:-0}" ;;
      destroy) touch "$STATE/destroyed"; exit 0 ;;
      version) echo "    version: 0.81.1"; exit 0 ;;
    esac
    ;;
  inspect)
    case "$3" in
      '{{.State.Running}}')
        if [ "$4" = "clab-bgp-fabric-${STUB_STOPPED:-}" ]; then echo false; else echo true; fi
        exit 0
        ;;
      '{{.Image}}') echo sha256:feed; exit 0 ;;
    esac
    ;;
  image)
    echo "quay.io/frrouting/frr@sha256:$(printf '%064d' 0)"
    exit 0
    ;;
  exec)
    node="${2#clab-bgp-fabric-}"
    shift 2
    if [ "$1" = sh ] && [[ "$3" == *Ip6FragCreates* ]]; then
      echo "1 0 2"
      exit 0
    fi
    box="$BOX"
    [ "${STUB_NO_TC:-}" != true ] || box="$BOX_NO_TC"
    if [ "$1" = sh ] && [[ "$3" == *pathCount* ]]; then
      command -p sleep 0.2
      export A3_SAMPLER=true
    elif [ "$1" = sh ] && [[ "$3" == *"netem loss"* ]]; then
      [ -f "$STATE/sampler-sampled" ] || echo "before spine1's withdraw sampler took a sample" >"$STATE/silence-early"
      rc=0
      NODE="$node" PATH="$box" "$@" || rc=$?
      touch "$STATE/silence-done"
      exit "$rc"
    fi
    NODE="$node" PATH="$box" exec "$@"
    ;;
esac
echo "docker stub: unexpected call: $*" >&2
exit 99
STUB

  # sleep returns at once, on the host and in the box. Past its first sample,
  # spine1's withdraw sampler sleeps until leaf1's eth1 is silent or the
  # silence is over, 30 s at most. run.sh silences leaf1 right after the
  # sampler's first sample, so the bound only ends a run in which run.sh lost
  # the order.
  printf '#!/bin/bash\n' >"$BIN/sleep"
  cat >"$BOX/sleep" <<'STUB'
#!/bin/bash
[ "${A3_SAMPLER:-}" = true ] || exit 0
for ((i = 0; i < 3000; i++)); do
  [ ! -f "$STATE/filter" ] && [ ! -f "$STATE/silence-done" ] || exit 0
  command -p sleep 0.01
done
STUB

  # vtysh -c <command> prints the node's fixture for "vtysh -c <command>",
  # its .silenced variant while netem and the ingress drop are on eth1. The
  # withdraw sampler's calls mark $STATE/sampler-sampled.
  cat >"$BOX/vtysh" <<'STUB'
#!/bin/bash
PATH="$HOST_PATH"
[ "${A3_SAMPLER:-}" != true ] || touch "$STATE/sampler-sampled"
f="$FIX/$NODE/$(printf '%s' "vtysh $*" | tr -c 'A-Za-z0-9.-' '_')"
if [ -f "$STATE/netem" ] && [ -f "$STATE/filter" ] && [ -f "$f.silenced" ]; then
  f="$f.silenced"
fi
if [ -f "$f" ]; then
  cat "$f"
  exit 0
fi
echo "% no fixture on $NODE for: vtysh $*"
exit 1
STUB

  # ip prints the node's fixture for "ip <args>".
  cat >"$BOX/ip" <<'STUB'
#!/bin/bash
PATH="$HOST_PATH"
f="$FIX/$NODE/$(printf '%s' "ip $*" | tr -c 'A-Za-z0-9.-' '_')"
if [ -f "$f" ]; then
  cat "$f"
  exit 0
fi
echo "box ip: no fixture on $NODE for: ip $*" >&2
exit 1
STUB

  # tc keeps eth1's qdiscs and filter as files under $STATE. STUB_TC_NETEM
  # (unknown, eperm) and STUB_TC_MATCHALL (unknown) make the adds fail with
  # the kernel's own messages.
  cat >"$BOX/tc" <<'STUB'
#!/bin/bash
PATH="$HOST_PATH"
case "$*" in
  "qdisc add dev eth1 root netem loss 100%")
    case "${STUB_TC_NETEM:-ok}" in
      unknown) echo "Error: Specified qdisc kind is unknown." >&2; exit 2 ;;
      eperm) echo "RTNETLINK answers: Operation not permitted" >&2; exit 2 ;;
    esac
    touch "$STATE/netem"
    ;;
  "qdisc add dev eth1 ingress") touch "$STATE/ingress" ;;
  "filter add dev eth1 parent ffff: matchall action drop")
    if [ "${STUB_TC_MATCHALL:-ok}" = unknown ]; then
      echo "Error: TC classifier not found." >&2
      exit 2
    fi
    touch "$STATE/filter"
    ;;
  "qdisc del dev eth1 root") rm -f "$STATE/netem" ;;
  "qdisc del dev eth1 ingress") rm -f "$STATE/ingress" "$STATE/filter" ;;
  *)
    echo "box tc: unexpected call: $*" >&2
    exit 1
    ;;
esac
STUB

  chmod +x "$BIN/docker" "$BIN/sleep" "$BOX/sleep" "$BOX/vtysh" "$BOX/ip" "$BOX/tc"
  ln -s /bin/sh "$BOX/sh"
  ln -s "$(command -v grep)" "$BOX/grep"
  local f
  for f in sh grep sleep vtysh ip; do
    ln -s "$BOX/$f" "$BOX_NO_TC/$f"
  done
}

# fixture <node> <command> <answer>: what <command> prints on <node>.
fixture() {
  mkdir -p "$FIX/$1"
  printf '%s\n' "$3" >"$FIX/$1/$(printf '%s' "$2" | tr -c 'A-Za-z0-9.-' '_')"
}

# reset_lab: a healthy lab, every stub switch off, eth1 clean.
reset_lab() {
  unset STUB_DEPLOY_RC STUB_STOPPED STUB_NO_TC STUB_TC_NETEM STUB_TC_MATCHALL KEEP_LAB
  rm -rf "$FIX" "$STATE" "$DOCKER_LOG"
  mkdir -p "$STATE"

  local n summary two_paths
  summary='{"ipv4Unicast":{"peers":{"eth1":{"state":"Established"},"eth2":{"state":"Established"}}},"ipv6Unicast":{"peers":{"eth1":{"state":"Established"},"eth2":{"state":"Established"}}}}'
  for n in node1 leaf1 leaf2 spine1; do
    fixture "$n" "ip -j link show" \
      '[{"ifname":"lo","mtu":65536},{"ifname":"eth0","mtu":1500},{"ifname":"eth1","mtu":9000},{"ifname":"eth2","mtu":9000}]'
  done
  for n in leaf1 leaf2 spine1; do
    fixture "$n" "vtysh -c show bgp summary json" "$summary"
    fixture "$n" "vtysh -c show bgp ipv4 unicast 203.0.113.0/24 json" '{}'
  done
  for n in leaf1 leaf2; do
    fixture "$n" "vtysh -c show bfd peers json" '[{"interface":"eth1","status":"up"}]'
  done
  fixture leaf1 "vtysh -c show bfd peers json.silenced" '[{"interface":"eth1","status":"down"}]'

  two_paths='"pathCount":2,"paths":[{"aspath":{"string":"4200001002 4200000001"},"multipath":true,"bestpath":{"overall":true}},{"aspath":{"string":"4200001001 4200000001"},"multipath":true}]'
  fixture spine1 "vtysh -c show bgp ipv4 unicast 192.0.2.1/32 json" "{\"prefix\":\"192.0.2.1/32\",$two_paths}"
  fixture spine1 "vtysh -c show bgp ipv4 unicast 192.0.2.1/32 json.silenced" \
    '{"prefix":"192.0.2.1/32","pathCount":1,"paths":[{"aspath":{"string":"4200001002 4200000001"},"multipath":true,"bestpath":{"overall":true}}]}'
  fixture spine1 "vtysh -c show bgp ipv6 unicast 2001:db8::1/128 json" "{\"prefix\":\"2001:db8::1/128\",$two_paths}"
  fixture spine1 "ip -j route show 192.0.2.1" '[{"dst":"192.0.2.1","nexthops":[{"dev":"eth1"},{"dev":"eth2"}]}]'

  fixture node1 "vtysh -c show bgp ipv4 unicast neighbors eth1 advertised-routes json" \
    '{"advertisedRoutes":{"192.0.2.1/32":{}}}'
  fixture node1 "vtysh -c show bgp ipv6 unicast neighbors eth1 advertised-routes json" \
    '{"advertisedRoutes":{"2001:db8::1/128":{}}}'
  fixture node1 "vtysh -c show bgp ipv4 unicast 203.0.113.0/24 json" '{"prefix":"203.0.113.0/24","pathCount":1,"paths":[{}]}'
  fixture node1 "vtysh -c show bgp ipv4 unicast 198.18.0.0/15 json" '{}'
  fixture node1 "vtysh -c show bgp ipv4 unicast 0.0.0.0/0 json" '{"pathCount":2,"paths":[{},{}]}'
  fixture node1 "vtysh -c show bgp ipv6 unicast ::/0 json" '{"pathCount":2,"paths":[{},{}]}'
  fixture leaf1 "vtysh -c show bgp ipv4 unicast neighbors eth1 advertised-routes json" \
    '{"advertisedRoutes":{"192.0.2.1/32":{},"198.18.0.0/15":{}}}'
  fixture leaf1 "vtysh -c show running-config" 'router bgp 4200001001
  neighbor nodes maximum-prefix 8192'
}

# run_runner <subcommand>: runs run.sh with the stubs first on PATH; its
# output lands in OUT, its exit code in RC.
run_runner() {
  OUT="$(PATH="$BIN:$HOST_PATH" bash "$RUNNER" "$1" 2>&1)"
  RC=$?
}

# assert_torn_down <description>, assert_kept <description>
assert_torn_down() {
  if [[ -f "$STATE/destroyed" ]]; then
    echo "  PASS: $1"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $1 (no containerlab destroy ran)"
    FAIL=$((FAIL + 1))
  fi
}

assert_kept() {
  if [[ ! -f "$STATE/destroyed" ]]; then
    echo "  PASS: $1"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $1 (containerlab destroy ran)"
    FAIL=$((FAIL + 1))
  fi
}

# assert_eth1_clean <description>: no netem, ingress qdisc or filter left on
# leaf1's eth1.
assert_eth1_clean() {
  local f left=""
  for f in netem ingress filter; do
    [[ ! -f "$STATE/$f" ]] || left="$left $f"
  done
  assert_eq "$1" "" "$left"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_healthy_lab_passes_and_is_torn_down() {
  echo "Test: a healthy lab passes A1 to A6, restores eth1 and is torn down"
  reset_lab
  run_runner all

  assert_eq "the run exits 0" "0" "$RC"
  local id
  for id in A1 A2 A3 A4 A5 A6; do
    assert_contains "$id passes" "$OUT" "$id PASS "
  done
  assert_contains "the run record carries the A6 row" "$OUT" "| A6 | PASS |"
  assert_eq "leaf1 goes silent only after spine1's withdraw sampler took a sample" "" \
    "$(cat "$STATE/silence-early" 2>/dev/null)"
  assert_eth1_clean "leaf1's eth1 is restored after A3"
  assert_torn_down "the lab is torn down"
}

test_stopped_node_fails_and_is_torn_down() {
  echo "Test: a node container that is not running fails the run, the lab is still torn down"
  reset_lab
  export STUB_STOPPED=spine1
  run_runner all

  assert_eq "the run exits 1" "1" "$RC"
  assert_contains "the run names the stopped container" "$OUT" \
    "lab container clab-bgp-fabric-spine1 is not running"
  assert_torn_down "the lab is torn down"
}

test_failed_deploy_is_torn_down() {
  echo "Test: a failed deploy ends the run before check, the lab is still torn down"
  reset_lab
  export STUB_DEPLOY_RC=1
  run_runner all

  assert_nonzero_exit "the run exits non-zero" "$RC"
  assert_not_contains "no assertion runs" "$OUT" "A1 "
  assert_torn_down "the partial lab is torn down"
}

test_keep_lab_keeps_the_lab() {
  echo "Test: KEEP_LAB=true keeps the lab after all"
  reset_lab
  export KEEP_LAB=true
  run_runner all

  assert_eq "the run exits 0" "0" "$RC"
  assert_contains "the run says the lab stays" "$OUT" "KEEP_LAB=true: the lab stays up"
  assert_kept "the lab is not torn down"
}

test_a3_skips_on_a_kernel_without_netem() {
  echo "Test: a kernel without sch_netem makes A3 SKIP, and the run passes"
  reset_lab
  export STUB_TC_NETEM=unknown
  run_runner all

  assert_contains "A3 is skipped for sch_netem" "$OUT" "A3 SKIP "
  assert_contains "the skip names sch_netem" "$OUT" "host kernel without sch_netem"
  assert_eq "the run exits 0" "0" "$RC"
  assert_eth1_clean "leaf1's eth1 carries nothing"
}

test_a3_skips_on_a_kernel_without_the_ingress_modules() {
  echo "Test: a kernel without the ingress-drop modules makes A3 SKIP and removes netem"
  reset_lab
  export STUB_TC_MATCHALL=unknown
  run_runner all

  assert_contains "A3 is skipped" "$OUT" "A3 SKIP "
  assert_contains "the skip names the modules" "$OUT" "sch_ingress, cls_matchall or act_gact"
  assert_eq "the run exits 0" "0" "$RC"
  assert_eth1_clean "the netem qdisc A3 added is gone"
}

test_a3_fails_without_tc_in_the_image() {
  echo "Test: tc missing from the image fails A3 instead of blaming the kernel"
  reset_lab
  export STUB_NO_TC=true
  run_runner all

  assert_contains "A3 fails and names the image" "$OUT" \
    "A3 FAIL silencing eth1 on leaf1 failed: tc is not in the image"
  assert_eq "the run exits 1" "1" "$RC"
}

test_a3_fails_on_any_other_tc_error() {
  echo "Test: a tc error that is no missing module fails A3"
  reset_lab
  export STUB_TC_NETEM=eperm
  run_runner all

  assert_contains "A3 fails with the tc error" "$OUT" \
    "A3 FAIL silencing eth1 on leaf1 failed: RTNETLINK answers: Operation not permitted"
  assert_eq "the run exits 1" "1" "$RC"
}

test_a3_fails_when_bfd_stays_up() {
  echo "Test: a BFD session that stays up on the silent link fails A3"
  reset_lab
  rm "$FIX/leaf1/$(printf '%s' "vtysh -c show bfd peers json" | tr -c 'A-Za-z0-9.-' '_').silenced"
  run_runner all

  assert_contains "A3 fails on BFD" "$OUT" "A3 FAIL BFD on leaf1 still up 200 samples"
  assert_eq "the run exits 1" "1" "$RC"
  assert_eth1_clean "leaf1's eth1 is restored"
}

test_a3_fails_when_the_spine_keeps_both_paths() {
  echo "Test: a spine that keeps both paths after BFD went down fails A3"
  reset_lab
  rm "$FIX/spine1/$(printf '%s' "vtysh -c show bgp ipv4 unicast 192.0.2.1/32 json" | tr -c 'A-Za-z0-9.-' '_').silenced"
  run_runner all

  assert_contains "A3 fails on the withdraw" "$OUT" "spine1 still had 2 paths after 140 samples"
  assert_eq "the run exits 1" "1" "$RC"
}

test_a3_fails_when_the_spine_starts_at_one_path() {
  echo "Test: a spine at one path before the silence fails A3, and leaf1 is never silenced"
  reset_lab
  local f
  f="$FIX/spine1/$(printf '%s' "vtysh -c show bgp ipv4 unicast 192.0.2.1/32 json" | tr -c 'A-Za-z0-9.-' '_')"
  cp "$f.silenced" "$f"
  run_runner all

  assert_contains "A3 fails before the silence" "$OUT" \
    "A3 FAIL spine1's withdraw sampler saw no 2 paths for 192.0.2.1/32 before the silence; spine1 holds 1"
  assert_file_not_contains "leaf1's eth1 is never silenced" "$DOCKER_LOG" "netem loss"
  assert_eq "the run exits 1" "1" "$RC"
}

test_a3_fails_when_mktemp_fails() {
  echo "Test: a failed mktemp fails A3 on the host, and leaf1 is never silenced"
  reset_lab
  TMPDIR="$TMP_ROOT/no-such-dir" run_runner all

  assert_contains "A3 names mktemp" "$OUT" \
    "A3 FAIL mktemp failed on the host, so eth1 was not silenced"
  assert_file_not_contains "leaf1's eth1 is never silenced" "$DOCKER_LOG" "netem loss"
  assert_eq "the run exits 1" "1" "$RC"
}

test_a4_fails_without_the_node_bait() {
  echo "Test: A4 fails when node1 does not hold 203.0.113.0/24"
  reset_lab
  fixture node1 "vtysh -c show bgp ipv4 unicast 203.0.113.0/24 json" '{}'
  run_runner all

  assert_contains "A4 names the missing bait" "$OUT" \
    "A4 FAIL the bait 203.0.113.0/24 is not in node1's BGP table"
  assert_eq "the run exits 1" "1" "$RC"
}

test_a4_fails_without_the_leaf_bait() {
  echo "Test: A4 fails when leaf1 does not offer 198.18.0.0/15"
  reset_lab
  fixture leaf1 "vtysh -c show bgp ipv4 unicast neighbors eth1 advertised-routes json" \
    '{"advertisedRoutes":{"192.0.2.1/32":{}}}'
  run_runner all

  assert_contains "A4 names the missing bait" "$OUT" \
    "A4 FAIL leaf1 does not offer the bait 198.18.0.0/15 to node1"
  assert_eq "the run exits 1" "1" "$RC"
}

test_a6_fails_when_vtysh_answers_nothing() {
  echo "Test: A6 fails when vtysh answers nothing for node1's advertised routes"
  reset_lab
  fixture node1 "vtysh -c show bgp ipv4 unicast neighbors eth1 advertised-routes json" ''
  run_runner all

  assert_contains "A6 fails on the empty answer" "$OUT" \
    "A6 FAIL node1 advertises on eth1 in ipv4: no JSON from vtysh"
  assert_eq "the run exits 1" "1" "$RC"
}

# --- Run ---
make_stubs
test_healthy_lab_passes_and_is_torn_down
test_stopped_node_fails_and_is_torn_down
test_failed_deploy_is_torn_down
test_keep_lab_keeps_the_lab
test_a3_skips_on_a_kernel_without_netem
test_a3_skips_on_a_kernel_without_the_ingress_modules
test_a3_fails_without_tc_in_the_image
test_a3_fails_on_any_other_tc_error
test_a3_fails_when_bfd_stays_up
test_a3_fails_when_the_spine_keeps_both_paths
test_a3_fails_when_the_spine_starts_at_one_path
test_a3_fails_when_mktemp_fails
test_a4_fails_without_the_node_bait
test_a4_fails_without_the_leaf_bait
test_a6_fails_when_vtysh_answers_nothing

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
