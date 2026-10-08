#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the job table and the stage figure of the CI workflow page name every
# job of .github/workflows/ci.yaml with its needs list.
#
# The jobs are the keys at two spaces of indent after the line `jobs:`. The
# needs of a job is the first line between its key and the next job key that
# starts with four spaces and `needs:`, written as a flow list or a single
# name; a job without such a line needs nothing. `## Job Dependency DAG` of
# docs/reference/ci-cd/ci-workflow.md holds a table with one row per job: the
# job in backticks in the first cell, its stage in the second, and `none` or
# its needs in backticks in the third, in the order of the workflow. The
# stages follow the figure: a row has the stage Image build, that job needs
# every Gates job and no other job but the change detection, which needs no
# job, and no job needs a Checks job. `## Jobs` describes every job in a
# `### <job>` section.
# docs/diagrams/ci-stage-overview.drawio and .svg carry every job and every
# stage as a label of its own. check_ci_jobs prints one problem per line and
# nothing for a consistent tree. The figure check runs one way: a label the
# figure has and neither the workflow nor the table names is not reported.
# The fixture tests prove each check on a scratch tree before the last test
# runs it on the repository.
#
# Usage: bash tests/unit/docs/ci_stage_overview_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

FIGURE="ci-stage-overview"
TABLE_HEADER='| Job | Stage | `needs` |'

# Print one line per job of a workflow: the key, a tab, how the job states
# its needs (none, list or block), a tab, and the names it needs, joined by a
# comma and a space. A `needs:` line with nothing after the colon is a block
# list, whose names this reader does not collect.
# shellcheck disable=SC2016 # an awk program, not shell
JOBS_AWK='
function flush() {
  if (job != "") print job "\t" kind "\t" needs
  job = ""
}

