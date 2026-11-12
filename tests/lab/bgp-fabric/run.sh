#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Deploys, checks and destroys the containerlab lab of the BGP fabric contract
# (docs/reference/networking/bgp-fabric-contract.md). containerlab runs from
# its pinned container image, so the host needs Docker, a user in the docker
# group, jq and a kernel with the vrf and vxlan modules, and no installed
# containerlab. check runs the assertions A1 to A7 of the page's lab plan,
# prints one PASS, FAIL or SKIP line per assertion and the run record for the
# page, and exits 1 when an assertion fails.
#
# Usage: tests/lab/bgp-fabric/run.sh [up|check|down|all]
#   KEEP_LAB=true  all skips the teardown, so a failed lab can be inspected
#   CLAB_IMAGE     the containerlab image (default: the pin below)
#   A1_WAIT_MS, A2_WAIT_MS, A3_READY_WAIT_MS, A3_RESTORE_WAIT_MS
#                  how long check waits for a state before it fails; the
#                  defaults suit a real lab, the unit test shortens them

set -euo pipefail

# Builtins only up to the docker check, so a host without docker on PATH gets
# that message and nothing else.
case "${BASH_SOURCE[0]}" in
  */*) LAB_DIR="$(cd "${BASH_SOURCE[0]%/*}" && pwd)" ;;
  *) LAB_DIR="$PWD" ;;
esac
LAB=bgp-fabric
TOPOLOGY=bgp-fabric.clab.yml
NODES="node1 leaf1 leaf2 leaf3 spine1"
CLAB_IMAGE_DEFAULT="ghcr.io/srl-labs/clab:0.81.1@sha256:9524cfe7e61af502e16fde2e8afcf2256e8eb4672ed5c5417a2f67fdb22c34cf"
CLAB_IMAGE="${CLAB_IMAGE:-$CLAB_IMAGE_DEFAULT}"
KEEP_LAB="${KEEP_LAB:-false}"

# The bounds are the contract's (C5) and not configurable.
A3_BFD_BOUND_MS=1000      # BFD down after the silence
A3_WITHDRAW_BOUND_MS=2000 # leaf3 down to one path after the silence
A3_BFD_SAMPLES=200        # 20 ms apart
A3_WITHDRAW_SAMPLES=140   # 100 ms apart from before the silence: 4 s for BFD, then 10 s
# The waits only end a lab that never reaches a state.
A1_WAIT_MS="${A1_WAIT_MS:-60000}"                 # A1: sessions and BFD up after 'up'
A2_WAIT_MS="${A2_WAIT_MS:-30000}"                 # A2: node1's routes at leaf3 after A1
A3_READY_WAIT_MS="${A3_READY_WAIT_MS:-10000}"     # A3: leaf3's sampler at two paths before the silence
A3_RESTORE_WAIT_MS="${A3_RESTORE_WAIT_MS:-60000}" # A3: leaf3 back at two paths after the restore

FAILED=0
RECORD_ROWS=""
A2_VERDICT=""
A2_MEASURED=""

usage() {
  cat >&2 <<EOF
usage: ${0##*/} [up|check|down|all]
  up     deploy the lab (containerlab deploy --reconfigure)
  check  run the assertions A1 to A7 against the deployed lab
  down   destroy the lab (containerlab destroy --cleanup)
  all    up, check, then down unless KEEP_LAB=true; the default subcommand
EOF
  exit 1
}

die() {
  echo "run.sh: $*" >&2
  exit 1
}

preconditions() {
  command -v docker >/dev/null 2>&1 || die "docker is not on PATH"
  docker info >/dev/null 2>&1 || die "docker is not usable by this user; join the docker group"
  command -v jq >/dev/null 2>&1 || die "jq is not on PATH"
  # modinfo often lives in an sbin directory a user's PATH leaves out.
  local modinfo n
  modinfo="$(PATH="$PATH:/usr/sbin:/sbin" command -v modinfo)" ||
    die "modinfo is not on PATH; the lab checks the vrf and vxlan modules with it"
  for n in vrf vxlan; do
    "$modinfo" -n "$n" >/dev/null 2>&1 || die "the host kernel ships no vrf/vxlan module; the lab needs both"
  done
  [ -f "$LAB_DIR/daemons" ] || die "missing daemons under $LAB_DIR"
  for n in $NODES; do
    [ -f "$LAB_DIR/$n/frr.conf" ] || die "missing $n/frr.conf under $LAB_DIR"
    [ -f "$LAB_DIR/$n/setup.sh" ] || die "missing $n/setup.sh under $LAB_DIR"
  done
}

