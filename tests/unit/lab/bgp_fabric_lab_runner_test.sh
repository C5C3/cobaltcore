#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the verdicts and the teardown of tests/lab/bgp-fabric/run.sh against
# a fake lab of five boxes. A docker stub first on PATH answers the
# containerlab, inspect and logs calls and runs every "docker exec" on this
# host inside a fake box: its vtysh and ip answer from fixtures, its tc keeps
# the state of leaf1's eth1, and the in-box scripts of A3 run for real.
#   - a healthy lab passes A1 to A7, prints the record with seven rows,
#     silences leaf1 only after leaf3's sampler printed ready, restores
#     leaf1's eth1 and is torn down
#   - a node container that is not running, a box whose setup.sh failed (a
#     restarted container whose logs the stub serves) and a failed deploy end
#     the run non-zero with the log tail printed, and the lab is still torn
#     down; KEEP_LAB=true keeps it
#   - a leaf3 at one path when A3's sampler starts is sampled again until it
#     shows 2 paths, and leaf1 goes silent only then
#   - A3's clock starts once the ingress drop is in place, so neither a slow
#     netem add nor a slow drop counts toward BFD's 1000 ms
#   - the kernel's three extack texts for a missing module make A3 SKIP, and
#     nothing on leaf1's eth1 drops when the in-box script exits;
#     tc missing from the image or any other tc error makes it FAIL, and so
#     do a sampler that never sees 2 paths, a BFD session that stays up or
#     goes down after 1000 ms, a leaf3 that keeps both paths, drops one
#     after 2000 ms or is not back at 2 paths after the restore, and a
#     failed mktemp
#   - A2 fails on one route distinguisher, A4 while a bait is missing and on
#     each filter leak, A5 on link ends at the wrong MTU and on each failed
#     probe reading, A6 on a non-host route, on no route and without the
#     prefix limit, A7 on a main-table route via eth1 and on a fabric-VRF
#     route via eth0, and every assertion that asks vtysh fails on an empty
#     answer
#   - check without a lab names the missing node1 container, an unknown
#     subcommand prints the four accepted ones, and a host without docker or
#     without the vrf and vxlan modules is refused before any container starts
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

# The waits of run.sh, short enough for a fake lab that never converges.
export A1_WAIT_MS=1000 A2_WAIT_MS=1000 A3_READY_WAIT_MS=2000 A3_RESTORE_WAIT_MS=2000

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
  #   inspect (state)                "true 0" for every box; STUB_STOPPED
  #                                  is not running, STUB_RESTARTED restarted
  #                                  once; no container when STUB_NO_LAB=true
  #   logs --tail 20 <container>     the failed setup.sh of that box
  #   exec <container> <cmd...>      runs <cmd...> in the fake box of that
  #                                  node, without tc when STUB_NO_TC=true;
  #                                  the A5 probe prints STUB_A5_PROBE, a
  #                                  passing reading by default
  # leaf3's withdraw sampler starts 0.2 s late, as a busy dockerd may start
  # it, so only run.sh's wait for its ready line keeps the order. A silence
  # that starts before the sampler saw 2 paths says so in
  # $STATE/silence-early, and $STATE/drop-at-exit names what still drops on
  # leaf1's eth1 when the in-box silence script exits.
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
      '{{.State.Running}} {{.RestartCount}}')
        [ "${STUB_NO_LAB:-}" != true ] || { echo "Error: No such object: $4" >&2; exit 1; }
        case "$4" in
          "clab-bgp-fabric-${STUB_STOPPED:-}") echo "false 0" ;;
          "clab-bgp-fabric-${STUB_RESTARTED:-}") echo "true 1" ;;
          *) echo "true 0" ;;
        esac
        exit 0
        ;;
      '{{.Image}}') echo sha256:feed; exit 0 ;;
    esac
    ;;
  image)
    echo "quay.io/frrouting/frr@sha256:$(printf '%064d' 0)"
    exit 0
    ;;
  logs)
    echo "Error: Unknown device type. (the failed setup.sh of $4)"
    exit 0
    ;;
  exec)
    node="${2#clab-bgp-fabric-}"
    shift 2
    if [ "$1" = sh ] && [[ "$3" == *Ip6FragCreates* ]]; then
      echo "${STUB_A5_PROBE:-1 0 2}"
      exit 0
    fi
    box="$BOX"
    [ "${STUB_NO_TC:-}" != true ] || box="$BOX_NO_TC"
    if [ "$1" = sh ] && [[ "$3" == *pathCount* ]]; then
      command -p sleep 0.2
      export A3_SAMPLER=true
    elif [ "$1" = sh ] && [[ "$3" == *"netem loss"* ]]; then
      [ -f "$STATE/sampler-ready" ] || echo "before leaf3's sampler saw 2 paths" >"$STATE/silence-early"
      rc=0
      NODE="$node" PATH="$box" "$@" || rc=$?
      for f in netem-loss filter; do
        [ ! -f "$STATE/$f" ] || printf ' %s' "$f" >>"$STATE/drop-at-exit"
      done
      touch "$STATE/silence-done"
      exit "$rc"
    fi
    NODE="$node" PATH="$box" exec "$@"
    ;;
