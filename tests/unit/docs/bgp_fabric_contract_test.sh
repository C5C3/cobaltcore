#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify docs/reference/networking/bgp-fabric-contract.md stays in step with
# the code it cites, the lab it imports and the pages that link it:
#   - the page opens with frontmatter carrying title: and quadrant:
#   - the max_header_size the page states is the one reconcile_config.go
#     renders, and the tenant MTUs and minimum node network MTUs derived from
#     it follow; a copy of the Go file that renders 39 proves the comparison
#     names the page's value
#   - the type_drivers value the page quotes is the one the operator renders,
#     and the operator still renders no global_physnet_mtu or path_mtu (the
#     page names spec.extraConfig as today's lever)
#   - the page cites ovn-encap-ip, global_physnet_mtu, path_mtu,
#     connected-as-host, neutron-bgp-peer-bridges, the two neutron 29.0.0
#     BadRequest messages, the L3VNI and route target of the lab, and carries
#     the warning box of the lab run
#   - both code imports resolve to files under tests/lab/bgp-fabric/, and the
#     imported node configuration runs BGP in the fabric VRF
#   - node1's outbound prefix-lists permit exactly the ranges the address
#     table announces, as /32 for IPv4 and /128 for IPv6
#   - the sidebar lists the page, and the four pages that promised it link it
#   - the containerlab run record has a row per assertion, and the leaf-pair
#     record states that none was taken yet
#
# Runs under Bash 3.2.
#
# Usage: bash tests/unit/docs/bgp_fabric_contract_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

PAGE="$PROJECT_ROOT/docs/reference/networking/bgp-fabric-contract.md"
PAGE_DIR="$(dirname "$PAGE")"
CONFIG_GO="$PROJECT_ROOT/operators/neutron/internal/controller/reconcile_config.go"
CONTROLLER_DIR="$PROJECT_ROOT/operators/neutron/internal/controller"
SIDEBAR="$PROJECT_ROOT/docs/.vitepress/sidebar.ts"
LAB_DIR="$PROJECT_ROOT/tests/lab/bgp-fabric"

if [[ ! -f "$PAGE" ]]; then
  echo "FAIL: $PAGE does not exist"
  exit 1
fi

# The page with its line breaks folded, for phrases a rewrap may split.
PAGE_FLAT="$(tr '\n' ' ' <"$PAGE")"

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# assert_page_says <description> <phrase>: the folded page contains <phrase>.
# A failure prints the phrase, not the page.
assert_page_says() {
  if [[ "$PAGE_FLAT" == *"$2"* ]]; then
    echo "  PASS: $1"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $1"
    echo "    expected the page to contain: $2"
    FAIL=$((FAIL + 1))
  fi
}

# rendered <option> <go file>: the value reconcile_config.go renders for
# <option>, a map entry "<option>": "<value>".
rendered() {
  sed -n "s/.*\"$1\": *\"\\([^\"]*\\)\".*/\\1/p" "$2" | head -n 1
}

# stated_max_header_size: the number after "`max_header_size` of " on the page.
stated_max_header_size() {
  printf '%s' "$PAGE_FLAT" | sed -n 's/.*`max_header_size` of \([0-9][0-9]*\).*/\1/p'
}

# max_header_size_drift <go file>: prints how the page and <go file> part,
# nothing when they agree.
max_header_size_drift() {
  local go page
  go="$(rendered max_header_size "$1")"
  page="$(stated_max_header_size)"
  if [[ "$go" != "$page" ]]; then
    echo "page states max_header_size of ${page:-nothing}, reconcile_config.go renders ${go:-nothing}"
  fi
}

# option_renderers <option> <dir>: the Go files under <dir>, tests left out,
# that carry "<option>" as a string literal.
option_renderers() {
  grep -rlF "\"$1\"" --include='*.go' --exclude='*_test.go' "$2" || true
}

# section <heading line>: the page's lines after the heading up to the next
# heading of any level.
section() {
  awk -v h="$1" '
    $0 == h { on = 1; next }
    on && /^#/ { exit }
    on { print }
  ' "$PAGE"
}

