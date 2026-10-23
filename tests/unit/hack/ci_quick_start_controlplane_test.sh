#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-quick-start-controlplane.sh with a stubbed kubectl and a stub
# onboarding script (ONBOARD_SCRIPT) and the real yq and jq:
#   - up against the real docs/quick-start-controlplane.md applies the
#     OVNCentral, waits for it, applies the ControlPlane, waits for the
#     MariaDB, reads the Secret, onboards `openstack controlplane` and
#     waits for the ControlPlane, in that order, and leaves stage at done;
#   - the copies it writes are the OVNCentral controlplane-ovn and the
#     ControlPlane controlplane in openstack, without publicEndpoint on port
#     443 and with the page's six on 8443;
#   - an empty doc, a doc without a `# controlplane.yaml` block, a ControlPlane
#     of another name, kind or namespace, an OVNCentral of another kind or
#     namespace or without a name, a missing yq and a non-numeric timeout exit
#     1 before any kubectl call, and of two `# controlplane.yaml` blocks the
#     first is applied;
#   - a failing OVNCentral wait, ControlPlane apply, MariaDB Ready wait,
#     onboarding or ControlPlane wait exits 1 with its ERROR: line, leaves
#     stage at the failed stage and runs nothing after it;
#   - a MariaDB that never appears exits 1 with its ERROR: line, the wait for
#     its creation gets the whole MARIADB_TIMEOUT, and the Ready wait gets
#     what is left of it, at least 1s;
#   - a failed Secret read and a Secret without root_token exit 1 before the
#     onboarding;
#   - the token reaches the onboarding script in BAO_TOKEN only, appears in no
#     argument and no output, and is unset before the ControlPlane wait;
#   - summary writes the stage (deploy-infra without a stage file, the
#     default STATE_DIR _output/flux-path the workflow relies on), the
#     conditions as table rows with a backtick in a message turned into a
#     quote, a placeholder for a ControlPlane without conditions and for no
#     OVNCentral, "(unavailable:" for each failed read and for output jq
#     cannot read, creates the file's directory, reads no Secret and exits 0;
#   - a usage error exits 2, and the script never sets -x.
# The up cases are counted as SKIP when yq is not on PATH.
#
# Usage: bash tests/unit/hack/ci_quick_start_controlplane_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
QS_SH="$PROJECT_ROOT/hack/ci-quick-start-controlplane.sh"
QUICK_START_DOC_REAL="$PROJECT_ROOT/docs/quick-start-controlplane.md"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# The script's knobs come from the environment; a developer's own must not
# leak into the cases.
unset QUICK_START_DOC ONBOARD_SCRIPT STATE_DIR CONTROLPLANE_NAME KIND_HOST_PORT \
  OVN_TIMEOUT MARIADB_TIMEOUT READY_TIMEOUT BAO_TOKEN

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

STUB_BIN="$TMP_DIR/bin"
ONBOARD_STUB="$TMP_DIR/onboard.sh"
CALL_LOG="$TMP_DIR/calls.log"
ONBOARD_ENV_LOG="$TMP_DIR/onboard.env"
WAIT_ENV_LOG="$TMP_DIR/wait.env"
STUB_TOKEN="s.stub-root-token-7f3a9c"
CASE_NO=0

# The ControlPlane and OVNCentral the summary reads.
CP_JSON='{"status":{"conditions":[
  {"type":"KeystoneReady","status":"True","reason":"Ready","message":"Keystone is Ready"},
  {"type":"GlanceReady","status":"False","reason":"Failed","message":"run `kubectl describe` now"},
  {"type":"Ready","status":"False","reason":"NovaNotReady","message":"waiting for a|b\nnext"}]}}'
OVN_JSON='{"items":[{"metadata":{"name":"controlplane-ovn"},"status":{"conditions":[
  {"type":"Ready","status":"True","reason":"Ready","message":"all databases up"}]}}]}'

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

have() { command -v "$1" >/dev/null 2>&1; }

