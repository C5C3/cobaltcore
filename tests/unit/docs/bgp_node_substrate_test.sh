#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify docs/reference/networking/bgp-node-substrate.md stays in step with the
# overlay, the suite and the lab it cites:
#   - the page opens with frontmatter carrying title: and quadrant:, the
#     sidebar lists it, and its sections come in the documented order
#   - the three code imports are page-relative and resolve: the two
#     FRRConfigurations of the suite and the lab node's FRR configuration
#   - the page records D1 to D5, quotes upstream's warning on the raw snippet
#     and names the four outbound lists the snippet widens
#   - the suite table has a row per step S1 to S9, the hardware table a row
#     per check H1 to H4, and the hardware record states that no run was
#     taken
#   - the chart version, both image pins of release.yaml and the FRR pin of
#     the lab appear on the page; a copy of release.yaml with another tag
#     proves the comparison names the stale pin
#   - the contract page links the page and no longer says that its CI job
#     belongs to a later phase
#
# Runs under Bash 3.2.
#
# Usage: bash tests/unit/docs/bgp_node_substrate_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

PAGE="$PROJECT_ROOT/docs/reference/networking/bgp-node-substrate.md"
PAGE_DIR="$(dirname "$PAGE")"
CONTRACT="$PROJECT_ROOT/docs/reference/networking/bgp-fabric-contract.md"
SIDEBAR="$PROJECT_ROOT/docs/.vitepress/sidebar.ts"
RELEASE="$PROJECT_ROOT/deploy/overlays/kind/frr-k8s/release.yaml"
CLAB="$PROJECT_ROOT/tests/lab/bgp-fabric/bgp-fabric.clab.yml"

if [[ ! -f "$PAGE" ]]; then
  echo "FAIL: $PAGE does not exist"
  exit 1
fi

# The page with its line breaks folded, for phrases a rewrap may split.
PAGE_FLAT="$(tr '\n' ' ' <"$PAGE")"

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# assert_page_says <description> <phrase>: the folded page contains <phrase>.
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

# section <heading>: the lines after the heading line up to the next heading
# of the same or a higher level.
section() {
  awk -v h="$1" '
    $0 == h { on = 1; level = index(h, " "); next }
    on && /^#+ / && index($0, " ") <= level { exit }
    on { print }
  ' "$PAGE"
}