# clab runs containerlab in the form its install page documents: privileged,
# with the Docker socket and the host's network and PID namespaces, the lab
# directory mounted at its own path.
clab() {
  docker run --rm --privileged --network host \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v /var/run/netns:/var/run/netns \
    -v /etc/hosts:/etc/hosts \
    -v /var/lib/docker/containers:/var/lib/docker/containers \
    --pid=host \
    -v "$LAB_DIR:$LAB_DIR" -w "$LAB_DIR" \
    "$CLAB_IMAGE" containerlab "$@"
}

# box_state <node> prints "<running> <restart count>" of the node's container,
# nothing when it does not exist. containerlab gives every box the restart
# policy always, so a box whose setup.sh or FRR exits shows up as restarted.
box_state() {
  docker inspect --format '{{.State.Running}} {{.RestartCount}}' "clab-$LAB-$1" 2>/dev/null || true
}

# exited_logs prints the log tail of every lab container that is not running
# or has restarted. A setup.sh that fails (a missing kernel module, an
# address already present on a kept lab) ends its box before containerlab
# reports anything.
exited_logs() {
  local n state
  for n in $NODES; do
    state="$(box_state "$n")"
    case "$state" in
      "" | "true 0") continue ;;
    esac
    echo "--- the last 20 log lines of clab-$LAB-$n, which exited ---" >&2
    docker logs --tail 20 "clab-$LAB-$n" >&2 2>&1 || true
  done
}

lab_up() {
  clab deploy -t "$TOPOLOGY" --reconfigure || {
    exited_logs
    die "containerlab deploy failed"
  }
  require_lab
}

lab_down() { clab destroy -t "$TOPOLOGY" --cleanup; }

require_lab() {
  local n state
  for n in $NODES; do
    state="$(box_state "$n")"
    case "$state" in
      "true 0") ;;
      "") die "lab container clab-$LAB-$n is not running; run '${0##*/} up' first" ;;
      false*)
        exited_logs
        die "lab container clab-$LAB-$n is not running; run '${0##*/} up' first"
        ;;
      *)
        exited_logs
        die "lab container clab-$LAB-$n exited (restart count ${state#* }): its setup.sh or FRR failed"
        ;;
    esac
  done
}

# vty <node> <command> prints the output of one vtysh command, or the reason
# it has none and returns 1. The image ships no vtysh.conf and vtysh says so
# on stderr on every call; that notice is what 2>/dev/null drops. vtysh
# prints its own errors on stdout and exits 1.
vty() {
  local out
  if ! out="$(docker exec "clab-$LAB-$1" vtysh -c "$2" 2>/dev/null)"; then
    echo "vtysh on $1 failed: $2: ${out:-no output}"
    return 1
  fi
  if [ -z "$out" ]; then
    echo "vtysh on $1 failed: $2: empty answer"
    return 1
  fi
  printf '%s\n' "$out"
}

# prefix_paths <node> <vrf> <family> <prefix> prints the number of BGP paths
# for <prefix>, 0 when the table has none, or the reason and returns 1.
prefix_paths() {
  local out
  out="$(vty "$1" "show bgp vrf $2 $3 unicast $4 json")" || { echo "$out"; return 1; }
  jq '.paths // [] | length' <<<"$out" 2>/dev/null || {
    echo "vtysh on $1 answered no JSON for $4"
    return 1
  }
}

# now_ms reads the host's uptime clock in milliseconds. The lab containers
# read the same clock, so a timestamp taken inside one compares with it.
now_ms() {
  local up _
  read -r up _ </proc/uptime
  uptime_ms "$up"
}

