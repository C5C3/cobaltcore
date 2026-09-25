#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the debug-e2e-failure collector downloads a failed job's log on
# every gh it meets, and says why when it cannot.
#
# gh 2.97.0 and later refuse to print a raw response that carries terminal
# escape sequences unless --allow-escape-sequences is passed, and every
# Actions job log carries colour codes. The collector used to discard gh's
# error and report every refused log as "HTTP 404: the runner never uploaded
# one": on run 35905784592 both failed e2e-chaos jobs read as log-less while
# the endpoint served 1.3 MB and 0.85 MB, and the signature scan ran on an
# empty file. Older gh does not know the flag and rejects it, so the cases
# below pin both sides, plus gh's own message on a fetch that fails.
#
# gh is a stub that answers the calls the collector makes for one failed job;
# GH_STUB_MODE picks how the job-log endpoint behaves.
#
# Usage: bash tests/unit/ci/collect_e2e_failure_job_logs_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
COLLECTOR="$PROJECT_ROOT/.claude/skills/debug-e2e-failure/scripts/collect-e2e-failure.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

ESC="$(printf '\033')"
REFUSAL="the response contains terminal escape sequences; pass --allow-escape-sequences to output it anyway"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# write_gh_stub <path> — Writes a gh stand-in for run 1 of o/r with one
# failed job, 101 "e2e-chaos (keystone)". GH_STUB_MODE sets the job-log
# endpoint's behaviour:
#   new      gh >= 2.97: --help lists the flag; the log is refused without it
#   old      gh <  2.97: the flag is an unknown flag; the log prints without it
#   missing  the endpoint answers 404 (a runner that never uploaded a log)
#   refused  gh >= 2.97 whose --help does not list the flag: always refused
write_gh_stub() {
  cat >"$1" <<'STUB'
#!/bin/bash
mode="${GH_STUB_MODE:?GH_STUB_MODE is not set}"
refusal="the response contains terminal escape sequences; pass --allow-escape-sequences to output it anyway"
allow=0
args=()
for a in "$@"; do
  if [ "$a" = "--allow-escape-sequences" ]; then allow=1; else args+=("$a"); fi
done
flag_known=0
case "$mode" in new|missing) flag_known=1 ;; esac
if [ "$allow" = 1 ] && [ "$flag_known" = 0 ]; then
  echo "unknown flag: --allow-escape-sequences" >&2
  exit 1
fi
set -- "${args[@]}"
case "$*" in
  "auth status") ;;
  "run view 1 --repo o/r --json status,"*)
    printf 'completed\037failure\0371\037pull_request\037topic\037https://github.invalid/o/r/actions/runs/1\037stub run\n' ;;
  "run view 1 --repo o/r --json jobs "*)
    printf '101\te2e-chaos (keystone)\tfailure\t5\tRun chainsaw\tSet up job=success\n' ;;
  "api --help")
    echo "Usage:  gh api <endpoint> [flags]"
    echo "FLAGS"
    if [ "$flag_known" = 1 ]; then
      echo "      --allow-escape-sequences   Allow printing terminal escape sequences"
    fi
    echo "  -H, --header key:value   Add a HTTP request header in key:value format" ;;
  "api repos/o/r/actions/jobs/101/logs")
    case "$mode" in
      missing)
        echo '{"message":"Not Found","status":"404"}'
        echo "gh: Not Found (HTTP 404)" >&2
        exit 1 ;;
      refused)
        echo "$refusal" >&2
        exit 1 ;;
      new)
        if [ "$allow" = 0 ]; then echo "$refusal" >&2; exit 1; fi ;;
    esac
    printf '2026-09-20T10:00:00.0000000Z \033[36;1mmake e2e-chaos\033[0m\n'
    printf '2026-09-20T10:05:00.0000000Z \033[31m--- FAIL: chainsaw/stub-pod-kill (61.20s)\033[0m\n' ;;
  "api repos/o/r/actions/jobs/101 --jq "*) echo "runner-1" ;;
  "api repos/o/r/check-runs/101/annotations --jq "*) ;;
  "api repos/o/r/actions/runs/1/artifacts"*) ;;
  *)
    echo "gh stub: unexpected call: gh $*" >&2
    exit 99 ;;
