#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-report-red-run.sh with a stubbed gh and the real jq:
#   - with no open issue of the title it creates exactly one, whose body holds
#     the workflow, the result, the run URL, the commit and the summary file;
#   - with an open issue of exactly that title that github-actions[bot]
#     opened it comments on it and creates nothing; a title that only starts
#     with it, an issue someone else opened and a pull request match nothing;
#     of two matches it comments on the lower number;
#   - a missing or empty SUMMARY_FILE puts the fallback line into the body,
#     an unset REPORT_SHA leaves out the Commit: line, and a summary over
#     60000 bytes is cut so the body stays under GitHub's 65536;
#   - the cut keeps whole lines, so a code span holding an @name never loses
#     its closing backtick, and it closes a fence it leaves open;
#   - an empty REPORT_TITLE, REPORT_RESULT or RUN_URL exits 1 before any gh
#     call;
#   - a failing, empty or non-list listing exits 1 without a create or a
#     comment, and a failing create or comment exits 1;
#   - it reads every page of the open issues, and adds no label.
#
# Usage: bash tests/unit/hack/ci_report_red_run_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
REPORT_SH="$PROJECT_ROOT/hack/ci-report-red-run.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

unset REPORT_TITLE REPORT_RESULT RUN_URL REPORT_SHA SUMMARY_FILE GITHUB_WORKFLOW

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

STUB_BIN="$TMP_DIR/bin"
GH_LOG="$TMP_DIR/gh.log"
BODY_COPY="$TMP_DIR/body.md"
SUMMARY="$TMP_DIR/summary.md"

TITLE="Scheduled Flux path run is red"
RUN="https://github.com/C5C3/cobaltcore/actions/runs/123"
SHA="0123456789abcdef0123456789abcdef01234567"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stub
# gh logs its arguments in brackets, one call per line, to $GH_LOG, and
# copies the file after --body-file to $BODY_COPY. It answers:
#   api --paginate $STUB_ISSUES; fails when $STUB_LIST_FAIL
#   issue create   ok; fails when $STUB_CREATE_FAIL
#   issue comment  ok; fails when $STUB_COMMENT_FAIL
make_stub() {
  mkdir -p "$STUB_BIN"
  cat >"$STUB_BIN/gh" <<'STUB'
#!/bin/bash
line=""
for a in "$@"; do line="$line[$a]"; done
echo "$line" >>"$GH_LOG"
prev=""
for a in "$@"; do
  [ "$prev" = "--body-file" ] && cp "$a" "$BODY_COPY"
  prev="$a"
done
case "$1 $2" in
  "api --paginate")
    if [ -n "${STUB_LIST_FAIL:-}" ]; then
      echo "HTTP 502: Bad Gateway (https://api.github.com/repos/C5C3/cobaltcore/issues?page=2)" >&2
      exit 1
    fi
    printf '%s\n' "$STUB_ISSUES"
    ;;
  "issue create")
    if [ -n "${STUB_CREATE_FAIL:-}" ]; then
      echo "HTTP 403: Resource not accessible by integration" >&2
      exit 1
    fi
    echo "https://github.com/C5C3/cobaltcore/issues/99"
    ;;
  "issue comment")
    if [ -n "${STUB_COMMENT_FAIL:-}" ]; then
      echo "HTTP 403: Resource not accessible by integration" >&2
      exit 1
    fi
    echo "https://github.com/C5C3/cobaltcore/issues/$3#issuecomment-1"
    ;;
  *)
    echo "stub: unexpected gh $*" >&2
    exit 1
    ;;
esac
STUB
  chmod +x "$STUB_BIN/gh"
}

# run_report [NAME=VALUE]...
# Run the script with the stub on PATH, the default inputs and the given
# NAME=VALUE pairs on top. Sets OUT (stdout and stderr) and RC.
run_report() {
  : >"$GH_LOG"
  rm -f "$BODY_COPY"
  OUT="$(env PATH="$STUB_BIN:$PATH" GH_LOG="$GH_LOG" BODY_COPY="$BODY_COPY" \
    GITHUB_WORKFLOW="Flux path" STUB_ISSUES='[]' \
    REPORT_TITLE="$TITLE" REPORT_RESULT=failure RUN_URL="$RUN" \
    REPORT_SHA="$SHA" SUMMARY_FILE="$SUMMARY" \
    "$@" bash "$REPORT_SH" 2>&1)"
  RC=$?
}

# issue <number> <title> [<login>] — One open issue of the REST listing,
# opened by <login> (default github-actions[bot]).
issue() {
  printf '{"number":%s,"title":"%s","user":{"login":"%s"}}' "$1" "$2" "${3:-github-actions[bot]}"
}

