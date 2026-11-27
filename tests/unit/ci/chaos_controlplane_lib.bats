#!/usr/bin/env bats
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
# Usage: bin/bats tests/unit/ci/chaos_controlplane_lib.bats

load '../../lib/bats/common'
load '../../lib/bats/stubs'

LIB="$PROJECT_ROOT/tests/e2e-chaos/lib/controlplane.sh"

# The stub kubectl appends every call to $STUB_STATE/calls and the stdin of
# apply and delete to $STUB_STATE/stdin. A condition status read prints
# STUB_STATUS and a reason read STUB_REASON, and both fail on STUB_GET_FAIL;
# the root token comes from STUB_TOKEN. run_lib turns those four into rules.
setup() {
  TMP_DIR="$BATS_TEST_TMPDIR"
  stub_on kubectl '*'
  stub_on kubectl 'apply* -f -*' --exec 'cat >>"$STUB_DIR/state/stdin"'
  stub_on kubectl 'delete* -f -*' --exec 'cat >>"$STUB_DIR/state/stdin"'
  stub_on kubectl 'get *{range .status.conditions*' --out "Ready=False (Stub: the stub's conditions)"
  reset_stub
  STUB="$STUB_DIR"
  # $STUB_STATE/calls is the call log of kubectl, so the bootstrap scripts
  # below log into it too and calls keeps the order of both.
  ln -s "$STUB/calls/kubectl" "$STUB/state/calls"

  # Both bootstrap scripts record the identity and whether the token reached
  # them, and fail on STUB_BOOTSTRAP_FAIL.
  for script in write-bootstrap-secrets.sh setup-database-tenant.sh; do
    stub_script "$script" <<'SH'
echo "$(basename "$0") KORC_CONTROLPLANES=${KORC_CONTROLPLANES:-} BAO_TOKEN=${BAO_TOKEN:+set} args=$*" >>"$STUB_STATE/calls"
[ -z "${STUB_BOOTSTRAP_FAIL:-}" ]
SH
  done
}

# reset_stub: the answers to a call that names no STUB_* value. run_lib
# declares them again before its own, so no value outlives its call.
reset_stub() {
  stub_on kubectl 'get *\].reason}*' --out-raw ''
  stub_on kubectl 'get *\].status}*' --out-raw ''
  stub_token ''
}

# stub_token <token>: the Secret openbao-init-keys with the root token.
stub_token() {
  stub_on kubectl 'get *openbao-init-keys*' --out "$(printf '{"root_token": "%s"}' "$1" | base64)"
}

# run_lib <VAR=value...> -- <shell code>: run code with the library sourced,
# the stubs first on PATH and NAMESPACE=chaos-ns; $output and $status hold the
# result. Each call starts with an empty call log and stdin, the kubectl
# answers of reset_stub, and the rules its STUB_* values name.
run_lib() {
  local envs=() get_fail=''
  reset_stub
  while [ "$1" != "--" ]; do
    case "$1" in
      STUB_STATUS=*) stub_on kubectl 'get *\].status}*' --out-raw "${1#*=}" ;;
      STUB_REASON=*) stub_on kubectl 'get *\].reason}*' --out-raw "${1#*=}" ;;
      STUB_TOKEN=*) stub_token "${1#*=}" ;;
      STUB_GET_FAIL=*) get_fail="${1#*=}" ;;
      *) envs+=("$1") ;;
    esac
    shift
  done
  shift
  # The failed read comes before every answer of a status or reason read.
  if [ -n "$get_fail" ]; then
    stub_on kubectl 'get *\].status}*' --rc 1 --err "Error from server: the stub refuses the read"
    stub_on kubectl 'get *\].reason}*' --rc 1 --err "Error from server: the stub refuses the read"
  fi
  rm -f "$STUB/state/stdin"
  : >"$STUB/state/calls"
  run with_stubs env STUB_STATE="$STUB/state" NAMESPACE=chaos-ns \
    ${envs[@]+"${envs[@]}"} bash -c ". '$LIB'; CP_BOOTSTRAP_DIR='$STUB/bin'; $1"
}

calls() {
  cat "$STUB/state/calls"
}

