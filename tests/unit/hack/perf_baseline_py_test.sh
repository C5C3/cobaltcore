#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/perf-baseline.py on hand-written workload logs and JSON files:
#   1. token: the nearest-rank p50 and p99 of the ok issue and validate
#      records, the sample and failure counts and meta.release; lines that
#      are not records are skipped and counted, records of other streams are
#      left out; too few samples or one failed record exit 1 with the JSON
#      written; an op whose records all failed has null percentiles, and at
#      most ten offending records are printed; an empty or missing log exits 2.
#   2. compare: 25 % above the baseline is a REGRESSION (exit 1), 15 % and
#      exactly 20 % are ok; .samples and .failed rows are printed and never
#      judged; a key absent from the measured files, a measured null, a
#      release mismatch and an unparsable file exit 2, as do a baseline
#      without recorded.release, a metrics object or numeric values, a
#      measured file without metrics or with a string value, and a negative,
#      nan or inf tolerance.
#   3. write-baseline: the merged metrics under a recorded block, sorted keys
#      and one trailing newline; a release mismatch, a null and a key in two
#      files exit 2.
#
# Usage: bash tests/unit/hack/perf_baseline_py_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
TOOL="$PROJECT_ROOT/hack/perf-baseline.py"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# tool <args...> — run perf-baseline.py; OUT holds stdout and stderr, RC the
# exit code.
tool() {
  RC=0
  OUT="$(python3 "$TOOL" "$@" 2>&1)" || RC=$?
}

# record <op> <ok> <ms> [stream] — print one workload record line.
record() {
  printf '{"t": 1760000000.0, "s": "%s", "op": "%s", "ok": %s, "code": 201, "ms": %s, "msg": ""}\n' \
    "${4:-token}" "$1" "$2" "$3"
}

# token_log <file> — READY, ten ok issue records of 1..10 ms and ten ok
# validate records of 11..20 ms, in cycle order, and one http record.
token_log() {
  local i
  {
    echo "READY token"
    for i in 1 2 3 4 5 6 7 8 9 10; do
      record issue true "$i"
      record validate true "$((i + 10))"
    done
    record get true 999 http
  } >"$1"
}

# json_field <file> <python expression over d> — print the value as JSON.
json_field() {
  python3 -c 'import json, sys; d = json.load(open(sys.argv[1])); print(json.dumps(eval(sys.argv[2])))' "$1" "$2"
}

# measured <file> <release> <json metrics> — write a measured file.
measured() {
  printf '{"meta": {"release": "%s", "measured_at": "2026-10-20T01:30:00+00:00"}, "metrics": %s}\n' "$2" "$3" >"$1"
}

# baseline <file> <release> <json metrics> — write a baseline file.
baseline() {
  printf '{"recorded": {"release": "%s", "commit": "abc", "run": "https://example.invalid/runs/1", "date": "2026-10-20"}, "metrics": %s}\n' "$2" "$3" >"$1"
}

# ---------------------------------------------------------------------------
# token
# ---------------------------------------------------------------------------

test_token_writes_the_percentiles() {
  echo "Test: token writes the nearest-rank percentiles, counts and meta.release"

  token_log "$TMP_DIR/ok.log"
  local out="$TMP_DIR/token/nested/ok.json"
  tool token --log "$TMP_DIR/ok.log" --release 2026.1 --min-samples 5 --out "$out"
  assert_eq "token exits 0" "0" "$RC"
  assert_contains "the READY line is the one skipped line" "$OUT" "skipped 1 line(s)"
  assert_contains "the issue summary is printed" "$OUT" "token.issue: 10 ok, 0 failed, p50=5 ms, p99=10 ms"
  if [ ! -f "$out" ]; then
    echo "  FAIL: $out was not written"
    FAIL=$((FAIL + 1))
    return
  fi
  assert_eq "the eight token metrics" \
    '{"token.issue.failed": 0, "token.issue.p50_ms": 5, "token.issue.p99_ms": 10, "token.issue.samples": 10, "token.validate.failed": 0, "token.validate.p50_ms": 15, "token.validate.p99_ms": 20, "token.validate.samples": 10}' \
    "$(python3 -c 'import json, sys; print(json.dumps(json.load(open(sys.argv[1]))["metrics"], sort_keys=True))' "$out")"
  assert_eq "meta.release is --release" '"2026.1"' "$(json_field "$out" 'd["meta"]["release"]')"
  assert_eq "meta.token_records counts the token records, not the http one" "20" \
    "$(json_field "$out" 'd["meta"]["token_records"]')"
  assert_contains "meta.measured_at is a UTC timestamp" "$(json_field "$out" 'd["meta"]["measured_at"]')" "+00:00"
}