uptime_ms() { # <seconds with two decimals, as /proc/uptime prints them>
  echo $((10#${1%.*} * 1000 + 10#${1#*.} * 10))
}

report() { # <id> <PASS|FAIL|SKIP> <measured>
  echo "$1 $2 $3"
  RECORD_ROWS="$RECORD_ROWS| $1 | $2 | $3 |
"
  [ "$2" != FAIL ] || FAILED=1
}

# --- A1: every session Established, BFD up on the node sessions ---

a1_unmet() { # prints the first unmet condition, nothing when all hold
  local n af p out
  for n in leaf1 leaf2; do
    out="$(vty "$n" 'show bgp vrf cobaltcore summary json')" || { echo "$out"; return; }
    for af in ipv4Unicast ipv6Unicast; do
      [ "$(jq -r --arg af "$af" '.[$af].peers.eth1.state // "absent"' <<<"$out" 2>/dev/null)" = Established ] ||
        { echo "$n eth1 not Established in $af"; return; }
    done
    out="$(vty "$n" 'show bfd peers json')" || { echo "$out"; return; }
    [ "$(jq -r '[.[] | select(.interface == "eth1") | .status] | first // "absent"' <<<"$out" 2>/dev/null)" = up ] ||
      { echo "$n BFD peer on eth1 not up"; return; }
  done
  out="$(vty spine1 'show bgp summary json')" || { echo "$out"; return; }
  for p in 192.0.2.129 192.0.2.130 192.0.2.131; do
    [ "$(jq -r --arg p "$p" '.l2VpnEvpn.peers[$p].state // "absent"' <<<"$out" 2>/dev/null)" = Established ] ||
      { echo "spine1 $p not Established in l2VpnEvpn"; return; }
  done
}

a1() {
  local start unmet
  start="$(now_ms)"
  # vtysh fails while the daemons are still starting; a1_unmet reports that
  # as an unmet condition, and A1_WAIT_MS ends the wait.
  while :; do
    unmet="$(a1_unmet)" || unmet="the session check exited with an error"
    [ -n "$unmet" ] || break
    if [ $(($(now_ms) - start)) -ge "$A1_WAIT_MS" ]; then
      report A1 FAIL "after $((A1_WAIT_MS / 1000)) s: $unmet"
      return
    fi
    sleep 2
  done
  report A1 PASS "leaf1 and leaf2 eth1 Established in ipv4Unicast and ipv6Unicast with BFD up, spine1 l2VpnEvpn Established to 192.0.2.129, .130 and .131, after $((($(now_ms) - start) / 1000)) s"
}

# --- A2: the observer VTEP holds node1's host routes over both leaves ---

# a2_try sets A2_VERDICT and A2_MEASURED from one reading of the lab.
a2_try() {
  local v4 v6 fib evpn rds want check
  A2_VERDICT=FAIL
  want='["192.0.2.129","192.0.2.130"]'
  # FRR prints the VTEP next hop of an imported IPv6 route IPv4-mapped.
  check='(.paths // [] | length) == 2
    and ([.paths[] | .nexthops[0].ip // "none" | sub("^::ffff:"; "")] | sort) == $want
    and ([.paths[] | select(.bestpath.overall == true)] | length) == 1
    and ([.paths[] | select(.bestpath.overall != true) | select(.multipath == true)] | length) == 1'
  v4="$(vty leaf3 'show bgp vrf cobaltcore ipv4 unicast 192.0.2.1/32 json')" || { A2_MEASURED="$v4"; return; }
  v6="$(vty leaf3 'show bgp vrf cobaltcore ipv6 unicast 2001:db8::1/128 json')" || { A2_MEASURED="$v6"; return; }
  fib="$(docker exec "clab-$LAB-leaf3" ip -j route show vrf cobaltcore 192.0.2.1 | jq '.[0].nexthops // [] | length')" ||
    { A2_MEASURED="ip route on leaf3 failed"; return; }
  evpn="$(vty spine1 'show bgp l2vpn evpn route type prefix json')" || { A2_MEASURED="$evpn"; return; }
  # FRR keys EVPN routes by route distinguisher, then by their NLRI string.
  rds="$(jq -r --arg k '[5]:[0]:[32]:[192.0.2.1]' \
    '[to_entries[] | select((.value | type) == "object" and (.value | has($k))) | .key] | sort | join(" ")' \
    <<<"$evpn" 2>/dev/null)" || rds=""
  if [ "$(jq --argjson want "$want" "$check" <<<"$v4" 2>/dev/null)" != true ]; then
    A2_MEASURED="192.0.2.1/32 on leaf3: $(jq -c '[.paths // [] | .[] | {nexthop: .nexthops[0].ip, best: .bestpath.overall, multipath}]' <<<"$v4" 2>/dev/null)"
  elif [ "$(jq --argjson want "$want" "$check" <<<"$v6" 2>/dev/null)" != true ]; then
    A2_MEASURED="2001:db8::1/128 on leaf3: $(jq -c '[.paths // [] | .[] | {nexthop: .nexthops[0].ip, best: .bestpath.overall, multipath}]' <<<"$v6" 2>/dev/null)"
  elif [ "$fib" != 2 ]; then
    A2_MEASURED="192.0.2.1 has $fib next hops in leaf3's cobaltcore table"
  elif ! [[ "$rds" =~ ^192\.0\.2\.129:[0-9]+\ 192\.0\.2\.130:[0-9]+$ ]]; then
    A2_MEASURED="spine1 holds [5]:[0]:[32]:[192.0.2.1] under the route distinguishers '${rds}', not one of 192.0.2.129 and one of 192.0.2.130"
  else
    A2_VERDICT=PASS
    A2_MEASURED="2 paths for 192.0.2.1/32 and 2001:db8::1/128 on leaf3 over the VTEPs 192.0.2.129 and 192.0.2.130, one bestpath and one multipath each, 2 kernel next hops; spine1 holds [5]:[0]:[32]:[192.0.2.1] under ${rds/ / and }"
  fi
}

# The sessions of A1 can be up before the leaves' Type-5 routes have reached
# leaf3, so a2 reads the lab until it holds or A2_WAIT_MS ends the wait.
a2() {
  local start
  start="$(now_ms)"
  until a2_try && [ "$A2_VERDICT" = PASS ]; do
    if [ $(($(now_ms) - start)) -ge "$A2_WAIT_MS" ]; then
      report A2 FAIL "after $((A2_WAIT_MS / 1000)) s: $A2_MEASURED"
      return
    fi
    sleep 2
  done
  report A2 PASS "$A2_MEASURED"
}

# --- A3: BFD detects a silent node link, the remote VTEP withdraws the path ---

# Runs in leaf1 with the sample bound as $1. The ingress filter drops what
# node1 sends to leaf1, netem what leaf1 sends to node1, so the link is silent
# both ways with its carrier up. netem goes on at 0% loss before the ingress
# drop and rises to 100% loss after it, through a change that loads no module.
# Prints the uptime once the ingress drop is in place, from when leaf1
# receives nothing from node1, and the uptime when BFD reports the node peer
# down, sampled every 20 ms; the tc calls that add the qdiscs and the filter,
# with any module the kernel loads for them, stay out of the measurement. The
# exit code tells a3 what happened: 97 the kernel has no sch_netem (nothing on
# eth1 drops yet), 96 it lacks a module of the ingress drop, 95 any other tc
# failure, 98 BFD still up after $1 samples. The three texts are the kernel's
# own extack messages for a missing module.
A3_SILENCE='
missing_module() {
  case "$1" in *"kind is unknown"* | *"TC classifier not found"* | *"Failed to load TC action module"*) return 0 ;; esac
  return 1
}
command -v tc >/dev/null 2>&1 || { echo "tc is not in the image"; exit 95; }
err=$(tc qdisc add dev eth1 root netem loss 0% 2>&1) || {
  echo "$err"
  missing_module "$err" && exit 97
  exit 95
}
err=$( (tc qdisc add dev eth1 ingress && tc filter add dev eth1 ingress matchall action drop) 2>&1) || {
  echo "$err"
  missing_module "$err" && exit 96
  exit 95
}
read -r s _ </proc/uptime
err=$(tc qdisc change dev eth1 root netem loss 100% 2>&1) || {
  echo "$err"
  exit 95
}
i=0
until vtysh -c "show bfd peers json" 2>/dev/null | grep -q "\"status\":\"down\""; do
  i=$((i + 1))
  [ "$i" -lt "$1" ] || exit 98
  sleep 0.02
done
read -r e _ </proc/uptime
echo "$s $e"'

# Runs in leaf3 with the sample bounds as $1 and $2. Samples every 100 ms
# until node1's /32 shows two paths and prints ready, then samples every
# 100 ms and prints the uptime once leaf3 holds one path; exits 1 when the two
# paths do not show within $2 samples or the one path not within $1. a3
# passes A3_READY_WAIT_MS in 100 ms samples as $2, so the sampler keeps trying
# as long as a3 waits for the ready line. a3 silences leaf1 only after that
# line, so leaf3 was at two paths before the silence and no docker exec
# start-up lands in the measurement.
A3_WITHDRAW='
i=0
until vtysh -c "show bgp vrf cobaltcore ipv4 unicast 192.0.2.1/32 json" 2>/dev/null | grep -q "\"pathCount\":2,"; do
  i=$((i + 1))
  [ "$i" -lt "$2" ] || exit 1
  sleep 0.1
done
echo ready
i=0
until vtysh -c "show bgp vrf cobaltcore ipv4 unicast 192.0.2.1/32 json" 2>/dev/null | grep -q "\"pathCount\":1,"; do
  i=$((i + 1))
  [ "$i" -lt "$1" ] || exit 1
  sleep 0.1
done
read -r e _ </proc/uptime
echo "$e"'

a3_restore() {
  docker exec "clab-$LAB-leaf1" sh -c 'tc qdisc del dev eth1 root 2>/dev/null; tc qdisc del dev eth1 ingress 2>/dev/null; true'
}

a3() {
  local out rc=0 s e w start bfd_ms withdraw_ms="" held="" back_ms="" paths wfile wpid
  wfile="$(mktemp)" || { report A3 FAIL "mktemp failed on the host, so eth1 was not silenced"; return; }
  # From the first tc call to the restore, an interrupt restores eth1 too.
  trap 'a3_restore; rm -f "$wfile"; exit 130' INT TERM
  docker exec "clab-$LAB-leaf3" sh -c "$A3_WITHDRAW" a3 "$A3_WITHDRAW_SAMPLES" "$((A3_READY_WAIT_MS / 100))" >"$wfile" &
  wpid=$!
  start="$(now_ms)"
  until [ "$(head -n 1 "$wfile")" = ready ]; do
    if ! kill -0 "$wpid" 2>/dev/null || [ $(($(now_ms) - start)) -ge "$A3_READY_WAIT_MS" ]; then
      kill "$wpid" 2>/dev/null || true
      wait "$wpid" 2>/dev/null || true
      rm -f "$wfile"
      trap - INT TERM
      paths="$(prefix_paths leaf3 cobaltcore ipv4 192.0.2.1/32)" || true
      report A3 FAIL "leaf3's sampler saw no 2 paths for 192.0.2.1/32 before the silence; leaf3 holds $paths"
      return
    fi
    sleep 0.05
  done
  out="$(docker exec "clab-$LAB-leaf1" sh -c "$A3_SILENCE" a3 "$A3_BFD_SAMPLES")" || rc=$?
  if [ "$rc" = 0 ]; then
    read -r s e <<<"$out"
    start="$(uptime_ms "$s")"
    bfd_ms=$(($(uptime_ms "$e") - start))
    if wait "$wpid" && w="$(sed -n 2p "$wfile")" && [ -n "$w" ]; then
      withdraw_ms=$(($(uptime_ms "$w") - start))
    else
      held="$(prefix_paths leaf3 cobaltcore ipv4 192.0.2.1/32)" || true
    fi
  else
    # Killing the docker exec leaves the sampling in leaf3 to end at its bound.
    kill "$wpid" 2>/dev/null || true
    wait "$wpid" 2>/dev/null || true
  fi
  rm -f "$wfile"
  a3_restore
  trap - INT TERM
  case "$rc" in
    0) ;;
    97)
      report A3 SKIP "tc qdisc add dev eth1 root netem loss 0% failed on leaf1 ($out): host kernel without sch_netem"
      return
      ;;
    96)
      report A3 SKIP "the ingress drop on leaf1 eth1 failed ($out): host kernel without sch_ingress, cls_matchall or act_gact"
      return
      ;;
    98)
      report A3 FAIL "BFD on leaf1 still up $A3_BFD_SAMPLES samples, 20 ms apart, after eth1 went silent"
      return
      ;;
    *)
      report A3 FAIL "silencing eth1 on leaf1 failed: $out"
      return
      ;;
  esac

  start="$(now_ms)"
  while [ $(($(now_ms) - start)) -lt "$A3_RESTORE_WAIT_MS" ]; do
    paths="$(prefix_paths leaf3 cobaltcore ipv4 192.0.2.1/32)" || paths=error
    if [ "$paths" = 2 ]; then
      back_ms=$(($(now_ms) - start))
      break
    fi
    sleep 1
  done

  if [ -z "$withdraw_ms" ]; then
    report A3 FAIL "BFD down after $bfd_ms ms, leaf3 still had $held paths after $A3_WITHDRAW_SAMPLES samples, 100 ms apart, from before the silence"
  elif [ -z "$back_ms" ]; then
    report A3 FAIL "BFD down after $bfd_ms ms, leaf3 at 1 path after $withdraw_ms ms, not back to 2 paths $((A3_RESTORE_WAIT_MS / 1000)) s after the restore"
  elif [ "$bfd_ms" -gt "$A3_BFD_BOUND_MS" ] || [ "$withdraw_ms" -gt "$A3_WITHDRAW_BOUND_MS" ]; then
    report A3 FAIL "BFD down after $bfd_ms ms (bound $A3_BFD_BOUND_MS ms), leaf3 at 1 path after $withdraw_ms ms (bound $A3_WITHDRAW_BOUND_MS ms)"
  else
    report A3 PASS "BFD down after $bfd_ms ms, leaf3 at 1 path after $withdraw_ms ms, 2 paths again $((back_ms / 1000)) s after the restore"
  fi
}