# --- Test 1 ---------------------------------------------------------------
@test "cp_wait_condition passes on the wanted status and fails at its deadline" {
  run_lib STUB_STATUS=True -- 'cp_wait_condition controlplane cp-x KeystoneReady True 60'
  assert_eq "it returns 0 on True" "0" "$status"
  assert_contains "it says so" "$output" "OK: controlplane/cp-x KeystoneReady=True"
  assert_contains "it reads the condition in NAMESPACE" "$(calls)" \
    "get controlplane cp-x -n chaos-ns -o jsonpath={.status.conditions[?(@.type=='KeystoneReady')].status}"

  run_lib STUB_STATUS=False -- 'cp_wait_condition controlplane cp-x KeystoneReady True 0'
  assert_eq "it returns 1 at the deadline" "1" "$status"
  assert_contains "it names the last status" "$output" \
    "ERROR: controlplane/cp-x KeystoneReady not True within 0s (last: 'False')"
  assert_contains "it prints the CR's conditions" "$output" "Ready=False (Stub: the stub's conditions)"

  run_lib -- 'cp_wait_condition controlplane cp-x KeystoneReady True 0'
  assert_contains "an absent condition reads <none>" "$output" "(last: '<none>')"
}

# --- Test 2 ---------------------------------------------------------------
@test "cp_export_bao_token exports the root token and refuses an empty one" {
  run_lib STUB_TOKEN=s3cret -- 'cp_export_bao_token && echo "token=${BAO_TOKEN}"'
  assert_eq "it returns 0 with a token" "0" "$status"
  assert_contains "it exports the root token" "$output" "token=s3cret"

  local token
  for token in "" null; do
    run_lib STUB_TOKEN="$token" -- 'cp_export_bao_token'
    soft assert_eq "it returns 1 on '${token}'" "1" "$status"
    soft assert_contains "it names the Secret" "$output" \
      "ERROR: no OpenBao root token in Secret shared-services/openbao-init-keys"
  done
}

# --- Test 3 ---------------------------------------------------------------
@test "cp_seed seeds NAMESPACE/<cp> with the root token and fails with the script" {
  run_lib STUB_TOKEN=s3cret -- 'cp_seed cp-x'
  assert_eq "it returns 0" "0" "$status"
  assert_contains "the script seeds the plane's identity with the token" "$(calls)" \
    "write-bootstrap-secrets.sh KORC_CONTROLPLANES=chaos-ns/cp-x BAO_TOKEN=set args="

  run_lib STUB_TOKEN=s3cret STUB_BOOTSTRAP_FAIL=1 -- 'cp_seed cp-x'
  assert_eq "a failed script fails the seed" "1" "$status"
  assert_not_contains "and it reports no success" "$output" "OK:"

  run_lib STUB_TOKEN= -- 'cp_seed cp-x'
  assert_eq "without a token nothing is seeded" "1" "$status"
  assert_not_contains "and the script never runs" "$(calls)" "write-bootstrap-secrets.sh"
}

# --- Test 4 ---------------------------------------------------------------
@test "cp_wait_tail waits for the four conditions and refuses a reason other than AllReady" {
  run_lib STUB_STATUS=True STUB_REASON=AllReady -- 'cp_wait_tail cp-x'
  assert_eq "it returns 0 on AllReady" "0" "$status"
  local ctype
  for ctype in KORCReady AdminCredentialReady ESOTenantStoreReady Ready; do
    soft assert_contains "it waits for ${ctype}" "$output" "OK: controlplane/cp-x ${ctype}=True"
  done

  run_lib STUB_STATUS=True STUB_REASON=NotAllReady -- 'cp_wait_tail cp-x'
  assert_eq "it returns 1 on another reason" "1" "$status"
  assert_contains "it names the reason" "$output" \
    "ERROR: controlplane/cp-x is Ready with reason 'NotAllReady', not AllReady"
}

# --- Test 5 ---------------------------------------------------------------
@test "cp_bring_up runs the sequence in order and stops at its first failure" {
  run_lib STUB_TOKEN=s3cret STUB_STATUS=True STUB_REASON=AllReady -- 'cp_bring_up cp-x 00-controlplane-cr.yaml'
  assert_eq "it returns 0" "0" "$status"
  local order
  order="$(calls | grep -oE 'write-bootstrap-secrets.sh|apply -n chaos-ns -f 00-controlplane-cr.yaml|InfrastructureReady|wait mariadb/openstack-db|setup-database-tenant.sh|DBCredentialsReady|KeystoneReady|KORCReady' | uniq | xargs)"
  assert_eq "seed, apply, infrastructure, MariaDB, tenant, credentials, Keystone, K-ORC" \
    "write-bootstrap-secrets.sh apply -n chaos-ns -f 00-controlplane-cr.yaml InfrastructureReady wait mariadb/openstack-db setup-database-tenant.sh DBCredentialsReady KeystoneReady KORCReady" \
    "$order"
  assert_contains "the tenant script gets the namespace and the plane" "$(calls)" \
    "setup-database-tenant.sh KORC_CONTROLPLANES= BAO_TOKEN=set args=chaos-ns cp-x"

  run_lib STUB_TOKEN=s3cret STUB_BOOTSTRAP_FAIL=1 -- 'cp_bring_up cp-x 00-controlplane-cr.yaml'
  assert_eq "a failed seed fails the bring-up" "1" "$status"
  assert_not_contains "and the ControlPlane is never applied" "$(calls)" "apply"
}