test_token_skips_lines_that_are_not_records() {
  echo "Test: token skips lines that are not JSON records and counts them"

  {
    echo "READY token"
    record issue true 1
    echo "garbage"
    record validate true 11
    echo "{not json"
    echo '["a", "list"]'
    echo '{"s": "token", "op": "issue", "ok": true}'
    echo ""
    for i in 2 3 4 5 6 7 8 9 10; do
      record issue true "$i"
      record validate true "$((i + 10))"
    done
    echo "DONE"
  } >"$TMP_DIR/noisy.log"
  local out="$TMP_DIR/noisy.json"
  tool token --log "$TMP_DIR/noisy.log" --release 2026.1 --min-samples 5 --out "$out"
  assert_eq "token exits 0" "0" "$RC"
  assert_contains "six lines are skipped, the blank one is not counted" "$OUT" "skipped 6 line(s)"
  assert_eq "the percentiles come from the valid records" "[5, 10, 15, 20]" \
    "$(json_field "$out" '[d["metrics"][k] for k in ("token.issue.p50_ms", "token.issue.p99_ms", "token.validate.p50_ms", "token.validate.p99_ms")]')"
}

test_token_fails_on_a_failed_record() {
  echo "Test: one failed validate record exits 1 and the JSON is still written"

  token_log "$TMP_DIR/failed.log"
  record validate false 15000 >>"$TMP_DIR/failed.log"
  local out="$TMP_DIR/failed.json"
  rm -f "$out"
  tool token --log "$TMP_DIR/failed.log" --release 2026.1 --min-samples 5 --out "$out"
  assert_eq "token exits 1" "1" "$RC"
  assert_contains "the failure names validate and 1 failed" "$OUT" "FAIL: token.validate: 1 failed record(s)"
  assert_contains "the offending record is printed" "$OUT" 'offending: {"t": 1760000000.0, "s": "token", "op": "validate", "ok": false'
  assert_not_contains "issue has no failure" "$OUT" "FAIL: token.issue"
  if [ -f "$out" ]; then
    assert_eq "the JSON counts the failed record" "1" "$(json_field "$out" 'd["metrics"]["token.validate.failed"]')"
    assert_eq "the failed record's ms is not in the p99" "20" "$(json_field "$out" 'd["metrics"]["token.validate.p99_ms"]')"
  else
    echo "  FAIL: $out was not written"
    FAIL=$((FAIL + 1))
  fi
}

test_token_fails_on_too_few_samples() {
  echo "Test: fewer ok records than --min-samples exits 1 naming the op and its count"

  token_log "$TMP_DIR/short.log"
  local out="$TMP_DIR/short.json"
  rm -f "$out"
  tool token --log "$TMP_DIR/short.log" --release 2026.1 --min-samples 300 --out "$out"
  assert_eq "token exits 1" "1" "$RC"
  assert_contains "issue is named with its count" "$OUT" "FAIL: token.issue: 10 ok record(s), want at least 300"
  assert_contains "validate is named with its count" "$OUT" "FAIL: token.validate: 10 ok record(s), want at least 300"
  if [ -f "$out" ]; then
    echo "  PASS: the JSON is written"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $out was not written"
    FAIL=$((FAIL + 1))
  fi
}

