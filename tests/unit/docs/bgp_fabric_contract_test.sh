#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify docs/reference/networking/bgp-fabric-contract.md stays in step with
# the code it cites, the lab it imports and the pages that link it:
#   - the page opens with frontmatter carrying title: and quadrant:
#   - the max_header_size the page states is the one reconcile_config.go
#     renders, and the MTU numbers derived from it (overhead, tenant MTU,
#     minimum node MTU) follow; a copy of the Go file with another value
#     proves the comparison names both
#   - the page cites ovn-encap-ip, global_physnet_mtu, connected-as-host and
#     the two neutron 29.0.0 BadRequest messages, and the operator code still
#     does what the page says (encap IP, type drivers, VNI range, no rendered
#     global_physnet_mtu or overlay_ip_version)
#   - both code imports resolve to files under tests/lab/bgp-fabric/
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
CONFIG_GO="$PROJECT_ROOT/operators/neutron/internal/controller/reconcile_config.go"
NODES_GO="$PROJECT_ROOT/operators/ovn/internal/controller/reconcile_nodes.go"
SIDEBAR="$PROJECT_ROOT/docs/.vitepress/config.ts"
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

# rendered_max_header_size <go file>: the value reconcile_config.go renders.
rendered_max_header_size() {
  awk 'match($0, /"max_header_size": *"[0-9]+"/) {
    s = substr($0, RSTART, RLENGTH); gsub(/[^0-9]/, "", s); print s; exit
  }' "$1"
}

# stated_max_header_size <page>: the number after "`max_header_size` of ".
stated_max_header_size() {
  tr '\n' ' ' <"$1" | sed -n 's/.*`max_header_size` of \([0-9][0-9]*\).*/\1/p'
}

# option_renderers <option> <dir>: the Go files under <dir>, tests left out,
# that carry "<option>" as a string literal.
option_renderers() {
  grep -rlF "\"$1\"" --include='*.go' --exclude='*_test.go' "$2" || true
}

