#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the RESULT_JSON output of hack/perf-reconcile-benchmark.sh against a
# stub kubectl and a stub curl that serves two fixture scrapes:
#   - without RESULT_JSON the report on stdout is the golden block below,
#     byte for byte, so the nightly's opt-in changes nothing for a local run;
#   - with RESULT_JSON the report is the same, and the file holds the six
#     reconcile.1.* quantiles, the sub-reconciler ones null (no sample in the
#     window, printed nan), and the meta block perf-baseline.py reads;
#   - under a comma-decimal locale (de_DE) the report and the file still
#     carry decimal points;
#   - a red GATE_P95_SECONDS still leaves the file behind;
#   - the applied CR puts replicas under spec.deployment, the only place the
#     Keystone CRD has it;
#   - the CR is deleted after the scrapes, and the keystone-perf-token CR of
#     hack/perf-token-baseline.sh, which shares the name prefix, is neither
#     waited for nor deleted;
#   - the nightly's CR_COUNTS="1 3" writes the six quantiles of both counts
#     into one file, and the second count waits for and deletes all three CRs;
#   - no CR name and no database comes back in a later count, because the
#     MariaDB User and Grant of a deleted CR outlive it and a re-created CR of
#     the same name runs into their teardown.
#
# The fixture's end-to-end histogram has 5 reconciles in (0.005, 0.01] and 5
# in (0.01, 0.025]; the sub-reconciler histogram has the same counts in both
# scrapes.
#
# Usage: bash tests/unit/hack/perf_reconcile_benchmark_result_json_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
BENCH="$PROJECT_ROOT/hack/perf-reconcile-benchmark.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

STUB_BIN="$TMP_DIR/bin"
mkdir -p "$STUB_BIN"

# The stub kubectl keeps its state in $STUB_DIR: calls (one line per call),
# applied.yaml (the last manifest applied), applied-all.yaml (every manifest
# applied) and applied (set while its CRs exist).
cat >"$STUB_BIN/kubectl" <<'SH'
#!/bin/bash
echo "$*" >>"$STUB_DIR/calls"
args=" $* "
case "$args" in
  *" version "*) exit 0 ;;
  *" get deploy "*) exit 0 ;;
  *" apply -f "*)
    prev=""
    for a in "$@"; do
      if [ "$prev" = "-f" ]; then
        cp "$a" "$STUB_DIR/applied.yaml"
        cat "$a" >>"$STUB_DIR/applied-all.yaml"
      fi
      prev="$a"
    done
    touch "$STUB_DIR/applied"
    ;;
  *" get keystone -o name "*)
    # The CR of hack/perf-token-baseline.sh shares the keystone-perf- prefix.
    echo "keystone.keystone.openstack.c5c3.io/keystone-perf-token"
    if [ -f "$STUB_DIR/applied" ]; then
      sed -n 's|^  name: \(keystone-perf-[0-9]*-[0-9]*\)$|keystone.keystone.openstack.c5c3.io/\1|p' "$STUB_DIR/applied.yaml"
    fi
    ;;
  *" wait "*) exit 0 ;;
  *" port-forward "*) exec sleep 300 ;;
  *" delete "*) rm -f "$STUB_DIR/applied" ;;
  *) echo "stub kubectl: unexpected call: $*" >&2; exit 1 ;;
esac
exit 0
SH

# The stub curl answers the readiness probe, then the before scrape, then the
# after scrape, and starts over for the next count; it counts its calls in
# $STUB_DIR/curl.calls.
cat >"$STUB_BIN/curl" <<'SH'
#!/bin/bash
n=$(( $(cat "$STUB_DIR/curl.calls" 2>/dev/null || echo 0) + 1 ))
echo "$n" >"$STUB_DIR/curl.calls"
echo "curl" >>"$STUB_DIR/calls"
case "$(( (n - 1) % 3 ))" in
  0) exit 0 ;;
  1) cat "$STUB_DIR/before.txt" ;;
  2) cat "$STUB_DIR/after.txt" ;;
esac
SH
chmod +x "$STUB_BIN/kubectl" "$STUB_BIN/curl"

