#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/perf-token-baseline.sh against a stub kubectl, with the real
# tests/e2e-chaos/lib/workload.sh and hack/perf-baseline.py behind it:
#   - a cluster kubectl cannot reach, or one without the operator Deployment,
#     stops in the preflight with exit 1 before any CR is applied;
#   - the happy path applies keystone-perf-token with its own database and
#     spec.deployment.replicas, starts the token stream against the CR's
#     Service, writes the eight token.* metrics with IMAGE_TAG as
#     meta.release, and deletes the pod and the CR;
#   - too few samples exit 1 naming the op and its count, with the JSON
#     written;
#   - a CR that never turns Ready, and a workload pod that fails before
#     READY, exit 1 after the trap deleted the CR;
#   - an empty pod log exits 2;
#   - CHAOS_WORKLOAD_IMAGE reaches the pod.
#
# Usage: bash tests/unit/hack/perf_token_baseline_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SCRIPT="$PROJECT_ROOT/hack/perf-token-baseline.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

STUB_BIN="$TMP_DIR/bin"
mkdir -p "$STUB_BIN"

# The stub kubectl keeps its state in $STUB_DIR:
#   calls         one line per call; an argument over 300 bytes is written as
#                 <N bytes> (the workload source kubectl run carries)
#   applied.yaml  the manifest kubectl apply was given
#   log           what the first `logs` call prints, the one workload.sh start
#                 polls for READY; log.final, when present, is what every
#                 later call prints
# and reads these switches: STUB_NO_CLUSTER, STUB_NO_OPERATOR, STUB_WAIT_FAIL
# and STUB_PHASE (Running).
cat >"$STUB_BIN/kubectl" <<'SH'
#!/bin/bash
{
  for a in "$@"; do
    if [ "${#a}" -gt 300 ]; then printf '<%d bytes> ' "${#a}"; else printf '%s ' "$a"; fi
  done
  echo
} >>"$STUB_DIR/calls"
args=" $* "
case "$args" in
  *" version "*)
    if [ -n "${STUB_NO_CLUSTER:-}" ]; then echo "The connection to the server was refused" >&2; exit 1; fi
    ;;
  *" get deploy "*)
    if [ -n "${STUB_NO_OPERATOR:-}" ]; then echo 'Error from server (NotFound): deployments.apps "keystone-operator" not found' >&2; exit 1; fi
    ;;
  *" apply -f "*)
    prev=""
    for a in "$@"; do
      if [ "$prev" = "-f" ]; then cp "$a" "$STUB_DIR/applied.yaml"; fi
      prev="$a"
    done
    ;;
  *" wait "*)
    if [ -n "${STUB_WAIT_FAIL:-}" ]; then echo "error: timed out waiting for the condition on keystones/keystone-perf-token" >&2; exit 1; fi
    ;;
  *" get secret "*) printf '%s' "$(printf 's3cret' | base64)" ;;
  *" get pod "*) printf '%s' "${STUB_PHASE:-Running}" ;;
  *" logs "*)
    n=$(( $(cat "$STUB_DIR/logs.calls" 2>/dev/null || echo 0) + 1 ))
    echo "$n" >"$STUB_DIR/logs.calls"
    if [ "$n" -gt 1 ] && [ -f "$STUB_DIR/log.final" ]; then
      cat "$STUB_DIR/log.final"
    else
      cat "$STUB_DIR/log"
    fi
    ;;
  *" run "* | *" delete "*) ;;
  *" describe "*) printf 'Name:   chaos-wl-perf-token\nStatus: %s\n' "${STUB_PHASE:-Running}" ;;
  *) echo "stub kubectl: unexpected call: $*" >&2; exit 1 ;;
esac
exit 0
SH
chmod +x "$STUB_BIN/kubectl"

# record <op> <ok> <ms> — print one token-stream record line.
record() {
  printf '{"t": 1760000000.0, "s": "token", "op": "%s", "ok": %s, "code": 201, "ms": %s, "msg": ""}\n' "$1" "$2" "$3"
}

