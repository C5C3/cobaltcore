#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the containerlab lab of the BGP fabric contract under
# tests/lab/bgp-fabric/ keeps the shape the contract page and run.sh rely on,
# without deploying it:
#   - every file carries the SPDX pair
#   - every bind source in the topology exists: one daemons file for every
#     box, an frr.conf and a setup.sh per box
#   - the FRR image is pinned by digest on exactly one line, the line the
#     Renovate customManager reads
#   - the two node links carry mtu: 9000 and the three fabric links
#     mtu: 9216; the check names the link that does not, and a copy with one
#     mtu removed proves it does
#   - node1 states the node side of the contract (its ASN in the fabric VRF,
#     BFD 300/300/3, maximum-paths 2) and its setup.sh builds the fabric VRF
#     with the bait and keeps eth0's defaults
#   - node1 binds its four prefix-lists to the leaf sessions, accepts the two
#     defaults alone and announces /32 and /128 only; leaf1 and leaf2 bind
#     their inbound lists to the node session and accept exactly what node1
#     announces
#   - every leaf builds the L3VNI 20001 at MTU 9166; leaf1 and leaf2 map the
#     deployment VRF, bound the node session and offer the bait; leaf3
#     installs both paths and holds the A5 target; spine1 reflects and runs
#     no VNI
#   - every setup.sh waits for its links and ends by exec'ing FRR's start
#   - run.sh is strict and pins the containerlab image by digest
#
# No containerlab, Docker or network access is needed; run.sh is the lab's
# own proof, run by hand.
#
# Usage: bash tests/unit/lab/bgp_fabric_lab_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

LAB_DIR="$PROJECT_ROOT/tests/lab/bgp-fabric"
TOPOLOGY="$LAB_DIR/bgp-fabric.clab.yml"
RUNNER="$LAB_DIR/run.sh"
NODES="node1 leaf1 leaf2 leaf3 spine1"

if [[ ! -f "$TOPOLOGY" ]]; then
  echo "FAIL: $TOPOLOGY does not exist"
  exit 1
