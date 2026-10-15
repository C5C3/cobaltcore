#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Deploys, checks and destroys the containerlab lab of the BGP fabric contract
# (docs/reference/networking/bgp-fabric-contract.md). containerlab runs from
# its pinned container image, so the host needs Docker, a user in the docker
# group and jq, and no installed containerlab. check runs the assertions A1 to
# A6 of the page's lab plan, prints one PASS, FAIL or SKIP line per assertion
# and the run record for the page, and exits 1 when an assertion fails.
#
# Usage: tests/lab/bgp-fabric/run.sh [up|check|down|all]
#   KEEP_LAB=true  all skips the teardown, so a failed lab can be inspected
#   CLAB_IMAGE     the containerlab image (default: the pin below)

set -euo pipefail

# Builtins only up to the docker check, so a host without docker on PATH gets
# that message and nothing else.
case "${BASH_SOURCE[0]}" in
  */*) LAB_DIR="$(cd "${BASH_SOURCE[0]%/*}" && pwd)" ;;
  *) LAB_DIR="$PWD" ;;
esac
LAB=bgp-fabric
TOPOLOGY=bgp-fabric.clab.yml
NODES="node1 leaf1 leaf2 spine1"
CLAB_IMAGE_DEFAULT="ghcr.io/srl-labs/clab:0.81.1@sha256:9524cfe7e61af502e16fde2e8afcf2256e8eb4672ed5c5417a2f67fdb22c34cf"
CLAB_IMAGE="${CLAB_IMAGE:-$CLAB_IMAGE_DEFAULT}"
KEEP_LAB="${KEEP_LAB:-false}"

# The bounds of the page's lab plan, and the samples the in-box waits take.
A1_WAIT_MS=60000          # A1: sessions and BFD up after 'up'
A3_BFD_BOUND_MS=1000      # C5: BFD down after the silence
A3_WITHDRAW_BOUND_MS=2000 # C5: spine1 down to one path after the silence
A3_RESTORE_WAIT_MS=60000  # A3: spine1 back at two paths after the restore
A3_READY_WAIT_MS=10000    # A3: spine1's withdraw sampler at two paths before the silence
A3_BFD_SAMPLES=200        # 20 ms apart
A3_WITHDRAW_SAMPLES=140   # 100 ms apart from before the silence: 4 s for BFD, then 10 s

FAILED=0
RECORD_ROWS=""

usage() {
  cat >&2 <<EOF
usage: ${0##*/} [up|check|down|all]
  up     deploy the lab (containerlab deploy --reconfigure)
  check  run the assertions A1 to A6 against the deployed lab
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
  [ -f "$LAB_DIR/daemons" ] || die "missing daemons under $LAB_DIR"
  local n
  for n in $NODES; do
    [ -f "$LAB_DIR/$n/frr.conf" ] || die "missing $n/frr.conf under $LAB_DIR"
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

lab_up() { clab deploy -t "$TOPOLOGY" --reconfigure; }
lab_down() { clab destroy -t "$TOPOLOGY" --cleanup; }

require_lab() {
  local n
  for n in $NODES; do
    [ "$(docker inspect --format '{{.State.Running}}' "clab-$LAB-$n" 2>/dev/null)" = true ] ||
      die "lab container clab-$LAB-$n is not running; run '${0##*/} up' first"
  done
}

# vty <node> <command> prints the output of one vtysh command. The image ships
# no vtysh.conf and vtysh says so on stderr on every call; that notice is what
# 2>/dev/null drops. vtysh prints its own errors on stdout and exits 1.
vty() {
  local out
  if ! out="$(docker exec "clab-$LAB-$1" vtysh -c "$2" 2>/dev/null)"; then
    echo "vtysh on $1 failed: $2: $out" >&2
    return 1
  fi
  printf '%s\n' "$out"
}

