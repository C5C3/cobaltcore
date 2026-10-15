#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the containerlab lab of the BGP fabric contract under
# tests/lab/bgp-fabric/ keeps the shape the contract page and run.sh rely on,
# without deploying it:
#   - every file carries the SPDX pair
#   - every bind source in the topology exists: one daemons file for every
#     box, an frr.conf per node
#   - the FRR image is pinned by digest on exactly one line, the line the
#     Renovate customManager reads
#   - every link carries mtu: 9000; the check names the link that does not,
#     and a copy with one mtu removed proves it does
#   - node1's FRR config states the contract values (BFD 300/300/3,
#     maximum-paths 2, the /32 and /128 bounds of the outbound filter) and
#     carries the bait A4 needs, 203.0.113.0/24 redistributed
#   - each leaf bounds the node session, originates the default and
#     redistributes the bait 198.18.0.0/15
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
NODES="node1 leaf1 leaf2 spine1"

if [[ ! -f "$TOPOLOGY" ]]; then
  echo "FAIL: $TOPOLOGY does not exist"
  exit 1
fi

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# links_without_mtu_9000 <topology>: prints the endpoints of every link whose
# entry under links: carries no "mtu: 9000" before the next entry.
links_without_mtu_9000() {
  awk '
    /^  links:/ { in_links = 1; next }
    in_links && /^  [^ ]/ { in_links = 0 }
    !in_links { next }
    /^    - endpoints:/ {
      if (link != "" && !mtu) print link
      link = $0; sub(/^    - endpoints: */, "", link); mtu = 0; next
    }
    /^      mtu: 9000$/ { mtu = 1 }
    END { if (link != "" && !mtu) print link }
  ' "$1"
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

  # The topology, run.sh, the daemons file and an frr.conf per node.
  assert_gte "the lab has at least its seven files" "$count" 7
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
  done < <(sed -n 's/^ *- \([^:]*\):\/etc\/frr\/.*$/\1/p' "$TOPOLOGY")

  # The daemons file once for the kind, an frr.conf per node.
  assert_eq "the topology binds the daemons file and four frr.conf files" "5" "$count"

  assert_file_contains_fixed "the kind binds the daemons file for every box" "$TOPOLOGY" \
    "- daemons:/etc/frr/daemons"
  local n
  for n in $NODES; do
    assert_file_contains_fixed "the topology binds $n/frr.conf" "$TOPOLOGY" \
      "- $n/frr.conf:/etc/frr/frr.conf"
  done
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

# --- Test 4: every link carries mtu: 9000 ---
test_every_link_carries_mtu_9000() {
  echo "Test: every link of the topology carries mtu: 9000"

  local links missing
  links="$(grep -c '^    - endpoints:' "$TOPOLOGY" || true)"
  assert_eq "the topology has four links" "4" "$links"

  missing="$(links_without_mtu_9000 "$TOPOLOGY")"
  if [[ -z "$missing" ]]; then
    echo "  PASS: every link carries mtu: 9000"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: links without mtu: 9000:"
    printf '    %s\n' "$missing"
    FAIL=$((FAIL + 1))
  fi

  # The check has to name the link it misses. Drop the mtu of the second link
  # in a copy and expect exactly that link back.
  local copy
  copy="$TMP_ROOT/missing-mtu.clab.yml"
  awk '/^      mtu: 9000$/ { n++; if (n == 2) next } { print }' "$TOPOLOGY" >"$copy"
  assert_eq "a link with its mtu removed is named" \
    '["node1:eth2", "leaf2:eth1"]' "$(links_without_mtu_9000 "$copy")"
}

# --- Test 5: node1 states the contract values ---
test_node_config_states_the_contract_values() {
  echo "Test: node1/frr.conf states the node side of the contract"

  local conf="$LAB_DIR/node1/frr.conf"
  assert_file_contains_fixed "BFD transmit interval 300 ms" "$conf" "transmit-interval 300"
  assert_file_contains_fixed "BFD receive interval 300 ms" "$conf" "receive-interval 300"
  assert_file_contains_fixed "BFD detect multiplier 3" "$conf" "detect-multiplier 3"
  assert_file_contains_fixed "ECMP over both uplinks" "$conf" "maximum-paths 2"
  assert_file_contains_fixed "the outbound filter passes IPv4 /32 only" "$conf" "ge 32"
  assert_file_contains_fixed "the outbound filter passes IPv6 /128 only" "$conf" "ge 128"
  assert_file_contains_fixed "inbound IPv4 is the default route only" "$conf" \
    "ip prefix-list FABRIC-IN-V4 seq 10 permit 0.0.0.0/0"
  assert_file_contains_fixed "inbound IPv6 is the default route only" "$conf" \
    "ipv6 prefix-list FABRIC-IN-V6 seq 10 permit ::/0"
  assert_file_contains_fixed "the bait 203.0.113.0/24 sits on the loopback" "$conf" \
    "ip address 203.0.113.1/24"
  assert_file_contains_fixed "the bait reaches the BGP table" "$conf" "redistribute connected"
}

# --- Test 6: each leaf bounds the node session, sends the default and its bait ---
test_leaf_configs_bound_and_originate() {
  echo "Test: each leaf bounds the node session, sends it the default and its bait"

  local leaf conf
  for leaf in leaf1 leaf2; do
    conf="$LAB_DIR/$leaf/frr.conf"
    assert_file_contains_fixed "$leaf caps the node session at 8192 prefixes" "$conf" \
      "neighbor nodes maximum-prefix 8192"
    assert_file_contains_fixed "$leaf originates the default toward the node" "$conf" \
      "neighbor nodes default-originate"
    assert_file_contains_fixed "$leaf holds the bait 198.18.0.0/15" "$conf" \
      "ip route 198.18.0.0/15 blackhole"
    assert_file_contains_fixed "$leaf offers the bait to the node" "$conf" "redistribute static"
  done
}

# --- Test 7: run.sh is strict and pins containerlab by digest ---
test_runner_is_strict_and_pinned() {
  echo "Test: run.sh starts strict and pins the containerlab image by digest"

  local first
  first="$(awk '!/^#/ && !/^[[:space:]]*$/ { print; exit }' "$RUNNER")"
  assert_eq "the first command of run.sh is set -euo pipefail" "set -euo pipefail" "$first"
  assert_file_contains "CLAB_IMAGE_DEFAULT pins ghcr.io/srl-labs/clab by digest" "$RUNNER" \
    '^CLAB_IMAGE_DEFAULT="ghcr\.io/srl-labs/clab:[^@"]*@sha256:[a-f0-9]\{64\}"$'
}

# --- Run ---
test_every_lab_file_carries_the_spdx_pair
test_every_bind_source_exists
test_frr_image_is_pinned_once
test_every_link_carries_mtu_9000
test_node_config_states_the_contract_values
test_leaf_configs_bound_and_originate
test_runner_is_strict_and_pinned

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