# write_log <file> <cycles> — the records of <cycles> ok issue and validate
# cycles, READY after the first, as workload.py prints them.
write_log() {
  local i
  {
    for ((i = 1; i <= $2; i++)); do
      record issue true $((10 + i % 7))
      record validate true $((5 + i % 3))
      if [ "$i" -eq 1 ]; then echo "READY token"; fi
    done
  } >"$1"
}

# new_stub [cycles] — empty the stub state; the pod log holds <cycles> cycles
# (default 300).
new_stub() {
  rm -rf "$TMP_DIR/state"
  mkdir -p "$TMP_DIR/state"
  : >"$TMP_DIR/state/calls"
  write_log "$TMP_DIR/state/log" "${1:-300}"
}

# measure [VAR=value...] — run the script with the stub first on PATH and a
# zero duration; OUT holds stdout and stderr, RC the exit code. Call new_stub
# first.
measure() {
  RC=0
  OUT="$(env STUB_DIR="$TMP_DIR/state" PATH="$STUB_BIN:$PATH" DURATION_SECONDS=0 \
    RESULT_JSON="$TMP_DIR/out/nested/token.json" CHAOS_WORKLOAD_IMAGE= CHAOS_WORKLOAD_READY_SECONDS=30 \
    "$@" bash "$SCRIPT" 2>&1)" || RC=$?
  CALLS="$(cat "$TMP_DIR/state/calls")"
}