# writes <verb> — The number of logged gh issue <verb> calls.
writes() {
  grep -c "^\[issue\]\[$1\]" "$GH_LOG" | tr -d ' '
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_no_open_issue_creates_one() {
  echo "Test: no open issue of the title creates exactly one"

  printf '## Flux path run summary\n\nStage: `ready`\n' >"$SUMMARY"
  run_report
  assert_eq "the script exits 0" "0" "$RC"
  assert_eq "it reads every page of the open issues" \
    "[api][--paginate][repos/{owner}/{repo}/issues?state=open&per_page=100]" "$(sed -n 1p "$GH_LOG")"
  assert_eq "one create" "1" "$(writes create)"
  assert_eq "no comment" "0" "$(writes comment)"
  assert_contains "the create carries the title" "$(cat "$GH_LOG")" \
    "[issue][create][--title][Scheduled Flux path run is red][--body-file]"
  assert_not_contains "no call adds a label" "$(cat "$GH_LOG")" "--label"
  assert_file_contains_fixed "the body names the workflow and the result" "$BODY_COPY" \
    'The `Flux path` run ended with result `failure`.'
  assert_file_contains_fixed "the body links the run" "$BODY_COPY" "Run: $RUN"
  assert_file_contains_fixed "the body names the commit" "$BODY_COPY" "Commit: $SHA"
  assert_file_contains_fixed "the body carries the summary" "$BODY_COPY" 'Stage: `ready`'
  assert_contains "it says what it did" "$OUT" "OK: created the tracking issue"
}

test_an_open_issue_gets_a_comment() {
  echo "Test: an open issue of exactly the title that the bot opened gets a comment"

  run_report STUB_ISSUES="[$(issue 7 'Something else'),$(issue 42 "$TITLE")]"
  assert_eq "the script exits 0" "0" "$RC"
  assert_contains "it comments on #42" "$(cat "$GH_LOG")" "[issue][comment][42][--body-file]"
  assert_eq "and creates nothing" "0" "$(writes create)"

  run_report STUB_ISSUES="[$(issue 12 "$TITLE (old)")]"
  assert_eq "a longer title matches nothing, so the script creates" "1" "$(writes create)"
  assert_eq "and comments on nothing" "0" "$(writes comment)"

  run_report STUB_ISSUES="[$(issue 57 "$TITLE"),$(issue 42 "$TITLE")]"
  assert_contains "of two matches the lower number gets the comment" "$(cat "$GH_LOG")" \
    "[issue][comment][42][--body-file]"
  assert_eq "exactly one comment" "1" "$(writes comment)"
}

test_only_the_bots_issue_matches() {
  echo "Test: an issue someone else opened, or a pull request, never gets the comment"

  run_report STUB_ISSUES="[$(issue 3 "$TITLE" outsider),$(issue 42 "$TITLE")]"
  assert_contains "a lower outsider's issue loses to the bot's #42" "$(cat "$GH_LOG")" \
    "[issue][comment][42][--body-file]"
  assert_eq "exactly one comment" "1" "$(writes comment)"

  run_report STUB_ISSUES="[$(issue 3 "$TITLE" outsider)]"
  assert_eq "an outsider's issue alone matches nothing, so the script creates" "1" "$(writes create)"
  assert_eq "and comments on nothing" "0" "$(writes comment)"

  run_report STUB_ISSUES='[{"number":5,"title":"'"$TITLE"'","user":{"login":"github-actions[bot]"},"pull_request":{}}]'
  assert_eq "a pull request of the bot matches nothing, so the script creates" "1" "$(writes create)"
  assert_eq "and comments on nothing" "0" "$(writes comment)"
}

test_the_body_without_a_summary() {
  echo "Test: a missing or empty summary file gives the fallback line"

  run_report SUMMARY_FILE="$TMP_DIR/missing.md"
  assert_eq "a missing file exits 0" "0" "$RC"
  assert_file_contains_fixed "a missing file gives the fallback" "$BODY_COPY" "The job wrote no summary"

  : >"$TMP_DIR/empty.md"
  run_report SUMMARY_FILE="$TMP_DIR/empty.md" REPORT_SHA=
  assert_file_contains_fixed "an empty file gives the fallback" "$BODY_COPY" "The job wrote no summary"
  assert_file_not_contains "without REPORT_SHA there is no Commit: line" "$BODY_COPY" "^Commit:"
}

test_a_long_summary_is_cut() {
  echo "Test: a summary over 60000 bytes is cut below GitHub's body limit"

  local long="$TMP_DIR/long.md"
  {
    printf '## Flux path run summary\n\n'
    head -c 70000 /dev/zero | tr '\0' 'x'
  } >"$long"
  run_report SUMMARY_FILE="$long"
  assert_eq "the script exits 0" "0" "$RC"
  assert_file_contains_fixed "the body starts the summary" "$BODY_COPY" "## Flux path run summary"
  assert_file_contains_fixed "and says where it was cut" "$BODY_COPY" \
    "(The summary is cut at 60000 bytes here. The run has all of it.)"
  local size
  size="$(wc -c <"$BODY_COPY" | tr -d ' ')"
  assert_eq "the body of $size bytes stays within 65536" "yes" \
    "$([ "$size" -le 65536 ] && echo yes || echo no)"

  run_report
  assert_file_not_contains "a short summary is not cut" "$BODY_COPY" "is cut at"
}

# rows <count> <prefix> — <count> lines of 100 bytes, each <prefix> and a run
# of a's, so a summary reaches a byte mark in known steps.
rows() {
  local i=0 pad
  pad="$(head -c $((99 - ${#2})) /dev/zero | tr '\0' 'a')"
  while [ "$i" -lt "$1" ]; do
    printf '%s%s\n' "$2" "$pad"
    i=$((i + 1))
  done
}

test_the_cut_keeps_markdown_closed() {
  echo "Test: the cut keeps whole lines and closes a fence it leaves open"

  local file="$TMP_DIR/cut.md" at
  {
    printf '## Flux path run summary\n\n| type | status | reason | message |\n| --- | --- | --- | --- |\n'
    rows 585 '| Filler | True | Ok | '
    printf '| Synced | True | Ok | `kept` |\n'
    printf '| Ready | False | Failed | `%s @someone %s` |\n' \
      "$(head -c 1000 /dev/zero | tr '\0' 'x')" "$(head -c 2000 /dev/zero | tr '\0' 'y')"
  } >"$file"
  at="$(grep -bo '@someone' "$file" | cut -d: -f1)"
  assert_eq "the fixture's @someone at byte $at lies before the cut, its row ends after it" "yes" \
    "$([ "$at" -lt 60000 ] && [ "$(wc -c <"$file")" -gt 60000 ] && echo yes || echo no)"
  run_report SUMMARY_FILE="$file"
  assert_eq "the script exits 0" "0" "$RC"
  assert_eq "no line holds @someone without the backtick that closes its code span" "" \
    "$(grep '@someone' "$BODY_COPY" | grep -v '@someone[^`]*`')"
  assert_file_contains_fixed "the last whole row before the cut stays" "$BODY_COPY" \
    '| Synced | True | Ok | `kept` |'

  {
    printf '## Flux path run summary\n\n### HelmReleases\n\n```text\n'
    rows 620 'openstack   keystone   False   '
    printf '```\n'
  } >"$file"
  run_report SUMMARY_FILE="$file"
  assert_eq "the script exits 0" "0" "$RC"
  assert_eq "a fence the cut left open is closed before the note" "0" \
    "$(awk '/^```/ { n++ } /is cut at/ { print n % 2; exit }' "$BODY_COPY")"
}

test_required_inputs() {
  echo "Test: an empty required input exits 1 before any gh call"

  local name
  for name in REPORT_TITLE REPORT_RESULT RUN_URL; do
    run_report "$name="
    assert_eq "an empty $name exits 1" "1" "$RC"
    assert_contains "naming $name" "$OUT" "ERROR: $name is empty"
    assert_eq "with no gh call" "" "$(cat "$GH_LOG")"
  done
}

test_a_failing_listing_files_nothing() {
  echo "Test: a failing or unreadable listing exits 1 and writes nothing"

  run_report STUB_LIST_FAIL=1
  assert_eq "a failing listing exits 1" "1" "$RC"
  assert_contains "with gh's message" "$OUT" "HTTP 502"
  assert_eq "and no create or comment" "0 0" "$(writes create) $(writes comment)"

  local listing
  for listing in 'not json' '' '{}'; do
    run_report STUB_ISSUES="$listing"
    assert_eq "a listing of '$listing' exits 1" "1" "$RC"
    assert_eq "and no create or comment" "0 0" "$(writes create) $(writes comment)"
  done
}

test_a_failing_write() {
  echo "Test: a failing create or comment exits 1"

  run_report STUB_CREATE_FAIL=1
  assert_eq "a failing create exits 1" "1" "$RC"
  assert_contains "with gh's message" "$OUT" "HTTP 403"

  run_report STUB_COMMENT_FAIL=1 STUB_ISSUES="[$(issue 42 "$TITLE")]"
  assert_eq "a failing comment exits 1" "1" "$RC"
}

test_the_script_is_executable() {
  echo "Test: the script is executable"

  if [ -x "$REPORT_SH" ]; then
    assert_eq "hack/ci-report-red-run.sh is executable" "yes" "yes"
  else
    assert_eq "hack/ci-report-red-run.sh is executable" "yes" "no"
  fi
}

if command -v jq >/dev/null 2>&1; then
  make_stub
  test_no_open_issue_creates_one
  test_an_open_issue_gets_a_comment
  test_only_the_bots_issue_matches
  test_the_body_without_a_summary
  test_a_long_summary_is_cut
  test_the_cut_keeps_markdown_closed
  test_required_inputs
  test_a_failing_listing_files_nothing
  test_a_failing_write
else
  echo "  SKIP: jq not installed (the script matches the listing with it)"
  SKIP=$((SKIP + 1))
fi
test_the_script_is_executable

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
