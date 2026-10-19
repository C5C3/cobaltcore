#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the `#### Lab failover runs` subsection of
# docs/reference/infrastructure/infrastructure-manifests.md (#1393) and the
# pages beside it:
#   1. the heading occurs once, after `#### Lab fault runs` and before
#      `### Lab dizzy stack`
#   2. its scenario table names the suites of hack/lab-failover.sh, in the
#      script's order
#   3. it names the four make targets, the five variables, and that down runs
#      before make teardown-infra
#   4. it holds the sentence that no run is recorded or a line that starts
#      with `The run of 20YY-MM-DD`, not both and not neither. With the
#      sentence, it has no result table and every suite's measured budget line
#      reads `estimate`; with the record, the result table has one row per
#      suite in the script's order with PASS or FAIL, every FAIL row names an
#      issue, and every suite's measured budget line reads `lab`
#   5. docs/reference/testing/chaos-e2e-tests.md names tests/lab-failover/ and
#      links the subsection
#   6. the capacity sentences of the Lab ControlPlane table and of
#      controlplane-lab.yaml name forge at 128 GiB and newforge at 29.9 GiB
#      allocatable
#
# Subsections run from their heading to the next heading of any level, fenced
# code included. INFRA_MANIFESTS_DOC overrides the page.
#
# Usage: bash tests/unit/docs/infrastructure_manifests_lab_failover_runs_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

DOC="${INFRA_MANIFESTS_DOC:-$PROJECT_ROOT/docs/reference/infrastructure/infrastructure-manifests.md}"
CHAOS_DOC="$PROJECT_ROOT/docs/reference/testing/chaos-e2e-tests.md"
LAB_CR="$PROJECT_ROOT/deploy/lab/metal-stack/controlplane/controlplane-lab.yaml"
LF_SH="$PROJECT_ROOT/hack/lab-failover.sh"
LF_DIR="$PROJECT_ROOT/tests/lab-failover"

PLACEHOLDER="No lab run of the failover suites is recorded yet."
RESULT_HEADER="| Suite | Verdict | Observed failures (worst stream) | Observed recovery | Component back after | Finding or issue |"

if [[ ! -f "$DOC" ]]; then
  echo "FAIL: $DOC does not exist"
  exit 1
fi

# headings prints "<line>:<text>" for every heading outside fenced code.
headings() {
  awk '
    /^[[:space:]]*(```|~~~)/ { fenced = !fenced; next }
    !fenced && /^#+ / { print NR ":" $0 }
  ' "$DOC"
}

# subsection prints the lines below the heading $1, fenced code included, up
# to the next heading of any level outside fenced code.
subsection() {
  awk -v heading="$1" '
    /^[[:space:]]*(```|~~~)/ { fenced = !fenced }
    !fenced && /^#+ / { if (inside) exit; if ($0 == heading) { inside = 1; next } }
    inside { print }
  ' "$DOC"
}

# line_of_heading <heading> — the line of the first heading with that text.
line_of_heading() {
  { grep -xE "[0-9]+:$1" <<<"$(headings)" || true; } | head -n 1 | cut -d: -f1
}

# scenarios — the LF_SCENARIOS of hack/lab-failover.sh, one per line.
scenarios() {
  awk '/^LF_SCENARIOS=\($/ { s = 1; next } s && /^\)$/ { exit } s { gsub(/[[:space:]]/, ""); print }' "$LF_SH"
}

# table_rows <text> <header> — the rows of the table under <header>, without
# the separator line.
table_rows() {
  awk -v header="$2" '
    $0 == header { t = 1; getline; next }
    t && /^\|/ { print; next }
    t { exit }
  ' <<<"$1"
}

# first_cells <rows> — the first cell of each row, backticks dropped.
first_cells() {
  awk -F'|' '{ c = $2; gsub(/[` ]/, "", c); print c }' <<<"$1"
}

# record_state <text> — placeholder, record, both or neither.
record_state() {
  local placeholder record
  placeholder="$(grep -cxF "$PLACEHOLDER" <<<"$1" || true)"
  record="$(grep -cE '^The run of 20[0-9]{2}-[0-9]{2}-[0-9]{2}' <<<"$1" || true)"
  if [[ "$placeholder" != "0" && "$record" != "0" ]]; then
    echo both
  elif [[ "$placeholder" != "0" ]]; then
    echo placeholder
  elif [[ "$record" != "0" ]]; then
    echo record
  else
    echo neither
  fi
}

RUNS="$(subsection '#### Lab failover runs')"
SCENARIOS="$(scenarios)"

# --- Test 1: the subsection's place ---
test_position() {
  echo "Test: '#### Lab failover runs' occurs once, between the fault runs and the dizzy stack"
  local count line faults dizzy
  count="$(grep -cxE '[0-9]+:#### Lab failover runs' <<<"$(headings)" || true)"
  assert_eq "'#### Lab failover runs' occurs once" "1" "$count"
  line="$(line_of_heading '#### Lab failover runs')"
  faults="$(line_of_heading '#### Lab fault runs')"
  dizzy="$(line_of_heading '### Lab dizzy stack')"
  if [[ -n "$line" && -n "$faults" && -n "$dizzy" ]] && ((faults < line && line < dizzy)); then
    echo "  PASS: the subsection sits on line $line, between line $faults and line $dizzy"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the subsection is missing or out of place (fault runs '${faults}', failover runs '${line}', dizzy stack '${dizzy}')"
    FAIL=$((FAIL + 1))
  fi
  assert_not_empty "the subsection has a body" "$RUNS"
}