esac
STUB
  chmod +x "$1"
}

# run_collect <tmp> <mode> — Runs the collector against the stub with the
# evidence under <tmp>/out. Echoes combined stdout/stderr; returns its exit code.
run_collect() {
  local tmp="$1" mode="$2"
  (
    export PATH="$tmp/bin:/usr/bin:/bin"
    GH_STUB_MODE="$mode" bash "$COLLECTOR" --run 1 --repo o/r --out "$tmp/out"
  ) 2>&1
}

# assert_log_downloaded <mode> — The job's log reaches failed-jobs.log with its
# colour codes stripped, and the collector reports its size.
assert_log_downloaded() {
  local mode="$1" tmp output exit_code
  tmp="$(mktemp -d)"
  mkdir -p "$tmp/bin"
  write_gh_stub "$tmp/bin/gh"

  output="$(run_collect "$tmp" "$mode")"
  exit_code=$?

  assert_eq "exits 0" "0" "$exit_code"
  assert_contains "reports the downloaded log" "$output" "e2e-chaos (keystone): 2 lines"
  assert_not_contains "does not report the log as missing" "$output" "no log"
  assert_file_contains_fixed "the chainsaw failure marker reaches failed-jobs.log" \
    "$tmp/out/failed-jobs.log" "--- FAIL: chainsaw/stub-pod-kill"
  assert_eq "failed-jobs.log carries no escape byte" "0" \
    "$(grep -c "$ESC" "$tmp/out/failed-jobs.log")"
  assert_contains "the suite mapping sees the failed test" "$output" "test 'stub-pod-kill'"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test A: gh 2.97.0 and later
# ---------------------------------------------------------------------------
test_new_gh_downloads_the_log() {
  echo "Test: a gh that refuses escape sequences still hands over the log"
  assert_log_downloaded new
}

# ---------------------------------------------------------------------------
# Test B: gh older than 2.97.0
# ---------------------------------------------------------------------------
test_old_gh_downloads_the_log() {
  echo "Test: a gh without --allow-escape-sequences is not passed the flag"
  assert_log_downloaded old
}

# ---------------------------------------------------------------------------
# Test C: the runner uploaded no log
# ---------------------------------------------------------------------------
test_missing_log_reports_gh_error() {
  echo "Test: a missing log is reported with gh's HTTP 404"

  local tmp output exit_code
  tmp="$(mktemp -d)"
  mkdir -p "$tmp/bin"
  write_gh_stub "$tmp/bin/gh"

  output="$(run_collect "$tmp" missing)"
  exit_code=$?

  assert_eq "exits 0" "0" "$exit_code"
  assert_contains "prints gh's error for the job" \
    "$output" "e2e-chaos (keystone): no log (gh: Not Found (HTTP 404))"
  assert_contains "lists the job as log-less" \
    "$output" "failed without a downloadable log: e2e-chaos (keystone) (job 101)"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test D: gh refused the log
# ---------------------------------------------------------------------------
test_refused_log_reports_gh_error() {
  echo "Test: a refused log is reported with gh's reason, not as a 404"

  local tmp output exit_code
  tmp="$(mktemp -d)"
  mkdir -p "$tmp/bin"
  write_gh_stub "$tmp/bin/gh"

  output="$(run_collect "$tmp" refused)"
  exit_code=$?

  assert_eq "exits 0" "0" "$exit_code"
  assert_contains "prints gh's refusal for the job" \
    "$output" "e2e-chaos (keystone): no log ($REFUSAL)"
  assert_not_contains "does not claim a 404" "$output" "HTTP 404"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_new_gh_downloads_the_log
test_old_gh_downloads_the_log
test_missing_log_reports_gh_error
test_refused_log_reports_gh_error

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