test_token_input_errors() {
  echo "Test: an empty log, a log without token records and a missing log exit 2"

  : >"$TMP_DIR/empty.log"
  tool token --log "$TMP_DIR/empty.log" --release 2026.1 --min-samples 1 --out "$TMP_DIR/empty.json"
  assert_eq "an empty log exits 2" "2" "$RC"
  assert_contains "the reason is printed" "$OUT" "token: the log holds no token record"
  if [ -e "$TMP_DIR/empty.json" ]; then
    echo "  FAIL: an empty log wrote a JSON file"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: an empty log writes no JSON file"
    PASS=$((PASS + 1))
  fi

  { echo "READY http"; record get true 3 http; } >"$TMP_DIR/http.log"
  tool token --log "$TMP_DIR/http.log" --release 2026.1 --min-samples 1 --out "$TMP_DIR/http.json"
  assert_eq "a log of another stream exits 2" "2" "$RC"
  assert_contains "the reason is printed" "$OUT" "the log holds no token record"

  tool token --log "$TMP_DIR/absent.log" --release 2026.1 --min-samples 1 --out "$TMP_DIR/absent.json"
  assert_eq "a missing log exits 2" "2" "$RC"
  assert_contains "the reason names the file" "$OUT" "cannot open $TMP_DIR/absent.log"

  tool token --log "$TMP_DIR/ok.log" --release 2026.1 --min-samples -1 --out "$TMP_DIR/neg.json"
  assert_eq "a negative --min-samples exits 2" "2" "$RC"
}

test_token_op_without_an_ok_record() {
  echo "Test: an op whose records all failed writes null percentiles and at most ten offending records"

  local i
  {
    for i in 1 2 3 4 5 6 7 8 9 10; do record issue true "$i"; done
    for i in 1 2 3 4 5 6 7 8 9 10 11 12; do record validate false 15000; done
  } >"$TMP_DIR/all-failed.log"
  local out="$TMP_DIR/all-failed.json"
  tool token --log "$TMP_DIR/all-failed.log" --release 2026.1 --min-samples 5 --out "$out"
  assert_eq "token exits 1" "1" "$RC"
  assert_eq "validate's p50 and p99 are null" "[null, null]" \
    "$(json_field "$out" '[d["metrics"]["token.validate.p50_ms"], d["metrics"]["token.validate.p99_ms"]]' 2>&1)"
  assert_eq "twelve failed records print ten offending lines" "10" "$(grep -c '^offending: ' <<<"$OUT")"
}

# ---------------------------------------------------------------------------
# compare
# ---------------------------------------------------------------------------

test_compare_judges_the_tolerance() {
  echo "Test: compare flags 25 % above the baseline, passes 15 % and exactly 20 %"

  baseline "$TMP_DIR/base.json" 2026.1 '{"token.issue.p50_ms": 100}'
  measured "$TMP_DIR/m25.json" 2026.1 '{"token.issue.p50_ms": 125}'
  tool compare --baseline "$TMP_DIR/base.json" --tolerance-percent 20 --measured "$TMP_DIR/m25.json"
  assert_eq "25 % above exits 1" "1" "$RC"
  assert_contains "the row is a REGRESSION with its delta" "$OUT" "+25.0 %  REGRESSION"
  assert_contains "the summary counts the regression" "$OUT" "compare: 1 regression(s) above 20 %"

  measured "$TMP_DIR/m15.json" 2026.1 '{"token.issue.p50_ms": 115}'
  tool compare --baseline "$TMP_DIR/base.json" --tolerance-percent 20 --measured "$TMP_DIR/m15.json"
  assert_eq "15 % above exits 0" "0" "$RC"
  assert_contains "the row is ok with its delta" "$OUT" "+15.0 %  ok"
  assert_contains "the summary says every metric is within" "$OUT" "compare: every judged metric is within 20 %"

  measured "$TMP_DIR/m20.json" 2026.1 '{"token.issue.p50_ms": 120}'
  tool compare --baseline "$TMP_DIR/base.json" --tolerance-percent 20 --measured "$TMP_DIR/m20.json"
  assert_eq "exactly 20 % above exits 0" "0" "$RC"

  baseline "$TMP_DIR/base-s.json" 2026.1 '{"reconcile.1.e2e_p95_s": 0.2}'
  measured "$TMP_DIR/ms.json" 2026.1 '{"reconcile.1.e2e_p95_s": 0.3}'
  tool compare --baseline "$TMP_DIR/base-s.json" --tolerance-percent 20 --measured "$TMP_DIR/ms.json"
  assert_eq "a _s key 50 % above exits 1" "1" "$RC"
}