# --- Test 2: the scenario table ---
test_scenario_table() {
  echo "Test: the scenario table names the suites of hack/lab-failover.sh in its order"
  local rows
  assert_eq "the script names eleven scenarios" "11" "$(grep -c . <<<"$SCENARIOS")"
  rows="$(table_rows "$RUNS" "| Suite | Target | Fault | Streams | Fault check in the window | Recovery check, bound from fault-end | Estimated budget |")"
  assert_eq "the table names the scenarios in the script's order" "$SCENARIOS" "$(first_cells "$rows")"
  local suite missing=""
  while IFS= read -r suite; do
    [[ -d "$LF_DIR/$suite" ]] || missing="${missing} ${suite}"
  done <<<"$SCENARIOS"
  assert_eq "every scenario has its suite directory" "" "$missing"
}

# --- Test 3: the runbook ---
test_runbook() {
  echo "Test: the subsection names the make targets, the variables and the teardown order"
  local item
  for item in lab-failover-preflight lab-failover-up lab-failover-run lab-failover-down; do
    assert_contains "it names make $item" "$RUNS" "make $item"
  done
  for item in LAB_FAILOVER_CONTEXT LAB_FAILOVER_GO_AHEAD LAB_FAILOVER_MIN_FREE_MEMORY LAB_FAILOVER_RUN \
    LAB_FAILOVER_SCENARIOS; do
    assert_contains "the variables table has $item" "$RUNS" "| \`$item\` |"
  done
  assert_contains "down runs before make teardown-infra" "$(tr '\n' ' ' <<<"$RUNS")" \
    'so run `make lab-failover-down` before `EXTERNAL_CLUSTER=true make teardown-infra`'
  assert_contains "the floor's default is 38Gi" "$RUNS" '| `LAB_FAILOVER_MIN_FREE_MEMORY` | `38Gi` |'
  assert_contains "the issue's outputs go to #1393" "$RUNS" "[#1393](https://github.com/c5c3/cobaltcore/issues/1393)"
}

# --- Test 4: the record ---
test_record() {
  echo "Test: the subsection holds the placeholder or the record, and the budget lines agree"
  local state rows suite env wrong=""
  assert_eq "a text with both is refused" "both" \
    "$(record_state "$(printf '%s\n' "$PLACEHOLDER" 'The run of 2026-10-20 ran the eleven suites.')")"
  assert_eq "a text with neither is refused" "neither" "$(record_state 'The run is pending.')"
  state="$(record_state "$RUNS")"
  if [[ "$state" == "placeholder" || "$state" == "record" ]]; then
    echo "  PASS: the subsection holds the $state"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the subsection holds $state of the placeholder and a 'The run of YYYY-MM-DD' line"
    FAIL=$((FAIL + 1))
  fi
  rows="$(table_rows "$RUNS" "$RESULT_HEADER")"
  if [[ "$state" == "record" ]]; then
    env=lab
    assert_eq "the result table names the scenarios in the script's order" "$SCENARIOS" "$(first_cells "$rows")"
    assert_eq "every row's verdict is PASS or FAIL" "" \
      "$(awk -F'|' '{ v = $3; gsub(/[` ]/, "", v); if (v != "PASS" && v != "FAIL") print $2 }' <<<"$rows")"
    assert_eq "every FAIL row names an issue" "" \
      "$(awk -F'|' '{ v = $3; gsub(/[` ]/, "", v); if (v == "FAIL" && $7 !~ /issues\/[0-9]+/) print $2 }' <<<"$rows")"
  else
    env=estimate
    assert_eq "without a record there is no result table" "" "$rows"
  fi
  while IFS= read -r suite; do
    grep -qE "^# Chaos budget measured: [0-9]{4}-[0-9]{2}-[0-9]{2}, ${env}, " "$LF_DIR/$suite/chainsaw-test.yaml" ||
      wrong="${wrong} ${suite}"
  done <<<"$SCENARIOS"
  assert_eq "every suite's measured budget line reads ${env}" "" "$wrong"
}

# --- Test 5: the chaos reference ---
test_chaos_page() {
  echo "Test: the chaos reference names the lab failover suites and links the subsection"
  assert_file_contains_fixed "it names tests/lab-failover/" "$CHAOS_DOC" 'The suites under `tests/lab-failover/` are lab-only'
  assert_file_contains_fixed "it links the subsection" "$CHAOS_DOC" \
    '[Lab failover runs](../infrastructure/infrastructure-manifests.md#lab-failover-runs)'
}

# --- Test 6: the capacity sentences ---
test_capacity() {
  echo "Test: both capacity sentences name forge and newforge"
  local row comment
  row="$(grep -F '| `spec.sizing.profile` | `Minimal` |' "$DOC" || true)"
  assert_contains "the Lab ControlPlane row names forge at 128 GiB" "$row" 'a worker of `forge` with 16 CPUs and 128 GiB'
  assert_contains "the Lab ControlPlane row names newforge at 29.9 GiB" "$row" \
    'a worker of `newforge` with 15.9 CPUs and 29.9 GiB allocatable'
  comment="$(awk '/^  # Lab setting, identical to the quick start\./ { c = 1 } c && /^  [^#]/ { exit } c' "$LAB_CR" | tr '\n' ' ')"
  assert_contains "the lab CR names forge at 128 GiB" "$comment" "a worker of forge   # offers 16 CPUs and 128 GiB"
  assert_contains "the lab CR names newforge at 29.9 GiB" "$comment" "a worker of newforge 15.9 CPUs and 29.9 GiB   # allocatable"
}

test_position
test_scenario_table
test_runbook
test_record
test_chaos_page
test_capacity

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
