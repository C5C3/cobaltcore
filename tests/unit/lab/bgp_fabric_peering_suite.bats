#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the fixtures of the frr-k8s fabric peering suite under
# tests/e2e-bgp-fabric/ keep the shape the suite and the BGP Node Substrate
# page rely on, without a cluster:
#   - every suite file carries the SPDX pair
#   - every image of the two Pod files is the FRR pin of the BGP fabric lab,
#     so Renovate moves the lab and the peers together
#   - daemons is the lab's daemons file with ospfd off and nothing else
#     changed
#   - 03-frrconfiguration.yaml states the node side of the contract with the
#     values tests/lab/bgp-fabric/node1/frr.conf uses (ASN, router ID, VRF,
#     BFD 300/300/3), both loopbacks, the two defaults it accepts, mode: all
#     and the NODE_NAME placeholder
#   - 04-frrconfiguration-kernel-routes.yaml redistributes kernel routes,
#     imports ovnvrf42 and widens the four lists frr-k8s generates
#   - peer.sh sets accept_ra=0 on the uplink before the uplink comes up
#   - node-prep.sh, run against a stub `ip`, names the missing vrf module and
#     exits 1, ignores only a missing device on delete, and rejects an
#     unknown verb
#   - the fabric peers of tests/e2e-ovn-overlay/fabric-advertised-route/ run
#     the same lab pin, and their peer.sh leaves the node end of the uplink to
#     the OVNChassis: it neither enslaves it nor brings it up
#
# Usage: bin/bats tests/unit/lab/bgp_fabric_peering_suite.bats

load '../../lib/bats/common'
load '../../lib/bats/stubs'

SUITE_ROOT="$PROJECT_ROOT/tests/e2e-bgp-fabric"
SUITE="$SUITE_ROOT/frr-k8s-fabric-peering"
LAB_DIR="$PROJECT_ROOT/tests/lab/bgp-fabric"
NODE1_CONF="$LAB_DIR/node1/frr.conf"
PER_NODE="$SUITE/03-frrconfiguration.yaml"
KERNEL_ROUTES="$SUITE/04-frrconfiguration-kernel-routes.yaml"
OVERLAY_SUITE="$PROJECT_ROOT/tests/e2e-ovn-overlay/fabric-advertised-route"

setup() {
  TMP_ROOT="$BATS_TEST_TMPDIR"
}

@test "every file under tests/e2e-bgp-fabric/ carries the SPDX pair" {
  local f rel
  while IFS= read -r f; do
    rel="${f#"$PROJECT_ROOT"/}"
    soft assert_file_matches "$rel carries SPDX-FileCopyrightText" "$f" \
      "SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company"
    soft assert_file_matches "$rel carries SPDX-License-Identifier" "$f" \
      "SPDX-License-Identifier: Apache-2.0"
  done < <(find "$SUITE_ROOT" -type f | sort)
}

@test "every image of the two Pod files is the FRR pin of the lab" {
  local pin
  pin="$(awk '/^[[:space:]]+image: quay\.io\/frrouting\/frr:/ { print $2; exit }' "$LAB_DIR/bgp-fabric.clab.yml")"
  assert_not_empty "the lab pins an FRR image" "$pin"

  local f images
  for f in 01-fabric-node.yaml 02-fabric-peers.yaml; do
    images="$(awk '/^[[:space:]]+image:/ { print $2 }' "$SUITE/$f" | sort -u)"
    soft assert_eq "$f runs only the lab's FRR pin" "$pin" "$images"
  done
  images="$(awk '/^[[:space:]]+image:/ { print $2 }' "$OVERLAY_SUITE/03-fabric-peers.yaml" | sort -u)"
  assert_eq "the overlay suite's 03-fabric-peers.yaml runs only the lab's FRR pin" "$pin" "$images"
}

@test "the overlay suite's peer.sh leaves the node end of the uplink alone" {
  local peer="$OVERLAY_SUITE/peer.sh"
  assert_file_matches "peer.sh creates the veth uplink in the node's namespace" "$peer" \
    'nsenter -t 1 -n -- ip link add "\$UPLINK" mtu 9000 type veth peer name eth1 mtu 9000 netns \$\$'
  assert_file_not_matches "peer.sh does not enslave the uplink" "$peer" 'master'
  assert_file_not_matches "peer.sh does not bring the uplink up" "$peer" 'ip link set "\$UPLINK" up'
}

