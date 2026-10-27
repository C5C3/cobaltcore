#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-report-nightly-chaos.sh with a stubbed gh, a stubbed
# hack/ci-report-red-run.sh that logs its env, the real jq and the real
# hack/ci-junit-failed-suites.py:
#   - five green legs file nothing, exit 0, and write a step summary naming
#     the five legs and the self-hosted total;
#   - a failed leg whose report holds two failed testsuites files one issue
#     per suite, each summary holding the leg, the job URL and the message
#     inside a fence; a message holding an @name and a backtick fence stays
#     inside a longer fence;
#   - a cancelled leg without a report, a failed leg whose report is
#     malformed, and a failed leg whose report names no failed suite each file
#     the leg, with the job's conclusion as the result;
#   - a night where no leg ran files the run, with the conclusion of
#     build-e2e-images, as "skipped" when that build passed, or as "missing"
#     when the listing holds no job;
#   - self-hosted jobs summing 601 minutes file the budget, 600 do not; an
#     ubuntu-latest job and a job without completed_at add nothing;
#   - a failing listing, a listing that is no JSON and a page without a jobs
#     array exit 1 before any filing;
#   - an empty RUN_ID, RUN_URL or REPORTS_DIR, and a BUDGET_MINUTES that is
#     no whole number, exit 1 before any gh call;
#   - a failed filing does not stop the next one, and the script exits 1
#     naming the title it could not file.
#
# Usage: bash tests/unit/hack/ci_report_nightly_chaos_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

unset RUN_ID RUN_URL REPORTS_DIR BUDGET_MINUTES REPORT_SHA GITHUB_STEP_SUMMARY

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

HACK="$TMP_DIR/hack"
STUB_BIN="$TMP_DIR/bin"
REPORTS="$TMP_DIR/reports"
GH_LOG="$TMP_DIR/gh.log"
CALLS="$TMP_DIR/calls.log"
STEP_SUMMARY="$TMP_DIR/step-summary.md"
RUN="https://github.com/C5C3/cobaltcore/actions/runs/123"
SHA="0123456789abcdef0123456789abcdef01234567"
JOB_URL="https://github.com/C5C3/cobaltcore/actions/runs/123/job"