# new_stub — empty the stub state and write the two fixture scrapes.
new_stub() {
  rm -rf "$TMP_DIR/state"
  mkdir -p "$TMP_DIR/state"
  : >"$TMP_DIR/state/calls"
  cat >"$TMP_DIR/state/before.txt" <<'EOF'
# HELP controller_runtime_reconcile_time_seconds Length of time per reconciliation per controller
# TYPE controller_runtime_reconcile_time_seconds histogram
controller_runtime_reconcile_time_seconds_bucket{controller="keystone",le="0.005"} 0
controller_runtime_reconcile_time_seconds_bucket{controller="keystone",le="0.01"} 0
controller_runtime_reconcile_time_seconds_bucket{controller="keystone",le="0.025"} 0
controller_runtime_reconcile_time_seconds_bucket{controller="keystone",le="+Inf"} 0
keystone_operator_reconcile_duration_seconds_bucket{sub_reconciler="database",le="0.01"} 3
keystone_operator_reconcile_duration_seconds_bucket{sub_reconciler="database",le="+Inf"} 3
EOF
  cat >"$TMP_DIR/state/after.txt" <<'EOF'
# HELP controller_runtime_reconcile_time_seconds Length of time per reconciliation per controller
# TYPE controller_runtime_reconcile_time_seconds histogram
controller_runtime_reconcile_time_seconds_bucket{controller="keystone",le="0.005"} 0
controller_runtime_reconcile_time_seconds_bucket{controller="keystone",le="0.01"} 5
controller_runtime_reconcile_time_seconds_bucket{controller="keystone",le="0.025"} 10
controller_runtime_reconcile_time_seconds_bucket{controller="keystone",le="+Inf"} 10
keystone_operator_reconcile_duration_seconds_bucket{sub_reconciler="database",le="0.01"} 3
keystone_operator_reconcile_duration_seconds_bucket{sub_reconciler="database",le="+Inf"} 3
EOF
}

# bench [VAR=value...] — run the benchmark with the stubs first on PATH, for
# one CR unless CR_COUNTS is given; BENCH_OUT holds stdout, BENCH_ERR stderr,
# BENCH_RC the exit code. The caller's locale passes through.
bench() {
  new_stub
  BENCH_RC=0
  env STUB_DIR="$TMP_DIR/state" PATH="$STUB_BIN:$PATH" \
    CR_COUNTS="1" SETTLE_SECONDS=0 IMAGE_TAG=2026.1 GATE_P95_SECONDS= RESULT_JSON= \
    "$@" bash "$BENCH" >"$TMP_DIR/stdout" 2>"$TMP_DIR/stderr" || BENCH_RC=$?
  BENCH_OUT="$(cat "$TMP_DIR/stdout")"
  BENCH_ERR="$(cat "$TMP_DIR/stderr")"
}

# json_field <file> <python expression over d> — print the expression's value
# as JSON, so null stays null and 0.01 stays 0.01.
json_field() {
  python3 -c 'import json, sys; d = json.load(open(sys.argv[1])); print(json.dumps(eval(sys.argv[2])))' "$1" "$2"
}

# The report of the fixture, as the script printed it before RESULT_JSON
# existed: a blank line, the heading, the two quantile lines, a blank line.
GOLDEN="$(printf '\n=== Reconcile latency for 1 CR(s) (settle 0s) ===\n  end-to-end (controller_runtime_reconcile_time_seconds): p50=0.0100s p95=0.0235s p99=0.0247s\n  sub-reconciler (keystone_operator_reconcile_duration_seconds): p50=nans p95=nans p99=nans\n\n')"

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_report_without_result_json_is_unchanged() {
  echo "Test: without RESULT_JSON the report is the golden block"

  bench
  assert_eq "the benchmark exits 0" "0" "$BENCH_RC"
  assert_eq "stdout is the golden report" "$GOLDEN" "$BENCH_OUT"
  assert_not_contains "nothing is said about a result file" "$BENCH_ERR" "wrote"
}