# section <page> <heading line>: the lines after the heading up to the next
# heading of any level.
section() {
  awk -v h="$2" '
    $0 == h { on = 1; next }
    on && /^#/ { exit }
    on { print }
  ' "$1"
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

  local rendered stated
  rendered="$(rendered_max_header_size "$CONFIG_GO")"
  stated="$(stated_max_header_size "$PAGE")"
  assert_not_empty "reconcile_config.go renders a max_header_size" "$rendered"
  if [[ -z "$rendered" ]]; then
    return
  fi
  assert_eq "the page states the max_header_size reconcile_config.go renders" "$rendered" "$stated"

  assert_page_says "the page spells out the IPv4 overhead" \
    "$rendered + 20 = $((rendered + 20))"
  assert_file_contains_fixed "the page states the IPv4 tenant MTU" "$PAGE" \
    "| IPv4 | 9000 | 20 | $rendered | $((9000 - 20 - rendered)) |"
  assert_file_contains_fixed "the page states the IPv6 tenant MTU" "$PAGE" \
    "| IPv6 | 9000 | 40 | $rendered | $((9000 - 40 - rendered)) |"
  assert_page_says "the page states the IPv4 minimum node MTU" \
    "at least $((1500 + 20 + rendered)) bytes over an IPv4 underlay"
  assert_page_says "the page states the IPv6 minimum node MTU" \
    "or $((1500 + 40 + rendered)) over IPv6"

  # The comparison has to name both values when they part: render 39 in a
  # copy of the Go file and expect the page's 38 to stand out.
  local copy
  copy="$TMP_ROOT/reconcile_config.go"
  sed 's/"max_header_size": *"[0-9]*"/"max_header_size": "39"/' "$CONFIG_GO" >"$copy"
  assert_eq "a changed max_header_size in the Go file is read back" "39" \
    "$(rendered_max_header_size "$copy")"
  if [[ "$(rendered_max_header_size "$copy")" != "$stated" ]]; then
    echo "  PASS: a page that still says $stated is caught against a rendered 39"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the page says 39 too, so the copy proves nothing"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 3: the operator and upstream facts the page cites ---
test_page_cites_the_code_and_upstream() {
  echo "Test: the page cites the operator and neutron 29.0.0 facts, and the code still holds them"

  assert_file_contains_fixed "the page names ovn-encap-ip" "$PAGE" "ovn-encap-ip"
  assert_file_contains_fixed "the page names global_physnet_mtu" "$PAGE" "global_physnet_mtu"
  assert_file_contains_fixed "the page names connected-as-host" "$PAGE" "connected-as-host"
  assert_page_says "the page attributes the upstream facts to neutron 29.0.0" \
    "neutron 29.0.0"
  assert_file_contains_fixed "the page quotes the VLAN rejection" "$PAGE" \
    "VLAN provider networks are not supported when the BGP service plugin is enabled"
  assert_file_contains_fixed "the page quotes the second-flat-network rejection" "$PAGE" \
    "Only a single flat provider network is supported"
  assert_page_says "the page names spec.extraConfig as the lever" \
    "\`spec.extraConfig\` sets the option under \`[DEFAULT]\`"

  # What the page says the operators do.
  assert_file_contains_fixed "the chassis writes the node address as the encap IP" "$NODES_GO" \
    'external_ids:ovn-encap-ip="${NODE_IP}"'
  assert_file_contains "the Neutron operator renders type_drivers geneve,flat" "$CONFIG_GO" \
    '"type_drivers": *"geneve,flat"'
  assert_file_contains "the Neutron operator renders vni_ranges 1:65536" "$CONFIG_GO" \
    '"vni_ranges": *"1:65536"'
  # A rendered option is a string literal in the controller's own code (a map
  # key, an index assignment or a constant); a test that sets one through
  # extraConfig renders nothing.
  local option fake
  for option in global_physnet_mtu overlay_ip_version; do
    assert_eq "the Neutron operator renders no $option (the page relies on its default)" "" \
      "$(option_renderers "$option" "$PROJECT_ROOT/operators/neutron/internal/controller")"
  done

  # Both forms are caught and a test file is not: an index assignment, a map
  # key, and a _test.go file that sets the option.
  fake="$TMP_ROOT/controller"
  mkdir -p "$fake"
  printf '\tdefaults["DEFAULT"]["global_physnet_mtu"] = mtu\n' >"$fake/assign.go"
  printf '\t"overlay_ip_version": "6",\n' >"$fake/literal.go"
  printf '\t"global_physnet_mtu": "9000",\n' >"$fake/config_test.go"
  assert_eq "an index assignment of global_physnet_mtu is caught, its test file is not" \
    "$fake/assign.go" "$(option_renderers global_physnet_mtu "$fake")"
  assert_eq "a map key overlay_ip_version is caught" \
    "$fake/literal.go" "$(option_renderers overlay_ip_version "$fake")"
}

# --- Test 4: the code imports resolve ---
test_code_imports_resolve() {
  echo "Test: both code imports resolve to files under tests/lab/bgp-fabric/"

  local imports line target count=0
  imports="$(grep '^<<< @/../tests/lab/bgp-fabric/' "$PAGE" || true)"
  assert_contains "the page imports the topology" "$imports" "bgp-fabric.clab.yml"
  assert_contains "the page imports node1's FRR config" "$imports" "node1/frr.conf"
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    count=$((count + 1))
    # Strip the import prefix and a {lang}, {lines} or #region suffix.
    target="${line#<<< @/../tests/lab/bgp-fabric/}"
    target="${target%%\{*}"
    target="${target%%#*}"
    if [[ -f "$LAB_DIR/$target" ]]; then
      echo "  PASS: import $target exists"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: import $target does not exist under tests/lab/bgp-fabric/"
      FAIL=$((FAIL + 1))
    fi
  done <<<"$imports"
  assert_eq "the page has two code imports from the lab" "2" "$count"
}

# --- Test 5: sidebar and the pages that link the contract ---
test_page_is_listed_and_linked() {
  echo "Test: the sidebar lists the page and the four promising pages link it"

  assert_file_contains_fixed "config.ts lists the page" "$SIDEBAR" \
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
  record="$(section "$PAGE" "#### containerlab")"
  assert_not_empty "the page has a containerlab run record" "$record"
  for id in A1 A2 A3 A4 A5 A6; do
    assert_contains "the containerlab record has a row for $id" "$record" "| $id |"
  done
  assert_contains "the record names the containerlab version" "$record" "with containerlab "
  assert_contains "the record names the FRR image digest" "$record" "quay.io/frrouting/frr@sha256:"

  leaf="$(section "$PAGE" "#### Leaf pair")"
  assert_contains "the leaf-pair record states that no run was taken" "$leaf" \
    "No acceptance run has been recorded yet."
}

# --- Run ---
test_page_opens_with_frontmatter
test_max_header_size_matches_the_operator
test_page_cites_the_code_and_upstream
test_code_imports_resolve
test_page_is_listed_and_linked
test_run_records

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