esac
echo "docker stub: unexpected call: $*" >&2
exit 99
STUB

  # modinfo knows the vrf and vxlan modules unless STUB_NO_MODULES=true.
  cat >"$BIN/modinfo" <<'STUB'
#!/bin/bash
[ "${STUB_NO_MODULES:-}" != true ] || { echo "modinfo: ERROR: Module $2 not found." >&2; exit 1; }
echo "/lib/modules/fake/$2.ko"
STUB

  # sleep returns at once, on the host and in the box. Once leaf3's sampler
  # saw 2 paths, it sleeps until leaf1's eth1 is silent (the ingress drop and
  # netem at 100% loss in place) or the silence is over, 30 s at most. run.sh
  # silences leaf1 right after the sampler's ready line, so the bound only ends
  # a run in which run.sh lost the order.
  printf '#!/bin/bash\n' >"$BIN/sleep"
  cat >"$BOX/sleep" <<'STUB'
#!/bin/bash
[ "${A3_SAMPLER:-}" = true ] && [ -f "$STATE/sampler-ready" ] || exit 0
for ((i = 0; i < 3000; i++)); do
  if { [ -f "$STATE/netem-loss" ] && [ -f "$STATE/filter" ]; } || [ -f "$STATE/silence-done" ]; then
    exit 0
  fi
  command -p sleep 0.01
done
STUB

  # vtysh -c <command> prints the node's fixture for "vtysh -c <command>",
  # its .silenced variant while netem at 100% loss and the ingress drop are
  # on eth1, and nothing at all when STUB_VTYSH_EMPTY=true. leaf3's sampler
  # reads a .sampler-first variant once, before the others, and an answer
  # with 2 paths to it marks $STATE/sampler-ready. STUB_NO_RECOVERY=true
  # keeps the .silenced variants after the silence;
  # STUB_SILENCED_DELAY=<node>:<s> waits <s> seconds before the first
  # .silenced answer on <node>.
  cat >"$BOX/vtysh" <<'STUB'
#!/bin/bash
PATH="$HOST_PATH"
[ "${STUB_VTYSH_EMPTY:-}" != true ] || exit 0
f="$FIX/$NODE/$(printf '%s' "vtysh $*" | tr -c 'A-Za-z0-9.-' '_')"
if [ "${A3_SAMPLER:-}" = true ] && [ -f "$f.sampler-first" ]; then
  cat "$f.sampler-first"
  rm "$f.sampler-first"
  exit 0
fi
if [ -f "$f.silenced" ] && { { [ -f "$STATE/netem-loss" ] && [ -f "$STATE/filter" ]; } ||
  { [ "${STUB_NO_RECOVERY:-}" = true ] && [ -f "$STATE/silence-done" ]; }; }; then
  f="$f.silenced"
  if [ "${STUB_SILENCED_DELAY%%:*}" = "$NODE" ] && [ ! -f "$STATE/delayed" ]; then
    touch "$STATE/delayed"
    sleep "${STUB_SILENCED_DELAY#*:}"
  fi
fi
if [ -f "$f" ]; then
  [ "${A3_SAMPLER:-}" != true ] || ! grep -q '"pathCount":2,' "$f" || touch "$STATE/sampler-ready"
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

  # ping answers; A7 pings node1's gateway with it.
  printf '#!/bin/bash\nexit 0\n' >"$BOX/ping"

  # tc keeps eth1's qdiscs and filter as files under $STATE: netem for the
  # root qdisc, netem-loss while it drops everything, ingress and filter.
  # STUB_TC_NETEM (unknown, eperm) and STUB_TC_MATCHALL (classifier, action)
  # make the adds fail with the kernel's own messages; STUB_TC_NETEM=slow and
  # STUB_TC_MATCHALL=slow take 1.2 s to add netem and the ingress drop.
  cat >"$BOX/tc" <<'STUB'
#!/bin/bash
PATH="$HOST_PATH"
case "$*" in
  "qdisc add dev eth1 root netem loss 0%" | "qdisc add dev eth1 root netem loss 100%")
    case "${STUB_TC_NETEM:-ok}" in
      unknown) echo "Error: Specified qdisc kind is unknown." >&2; exit 2 ;;
      eperm) echo "RTNETLINK answers: Operation not permitted" >&2; exit 2 ;;
      slow) sleep 1.2 ;;
    esac
    touch "$STATE/netem"
    [ "$*" = "qdisc add dev eth1 root netem loss 0%" ] || touch "$STATE/netem-loss"
    ;;
  "qdisc change dev eth1 root netem loss 100%")
    [ -f "$STATE/netem" ] || { echo "Error: Qdisc not found. To create specify NLM_F_CREATE flag." >&2; exit 2; }
    touch "$STATE/netem-loss"
    ;;
  "qdisc add dev eth1 ingress") touch "$STATE/ingress" ;;
  "filter add dev eth1 ingress matchall action drop")
    case "${STUB_TC_MATCHALL:-ok}" in
      classifier) echo "Error: TC classifier not found." >&2; exit 2 ;;
      action) echo "Error: Failed to load TC action module." >&2; exit 2 ;;
      slow) sleep 1.2 ;;
    esac
    touch "$STATE/filter"
    ;;
  "qdisc del dev eth1 root") rm -f "$STATE/netem" "$STATE/netem-loss" ;;
  "qdisc del dev eth1 ingress") rm -f "$STATE/ingress" "$STATE/filter" ;;
  *)
    echo "box tc: unexpected call: $*" >&2
    exit 1
    ;;