test_result_json_holds_the_six_quantiles() {
  echo "Test: RESULT_JSON holds the six reconcile.1.* quantiles and the meta block"

  local out="$TMP_DIR/nested/dir/r.json"
  bench RESULT_JSON="$out"
  assert_eq "the benchmark exits 0" "0" "$BENCH_RC"
  assert_eq "stdout is byte-identical to the run without RESULT_JSON" "$GOLDEN" "$BENCH_OUT"
  assert_contains "the file is logged on stderr" "$BENCH_ERR" "wrote $out"
  if [ ! -f "$out" ]; then
    echo "  FAIL: $out was not written"
    FAIL=$((FAIL + 1))
    return
  fi
  assert_eq "the metrics are the six reconcile.1.* keys" \
    '["reconcile.1.e2e_p50_s", "reconcile.1.e2e_p95_s", "reconcile.1.e2e_p99_s", "reconcile.1.sub_p50_s", "reconcile.1.sub_p95_s", "reconcile.1.sub_p99_s"]' \
    "$(json_field "$out" 'sorted(d["metrics"])')"
  assert_eq "e2e_p50_s is the printed p50" "0.01" "$(json_field "$out" 'd["metrics"]["reconcile.1.e2e_p50_s"]')"
  assert_eq "e2e_p95_s is the printed p95" "0.0235" "$(json_field "$out" 'd["metrics"]["reconcile.1.e2e_p95_s"]')"
  assert_eq "e2e_p99_s is the printed p99" "0.0247" "$(json_field "$out" 'd["metrics"]["reconcile.1.e2e_p99_s"]')"
  assert_eq "a nan quantile is written as null" "[null, null, null]" \
    "$(json_field "$out" '[d["metrics"]["reconcile.1.sub_p%d_s" % q] for q in (50, 95, 99)]')"
  assert_eq "meta.release is IMAGE_TAG" '"2026.1"' "$(json_field "$out" 'd["meta"]["release"]')"
  assert_eq "meta.cr_counts is CR_COUNTS" '"1"' "$(json_field "$out" 'd["meta"]["cr_counts"]')"
  assert_eq "meta.settle_seconds is SETTLE_SECONDS as a number" "0" "$(json_field "$out" 'd["meta"]["settle_seconds"]')"
  assert_not_empty "meta.measured_at is set" "$(json_field "$out" 'd["meta"]["measured_at"] or ""' | tr -d '"')"
}

test_a_red_gate_still_writes_the_file() {
  echo "Test: a red GATE_P95_SECONDS exits 1 and still writes RESULT_JSON"

  local out="$TMP_DIR/gate.json"
  bench RESULT_JSON="$out" GATE_P95_SECONDS=0.01
  assert_eq "the benchmark exits 1" "1" "$BENCH_RC"
  assert_contains "the gate names the p95" "$BENCH_ERR" "end-to-end p95 0.0235s exceeds gate 0.01s"
  if [ -f "$out" ]; then
    assert_eq "the file holds the p95 the gate judged" "0.0235" \
      "$(json_field "$out" 'd["metrics"]["reconcile.1.e2e_p95_s"]')"
  else
    echo "  FAIL: $out was not written on a red gate"
    FAIL=$((FAIL + 1))
  fi
}

test_a_comma_decimal_locale_keeps_decimal_points() {
  echo "Test: under a de_DE locale the report and RESULT_JSON keep decimal points"

  local loc
  loc="$(locale -a 2>/dev/null | grep -iE '^de_DE\.utf-?8$' | head -1)"
  if [ -z "$loc" ]; then
    echo "  SKIP: no de_DE UTF-8 locale is installed"
    SKIP=$((SKIP + 1))
    return
  fi
  local out="$TMP_DIR/de.json"
  bench RESULT_JSON="$out" LC_ALL="$loc"
  assert_eq "the benchmark exits 0" "0" "$BENCH_RC"
  assert_eq "stdout is the golden report" "$GOLDEN" "$BENCH_OUT"
  assert_eq "the file parses and holds the p95 as a number" "0.0235" \
    "$(json_field "$out" 'd["metrics"]["reconcile.1.e2e_p95_s"]' 2>&1)"
}

test_the_cr_sets_replicas_under_spec_deployment() {
  echo "Test: the applied CR sets spec.deployment.replicas, not spec.replicas"

  bench
  local manifest="$TMP_DIR/state/applied.yaml"
  if [ ! -f "$manifest" ]; then
    echo "  FAIL: no manifest was applied"
    FAIL=$((FAIL + 1))
    return
  fi
  assert_eq "spec.deployment.replicas is 1" "1" \
    "$(awk '/^  deployment:$/ { d = 1; next } d && /^    replicas:/ { print $2; exit } /^  [a-z]/ { d = 0 }' "$manifest")"
  assert_file_not_contains "no replicas directly under spec" "$manifest" '^  replicas:'
}

