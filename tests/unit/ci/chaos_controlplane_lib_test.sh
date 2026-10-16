#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the ControlPlane bring-up library of the chaos suites on the
# controlplane leg (tests/e2e-chaos/lib/controlplane.sh, #1390), against a
# stub kubectl and stub OpenBao bootstrap scripts:
#   1. cp_wait_condition passes on the wanted status and fails at its deadline
#      naming the last status and the CR's conditions.
#   2. cp_export_bao_token refuses an empty or null root token.
#   3. cp_seed hands the bootstrap script the plane's identity and the token,
#      and fails with it.
#   4. cp_wait_tail refuses a Ready that is not AllReady.
#   5. cp_bring_up stops at its first failure.
#   6. cp_render, cp_apply_file and cp_delete_file put the namespace in for
#      @NS@.
#   7. cp_teardown waits for the ControlPlane with a 12-minute bound.
#   8. The library parses, and sourcing it calls nothing.
#   9. cp_status and cp_reason read one condition and fail on a failed read,
#      which cp_wait_condition retries.
#  10. No chaos suite tests a cp_status or cp_reason read inside `[`, where
#      bash drops the read's exit status and set -e misses a failed read,
#      and no suite compares a cond read with True there, where a failed
#      read passes a check that nothing is Ready.
#
# Usage: bash tests/unit/ci/chaos_controlplane_lib_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
LIB="$PROJECT_ROOT/tests/e2e-chaos/lib/controlplane.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT
STUB="$TMP_DIR/stub"
mkdir -p "$STUB/bin" "$STUB/bootstrap"

# The stub kubectl appends every call to $STUB_STATE/calls and the stdin of
# apply and delete to $STUB_STATE/stdin. A condition status read prints
# STUB_STATUS and a reason read STUB_REASON, and both fail on STUB_GET_FAIL;
# the root token comes from STUB_TOKEN.
cat >"$STUB/bin/kubectl" <<'SH'
#!/bin/bash
echo "$*" >>"$STUB_STATE/calls"
case "$1" in
  get)
    case "$*" in
      *"].status}"* | *"].reason}"*)
        if [ -n "${STUB_GET_FAIL:-}" ]; then
          echo "Error from server: the stub refuses the read" >&2
          exit 1
        fi
        ;;
    esac
    case "$*" in
      *openbao-init-keys*)
        printf '{"root_token": "%s"}' "${STUB_TOKEN:-}" | base64
        ;;
      *"].status}"*) printf '%s' "${STUB_STATUS:-}" ;;
      *"].reason}"*) printf '%s' "${STUB_REASON:-}" ;;
      *"{range .status.conditions"*) echo "Ready=False (Stub: the stub's conditions)" ;;
    esac
    ;;
  apply | delete)
    case "$*" in *" -f -"*) cat >>"$STUB_STATE/stdin" ;; esac
    ;;
esac
exit 0
SH
# Both bootstrap scripts record the identity and whether the token reached
# them, and fail on STUB_BOOTSTRAP_FAIL.
for script in write-bootstrap-secrets.sh setup-database-tenant.sh; do
  cat >"$STUB/bootstrap/$script" <<'SH'
