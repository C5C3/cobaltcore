#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/audit-rbac-usage.py on a fixture repository written into a temp
# directory: one operator, demo, with its controller-gen role.yaml, and an
# audit log of hand-written events of its ServiceAccount
# (system:serviceaccount:demo-system:demo-operator), the script run against
# both via --repo-root and --audit-log.
#   - a rule get,list,watch on secrets with events for get and list reports
#     watch as unused;
#   - a rule whose verb is "*" reports nothing unused;
#   - an event with subresource status matches keystones/status;
#   - the leases rule the generator appends counts as granted;
#   - an event of another ServiceAccount is ignored;
#   - a delete event on a kind no rule covers appears under USED, NOT GRANTED;
#   - a line that is not JSON, or an event without a verb, raises
#     skipped_lines by one;
#   - a patch answered 201 uses create as well, one answered 200 does not;
#   - a watch with sendInitialEvents=true uses list as well, a plain watch
#     does not;
#   - an unexercised get is implied by list on the same kind or its status
#     subresource, and reported as unused where no list is granted;
#   - --fail-on-unused turns an unused verb into exit 1;
#   - an empty log, an unreadable log, and a role.yaml without the
#     demo-operator ClusterRole exit 2, the last naming the role;
#   - --json writes the documented shape;
#   - without --operator the operator comes from the chart layout.
#
# Usage: bash tests/unit/hack/audit_rbac_usage_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
AUDIT="$PROJECT_ROOT/hack/audit-rbac-usage.py"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

DEMO_USER="system:serviceaccount:demo-system:demo-operator"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# The demo-operator ClusterRole controller-gen writes for the manager markers,
# followed by a demo-webhook role the audit must not read.
DEMO_ROLE='---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: demo-operator
rules:
- apiGroups:
  - ""
  resources:
  - secrets
  verbs:
  - get
  - list
  - watch
- apiGroups:
  - ""
  resources:
  - configmaps
  verbs:
  - "*"
- apiGroups:
  - apps
  resources:
  - deployments
  verbs:
  - create
  - get
  - list
  - patch
- apiGroups:
  - demo.c5c3.io
  resources:
  - keystones
  verbs:
  - list
- apiGroups:
  - demo.c5c3.io
  resources:
  - keystones/status
  verbs:
  - get
  - patch
  - update
- apiGroups:
  - scheduling.k8s.io
  resources:
  - priorityclasses
  verbs:
  - get
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: demo-webhook
rules:
- apiGroups:
  - scheduling.k8s.io
  resources:
  - priorityclasses
  verbs:
  - get'

# event <user> <verb> <apiGroup> <resource> [<subresource>] [<code>] — echo one
# audit event line as kube-apiserver writes it at Metadata level.
event() {
  local user="$1" verb="$2" group="$3" resource="$4" sub="${5:-}" code="${6:-200}"
  local subfield=""
  [ -n "$sub" ] && subfield=",\"subresource\":\"$sub\""
  printf '{"kind":"Event","apiVersion":"audit.k8s.io/v1","level":"Metadata","stage":"ResponseComplete","verb":"%s","user":{"username":"%s","groups":["system:serviceaccounts"]},"objectRef":{"resource":"%s","namespace":"openstack","name":"demo","apiGroup":"%s"%s},"responseStatus":{"metadata":{},"code":%s},"annotations":{"authorization.k8s.io/decision":"allow","authorization.k8s.io/reason":"RBAC: allowed by ClusterRoleBinding"}}\n' \
    "$verb" "$user" "$resource" "$group" "$subfield" "$code"
}

# The baseline log: get and list on secrets, a wildcard-granted configmap read,
# a status patch, a leader-election update, another ServiceAccount's delete, a
# delete on a kind no rule covers, a discovery request without objectRef, and
# one line that is not JSON.
baseline_log() {
  event "$DEMO_USER" get "" secrets
  event "$DEMO_USER" list "" secrets
  event "$DEMO_USER" get "" configmaps
  event "$DEMO_USER" patch demo.c5c3.io keystones status
  event "$DEMO_USER" update coordination.k8s.io leases
  event "system:serviceaccount:other-system:other-operator" delete "" secrets
  event "$DEMO_USER" delete demo.c5c3.io widgets
  printf '{"kind":"Event","verb":"get","requestURI":"/apis","user":{"username":"%s"}}\n' "$DEMO_USER"
  echo 'not json'
}

new_root() {
  TMP="$(mktemp -d)"
  OUT="$TMP/out"
  ERR="$TMP/err"
  LOG="$TMP/audit.jsonl"
  mkdir -p "$TMP/operators/demo/config/rbac" "$TMP/operators/demo/helm/demo-operator"
  printf '%s\n' "$1" >"$TMP/operators/demo/config/rbac/role.yaml"
  : >"$LOG"
}

audit() {
  python3 "$AUDIT" --repo-root "$TMP" --audit-log "$LOG" "$@" >"$OUT" 2>"$ERR"
  RC=$?
}