prefix_paths() { # <node> <family> <prefix>
  vty "$1" "show bgp $2 unicast $3 json" | jq '.paths // [] | length'
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

# --- A1: every session Established, BFD up on the node-facing sessions ---

a1_unmet() { # prints the first unmet condition, nothing when all hold
  local n af p peers bfd out
  for n in leaf1 leaf2 spine1; do
    # spine1 reads the leaf-spine sessions; each leaf adds its node peer and
    # the BFD session on it.
    case "$n" in
      spine1) peers="eth1 eth2" bfd=false ;;
      *) peers=eth1 bfd=true ;;
    esac
    out="$(vty "$n" 'show bgp summary json')" || { echo "vtysh on $n failed"; return; }
    for p in $peers; do
      for af in ipv4Unicast ipv6Unicast; do
        [ "$(jq -r --arg af "$af" --arg p "$p" '.[$af].peers[$p].state // "absent"' <<<"$out")" = Established ] ||
          { echo "$n $p $af not Established"; return; }
      done
    done
    [ "$bfd" = true ] || continue
    out="$(vty "$n" 'show bfd peers json')" || { echo "vtysh on $n failed"; return; }
    [ "$(jq -r '[.[] | select(.interface == "eth1") | .status] | first // "absent"' <<<"$out")" = up ] ||
      { echo "$n BFD peer on eth1 not up"; return; }
  done
}

a1() {
  local start unmet
  start="$(now_ms)"
  # vtysh fails while the daemons are still starting; a1_unmet reports that
  # as an unmet condition, and A1_WAIT_MS ends the wait.
  while :; do
    unmet="$(a1_unmet 2>/dev/null)" || unmet="the session check exited with an error"
    [ -n "$unmet" ] || break
    if [ $(($(now_ms) - start)) -ge "$A1_WAIT_MS" ]; then
      report A1 FAIL "after $((A1_WAIT_MS / 1000)) s: $unmet"
      return
    fi
    sleep 2
  done
  report A1 PASS "leaf1, leaf2 and spine1 sessions Established in both families, both node BFD sessions up, after $((($(now_ms) - start) / 1000)) s"
}

# --- A2: the spine holds node1's host routes over both leaves ---

a2() {
  local v4 v6 fib want check
  want='["4200001001 4200000001","4200001002 4200000001"]'
  check='(.paths | length) == 2
    and ([.paths[].aspath.string] | sort) == $want
    and ([.paths[] | select(.bestpath.overall == true)] | length) == 1
    and ([.paths[] | select(.bestpath.overall != true) | select(.multipath == true)] | length) == 1'
  v4="$(vty spine1 'show bgp ipv4 unicast 192.0.2.1/32 json')" || { report A2 FAIL "vtysh on spine1 failed"; return; }
  v6="$(vty spine1 'show bgp ipv6 unicast 2001:db8::1/128 json')" || { report A2 FAIL "vtysh on spine1 failed"; return; }
  fib="$(docker exec "clab-$LAB-spine1" ip -j route show 192.0.2.1 | jq '.[0].nexthops // [] | length')" ||
    { report A2 FAIL "ip route on spine1 failed"; return; }
  if [ "$(jq --argjson want "$want" "$check" <<<"$v4")" != true ]; then
    report A2 FAIL "192.0.2.1/32 on spine1: $(jq -c '[.paths // [] | .[] | {as: .aspath.string, best: .bestpath.overall, multipath}]' <<<"$v4")"
  elif [ "$(jq --argjson want "$want" "$check" <<<"$v6")" != true ]; then
    report A2 FAIL "2001:db8::1/128 on spine1: $(jq -c '[.paths // [] | .[] | {as: .aspath.string, best: .bestpath.overall, multipath}]' <<<"$v6")"
  elif [ "$fib" != 2 ]; then
    report A2 FAIL "192.0.2.1 has $fib next hops in the spine1 kernel"
  else
    report A2 PASS "2 paths for 192.0.2.1/32 and 2001:db8::1/128 on spine1, AS paths 4200001001 4200000001 and 4200001002 4200000001, one bestpath and one multipath each, 2 kernel next hops"
  fi
}

# --- A3: BFD detects a silent link, the spine withdraws the path ---