# The 22:00 UTC start of a night, in epoch seconds.
NIGHT=1793138400

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stubs
# The reporter and the reader are copied beside a ci-report-red-run.sh stub,
# which appends "TITLE|RESULT|RUN_URL|SHA" to $CALLS, copies SUMMARY_FILE to
# $TMP_DIR/summary-<n>.md and fails when REPORT_TITLE is $STUB_FAIL_TITLE. gh
# logs its arguments to $GH_LOG and prints $STUB_JOBS; it fails when
# $STUB_LIST_FAIL is set.
make_stubs() {
  mkdir -p "$HACK" "$STUB_BIN"
  cp "$PROJECT_ROOT/hack/ci-report-nightly-chaos.sh" "$PROJECT_ROOT/hack/ci-junit-failed-suites.py" "$HACK/"
  cat >"$HACK/ci-report-red-run.sh" <<'STUB'
#!/bin/bash
echo "${REPORT_TITLE}|${REPORT_RESULT}|${RUN_URL}|${REPORT_SHA:-}" >>"$CALLS"
n=$(wc -l <"$CALLS" | tr -d ' ')
cp "$SUMMARY_FILE" "$TMP_DIR/summary-$n.md"
[ "${REPORT_TITLE}" != "${STUB_FAIL_TITLE:-}" ]
STUB
  cat >"$STUB_BIN/gh" <<'STUB'
#!/bin/bash
line=""
for a in "$@"; do line="$line[$a]"; done
echo "$line" >>"$GH_LOG"
if [ -n "${STUB_LIST_FAIL:-}" ]; then
  echo "HTTP 502: Bad Gateway" >&2
  exit 1
fi
printf '%s\n' "$STUB_JOBS"
STUB
  chmod +x "$HACK"/* "$STUB_BIN/gh"
}

# job <name> <conclusion> <runner-label> <minutes> — one job of the listing,
# started at the night's start; minutes "-" leaves it without completed_at.
job() {
  jq -nc --arg n "$1" --arg c "$2" --arg l "$3" --arg m "$4" --argjson t "$NIGHT" \
    --arg u "$JOB_URL/$RANDOM" '
    {name: $n, html_url: $u, labels: [$l], started_at: ($t | todate)}
    + if $m == "-" then {status: "in_progress", conclusion: null, completed_at: null}
      else {status: "completed", conclusion: $c,
            completed_at: ($t + ($m | tonumber) * 60 | todate)} end'
}

# leg <leg> <conclusion> <minutes> — an e2e-chaos matrix leg on self-hosted.
leg() {
  job "e2e-chaos ($1, tests/e2e/infrastructure/chaos-mesh-health tests/e2e-chaos/x...)" "$2" self-hosted "$3"
}

# page <job-json...> — one page of the listing.
page() {
  printf '%s\n' "$@" | jq -sc '{total_count: length, jobs: .}'
}

# green_prefix — the jobs before the legs on a night that built its images.
green_prefix() {
  job changes success ubuntu-latest 1
  job test-shell success ubuntu-latest 7
  job build-e2e-images success self-hosted 18
}

# junit <leg> <suite> [<failure-message>]... — write the leg's report with
# one testsuite per suite; a suite followed by a message failed with it, one
# followed by "" passed.
junit() {
  local leg="$1" dir
  shift
  dir="$REPORTS/e2e-chaos-junit-report-$leg"
  mkdir -p "$dir"
  {
    echo '<testsuites name="chainsaw-report" time="10.0" tests="2" failures="1" skipped="0">'
    while [ "$#" -ge 2 ]; do
      printf '  <testsuite name="tests/e2e-chaos/%s" tests="1" failures="0" errors="0" id="0" time="">\n' "$1"
      if [ -n "$2" ]; then
        printf '    <testcase name="%s" classname="" time="1.0"><failure message="%s"></failure></testcase>\n' "$1" "$2"
      else
        printf '    <testcase name="%s" classname="" time="1.0"></testcase>\n' "$1"
      fi
      printf '  </testsuite>\n'
      shift 2
    done
    echo '</testsuites>'
  } >"$dir/chainsaw-chaos-report.xml"
}

# reset — empty the logs, the reports and the stub knobs.
reset() {
  rm -rf "$REPORTS" "$TMP_DIR"/summary-*.md
  : >"$GH_LOG"
  : >"$CALLS"
  : >"$STEP_SUMMARY"
  mkdir -p "$REPORTS"
  unset STUB_LIST_FAIL STUB_FAIL_TITLE
  STUB_JOBS=""
}

# run_report [ENV=value ...] — run the reporter on the stubs; OUT holds stdout,
# ERR stderr, RC the exit code.
run_report() {
  RC=0
  OUT="$(env PATH="$STUB_BIN:$PATH" GH_LOG="$GH_LOG" CALLS="$CALLS" TMP_DIR="$TMP_DIR" \
    STUB_JOBS="$STUB_JOBS" STUB_LIST_FAIL="${STUB_LIST_FAIL:-}" \
    STUB_FAIL_TITLE="${STUB_FAIL_TITLE:-}" \
    RUN_ID=123 RUN_URL="$RUN" REPORTS_DIR="$REPORTS" REPORT_SHA="$SHA" \
    GITHUB_STEP_SUMMARY="$STEP_SUMMARY" "$@" \
    bash "$HACK/ci-report-nightly-chaos.sh" 2>"$TMP_DIR/stderr")" || RC=$?
  ERR="$(cat "$TMP_DIR/stderr")"
}

# calls — the filed titles and results, one "TITLE|RESULT" per line.
calls() {
  cut -d'|' -f1,2 "$CALLS"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_a_green_night_files_nothing() {
  echo "Test: five green legs file nothing and write the step summary"
  reset
  STUB_JOBS="$(page "$(green_prefix)" "$(leg pod success 100)" "$(leg network success 90)" \
    "$(leg ovn success 12)" "$(leg nova success 137)" "$(leg controlplane success 45)")"

  run_report
  assert_eq "it exits 0" "0" "$RC"
  assert_eq "nothing is filed" "" "$(calls)"
  assert_contains "gh lists the latest jobs of every page" "$(cat "$GH_LOG")" \
    "[api][--paginate][repos/{owner}/{repo}/actions/runs/123/jobs?filter=latest&per_page=100]"
  local leg
  for leg in pod network ovn nova controlplane; do
    assert_file_contains "the step summary names the $leg leg" "$STEP_SUMMARY" "^| $leg | success | "
  done
  assert_file_contains "the step summary counts build and legs as self-hosted" "$STEP_SUMMARY" \
    "^Self-hosted runner time: 402 of 600 minutes\.$"
  assert_contains "the step summary goes to stdout too" "$OUT" "### Nightly chaos run"
}

test_a_red_leg_files_each_failed_suite() {
  echo "Test: a red leg with two failed testsuites files one issue per suite"
  reset
  STUB_JOBS="$(page "$(green_prefix)" "$(leg pod success 100)" "$(leg network failure 90)")"
  junit network neutron-mariadb-outage "exit status 1" glance-mariadb-outage "" \
    cinder-broker-pod-kill "signal: killed"

  run_report
  assert_eq "it exits 0" "0" "$RC"
  assert_eq "exactly the two suites are filed" \
    "Nightly chaos suite neutron-mariadb-outage is red|failure
Nightly chaos suite cinder-broker-pod-kill is red|failure" "$(calls)"
  assert_eq "each call carries the run URL and the commit" \
    "$RUN|$SHA" "$(head -1 "$CALLS" | cut -d'|' -f3,4)"
  local url
  url="$(printf '%s\n' "$STUB_JOBS" | jq -r '.jobs[] | select(.name | startswith("e2e-chaos (network,")) | .html_url')"
  assert_file_contains "the summary names the leg" "$TMP_DIR/summary-1.md" '^Leg: `network`$'
  assert_file_contains "the summary names the suite" "$TMP_DIR/summary-1.md" '^Suite: `neutron-mariadb-outage`$'
  assert_file_contains_fixed "the summary links the job" "$TMP_DIR/summary-1.md" "Job: $url"
  assert_eq "the message sits inside a fence" '```text
exit status 1
```' "$(sed -n '/^```text$/,/^```$/p' "$TMP_DIR/summary-1.md")"
  assert_file_contains_fixed "the second summary holds its own message" "$TMP_DIR/summary-2.md" "signal: killed"
  assert_file_contains "the step summary lists the failed suites" "$STEP_SUMMARY" \
    "^| network | failure | 90 | neutron-mariadb-outage, cinder-broker-pod-kill |$"
}

test_a_message_cannot_break_out_of_its_fence() {
  echo "Test: a message with an @name and a fence stays inside a longer fence"
  reset
  STUB_JOBS="$(page "$(green_prefix)" "$(leg pod failure 100)")"
  junit pod eso-down 'ping @someone ```done```'

  run_report
  assert_eq "the suite is filed" "Nightly chaos suite eso-down is red|failure" "$(calls)"
  assert_eq "a four-backtick fence holds the message" '````text
ping @someone ```done```
````' "$(sed -n '/^````text$/,/^````$/p' "$TMP_DIR/summary-1.md")"
}

test_a_leg_without_a_suite_files_the_leg() {
  echo "Test: a cancelled leg without a report files the leg with its conclusion"
  reset
  STUB_JOBS="$(page "$(green_prefix)" "$(leg nova cancelled 150)")"

  run_report
  assert_eq "it exits 0" "0" "$RC"
  assert_eq "the leg is filed as cancelled" "Nightly chaos leg nova is red|cancelled" "$(calls)"
  assert_file_contains "the summary names the leg" "$TMP_DIR/summary-1.md" '^Leg: `nova`$'
  assert_file_contains "the summary says the report is missing" "$TMP_DIR/summary-1.md" "uploaded no JUnit report"

  echo "Test: a failed leg whose report is malformed files the leg"
  reset
  STUB_JOBS="$(page "$(green_prefix)" "$(leg ovn failure 12)")"
  mkdir -p "$REPORTS/e2e-chaos-junit-report-ovn"
  printf '<testsuites><testsuite name="tests/e2e-chaos/x">' >"$REPORTS/e2e-chaos-junit-report-ovn/chainsaw-chaos-report.xml"

  run_report
  assert_eq "the leg is filed as failed" "Nightly chaos leg ovn is red|failure" "$(calls)"
  assert_file_contains "the summary quotes the reader's error" "$TMP_DIR/summary-1.md" "is no JUnit report:"
  assert_eq "the reader's error sits inside a fence" "2" \
    "$(grep -c '^```' "$TMP_DIR/summary-1.md")"

  echo "Test: a failed leg whose report names no failed suite files the leg"
  reset
  STUB_JOBS="$(page "$(green_prefix)" "$(leg controlplane failure 45)")"
  junit controlplane rabbitmq-operator-cascading-failure ""

  run_report
  assert_eq "the leg is filed as failed" "Nightly chaos leg controlplane is red|failure" "$(calls)"
  assert_file_contains "the summary says no suite failed" "$TMP_DIR/summary-1.md" "names no failed suite"
}

test_a_night_without_legs_files_the_run() {
  echo "Test: every leg skipped after a red image build files the run"
  reset
  STUB_JOBS="$(page "$(job changes success ubuntu-latest 1)" \
    "$(job build-e2e-images failure self-hosted 20)" \
    "$(leg pod skipped 0)" "$(leg network skipped 0)")"

  run_report
  assert_eq "it exits 0" "0" "$RC"
  assert_eq "the run is filed with the build's conclusion" "Nightly chaos run is red|failure" "$(calls)"
  assert_file_contains "the summary names the build" "$TMP_DIR/summary-1.md" 'the `build-e2e-images` job ended `failure`'
  assert_file_contains "the step summary says no leg ran" "$STEP_SUMMARY" 'No `e2e-chaos` leg ran'

  echo "Test: every leg skipped after a green image build files the run as skipped"
  reset
  STUB_JOBS="$(page "$(green_prefix)" "$(leg pod skipped 0)" "$(leg network skipped 0)")"

  run_report
  assert_eq "it exits 0" "0" "$RC"
  assert_eq "the run is filed as skipped" "Nightly chaos run is red|skipped" "$(calls)"
  assert_file_contains "the summary names e2e-chaos" "$TMP_DIR/summary-1.md" 'the `e2e-chaos` job ended `skipped`'

  echo "Test: a listing without any job files the run as missing"
  reset
  STUB_JOBS='{"total_count": 0, "jobs": []}'

  run_report
  assert_eq "it exits 0" "0" "$RC"
  assert_eq "the run is filed as missing" "Nightly chaos run is red|missing" "$(calls)"
}

test_the_runner_budget() {
  echo "Test: 601 self-hosted minutes file the budget, 600 do not"
  reset
  STUB_JOBS="$(page "$(green_prefix)" "$(leg pod success 300)" "$(leg nova success 282)" \
    "$(job report-nightly-chaos success ubuntu-latest 300)" \
    "$(job e2e-prometheus success self-hosted -)")"

  run_report
  assert_eq "600 minutes, an ubuntu-latest job and an unfinished job file nothing" "" "$(calls)"
  assert_file_contains "the step summary shows 600" "$STEP_SUMMARY" "^Self-hosted runner time: 600 of 600 minutes\.$"

  reset
  STUB_JOBS="$(page "$(green_prefix)" "$(leg pod success 300)" "$(leg nova success 283)")"

  run_report
  assert_eq "it exits 0" "0" "$RC"
  assert_eq "601 minutes file the budget" "Nightly chaos run is over its runner budget|failure" "$(calls)"
  assert_file_contains "the table lists the build" "$TMP_DIR/summary-1.md" '^| `build-e2e-images` | success | 18 |$'
  assert_file_contains "the table lists a leg" "$TMP_DIR/summary-1.md" '^| `e2e-chaos (nova, .*` | success | 283 |$'
  assert_file_contains "the table carries the total" "$TMP_DIR/summary-1.md" '^| Total | | 601 |$'

  reset
  STUB_JOBS="$(page "$(green_prefix)" "$(leg pod success 300)" "$(leg nova success 282)")"
  run_report BUDGET_MINUTES=500
  assert_eq "BUDGET_MINUTES moves the line" "Nightly chaos run is over its runner budget|failure" "$(calls)"
}

test_a_broken_listing_files_nothing() {
  local name
  for name in "a failing gh call" "a listing that is no JSON" "a page without a jobs array" "an empty listing"; do
    echo "Test: $name exits 1 and files nothing"
    reset
    case "$name" in
      "a failing gh call") STUB_LIST_FAIL=1 ;;
      "a listing that is no JSON") STUB_JOBS='<html>502 Bad Gateway</html>' ;;
      "a page without a jobs array")
        STUB_JOBS="$(page "$(leg pod failure 10)")"$'\n''{"message": "Not Found"}'
        ;;
      "an empty listing") STUB_JOBS="" ;;
    esac

    run_report
    assert_eq "it exits 1" "1" "$RC"
    assert_contains "it says nothing was filed" "$ERR" \
      "ERROR: listing the jobs of run 123 failed; nothing was filed"
    assert_eq "nothing is filed" "" "$(calls)"
  done
}

test_a_second_page_is_read() {
  echo "Test: the legs of every page are read"
  reset
  STUB_JOBS="$(page "$(green_prefix)")"$'\n'"$(page "$(leg pod failure 10)")"
  junit pod eso-down "exit status 1"

  run_report
  assert_eq "the leg on the second page is filed" "Nightly chaos suite eso-down is red|failure" "$(calls)"
}

test_an_empty_input_stops_before_gh() {
  local var
  for var in RUN_ID RUN_URL REPORTS_DIR; do
    echo "Test: an empty $var exits 1 before any gh call"
    reset
    STUB_JOBS="$(page "$(green_prefix)")"

    run_report "$var="
    assert_eq "it exits 1" "1" "$RC"
    assert_eq "it names the variable" "ERROR: $var is empty" "$ERR"
    assert_eq "gh is never called" "" "$(cat "$GH_LOG")"
  done

  echo "Test: a BUDGET_MINUTES that is no whole number exits 1 before any gh call"
  reset
  STUB_JOBS="$(page "$(green_prefix)")"

  run_report BUDGET_MINUTES=10m
  assert_eq "it exits 1" "1" "$RC"
  assert_eq "it names the budget" "ERROR: BUDGET_MINUTES is no whole number" "$ERR"
  assert_eq "gh is never called" "" "$(cat "$GH_LOG")"
}

test_a_failed_filing_does_not_stop_the_next() {
  echo "Test: the first of two filings fails, the second is still made"
  reset
  STUB_JOBS="$(page "$(green_prefix)" "$(leg network failure 90)")"
  junit network neutron-mariadb-outage "exit status 1" cinder-broker-pod-kill "exit status 1"
  STUB_FAIL_TITLE="Nightly chaos suite neutron-mariadb-outage is red"

  run_report
  assert_eq "it exits 1" "1" "$RC"
  assert_eq "both filings are tried" "2" "$(wc -l <"$CALLS" | tr -d ' ')"
  assert_contains "it names the title it could not file" "$ERR" \
    "ERROR: not filed: Nightly chaos suite neutron-mariadb-outage is red"
  assert_not_contains "it names only that one" "$ERR" "cinder-broker-pod-kill"
  assert_file_contains "the step summary is written anyway" "$STEP_SUMMARY" "^| network | failure | "
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
make_stubs
test_a_green_night_files_nothing
test_a_red_leg_files_each_failed_suite
test_a_message_cannot_break_out_of_its_fence
test_a_leg_without_a_suite_files_the_leg
test_a_night_without_legs_files_the_run
test_the_runner_budget
test_a_broken_listing_files_nothing
test_a_second_page_is_read
test_an_empty_input_stops_before_gh
test_a_failed_filing_does_not_stop_the_next

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