test_compare_prints_counts_without_judging_them() {
  echo "Test: compare prints .samples and .failed rows and never judges them"

  baseline "$TMP_DIR/base-c.json" 2026.1 '{"token.issue.p50_ms": 100, "token.issue.samples": 300, "token.issue.failed": 0}'
  measured "$TMP_DIR/mc.json" 2026.1 '{"token.issue.p50_ms": 100, "token.issue.samples": 900, "token.issue.failed": 3, "reconcile.3.e2e_p95_s": 0.5}'
  tool compare --baseline "$TMP_DIR/base-c.json" --tolerance-percent 20 --measured "$TMP_DIR/mc.json"
  assert_eq "compare exits 0" "0" "$RC"
  assert_eq "the samples row has no verdict" "token.issue.samples 300 900 +200.0 % -" \
    "$(grep '^token.issue.samples' <<<"$OUT" | tr -s ' ')"
  assert_eq "the failed row has no verdict and no delta" "token.issue.failed 0 3 n/a -" \
    "$(grep '^token.issue.failed' <<<"$OUT" | tr -s ' ')"
  assert_eq "a key only the measured files carry is printed as new" "reconcile.3.e2e_p95_s - 0.5 - new" \
    "$(grep '^reconcile.3.e2e_p95_s' <<<"$OUT" | tr -s ' ')"
}

test_compare_merges_measured_files() {
  echo "Test: compare merges the metrics of several measured files"

  baseline "$TMP_DIR/base-m.json" 2026.1 '{"token.issue.p50_ms": 100, "reconcile.1.e2e_p50_s": 0.01}'
  measured "$TMP_DIR/mt.json" 2026.1 '{"token.issue.p50_ms": 101}'
  measured "$TMP_DIR/mr.json" 2026.1 '{"reconcile.1.e2e_p50_s": 0.011}'
  tool compare --baseline "$TMP_DIR/base-m.json" --tolerance-percent 20 --measured "$TMP_DIR/mt.json" "$TMP_DIR/mr.json"
  assert_eq "compare exits 0" "0" "$RC"
  assert_contains "the reconcile row is judged" "$OUT" "+10.0 %  ok"
}

test_compare_input_errors() {
  echo "Test: compare exits 2 on a missing key, a null, a release mismatch and a bad file"

  baseline "$TMP_DIR/base-e.json" 2026.1 '{"token.issue.p50_ms": 100, "token.validate.p99_ms": 40}'

  measured "$TMP_DIR/me-missing.json" 2026.1 '{"token.issue.p50_ms": 100}'
  tool compare --baseline "$TMP_DIR/base-e.json" --tolerance-percent 20 --measured "$TMP_DIR/me-missing.json"
  assert_eq "a missing baseline key exits 2" "2" "$RC"
  assert_contains "the missing key is named" "$OUT" "baseline key token.validate.p99_ms is missing from the measured files"

  measured "$TMP_DIR/me-null.json" 2026.1 '{"token.issue.p50_ms": 100, "token.validate.p99_ms": null}'
  tool compare --baseline "$TMP_DIR/base-e.json" --tolerance-percent 20 --measured "$TMP_DIR/me-null.json"
  assert_eq "a measured null exits 2" "2" "$RC"
  assert_contains "the null key is named" "$OUT" "token.validate.p99_ms is null in the measured files"

  measured "$TMP_DIR/me-release.json" 2026.2 '{"token.issue.p50_ms": 100, "token.validate.p99_ms": 40}'
  tool compare --baseline "$TMP_DIR/base-e.json" --tolerance-percent 20 --measured "$TMP_DIR/me-release.json"
  assert_eq "a release mismatch exits 2" "2" "$RC"
  assert_contains "both releases are named" "$OUT" "is of release 2026.2, the baseline $TMP_DIR/base-e.json of release 2026.1"

  echo "{not json" >"$TMP_DIR/bad.json"
  tool compare --baseline "$TMP_DIR/bad.json" --tolerance-percent 20 --measured "$TMP_DIR/me-null.json"
  assert_eq "an unparsable baseline exits 2" "2" "$RC"
  assert_contains "the file is named" "$OUT" "cannot read $TMP_DIR/bad.json"

  tool compare --baseline "$TMP_DIR/base-e.json" --tolerance-percent 20 --measured "$TMP_DIR/bad.json"
  assert_eq "an unparsable measured file exits 2" "2" "$RC"

  tool compare --baseline "$TMP_DIR/absent-base.json" --tolerance-percent 20 --measured "$TMP_DIR/me-null.json"
  assert_eq "a missing baseline exits 2" "2" "$RC"
  assert_contains "the missing baseline is named" "$OUT" "cannot read $TMP_DIR/absent-base.json"

  echo '{"metrics": {"token.issue.p50_ms": 100}}' >"$TMP_DIR/no-meta.json"
  tool compare --baseline "$TMP_DIR/base-e.json" --tolerance-percent 20 --measured "$TMP_DIR/no-meta.json"
  assert_eq "a measured file without meta.release exits 2" "2" "$RC"
  assert_contains "the reason is printed" "$OUT" "has no meta.release"
}