# make_stubs
# kubectl logs `kubectl <argv>` per call to $CALL_LOG. It answers:
#   apply -f <f>          ok; fails when $STUB_FAIL_APPLY_CP and <f> holds
#                         the ControlPlane
#   wait ovncentral/*     ok; fails when $STUB_FAIL_WAIT_OVN
#   wait --for=create     ok after $STUB_CREATE_SLEEP seconds (default 0);
#                         times out when $STUB_MARIADB_NEVER
#   wait mariadb/*        ok; times out when $STUB_FAIL_WAIT_MARIADB
#   get secret            the base64 of $STUB_INIT_OUTPUT; fails when
#                         $STUB_SECRET_FAIL
#   wait controlplane/*   logs BAO_TOKEN=<value> to $WAIT_ENV_LOG; fails when
#                         $STUB_FAIL_WAIT_CP
#   get controlplane/*, get ovncentrals, get helmrelease, get gitrepository,…
#                         $CP_JSON, $OVN_JSON and a fixed table
# and fails every call when $STUB_ALL_FAIL.
# The onboarding stub logs `onboard <argv>` to $CALL_LOG and its BAO_TOKEN to
# $ONBOARD_ENV_LOG, then exits $STUB_ONBOARD_RC (default 0).
make_stubs() {
  mkdir -p "$STUB_BIN"
  cat >"$STUB_BIN/kubectl" <<'STUB'
#!/bin/bash
echo "kubectl $*" >>"$CALL_LOG"
if [ -n "${STUB_ALL_FAIL:-}" ]; then
  echo "error: stub: connection refused" >&2
  exit 1
fi
case "$1 $2" in
  "apply -f")
    if [ -n "${STUB_FAIL_APPLY_CP:-}" ] && grep -q '^kind: ControlPlane$' "$3"; then
      echo "error: stub: admission webhook denied the request" >&2
      exit 1
    fi
    echo "applied $3"
    ;;
  "wait ovncentral/"*)
    if [ -n "${STUB_FAIL_WAIT_OVN:-}" ]; then
      echo "error: timed out waiting for the condition on ovncentrals/${2#*/}" >&2
      exit 1
    fi
    ;;
  "wait --for=create")
    sleep "${STUB_CREATE_SLEEP:-0}"
    if [ -n "${STUB_MARIADB_NEVER:-}" ]; then
      echo "error: timed out waiting for the condition" >&2
      exit 1
    fi
    ;;
  "wait mariadb/"*)
    if [ -n "${STUB_FAIL_WAIT_MARIADB:-}" ]; then
      echo "error: timed out waiting for the condition on mariadbs/openstack-db" >&2
      exit 1
    fi
    ;;
  "get secret")
    if [ -n "${STUB_SECRET_FAIL:-}" ]; then
      echo 'Error from server (Forbidden): secrets "openbao-init-keys" is forbidden' >&2
      exit 1
    fi
    printf '%s' "$STUB_INIT_OUTPUT" | base64 | tr -d '\n'
    ;;
  "wait controlplane/"*)
    echo "BAO_TOKEN=${BAO_TOKEN:-}" >>"$WAIT_ENV_LOG"
    if [ -n "${STUB_FAIL_WAIT_CP:-}" ]; then
      echo "error: timed out waiting for the condition on controlplanes/${2#*/}" >&2
      exit 1
    fi
    ;;
  "get controlplane/"*) printf '%s\n' "$CP_JSON" ;;
  "get ovncentrals") printf '%s\n' "$OVN_JSON" ;;
  "get helmrelease") printf 'NAMESPACE     NAME           READY\nc5c3-system   c5c3-operator  True\n' ;;
  "get gitrepository,helmrepository,kustomization") printf 'NAME                    READY\ngitrepository/k-orc    True\n' ;;
  *)
    echo "error: stub: unexpected kubectl $*" >&2
    exit 1
    ;;
esac
STUB
  chmod +x "$STUB_BIN/kubectl"

  cat >"$ONBOARD_STUB" <<'STUB'