@test "daemons is the lab's daemons file with ospfd=no" {
  local diff_out
  diff_out="$(diff <(sed 's/^ospfd=yes$/ospfd=no/' "$LAB_DIR/daemons") "$SUITE/daemons" || true)"
  assert_eq "daemons differs from the lab's only in ospfd" "" "$diff_out"
  assert_file_matches "daemons starts bgpd" "$SUITE/daemons" "^bgpd=yes$"
  assert_file_matches "daemons starts bfdd" "$SUITE/daemons" "^bfdd=yes$"
}

@test "03-frrconfiguration.yaml states the node side with the lab's values" {
  require_tool yq

  local asn vrf router_id tx rx mult
  asn="$(awk '/^router bgp [0-9]+ vrf / { print $3; exit }' "$NODE1_CONF")"
  vrf="$(awk '/^router bgp [0-9]+ vrf / { print $5; exit }' "$NODE1_CONF")"
  router_id="$(awk '/^ bgp router-id / { print $3; exit }' "$NODE1_CONF")"
  tx="$(awk '/transmit-interval/ { print $2; exit }' "$NODE1_CONF")"
  rx="$(awk '/receive-interval/ { print $2; exit }' "$NODE1_CONF")"
  mult="$(awk '/detect-multiplier/ { print $2; exit }' "$NODE1_CONF")"

  q() { yq -r "$1" "$PER_NODE"; }

  assert_eq "the router carries node1's ASN" "$asn" "$(q '.spec.bgp.routers[0].asn')"
  assert_eq "the router carries node1's router ID" "$router_id" "$(q '.spec.bgp.routers[0].id')"
  assert_eq "the router sits in node1's VRF" "$vrf" "$(q '.spec.bgp.routers[0].vrf')"
  assert_eq "the BFD profile is node1's ${rx}/${tx}/${mult}" "fabric ${rx} ${tx} ${mult}" \
    "$(q '.spec.bgp.bfdProfiles[0] | .name + " " + (.receiveInterval | tostring) + " " + (.transmitInterval | tostring) + " " + (.detectMultiplier | tostring)')"
  assert_eq "the router announces both fabric loopbacks" "192.0.2.1/32 2001:db8::1/128" \
    "$(q '.spec.bgp.routers[0].prefixes | join(" ")')"
  assert_eq "two unnumbered neighbors, fab1 and fab2" "fab1 fab2" \
    "$(q '[.spec.bgp.routers[0].neighbors[].interface] | join(" ")')"
  assert_eq "both neighbors take any external ASN with BFD profile fabric" "external/fabric external/fabric" \
    "$(q '[.spec.bgp.routers[0].neighbors[] | .dynamicASN + "/" + .bfdProfile] | join(" ")')"
  assert_eq "both neighbors announce the listed prefixes" "all all" \
    "$(q '[.spec.bgp.routers[0].neighbors[].toAdvertise.allowed.mode] | join(" ")')"
  assert_eq "both neighbors accept the two defaults alone" \
    "filtered:0.0.0.0/0,::/0 filtered:0.0.0.0/0,::/0" \
    "$(q '[.spec.bgp.routers[0].neighbors[].toReceive.allowed | .mode + ":" + ([.prefixes[].prefix] | join(","))] | join(" ")')"
  assert_eq "the name carries the NODE_NAME placeholder" "fabric-NODE_NAME" "$(q '.metadata.name')"
  assert_eq "the node selector carries the NODE_NAME placeholder" "NODE_NAME" \
    "$(q '.spec.nodeSelector.matchLabels["kubernetes.io/hostname"]')"
}