test_compare_rejects_malformed_files() {
  echo "Test: compare exits 2 on a malformed baseline or measured file"

  # One case per line: what is wrong|baseline|measured file|reason printed.
  local name base meas want
  while IFS='|' read -r name base meas want; do
    printf '%s\n' "$base" >"$TMP_DIR/case-base.json"
    printf '%s\n' "$meas" >"$TMP_DIR/case-measured.json"
    tool compare --baseline "$TMP_DIR/case-base.json" --tolerance-percent 20 --measured "$TMP_DIR/case-measured.json"
    assert_eq "$name exits 2" "2" "$RC"
    assert_contains "$name is named" "$OUT" "$want"
  done <<'EOF'
a baseline without recorded.release|{"metrics": {"token.issue.p50_ms": 100}}|{"meta": {"release": "2026.1"}, "metrics": {"token.issue.p50_ms": 100}}|has no recorded.release
a baseline without a metrics object|{"recorded": {"release": "2026.1"}}|{"meta": {"release": "2026.1"}, "metrics": {"token.issue.p50_ms": 100}}|has no metrics object
a baseline string value|{"recorded": {"release": "2026.1"}, "metrics": {"token.issue.p50_ms": "x"}}|{"meta": {"release": "2026.1"}, "metrics": {"token.issue.p50_ms": 100}}|baseline key token.issue.p50_ms is not a number
a measured file without a metrics object|{"recorded": {"release": "2026.1"}, "metrics": {"token.issue.p50_ms": 100}}|{"meta": {"release": "2026.1"}}|case-measured.json has no metrics object
a measured string value|{"recorded": {"release": "2026.1"}, "metrics": {"token.issue.p50_ms": 100}}|{"meta": {"release": "2026.1"}, "metrics": {"token.issue.p50_ms": "x"}}|token.issue.p50_ms in
EOF
}

test_compare_rejects_a_bad_tolerance() {
  echo "Test: a negative, nan or inf --tolerance-percent exits 2"

  baseline "$TMP_DIR/base-t.json" 2026.1 '{"token.issue.p50_ms": 100}'
  measured "$TMP_DIR/mt-ok.json" 2026.1 '{"token.issue.p50_ms": 100}'
  local tolerance
  for tolerance in -1 nan inf; do
    tool compare --baseline "$TMP_DIR/base-t.json" --tolerance-percent "$tolerance" --measured "$TMP_DIR/mt-ok.json"
    assert_eq "--tolerance-percent $tolerance exits 2" "2" "$RC"
    assert_contains "--tolerance-percent $tolerance is refused" "$OUT" "is not a finite number of at least 0"
  done
}

# ---------------------------------------------------------------------------
# write-baseline
# ---------------------------------------------------------------------------

