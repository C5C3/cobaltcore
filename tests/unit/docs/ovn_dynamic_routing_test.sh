#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify docs/reference/networking/ovn-dynamic-routing.md stays in step with the
# suite it imports and the OVN options it documents:
#   - the page opens with frontmatter carrying title: and quadrant:, the
#     sidebar lists it after BGP Node Substrate, and its six sections come in
#     the documented order
#   - its one code import is page-relative and resolves to routers.sh of the
#     fabric-advertised-route suite
#   - every router option and every port option the page documents has a row
#     of its section's table, and every option routers.sh sets is documented
#   - the five values of dynamic-routing-redistribute are named
#
# Runs under Bash 3.2.
#
# Usage: bash tests/unit/docs/ovn_dynamic_routing_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

PAGE="$PROJECT_ROOT/docs/reference/networking/ovn-dynamic-routing.md"
PAGE_DIR="$(dirname "$PAGE")"
SIDEBAR="$PROJECT_ROOT/docs/.vitepress/sidebar.ts"
ROUTERS="$PROJECT_ROOT/tests/e2e-ovn-overlay/fabric-advertised-route/routers.sh"

if [[ ! -f "$PAGE" ]]; then
  echo "FAIL: $PAGE does not exist"
  exit 1
fi

# section <heading>: the lines after the heading line up to the next heading
# of the same or a higher level.
section() {
  awk -v h="$1" '
    $0 == h { on = 1; level = index(h, " "); next }
    on && /^#+ / && index($0, " ") <= level { exit }
    on { print }
  ' "$PAGE"
}

# table_row_names <heading>: the backticked first cell of every table row of
# the section, one per line.
table_row_names() {
  section "$1" | awk -F'|' '/^\| `/ { gsub(/[ `]/, "", $2); print $2 }'
}

test_page_opens_with_frontmatter_and_is_listed() {
  echo "Test: frontmatter, sidebar entry and section order"

  local front sections
  front="$(head -4 "$PAGE")"
  assert_contains "frontmatter starts on line 1 and carries the title" \
    "$front" $'---\ntitle: OVN Dynamic Routing'
  assert_contains "frontmatter carries the quadrant" "$front" "quadrant: operator"
  assert_file_contains_fixed "sidebar.ts lists the page" "$SIDEBAR" \
    "{ text: 'OVN Dynamic Routing', link: '/reference/networking/ovn-dynamic-routing' }"
  assert_eq "the page follows BGP Node Substrate in the sidebar" \
    "OVN Dynamic Routing" \
    "$(grep -A1 "text: 'BGP Node Substrate'" "$SIDEBAR" | tail -1 | sed -e "s/.*text: '//" -e "s/'.*//")"

  sections="$(grep '^## ' "$PAGE" | paste -sd'|' -)"
  assert_eq "the six sections come in order" \
    "## Who sets what|## Router options|## Port options|## What ovn-controller does on a chassis|## Worked example|## Observing" \
    "$sections"
}

test_code_import_resolves() {
  echo "Test: the code import is page-relative and resolves to routers.sh"

  local imports target
  imports="$(grep '^<<< ' "$PAGE" || true)"
  assert_eq "the page has one code import" "1" "$(grep -c . <<<"$imports" || true)"
  assert_file_not_contains "no import starts with @/, which cobaltcore.dev cannot resolve" "$PAGE" '^<<< @/'
  target="${imports#<<< }"
  if [[ -f "$PAGE_DIR/$target" && "$(cd "$PAGE_DIR" && cd "$(dirname "$target")" && pwd)/$(basename "$target")" == "$ROUTERS" ]]; then
    echo "  PASS: import $target resolves to the suite's routers.sh"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: import $target does not resolve to $ROUTERS"
    FAIL=$((FAIL + 1))
  fi
}

test_every_option_has_a_row() {
  echo "Test: every documented option has a row, and every option routers.sh sets is documented"

  local router_rows port_rows option
  router_rows="$(table_row_names '## Router options')"
  port_rows="$(table_row_names '## Port options')"
  for option in dynamic-routing dynamic-routing-redistribute dynamic-routing-redistribute-local-only \
    dynamic-routing-vrf-id dynamic-routing-vrf-name dynamic-routing-no-learning \
    dynamic-routing-v4-prefix-nexthop dynamic-routing-v6-prefix-nexthop; do
    assert_eq "the router options table has a row for $option" "1" \
      "$(grep -cx -- "$option" <<<"$router_rows" || true)"
  done
  for option in dynamic-routing-maintain-vrf dynamic-routing-port-name dynamic-routing-redistribute \
    dynamic-routing-no-learning routing-protocol-redirect routing-protocols; do
    assert_eq "the port options table has a row for $option" "1" \
      "$(grep -cx -- "$option" <<<"$port_rows" || true)"
  done

  # Every option routers.sh writes is one a reader of the example can look up.
  local used
  used="$(grep -oE 'options:[a-z-]+' "$ROUTERS" | sed 's/^options://' | sort -u | grep -v '^chassis$')"
  assert_not_empty "routers.sh sets dynamic routing options" "$used"
  while IFS= read -r option; do
    if grep -qx -- "$option" <<<"$router_rows"$'\n'"$port_rows"; then
      echo "  PASS: $option, which routers.sh sets, has a row"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $option, which routers.sh sets, has no row"
      FAIL=$((FAIL + 1))
    fi
  done <<<"$used"
}

test_redistribute_values_are_named() {
  echo "Test: the five values of dynamic-routing-redistribute are named"

  local row value
  row="$(section '## Router options' | grep '^| `dynamic-routing-redistribute` |')"
  for value in connected connected-as-host static nat lb; do
    assert_contains "the redistribute row names \`$value\`" "$row" "\`$value\`"
  done
}

# A table row check that cannot fail is no check: drop a row from a copy and
# the extraction has to miss it.
test_row_extraction_sees_a_missing_row() {
  echo "Test: the row extraction misses a row a copy of the page drops"

  local copy
  copy="$(mktemp)"
  grep -v '^| `dynamic-routing-vrf-name` |' "$PAGE" >"$copy"
  assert_eq "a copy without the vrf-name row has none" "0" \
    "$(PAGE="$copy" table_row_names '## Router options' | grep -cx 'dynamic-routing-vrf-name' || true)"
  rm -f "$copy"
}

test_page_opens_with_frontmatter_and_is_listed
test_code_import_resolves
test_every_option_has_a_row
test_redistribute_values_are_named
test_row_extraction_sees_a_missing_row

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
