#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/gen-helm-rbac-rules.py on a fixture repository written into a
# temp directory: one operator, demo, with its controller-gen role.yaml and an
# empty chart templates/ directory, the script run against it via --repo-root.
#   - a run writes both templates: _rbac-rules.tpl ends with the leases rule,
#     _webhook-rbac-rules.tpl defines demo-operator.webhookRbacRules from the
#     demo-webhook ClusterRole alone, without the Secret grant of the manager
#     and without a leases rule;
#   - --check passes afterwards and reports DRIFT for a hand-edited webhook
#     template;
#   - a role.yaml without the demo-operator or the demo-webhook ClusterRole
#     exits 1 naming the missing role;
#   - a demo-webhook grant demo-operator does not carry exits 1 and writes
#     nothing;
#   - that failure also leaves a valid operator, alpha, processed before demo,
#     unwritten;
#   - an empty demo-webhook rules list renders an empty YAML list.
#
# Usage: bash tests/unit/hack/gen_helm_rbac_rules_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
GEN="$PROJECT_ROOT/hack/gen-helm-rbac-rules.py"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TEMPLATES="operators/demo/helm/demo-operator/templates"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# The demo-operator ClusterRole controller-gen writes for the manager markers.
MANAGER_ROLE='---
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
- apiGroups:
  - scheduling.k8s.io
  resources:
  - priorityclasses
  verbs:
  - get
  - list
  - watch'

# webhook_role <rules> — the demo-webhook ClusterRole with the given rules.
webhook_role() {
  printf -- '---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: demo-webhook\nrules:\n%s\n' "$1"
}

PRIORITYCLASS_GET='- apiGroups:
  - scheduling.k8s.io
  resources:
  - priorityclasses
  verbs:
  - get'

# new_root <role.yaml content> — a fresh fixture repository in $TMP.
new_root() {
  TMP="$(mktemp -d)"
  OUT="$TMP/out"
  ERR="$TMP/err"
  mkdir -p "$TMP/operators/demo/config/rbac" "$TMP/$TEMPLATES"
  printf '%s\n' "$1" >"$TMP/operators/demo/config/rbac/role.yaml"
}

# gen [--check] — runs the script on $TMP; stdout and stderr go to $OUT and
# $ERR, the exit code to $RC.
gen() {
  python3 "$GEN" --repo-root "$TMP" "$@" >"$OUT" 2>"$ERR"
  RC=$?
}

# define_body <file> — the lines between the define line and {{- end }}.
define_body() {
  sed -n '/^{{- define /,/^{{- end }}$/p' "$1" | sed '1d;$d'
}