test_write_baseline_merges_two_files() {
  echo "Test: write-baseline merges two measured files under a recorded block"

  measured "$TMP_DIR/wt.json" 2026.1 '{"token.validate.p99_ms": 40, "token.issue.p50_ms": 12}'
  measured "$TMP_DIR/wr.json" 2026.1 '{"reconcile.1.e2e_p95_s": 0.0235}'
  local out="$TMP_DIR/written/baseline.json"
  tool write-baseline --measured "$TMP_DIR/wt.json" "$TMP_DIR/wr.json" --release 2026.1 \
    --commit 0123456789abcdef --run https://github.com/C5C3/cobaltcore/actions/runs/42 --out "$out"
  assert_eq "write-baseline exits 0" "0" "$RC"
  assert_contains "the file and the count are printed" "$OUT" "wrote $out (3 metrics)"
  if [ ! -f "$out" ]; then
    echo "  FAIL: $out was not written"
    FAIL=$((FAIL + 1))
    return
  fi
  assert_eq "metrics holds both files' keys" \
    '["reconcile.1.e2e_p95_s", "token.issue.p50_ms", "token.validate.p99_ms"]' \
    "$(json_field "$out" 'sorted(d["metrics"])')"
  assert_eq "recorded names release, commit and run" \
    '["2026.1", "0123456789abcdef", "https://github.com/C5C3/cobaltcore/actions/runs/42"]' \
    "$(json_field "$out" '[d["recorded"][k] for k in ("release", "commit", "run")]')"
  assert_eq "recorded.date is a UTC date" "1" \
    "$(json_field "$out" 'd["recorded"]["date"]' | grep -cE '^"[0-9]{4}-[0-9]{2}-[0-9]{2}"$')"
  assert_eq "the keys are sorted with a two-space indent and one trailing newline" "same" \
    "$(python3 -c 'import json, sys; b = open(sys.argv[1]).read(); print("same" if b == json.dumps(json.loads(b), indent=2, sort_keys=True) + "\n" else "differs")' "$out")"

  # The written file is a baseline compare accepts.
  tool compare --baseline "$out" --tolerance-percent 20 --measured "$TMP_DIR/wt.json" "$TMP_DIR/wr.json"
  assert_eq "compare accepts the written baseline" "0" "$RC"
}

test_write_baseline_input_errors() {
  echo "Test: write-baseline exits 2 on a release mismatch, a null and a duplicate key"

  measured "$TMP_DIR/w-old.json" 2026.2 '{"token.issue.p50_ms": 12}'
  tool write-baseline --measured "$TMP_DIR/w-old.json" --release 2026.1 --commit c --run r --out "$TMP_DIR/w1.json"
  assert_eq "a release mismatch exits 2" "2" "$RC"
  assert_contains "both releases are named" "$OUT" "is of release 2026.2, not 2026.1"

  measured "$TMP_DIR/w-null.json" 2026.1 '{"reconcile.1.sub_p50_s": null}'
  tool write-baseline --measured "$TMP_DIR/w-null.json" --release 2026.1 --commit c --run r --out "$TMP_DIR/w2.json"
  assert_eq "a null value exits 2" "2" "$RC"
  assert_contains "the null key is named" "$OUT" "reconcile.1.sub_p50_s is null"

  measured "$TMP_DIR/w-a.json" 2026.1 '{"token.issue.p50_ms": 12}'
  measured "$TMP_DIR/w-b.json" 2026.1 '{"token.issue.p50_ms": 13}'
  tool write-baseline --measured "$TMP_DIR/w-a.json" "$TMP_DIR/w-b.json" --release 2026.1 --commit c --run r --out "$TMP_DIR/w3.json"
  assert_eq "a key in two files exits 2" "2" "$RC"
  assert_contains "the key is named" "$OUT" "metric token.issue.p50_ms appears in"

  local f
  for f in w1 w2 w3; do
    if [ -e "$TMP_DIR/$f.json" ]; then
      echo "  FAIL: $f.json was written despite the input error"
      FAIL=$((FAIL + 1))
      return
    fi
  done
  echo "  PASS: no baseline is written on an input error"
  PASS=$((PASS + 1))
}

test_token_writes_the_percentiles
test_token_skips_lines_that_are_not_records
test_token_fails_on_a_failed_record
test_token_fails_on_too_few_samples
test_token_input_errors
test_token_op_without_an_ok_record
test_compare_judges_the_tolerance
test_compare_prints_counts_without_judging_them
test_compare_merges_measured_files
test_compare_input_errors
test_compare_rejects_malformed_files
test_compare_rejects_a_bad_tolerance
test_write_baseline_merges_two_files
test_write_baseline_input_errors

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