# announced_rows <family> <length>: the address-table rows of <family>
# announced as /<length>, as the permit clause "<range> ge <length>", sorted.
announced_rows() {
  awk -F'|' -v fam="$1" -v len="$2" '
    { for (i = 2; i <= 5; i++) gsub(/^ +| +$|`/, "", $i) }
    $3 == fam && $5 == "/" len { print $2 " ge " len }
  ' "$PAGE" | sort
}

# permits <list>: the permit clauses of prefix-list <list> in node1's
# frr.conf, the list name and the sequence number stripped, sorted.
permits() {
  awk -v l="$1" '$0 ~ ("prefix-list " l " seq ") && / permit / { sub(/.* permit /, ""); print }' \
    "$LAB_DIR/node1/frr.conf" | sort
}

# --- Test 1: frontmatter ---
test_page_opens_with_frontmatter() {
  echo "Test: the page opens with frontmatter carrying title and quadrant"

  local front
  front="$(awk 'NR == 1 && $0 != "---" { exit } NR > 1 && $0 == "---" { exit } NR > 1 { print }' "$PAGE")"
  assert_contains "frontmatter starts on line 1 and carries the title" \
    "$front" "title: BGP Fabric Contract"
  assert_contains "frontmatter carries the quadrant" "$front" "quadrant: operator"
}

# --- Test 2: max_header_size and the MTU numbers derived from it ---
test_max_header_size_matches_the_operator() {
  echo "Test: the page's max_header_size and MTU arithmetic match reconcile_config.go"

  local n
  n="$(rendered max_header_size "$CONFIG_GO")"
  assert_not_empty "reconcile_config.go renders a max_header_size" "$n"
  if [[ -z "$n" ]]; then
    return
  fi
  assert_eq "the page states the max_header_size reconcile_config.go renders" "" \
    "$(max_header_size_drift "$CONFIG_GO")"

  local mtu
  for mtu in 1500 9000; do
    assert_file_contains_fixed "the page states the tenant MTU of a $mtu-byte IPv4 node network" "$PAGE" \
      "| 4 | $mtu | 20 | $n | $((mtu - 20 - n)) |"
    assert_file_contains_fixed "the page states the tenant MTU of a $mtu-byte IPv6 node network" "$PAGE" \
      "| 6 | $mtu | 40 | $n | $((mtu - 40 - n)) |"
  done
  assert_page_says "the page states both minimum node network MTUs" \
    "at least $((1500 + 20 + n)) over IPv4 or $((1500 + 40 + n)) over IPv6"
  assert_page_says "the page cites the file that renders it" \
    "\`max_header_size\` of $n (\`operators/neutron/internal/controller/reconcile_config.go\`)"

  # The comparison has to name the page's value when the two part: render 39
  # in a copy of the Go file.
  local copy drift
  copy="$TMP_ROOT/reconcile_config.go"
  sed 's/"max_header_size": *"[0-9]*"/"max_header_size": "39"/' "$CONFIG_GO" >"$copy"
  drift="$(max_header_size_drift "$copy")"
  assert_contains "a Go file that renders 39 is caught, naming the page's value" "$drift" \
    "page states max_header_size of $n, reconcile_config.go renders 39"
}

# --- Test 3: the operator and upstream facts the page cites ---
test_page_cites_the_code_and_upstream() {
  echo "Test: the page cites the operator and upstream facts, and the code still holds them"

  local phrase
  for phrase in ovn-encap-ip global_physnet_mtu path_mtu connected-as-host neutron-bgp-peer-bridges \
    "20001" "65000:20001" "::: warning"; do
    assert_file_contains_fixed "the page names $phrase" "$PAGE" "$phrase"
  done
  assert_page_says "the page quotes the VLAN rejection in full" \
    "VLAN provider networks are not supported when the BGP service plugin is enabled. Only flat provider networks are supported."
  assert_page_says "the page quotes the second-flat-network rejection in full" \
    "Only a single flat provider network is supported when the BGP service plugin is enabled."
  assert_page_says "the page attributes both to the upstream plugin" \
    "neutron 29.0.0, \`neutron/services/bgp/plugin.py\`"

  local drivers
  drivers="$(rendered type_drivers "$CONFIG_GO")"
  assert_not_empty "reconcile_config.go renders type_drivers" "$drivers"
  assert_file_contains_fixed "the page quotes the type_drivers the operator renders" "$PAGE" \
    "\`type_drivers = $drivers\`"
  assert_file_contains_fixed "the chassis writes the node address as the encap IP" \
    "$PROJECT_ROOT/operators/ovn/internal/controller/reconcile_nodes.go" \
    'external_ids:ovn-encap-ip="${NODE_IP}"'

  # A rendered option is a string literal in the controller's own code; a
  # test that sets one through extraConfig renders nothing.
  local option
  for option in global_physnet_mtu path_mtu; do
    assert_eq "the Neutron operator renders no $option (the page names spec.extraConfig as the lever)" "" \
      "$(option_renderers "$option" "$CONTROLLER_DIR")"
  done
}

# --- Test 4: the code imports resolve ---
test_code_imports_resolve() {
  echo "Test: both code imports resolve to files under tests/lab/bgp-fabric/"

  local line target count=0
  assert_file_not_contains "no import starts with @/, which cobaltcore.dev cannot resolve" "$PAGE" '^<<< @/'
  while IFS= read -r line; do
    count=$((count + 1))
    # Strip the import prefix and a {lang}, {lines} or #region suffix.
    target="${line#<<< }"
    target="${target%%\{*}"
    target="${target%%#*}"
    target="${target%"${target##*[![:space:]]}"}"
    if [[ -f "$PAGE_DIR/$target" ]]; then
      echo "  PASS: import $target exists"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: import $target does not exist relative to the page"
      FAIL=$((FAIL + 1))
    fi
  done < <(grep '^<<< ' "$PAGE" || true)
  assert_eq "the page has two code imports" "2" "$count"
  assert_file_contains_fixed "the page imports the topology" "$PAGE" \
    "<<< ../../../tests/lab/bgp-fabric/bgp-fabric.clab.yml"
  assert_file_contains_fixed "the page imports node1's FRR config" "$PAGE" \
    "<<< ../../../tests/lab/bgp-fabric/node1/frr.conf"
  assert_file_contains_fixed "the imported node config runs BGP in the fabric VRF" \
    "$LAB_DIR/node1/frr.conf" "router bgp 4200000001 vrf fabric"
}

# --- Test 5: sidebar and the pages that link the contract ---
test_page_is_listed_and_linked() {
  echo "Test: the sidebar lists the page and the four promising pages link it"

  assert_file_contains_fixed "sidebar.ts lists the page" "$SIDEBAR" \
    "/reference/networking/bgp-fabric-contract"

  local p count
  for p in docs/guides/neutron/create-a-provider-network.md docs/reference/neutron/index.md \
    docs/reference/ovn/index.md docs/architecture/index.md; do
    count="$(grep -c 'bgp-fabric-contract\.md' "$PROJECT_ROOT/$p" || true)"
    assert_eq "$p links the contract once" "1" "$count"
  done
  assert_file_not_contains "the provider-network guide no longer promises the page" \
    "$PROJECT_ROOT/docs/guides/neutron/create-a-provider-network.md" "arrives with that work"
}

# --- Test 6: the run records ---
test_run_records() {
  echo "Test: the containerlab record has a row per assertion, the leaf-pair record is empty"

  local record leaf id
  record="$(section "#### containerlab")"
  assert_not_empty "the page has a containerlab run record" "$record"
  for id in A1 A2 A3 A4 A5 A6 A7; do
    assert_contains "the containerlab record has a row for $id" "$record" "| $id |"
  done
  assert_contains "the record names the containerlab version" "$record" "with containerlab"
  assert_contains "the record names the FRR image digest" "$record" "quay.io/frrouting/frr@sha256:"

  leaf="$(section "#### Leaf pair")"
  assert_contains "the leaf-pair record states that no run was taken" "$leaf" \
    "No acceptance run has been recorded yet."
}

# --- Test 7: the address table drives node1's outbound filter ---
test_address_table_drives_the_node_filter() {
  echo "Test: node1's outbound prefix-lists permit exactly the ranges the address table announces"

  local want
  want="$(announced_rows IPv4 32)"
  assert_not_empty "the address table announces IPv4 rows as /32" "$want"
  assert_eq "FABRIC-OUT-V4 permits the table's IPv4 rows as /32" "$want" "$(permits FABRIC-OUT-V4)"
  want="$(announced_rows IPv6 128)"
  assert_not_empty "the address table announces IPv6 rows as /128" "$want"
  assert_eq "FABRIC-OUT-V6 permits the table's IPv6 rows as /128" "$want" "$(permits FABRIC-OUT-V6)"
}

# --- Run ---
test_page_opens_with_frontmatter
test_max_header_size_matches_the_operator
test_page_cites_the_code_and_upstream
test_code_imports_resolve
test_page_is_listed_and_linked
test_run_records
test_address_table_drives_the_node_filter

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