# --- A4: the node keeps its bait inside, accepts defaults only ---

a4() {
  local out n held v4 v6
  # A bait proves a filter only while it is in place: node1 holds
  # 203.0.113.0/24, and leaf1's deployment VRF holds 198.18.0.0/15.
  held="$(prefix_paths node1 fabric ipv4 203.0.113.0/24)" || { report A4 FAIL "$held"; return; }
  if [ "$held" = 0 ]; then
    report A4 FAIL "the bait 203.0.113.0/24 is not in node1's BGP table"
    return
  fi
  held="$(prefix_paths leaf1 cobaltcore ipv4 198.18.0.0/15)" || { report A4 FAIL "$held"; return; }
  if [ "$held" = 0 ]; then
    report A4 FAIL "the bait 198.18.0.0/15 is not in leaf1's cobaltcore BGP table"
    return
  fi
  out="$(vty node1 'show bgp vrf fabric ipv4 unicast neighbors eth1 advertised-routes json')" || { report A4 FAIL "$out"; return; }
  if [ "$(jq '.advertisedRoutes // {} | has("203.0.113.0/24")' <<<"$out" 2>/dev/null)" != false ]; then
    report A4 FAIL "node1 advertises 203.0.113.0/24 on eth1, or its answer is no JSON"
    return
  fi
  for n in leaf1 leaf2 leaf3; do
    held="$(prefix_paths "$n" cobaltcore ipv4 203.0.113.0/24)" || { report A4 FAIL "$held"; return; }
    if [ "$held" != 0 ]; then
      report A4 FAIL "$n holds 203.0.113.0/24 with $held paths"
      return
    fi
  done
  held="$(prefix_paths node1 fabric ipv4 198.18.0.0/15)" || { report A4 FAIL "$held"; return; }
  if [ "$held" != 0 ]; then
    report A4 FAIL "node1 accepted 198.18.0.0/15 with $held paths"
    return
  fi
  v4="$(prefix_paths node1 fabric ipv4 0.0.0.0/0)" || { report A4 FAIL "$v4"; return; }
  v6="$(prefix_paths node1 fabric ipv6 ::/0)" || { report A4 FAIL "$v6"; return; }
  if [ "$v4" != 2 ] || [ "$v6" != 2 ]; then
    report A4 FAIL "node1 has $v4 paths for 0.0.0.0/0 and $v6 for ::/0"
    return
  fi
  report A4 PASS "203.0.113.0/24 held by node1, absent from its advertised routes on eth1 and from leaf1, leaf2 and leaf3; 198.18.0.0/15 held by leaf1, absent on node1; 2 paths for 0.0.0.0/0 and for ::/0 on node1"
}