# Runs in leaf1 with the sample bound as $1. netem drops what leaf1 sends to
# node1, the ingress filter what node1 sends to leaf1, so the link is silent
# both ways with its carrier up. Prints the uptime before the first tc call
# and when BFD reports the node peer down, sampled every 20 ms. The exit code
# tells a3 what happened: 97 the kernel has no netem (nothing to restore), 96
# it lacks a module of the ingress drop (netem is in place), 95 any other tc
# failure, 98 BFD still up after $1 samples.
A3_SILENCE='
command -v tc >/dev/null 2>&1 || { echo "tc is not in the image"; exit 95; }
read -r s _ </proc/uptime
err=$(tc qdisc add dev eth1 root netem loss 100% 2>&1) || {
  echo "$err"
  case "$err" in *"kind is unknown"* | *"No such file or directory"*) exit 97 ;; esac
  exit 95
}
err=$( (tc qdisc add dev eth1 ingress && tc filter add dev eth1 parent ffff: matchall action drop) 2>&1) || {
  echo "$err"
  case "$err" in
    *"kind is unknown"* | *"classifier not found"* | *"Failed to load"* | *"No such file or directory"*) exit 96 ;;
  esac
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

# Runs in spine1 with the sample bound as $1. Exits 1 at once unless its first
# sample shows node1's /32 at two paths, then prints ready, samples every
# 100 ms and prints the uptime once spine1 holds one path; exits 1 when it does
# not within $1 samples. a3 silences leaf1 only after the ready line, so
# spine1 was at two paths before the silence and no docker exec start-up lands
# in the measurement.
A3_WITHDRAW='
vtysh -c "show bgp ipv4 unicast 192.0.2.1/32 json" 2>/dev/null | grep -q "\"pathCount\":2," || exit 1
echo ready
i=0
until vtysh -c "show bgp ipv4 unicast 192.0.2.1/32 json" 2>/dev/null | grep -q "\"pathCount\":1,"; do
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
  docker exec "clab-$LAB-spine1" sh -c "$A3_WITHDRAW" a3 "$A3_WITHDRAW_SAMPLES" >"$wfile" &
  wpid=$!
  start="$(now_ms)"
  until [ "$(head -n 1 "$wfile")" = ready ]; do
    if ! kill -0 "$wpid" 2>/dev/null || [ $(($(now_ms) - start)) -ge "$A3_READY_WAIT_MS" ]; then
      kill "$wpid" 2>/dev/null || true
      wait "$wpid" 2>/dev/null || true
      rm -f "$wfile"
      trap - INT TERM
      paths="$(prefix_paths spine1 ipv4 192.0.2.1/32)" || paths=error
      report A3 FAIL "spine1's withdraw sampler saw no 2 paths for 192.0.2.1/32 before the silence; spine1 holds $paths"
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
      held="$(prefix_paths spine1 ipv4 192.0.2.1/32)" || held=error
    fi
  else
    # Killing the docker exec leaves the sampling in spine1 to end at its bound.
    kill "$wpid" 2>/dev/null || true
    wait "$wpid" 2>/dev/null || true
  fi
  rm -f "$wfile"
  a3_restore
  trap - INT TERM
  case "$rc" in
    0) ;;
    97)
      report A3 SKIP "tc qdisc add dev eth1 root netem loss 100% failed on leaf1 ($out): host kernel without sch_netem"
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
    paths="$(prefix_paths spine1 ipv4 192.0.2.1/32)" || paths=error
    if [ "$paths" = 2 ]; then
      back_ms=$(($(now_ms) - start))
      break
    fi
    sleep 1
  done

  if [ -z "$withdraw_ms" ]; then
    report A3 FAIL "BFD down after $bfd_ms ms, spine1 still had $held paths after $A3_WITHDRAW_SAMPLES samples, 100 ms apart, from before the silence"
  elif [ -z "$back_ms" ]; then
    report A3 FAIL "BFD down after $bfd_ms ms, spine1 at 1 path after $withdraw_ms ms, not back to 2 paths $((A3_RESTORE_WAIT_MS / 1000)) s after the restore"
  elif [ "$bfd_ms" -gt "$A3_BFD_BOUND_MS" ] || [ "$withdraw_ms" -gt "$A3_WITHDRAW_BOUND_MS" ]; then
    report A3 FAIL "BFD down after $bfd_ms ms (bound $A3_BFD_BOUND_MS ms), spine1 at 1 path after $withdraw_ms ms (bound $A3_WITHDRAW_BOUND_MS ms)"
  else
    report A3 PASS "BFD down after $bfd_ms ms, spine1 at 1 path after $withdraw_ms ms, 2 paths again $((back_ms / 1000)) s after the restore"
  fi
}