#!/bin/bash
echo "onboard $*" >>"$CALL_LOG"
printf '%s' "${BAO_TOKEN:-}" >"$ONBOARD_ENV_LOG"
exit "${STUB_ONBOARD_RC:-0}"
STUB
  chmod +x "$ONBOARD_STUB"
}

# run_qs [NAME=VALUE]... <args>...
# Run the script with the stubs on PATH, a fresh STATE_DIR and the leading
# NAME=VALUE pairs in its environment. Sets OUT (stdout and stderr), RC and
# STATE.
run_qs() {
  CASE_NO=$((CASE_NO + 1))
  STATE="$TMP_DIR/state-$CASE_NO"
  # A later NAME=VALUE overrides an earlier one in env's argument list.
  local assigns=(PATH="$STUB_BIN:$PATH" STATE_DIR="$STATE" ONBOARD_SCRIPT="$ONBOARD_STUB"
    CALL_LOG="$CALL_LOG" ONBOARD_ENV_LOG="$ONBOARD_ENV_LOG" WAIT_ENV_LOG="$WAIT_ENV_LOG"
    STUB_INIT_OUTPUT="{\"root_token\":\"$STUB_TOKEN\",\"unseal_keys_b64\":[\"k1\"]}"
    CP_JSON="$CP_JSON" OVN_JSON="$OVN_JSON")
  while [ "$#" -gt 0 ]; do
    case "$1" in
      [A-Z_]*=*) assigns+=("$1") ;;
      *) break ;;
    esac
    shift
  done
  : >"$CALL_LOG"
  rm -f "$ONBOARD_ENV_LOG" "$WAIT_ENV_LOG"
  OUT="$(env "${assigns[@]}" bash "$QS_SH" "$@" 2>&1)"
  RC=$?
}

# call_heads — The first three words of every logged call, one per line.
call_heads() {
  awk '{ print $1, $2, $3 }' "$CALL_LOG"
}

# stage — The content of the stage file of the last run, or <absent>.
stage() {
  cat "$STATE/stage" 2>/dev/null || echo "<absent>"
}