# json_field <file> <python expression over d> — print the value as JSON.
json_field() {
  python3 -c 'import json, sys; d = json.load(open(sys.argv[1])); print(json.dumps(eval(sys.argv[2])))' "$1" "$2"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_preflight_stops_before_any_cr() {
  echo "Test: the preflight exits 1 before any CR is applied"

  new_stub
  measure STUB_NO_OPERATOR=1
  assert_eq "no operator Deployment exits 1" "1" "$RC"
  assert_contains "the preflight names the Deployment" "$OUT" \
    "operator Deployment keystone-system/keystone-operator not found; deploy the operator first"
  assert_not_contains "no CR is applied" "$CALLS" "apply"
  assert_not_contains "nothing is deleted" "$CALLS" "delete"

  new_stub
  measure STUB_NO_CLUSTER=1
  assert_eq "an unreachable cluster exits 1" "1" "$RC"
  assert_contains "the preflight says kubectl cannot reach a cluster" "$OUT" "kubectl cannot reach a cluster"
  assert_not_contains "no CR is applied" "$CALLS" "apply"
}

test_happy_path_writes_the_token_metrics() {
  echo "Test: a full run writes the eight token metrics and cleans up"

  new_stub
  rm -rf "$TMP_DIR/out"
  measure IMAGE_TAG=2026.2
  local out="$TMP_DIR/out/nested/token.json"
  assert_eq "the run exits 0" "0" "$RC"
  if [ -f "$out" ]; then
    assert_eq "RESULT_JSON holds the eight token.* metrics" \
      '["token.issue.failed", "token.issue.p50_ms", "token.issue.p99_ms", "token.issue.samples", "token.validate.failed", "token.validate.p50_ms", "token.validate.p99_ms", "token.validate.samples"]' \
      "$(json_field "$out" 'sorted(d["metrics"])')"
    assert_eq "meta.release is IMAGE_TAG" '"2026.2"' "$(json_field "$out" 'd["meta"]["release"]')"
    assert_eq "each op counts its 300 samples" "[300, 300]" \
      "$(json_field "$out" '[d["metrics"]["token.issue.samples"], d["metrics"]["token.validate.samples"]]')"
  else
    echo "  FAIL: $out was not written"
    FAIL=$((FAIL + 1))
  fi

  local manifest="$TMP_DIR/state/applied.yaml"
  assert_file_contains_fixed "the CR is keystone-perf-token" "$manifest" "name: keystone-perf-token"
  assert_file_contains_fixed "the CR has its own database" "$manifest" "database: keystone_perf_token"
  assert_file_contains_fixed "replicas sit under spec.deployment" "$manifest" "  deployment:
    replicas: 1"
  assert_file_contains_fixed "the image tag is IMAGE_TAG" "$manifest" 'tag: "2026.2"'
  assert_file_contains_fixed "bootstrap reads keystone-admin" "$manifest" "name: keystone-admin"

  assert_contains "the CR is waited for" "$CALLS" "-n openstack wait --for=condition=Ready --timeout=600s keystone/keystone-perf-token"
  assert_contains "the token stream targets the CR's Service" "$CALLS" \
    "--streams token --keystone http://keystone-perf-token.openstack.svc:5000/v3"
  assert_contains "the pod log is read" "$CALLS" "-n openstack logs chaos-wl-perf-token"
  assert_contains "the workload pod is deleted" "$CALLS" "delete pod chaos-wl-perf-token -n openstack --ignore-not-found --wait=false"
  assert_contains "the CR is deleted and waited for" "$CALLS" \
    "-n openstack delete keystone keystone-perf-token --ignore-not-found --wait=true --timeout=600s"
}

test_too_few_samples_exit_1() {
  echo "Test: too few samples exit 1 naming the op and its count, with the JSON written"

  new_stub 20
  rm -rf "$TMP_DIR/out"
  measure MIN_SAMPLES=300
  assert_eq "the run exits 1" "1" "$RC"
  assert_contains "issue is named with its count" "$OUT" "FAIL: token.issue: 20 ok record(s), want at least 300"
  if [ -f "$TMP_DIR/out/nested/token.json" ]; then
    echo "  PASS: the JSON is written"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the JSON was not written"
    FAIL=$((FAIL + 1))
  fi
  assert_contains "the CR is deleted" "$CALLS" "delete keystone keystone-perf-token"
}

test_cr_never_ready_exits_1() {
  echo "Test: a CR that never turns Ready exits 1 after the trap deleted it"

  new_stub
  measure STUB_WAIT_FAIL=1
  assert_eq "the run exits 1" "1" "$RC"
  assert_contains "the kubectl wait error is shown" "$OUT" "timed out waiting for the condition"
  assert_contains "the script names the CR" "$OUT" "Keystone openstack/keystone-perf-token did not become Ready within 600s"
  assert_contains "the trap deletes the CR" "$CALLS" "delete keystone keystone-perf-token"
  assert_eq "no workload pod is started" "0" "$(grep -c '^run ' "$TMP_DIR/state/calls")"
}

test_failed_workload_exits_1() {
  echo "Test: a workload pod that fails before READY exits 1 with the harness message"

  new_stub
  : >"$TMP_DIR/state/log"
  measure STUB_PHASE=Failed
  assert_eq "the run exits 1" "1" "$RC"
  assert_contains "the harness message is shown" "$OUT" "FAIL: start perf-token: the pod reached Failed before READY on: token"
  assert_contains "the script says the workload did not start" "$OUT" "the token workload did not start"
  assert_contains "the trap deletes the CR" "$CALLS" "delete keystone keystone-perf-token"
}

test_empty_log_exits_2() {
  echo "Test: an empty pod log exits 2"

  new_stub
  : >"$TMP_DIR/state/log.final"
  measure
  assert_eq "the run exits 2" "2" "$RC"
  assert_contains "the tool says why" "$OUT" "the log holds no token record"
  assert_contains "the trap deletes the CR" "$CALLS" "delete keystone keystone-perf-token"
}

test_workload_image_reaches_the_pod() {
  echo "Test: CHAOS_WORKLOAD_IMAGE reaches the workload pod"

  new_stub
  measure CHAOS_WORKLOAD_IMAGE=ghcr.io/c5c3/tempest:2026.2
  assert_eq "the run exits 0" "0" "$RC"
  assert_contains "the pod runs the given image" "$CALLS" "run chaos-wl-perf-token -n openstack --image=ghcr.io/c5c3/tempest:2026.2"
}

test_preflight_stops_before_any_cr
test_happy_path_writes_the_token_metrics
test_too_few_samples_exit_1
test_cr_never_ready_exits_1
test_failed_workload_exits_1
test_empty_log_exits_2
test_workload_image_reaches_the_pod

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
