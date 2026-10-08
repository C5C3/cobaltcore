#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the chaos page and the e2e-chaos matrix name every chaos suite.
#
# A chaos suite is a directory directly under tests/e2e-chaos/ that holds a
# chainsaw-test.yaml. docs/reference/testing/chaos-e2e-tests.md lists every
# suite in the table under `## Test Suite Inventory` and describes it in a
# `### <suite>` section. The matrix of the e2e-chaos job in
# .github/workflows/ci.yaml runs every suite in exactly one entry: a line
# `- suite: <leg>` starts an entry, and the `tests/e2e-chaos/<suite>` tokens
# on the lines below it are its suites. check_chaos_suites prints one problem
# per line and nothing for a consistent tree. The fixture tests prove each
# check on a scratch tree before the last test runs it on the repository.
#
# Usage: bash tests/unit/docs/chaos_suite_inventory_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

# Read the body of the e2e-chaos job, as job_block prints it, and print one
# line per suite of its matrix: the leg, a tab and the suite. The matrix
# starts at the first `- suite:` line and ends at the first line after it,
# other than a blank or a comment line, that is indented by four spaces or
# fewer.
# shellcheck disable=SC2016 # an awk program, not shell
MATRIX_AWK='
{
  line = $0
  if (line ~ /^[ \t]*$/ || line ~ /^[ \t]*#/) next
  if (match(line, /^[ \t]*- suite:[ \t]*/)) {
    leg = substr(line, RSTART + RLENGTH)
    sub(/[ \t]*(#.*)?$/, "", leg)
    next
  }
  if (leg == "") next
  if (line ~ /^ ? ? ? ?[^ ]/) exit
  while (match(line, /tests\/e2e-chaos\/[A-Za-z0-9_.-]+/)) {
    suite = substr(line, RSTART, RLENGTH)
    sub(/^tests\/e2e-chaos\//, "", suite)
    print leg "\t" suite
    line = substr(line, RSTART + RLENGTH)
  }
}
'

# Print the suites of the inventory table: a line "#table" once the header
# row, which starts with header, is found, then the first cell of every row,
# the link text where it is a link.
# shellcheck disable=SC2016 # an awk program, not shell
ROWS_AWK='
$0 == "## Test Suite Inventory" { after = 1; next }
after && !in_table && index($0, header) == 1 { in_table = 1; print "#table"; next }
in_table {
  if (substr($0, 1, 1) != "|") exit
  if ($0 ~ /^\|[ \t:|-]+$/) next
  split($0, cells, "|")
  name = cells[2]
  gsub(/^[ \t]+/, "", name)
  gsub(/[ \t]+$/, "", name)
  if (substr(name, 1, 1) == "[" && index(name, "]") > 0) name = substr(name, 2, index(name, "]") - 2)
  print name
}
'

# Compare SUITES, LEGS, ROWS and SECTIONS from the environment.
# shellcheck disable=SC2016 # an awk program, not shell
COMPARE_AWK='
BEGIN {
  n = split(ENVIRON["SUITES"], suites, "\n")
  for (i = 1; i <= n; i++) suite[suites[i]] = 1
  m = split(ENVIRON["LEGS"], lines, "\n")
  for (i = 1; i <= m; i++) {
    split(lines[i], f, "\t")
    if (!(f[2] in suite)) { print "leg lists a missing suite: " f[1] ": " f[2]; continue }
    if ((f[2] "|" f[1]) in seen) continue
    seen[f[2] "|" f[1]] = 1
    if (count[f[2]] > 0) legs[f[2]] = legs[f[2]] ", " f[1]
    else legs[f[2]] = f[1]
    count[f[2]]++
  }
  r = split(ENVIRON["ROWS"], lines, "\n")
  for (i = 1; i <= r; i++) {
    if (lines[i] == "#table") continue
    row[lines[i]] = 1
    if (!(lines[i] in suite)) print "row without a suite: " lines[i]
  }
  s = split(ENVIRON["SECTIONS"], lines, "\n")
  for (i = 1; i <= s; i++) section[lines[i]] = 1
  for (i = 1; i <= n; i++) {
    name = suites[i]
    if (!(name in row)) print "suite without a row: " name
    if (!(name in section)) print "suite without a section: " name
    if (!(name in count)) print "suite in no leg: " name
    else if (count[name] > 1) print "suite in two legs: " name ": " legs[name]
  }
}
'

# Print one line per sentence of the page that counts the suites wrongly.
# shellcheck disable=SC2016 # an awk program, not shell
COUNT_AWK='
{
  line = $0
  while (match(line, /[0-9]+ (chaos )?test suites/)) {
    said = substr(line, RSTART, RLENGTH)
    sub(/ .*$/, "", said)
    if (said + 0 != want + 0)
      printf "%s:%d: says %s suites, the tree has %d\n", page, FNR, said, want
    line = substr(line, RSTART + RLENGTH)
  }
}
'

# check_chaos_suites <suites-dir> <ci.yaml> <page>: print one problem per
# line, sorted, and nothing when the matrix, the inventory and the sections
# name every suite. A directory without a suite, a missing workflow, a
# workflow without the e2e-chaos matrix, a missing page and a page without
# the inventory table are each the only line.
check_chaos_suites() {
  local dir="$1" workflow="$2" page="$3"

  local suites="" d
  if [[ -d "$dir" ]]; then
    for d in "$dir"/*/; do
      [[ -f "${d}chainsaw-test.yaml" ]] || continue
      d="${d%/}"
      suites="$suites${suites:+$'\n'}${d##*/}"
    done
  fi
  if [[ -z "$suites" ]]; then
    echo "no suite under $dir"
    return
  fi
  if [[ ! -f "$workflow" ]]; then
    echo "missing $workflow"
    return
  fi
  local legs
  legs="$(CI_YAML="$workflow" job_block e2e-chaos | awk "$MATRIX_AWK")"
  if [[ -z "$legs" ]]; then
    echo "no e2e-chaos matrix in $workflow"
    return
  fi
  if [[ ! -f "$page" ]]; then
    echo "missing $page"
    return
  fi
  local rows
  rows="$(awk -v header='| Suite |' "$ROWS_AWK" "$page")"
  if [[ -z "$rows" ]]; then
    echo "no inventory table in $page"
    return
  fi
  local sections
  sections="$(sed -n 's/^### //p' "$page")"

  {
    SUITES="$suites" LEGS="$legs" ROWS="$rows" SECTIONS="$sections" awk "$COMPARE_AWK" </dev/null
    awk -v page="$page" -v want="$(wc -l <<<"$suites")" "$COUNT_AWK" "$page"
  } | LC_ALL=C sort
}

# ---------------------------------------------------------------------------
# Fixture
# ---------------------------------------------------------------------------

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# build_fixture: write a consistent tree and print its root. suites/ holds
# three suites. The workflow runs them in two legs, with a comment line that
# names a suite, a token outside tests/e2e-chaos/ and a comment after the
# matrix. The page counts the suites, lists them in a table whose first
# cells are links or plain text, and gives each a section.
build_fixture() {
  local root s
  root="$(mktemp -d "$TMP_ROOT/fixture.XXXXXX")"
  for s in alpha-pod-kill beta-network-partition gamma-pod-kill; do
    mkdir -p "$root/suites/$s"
    echo "kind: Test" >"$root/suites/$s/chainsaw-test.yaml"
  done
  cat >"$root/ci.yaml" <<'EOF'
name: CI
on:
  pull_request:
jobs:
  changes:
    runs-on: ubuntu-latest
  e2e-chaos:
    needs: [changes]
    strategy:
      matrix:
        include:
          # tests/e2e-chaos/gamma-pod-kill moved to the pod entry
          - suite: pod
            test_dirs: >-
              tests/e2e/infrastructure/chaos-mesh-health
              tests/e2e-chaos/alpha-pod-kill

              tests/e2e-chaos/gamma-pod-kill
          - suite: network
            test_dirs: >-
              tests/e2e-chaos/beta-network-partition
    runs-on: ${{ matrix.runner }}
    # tests/e2e-chaos/chainsaw-config.yaml sets parallel: 1
    steps:
      - run: chainsaw test tests/e2e-chaos/alpha-pod-kill
  tempest:
    runs-on: ubuntu-latest
EOF
  cat >"$root/page.md" <<'EOF'
# Chaos E2E Test Suites

The 3 chaos test suites validate operator behavior.

## Test Suite Inventory

| Suite | Scenario ID | CR Name |
| --- | --- | --- |
| [alpha-pod-kill](#alpha-pod-kill) | — | `alpha` |
| beta-network-partition | — | `beta` |
| [gamma-pod-kill](#gamma-pod-kill) | — | `gamma` |

## Test Suite Details

### alpha-pod-kill

### beta-network-partition

### gamma-pod-kill
EOF
  printf '%s\n' "$root"
}

# rewrite <file> <sed-script>: apply a sed script in place (BSD and GNU sed).
rewrite() {
  sed "$2" "$1" >"$1.tmp" && mv "$1.tmp" "$1"
}

# check <root>: run check_chaos_suites on a fixture tree.
check() {
  check_chaos_suites "$1/suites" "$1/ci.yaml" "$1/page.md"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_consistent_fixture_reports_nothing() {
  echo "Test: a page and a matrix that name every suite report nothing"

  local root
  root="$(build_fixture)"
  assert_eq "no problems" "" "$(check "$root")"
}

test_directory_without_a_suite_is_the_only_report() {
  echo "Test: a suites directory that is missing or holds no suite is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/page.md"
  assert_eq "absent directory" "no suite under $root/absent" \
    "$(check_chaos_suites "$root/absent" "$root/ci.yaml" "$root/page.md")"

  rm "$root"/suites/*/chainsaw-test.yaml
  assert_eq "directories without chainsaw-test.yaml" "no suite under $root/suites" "$(check "$root")"
}

test_missing_workflow_is_the_only_report() {
  echo "Test: a workflow path that is not a file is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/page.md"
  assert_eq "absent path" "missing $root/absent.yaml" \
    "$(check_chaos_suites "$root/suites" "$root/absent.yaml" "$root/page.md")"
  assert_eq "a directory" "missing $root/suites" \
    "$(check_chaos_suites "$root/suites" "$root/suites" "$root/page.md")"
}

test_workflow_without_the_matrix_is_the_only_report() {
  echo "Test: a workflow without the e2e-chaos matrix is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/page.md"
  rewrite "$root/ci.yaml" 's/^  e2e-chaos:$/  e2e-other:/'
  assert_eq "no e2e-chaos job" "no e2e-chaos matrix in $root/ci.yaml" "$(check "$root")"

  root="$(build_fixture)"
  rm "$root/page.md"
  rewrite "$root/ci.yaml" '/- suite:/d'
  assert_eq "a job without a leg" "no e2e-chaos matrix in $root/ci.yaml" "$(check "$root")"
}

test_missing_page_is_the_only_report() {
  echo "Test: a page that does not exist is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/page.md"
  rm -r "$root/suites/gamma-pod-kill"
  assert_eq "absent page" "missing $root/page.md" "$(check "$root")"
}

test_page_without_the_table_is_the_only_report() {
  echo "Test: a page without the inventory table is the only report"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" '/^| Suite |/d'
  rewrite "$root/page.md" '/^### gamma-pod-kill$/d'
  assert_eq "no header row" "no inventory table in $root/page.md" "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/page.md" '/^## Test Suite Inventory$/d'
  assert_eq "a table outside the section" "no inventory table in $root/page.md" "$(check "$root")"
}

test_suite_without_a_row() {
  echo "Test: a suite without a row is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" '/^| \[gamma-pod-kill\]/d'
  assert_eq "the row of gamma-pod-kill removed" "suite without a row: gamma-pod-kill" "$(check "$root")"
}

test_row_without_a_suite() {
  echo "Test: a row whose suite has no directory is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's/^| beta-network-partition |.*$/&\
| [delta-pod-kill](#delta-pod-kill) | — | `delta` |/'
  assert_eq "a row of delta-pod-kill added" "row without a suite: delta-pod-kill" "$(check "$root")"
}

test_first_cell_without_a_link() {
  echo "Test: a first cell that is no link counts by its text"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's/^| beta-network-partition |/| beta-network |/'
  assert_eq "plain cell text that names no suite" \
    "row without a suite: beta-network"$'\n'"suite without a row: beta-network-partition" \
    "$(check "$root")"
}

test_suite_without_a_section() {
  echo "Test: a suite without a section is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" '/^### beta-network-partition$/d'
  assert_eq "the heading of beta-network-partition removed" \
    "suite without a section: beta-network-partition" "$(check "$root")"
}

test_suite_in_no_leg() {
  echo "Test: a suite that no matrix entry lists is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/ci.yaml" '\#^              tests/e2e-chaos/gamma-pod-kill$#d'
  assert_eq "gamma-pod-kill in no leg" "suite in no leg: gamma-pod-kill" "$(check "$root")"
}

test_suite_in_two_legs() {
  echo "Test: a suite that two matrix entries list is reported with both"

  local root
  root="$(build_fixture)"
  rewrite "$root/ci.yaml" 's#^              tests/e2e-chaos/beta-network-partition$#&\
              tests/e2e-chaos/alpha-pod-kill#'
  assert_eq "alpha-pod-kill in pod and network" \
    "suite in two legs: alpha-pod-kill: pod, network" "$(check "$root")"
}

test_leg_lists_a_missing_suite() {
  echo "Test: a matrix token without a suite directory is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/ci.yaml" 's#^              tests/e2e-chaos/beta-network-partition$#&\
              tests/e2e-chaos/epsilon-outage#'
  assert_eq "epsilon-outage has no directory" \
    "leg lists a missing suite: network: epsilon-outage" "$(check "$root")"

  root="$(build_fixture)"
  mkdir -p "$root/suites/zeta-outage"
  rewrite "$root/ci.yaml" 's#^              tests/e2e-chaos/beta-network-partition$#&\
              tests/e2e-chaos/zeta-outage#'
  assert_eq "a directory without chainsaw-test.yaml is no suite" \
    "leg lists a missing suite: network: zeta-outage" "$(check "$root")"
}

test_comment_line_token_does_not_count() {
  echo "Test: a token on a comment line of the matrix does not count"

  local root
  root="$(build_fixture)"
  rewrite "$root/ci.yaml" '\#^              tests/e2e-chaos/gamma-pod-kill$#d'
  rewrite "$root/ci.yaml" 's#^          - suite: network$#&\
            \# tests/e2e-chaos/gamma-pod-kill runs elsewhere#'
  assert_eq "the comment line was written" "1" \
    "$(grep -c '^            # tests/e2e-chaos/gamma-pod-kill runs elsewhere$' "$root/ci.yaml")"
  assert_eq "gamma-pod-kill only in comments" "suite in no leg: gamma-pod-kill" "$(check "$root")"
}

test_wrong_suite_count() {
  echo "Test: a sentence that counts the suites wrongly is reported with its line"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's/The 3 chaos test suites/The 4 chaos test suites/'
  rewrite "$root/page.md" 's/^## Test Suite Details$/&\
\
All 2 test suites run on kind./'
  assert_eq "four and two said, three in the tree" \
    "$root/page.md:15: says 2 suites, the tree has 3"$'\n'"$root/page.md:3: says 4 suites, the tree has 3" \
    "$(check "$root")"
}

test_repository_page_and_matrix_name_every_suite() {
  echo "Test: the repository's chaos page and e2e-chaos matrix name every suite"

  local suites="$PROJECT_ROOT/tests/e2e-chaos"
  local workflow="$PROJECT_ROOT/.github/workflows/ci.yaml"
  local page="$PROJECT_ROOT/docs/reference/testing/chaos-e2e-tests.md"
  local copy
  assert_eq "no problems in the repository" "" \
    "$(check_chaos_suites "$suites" "$workflow" "$page")"

  # The same check on a copy of the page without the row of
  # glance-garage-outage proves it reads the real inventory.
  copy="$(mktemp -d "$TMP_ROOT/copy.XXXXXX")"
  cp "$page" "$copy/chaos-e2e-tests.md"
  rewrite "$copy/chaos-e2e-tests.md" '/^| \[glance-garage-outage\]/d'
  assert_eq "a copy without the row of glance-garage-outage" \
    "suite without a row: glance-garage-outage" \
    "$(check_chaos_suites "$suites" "$workflow" "$copy/chaos-e2e-tests.md")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_consistent_fixture_reports_nothing
test_directory_without_a_suite_is_the_only_report
test_missing_workflow_is_the_only_report
test_workflow_without_the_matrix_is_the_only_report
test_missing_page_is_the_only_report
test_page_without_the_table_is_the_only_report
test_suite_without_a_row
test_row_without_a_suite
test_first_cell_without_a_link
test_suite_without_a_section
test_suite_in_no_leg
test_suite_in_two_legs
test_leg_lists_a_missing_suite
test_comment_line_token_does_not_count
test_wrong_suite_count
test_repository_page_and_matrix_name_every_suite

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