# --- A5: MTU 9000 on the node links, 9216 on the fabric links, a 9000-byte
# packet crosses the VXLAN fabric whole ---

A5_ENDS="node1:eth1=9000 node1:eth2=9000 leaf1:eth1=9000 leaf2:eth1=9000
leaf1:eth2=9216 leaf2:eth2=9216 leaf3:eth1=9216 spine1:eth1=9216 spine1:eth2=9216 spine1:eth3=9216"

# Runs in node1. Ip6FragCreates counts the fragments node1 builds itself:
# the 9000-byte probe must leave whole and be answered, the 9001-byte one
# must be cut at the source. Their payloads, 8952 and 8953 bytes, are those
# sizes minus the 40-byte IPv6 header and the 8-byte ICMPv6 header; another
# node MTU needs both recomputed. BusyBox ping has no -M do, and IPv6 routers
# never fragment, so this is how the 9000-byte ceiling shows. -I fabric binds
# the socket to the VRF device, so the lookup uses the fabric table and the
# source is the fabric loopback.
A5_PROBE='
frag() { awk "\$1 == \"Ip6FragCreates\" {print \$2}" /proc/net/snmp6; }
ip -6 route flush cache
a=$(frag)
ok=0
ping -6 -c 1 -W 3 -s 8952 -I fabric 2001:db8:0:ff::3 >/dev/null 2>&1 && ok=1
b=$(frag)
ping -6 -c 1 -W 3 -s 8953 -I fabric 2001:db8:0:ff::3 >/dev/null 2>&1 || true
c=$(frag)
echo "$ok $((b - a)) $((c - b))"'