# skip_without_yq <checks> — Count the case as SKIP and return 1 without yq.
skip_without_yq() {
  if have yq; then
    return 0
  fi
  echo "  SKIP: yq not installed ($1 checks skipped)"
  SKIP=$((SKIP + $1))
  return 1
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_up_against_the_page() {
  echo "Test: up against the real page runs Steps 3 to 6 in order"
  skip_without_yq 5 || return

  run_qs up
  assert_eq "up exits 0" "0" "$RC"
  assert_eq "the calls follow the page's steps" \
    "kubectl apply -f
kubectl wait ovncentral/controlplane-ovn
kubectl apply -f
kubectl wait --for=create
kubectl wait mariadb/openstack-db
kubectl get secret
onboard openstack controlplane
kubectl wait controlplane/controlplane" "$(call_heads)"
  assert_eq "the applies name the two copies" \
    "kubectl apply -f $STATE/controlplane-ovn.yaml
kubectl apply -f $STATE/controlplane.yaml" "$(grep '^kubectl apply ' "$CALL_LOG")"
  assert_eq "stage reads done" "done" "$(stage)"
  assert_contains "it ends on the Ready line" "$OUT" "OK: controlplane/controlplane in openstack is Ready"
}

test_the_copies() {
  echo "Test: the copies are the page's OVNCentral and ControlPlane"
  skip_without_yq 5 || return

  run_qs up
  assert_eq "the OVN copy is the OVNCentral controlplane-ovn in openstack" \
    "OVNCentral controlplane-ovn openstack" \
    "$(yq '[.kind, .metadata.name, .metadata.namespace] | join(" ")' "$STATE/controlplane-ovn.yaml")"
  assert_eq "the CR copy is the ControlPlane controlplane in openstack" \
    "ControlPlane controlplane openstack" \
    "$(yq '[.kind, .metadata.name, .metadata.namespace] | join(" ")' "$STATE/controlplane.yaml")"
  assert_eq "on port 443 no service keeps a publicEndpoint" "0" \
    "$(yq '[.spec.services[] | select(has("publicEndpoint"))] | length' "$STATE/controlplane.yaml")"

  run_qs KIND_HOST_PORT=8443 up
  assert_eq "on 8443 up exits 0" "0" "$RC"
  assert_eq "on 8443 the copy keeps the page's six publicEndpoint values" \
    "keystone=https://keystone.127-0-0-1.nip.io:8443/v3
glance=https://glance.127-0-0-1.nip.io:8443
placement=https://placement.127-0-0-1.nip.io:8443
barbican=https://barbican.127-0-0-1.nip.io:8443
neutron=https://neutron.127-0-0-1.nip.io:8443
nova=https://nova.127-0-0-1.nip.io:8443" \
    "$(yq '.spec.services | to_entries | .[] | select(.value | has("publicEndpoint")) | .key + "=" + .value.publicEndpoint' \
      "$STATE/controlplane.yaml")"
}

test_a_doc_without_the_blocks() {
  echo "Test: a doc without one of the two blocks fails before any kubectl call"
  skip_without_yq 6 || return

  : >"$TMP_DIR/empty.md"
  run_qs QUICK_START_DOC="$TMP_DIR/empty.md" up
  assert_eq "an empty doc exits 1" "1" "$RC"
  assert_contains "naming the OVN marker" "$OUT" \
    "$TMP_DIR/empty.md has no yaml block that starts with '# controlplane-ovn.yaml'"
  assert_eq "and calls no kubectl" "" "$(cat "$CALL_LOG")"

  printf '```yaml\n# controlplane-ovn.yaml\nkind: OVNCentral\n```\n' >"$TMP_DIR/ovn-only.md"
  run_qs QUICK_START_DOC="$TMP_DIR/ovn-only.md" up
  assert_eq "a doc without the ControlPlane block exits 1" "1" "$RC"
  assert_contains "naming the ControlPlane marker" "$OUT" \
    "has no yaml block that starts with '# controlplane.yaml'"
  assert_eq "and calls no kubectl" "" "$(cat "$CALL_LOG")"
}

test_a_controlplane_of_another_name() {
  echo "Test: a ControlPlane of another name fails before the apply"
  skip_without_yq 3 || return

  sed '/^# controlplane.yaml$/,/^```$/ s/^  name: controlplane$/  name: other/' \
    "$QUICK_START_DOC_REAL" >"$TMP_DIR/other.md"
  run_qs QUICK_START_DOC="$TMP_DIR/other.md" up
  assert_eq "up exits 1" "1" "$RC"
  assert_contains "naming the field and both values" "$OUT" \
    "metadata.name is 'other', expected 'controlplane'"
  assert_eq "nothing is applied" "" "$(grep '^kubectl apply' "$CALL_LOG")"
}

# expect_extract_failure <label> <sed program> <message> — Run up against the
# page edited by the sed program; it must exit 1 with <message> before any
# kubectl call.
expect_extract_failure() {
  sed "$2" "$QUICK_START_DOC_REAL" >"$TMP_DIR/edit.md"
  run_qs QUICK_START_DOC="$TMP_DIR/edit.md" up
  assert_eq "$1 exits 1" "1" "$RC"
  assert_contains "naming the field and both values" "$OUT" "$3"
  assert_eq "and calls no kubectl" "" "$(cat "$CALL_LOG")"
}

test_an_edited_block() {
  echo "Test: a block of another kind or namespace, or without a name, fails before any kubectl call"
  skip_without_yq 15 || return

  expect_extract_failure "an OVNCentral of another kind" \
    '/^# controlplane-ovn.yaml$/,/^```$/ s/^kind: OVNCentral$/kind: Other/' \
    "controlplane-ovn.yaml: kind is 'Other', expected 'OVNCentral'"
  expect_extract_failure "an OVNCentral in another namespace" \
    '/^# controlplane-ovn.yaml$/,/^```$/ s/^  namespace: openstack$/  namespace: other/' \
    "controlplane-ovn.yaml: metadata.namespace is 'other', expected 'openstack'"
  expect_extract_failure "an OVNCentral without a name" \
    '/^# controlplane-ovn.yaml$/,/^```$/ { /^  name: controlplane-ovn$/d; }' \
    "controlplane-ovn.yaml: metadata.name is 'null', expected a name"
  expect_extract_failure "a ControlPlane of another kind" \
    '/^# controlplane.yaml$/,/^```$/ s/^kind: ControlPlane$/kind: Other/' \
    "controlplane.yaml: kind is 'Other', expected 'ControlPlane'"
  expect_extract_failure "a ControlPlane in another namespace" \
    '/^# controlplane.yaml$/,/^```$/ s/^  namespace: openstack$/  namespace: other/' \
    "controlplane.yaml: metadata.namespace is 'other', expected 'openstack'"
}

test_the_first_controlplane_block() {
  echo "Test: of two # controlplane.yaml blocks the first is applied"
  skip_without_yq 3 || return

  awk '/^# controlplane.yaml$/ { n++ } n == 2 && /^  name: controlplane$/ { $0 = "  name: second" } { print }' \
    "$QUICK_START_DOC_REAL" >"$TMP_DIR/second.md"
  assert_eq "the page's second block is renamed" "1" "$(grep -c '^  name: second$' "$TMP_DIR/second.md")"
  run_qs QUICK_START_DOC="$TMP_DIR/second.md" up
  assert_eq "up exits 0" "0" "$RC"
  assert_eq "the copy is the first block's ControlPlane" "controlplane" \
    "$(yq '.metadata.name' "$STATE/controlplane.yaml")"
}

test_a_missing_tool() {
  echo "Test: without yq on PATH up fails before any kubectl call"

  # A PATH of the kubectl stub and dirname alone: /usr/bin may hold yq.
  local bin="$TMP_DIR/bin-noyq"
  mkdir -p "$bin"
  cp "$STUB_BIN/kubectl" "$bin/kubectl"
  ln -sf "$(command -v dirname)" "$bin/dirname"
  : >"$CALL_LOG"
  OUT="$(env PATH="$bin" CALL_LOG="$CALL_LOG" STATE_DIR="$TMP_DIR/state-noyq" \
    "$BASH" "$QS_SH" up 2>&1)"
  RC=$?
  assert_eq "up exits 1" "1" "$RC"
  assert_contains "naming yq" "$OUT" "ERROR: yq is not on PATH"
  assert_eq "and calls no kubectl" "" "$(cat "$CALL_LOG")"
}

test_a_non_numeric_timeout() {
  echo "Test: a timeout that is not a whole number fails before any kubectl call"
  skip_without_yq 3 || return

  run_qs OVN_TIMEOUT=abc up
  assert_eq "up exits 1" "1" "$RC"
  assert_contains "naming the knob" "$OUT" "ERROR: OVN_TIMEOUT is 'abc', expected a whole number"
  assert_eq "and calls no kubectl" "" "$(cat "$CALL_LOG")"
}

test_a_failing_ovn_wait() {
  echo "Test: a failing OVNCentral wait stops at ovn"
  skip_without_yq 4 || return

  run_qs STUB_FAIL_WAIT_OVN=1 up
  assert_eq "up exits 1" "1" "$RC"
  assert_contains "with the stage and the command" "$OUT" \
    "ERROR: ovn: kubectl wait ovncentral/controlplane-ovn"
  assert_eq "stage reads ovn" "ovn" "$(stage)"
  assert_eq "the ControlPlane is never applied" "" "$(grep 'controlplane.yaml$' "$CALL_LOG")"
}

test_a_failing_controlplane_apply() {
  echo "Test: a failing ControlPlane apply stops at apply"
  skip_without_yq 4 || return

  run_qs STUB_FAIL_APPLY_CP=1 up
  assert_eq "up exits 1" "1" "$RC"
  assert_contains "with the stage" "$OUT" "ERROR: apply: kubectl apply -f $STATE/controlplane.yaml failed (exit 1)"
  assert_contains "below kubectl's own message" "$OUT" "admission webhook denied the request"
  assert_eq "the MariaDB is never waited for" "" "$(grep 'mariadb' "$CALL_LOG")"
}

test_the_mariadb_wait() {
  echo "Test: the MariaDB wait times out, or waits for Ready once the MariaDB exists"
  skip_without_yq 7 || return

  run_qs STUB_MARIADB_NEVER=1 MARIADB_TIMEOUT=2 up
  assert_eq "a MariaDB that never appears exits 1" "1" "$RC"
  assert_contains "naming the stage and the budget" "$OUT" \
    "ERROR: mariadb: kubectl wait --for=create mariadb/openstack-db -n openstack --timeout=2s failed (exit 1)"
  assert_contains "below kubectl's own message" "$OUT" "error: timed out waiting for the condition"
  assert_eq "the onboarding never runs" "" "$(grep '^onboard' "$CALL_LOG")"

  run_qs MARIADB_TIMEOUT=300 up
  assert_eq "a MariaDB that appears lets up finish" "0" "$RC"
  assert_eq "the wait for its creation gets the whole budget" \
    "kubectl wait --for=create mariadb/openstack-db -n openstack --timeout=300s" \
    "$(grep '^kubectl wait --for=create' "$CALL_LOG")"
  assert_eq "then waited for, and the rest ran" \
    "kubectl wait mariadb/openstack-db
kubectl get secret
onboard openstack controlplane
kubectl wait controlplane/controlplane" "$(call_heads | sed -n '5,$p')"
}

test_the_mariadb_ready_wait() {
  echo "Test: the MariaDB Ready wait gets what is left of the budget, at least 1s"
  skip_without_yq 8 || return

  local left
  run_qs STUB_CREATE_SLEEP=1 MARIADB_TIMEOUT=300 up
  assert_eq "up exits 0" "0" "$RC"
  left="$(sed -n 's/^kubectl wait mariadb\/openstack-db -n openstack --for=condition=Ready --timeout=\([0-9]*\)s$/\1/p' "$CALL_LOG")"
  assert_gte "the Ready wait gets what the creation left" "${left:-0}" 250
  assert_gte "and no more" 299 "${left:-1000}"

  run_qs MARIADB_TIMEOUT=0 up
  assert_contains "a spent budget leaves the Ready wait 1s" "$(cat "$CALL_LOG")" \
    "kubectl wait mariadb/openstack-db -n openstack --for=condition=Ready --timeout=1s"

  run_qs STUB_FAIL_WAIT_MARIADB=1 up
  assert_eq "a MariaDB that never becomes Ready exits 1" "1" "$RC"
  assert_contains "with the stage and the command" "$OUT" \
    "ERROR: mariadb: kubectl wait mariadb/openstack-db -n openstack --for=condition=Ready"
  assert_eq "stage reads mariadb" "mariadb" "$(stage)"
  assert_eq "the onboarding never runs" "" "$(grep '^onboard' "$CALL_LOG")"
}

test_the_secret_read() {
  echo "Test: a failed Secret read and a Secret without root_token stop before the onboarding"
  skip_without_yq 6 || return

  run_qs STUB_SECRET_FAIL=1 up
  assert_eq "a failed read exits 1" "1" "$RC"
  assert_contains "naming the Secret" "$OUT" "ERROR: cannot read Secret shared-services/openbao-init-keys"
  assert_eq "the onboarding never runs" "" "$(grep '^onboard' "$CALL_LOG")"

  run_qs STUB_INIT_OUTPUT='{"unseal_keys_b64":["k1"]}' up
  assert_eq "a Secret without root_token exits 1" "1" "$RC"
  assert_contains "saying so" "$OUT" \
    "ERROR: Secret shared-services/openbao-init-keys carries no root_token"
  assert_eq "the onboarding never runs" "" "$(grep '^onboard' "$CALL_LOG")"
}

test_the_token_stays_in_the_environment() {
  echo "Test: the token reaches the onboarding in BAO_TOKEN and nowhere else"
  skip_without_yq 5 || return

  run_qs up
  assert_eq "the onboarding sees the token in BAO_TOKEN" "$STUB_TOKEN" "$(cat "$ONBOARD_ENV_LOG" 2>/dev/null)"
  assert_eq "its arguments are the namespace and the name alone" \
    "onboard openstack controlplane" "$(grep '^onboard' "$CALL_LOG")"
  assert_not_contains "no logged call carries the token" "$(cat "$CALL_LOG")" "$STUB_TOKEN"
  assert_not_contains "the output of up does not carry the token" "$OUT" "$STUB_TOKEN"
  assert_eq "BAO_TOKEN is unset before the ControlPlane wait" "BAO_TOKEN=" "$(cat "$WAIT_ENV_LOG")"
}

test_a_failing_onboarding() {
  echo "Test: a failing onboarding stops at onboard"
  skip_without_yq 4 || return

  run_qs STUB_ONBOARD_RC=3 up
  assert_eq "up exits 1" "1" "$RC"
  assert_contains "naming the script and its exit" "$OUT" \
    "ERROR: onboard: $ONBOARD_STUB openstack controlplane failed (exit 3)"
  assert_eq "stage reads onboard" "onboard" "$(stage)"
  assert_eq "the ControlPlane wait never runs" "" "$(grep 'wait controlplane' "$CALL_LOG")"
}

test_a_failing_controlplane_wait() {
  echo "Test: a failing ControlPlane wait stops at ready"
  skip_without_yq 3 || return

  run_qs STUB_FAIL_WAIT_CP=1 up
  assert_eq "up exits 1" "1" "$RC"
  assert_contains "with the stage and the command" "$OUT" \
    "ERROR: ready: kubectl wait controlplane/controlplane"
  assert_eq "stage reads ready" "ready" "$(stage)"
}

test_summary() {
  echo "Test: summary writes the stage, the conditions and placeholders, and exits 0"

  run_qs summary "$TMP_DIR/s1/summary.md"
  local s1="$TMP_DIR/s1/summary.md"
  assert_eq "summary exits 0" "0" "$RC"
  assert_file_contains_fixed "without a stage file the stage is deploy-infra" "$s1" 'Stage: `deploy-infra`'
  assert_eq "it reads no Secret" "" "$(grep 'secret' "$CALL_LOG")"

  mkdir -p "$TMP_DIR/state-ready"
  echo ready >"$TMP_DIR/state-ready/stage"
  local s2="$TMP_DIR/s2/new/dir/summary.md"
  run_qs STATE_DIR="$TMP_DIR/state-ready" summary "$s2"
  assert_eq "summary into a missing directory exits 0" "0" "$RC"
  assert_file_contains_fixed "the stage is ready" "$s2" 'Stage: `ready`'
  assert_file_contains_fixed "the first condition is a row" "$s2" \
    '| KeystoneReady | True | Ready | `Keystone is Ready` |'
  assert_file_contains_fixed "a backtick in a message cannot close its code span" "$s2" \
    "| GlanceReady | False | Failed | \`run 'kubectl describe' now\` |"
  assert_file_contains_fixed "the last keeps its row, with | escaped and the newline gone" "$s2" \
    '| Ready | False | NovaNotReady | `waiting for a\|b next` |'
  assert_file_contains_fixed "the OVNCentral's Ready is a row" "$s2" \
    '| controlplane-ovn | True | Ready | `all databases up` |'
  assert_file_contains_fixed "the HelmReleases are listed" "$s2" 'c5c3-system   c5c3-operator  True'
  assert_file_contains_fixed "the Flux sources are listed" "$s2" 'gitrepository/k-orc    True'

  run_qs STUB_ALL_FAIL=1 summary "$TMP_DIR/s3/summary.md"
  assert_eq "with every read failing summary still exits 0" "0" "$RC"
  assert_eq "and writes a placeholder for each of the four reads" "4" \
    "$(grep -c '^(unavailable: error: stub: connection refused)$' "$TMP_DIR/s3/summary.md")"
}

test_summary_placeholders() {
  echo "Test: summary says so for no conditions, no OVNCentral and output jq cannot read"

  local s4="$TMP_DIR/s4/summary.md" s5="$TMP_DIR/s5/summary.md"
  run_qs CP_JSON='{"metadata":{"name":"controlplane"}}' OVN_JSON='{"items":[]}' summary "$s4"
  assert_eq "summary exits 0" "0" "$RC"
  assert_file_contains_fixed "a ControlPlane without status says so" "$s4" '(no conditions)'
  assert_file_contains_fixed "no OVNCentral says so" "$s4" '(no OVNCentral in openstack)'

  run_qs OVN_JSON='{"items":[{"metadata":{"name":"controlplane-ovn"}}]}' summary "$s4"
  assert_file_contains_fixed "an OVNCentral without Ready gets empty cells" "$s4" \
    '| controlplane-ovn |  |  |  |'

  run_qs CP_JSON='not json' summary "$s5"
  assert_eq "output jq cannot read still exits 0" "0" "$RC"
  assert_eq "and gives the one placeholder, for the conditions" "1" "$(grep -c '^(unavailable: ' "$s5")"
  assert_file_contains_fixed "while the OVNCentral keeps its row" "$s5" \
    '| controlplane-ovn | True | Ready | `all databases up` |'
}

test_the_default_state_dir() {
  echo "Test: summary reads the stage from _output/flux-path, where the workflow's up writes it"

  local wd="$TMP_DIR/wd"
  mkdir -p "$wd/_output/flux-path"
  echo ready >"$wd/_output/flux-path/stage"
  (cd "$wd" && env PATH="$STUB_BIN:$PATH" CALL_LOG="$CALL_LOG" CP_JSON="$CP_JSON" OVN_JSON="$OVN_JSON" \
    bash "$QS_SH" summary _output/flux-path/summary.md >/dev/null 2>&1)
  assert_eq "summary exits 0" "0" "$?"
  assert_file_contains_fixed "the stage comes from the default directory" \
    "$wd/_output/flux-path/summary.md" 'Stage: `ready`'
}

test_usage_and_hygiene() {
  echo "Test: usage errors exit 2, and the script is executable and never traces"

  run_qs
  assert_eq "no argument exits 2" "2" "$RC"
  assert_contains "with the usage line" "$OUT" "usage: hack/ci-quick-start-controlplane.sh up | summary <file>"
  run_qs bogus
  assert_eq "an unknown subcommand exits 2" "2" "$RC"
  run_qs summary
  assert_eq "summary without a file exits 2" "2" "$RC"

  if [ -x "$QS_SH" ]; then
    assert_eq "the script is executable" "yes" "yes"
  else
    assert_eq "the script is executable" "yes" "no"
  fi
  assert_file_not_contains "it never sets -x" "$QS_SH" '^[[:space:]]*set -[a-z]*x'
  assert_file_not_contains "it never sets -o xtrace" "$QS_SH" 'xtrace'
}

make_stubs
test_up_against_the_page
test_the_copies
test_a_doc_without_the_blocks
test_a_controlplane_of_another_name
test_an_edited_block
test_the_first_controlplane_block
test_a_missing_tool
test_a_non_numeric_timeout
test_a_failing_ovn_wait
test_a_failing_controlplane_apply
test_the_mariadb_wait
test_the_mariadb_ready_wait
test_the_secret_read
test_the_token_stays_in_the_environment
test_a_failing_onboarding
test_a_failing_controlplane_wait
test_summary
test_summary_placeholders
test_the_default_state_dir
test_usage_and_hygiene

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
