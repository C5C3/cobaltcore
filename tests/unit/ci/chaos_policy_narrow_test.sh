#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify narrow() of the openbao-tenant-policy-narrowed chaos suite
# (tests/e2e-chaos/openbao-tenant-policy-narrowed/probe-lib.sh, #1390) offline,
# against deploy/openbao/policies/eso-tenant.hcl with the accessor substituted
# the way setup-policies.sh writes it into OpenBao:
#   1. On the shipped policy it exits 0 and drops exactly the stanza that
#      grants read on the caller's own Keystone subtree, three lines.
#   2. The fernet-key, credential-key and every other stanza stay.
#   3. A policy with that stanza twice, or without it, exits 1 and says how
#      many stanzas matched.
#
# The suite runs narrow() only against the live policy, after its Keystone
# bring-up; a stanza another change adds to the shared policy would otherwise
# surface there first.
#
# Usage: bash tests/unit/ci/chaos_policy_narrow_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
PROBE_LIB="$PROJECT_ROOT/tests/e2e-chaos/openbao-tenant-policy-narrowed/probe-lib.sh"
POLICY_FILE="$PROJECT_ROOT/deploy/openbao/policies/eso-tenant.hcl"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

READ_STANZA='path "kv-v2/data/openstack/keystone/{{identity.entity.aliases.auth_kubernetes_0123.metadata.service_account_namespace}}/*" {'

# The policy as OpenBao holds it: the placeholder replaced by an accessor.
POLICY="$(sed 's/KUBERNETES_MANAGEMENT_ACCESSOR/auth_kubernetes_0123/g' "$POLICY_FILE")"

# run_narrow <policy>: narrow() with the probe library sourced; NARROW_OUT,
# NARROW_ERR and NARROW_RC hold the result.
run_narrow() {
  local err
  err="$(mktemp)"
  NARROW_RC=0
  NARROW_OUT="$(bash -c ". '$PROBE_LIB'; narrow" <<<"$1" 2>"$err")" || NARROW_RC=$?
  NARROW_ERR="$(cat "$err")"
  rm -f "$err"
}

# --- Test 1 ---------------------------------------------------------------
test_narrow_drops_the_one_read_stanza() {
  echo "Test: narrow drops the Keystone read stanza of the shipped policy and nothing else"
  run_narrow "$POLICY"
  assert_eq "it exits 0" "0" "$NARROW_RC"
  assert_eq "it reports nothing" "" "$NARROW_ERR"
  assert_contains "the input holds the read stanza" "$POLICY" "$READ_STANZA"
  assert_not_contains "the output does not" "$NARROW_OUT" "$READ_STANZA"
  assert_eq "exactly three lines go" "3" \
    "$(diff <(printf '%s\n' "$POLICY") <(printf '%s\n' "$NARROW_OUT") | grep -c '^<')"
  assert_eq "no line is added" "0" \
    "$(diff <(printf '%s\n' "$POLICY") <(printf '%s\n' "$NARROW_OUT") | grep -c '^>')"
}

# --- Test 2 ---------------------------------------------------------------
test_narrow_keeps_the_sibling_stanzas() {
  echo "Test: narrow keeps the stanzas under the same prefix and every other one"
  run_narrow "$POLICY"
  local leaf
  for leaf in fernet-keys credential-keys; do
    assert_contains "the data stanza of ${leaf} stays" "$NARROW_OUT" "/+/${leaf}\" {"
  done
  assert_eq "every other path stanza stays" \
    "$(($(grep -c '^path ' <<<"$POLICY") - 1))" "$(grep -c '^path ' <<<"$NARROW_OUT")"
}

# --- Test 3 ---------------------------------------------------------------
test_narrow_refuses_any_count_but_one() {
  echo "Test: narrow exits 1 when the read stanza is there twice or not at all"
  local without
  run_narrow "$POLICY"
  without="$NARROW_OUT"

  run_narrow "$(printf '%s\n%s\n  capabilities = ["read"]\n}\n' "$POLICY" "$READ_STANZA")"
  assert_eq "two stanzas: exit 1" "1" "$NARROW_RC"
  assert_eq "it names the count" "FAIL: narrowing matched 2 stanzas" "$NARROW_ERR"

  run_narrow "$without"
  assert_eq "no stanza: exit 1" "1" "$NARROW_RC"
  assert_eq "it names the count" "FAIL: narrowing matched 0 stanzas" "$NARROW_ERR"
}

# --- Run ---
test_narrow_drops_the_one_read_stanza
test_narrow_keeps_the_sibling_stanzas
test_narrow_refuses_any_count_but_one

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