a5() {
  local n out end box ifname want got bad="" ok frag_whole frag_over node_ends=0 fabric_ends=0
  for n in $NODES; do
    out="$(docker exec "clab-$LAB-$n" ip -j link show)" || { report A5 FAIL "ip link on $n failed"; return; }
    for end in $A5_ENDS; do
      box="${end%%:*}"
      [ "$box" = "$n" ] || continue
      ifname="${end#*:}"
      want="${ifname#*=}"
      ifname="${ifname%%=*}"
      if [ "$want" = 9000 ]; then node_ends=$((node_ends + 1)); else fabric_ends=$((fabric_ends + 1)); fi
      got="$(jq -r --arg i "$ifname" '[.[] | select(.ifname == $i) | .mtu] | first // "absent"' <<<"$out" 2>/dev/null)" || got=unreadable
      [ "$got" = "$want" ] || bad="$bad $box:$ifname=$got (want $want)"
    done
  done
  if [ -n "$bad" ]; then
    report A5 FAIL "link ends at the wrong MTU:$bad"
    return
  fi
  out="$(docker exec "clab-$LAB-node1" sh -c "$A5_PROBE")" || { report A5 FAIL "the probe on node1 failed: $out"; return; }
  read -r ok frag_whole frag_over <<<"$out"
  if ! [[ "${frag_whole:-}" =~ ^[0-9]+$ && "${frag_over:-}" =~ ^[0-9]+$ ]]; then
    report A5 FAIL "the probe on node1 printed '$out'"
  elif [ "$ok" != 1 ]; then
    report A5 FAIL "$((node_ends + fabric_ends)) link ends at their MTU, but the 9000-byte probe from node1 to 2001:db8:0:ff::3 got no reply"
  elif [ "$frag_whole" != 0 ]; then
    report A5 FAIL "the 9000-byte probe was answered but node1 cut it into $frag_whole fragments"
  elif [ "$frag_over" = 0 ]; then
    report A5 FAIL "the 9001-byte probe left node1 without being fragmented"
  else
    report A5 PASS "$node_ends node link ends at MTU 9000, $fabric_ends fabric link ends at 9216; the 9000-byte probe to 2001:db8:0:ff::3 answered with 0 fragments; the 9001-byte probe cut into $frag_over fragments at node1"
  fi
}