# result <python expression over r, the demo entry of the JSON report>
result() {
  python3 -c '
import json, sys
r = json.load(open(sys.argv[1]))["demo"]
print(eval(sys.argv[2]))
' "$TMP/report.json" "$1"
}

# unused_verbs <resource> — the unused verbs of resource, comma-separated.
unused_verbs() {
  result "','.join(v for row in r['unused'] if row['resource'] == '$1' for v in row['verbs'])"
}

# ---------------------------------------------------------------------------
# Test A: the diff on the baseline log
# ---------------------------------------------------------------------------
test_baseline_diff() {
  echo "Test: the baseline log against the demo role"
  new_root "$DEMO_ROLE"
  baseline_log >"$LOG"
  audit --operator demo --json "$TMP/report.json"
  assert_eq "exit code is 0 with findings" "0" "$RC"

  assert_eq "get,list,watch on secrets with get and list used: watch is unused" \
    "watch" "$(unused_verbs secrets)"
  assert_eq "the wildcard verb rule on configmaps reports nothing unused" \
    "" "$(unused_verbs configmaps)"
  assert_eq "the status patch matches keystones/status; update is unused" \
    "update" "$(unused_verbs keystones/status)"
  assert_eq "the leases update is granted by the appended leases rule" \
    "False" "$(result "any(row['resource'] == 'leases' for row in r['used_not_granted'])")"
  assert_contains "the leases update is used" \
    "$(result "','.join(row['verb'] for row in r['used'] if row['resource'] == 'leases')")" "update"
  assert_eq "another ServiceAccount's delete on secrets is ignored" \
    "False" "$(result "any(row['verb'] == 'delete' and row['resource'] == 'secrets' for row in r['used'])")"
  assert_eq "the delete on widgets is the one used-not-granted row" \
    "[('demo.c5c3.io', 'widgets', 'delete')]" \
    "$(result "[(row['apiGroup'], row['resource'], row['verb']) for row in r['used_not_granted']]")"
  assert_contains "the report lists it under USED, NOT GRANTED" "$(cat "$OUT")" "USED, NOT GRANTED (1)"
  assert_contains "the row names the kind and the verb" "$(cat "$OUT")" "widgets.demo.c5c3.io: delete"
  assert_eq "the line that is not JSON is the one skipped line" "1" "$(result "r['skipped_lines']")"
  assert_eq "events counts the demo ServiceAccount's seven events" "7" "$(result "r['events']")"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Test B: skipped lines
# ---------------------------------------------------------------------------
test_skipped_lines() {
  echo "Test: a line that is not JSON and an event without a verb are skipped"
  new_root "$DEMO_ROLE"
  event "$DEMO_USER" get "" secrets >"$LOG"
  audit --operator demo --json "$TMP/report.json"
  assert_eq "no line is skipped in a clean log" "0" "$(result "r['skipped_lines']")"

  echo '{"truncated' >>"$LOG"
  audit --operator demo --json "$TMP/report.json"
  assert_eq "a line that is not JSON raises skipped_lines by one" "1" "$(result "r['skipped_lines']")"

  printf '{"kind":"Event","user":{"username":"%s"}}\n' "$DEMO_USER" >>"$LOG"
  audit --operator demo --json "$TMP/report.json"
  assert_eq "an event without a verb raises it by one more" "2" "$(result "r['skipped_lines']")"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Test C: a patch that created the object
# ---------------------------------------------------------------------------
test_patch_201_uses_create() {
  echo "Test: a patch answered 201 uses create, one answered 200 does not"
  new_root "$DEMO_ROLE"
  {
    event "$DEMO_USER" list apps deployments
    event "$DEMO_USER" patch apps deployments "" 200
  } >"$LOG"
  audit --operator demo --json "$TMP/report.json"
  assert_eq "after a 200 patch, create is unused" "create" "$(unused_verbs deployments)"

  {
    event "$DEMO_USER" list apps deployments
    event "$DEMO_USER" patch apps deployments "" 201
  } >"$LOG"
  audit --operator demo --json "$TMP/report.json"
  assert_eq "after a 201 patch, create is used" "" "$(unused_verbs deployments)"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Test C2: a watch that streams the initial list
# ---------------------------------------------------------------------------

# watch_event <query> — a watch on secrets whose requestURI carries query.
watch_event() {
  printf '{"kind":"Event","stage":"ResponseStarted","verb":"watch","requestURI":"/api/v1/secrets?%s","user":{"username":"%s"},"objectRef":{"resource":"secrets","apiVersion":"v1"},"responseStatus":{"code":200}}\n' \
    "$1" "$DEMO_USER"
}

test_watch_list_uses_list() {
  echo "Test: a watch with sendInitialEvents=true uses list, a plain watch does not"
  new_root "$DEMO_ROLE"
  {
    event "$DEMO_USER" get "" secrets
    watch_event "allowWatchBookmarks=true&resourceVersion=1234&watch=true"
  } >"$LOG"
  audit --operator demo --json "$TMP/report.json"
  assert_eq "after a plain watch, list is unused" "list" "$(unused_verbs secrets)"

  {
    event "$DEMO_USER" get "" secrets
    watch_event "allowWatchBookmarks=true&resourceVersionMatch=NotOlderThan&sendInitialEvents=true&watch=true"
  } >"$LOG"
  audit --operator demo --json "$TMP/report.json"
  assert_eq "after a watch that streams the initial list, list is used" "" "$(unused_verbs secrets)"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Test D: get follows list
# ---------------------------------------------------------------------------
test_get_follows_list() {
  echo "Test: an unexercised get is implied by list on the same kind"
  new_root "$DEMO_ROLE"
  event "$DEMO_USER" list "" secrets >"$LOG"
  audit --operator demo --json "$TMP/report.json"
  assert_eq "get on secrets beside a granted list is not unused" \
    "watch" "$(unused_verbs secrets)"
  assert_eq "get on keystones/status is implied by list on keystones" \
    "patch,update" "$(unused_verbs keystones/status)"
  assert_eq "the implied gets are listed" \
    "/secrets apps/deployments coordination.k8s.io/leases demo.c5c3.io/keystones/status" \
    "$(result "' '.join(sorted(row['apiGroup'] + '/' + row['resource'] for row in r['get_implied_by_list']))")"
  assert_eq "get on priorityclasses, where no list is granted, is unused" \
    "get" "$(unused_verbs priorityclasses)"
  assert_contains "the report has the GET IMPLIED BY LIST section" "$(cat "$OUT")" "GET IMPLIED BY LIST (4)"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Test E: exit codes
# ---------------------------------------------------------------------------
test_exit_codes() {
  echo "Test: --fail-on-unused, an empty log, an unreadable log and a missing role"
  new_root "$DEMO_ROLE"
  baseline_log >"$LOG"
  audit --operator demo --fail-on-unused
  assert_eq "--fail-on-unused exits 1 when watch on secrets is unused" "1" "$RC"

  : >"$LOG"
  audit --operator demo
  assert_eq "an empty log exits 2" "2" "$RC"
  assert_contains "it names the operator without events" "$(cat "$ERR")" \
    "no audit event carries the username of demo-operator"

  python3 "$AUDIT" --repo-root "$TMP" --audit-log "$TMP/absent.jsonl" --operator demo >"$OUT" 2>"$ERR"
  assert_eq "an unreadable log exits 2" "2" "$?"
  assert_contains "it names the log" "$(cat "$ERR")" "cannot read audit log $TMP/absent.jsonl"
  rm -rf "$TMP"

  new_root '---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: demo-webhook
rules: []'
  baseline_log >"$LOG"
  audit --operator demo
  assert_eq "a role.yaml without demo-operator exits 2" "2" "$RC"
  assert_contains "it names the missing role" "$(cat "$ERR")" \
    "operators/demo/config/rbac/role.yaml has no ClusterRole named demo-operator"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Test F: the JSON shape and the default operator set
# ---------------------------------------------------------------------------
test_json_shape() {
  echo "Test: --json writes the documented shape; the chart layout names the operator"
  new_root "$DEMO_ROLE"
  baseline_log >"$LOG"
  audit --json "$TMP/report.json"
  assert_eq "exit code is 0" "0" "$RC"
  assert_eq "the report has one entry per discovered operator" "['demo']" \
    "$(python3 -c 'import json, sys; print(sorted(json.load(open(sys.argv[1]))))' "$TMP/report.json")"
  assert_eq "the entry carries the documented keys" \
    "['events', 'get_implied_by_list', 'skipped_lines', 'unused', 'used', 'used_not_granted']" \
    "$(result "sorted(r)")"
  assert_eq "used rows are apiGroup, resource, verb" "['apiGroup', 'resource', 'verb']" \
    "$(result "sorted(r['used'][0])")"
  assert_eq "unused rows are apiGroup, resource, verbs" "['apiGroup', 'resource', 'verbs']" \
    "$(result "sorted(r['unused'][0])")"
  assert_eq "used-not-granted rows add the authorization reason" \
    "['apiGroup', 'reason', 'resource', 'verb']" "$(result "sorted(r['used_not_granted'][0])")"
  assert_eq "events and skipped_lines are counts" "True" \
    "$(result "isinstance(r['events'], int) and isinstance(r['skipped_lines'], int)")"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if ! command -v python3 >/dev/null 2>&1 || ! python3 -c 'import yaml' 2>/dev/null; then
  echo "SKIP: python3 with PyYAML not installed (every case needs it)"
  SKIP=$((SKIP + 1))
else
  test_baseline_diff
  test_skipped_lines
  test_patch_201_uses_create
  test_watch_list_uses_list
  test_get_follows_list
  test_exit_codes
  test_json_shape
fi

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