# check_pins <page> <release> <clab>: prints one line per pin of <release>
# (the chart version and each repository:tag of its values) and <clab> (the
# FRR image) that <page> does not name; nothing when the page is current.
check_pins() {
  local page="$1" release="$2" clab="$3" pin
  {
    awk '/^      version: / { gsub(/"/, "", $2); print $2; exit }' "$release"
    awk '
      /^[[:space:]]+repository: / { repo = $2 }
      /^[[:space:]]+tag: / && repo != "" { tag = $2; gsub(/"/, "", tag); print repo ":" tag; repo = "" }
    ' "$release"
    awk '/^[[:space:]]+image: quay\.io\/frrouting\/frr:/ { print $2; exit }' "$clab"
  } | while IFS= read -r pin; do
    if ! grep -qF -- "$pin" "$page"; then
      echo "not on the page: $pin"
    fi
  done
}

test_page_opens_with_frontmatter_and_is_listed() {
  echo "Test: frontmatter, sidebar entry and section order"

  local front
  front="$(head -n 4 "$PAGE")"
  assert_contains "frontmatter starts on line 1 and carries the title" \
    "$front" "---
title: BGP Node Substrate"
  assert_contains "frontmatter carries the quadrant" "$front" "quadrant: operator"
  assert_file_contains_fixed "sidebar.ts lists the page" "$SIDEBAR" \
    "{ text: 'BGP Node Substrate', link: '/reference/networking/bgp-node-substrate' }"

  local want got
  want="Decisions|Install|Node prerequisites|The per-node FRRConfiguration|What the typed API cannot say|Nodes with a host FRR|Observing|What the suite proves|Hardware acceptance|Pins|See also"
  got="$(sed -n 's/^## //p' "$PAGE" | paste -sd'|' -)"
  assert_eq "the page has its eleven sections in order" "$want" "$got"
}

test_code_imports_resolve() {
  echo "Test: the three code imports are page-relative and resolve"

  local line target count=0
  assert_file_not_contains "no import starts with @/, which cobaltcore.dev cannot resolve" "$PAGE" '^<<< @/'
  while IFS= read -r line; do
    count=$((count + 1))
    target="${line#<<< }"
    target="${target%%\{*}"
    target="${target%"${target##*[![:space:]]}"}"
    if [[ "$target" == ../* && -f "$PAGE_DIR/$target" ]]; then
      echo "  PASS: import $target is page-relative and exists"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: import $target is not a page-relative path to an existing file"
      FAIL=$((FAIL + 1))
    fi
  done < <(grep '^<<< ' "$PAGE" || true)
  assert_eq "the page has three code imports" "3" "$count"
  assert_file_contains_fixed "the page imports the per-node FRRConfiguration" "$PAGE" \
    "<<< ../../../tests/e2e-bgp-fabric/frr-k8s-fabric-peering/03-frrconfiguration.yaml"
  assert_file_contains_fixed "the page imports the raw snippet" "$PAGE" \
    "<<< ../../../tests/e2e-bgp-fabric/frr-k8s-fabric-peering/04-frrconfiguration-kernel-routes.yaml"
  assert_file_contains_fixed "the page imports the lab node's FRR configuration" "$PAGE" \
    "<<< ../../../tests/lab/bgp-fabric/node1/frr.conf"
}

test_decisions_and_raw_snippet() {
  echo "Test: D1 to D5, upstream's warning and the generated list names"

  local d
  for d in "D1, speaker." "D2, ownership." "D3, nodes that already run FRR." "D4, placement." "D5, hardware."; do
    assert_contains "the decisions record $d" "$(section "## Decisions")" "**$d**"
  done
  assert_page_says "the page quotes upstream's warning verbatim" \
    "\"The RawConfig feature is UNSUPPORTED and intended ONLY FOR EXPERIMENTATION\""
  local list
  for list in fab1-fabric-allowed-ipv4 fab2-fabric-allowed-ipv4 fab1-fabric-allowed-ipv6 fab2-fabric-allowed-ipv6; do
    assert_page_says "the page names the generated list $list" "\`$list\`"
  done
  assert_eq "the page has one callout box" "1" "$(grep -c '^::: ' "$PAGE")"
}

test_tables_and_record() {
  echo "Test: the suite table S1 to S9, the hardware table H1 to H4 and an empty record"

  local suite hardware record id
  suite="$(section "## What the suite proves")"
  for id in S1 S2 S3 S4 S5 S6 S7 S8 S9; do
    assert_contains "the suite table has a row for $id" "$suite" "| $id |"
  done
  hardware="$(section "## Hardware acceptance")"
  for id in H1 H2 H3 H4; do
    assert_contains "the hardware table has a row for $id" "$hardware" "| $id |"
  done
  record="$(section "### Hardware record")"
  assert_contains "the hardware record states that no run was taken" "$record" \
    "No hardware run has been recorded yet."
  assert_eq "the hardware record holds no table" "" "$(grep '^|' <<<"$record" || true)"
}

test_pins_match_the_manifests() {
  echo "Test: the page names the pins of release.yaml and of the lab"

  assert_eq "every pin of release.yaml and the lab is on the page" "" \
    "$(check_pins "$PAGE" "$RELEASE" "$CLAB")"

  # A release.yaml with a newer controller tag proves check_pins reads the
  # file and names the pin the page lacks.
  sed 's/tag: "v0.0.27@sha256:[0-9a-f]*"/tag: "v0.0.28@sha256:0000000000000000000000000000000000000000000000000000000000000000"/' \
    "$RELEASE" >"$TMP_ROOT/release.yaml"
  assert_eq "a changed controller tag is reported" \
    "not on the page: quay.io/metallb/frr-k8s:v0.0.28@sha256:0000000000000000000000000000000000000000000000000000000000000000" \
    "$(check_pins "$PAGE" "$TMP_ROOT/release.yaml" "$CLAB")"
}

test_contract_page_links_the_page() {
  echo "Test: the contract page links the page and drops the deferral"

  assert_eq "the contract page links bgp-node-substrate.md" "true" \
    "$(grep -q 'bgp-node-substrate\.md' "$CONTRACT" && echo true || echo false)"
  assert_file_not_contains "the contract page no longer defers its CI job" "$CONTRACT" \
    "that job belongs to #1362"
}

test_page_opens_with_frontmatter_and_is_listed
test_code_imports_resolve
test_decisions_and_raw_snippet
test_tables_and_record
test_pins_match_the_manifests
test_contract_page_links_the_page

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