#!/bin/bash
echo "$(basename "$0") KORC_CONTROLPLANES=${KORC_CONTROLPLANES:-} BAO_TOKEN=${BAO_TOKEN:+set} args=$*" >>"$STUB_STATE/calls"
[ -z "${STUB_BOOTSTRAP_FAIL:-}" ]
SH
done
chmod +x "$STUB/bin/kubectl" "$STUB"/bootstrap/*.sh

# run_lib <VAR=value...> -- <shell code>: run code with the library sourced,
# the stubs first on PATH and NAMESPACE=chaos-ns; CP_OUT and CP_RC hold the
# result.
run_lib() {
  local envs=()
  while [ "$1" != "--" ]; do
    envs+=("$1")
    shift
  done
  shift
  rm -rf "$STUB/state"
  mkdir -p "$STUB/state"
  : >"$STUB/state/calls"
  CP_RC=0
  CP_OUT="$(env STUB_STATE="$STUB/state" NAMESPACE=chaos-ns PATH="$STUB/bin:$PATH" \
    ${envs[@]+"${envs[@]}"} bash -c ". '$LIB'; CP_BOOTSTRAP_DIR='$STUB/bootstrap'; $1" 2>&1)" || CP_RC=$?
}

calls() {
  cat "$STUB/state/calls"
}

# --- Test 1 ---------------------------------------------------------------
test_wait_condition_passes_on_the_status_and_fails_at_the_deadline() {
  echo "Test: cp_wait_condition passes on the wanted status and fails at its deadline"
  run_lib STUB_STATUS=True -- 'cp_wait_condition controlplane cp-x KeystoneReady True 60'
  assert_eq "it returns 0 on True" "0" "$CP_RC"
  assert_contains "it says so" "$CP_OUT" "OK: controlplane/cp-x KeystoneReady=True"
  assert_contains "it reads the condition in NAMESPACE" "$(calls)" \
    "get controlplane cp-x -n chaos-ns -o jsonpath={.status.conditions[?(@.type=='KeystoneReady')].status}"

  run_lib STUB_STATUS=False -- 'cp_wait_condition controlplane cp-x KeystoneReady True 0'
  assert_eq "it returns 1 at the deadline" "1" "$CP_RC"
  assert_contains "it names the last status" "$CP_OUT" \
    "ERROR: controlplane/cp-x KeystoneReady not True within 0s (last: 'False')"
  assert_contains "it prints the CR's conditions" "$CP_OUT" "Ready=False (Stub: the stub's conditions)"

  run_lib -- 'cp_wait_condition controlplane cp-x KeystoneReady True 0'
  assert_contains "an absent condition reads <none>" "$CP_OUT" "(last: '<none>')"
}

# --- Test 2 ---------------------------------------------------------------
test_export_bao_token_refuses_an_empty_token() {
  echo "Test: cp_export_bao_token exports the root token and refuses an empty one"
  run_lib STUB_TOKEN=s3cret -- 'cp_export_bao_token && echo "token=${BAO_TOKEN}"'
  assert_eq "it returns 0 with a token" "0" "$CP_RC"
  assert_contains "it exports the root token" "$CP_OUT" "token=s3cret"

  local token
  for token in "" null; do
    run_lib STUB_TOKEN="$token" -- 'cp_export_bao_token'
    assert_eq "it returns 1 on '${token}'" "1" "$CP_RC"
    assert_contains "it names the Secret" "$CP_OUT" \
      "ERROR: no OpenBao root token in Secret shared-services/openbao-init-keys"
  done
}

# --- Test 3 ---------------------------------------------------------------
test_seed_hands_the_identity_and_the_token_to_the_bootstrap_script() {
  echo "Test: cp_seed seeds NAMESPACE/<cp> with the root token and fails with the script"
  run_lib STUB_TOKEN=s3cret -- 'cp_seed cp-x'
  assert_eq "it returns 0" "0" "$CP_RC"
  assert_contains "the script seeds the plane's identity with the token" "$(calls)" \
    "write-bootstrap-secrets.sh KORC_CONTROLPLANES=chaos-ns/cp-x BAO_TOKEN=set args="

  run_lib STUB_TOKEN=s3cret STUB_BOOTSTRAP_FAIL=1 -- 'cp_seed cp-x'
  assert_eq "a failed script fails the seed" "1" "$CP_RC"
  assert_not_contains "and it reports no success" "$CP_OUT" "OK:"

  run_lib STUB_TOKEN= -- 'cp_seed cp-x'
  assert_eq "without a token nothing is seeded" "1" "$CP_RC"
  assert_not_contains "and the script never runs" "$(calls)" "write-bootstrap-secrets.sh"
}

# --- Test 4 ---------------------------------------------------------------
test_wait_tail_refuses_a_ready_that_is_not_all_ready() {
  echo "Test: cp_wait_tail waits for the four conditions and refuses a reason other than AllReady"
  run_lib STUB_STATUS=True STUB_REASON=AllReady -- 'cp_wait_tail cp-x'
  assert_eq "it returns 0 on AllReady" "0" "$CP_RC"
  local ctype
  for ctype in KORCReady AdminCredentialReady ESOTenantStoreReady Ready; do
    assert_contains "it waits for ${ctype}" "$CP_OUT" "OK: controlplane/cp-x ${ctype}=True"
  done

  run_lib STUB_STATUS=True STUB_REASON=NotAllReady -- 'cp_wait_tail cp-x'
  assert_eq "it returns 1 on another reason" "1" "$CP_RC"
  assert_contains "it names the reason" "$CP_OUT" \
    "ERROR: controlplane/cp-x is Ready with reason 'NotAllReady', not AllReady"
}

# --- Test 5 ---------------------------------------------------------------
test_bring_up_stops_at_its_first_failure() {
  echo "Test: cp_bring_up runs the sequence in order and stops at its first failure"
  run_lib STUB_TOKEN=s3cret STUB_STATUS=True STUB_REASON=AllReady -- 'cp_bring_up cp-x 00-controlplane-cr.yaml'
  assert_eq "it returns 0" "0" "$CP_RC"
  local order
  order="$(calls | grep -oE 'write-bootstrap-secrets.sh|apply -n chaos-ns -f 00-controlplane-cr.yaml|InfrastructureReady|wait mariadb/openstack-db|setup-database-tenant.sh|DBCredentialsReady|KeystoneReady|KORCReady' | uniq | xargs)"
  assert_eq "seed, apply, infrastructure, MariaDB, tenant, credentials, Keystone, K-ORC" \
    "write-bootstrap-secrets.sh apply -n chaos-ns -f 00-controlplane-cr.yaml InfrastructureReady wait mariadb/openstack-db setup-database-tenant.sh DBCredentialsReady KeystoneReady KORCReady" \
    "$order"
  assert_contains "the tenant script gets the namespace and the plane" "$(calls)" \
    "setup-database-tenant.sh KORC_CONTROLPLANES= BAO_TOKEN=set args=chaos-ns cp-x"

  run_lib STUB_TOKEN=s3cret STUB_BOOTSTRAP_FAIL=1 -- 'cp_bring_up cp-x 00-controlplane-cr.yaml'
  assert_eq "a failed seed fails the bring-up" "1" "$CP_RC"
  assert_not_contains "and the ControlPlane is never applied" "$(calls)" "apply"
}

# --- Test 6 ---------------------------------------------------------------
test_apply_and_delete_file_put_the_namespace_in() {
  echo "Test: cp_render, cp_apply_file and cp_delete_file replace @NS@ and act in NAMESPACE"
  printf 'metadata:\n  namespace: @NS@\nspec:\n  url: http://x.@NS@.svc\n' >"$TMP_DIR/fixture.yaml"
  local rendered
  rendered="$(printf 'metadata:\n  namespace: chaos-ns\nspec:\n  url: http://x.chaos-ns.svc')"
  run_lib -- "cp_render '$TMP_DIR/fixture.yaml'"
  assert_eq "cp_render prints the fixture with every @NS@ the namespace" "$rendered" "$CP_OUT"
  assert_eq "and calls nothing" "" "$(calls)"

  run_lib -- "cp_apply_file '$TMP_DIR/fixture.yaml'"
  assert_eq "it returns 0" "0" "$CP_RC"
  assert_contains "it applies in NAMESPACE from stdin" "$(calls)" "apply -n chaos-ns -f -"
  assert_eq "every @NS@ is the namespace" "$rendered" "$(cat "$STUB/state/stdin")"

  run_lib -- "cp_delete_file '$TMP_DIR/fixture.yaml'"
  assert_contains "the delete tolerates an absent object" "$(calls)" "delete -n chaos-ns --ignore-not-found -f -"
  assert_not_contains "and gets the namespace in" "$(cat "$STUB/state/stdin")" "@NS@"
}

# --- Test 7 ---------------------------------------------------------------
test_teardown_waits_twelve_minutes() {
  echo "Test: cp_teardown deletes the ControlPlane and waits at most 12 minutes"
  run_lib -- 'cp_teardown cp-x'
  assert_eq "it returns 0" "0" "$CP_RC"
  assert_eq "one bounded, waited delete" "delete controlplane cp-x -n chaos-ns --ignore-not-found --wait --timeout=12m" \
    "$(calls)"
}

# --- Test 8 ---------------------------------------------------------------
test_the_library_parses_and_has_no_side_effect_on_source() {
  echo "Test: the library parses and sourcing it calls nothing"
  assert_eq "bash -n passes" "0" "$(bash -n "$LIB" 2>&1 >/dev/null; echo $?)"
  run_lib -- 'true'
  assert_eq "sourcing it runs no kubectl" "" "$(calls)"
}

# --- Test 9 ---------------------------------------------------------------
test_status_and_reason_fail_on_a_failed_read() {
  echo "Test: cp_status and cp_reason read one condition and fail on a failed read"
  run_lib STUB_STATUS=False STUB_REASON=NotAllReady -- \
    'echo "$(cp_status controlplane cp-x Ready)/$(cp_reason controlplane cp-x Ready)"'
  assert_eq "they read the status and the reason" "False/NotAllReady" "$CP_OUT"
  assert_contains "cp_status reads the condition in NAMESPACE" "$(calls)" \
    "get controlplane cp-x -n chaos-ns -o jsonpath={.status.conditions[?(@.type=='Ready')].status}"

  local reader
  for reader in cp_status cp_reason; do
    run_lib STUB_GET_FAIL=1 -- "${reader} controlplane cp-x Ready"
    assert_eq "${reader} returns 1 on a failed read" "1" "$CP_RC"
    assert_contains "and passes the error on" "$CP_OUT" "Error from server: the stub refuses the read"
  done

  run_lib STUB_GET_FAIL=1 -- 'cp_wait_condition controlplane cp-x Ready True 0'
  assert_eq "cp_wait_condition retries an unreadable condition to its deadline" "1" "$CP_RC"
  assert_contains "and names it as no status" "$CP_OUT" "(last: '<none>')"
}

# --- Test 10 --------------------------------------------------------------
test_no_suite_tests_a_read_inside_a_test_command() {
  echo "Test: no chaos suite tests a cp_status or cp_reason read, or compares a cond read with True, inside a [ ] test"
  local hits
  hits="$(grep -rnE '\[ +!? *"?\$\((cp_status|cp_reason) ' "$PROJECT_ROOT/tests/e2e-chaos" || true)"
  assert_eq "every read is assigned before it is tested" "" "$hits"
  hits="$(grep -rnE '\[ +!? *"?\$\(cond [^)]*\)"? +==? +"?True' "$PROJECT_ROOT/tests/e2e-chaos" || true)"
  assert_eq "every cond read compared with True is assigned before it is tested" "" "$hits"
}

# --- Run ---
test_wait_condition_passes_on_the_status_and_fails_at_the_deadline
test_export_bao_token_refuses_an_empty_token
test_seed_hands_the_identity_and_the_token_to_the_bootstrap_script
test_wait_tail_refuses_a_ready_that_is_not_all_ready
test_bring_up_stops_at_its_first_failure
test_apply_and_delete_file_put_the_namespace_in
test_teardown_waits_twelve_minutes
test_the_library_parses_and_has_no_side_effect_on_source
test_status_and_reason_fail_on_a_failed_read
test_no_suite_tests_a_read_inside_a_test_command

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