# --- A6: host routes only toward the fabric, the leaf bounds the session ---

a6() {
  local out bad af suffix
  for af in ipv4:/32 ipv6:/128; do
    suffix="${af#*:}"
    af="${af%%:*}"
    out="$(vty node1 "show bgp vrf fabric $af unicast neighbors eth1 advertised-routes json")" || { report A6 FAIL "$out"; return; }
    # "ok" is the one passing answer: non-JSON gives jq nothing to print,
    # and a blank bad must not read as a pass.
    bad="$(jq -r --arg s "$suffix" '.advertisedRoutes // {} | keys
      | if length == 0 then "nothing"
        else (map(select(endswith($s) | not)) | if length == 0 then "ok" else join(" ") end) end' <<<"$out" 2>/dev/null)" || bad=""
    if [ "$bad" != ok ]; then
      report A6 FAIL "node1 advertises on eth1 in $af: ${bad:-no JSON from vtysh}"
      return
    fi
  done
  out="$(vty leaf1 'show running-config')" || { report A6 FAIL "$out"; return; }
  if ! grep -q 'neighbor nodes maximum-prefix 8192' <<<"$out"; then
    report A6 FAIL "leaf1 running config has no neighbor nodes maximum-prefix 8192"
    return
  fi
  report A6 PASS "node1 advertises only /32 (IPv4) and /128 (IPv6) keys on eth1; leaf1 runs neighbor nodes maximum-prefix 8192"
}

# --- A7: the fabric stays in its VRF on the node and on the leaf ---

# via($devs) keeps the routes of a route list that leave through one of the
# devices in $devs, by their dev or by a multipath next hop.
ROUTES_VIA='def via($devs): [.[] | select([.dev, (.nexthops // [])[].dev] | any(. as $d | $devs | any(. == $d)))];'