# --- A4: the node keeps its bait inside, accepts defaults only ---

a4() {
  local out n held v4 v6
  # A bait proves a filter only while it is in place: node1 holds
  # 203.0.113.0/24, and leaf1 offers 198.18.0.0/15.
  held="$(prefix_paths node1 ipv4 203.0.113.0/24)" || { report A4 FAIL "vtysh on node1 failed"; return; }
  if ! [[ "$held" =~ ^[1-9] ]]; then
    report A4 FAIL "the bait 203.0.113.0/24 is not in node1's BGP table"
    return
  fi
  out="$(vty leaf1 'show bgp ipv4 unicast neighbors eth1 advertised-routes json')" || { report A4 FAIL "vtysh on leaf1 failed"; return; }
  if [ "$(jq '.advertisedRoutes // {} | has("198.18.0.0/15")' <<<"$out")" != true ]; then
    report A4 FAIL "leaf1 does not offer the bait 198.18.0.0/15 to node1"
    return
  fi
  out="$(vty node1 'show bgp ipv4 unicast neighbors eth1 advertised-routes json')" || { report A4 FAIL "vtysh on node1 failed"; return; }
  if [ "$(jq '.advertisedRoutes // {} | has("203.0.113.0/24")' <<<"$out")" != false ]; then
    report A4 FAIL "node1 advertises 203.0.113.0/24 on eth1"
    return
  fi
  for n in leaf1 leaf2 spine1; do
    held="$(prefix_paths "$n" ipv4 203.0.113.0/24)" || { report A4 FAIL "vtysh on $n failed"; return; }
    if [ "$held" != 0 ]; then
      report A4 FAIL "$n holds 203.0.113.0/24 with $held paths"
      return
    fi
  done
  held="$(prefix_paths node1 ipv4 198.18.0.0/15)" || { report A4 FAIL "vtysh on node1 failed"; return; }
  if [ "$held" != 0 ]; then
    report A4 FAIL "node1 accepted 198.18.0.0/15 with $held paths"
    return
  fi
  v4="$(prefix_paths node1 ipv4 0.0.0.0/0)" || { report A4 FAIL "vtysh on node1 failed"; return; }
  v6="$(prefix_paths node1 ipv6 ::/0)" || { report A4 FAIL "vtysh on node1 failed"; return; }
  if [ "$v4" != 2 ] || [ "$v6" != 2 ]; then
    report A4 FAIL "node1 has $v4 paths for 0.0.0.0/0 and $v6 for ::/0"
    return
  fi
  report A4 PASS "203.0.113.0/24 held by node1, not advertised and absent on leaf1, leaf2 and spine1; 198.18.0.0/15 offered by leaf1, absent on node1; 2 paths for 0.0.0.0/0 and for ::/0 on node1"
}

# --- A5: MTU 9000 on every link end, a 9000-byte packet crosses whole ---

# Runs in node1. Ip6FragCreates counts the fragments node1 builds itself:
# the 9000-byte probe must leave whole and be answered, the 9001-byte one
# must be cut at the source. BusyBox ping has no -M do, and IPv6 routers
# never fragment, so this is how the 9000-byte ceiling shows.
A5_PROBE='
frag() { awk "\$1 == \"Ip6FragCreates\" {print \$2}" /proc/net/snmp6; }
ip -6 route flush cache
a=$(frag)
ok=0
ping -6 -c 1 -W 3 -s 8952 -I 2001:db8::1 2001:db8::201 >/dev/null 2>&1 && ok=1
b=$(frag)
ping -6 -c 1 -W 3 -s 8953 -I 2001:db8::1 2001:db8::201 >/dev/null 2>&1 || true
c=$(frag)
echo "$ok $((b - a)) $((c - b))"'