# ---------------------------------------------------------------------------
# Test 1: a run writes both templates
# ---------------------------------------------------------------------------
test_writes_both_templates() {
  echo "Test: a run writes the manager and the webhook template"
  new_root "$MANAGER_ROLE
$(webhook_role "$PRIORITYCLASS_GET")"
  gen
  assert_eq "exit code is 0" "0" "$RC"
  assert_file_contains_fixed "the manager template defines rbacRules" \
    "$TMP/$TEMPLATES/_rbac-rules.tpl" '{{- define "demo-operator.rbacRules" -}}'
  assert_file_contains_fixed "the webhook template defines webhookRbacRules" \
    "$TMP/$TEMPLATES/_webhook-rbac-rules.tpl" '{{- define "demo-operator.webhookRbacRules" -}}'
  assert_file_contains_fixed "the webhook template names its source ClusterRole" \
    "$TMP/$TEMPLATES/_webhook-rbac-rules.tpl" \
    "SOURCE: operators/demo/config/rbac/role.yaml (ClusterRole demo-webhook)"

  local body
  body="$(define_body "$TMP/$TEMPLATES/_webhook-rbac-rules.tpl")"
  assert_eq "the webhook rules are the demo-webhook rules verbatim" "$PRIORITYCLASS_GET" "$body"
  assert_not_contains "the webhook rules name no secrets" "$body" "secrets"
  assert_not_contains "the webhook rules carry no leases rule" "$body" "leases"

  local tail_want
  tail_want='- apiGroups:
  - coordination.k8s.io
  resources:
  - leases
  verbs:
  - get
  - list
  - watch
  - create
  - update
  - patch
  - delete
{{- end }}'
  assert_eq "the manager template ends with the leases rule" \
    "$tail_want" "$(tail -n 13 "$TMP/$TEMPLATES/_rbac-rules.tpl")"
  assert_contains "the manager rules keep the secrets grant" \
    "$(define_body "$TMP/$TEMPLATES/_rbac-rules.tpl")" "- secrets"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Test 2: --check passes on fresh output and reports a hand edit
# ---------------------------------------------------------------------------
test_check_reports_drift() {
  echo "Test: --check passes on fresh output and reports a hand-edited webhook template"
  new_root "$MANAGER_ROLE
$(webhook_role "$PRIORITYCLASS_GET")"
  gen
  gen --check
  assert_eq "--check exits 0 right after a run" "0" "$RC"
  assert_contains "--check reports success" "$(cat "$OUT")" "Helm RBAC rules check passed."

  echo "# hand edit" >>"$TMP/$TEMPLATES/_webhook-rbac-rules.tpl"
  gen --check
  assert_eq "--check exits 1 after the webhook template is edited" "1" "$RC"
  assert_contains "--check names the edited file" "$(cat "$OUT")" \
    "DRIFT: $TEMPLATES/_webhook-rbac-rules.tpl is out of sync with operators/demo/config/rbac/role.yaml"
  assert_not_contains "--check leaves the manager template alone" "$(cat "$OUT")" \
    "DRIFT: $TEMPLATES/_rbac-rules.tpl"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Test 3: a missing ClusterRole document
# ---------------------------------------------------------------------------
test_missing_roles() {
  echo "Test: a role.yaml without one of the two ClusterRoles exits 1"
  new_root "$MANAGER_ROLE"
  gen
  assert_eq "a missing demo-webhook exits 1" "1" "$RC"
  assert_contains "the message names demo-webhook and the marker to add" "$(cat "$ERR")" \
    "has no ClusterRole named demo-webhook; add a roleName=demo-webhook marker"
  assert_eq "nothing is written" "" "$(ls "$TMP/$TEMPLATES")"
  rm -rf "$TMP"

  new_root "$(webhook_role "$PRIORITYCLASS_GET")"
  gen
  assert_eq "a missing demo-operator exits 1" "1" "$RC"
  assert_contains "the message names demo-operator" "$(cat "$ERR")" \
    "has no ClusterRole named demo-operator; run 'make manifests' first"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Test 4: the coverage guard
# ---------------------------------------------------------------------------
test_uncovered_webhook_grant() {
  echo "Test: a webhook grant the manager role lacks exits 1"
  new_root "$MANAGER_ROLE
$(webhook_role '- apiGroups:
  - ""
  resources:
  - secrets
  verbs:
  - delete')"
  gen
  assert_eq "an uncovered verb exits 1" "1" "$RC"
  assert_contains "the message names the verb, the resource and the group" "$(cat "$ERR")" \
    'demo-webhook grants delete on secrets in apiGroup "" that demo-operator does not'
  assert_eq "nothing is written" "" "$(ls "$TMP/$TEMPLATES")"

  gen --check
  assert_eq "--check runs the guard too" "1" "$RC"
  rm -rf "$TMP"

  new_root "${MANAGER_ROLE/- watch/- \"*\"}
$(webhook_role '- apiGroups:
  - scheduling.k8s.io
  resources:
  - priorityclasses
  verbs:
  - delete')"
  gen
  assert_eq "a manager verb * covers every webhook verb" "0" "$RC"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Test 5: a failing chart leaves every other chart untouched
# ---------------------------------------------------------------------------
test_failing_chart_writes_no_chart() {
  echo "Test: a failing chart writes no template of a chart processed before it"
  new_root "$MANAGER_ROLE
$(webhook_role '- apiGroups:
  - ""
  resources:
  - secrets
  verbs:
  - delete')"
  # alpha sorts before demo, so the script reaches alpha's valid roles first.
  local alpha_templates="operators/alpha/helm/alpha-operator/templates"
  mkdir -p "$TMP/operators/alpha/config/rbac" "$TMP/$alpha_templates"
  printf '%s\n%s\n' "${MANAGER_ROLE//demo-/alpha-}" "$(webhook_role "$PRIORITYCLASS_GET")" \
    | sed 's/demo-webhook/alpha-webhook/' >"$TMP/operators/alpha/config/rbac/role.yaml"
  gen
  assert_eq "the uncovered demo grant exits 1" "1" "$RC"
  assert_eq "the valid alpha chart is not written" "" "$(ls "$TMP/$alpha_templates")"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Test 6: an empty webhook rule set
# ---------------------------------------------------------------------------
test_empty_webhook_rules() {
  echo "Test: an empty demo-webhook rules list renders an empty list"
  new_root "$MANAGER_ROLE
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: demo-webhook
rules: []"
  gen
  assert_eq "exit code is 0" "0" "$RC"
  assert_eq "the define body is an empty YAML list" "[]" \
    "$(define_body "$TMP/$TEMPLATES/_webhook-rbac-rules.tpl")"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if ! command -v python3 >/dev/null 2>&1; then
  echo "SKIP: python3 not installed (every case needs it)"
  SKIP=$((SKIP + 1))
else
  test_writes_both_templates
  test_check_reports_drift
  test_missing_roles
  test_uncovered_webhook_grant
  test_failing_chart_writes_no_chart
  test_empty_webhook_rules
fi

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
