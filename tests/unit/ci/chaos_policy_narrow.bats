#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify narrow() of the openbao-tenant-policy-narrowed chaos suite
# (tests/e2e-chaos/openbao-tenant-policy-narrowed/probe-lib.sh, #1390) offline,
# against deploy/bases/shared-openbao/policies/eso-tenant.hcl with the accessor substituted
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
# Usage: bin/bats tests/unit/ci/chaos_policy_narrow.bats

load '../../lib/bats/common'

PROBE_LIB="$PROJECT_ROOT/tests/e2e-chaos/openbao-tenant-policy-narrowed/probe-lib.sh"
POLICY_FILE="$PROJECT_ROOT/deploy/bases/shared-openbao/policies/eso-tenant.hcl"

READ_STANZA='path "kv-v2/data/openstack/keystone/{{identity.entity.aliases.auth_kubernetes_0123.metadata.service_account_namespace}}/*" {'

# The policy as OpenBao holds it: the placeholder replaced by an accessor.
POLICY="$(sed 's/KUBERNETES_MANAGEMENT_ACCESSOR/auth_kubernetes_0123/g' "$POLICY_FILE")"

# run_narrow <policy>: narrow() with the probe library sourced; $output and
# $status of run hold the result, and $stderr is read from its own file as
# before (run --separate-stderr would strip its leading and trailing spaces).
run_narrow() {
  local err="$BATS_TEST_TMPDIR/narrow.err"
  run bash -c "exec 2>\"\$0\"; . '$PROBE_LIB'; narrow" "$err" <<<"$1"
  stderr="$(cat "$err")"
}

# --- Test 1 ---------------------------------------------------------------
@test "narrow drops the Keystone read stanza of the shipped policy and nothing else" {
  run_narrow "$POLICY"
  assert_eq "it exits 0" "0" "$status"
  assert_eq "it reports nothing" "" "$stderr"
  assert_contains "the input holds the read stanza" "$POLICY" "$READ_STANZA"
  assert_not_contains "the output does not" "$output" "$READ_STANZA"
  assert_eq "exactly three lines go" "3" \
    "$(diff <(printf '%s\n' "$POLICY") <(printf '%s\n' "$output") | grep -c '^<')"
  assert_eq "no line is added" "0" \
    "$(diff <(printf '%s\n' "$POLICY") <(printf '%s\n' "$output") | grep -c '^>')"
}

# --- Test 2 ---------------------------------------------------------------
@test "narrow keeps the stanzas under the same prefix and every other one" {
  run_narrow "$POLICY"
  local leaf
  for leaf in fernet-keys credential-keys; do
    soft assert_contains "the data stanza of ${leaf} stays" "$output" "/+/${leaf}\" {"
  done
  assert_eq "every other path stanza stays" \
    "$(($(grep -c '^path ' <<<"$POLICY") - 1))" "$(grep -c '^path ' <<<"$output")"
}

# --- Test 3 ---------------------------------------------------------------
@test "narrow exits 1 when the read stanza is there twice or not at all" {
  local without
  run_narrow "$POLICY"
  without="$output"

  run_narrow "$(printf '%s\n%s\n  capabilities = ["read"]\n}\n' "$POLICY" "$READ_STANZA")"
  assert_eq "two stanzas: exit 1" "1" "$status"
  assert_eq "it names the count" "FAIL: narrowing matched 2 stanzas" "$stderr"

  run_narrow "$without"
  assert_eq "no stanza: exit 1" "1" "$status"
  assert_eq "it names the count" "FAIL: narrowing matched 0 stanzas" "$stderr"
}