esac
STUB

  chmod +x "$BIN/docker" "$BIN/modinfo" "$BIN/sleep" "$BOX/sleep" "$BOX/vtysh" "$BOX/ip" "$BOX/ping" "$BOX/tc"
  ln -s /bin/sh "$BOX/sh"
  ln -s "$(command -v grep)" "$BOX/grep"
  local f
  for f in sh grep sleep vtysh ip ping; do
    ln -s "$BOX/$f" "$BOX_NO_TC/$f"
  done
}

# fixture_file <node> <command>: where the fixture of <command> on <node> lives.
fixture_file() {
  printf '%s/%s/%s' "$FIX" "$1" "$(printf '%s' "$2" | tr -c 'A-Za-z0-9.-' '_')"
}

# fixture <node> <command> <answer>: what <command> prints on <node>.
fixture() {
  mkdir -p "$FIX/$1"
  printf '%s\n' "$3" >"$(fixture_file "$1" "$2")"
}

# reset_lab: a healthy lab, every stub switch off, eth1 clean.
reset_lab() {
  unset STUB_DEPLOY_RC STUB_STOPPED STUB_RESTARTED STUB_NO_LAB STUB_NO_TC STUB_TC_NETEM \
    STUB_TC_MATCHALL STUB_VTYSH_EMPTY STUB_NO_MODULES STUB_A5_PROBE STUB_NO_RECOVERY \
    STUB_SILENCED_DELAY KEEP_LAB
  rm -rf "$FIX" "$STATE" "$DOCKER_LOG"
  mkdir -p "$STATE"

  local n leaf_summary paths
  # Link MTUs: the node links at 9000, the fabric links at 9216.
  fixture node1 "ip -j link show" \
    '[{"ifname":"lo","mtu":65536},{"ifname":"eth0","mtu":1500},{"ifname":"eth1","mtu":9000},{"ifname":"eth2","mtu":9000},{"ifname":"fabric","mtu":65575}]'
  for n in leaf1 leaf2; do
    fixture "$n" "ip -j link show" \
      '[{"ifname":"eth0","mtu":1500},{"ifname":"eth1","mtu":9000},{"ifname":"eth2","mtu":9216},{"ifname":"vni20001","mtu":9166}]'
  done
  fixture leaf3 "ip -j link show" '[{"ifname":"eth0","mtu":1500},{"ifname":"eth1","mtu":9216}]'
  fixture spine1 "ip -j link show" \
    '[{"ifname":"eth0","mtu":1500},{"ifname":"eth1","mtu":9216},{"ifname":"eth2","mtu":9216},{"ifname":"eth3","mtu":9216}]'

  # A1: the node sessions in the deployment VRF, BFD on them, the EVPN
  # sessions at the reflector.
  leaf_summary='{"ipv4Unicast":{"peers":{"eth1":{"state":"Established"}}},"ipv6Unicast":{"peers":{"eth1":{"state":"Established"}}}}'
  for n in leaf1 leaf2; do
    fixture "$n" "vtysh -c show bgp vrf cobaltcore summary json" "$leaf_summary"
    fixture "$n" "vtysh -c show bfd peers json" '[{"interface":"eth1","vrf":"cobaltcore","status":"up"}]'
  done
  fixture leaf1 "vtysh -c show bfd peers json.silenced" '[{"interface":"eth1","vrf":"cobaltcore","status":"down"}]'
  fixture spine1 "vtysh -c show bgp summary json" \
    '{"l2VpnEvpn":{"peers":{"192.0.2.129":{"state":"Established"},"192.0.2.130":{"state":"Established"},"192.0.2.131":{"state":"Established"}}}}'

  # A2 and A3: leaf3 holds node1's host routes over both VTEPs; FRR prints
  # an imported IPv6 route's VTEP next hop IPv4-mapped.
  paths='"paths":[{"nexthops":[{"ip":"192.0.2.130"}],"multipath":true,"bestpath":{"overall":true}},{"nexthops":[{"ip":"192.0.2.129"}],"multipath":true}]'
  fixture leaf3 "vtysh -c show bgp vrf cobaltcore ipv4 unicast 192.0.2.1/32 json" \
    "{\"prefix\":\"192.0.2.1/32\",\"pathCount\":2,$paths}"
  fixture leaf3 "vtysh -c show bgp vrf cobaltcore ipv4 unicast 192.0.2.1/32 json.silenced" \
    '{"prefix":"192.0.2.1/32","pathCount":1,"paths":[{"nexthops":[{"ip":"192.0.2.130"}],"multipath":true,"bestpath":{"overall":true}}]}'
  fixture leaf3 "vtysh -c show bgp vrf cobaltcore ipv6 unicast 2001:db8::1/128 json" \
    "{\"prefix\":\"2001:db8::1/128\",\"pathCount\":2,${paths//192.0.2/::ffff:192.0.2}}"
  fixture leaf3 "ip -j route show vrf cobaltcore 192.0.2.1" \
    '[{"dst":"192.0.2.1","nexthops":[{"gateway":"192.0.2.130","dev":"br20001"},{"gateway":"192.0.2.129","dev":"br20001"}]}]'
  fixture spine1 "vtysh -c show bgp l2vpn evpn route type prefix json" \
    '{"192.0.2.129:2":{"rd":"192.0.2.129:2","[5]:[0]:[32]:[192.0.2.1]":{"paths":[[{}]]}},"192.0.2.130:2":{"rd":"192.0.2.130:2","[5]:[0]:[32]:[192.0.2.1]":{"paths":[[{}]]}},"192.0.2.131:2":{"rd":"192.0.2.131:2","[5]:[0]:[128]:[2001:db8:0:ff::3]":{"paths":[[{}]]}},"numPrefix":3,"numPaths":3}'

  # A4: both baits in place, both filters holding, two defaults on node1.
  fixture node1 "vtysh -c show bgp vrf fabric ipv4 unicast 203.0.113.0/24 json" \
    '{"prefix":"203.0.113.0/24","pathCount":1,"paths":[{}]}'
  fixture leaf1 "vtysh -c show bgp vrf cobaltcore ipv4 unicast 198.18.0.0/15 json" \
    '{"prefix":"198.18.0.0/15","pathCount":2,"paths":[{},{}]}'
  for n in leaf1 leaf2 leaf3; do
    fixture "$n" "vtysh -c show bgp vrf cobaltcore ipv4 unicast 203.0.113.0/24 json" '{}'
  done
  fixture node1 "vtysh -c show bgp vrf fabric ipv4 unicast 198.18.0.0/15 json" '{}'
  fixture node1 "vtysh -c show bgp vrf fabric ipv4 unicast 0.0.0.0/0 json" '{"pathCount":2,"paths":[{},{}]}'
  fixture node1 "vtysh -c show bgp vrf fabric ipv6 unicast ::/0 json" '{"pathCount":2,"paths":[{},{}]}'

  # A6: host routes only, the prefix limit on the leaf.
  fixture node1 "vtysh -c show bgp vrf fabric ipv4 unicast neighbors eth1 advertised-routes json" \
    '{"advertisedRoutes":{"192.0.2.1/32":{}}}'
  fixture node1 "vtysh -c show bgp vrf fabric ipv6 unicast neighbors eth1 advertised-routes json" \
    '{"advertisedRoutes":{"2001:db8::1/128":{}}}'
  fixture leaf1 "vtysh -c show running-config" 'router bgp 65000 vrf cobaltcore
  neighbor nodes maximum-prefix 8192'

  # A7: node1's main table is the node network's, its fabric VRF the
  # fabric's; leaf1 peers with the node in the deployment VRF only.
  fixture node1 "ip -j route show default" '[{"dst":"default","gateway":"172.24.0.1","dev":"eth0"}]'
  fixture node1 "ip -j route show vrf fabric default" \
    '[{"dst":"default","nexthops":[{"via":{"family":"inet6","host":"fe80::1"},"dev":"eth2"},{"via":{"family":"inet6","host":"fe80::2"},"dev":"eth1"}]}]'
  fixture node1 "ip -j route show" \
    '[{"dst":"default","gateway":"172.24.0.1","dev":"eth0"},{"dst":"172.24.0.0/16","dev":"eth0"}]'
  fixture node1 "ip -j -6 route show" '[{"dst":"fe80::/64","dev":"eth0"},{"dst":"default","dev":"eth0"}]'
  fixture node1 "ip -j route show vrf fabric" \
    '[{"dst":"default","nexthops":[{"dev":"eth2"},{"dev":"eth1"}]},{"dst":"203.0.113.0/24","dev":"fabric"}]'
  fixture node1 "ip -j -6 route show vrf fabric" \
    '[{"dst":"2001:db8::1","dev":"fabric"},{"dst":"fe80::/64","dev":"eth1"},{"dst":"fe80::/64","dev":"eth2"},{"dst":"default","nexthops":[{"dev":"eth2"},{"dev":"eth1"}]}]'
  fixture leaf1 "vtysh -c show bgp summary json" '{"l2VpnEvpn":{"peers":{"192.0.2.193":{"state":"Established"}}}}'
}