test_the_cr_is_deleted_after_the_scrapes() {
  echo "Test: the benchmark deletes its CR after the scrapes"

  bench
  local calls
  calls="$(cat "$TMP_DIR/state/calls")"
  assert_contains "the CR is deleted with --wait" "$calls" \
    "-n openstack delete keystone.keystone.openstack.c5c3.io/keystone-perf-1-1 --wait=true --timeout=600s"
  assert_not_contains "the token measurement's CR is neither waited for nor deleted" "$calls" \
    "keystone.keystone.openstack.c5c3.io/keystone-perf-token"
  local last_curl first_delete
  last_curl="$(grep -n '^curl$' "$TMP_DIR/state/calls" | tail -1 | cut -d: -f1)"
  first_delete="$(grep -n ' delete ' "$TMP_DIR/state/calls" | head -1 | cut -d: -f1)"
  if [[ "${last_curl:-0}" =~ ^[0-9]+$ && "${first_delete:-0}" =~ ^[0-9]+$ ]] &&
    [ "${first_delete:-0}" -gt "${last_curl:-0}" ] && [ "${last_curl:-0}" -gt 0 ]; then
    echo "  PASS: the delete (call ${first_delete}) comes after the last scrape (call ${last_curl})"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the delete (call ${first_delete:-none}) does not come after the last scrape (call ${last_curl:-none})"
    FAIL=$((FAIL + 1))
  fi
}

test_result_json_merges_two_counts() {
  echo "Test: the nightly's CR_COUNTS=\"1 3\" writes the quantiles of both counts"

  local out="$TMP_DIR/two.json"
  bench CR_COUNTS="1 3" RESULT_JSON="$out"
  assert_eq "the benchmark exits 0" "0" "$BENCH_RC"
  if [ ! -f "$out" ]; then
    echo "  FAIL: $out was not written"
    FAIL=$((FAIL + 1))
    return
  fi
  assert_eq "the metrics are the six keys of each count" "true" \
    "$(json_field "$out" 'sorted(d["metrics"]) == sorted("reconcile.%d.%s_p%d_s" % (c, h, q) for c in (1, 3) for h in ("e2e", "sub") for q in (50, 95, 99))')"
  assert_eq "the second count has its own p95" "0.0235" "$(json_field "$out" 'd["metrics"]["reconcile.3.e2e_p95_s"]')"
  assert_eq "meta.cr_counts is CR_COUNTS" '"1 3"' "$(json_field "$out" 'd["meta"]["cr_counts"]')"
  local calls
  calls="$(cat "$TMP_DIR/state/calls")"
  assert_contains "the three CRs of the second count are waited for" "$calls" \
    "wait --for=condition=Ready --timeout=600s keystone.keystone.openstack.c5c3.io/keystone-perf-3-1 keystone.keystone.openstack.c5c3.io/keystone-perf-3-2 keystone.keystone.openstack.c5c3.io/keystone-perf-3-3"
  assert_contains "and deleted" "$calls" \
    "delete keystone.keystone.openstack.c5c3.io/keystone-perf-3-1 keystone.keystone.openstack.c5c3.io/keystone-perf-3-2 keystone.keystone.openstack.c5c3.io/keystone-perf-3-3 --wait=true"
}

test_no_name_comes_back_in_a_later_count() {
  echo "Test: no CR name and no database comes back in a later count"

  bench CR_COUNTS="1 3"
  local manifest="$TMP_DIR/state/applied-all.yaml"
  if [ ! -f "$manifest" ]; then
    echo "  FAIL: no manifest was applied"
    FAIL=$((FAIL + 1))
    return
  fi
  assert_eq "four CRs were applied over both counts" "4" \
    "$(grep -c '^  name: keystone-perf-' "$manifest")"
  assert_eq "no CR name is applied twice" "" \
    "$(grep '^  name: keystone-perf-' "$manifest" | sort | uniq -d)"
  assert_eq "no database is applied twice" "" \
    "$(grep '^    database: keystone_perf_' "$manifest" | sort | uniq -d)"
}

test_report_without_result_json_is_unchanged
test_result_json_holds_the_six_quantiles
test_a_red_gate_still_writes_the_file
test_a_comma_decimal_locale_keeps_decimal_points
test_the_cr_sets_replicas_under_spec_deployment
test_the_cr_is_deleted_after_the_scrapes
test_result_json_merges_two_counts
test_no_name_comes_back_in_a_later_count

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