# --- Test 6 ---------------------------------------------------------------
@test "cp_render, cp_apply_file and cp_delete_file replace @NS@ and act in NAMESPACE" {
  printf 'metadata:\n  namespace: @NS@\nspec:\n  url: http://x.@NS@.svc\n' >"$TMP_DIR/fixture.yaml"
  local rendered
  rendered="$(printf 'metadata:\n  namespace: chaos-ns\nspec:\n  url: http://x.chaos-ns.svc')"
  run_lib -- "cp_render '$TMP_DIR/fixture.yaml'"
  assert_eq "cp_render prints the fixture with every @NS@ the namespace" "$rendered" "$output"
  assert_eq "and calls nothing" "" "$(calls)"

  run_lib -- "cp_apply_file '$TMP_DIR/fixture.yaml'"
  assert_eq "it returns 0" "0" "$status"
  assert_contains "it applies in NAMESPACE from stdin" "$(calls)" "apply -n chaos-ns -f -"
  assert_eq "every @NS@ is the namespace" "$rendered" "$(cat "$STUB/state/stdin")"

  run_lib -- "cp_delete_file '$TMP_DIR/fixture.yaml'"
  assert_contains "the delete tolerates an absent object" "$(calls)" "delete -n chaos-ns --ignore-not-found -f -"
  assert_not_contains "and gets the namespace in" "$(cat "$STUB/state/stdin")" "@NS@"
}

# --- Test 7 ---------------------------------------------------------------
@test "cp_teardown deletes the ControlPlane and waits at most 12 minutes" {
  run_lib -- 'cp_teardown cp-x'
  assert_eq "it returns 0" "0" "$status"
  assert_eq "one bounded, waited delete" "delete controlplane cp-x -n chaos-ns --ignore-not-found --wait --timeout=12m" \
    "$(calls)"
}

# --- Test 8 ---------------------------------------------------------------
@test "the library parses and sourcing it calls nothing" {
  assert_eq "bash -n passes" "0" "$(bash -n "$LIB" 2>&1 >/dev/null; echo $?)"
  run_lib -- 'true'
  assert_eq "sourcing it runs no kubectl" "" "$(calls)"
}

# --- Test 9 ---------------------------------------------------------------
@test "cp_status and cp_reason read one condition and fail on a failed read" {
  run_lib STUB_STATUS=False STUB_REASON=NotAllReady -- \
    'echo "$(cp_status controlplane cp-x Ready)/$(cp_reason controlplane cp-x Ready)"'
  assert_eq "they read the status and the reason" "False/NotAllReady" "$output"
  assert_contains "cp_status reads the condition in NAMESPACE" "$(calls)" \
    "get controlplane cp-x -n chaos-ns -o jsonpath={.status.conditions[?(@.type=='Ready')].status}"

  local reader
  for reader in cp_status cp_reason; do
    run_lib STUB_GET_FAIL=1 -- "${reader} controlplane cp-x Ready"
    soft assert_eq "${reader} returns 1 on a failed read" "1" "$status"
    soft assert_contains "and passes the error on" "$output" "Error from server: the stub refuses the read"
  done

  run_lib STUB_GET_FAIL=1 -- 'cp_wait_condition controlplane cp-x Ready True 0'
  assert_eq "cp_wait_condition retries an unreadable condition to its deadline" "1" "$status"
  assert_contains "and names it as no status" "$output" "(last: '<none>')"
}

# --- Test 10 --------------------------------------------------------------
@test "no chaos suite tests a cp_status or cp_reason read, or compares a cond read with True, inside a [ ] test" {
  local hits
  hits="$(grep -rnE '\[ +!? *"?\$\((cp_status|cp_reason) ' "$PROJECT_ROOT/tests/e2e-chaos" || true)"
  assert_eq "every read is assigned before it is tested" "" "$hits"
  hits="$(grep -rnE '\[ +!? *"?\$\(cond [^)]*\)"? +==? +"?True' "$PROJECT_ROOT/tests/e2e-chaos" || true)"
  assert_eq "every cond read compared with True is assigned before it is tested" "" "$hits"
}

# --- Run ---