/^jobs:[ \t]*(#.*)?$/ { in_jobs = 1; next }
!in_jobs { next }
/^[^ \t#]/ { flush(); in_jobs = 0; next }

/^  [A-Za-z0-9_-]+:[ \t]*(#.*)?$/ {
  flush()
  job = $0
  sub(/^  /, "", job)
  sub(/:.*$/, "", job)
  kind = "none"
  needs = ""
  seen = 0
  next
}

job != "" && !seen && /^    needs:/ {
  seen = 1
  value = $0
  sub(/^    needs:[ \t]*/, "", value)
  sub(/[ \t]*#.*$/, "", value)
  if (value == "") { kind = "block"; next }
  kind = "list"
  gsub(/\[/, "", value)
  gsub(/\]/, "", value)
  count = split(value, parts, ",")
  for (i = 1; i <= count; i++) {
    name = parts[i]
    gsub(/^[ \t]+/, "", name)
    gsub(/[ \t]+$/, "", name)
    if (name != "") needs = needs (needs == "" ? "" : ", ") name
  }
}

END { flush() }
'

# Print the rows of the job table: a line "#table" once the header is found,
# then per row the job, a tab, the stage, a tab, and the names of the third
# cell joined by a comma and a space (empty for `none`).
# shellcheck disable=SC2016 # an awk program, not shell
ROWS_AWK='
function trim(s) {
  gsub(/^[ \t]+/, "", s)
  gsub(/[ \t]+$/, "", s)
  return s
}

$0 == "## Job Dependency DAG" { after = 1; next }
after && !in_table && $0 == header { in_table = 1; print "#table"; next }
in_table {
  if (substr($0, 1, 1) != "|") exit
  if ($0 ~ /^\|[ \t:|-]+$/) next
  split($0, cells, "|")
  job = cells[2]
  if (match(job, /`[^`]*`/)) job = substr(job, RSTART + 1, RLENGTH - 2)
  else job = trim(job)
  stage = trim(cells[3])
  rest = trim(cells[4])
  needs = ""
  if (rest != "none") {
    while (match(rest, /`[^`]*`/)) {
      needs = needs (needs == "" ? "" : ", ") substr(rest, RSTART + 1, RLENGTH - 2)
      rest = substr(rest, RSTART + RLENGTH)
    }
  }
  print job "\t" stage "\t" needs
}
'

# Print the `### ` headings of the `## Jobs` section.
# shellcheck disable=SC2016 # an awk program, not shell
SECTIONS_AWK='
$0 == "## Jobs" { in_jobs = 1; next }
/^## / { in_jobs = 0 }
in_jobs && sub(/^### /, "")
'

# Compare the jobs of JOBS with the rows of ROWS and the headings of SECTIONS,
# all from the environment. Of rows out of workflow order only the first is
# reported, so that one moved row is one line.
# shellcheck disable=SC2016 # an awk program, not shell
COMPARE_AWK='
BEGIN {
  jobs = split(ENVIRON["JOBS"], lines, "\n")
  for (i = 1; i <= jobs; i++) {
    split(lines[i], f, "\t")
    order[i] = f[1]
    pos[f[1]] = i
    kind[f[1]] = f[2]
    needs[f[1]] = f[3]
    if (f[2] == "block") print "unreadable needs: " f[1]
    c = split(f[3], names, ", ")
    for (k = 1; k <= c; k++) if (!(names[k] in needer)) needer[names[k]] = f[1]
  }
  rows = split(ENVIRON["ROWS"], lines, "\n")
  for (i = 1; i <= rows; i++) {
    if (lines[i] == "#table") continue
    f[1] = ""; f[2] = ""; f[3] = ""
    split(lines[i], f, "\t")
    job = f[1]
    if (job in has_row) { print "duplicate row: " job; continue }
    has_row[job] = 1
    if (f[2] == "") print "no stage: " job
    if (!(job in kind)) { print "row without a job: " job; continue }
    if (pos[job] < last && !misordered) {
      print "row out of workflow order: " job " after " last_job
      misordered = 1
    }
    if (pos[job] > last) { last = pos[job]; last_job = job }
    if (f[2] != "") stage[job] = f[2]
    if (f[2] == "Image build") build = job
    if (kind[job] == "block") continue
    if (f[3] != needs[job])
      printf "needs differs: %s: workflow \"%s\", table \"%s\"\n", job, needs[job], f[3]
  }
  if (build == "") print "no Image build row"
  c = split(needs[build], names, ", ")
  for (k = 1; k <= c; k++) gate[names[k]] = 1
  for (job in stage) {
    if (job in gate) {
      if (stage[job] != "Gates" && !(stage[job] == "Change detection" && needs[job] == ""))
        printf "stage differs: %s: needed by %s, table \"%s\"\n", job, build, stage[job]
    } else if (build != "" && stage[job] == "Gates")
      printf "stage differs: %s: not needed by %s, table \"Gates\"\n", job, build
    else if (stage[job] == "Checks" && (job in needer))
      printf "stage differs: %s: needed by %s, table \"Checks\"\n", job, needer[job]
  }
  s = split(ENVIRON["SECTIONS"], lines, "\n")
  for (i = 1; i <= s; i++) section[lines[i]] = 1
  for (i = 1; i <= jobs; i++) {
    if (!(order[i] in has_row)) print "job without a row: " order[i]
    if (!(order[i] in section)) print "job without a section: " order[i]
  }
}
'

# Print one line per sentence of the page that counts the jobs wrongly.
# shellcheck disable=SC2016 # an awk program, not shell
COUNT_AWK='
{
  line = $0
  while (match(line, /defines [0-9]+ jobs/)) {
    said = substr(line, RSTART, RLENGTH)
    gsub(/[^0-9]/, "", said)
    if (said + 0 != want + 0)
      printf "%s:%d: says %s jobs, the workflow defines %d\n", page, FNR, said, want
    line = substr(line, RSTART + RLENGTH)
  }
}
'

# check_ci_jobs <ci.yaml> <page> <diagrams-dir>: print one problem per line,
# sorted, and nothing when the table and the figure match the workflow. A
# missing or jobless workflow, a missing page and a page without the job
# table are each the only line.
check_ci_jobs() {
  local workflow="$1" page="$2" dir="$3"
  local drawio="$dir/$FIGURE.drawio" svg="$dir/$FIGURE.svg"

  if [[ ! -f "$workflow" ]]; then
    echo "missing $workflow"
    return
  fi
  local jobs
  jobs="$(awk "$JOBS_AWK" "$workflow")"
  if [[ -z "$jobs" ]]; then
    echo "no job in $workflow"
    return
  fi
  if [[ ! -f "$page" ]]; then
    echo "missing $page"
    return
  fi
  local rows
  rows="$(awk -v header="$TABLE_HEADER" "$ROWS_AWK" "$page")"
  if [[ -z "$rows" ]]; then
    echo "no job table in $page"
    return
  fi
  local sections
  sections="$(awk "$SECTIONS_AWK" "$page")"

  {
    JOBS="$jobs" ROWS="$rows" SECTIONS="$sections" awk "$COMPARE_AWK" </dev/null
    awk -v page="$page" -v want="$(wc -l <<<"$jobs")" "$COUNT_AWK" "$page"
    [[ -f "$drawio" ]] || echo "missing figure: $FIGURE.drawio"
    [[ -f "$svg" ]] || echo "missing figure: $FIGURE.svg"
    local label
    while IFS= read -r label; do
      [[ -n "$label" ]] || continue
      if [[ -f "$drawio" ]] && ! grep -F -q -- "value=\"$label\"" "$drawio"; then
        echo "not in the figure: $label ($FIGURE.drawio)"
      fi
      if [[ -f "$svg" ]] && ! grep -F -q -- ">$label<" "$svg"; then
        echo "not in the figure: $label ($FIGURE.svg)"
      fi
    done <<<"$(cut -f1 <<<"$jobs"; cut -s -f2 <<<"$rows" | LC_ALL=C sort -u)"
  } | LC_ALL=C sort
}

# ---------------------------------------------------------------------------
# Fixture
# ---------------------------------------------------------------------------

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

FIXTURE_LABELS=(changes test test-shell build publish
  "Change detection" Gates Checks "Every event" "Image build" Publish)

# build_fixture: write a consistent tree and print its root. The workflow has
# two-space keys before `jobs:`, a job key with a comment, a `needs:` deeper
# than four spaces, a flow list several lines below its key and a scalar
# needs. The page counts the five jobs, lists them in a table whose first
# cells are links or plain code, and gives each a section under `## Jobs`,
# where the next `## ` heading ends. diagrams/ holds a pair with the five jobs
# and the stages as labels.
build_fixture() {
  local root label n=0
  root="$(mktemp -d "$TMP_ROOT/fixture.XXXXXX")"
  mkdir -p "$root/diagrams"
  cat >"$root/ci.yaml" <<'EOF'
name: CI
on:
  push:
    branches: [main]
permissions:
  contents: read
jobs:
  changes:
    runs-on: ubuntu-latest
  test:
    runs-on: ubuntu-latest
    needs: [changes]
  test-shell:
    runs-on: ubuntu-latest
  build: # the image build
    runs-on: ubuntu-latest
    if: always()
    steps:
      - uses: ./.github/actions/example
        with:
          needs: none
    needs: [changes, test]
  publish:
    runs-on: ubuntu-latest
    needs: build
EOF
  cat >"$root/page.md" <<'EOF'
# CI Workflow

The workflow defines 5 jobs.

## Job Dependency DAG

| Job | Stage | `needs` |
| --- | --- | --- |
| [`changes`](#changes) | Change detection | none |
| [`test`](#test) | Gates | `changes` |
| `test-shell` | Every event | none |
| [`build`](#build) | Image build | `changes`, `test` |
| [`publish`](#publish) | Publish | `build` |

## Jobs

### changes

### test

### test-shell

### build

### publish

## Reusable CI Scripts
EOF
  {
    echo '<mxfile host="drawio" type="device"><diagram id="f" name="F"><mxGraphModel><root>'
    for label in "${FIXTURE_LABELS[@]}"; do
      n=$((n + 1))
      echo "<mxCell id=\"f-$n\" value=\"$label\" vertex=\"1\" parent=\"1\"/>"
    done
    echo '</root></mxGraphModel></diagram></mxfile>'
  } >"$root/diagrams/$FIGURE.drawio"
  {
    echo '<svg xmlns="http://www.w3.org/2000/svg">'
    for label in "${FIXTURE_LABELS[@]}"; do
      echo "<text x=\"0\" y=\"0\">$label</text>"
    done
    echo '</svg>'
  } >"$root/diagrams/$FIGURE.svg"
  printf '%s\n' "$root"
}

# rewrite <file> <sed-script>: apply a sed script in place (BSD and GNU sed).
rewrite() {
  sed "$2" "$1" >"$1.tmp" && mv "$1.tmp" "$1"
}

# check <root>: run check_ci_jobs on a fixture tree.
check() {
  check_ci_jobs "$1/ci.yaml" "$1/page.md" "$1/diagrams"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_consistent_fixture_reports_nothing() {
  echo "Test: a table and a figure that match the workflow report nothing"

  local root
  root="$(build_fixture)"
  assert_eq "no problems" "" "$(check "$root")"
}

test_missing_workflow_is_the_only_report() {
  echo "Test: a workflow path that is not a file is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/page.md"
  assert_eq "absent path" "missing $root/absent.yaml" \
    "$(check_ci_jobs "$root/absent.yaml" "$root/page.md" "$root/diagrams")"
  assert_eq "a directory" "missing $root/diagrams" \
    "$(check_ci_jobs "$root/diagrams" "$root/page.md" "$root/diagrams")"
}

test_workflow_without_a_job_is_the_only_report() {
  echo "Test: a workflow without a job key is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/page.md"
  rewrite "$root/ci.yaml" '/^jobs:/,$d'
  assert_eq "no jobs: line" "no job in $root/ci.yaml" "$(check "$root")"

  root="$(build_fixture)"
  rm "$root/page.md"
  rewrite "$root/ci.yaml" '/^jobs:/,${/^  /d;}'
  assert_eq "a jobs: line without a key" "no job in $root/ci.yaml" "$(check "$root")"
}

test_missing_page_is_the_only_report() {
  echo "Test: a page that does not exist is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/page.md" "$root/diagrams/$FIGURE.svg"
  assert_eq "absent page" "missing $root/page.md" "$(check "$root")"
}

test_page_without_the_table_is_the_only_report() {
  echo "Test: a page without the job table is the only report"

  local root
  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.svg"
  rewrite "$root/page.md" '/^| Job | Stage |/d'
  assert_eq "no header row" "no job table in $root/page.md" "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/page.md" '/^## Job Dependency DAG$/d'
  assert_eq "a table outside the section" "no job table in $root/page.md" "$(check "$root")"
}

test_job_without_a_row() {
  echo "Test: a job of the workflow without a row is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" '/^| `test-shell` |/d'
  assert_eq "the row of test-shell removed" "job without a row: test-shell" "$(check "$root")"
}

test_row_without_a_job() {
  echo "Test: a row whose job the workflow does not define is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's#^| \[`publish`\].*$#&\
| [`deploy`](\#deploy) | Publish | `build` |#'
  assert_eq "a row of deploy added" "row without a job: deploy" "$(check "$root")"
}

test_needs_lists_compare_names_and_order() {
  echo "Test: a table that names other needs, or the same in another order, is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's#`changes`, `test` |$#`changes`, `lint` |#'
  assert_eq "one differing name" \
    'needs differs: build: workflow "changes, test", table "changes, lint"' "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/page.md" 's#`changes`, `test` |$#`test`, `changes` |#'
  assert_eq "two names in another order" \
    'needs differs: build: workflow "changes, test", table "test, changes"' "$(check "$root")"
}

test_job_without_needs_reads_as_none() {
  echo "Test: a job without a needs: line matches none and nothing else"

  local root
  root="$(build_fixture)"
  assert_eq "changes against none" "" "$(check "$root")"

  rewrite "$root/page.md" 's#^\(| \[`changes`\](\#changes) | Change detection |\) none |$#\1 `test` |#'
  assert_eq "changes against a name" \
    'needs differs: changes: workflow "", table "test"' "$(check "$root")"
}

test_scalar_needs_reads_as_one_name() {
  echo "Test: a scalar needs: reads as its one name"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's#^\(| \[`publish`\](\#publish) | Publish |\) `build` |$#\1 none |#'
  assert_eq "publish against none" \
    'needs differs: publish: workflow "build", table ""' "$(check "$root")"
}

test_block_list_needs_is_unreadable() {
  echo "Test: a block-list needs: is reported as unreadable and not compared"

  local root
  root="$(build_fixture)"
  rewrite "$root/ci.yaml" 's#^    needs: build$#    needs:\
      - build#'
  assert_eq "publish with a block list" "unreadable needs: publish" "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/ci.yaml" 's#^    needs: build$#    needs: \# a list follows\
      - build#'
  assert_eq "a block list after a comment" "unreadable needs: publish" "$(check "$root")"
}

test_missing_label_reports_its_file() {
  echo "Test: a job missing from one file of the pair reports that file"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" '/value="publish"/d'
  assert_eq "missing from the drawio only" \
    "not in the figure: publish ($FIGURE.drawio)" "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.svg" '/>publish</d'
  assert_eq "missing from the svg only" \
    "not in the figure: publish ($FIGURE.svg)" "$(check "$root")"
}

test_label_inside_a_longer_label_counts_as_missing() {
  echo "Test: a job that only stands inside a longer label is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.drawio" '/value="test"/d'
  rewrite "$root/diagrams/$FIGURE.svg" '/>test</d'
  assert_eq "test beside test-shell" \
    "not in the figure: test ($FIGURE.drawio)"$'\n'"not in the figure: test ($FIGURE.svg)" \
    "$(check "$root")"
}

test_extra_figure_label_is_not_reported() {
  echo "Test: a label the figure has and the workflow lacks is not reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/diagrams/$FIGURE.svg" 's#^</svg>$#<text x="0" y="0">deploy</text></svg>#'
  rewrite "$root/diagrams/$FIGURE.drawio" 's#</root>#<mxCell id="f-9" value="deploy"/></root>#'
  assert_eq "no problems" "" "$(check "$root")"
}

test_missing_figure_file_skips_only_its_searches() {
  echo "Test: a missing file of the pair is reported beside the other lines"

  local root
  root="$(build_fixture)"
  rm "$root/diagrams/$FIGURE.svg"
  rewrite "$root/diagrams/$FIGURE.drawio" '/value="build"/d'
  rewrite "$root/page.md" '/^| `test-shell` |/d'
  assert_eq "no svg, the drawio and the table are still checked" \
    "job without a row: test-shell"$'\n'"missing figure: $FIGURE.svg"$'\n'"not in the figure: build ($FIGURE.drawio)" \
    "$(check "$root")"
}

test_empty_stage_cell() {
  echo "Test: a row with an empty stage cell is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's#^\(| \[`test`\](\#test) |\) Gates |#\1 |#'
  assert_eq "test without a stage" "no stage: test" "$(check "$root")"
}

test_stage_missing_from_the_figure() {
  echo "Test: a stage the figure does not carry is reported with its file"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's#^\(| \[`publish`\](\#publish) |\) Publish |#\1 Release |#'
  assert_eq "publish in the stage Release" \
    "not in the figure: Release ($FIGURE.drawio)"$'\n'"not in the figure: Release ($FIGURE.svg)" \
    "$(check "$root")"
}

test_gates_are_the_needs_of_the_image_build() {
  echo "Test: a job the image build needs outside Gates, or a gate it does not need, is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's#^| `test-shell` | Every event |#| `test-shell` | Checks |#'
  assert_eq "a check no job needs" "" "$(check "$root")"
  rewrite "$root/ci.yaml" 's#^    needs: \[changes, test\]$#    needs: [changes, test, test-shell]#'
  rewrite "$root/page.md" 's#`changes`, `test` |$#`changes`, `test`, `test-shell` |#'
  assert_eq "the image build needs a check" \
    'stage differs: test-shell: needed by build, table "Checks"' "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/page.md" 's#^\(| \[`publish`\](\#publish) |\) Publish |#\1 Gates |#'
  assert_eq "a gate the image build does not need" \
    'stage differs: publish: not needed by build, table "Gates"' "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/page.md" 's#^\(| \[`test`\](\#test) |\) Gates |#\1 Change detection |#'
  assert_eq "a gate in the stage Change detection" \
    'stage differs: test: needed by build, table "Change detection"' "$(check "$root")"
}

test_table_without_an_image_build_row() {
  echo "Test: a table without a row in the stage Image build is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's#| Image build |#| E2E image build |#'
  rewrite "$root/diagrams/$FIGURE.drawio" 's#value="Image build"#value="E2E image build"#'
  rewrite "$root/diagrams/$FIGURE.svg" 's#>Image build<#>E2E image build<#'
  assert_eq "the stage renamed in the table and the figure" "no Image build row" "$(check "$root")"
}

test_checks_are_needed_by_no_job() {
  echo "Test: a check that another job needs is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's#^| `test-shell` | Every event |#| `test-shell` | Checks |#'
  rewrite "$root/ci.yaml" 's#^    needs: build$#    needs: [build, test-shell]#'
  rewrite "$root/page.md" 's#^\(| \[`publish`\](\#publish) | Publish |\) `build` |$#\1 `build`, `test-shell` |#'
  assert_eq "publish needs a check" \
    'stage differs: test-shell: needed by publish, table "Checks"' "$(check "$root")"
}

test_duplicate_row() {
  echo "Test: a second row of a job is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's#^| \[`publish`\].*$#&\
&#'
  assert_eq "two rows of publish" "duplicate row: publish" "$(check "$root")"
}

test_row_out_of_workflow_order() {
  echo "Test: rows out of the workflow order are reported once"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" '/^| \[`test`\]/{
h
d
}
/^| `test-shell` |/G'
  assert_eq "test after test-shell" "row out of workflow order: test after test-shell" "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/page.md" '/^| \[`publish`\]/d'
  rewrite "$root/page.md" 's#^| --- | --- | --- |$#&\
| [`publish`](\#publish) | Publish | `build` |#'
  assert_eq "publish moved to the top" "row out of workflow order: changes after publish" "$(check "$root")"
}

test_job_without_a_section() {
  echo "Test: a job without a section under ## Jobs is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" '/^### test-shell$/d'
  assert_eq "the heading of test-shell removed" "job without a section: test-shell" "$(check "$root")"

  printf '\n### test-shell\n' >>"$root/page.md"
  assert_eq "the heading under the next ## section" "job without a section: test-shell" "$(check "$root")"
}

test_wrong_job_count() {
  echo "Test: a sentence that counts the jobs wrongly is reported with its line"

  local root
  root="$(build_fixture)"
  rewrite "$root/page.md" 's#defines 5 jobs#defines 6 jobs#'
  assert_eq "six jobs said, five defined" \
    "$root/page.md:3: says 6 jobs, the workflow defines 5" "$(check "$root")"
}

test_repository_table_and_figure_match_the_workflow() {
  echo "Test: the repository's job table and stage figure match ci.yaml"

  local workflow="$PROJECT_ROOT/.github/workflows/ci.yaml"
  local page="$PROJECT_ROOT/docs/reference/ci-cd/ci-workflow.md"
  local copy
  assert_eq "no problems in the repository" "" \
    "$(check_ci_jobs "$workflow" "$page" "$PROJECT_ROOT/docs/diagrams")"

  # The same check on a copy of the pair without the svg label
  # review-markers proves it reads the real figure.
  copy="$(mktemp -d "$TMP_ROOT/copy.XXXXXX")"
  cp "$PROJECT_ROOT/docs/diagrams/$FIGURE.drawio" "$PROJECT_ROOT/docs/diagrams/$FIGURE.svg" "$copy/"
  rewrite "$copy/$FIGURE.svg" '/>review-markers</d'
  assert_eq "a copy without review-markers" \
    "not in the figure: review-markers ($FIGURE.svg)" \
    "$(check_ci_jobs "$workflow" "$page" "$copy")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_consistent_fixture_reports_nothing
test_missing_workflow_is_the_only_report
test_workflow_without_a_job_is_the_only_report
test_missing_page_is_the_only_report
test_page_without_the_table_is_the_only_report
test_job_without_a_row
test_row_without_a_job
test_needs_lists_compare_names_and_order
test_job_without_needs_reads_as_none
test_scalar_needs_reads_as_one_name
test_block_list_needs_is_unreadable
test_missing_label_reports_its_file
test_label_inside_a_longer_label_counts_as_missing
test_extra_figure_label_is_not_reported
test_missing_figure_file_skips_only_its_searches
test_empty_stage_cell
test_stage_missing_from_the_figure
test_gates_are_the_needs_of_the_image_build
test_table_without_an_image_build_row
test_checks_are_needed_by_no_job
test_duplicate_row
test_row_out_of_workflow_order
test_job_without_a_section
test_wrong_job_count
test_repository_table_and_figure_match_the_workflow

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