@test "04-frrconfiguration-kernel-routes.yaml redistributes kernel routes and widens the generated lists" {
  local list
  for list in "ip prefix-list fab1-fabric-allowed-ipv4 seq 100 permit 198.51.100.0/24 ge 32" \
    "ip prefix-list fab2-fabric-allowed-ipv4 seq 100 permit 198.51.100.0/24 ge 32" \
    "ipv6 prefix-list fab1-fabric-allowed-ipv6 seq 100 permit 2001:db8::/64 ge 128" \
    "ipv6 prefix-list fab2-fabric-allowed-ipv6 seq 100 permit 2001:db8::/64 ge 128"; do
    soft assert_file_contains_fixed "names the generated list: $list" "$KERNEL_ROUTES" "      $list"
  done
  assert_eq "redistributes kernel routes in both families" "2" \
    "$(grep -c '^        redistribute kernel$' "$KERNEL_ROUTES")"
  assert_file_contains_fixed "the raw router is the ovnvrf42 router" "$KERNEL_ROUTES" \
    "      router bgp 4200000001 vrf ovnvrf42"

  require_tool yq
  assert_eq "the fabric router imports ovnvrf42" "ovnvrf42" \
    "$(yq -r '.spec.bgp.routers[] | select(.vrf == "fabric") | .imports[].vrf' "$KERNEL_ROUTES")"
}

@test "peer.sh sets accept_ra=0 on the uplink before the uplink comes up" {
  local ra up
  ra="$(grep -n 'accept_ra=0' "$SUITE/peer.sh" | head -1 | cut -d: -f1)"
  up="$(grep -n 'ip link set "$UPLINK" up' "$SUITE/peer.sh" | head -1 | cut -d: -f1)"
  assert_not_empty "peer.sh sets accept_ra=0" "$ra"
  assert_not_empty "peer.sh brings the uplink up" "$up"
  if [[ -n "$ra" && -n "$up" && "$ra" -lt "$up" ]]; then
    check_passed "accept_ra=0 (line $ra) precedes the link up (line $up)"
  else
    echo "accept_ra=0 (line ${ra:-none}) does not precede the link up (line ${up:-none})"
    fail "accept_ra=0 (line $ra) precedes the link up (line $up)"
  fi
}

# run_node_prep <verb> [env...]: runs a copy of node-prep.sh whose /tmp/ is the
# scratch directory, with the stub ip first on PATH. Prints the combined
# output and returns the script's exit status.
run_node_prep() {
  local verb="$1"
  shift
  with_stubs env "$@" sh "$TMP_ROOT/node-prep.sh" "$verb" 2>&1
}

@test "node-prep.sh against a stub ip" {
  mkdir -p "$TMP_ROOT/state"
  sed "s#/tmp/#$TMP_ROOT/state/#g" "$SUITE/node-prep.sh" >"$TMP_ROOT/node-prep.sh"
  # The stub answers like iproute2 on a kernel without the vrf module: every
  # device is missing, and a VRF device cannot be created. STUB_DEL_ERR makes
  # a delete fail for another reason.
  stub_script ip <<'EOF'
case "$*" in
  "link del "*)
    if [ -n "${STUB_DEL_ERR:-}" ]; then echo "$STUB_DEL_ERR" >&2; exit 2; fi
    echo "Cannot find device \"$3\"" >&2
    exit 1
    ;;
  "-4 route show default") echo "default via 172.18.0.1 dev eth0" ;;
  "-6 route show default") echo "default via fc00::1 dev eth0 metric 1024 pref medium" ;;
  "link add fabric type vrf table 100") echo "Error: Unknown device type." >&2; exit 2 ;;
  *) echo "unexpected: ip $*" >&2; exit 99 ;;
esac
EOF

  run run_node_prep up
  assert_eq "up exits 1 without the vrf module" "1" "$status"
  assert_contains "up names the missing vrf module" "$output" \
    "ERROR: cannot create a VRF device; the node kernel has no vrf module"
  assert_contains "up prints ip's own message" "$output" "Error: Unknown device type."
  assert_eq "up snapshots the IPv4 default before it fails" "default via 172.18.0.1 dev eth0" \
    "$(cat "$TMP_ROOT/state/default-v4.before" 2>/dev/null)"
  assert_eq "up never marks the node prepared" "absent" \
    "$([[ -e "$TMP_ROOT/state/prepared" ]] && echo present || echo absent)"

  run run_node_prep down
  assert_eq "down exits 0 when no device exists" "0" "$status"

  run run_node_prep down STUB_DEL_ERR="RTNETLINK answers: Operation not permitted"
  assert_eq "down exits 1 on any other delete error" "1" "$status"
  assert_contains "down names the failed delete" "$output" \
    "ERROR: ip link del ovn42: RTNETLINK answers: Operation not permitted"

  run run_node_prep sideways
  assert_eq "an unknown verb exits 2" "2" "$status"
  assert_contains "an unknown verb prints the usage" "$output" "usage:"
}