# run_runner [args...]: runs run.sh with the stubs first on PATH; its output
# lands in OUT, its exit code in RC.
run_runner() {
  OUT="$(PATH="$BIN:$HOST_PATH" bash "$RUNNER" "$@" 2>&1)"
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
  for f in netem netem-loss ingress filter; do
    [[ ! -f "$STATE/$f" ]] || left="$left $f"
  done
  assert_eq "$1" "" "$left"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_healthy_lab_passes_and_is_torn_down() {
  echo "Test: a healthy lab passes A1 to A7, restores eth1 and is torn down"
  reset_lab
  run_runner all

  assert_eq "the run exits 0" "0" "$RC"
  local id
  for id in A1 A2 A3 A4 A5 A6 A7; do
    assert_contains "$id passes" "$OUT" "$id PASS "
    assert_contains "the run record carries the $id row" "$OUT" "| $id | PASS |"
  done
  assert_contains "the record names the containerlab version" "$OUT" "with containerlab
0.81.1"
  assert_contains "the record names the lab tree" "$OUT" "(lab tree "
  assert_contains "A2 reads both route distinguishers on spine1" "$OUT" \
    "under 192.0.2.129:2 and 192.0.2.130:2"
  assert_eq "leaf1 goes silent only after leaf3's sampler saw 2 paths" "" \
    "$(cat "$STATE/silence-early" 2>/dev/null)"
  assert_eth1_clean "leaf1's eth1 is restored after A3"
  assert_torn_down "the lab is torn down"
}

test_stopped_node_fails_and_is_torn_down() {
  echo "Test: a node container that is not running fails the run with its log tail, the lab is still torn down"
  reset_lab
  export STUB_STOPPED=spine1
  run_runner all

  assert_eq "the run exits 1" "1" "$RC"
  assert_contains "the run names the stopped container" "$OUT" \
    "lab container clab-bgp-fabric-spine1 is not running"
  assert_contains "the run prints the box's log tail" "$OUT" "the failed setup.sh of clab-bgp-fabric-spine1"
  assert_not_contains "no assertion runs" "$OUT" "A1 "
  assert_torn_down "the lab is torn down"
}

test_failed_setup_fails_and_is_torn_down() {
  echo "Test: a box whose setup.sh failed (restarted by containerlab's policy) fails the run with its log tail"
  reset_lab
  export STUB_RESTARTED=leaf3
  run_runner all

  assert_eq "the run exits 1" "1" "$RC"
  assert_contains "the run names the restarted container" "$OUT" \
    "lab container clab-bgp-fabric-leaf3 exited (restart count 1): its setup.sh or FRR failed"
  assert_contains "the run prints the box's log tail" "$OUT" \
    "--- the last 20 log lines of clab-bgp-fabric-leaf3, which exited ---"
  assert_contains "the log tail is the box's own" "$OUT" "the failed setup.sh of clab-bgp-fabric-leaf3"
  assert_not_contains "no healthy box's log is printed" "$OUT" "the failed setup.sh of clab-bgp-fabric-node1"
  assert_torn_down "the lab is torn down"
}

test_failed_deploy_is_torn_down() {
  echo "Test: a failed deploy ends the run before check with the log tail, the lab is still torn down"
  reset_lab
  export STUB_DEPLOY_RC=1 STUB_STOPPED=leaf2
  run_runner all

  assert_nonzero_exit "the run exits non-zero" "$RC"
  assert_contains "the run says the deploy failed" "$OUT" "containerlab deploy failed"
  assert_contains "the run prints the exited box's log tail" "$OUT" "the failed setup.sh of clab-bgp-fabric-leaf2"
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

test_a3_skips_on_each_missing_module_text() {
  local setting text names
  for setting in "STUB_TC_NETEM=unknown|Specified qdisc kind is unknown|host kernel without sch_netem" \
    "STUB_TC_MATCHALL=classifier|TC classifier not found|sch_ingress, cls_matchall or act_gact" \
    "STUB_TC_MATCHALL=action|Failed to load TC action module|sch_ingress, cls_matchall or act_gact"; do
    text="${setting#*|}"
    names="${text#*|}"
    text="${text%%|*}"
    echo "Test: tc's \"$text\" makes A3 SKIP, and the run passes"
    reset_lab
    export "${setting%%|*}"
    run_runner all

    assert_contains "A3 is skipped" "$OUT" "A3 SKIP "
    assert_contains "the skip quotes the kernel's text" "$OUT" "$text"
    assert_contains "the skip names the missing module" "$OUT" "$names"
    assert_contains "the run record says so" "$OUT" "| A3 | SKIP |"
    assert_eq "the run exits 0" "0" "$RC"
    assert_eq "nothing on leaf1's eth1 drops when the in-box script exits" "" \
      "$(cat "$STATE/drop-at-exit" 2>/dev/null)"
    assert_eth1_clean "leaf1's eth1 carries nothing"
  done
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
  rm "$(fixture_file leaf1 "vtysh -c show bfd peers json").silenced"
  run_runner all

  assert_contains "A3 fails on BFD" "$OUT" "A3 FAIL BFD on leaf1 still up 200 samples"
  assert_eq "the run exits 1" "1" "$RC"
  assert_eth1_clean "leaf1's eth1 is restored"
}

test_a3_fails_when_leaf3_keeps_both_paths() {
  echo "Test: a leaf3 that keeps both paths after BFD went down fails A3"
  reset_lab
  rm "$(fixture_file leaf3 "vtysh -c show bgp vrf cobaltcore ipv4 unicast 192.0.2.1/32 json").silenced"
  run_runner all

  assert_contains "A3 fails on the withdraw" "$OUT" "leaf3 still had 2 paths after 140 samples"
  assert_eq "the run exits 1" "1" "$RC"
}

test_a3_waits_for_the_second_path_before_the_silence() {
  echo "Test: a leaf3 at one path when the sampler starts is sampled again, and A3 passes"
  reset_lab
  local f
  f="$(fixture_file leaf3 "vtysh -c show bgp vrf cobaltcore ipv4 unicast 192.0.2.1/32 json")"
  cp "$f.silenced" "$f.sampler-first"
  run_runner all

  assert_eq "the sampler read leaf3 at one path first" "" "$(ls "$f.sampler-first" 2>/dev/null)"
  assert_contains "A3 passes" "$OUT" "A3 PASS "
  assert_eq "leaf1 goes silent only after leaf3's sampler saw 2 paths" "" \
    "$(cat "$STATE/silence-early" 2>/dev/null)"
  assert_eq "the run exits 0" "0" "$RC"
}

test_a3_fails_when_the_sampler_never_sees_two_paths() {
  echo "Test: a sampler that never sees 2 paths fails A3, and leaf1 is never silenced"
  reset_lab
  local f
  f="$(fixture_file leaf3 "vtysh -c show bgp vrf cobaltcore ipv4 unicast 192.0.2.1/32 json")"
  cp "$f.silenced" "$f"
  run_runner all

  assert_contains "A3 fails before the silence" "$OUT" \
    "A3 FAIL leaf3's sampler saw no 2 paths for 192.0.2.1/32 before the silence; leaf3 holds 1"
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

test_a3_clock_starts_at_the_ingress_drop() {
  echo "Test: A3's clock starts once the ingress drop is in place, so a drop that takes 1.2 s passes"
  reset_lab
  export STUB_TC_MATCHALL=slow
  run_runner all

  assert_contains "A3 passes" "$OUT" "A3 PASS "
  assert_eq "the run exits 0" "0" "$RC"
  assert_eth1_clean "leaf1's eth1 is restored"
}

test_a3_clock_starts_after_the_netem_add() {
  echo "Test: netem goes on before A3's clock starts, so a netem add that takes 1.2 s passes"
  reset_lab
  export STUB_TC_NETEM=slow
  run_runner all

  assert_contains "A3 passes" "$OUT" "A3 PASS "
  assert_eq "the run exits 0" "0" "$RC"
  assert_eth1_clean "leaf1's eth1 is restored"
}

test_a3_fails_when_bfd_misses_its_bound() {
  echo "Test: a BFD session that goes down 1.2 s into the silence fails A3 on the 1000 ms bound"
  reset_lab
  export STUB_SILENCED_DELAY=leaf1:1.2
  run_runner all

  local line
  line="$(grep '^A3 ' <<<"$OUT")"
  assert_contains "A3 fails on BFD after more than 1000 ms" "$line" "A3 FAIL BFD down after 1"
  assert_contains "A3 names BFD's bound" "$line" " ms (bound 1000 ms), leaf3 at 1 path after "
  assert_eq "the run exits 1" "1" "$RC"
}

test_a3_fails_when_the_withdraw_misses_its_bound() {
  echo "Test: a leaf3 that drops to one path 2.2 s into the silence fails A3 on the 2000 ms bound"
  reset_lab
  export STUB_SILENCED_DELAY=leaf3:2.2
  run_runner all

  local line
  line="$(grep '^A3 ' <<<"$OUT")"
  assert_contains "A3 fails on the withdraw after more than 2000 ms" "$line" \
    " ms (bound 1000 ms), leaf3 at 1 path after 2"
  assert_contains "A3 names the withdraw bound" "$line" " ms (bound 2000 ms)"
  assert_eq "the run exits 1" "1" "$RC"
}

test_a3_fails_when_leaf3_does_not_recover() {
  echo "Test: a leaf3 still at one path after the restore fails A3"
  reset_lab
  export STUB_NO_RECOVERY=true
  run_runner all

  assert_contains "A3 fails on the restore" "$OUT" "not back to 2 paths 2 s after the restore"
  assert_eq "the run exits 1" "1" "$RC"
}

test_a2_fails_on_one_route_distinguisher() {
  echo "Test: A2 fails when spine1 holds node1's Type-5 route under one route distinguisher"
  reset_lab
  fixture spine1 "vtysh -c show bgp l2vpn evpn route type prefix json" \
    '{"192.0.2.129:2":{"rd":"192.0.2.129:2","[5]:[0]:[32]:[192.0.2.1]":{"paths":[[{}]]}},"numPrefix":1}'
  run_runner all

  assert_contains "A2 names the route distinguishers it found" "$OUT" \
    "A2 FAIL after 1 s: spine1 holds [5]:[0]:[32]:[192.0.2.1] under the route distinguishers '192.0.2.129:2'"
  assert_eq "the run exits 1" "1" "$RC"
}

test_a4_fails_without_the_node_bait() {
  echo "Test: A4 fails when node1 does not hold 203.0.113.0/24"
  reset_lab
  fixture node1 "vtysh -c show bgp vrf fabric ipv4 unicast 203.0.113.0/24 json" '{}'
  run_runner all

  assert_contains "A4 names the missing bait" "$OUT" \
    "A4 FAIL the bait 203.0.113.0/24 is not in node1's BGP table"
  assert_eq "the run exits 1" "1" "$RC"
}

test_a4_fails_without_the_leaf_bait() {
  echo "Test: A4 fails when leaf1 does not hold 198.18.0.0/15"
  reset_lab
  fixture leaf1 "vtysh -c show bgp vrf cobaltcore ipv4 unicast 198.18.0.0/15 json" '{}'
  run_runner all

  assert_contains "A4 names the missing bait" "$OUT" \
    "A4 FAIL the bait 198.18.0.0/15 is not in leaf1's cobaltcore BGP table"
  assert_eq "the run exits 1" "1" "$RC"
}

# Each row: <node>|<command>|<answer>|<verdict>. A healthy lab whose <node>
# answers <command> with <answer> prints <verdict> and fails the run.
test_a_wrong_answer_fails_its_assertion() {
  local node cmd answer want
  while IFS='|' read -r node cmd answer want <&3; do
    echo "Test: $want"
    reset_lab
    fixture "$node" "$cmd" "$answer"
    run_runner all

    assert_contains "the run prints the verdict" "$OUT" "$want"
    assert_eq "the run exits 1" "1" "$RC"
  done 3<<'EOF'
node1|vtysh -c show bgp vrf fabric ipv4 unicast neighbors eth1 advertised-routes json|{"advertisedRoutes":{"192.0.2.1/32":{},"203.0.113.0/24":{}}}|A4 FAIL node1 advertises 203.0.113.0/24 on eth1
leaf2|vtysh -c show bgp vrf cobaltcore ipv4 unicast 203.0.113.0/24 json|{"pathCount":1,"paths":[{}]}|A4 FAIL leaf2 holds 203.0.113.0/24 with 1 paths
node1|vtysh -c show bgp vrf fabric ipv4 unicast 198.18.0.0/15 json|{"pathCount":2,"paths":[{},{}]}|A4 FAIL node1 accepted 198.18.0.0/15 with 2 paths
node1|vtysh -c show bgp vrf fabric ipv4 unicast 0.0.0.0/0 json|{"pathCount":1,"paths":[{}]}|A4 FAIL node1 has 1 paths for 0.0.0.0/0 and 2 for ::/0
leaf2|ip -j link show|[{"ifname":"eth1","mtu":1500}]|A5 FAIL link ends at the wrong MTU: leaf2:eth1=1500 (want 9000) leaf2:eth2=absent (want 9216)
node1|vtysh -c show bgp vrf fabric ipv4 unicast neighbors eth1 advertised-routes json|{"advertisedRoutes":{"192.0.2.1/32":{},"10.0.0.0/24":{}}}|A6 FAIL node1 advertises on eth1 in ipv4: 10.0.0.0/24
node1|vtysh -c show bgp vrf fabric ipv6 unicast neighbors eth1 advertised-routes json|{"advertisedRoutes":{}}|A6 FAIL node1 advertises on eth1 in ipv6: nothing
leaf1|vtysh -c show running-config|router bgp 65000 vrf cobaltcore|A6 FAIL leaf1 running config has no neighbor nodes maximum-prefix 8192
EOF
}

# Each row: <reading>|<verdict>. node1's probe printing <reading>, "<reply>
# <fragments of the 9000-byte probe> <fragments of the 9001-byte probe>"
# when it works, prints <verdict> and fails the run.
test_a5_fails_on_each_failed_probe_reading() {
  local probe want
  while IFS='|' read -r probe want <&3; do
    echo "Test: $want"
    reset_lab
    export STUB_A5_PROBE="$probe"
    run_runner all

    assert_contains "the run prints the verdict" "$OUT" "$want"
    assert_eq "the run exits 1" "1" "$RC"
  done 3<<'EOF'
0 0 0|A5 FAIL 10 link ends at their MTU, but the 9000-byte probe from node1 to 2001:db8:0:ff::3 got no reply
1 2 2|A5 FAIL the 9000-byte probe was answered but node1 cut it into 2 fragments
1 0 0|A5 FAIL the 9001-byte probe left node1 without being fragmented
awk: not found|A5 FAIL the probe on node1 printed 'awk: not found'
EOF
}

test_a7_fails_on_a_main_table_route_via_eth1() {
  echo "Test: A7 fails when node1's main table holds a route via eth1"
  reset_lab
  fixture node1 "ip -j route show" \
    '[{"dst":"default","gateway":"172.24.0.1","dev":"eth0"},{"dst":"10.99.0.0/16","dev":"eth1"}]'
  run_runner all

  assert_contains "A7 names the route" "$OUT" \
    "A7 FAIL node1's main table holds fabric routes: 10.99.0.0/16 dev eth1"
  assert_eq "the run exits 1" "1" "$RC"
}

test_a7_fails_on_a_fabric_route_via_eth0() {
  echo "Test: A7 fails when node1's fabric VRF holds a route via eth0"
  reset_lab
  fixture node1 "ip -j -6 route show vrf fabric" '[{"dst":"fd00::/64","dev":"eth0"}]'
  run_runner all

  assert_contains "A7 names the route" "$OUT" \
    "A7 FAIL node1's fabric VRF holds node-network routes: fd00::/64 dev eth0"
  assert_eq "the run exits 1" "1" "$RC"
}

test_every_vtysh_assertion_fails_on_an_empty_answer() {
  echo "Test: every assertion that asks vtysh fails on an empty answer"
  reset_lab
  export STUB_VTYSH_EMPTY=true
  run_runner all

  local id
  for id in A1 A2 A3 A4 A6 A7; do
    assert_contains "$id fails on the empty answer" "$(grep "^$id " <<<"$OUT")" "empty answer"
  done
  assert_contains "A5 asks no vtysh and passes" "$OUT" "A5 PASS "
  assert_not_contains "no jq parse error reaches the output" "$OUT" "parse error"
  assert_eq "the run exits 1" "1" "$RC"
  assert_torn_down "the lab is torn down"
}

test_check_without_a_lab_names_node1() {
  echo "Test: check with no lab deployed names the missing node1 container"
  reset_lab
  export STUB_NO_LAB=true
  run_runner check

  assert_eq "check exits 1" "1" "$RC"
  assert_contains "check names the container" "$OUT" \
    "lab container clab-bgp-fabric-node1 is not running; run 'run.sh up' first"
  assert_not_contains "no jq parse error reaches the output" "$OUT" "parse error"
}

test_unknown_subcommand_prints_usage() {
  echo "Test: an unknown subcommand prints the four accepted ones"
  reset_lab
  run_runner frobnicate

  assert_eq "the run exits 1" "1" "$RC"
  assert_contains "usage names the subcommands" "$OUT" "usage: run.sh [up|check|down|all]"
  assert_file_not_contains "no docker call is made" "$DOCKER_LOG" "docker"
}

test_host_without_docker_is_refused() {
  echo "Test: a host without docker on PATH is refused before any container starts"
  reset_lab
  OUT="$(PATH=/nonexistent "$BASH" "$RUNNER" 2>&1)"
  RC=$?

  assert_eq "the run exits 1" "1" "$RC"
  assert_contains "the run names docker" "$OUT" "docker is not on PATH"
  assert_file_not_contains "no docker call is made" "$DOCKER_LOG" "docker"
}

test_kernel_without_modules_is_refused() {
  echo "Test: a kernel without the vrf and vxlan modules is refused before any container starts"
  reset_lab
  export STUB_NO_MODULES=true
  run_runner all

  assert_eq "the run exits 1" "1" "$RC"
  assert_contains "the run names both modules" "$OUT" \
    "the host kernel ships no vrf/vxlan module; the lab needs both"
  assert_file_not_contains "no containerlab runs" "$DOCKER_LOG" "containerlab"
}

# --- Run ---
make_stubs
test_healthy_lab_passes_and_is_torn_down
test_stopped_node_fails_and_is_torn_down
test_failed_setup_fails_and_is_torn_down
test_failed_deploy_is_torn_down
test_keep_lab_keeps_the_lab
test_a3_skips_on_each_missing_module_text
test_a3_fails_without_tc_in_the_image
test_a3_fails_on_any_other_tc_error
test_a3_fails_when_bfd_stays_up
test_a3_fails_when_leaf3_keeps_both_paths
test_a3_waits_for_the_second_path_before_the_silence
test_a3_fails_when_the_sampler_never_sees_two_paths
test_a3_fails_when_mktemp_fails
test_a3_clock_starts_at_the_ingress_drop
test_a3_clock_starts_after_the_netem_add
test_a3_fails_when_bfd_misses_its_bound
test_a3_fails_when_the_withdraw_misses_its_bound
test_a3_fails_when_leaf3_does_not_recover
test_a2_fails_on_one_route_distinguisher
test_a4_fails_without_the_node_bait
test_a4_fails_without_the_leaf_bait
test_a_wrong_answer_fails_its_assertion
test_a5_fails_on_each_failed_probe_reading
test_a7_fails_on_a_main_table_route_via_eth1
test_a7_fails_on_a_fabric_route_via_eth0
test_every_vtysh_assertion_fails_on_an_empty_answer
test_check_without_a_lab_names_node1
test_unknown_subcommand_prints_usage
test_host_without_docker_is_refused
test_kernel_without_modules_is_refused

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