fi

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# links_off_mtu <topology>: prints every link whose mtu: is not the one its
# kind needs, a node link (one end on node1) 9000, a fabric link 9216, as
# '<endpoints> has mtu <mtu>, needs <mtu>'.
links_off_mtu() {
  awk '
    function flush() {
      if (link == "") return
      want = (link ~ /"node1:/) ? 9000 : 9216
      if (mtu != want) print link " has mtu " (mtu == "" ? "none" : mtu) ", needs " want
    }
    /^  links:/ { in_links = 1; next }
    in_links && /^  [^ ]/ { in_links = 0 }
    !in_links { next }
    /^    - endpoints:/ {
      flush()
      link = $0; sub(/^    - endpoints: */, "", link); mtu = ""; next
    }
    /^      mtu: / { mtu = $2 }
    END { flush() }
  ' "$1"
}

# permits <frr.conf> <list>: the permit clauses of prefix-list <list>, one
# per line and sorted, with the list name and the sequence number stripped.
permits() {
  awk -v l="$2" '$0 ~ ("prefix-list " l " seq ") && / permit / { sub(/.* permit /, ""); print }' "$1" | sort
}

# --- Test 1: every lab file carries the SPDX pair ---
test_every_lab_file_carries_the_spdx_pair() {
  echo "Test: every file under tests/lab/bgp-fabric/ carries the SPDX pair"

  local f rel count=0
  # clab-bgp-fabric/ is the working directory containerlab writes during a
  # run (gitignored, root-owned); it is not part of the lab.
  while IFS= read -r f; do
    rel="${f#"$PROJECT_ROOT"/}"
    count=$((count + 1))
    assert_file_contains_fixed "$rel carries SPDX-FileCopyrightText" "$f" \
      "SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company"
    assert_file_contains_fixed "$rel carries SPDX-License-Identifier" "$f" \
      "SPDX-License-Identifier: Apache-2.0"
  done < <(find "$LAB_DIR" -path "$LAB_DIR/clab-bgp-fabric" -prune -o -type f -print | sort)

  # The topology, run.sh, the daemons file, an frr.conf and a setup.sh per box.
  assert_eq "the lab has its thirteen files" "13" "$count"
}

# --- Test 2: every bind source exists ---
test_every_bind_source_exists() {
  echo "Test: every bind source in the topology exists beside it"

  local src count=0
  while IFS= read -r src; do
    count=$((count + 1))
    if [[ -f "$LAB_DIR/$src" ]]; then
      echo "  PASS: bind source $src exists"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: bind source $src does not exist under tests/lab/bgp-fabric/"
      FAIL=$((FAIL + 1))
    fi
  done < <(sed -n 's/^ *- \([^:]*\):\/.*$/\1/p' "$TOPOLOGY")

  assert_eq "the topology binds the daemons file, five frr.conf and five setup.sh" "11" "$count"
  assert_file_contains_fixed "the kind binds the daemons file for every box" "$TOPOLOGY" \
    "- daemons:/etc/frr/daemons"
  local n
  for n in $NODES; do
    assert_file_contains_fixed "the topology binds $n/frr.conf" "$TOPOLOGY" \
      "- $n/frr.conf:/etc/frr/frr.conf"
    assert_file_contains_fixed "the topology binds $n/setup.sh" "$TOPOLOGY" \
      "- $n/setup.sh:/setup.sh"
  done
  assert_eq "every box starts from its setup.sh" "5" \
    "$(grep -c '^      entrypoint: /setup.sh$' "$TOPOLOGY" || true)"
}

# --- Test 3: the FRR image is pinned once, by digest ---
test_frr_image_is_pinned_once() {
  echo "Test: the FRR image is pinned by digest on exactly one line"

  local lines
  lines="$(grep -c 'quay\.io/frrouting/frr' "$TOPOLOGY" || true)"
  assert_eq "the FRR image appears on one line of the topology" "1" "$lines"
  assert_file_contains "the FRR image line carries a sha256 digest" "$TOPOLOGY" \
    '^      image: quay\.io/frrouting/frr:[^@ ]*@sha256:[a-f0-9]\{64\}$'
}

# --- Test 4: node links at 9000, fabric links at 9216 ---
test_every_link_carries_its_mtu() {
  echo "Test: the node links carry mtu: 9000, the fabric links mtu: 9216"

  local off
  assert_eq "the topology has five links" "5" "$(grep -c '^    - endpoints:' "$TOPOLOGY" || true)"
  assert_eq "two of them are node links" "2" "$(grep -c '^    - endpoints: \["node1:' "$TOPOLOGY" || true)"

  off="$(links_off_mtu "$TOPOLOGY")"
  if [[ -z "$off" ]]; then
    echo "  PASS: every link carries the mtu of its kind"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: links at the wrong mtu:"
    printf '    %s\n' "$off"
    FAIL=$((FAIL + 1))
  fi

  # The check has to name the link it misses. Drop the mtu of the last fabric
  # link in a copy and expect exactly that link back.
  local copy
  copy="$TMP_ROOT/missing-mtu.clab.yml"
  awk '/^      mtu: 9216$/ { n++; if (n == 3) next } { print }' "$TOPOLOGY" >"$copy"
  assert_eq "a fabric link with its mtu removed is named" \
    '["leaf3:eth1", "spine1:eth3"] has mtu none, needs 9216' "$(links_off_mtu "$copy")"
}

# --- Test 5: node1 states the node side of the contract ---
test_node1_states_the_node_side() {
  echo "Test: node1/frr.conf and node1/setup.sh state the node side of the contract"

  local conf="$LAB_DIR/node1/frr.conf" setup="$LAB_DIR/node1/setup.sh"
  assert_file_contains_fixed "node1's ASN runs in the fabric VRF" "$conf" "router bgp 4200000001 vrf fabric"
  assert_file_contains_fixed "BFD transmit interval 300 ms" "$conf" "transmit-interval 300"
  assert_file_contains_fixed "BFD receive interval 300 ms" "$conf" "receive-interval 300"
  assert_file_contains_fixed "BFD detect multiplier 3" "$conf" "detect-multiplier 3"
  assert_file_contains_fixed "ECMP over both uplinks" "$conf" "maximum-paths 2"

  assert_file_contains_fixed "setup.sh builds the fabric VRF" "$setup" "ip link add fabric type vrf table 100"
  assert_file_contains_fixed "setup.sh puts eth1 into the fabric VRF" "$setup" "ip link set eth1 master fabric"
  assert_file_contains_fixed "setup.sh puts eth2 into the fabric VRF" "$setup" "ip link set eth2 master fabric"
  assert_file_contains_fixed "setup.sh holds the bait 203.0.113.0/24" "$setup" "203.0.113.1/24"
  assert_eq "no lab file removes a default route (eth0 stands in for the node network)" "" \
    "$(grep -rl 'ip route del default' "$LAB_DIR" --exclude-dir=clab-bgp-fabric || true)"
}

# --- Test 6: the leaves, the observer and the spine ---
test_fabric_boxes_state_the_leaf_side() {
  echo "Test: the leaves map the deployment VRF to the L3VNI, the spine reflects"

  local leaf setup conf
  for leaf in leaf1 leaf2 leaf3; do
    setup="$LAB_DIR/$leaf/setup.sh"
    assert_file_contains_fixed "$leaf builds the deployment VRF" "$setup" "type vrf table 20001"
    assert_file_contains_fixed "$leaf builds the VXLAN device of the L3VNI" "$setup" "id 20001 nolearning"
    assert_eq "$leaf sets mtu 9166 on the bridge and the VXLAN device" "2" \
      "$(grep -c 'mtu 9166' "$setup" || true)"
  done
  for leaf in leaf1 leaf2; do
    conf="$LAB_DIR/$leaf/frr.conf"
    assert_file_contains_fixed "$leaf maps the VRF to the L3VNI" "$conf" "vni 20001"
    assert_file_contains_fixed "$leaf terminates the node session in the VRF" "$conf" "router bgp 65000 vrf cobaltcore"
    assert_file_contains_fixed "$leaf caps the node session at 8192 prefixes" "$conf" "maximum-prefix 8192"
    assert_file_contains_fixed "$leaf originates the default toward the node" "$conf" "default-originate"
    assert_file_contains_fixed "$leaf holds the bait 198.18.0.0/15" "$conf" "198.18.0.0/15"
    assert_file_contains_fixed "$leaf exports the route target" "$conf" "route-target export 65000:20001"
    assert_file_contains_fixed "$leaf exports the host routes as Type-5" "$conf" "advertise ipv4 unicast"
  done
  conf="$LAB_DIR/leaf3/frr.conf"
  assert_file_contains_fixed "leaf3 installs both VTEP paths" "$conf" "maximum-paths ibgp 2"
  assert_file_contains_fixed "leaf3 announces the A5 target" "$conf" "2001:db8:0:ff::3/128"

  conf="$LAB_DIR/spine1/frr.conf"
  assert_file_contains_fixed "spine1 reflects the leaves' EVPN routes" "$conf" "route-reflector-client"
  assert_file_not_contains "spine1 runs no VNI" "$conf" "vni"

  # The underlay sessions carry l2vpn evpn alone, so a leaf's default VRF
  # shows no IPv4 unicast peer.
  local n
  for n in leaf1 leaf2 leaf3 spine1; do
    assert_file_contains_fixed "$n keeps IPv4 unicast off the underlay session" "$LAB_DIR/$n/frr.conf" \
      "no bgp default ipv4-unicast"
  done
}

# --- Test 7: every setup.sh waits for its links and execs FRR ---
test_every_setup_waits_and_execs() {
  echo "Test: every setup.sh is executable, waits for its links and ends on an exec line"

  local n setup last
  for n in $NODES; do
    setup="$LAB_DIR/$n/setup.sh"
    if [[ -x "$setup" ]]; then
      echo "  PASS: $n/setup.sh is executable"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $n/setup.sh is not executable, so its box cannot start"
      FAIL=$((FAIL + 1))
    fi
    assert_file_contains_fixed "$n/setup.sh waits for its links" "$setup" '/sys/class/net/'
    last="$(awk 'NF { line = $0 } END { print line }' "$setup")"
    assert_contains "$n/setup.sh ends on an exec line" "$last" "exec "
  done
}

# --- Test 8: run.sh is strict and pins containerlab by digest ---
test_runner_is_strict_and_pinned() {
  echo "Test: run.sh starts strict and pins the containerlab image by digest"

  local first
  first="$(awk '!/^#/ && !/^[[:space:]]*$/ { print; exit }' "$RUNNER")"
  assert_eq "the first command of run.sh is set -euo pipefail" "set -euo pipefail" "$first"
  assert_file_contains "CLAB_IMAGE_DEFAULT pins ghcr.io/srl-labs/clab by digest" "$RUNNER" \
    '^CLAB_IMAGE_DEFAULT="ghcr\.io/srl-labs/clab:[^@"]*@sha256:[a-f0-9]\{64\}"$'
  if [[ -x "$RUNNER" ]]; then
    echo "  PASS: run.sh is executable"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: run.sh is not executable"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 9: the prefix-lists of C4 ---
test_prefix_lists_hold_c4() {
  echo "Test: node1's prefix-lists hold C4, the leaves accept exactly what node1 announces"

  local conf="$LAB_DIR/node1/frr.conf" line leaf
  for line in "neighbor leaves prefix-list FABRIC-OUT-V4 out" "neighbor leaves prefix-list FABRIC-IN-V4 in" \
    "neighbor leaves prefix-list FABRIC-OUT-V6 out" "neighbor leaves prefix-list FABRIC-IN-V6 in"; do
    assert_file_contains_fixed "node1 binds: $line" "$conf" "$line"
  done
  assert_eq "node1 accepts the IPv4 default alone" "0.0.0.0/0" "$(permits "$conf" FABRIC-IN-V4)"
  assert_eq "node1 accepts the IPv6 default alone" "::/0" "$(permits "$conf" FABRIC-IN-V6)"

  assert_not_empty "node1 announces IPv4 ranges" "$(permits "$conf" FABRIC-OUT-V4)"
  assert_eq "node1 announces IPv4 /32 only" "" "$(permits "$conf" FABRIC-OUT-V4 | grep -v ' ge 32$' || true)"
  assert_not_empty "node1 announces IPv6 ranges" "$(permits "$conf" FABRIC-OUT-V6)"
  assert_eq "node1 announces IPv6 /128 only" "" "$(permits "$conf" FABRIC-OUT-V6 | grep -v ' ge 128$' || true)"

  for leaf in leaf1 leaf2; do
    for line in "neighbor nodes prefix-list NODES-IN-V4 in" "neighbor nodes prefix-list NODES-IN-V6 in"; do
      assert_file_contains_fixed "$leaf binds: $line" "$LAB_DIR/$leaf/frr.conf" "$line"
    done
    assert_eq "$leaf's NODES-IN-V4 accepts exactly what node1's FABRIC-OUT-V4 announces" \
      "$(permits "$conf" FABRIC-OUT-V4)" "$(permits "$LAB_DIR/$leaf/frr.conf" NODES-IN-V4)"
    assert_eq "$leaf's NODES-IN-V6 accepts exactly what node1's FABRIC-OUT-V6 announces" \
      "$(permits "$conf" FABRIC-OUT-V6)" "$(permits "$LAB_DIR/$leaf/frr.conf" NODES-IN-V6)"
  done
}

# --- Run ---
test_every_lab_file_carries_the_spdx_pair
test_every_bind_source_exists
test_frr_image_is_pinned_once
test_every_link_carries_its_mtu
test_node1_states_the_node_side
test_fabric_boxes_state_the_leaf_side
test_every_setup_waits_and_execs
test_runner_is_strict_and_pinned
test_prefix_lists_hold_c4

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