a5() {
  local n out ends="" e count=0 bad="" ok frag_whole frag_over
  for n in $NODES; do
    out="$(docker exec "clab-$LAB-$n" ip -j link show)" || { report A5 FAIL "ip link on $n failed"; return; }
    ends="$ends $(jq -r --arg n "$n" '[.[] | select(.ifname == "eth1" or .ifname == "eth2") | "\($n):\(.ifname)=\(.mtu)"] | join(" ")' <<<"$out")"
  done
  for e in $ends; do
    count=$((count + 1))
    [ "${e##*=}" = 9000 ] || bad="$bad $e"
  done
  if [ "$count" != 8 ] || [ -n "$bad" ]; then
    report A5 FAIL "link ends not all at MTU 9000:$ends"
    return
  fi
  out="$(docker exec "clab-$LAB-node1" sh -c "$A5_PROBE")" || { report A5 FAIL "the probe on node1 failed: $out"; return; }
  read -r ok frag_whole frag_over <<<"$out"
  if ! [[ "$frag_whole" =~ ^[0-9]+$ && "$frag_over" =~ ^[0-9]+$ ]]; then
    report A5 FAIL "the probe on node1 printed '$out'"
  elif [ "$ok" != 1 ]; then
    report A5 FAIL "8 link ends at MTU 9000, but the 9000-byte probe from 2001:db8::1 to 2001:db8::201 got no reply"
  elif [ "$frag_whole" != 0 ]; then
    report A5 FAIL "the 9000-byte probe was answered but node1 cut it into $frag_whole fragments"
  elif [ "$frag_over" = 0 ]; then
    report A5 FAIL "the 9001-byte probe left node1 without being fragmented"
  else
    report A5 PASS "8 link ends at MTU 9000; the 9000-byte probe answered with 0 fragments; the 9001-byte probe cut into $frag_over fragments at node1"
  fi
}

# --- A6: host routes only toward the fabric, the leaf bounds the session ---

a6() {
  local out bad af suffix
  for af in ipv4:/32 ipv6:/128; do
    suffix="${af#*:}"
    af="${af%%:*}"
    out="$(vty node1 "show bgp $af unicast neighbors eth1 advertised-routes json")" || { report A6 FAIL "vtysh on node1 failed"; return; }
    # "ok" is the one passing answer: empty output and non-JSON give jq
    # nothing to print, and a blank bad must not read as a pass.
    bad="$(jq -r --arg s "$suffix" '.advertisedRoutes // {} | keys
      | if length == 0 then "nothing"
        else (map(select(endswith($s) | not)) | if length == 0 then "ok" else join(" ") end) end' <<<"$out")" || bad=""
    if [ "$bad" != ok ]; then
      report A6 FAIL "node1 advertises on eth1 in $af: ${bad:-no JSON from vtysh}"
      return
    fi
  done
  out="$(vty leaf1 'show running-config')" || { report A6 FAIL "vtysh on leaf1 failed"; return; }
  if ! grep -q 'neighbor nodes maximum-prefix 8192' <<<"$out"; then
    report A6 FAIL "leaf1 running config has no neighbor nodes maximum-prefix 8192"
    return
  fi
  report A6 PASS "node1 advertises only /32 (IPv4) and /128 (IPv6) keys on eth1; leaf1 runs neighbor nodes maximum-prefix 8192"
}

run_record() {
  local commit clab_version frr_image
  commit="$(git -C "$LAB_DIR" rev-parse --short HEAD 2>/dev/null)" || commit="no git checkout"
  if [ -n "$(git -C "$LAB_DIR" status --porcelain -- . 2>/dev/null)" ]; then
    commit="$commit with uncommitted changes under tests/lab/bgp-fabric"
  fi
  clab_version="$(docker run --rm "$CLAB_IMAGE" containerlab version | awk '$1 == "version:" {print $2; exit}')" ||
    clab_version="unknown (containerlab version failed)"
  frr_image="$(docker image inspect --format '{{index .RepoDigests 0}}' \
    "$(docker inspect --format '{{.Image}}' "clab-$LAB-node1")")" ||
    frr_image="unknown (docker image inspect failed)"
  cat <<EOF

#### containerlab

Run on $(date -u +%F) from commit $commit with containerlab $clab_version and
\`$frr_image\`.

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
  case "$cmd" in
    up) lab_up ;;
    down) lab_down ;;
    check) check ;;
    all)
      trap teardown EXIT
      lab_up
      check || rc=$?
      exit "$rc"
      ;;
  esac
}

main "$@"