a7() {
  local default main main6 fabric fabric6 bad gw out
  default="$(docker exec "clab-$LAB-node1" ip -j route show default)" || { report A7 FAIL "ip route on node1 failed"; return; }
  out="$(jq -r '[.[] | .dev // "multipath"] | join(" ")' <<<"$default" 2>/dev/null)" || out="no JSON"
  if [ "$out" != eth0 ]; then
    report A7 FAIL "node1's main table holds default routes via '$out', not eth0 alone"
    return
  fi
  out="$(docker exec "clab-$LAB-node1" ip -j route show vrf fabric default)" || { report A7 FAIL "ip route on node1 failed"; return; }
  if [ "$(jq -r 'if length == 1 then [.[0].nexthops // [] | .[].dev] | sort | join(" ") else "\(length) routes" end' <<<"$out" 2>/dev/null)" != "eth1 eth2" ]; then
    report A7 FAIL "node1's fabric VRF default is not one route over eth1 and eth2: $(jq -c . <<<"$out" 2>/dev/null)"
    return
  fi
  main="$(docker exec "clab-$LAB-node1" ip -j route show)" &&
    main6="$(docker exec "clab-$LAB-node1" ip -j -6 route show)" &&
    fabric="$(docker exec "clab-$LAB-node1" ip -j route show vrf fabric)" &&
    fabric6="$(docker exec "clab-$LAB-node1" ip -j -6 route show vrf fabric)" ||
    { report A7 FAIL "ip route on node1 failed"; return; }
  bad="$(jq -rn --argjson v4 "$main" --argjson v6 "$main6" "$ROUTES_VIA"'
    ($v4 + $v6) | via(["eth1", "eth2"]) + [.[] | select(.dst == "198.51.100.0/24" or .dst == "198.18.0.0/15")]
    | map("\(.dst) dev \(.dev // "multipath")") | unique | join(", ")' 2>&1)" || bad="no JSON: $bad"
  if [ -n "$bad" ]; then
    report A7 FAIL "node1's main table holds fabric routes: $bad"
    return
  fi
  bad="$(jq -rn --argjson v4 "$fabric" --argjson v6 "$fabric6" "$ROUTES_VIA"'
    ($v4 + $v6) | via(["eth0"]) | map("\(.dst) dev \(.dev // "multipath")") | unique | join(", ")' 2>&1)" || bad="no JSON: $bad"
  if [ -n "$bad" ]; then
    report A7 FAIL "node1's fabric VRF holds node-network routes: $bad"
    return
  fi
  gw="$(jq -r '.[0].gateway // empty' <<<"$default")"
  if [ -z "$gw" ] || ! docker exec "clab-$LAB-node1" ping -c 1 -W 3 -I eth0 "$gw" >/dev/null 2>&1; then
    report A7 FAIL "node1 cannot reach its node-network gateway '${gw}' over eth0"
    return
  fi
  out="$(vty leaf1 'show bgp summary json')" || { report A7 FAIL "$out"; return; }
  if [ "$(jq '[.[] | objects | .peers // {} | has("eth1")] | any' <<<"$out" 2>/dev/null)" != false ]; then
    report A7 FAIL "leaf1's default VRF has a peer on eth1, or its answer is no JSON"
    return
  fi
  out="$(vty leaf1 'show bgp vrf cobaltcore summary json')" || { report A7 FAIL "$out"; return; }
  if [ "$(jq '.ipv4Unicast.peers // {} | has("eth1")' <<<"$out" 2>/dev/null)" != true ]; then
    report A7 FAIL "leaf1's cobaltcore VRF has no peer on eth1"
    return
  fi
  report A7 PASS "node1's main table: one default via eth0 (gateway $gw answers), no route via eth1 or eth2 or to a bait; fabric VRF: one default over eth1 and eth2, no route via eth0; leaf1 peers on eth1 in cobaltcore only"
}

run_record() {
  local commit tree clab_version frr_image
  commit="$(git -C "$LAB_DIR" rev-parse --short HEAD 2>/dev/null)" || commit="no git checkout"
  if [ -n "$(git -C "$LAB_DIR" status --porcelain -- . 2>/dev/null)" ]; then
    commit="$commit with uncommitted changes under tests/lab/bgp-fabric"
  fi
  # The lab's tree hash outlives a rebase of the commit that carries it.
  tree="$(git -C "$LAB_DIR" rev-parse --short HEAD:./ 2>/dev/null)" || tree="unknown"
  clab_version="$(docker run --rm "$CLAB_IMAGE" containerlab version | awk '$1 == "version:" {print $2; exit}')" ||
    clab_version="unknown (containerlab version failed)"
  frr_image="$(docker image inspect --format '{{index .RepoDigests 0}}' \
    "$(docker inspect --format '{{.Image}}' "clab-$LAB-node1")")" ||
    frr_image="unknown (docker image inspect failed)"
  cat <<EOF

#### containerlab

Run on $(date -u +%F) from commit $commit (lab tree $tree) with containerlab
$clab_version and \`$frr_image\`.

| Assertion | Result | Measured |
| --- | --- | --- |
$RECORD_ROWS
EOF
}

check() {
  require_lab
  a1
  a2
  a3
  a4
  a5
  a6
  a7
  run_record
  [ "$FAILED" = 0 ]
}

# teardown is the EXIT trap of all, so the lab goes however the run ends: a
# failed deploy, a node that is not running, an interrupt. KEEP_LAB=true
# keeps it for inspection.
teardown() {
  local rc=$?
  if [ "$KEEP_LAB" = true ]; then
    echo "KEEP_LAB=true: the lab stays up; run '${0##*/} down' to remove it"
  elif ! lab_down; then
    echo "run.sh: the teardown failed; run '${0##*/} down'" >&2
    rc=1
  fi
  exit "$rc"
}

main() {
  local cmd="${1:-all}" rc=0
  case "$cmd" in
    up | check | down | all) ;;
    *) usage ;;
  esac
  [ "$#" -le 1 ] || usage
  preconditions
  # check runs as the left side of ||, as in all, so every assertion reports
  # its own verdict and a failed command inside one does not end the run.
  case "$cmd" in
    up) lab_up ;;
    down) lab_down ;;
    check) check || exit 1 ;;
    all)
      trap teardown EXIT
      lab_up
      check || rc=$?
      exit "$rc"
      ;;
  esac
}

main "$@"
