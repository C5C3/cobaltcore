#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the three OpenBao bootstrap scripts provision the Barbican secret-store
# leg, and provision it exactly once.
#
#   - setup-secret-engines.sh must mount KV v2 at barbican/ when the engine list
#     lacks it (including on a fresh instance whose list is empty), and must skip
#     the enable when it is already mounted. A second enable on an existing mount
#     is an error, so a re-run of the bootstrap would fail the whole script.
#   - setup-auth.sh must upsert the approle role Barbican's vault_plugin logs in
#     with, bound to the barbican-secretstore policy alone. A second policy on
#     that role widens what a leaked Barbican secret ID reaches.
#   - that policy must exist and grant what the role is bound to. setup-policies.sh
#     derives every policy name from its file basename, so token_policies=
#     barbican-secretstore is a string coupling to a file — a rename on either
#     side leaves the role bound to a policy OpenBao does not have.
#   - write-bootstrap-secrets.sh must seed the proving instance's unseal key when
#     it is missing and NEVER rewrite it. The key is the instance's static seal:
#     a rotated value seals the instance permanently, with no path back to the
#     stored data.
#
# All three scripts are driven with a recording kubectl stub, so nothing touches
# a cluster or an OpenBao. The stub answers the lookups each script makes on the
# way to the behavior under test and appends every invocation to a log file, so
# the assertions can check the exact arguments that reached the pod.
#
# Usage: bash tests/unit/deploy/barbican_secretstore_bootstrap_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
ENGINES_SCRIPT="$PROJECT_ROOT/deploy/openbao/bootstrap/setup-secret-engines.sh"
AUTH_SCRIPT="$PROJECT_ROOT/deploy/openbao/bootstrap/setup-auth.sh"
SEED_SCRIPT="$PROJECT_ROOT/deploy/openbao/bootstrap/write-bootstrap-secrets.sh"
POLICY_FILE="$PROJECT_ROOT/deploy/openbao/policies/barbican-secretstore.hcl"

# The static seal key of the proving OpenBao instance
# (deploy/kind/infrastructure/openbao-instance.yaml).
UNSEAL_PATH="kv-v2/bootstrap/openstack/openbao-instance/unseal-key"

# A typical engine map of an already-bootstrapped instance, minus barbican/.
ENGINES_WITHOUT_BARBICAN='{"kv-v2/":{},"pki/":{},"database/mariadb/":{}}'
ENGINES_WITH_BARBICAN='{"kv-v2/":{},"pki/":{},"database/mariadb/":{},"barbican/":{}}'
# A fresh instance: no engine is mounted yet.
ENGINES_EMPTY='{}'

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# make_kubectl <dir> <log> <secrets_json> <readable_kv_path>
# Installs a recording kubectl stub in <dir>. Every invocation's argument string
# lands in <log>; the canned answers are the ones the scripts branch on:
#
#   - `bao secrets list` returns <secrets_json>, the engine map
#     setup-secret-engines.sh pipes through `jq -r 'keys[]'`.
#   - `bao auth list` returns an empty map, so setup-auth.sh enables its mounts
#     (the enables themselves are recorded and swallowed).
#   - `bao kv get` succeeds only for <readable_kv_path> — that is the existence
#     probe write_secret_if_missing reads. Every other path reports a miss and is
#     therefore seeded.
#
# Everything else (engine enables, role writes, kv puts, metadata puts) is
# recorded and answered with exit 0, so each script runs to completion.
make_kubectl() {
  local dir="$1" log="$2" secrets_json="$3" readable_kv_path="$4"
  mkdir -p "$dir"
  : >"$log"
  cat >"$dir/kubectl" <<STUB
#!/bin/bash
printf '%s\n' "\$*" >>"${log}"

case "\$*" in
  *"bao secrets list"*)
    printf '%s' '${secrets_json}'
    ;;
  *"bao auth list"*)
    printf '%s' '{}'
    ;;
  *"bao kv get"*)
    if [[ -n "${readable_kv_path}" && "\$*" == *"${readable_kv_path}"* ]]; then
      printf '%s' '{"data":{"data":{}}}'
      exit 0
    fi
    exit 1
    ;;
esac
exit 0
STUB
  chmod +x "$dir/kubectl"
}

# make_failing_kubectl <dir> <log>
# A recording kubectl stub whose every invocation fails: the OpenBao pod is
# unreachable.
make_failing_kubectl() {
  local dir="$1" log="$2"
  mkdir -p "$dir"
  : >"$log"
  cat >"$dir/kubectl" <<STUB
#!/bin/bash
printf '%s\n' "\$*" >>"${log}"
exit 1
STUB
  chmod +x "$dir/kubectl"
}

# run_bootstrap <stub_dir> <script>
# Runs a bootstrap script with the kubectl stub prepended to PATH. Echoes the
# combined stdout/stderr; returns the script's exit code. KORC_CONTROLPLANES is
# pinned so a change to the script's default identity cannot move these tests.
run_bootstrap() {
  local stub_dir="$1" script="$2"
  (
    PATH="$stub_dir:$PATH"
    BAO_TOKEN="dummy-token"
    KORC_CONTROLPLANES="openstack/controlplane"
    export PATH BAO_TOKEN KORC_CONTROLPLANES
    bash "$script"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test: the barbican mount is created when it is absent
# ---------------------------------------------------------------------------
test_mounts_barbican_kv_when_absent() {
  echo "Test: setup-secret-engines.sh mounts barbican KV v2 when it is absent"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_kubectl "$tmp" "$tmp/calls.log" "$ENGINES_WITHOUT_BARBICAN" ""

  local output calls
  output="$(run_bootstrap "$tmp" "$ENGINES_SCRIPT")"
  calls="$(cat "$tmp/calls.log")"

  assert_contains "the barbican mount is created as KV version 2" \
    "$calls" "bao secrets enable -path=barbican -version=2 kv"
  assert_contains "the created mount is reported" \
    "$output" "KV v2 secret engine enabled at barbican/."
  assert_not_contains "an engine already in the list is not re-enabled" \
    "$calls" "bao secrets enable -path=kv-v2"
}

# ---------------------------------------------------------------------------
# Test: a fresh instance mounts barbican too
# ---------------------------------------------------------------------------
# The engine list of an uninitialized OpenBao is an empty JSON object, which
# `jq -r 'keys[]'` turns into no output at all. A guard reading that as "the
# path is there" would leave the brownfield leg without its mount.
test_mounts_barbican_kv_on_an_empty_engine_list() {
  echo "Test: setup-secret-engines.sh mounts barbican KV v2 on an empty engine list"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_kubectl "$tmp" "$tmp/calls.log" "$ENGINES_EMPTY" ""

  local exit_code calls
  run_bootstrap "$tmp" "$ENGINES_SCRIPT" >/dev/null
  exit_code=$?
  calls="$(cat "$tmp/calls.log")"

  assert_eq "the run succeeds" "0" "$exit_code"
  assert_contains "the barbican mount is created on a fresh instance" \
    "$calls" "bao secrets enable -path=barbican -version=2 kv"
  assert_contains "the other engines are created as well" \
    "$calls" "bao secrets enable -path=kv-v2 -version=2 kv"
}

# ---------------------------------------------------------------------------
# Test: an existing barbican mount is left alone
# ---------------------------------------------------------------------------
test_skips_the_barbican_mount_when_it_exists() {
  echo "Test: setup-secret-engines.sh skips the barbican mount when it exists"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_kubectl "$tmp" "$tmp/calls.log" "$ENGINES_WITH_BARBICAN" ""

  local output exit_code calls
  output="$(run_bootstrap "$tmp" "$ENGINES_SCRIPT")"
  exit_code=$?
  calls="$(cat "$tmp/calls.log")"

  assert_eq "the re-run succeeds" "0" "$exit_code"
  assert_contains "the skip is logged" \
    "$output" "already enabled at barbican/"
  assert_not_contains "the existing mount is not enabled a second time" \
    "$calls" "bao secrets enable -path=barbican"
}

# ---------------------------------------------------------------------------
# Test: the barbican approle role binds the barbican-secretstore policy alone
# ---------------------------------------------------------------------------
test_writes_the_barbican_approle_role() {
  echo "Test: setup-auth.sh writes the barbican AppRole role"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_kubectl "$tmp" "$tmp/calls.log" "$ENGINES_WITHOUT_BARBICAN" ""

  local exit_code role_call role_calls policies
  run_bootstrap "$tmp" "$AUTH_SCRIPT" >/dev/null
  exit_code=$?
  role_calls="$(grep -c "auth/approle/role/barbican" "$tmp/calls.log")"
  role_call="$(grep "auth/approle/role/barbican" "$tmp/calls.log")"
  policies="$(printf '%s\n' "$role_call" | tr ' ' '\n' | grep '^token_policies=')"

  assert_eq "the run succeeds" "0" "$exit_code"
  assert_eq "the role is written exactly once" "1" "$role_calls"
  assert_contains "the role is an upsert on the approle mount" \
    "$role_call" "bao write auth/approle/role/barbican"
  assert_eq "the role binds the barbican-secretstore policy and nothing else" \
    "token_policies=barbican-secretstore" "$policies"
  assert_contains "the issued token lives for an hour" "$role_call" "token_ttl=1h"
  assert_contains "the issued token cannot be renewed past four hours" \
    "$role_call" "token_max_ttl=4h"
  # The secret ID carries no use count and no CIDR bound, so its TTL is the only
  # thing bounding a leaked one — 30 days, a window a rotation loop can meet.
  assert_contains "the secret ID expires after 30 days" \
    "$role_call" "secret_id_ttl=720h"
}

# ---------------------------------------------------------------------------
# Test: the policy the barbican role is bound to exists and grants the mount
# ---------------------------------------------------------------------------
# setup-policies.sh names each policy after its file basename, so the role's
# token_policies=barbican-secretstore resolves to this file and nothing else.
test_barbican_secretstore_policy_backs_the_role() {
  echo "Test: the barbican-secretstore policy exists and grants the barbican/ mount"

  if [[ ! -f "$POLICY_FILE" ]]; then
    echo "  FAIL: $POLICY_FILE does not exist — the role is bound to a policy that is never written"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_file_contains "the policy grants the KV v2 data path" \
    "$POLICY_FILE" 'path "barbican/data/\*"'
  assert_file_contains "the policy grants the KV v2 metadata path" \
    "$POLICY_FILE" 'path "barbican/metadata/\*"'

  # A DELETE on metadata/ destroys every version of a secret permanently — the
  # one KV v2 operation Barbican's tenant key material has no recovery from.
  local metadata_caps
  metadata_caps="$(sed -n '/path "barbican\/metadata\/\*"/,/}/p' "$POLICY_FILE" \
    | grep 'capabilities')"
  assert_not_contains "the metadata path grants no irreversible delete" \
    "$metadata_caps" '"delete"'
}

# ---------------------------------------------------------------------------
# Test: the unseal key is seeded when it is missing
# ---------------------------------------------------------------------------
test_seeds_the_unseal_key_when_it_is_missing() {
  echo "Test: write-bootstrap-secrets.sh seeds the unseal key when it is missing"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # No path is readable, so every probe reports a miss.
  make_kubectl "$tmp" "$tmp/calls.log" "$ENGINES_WITHOUT_BARBICAN" ""

  local exit_code write_call
  run_bootstrap "$tmp" "$SEED_SCRIPT" >/dev/null
  exit_code=$?
  write_call="$(grep "bao kv put" "$tmp/calls.log" | grep "BAO_KV_PATH=$UNSEAL_PATH")"

  assert_eq "the run succeeds" "0" "$exit_code"
  assert_not_empty "the unseal key is written" "$write_call"
  assert_contains "the key is generated inside the pod, never on the host" \
    "$write_call" "sys/tools/random/32"
  assert_contains "the path travels as an environment variable" \
    "$write_call" "BAO_KV_PATH=$UNSEAL_PATH"
}

# ---------------------------------------------------------------------------
# Test: an existing unseal key is never rewritten
# ---------------------------------------------------------------------------
# A rewrite is unrecoverable: the openbao-operator has already handed the seeded
# value to the proving instance as its static seal, so a second, different key
# leaves the instance sealed for good.
test_never_rewrites_an_existing_unseal_key() {
  echo "Test: write-bootstrap-secrets.sh never rewrites an existing unseal key"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # The unseal key is readable; every other seed path still reports a miss.
  make_kubectl "$tmp" "$tmp/calls.log" "$ENGINES_WITHOUT_BARBICAN" "$UNSEAL_PATH"

  local output exit_code puts
  output="$(run_bootstrap "$tmp" "$SEED_SCRIPT")"
  exit_code=$?
  puts="$(grep "bao kv put" "$tmp/calls.log")"

  assert_eq "the re-run succeeds" "0" "$exit_code"
  assert_contains "the skip is logged for the unseal key" \
    "$output" "Secret '$UNSEAL_PATH' already exists"
  assert_not_contains "the stored key is left untouched" \
    "$puts" "BAO_KV_PATH=$UNSEAL_PATH"
  assert_not_empty "the other bootstrap secrets are still seeded" "$puts"
}

# ---------------------------------------------------------------------------
# Test: every bootstrap script refuses to run without a token
# ---------------------------------------------------------------------------
test_every_bootstrap_script_demands_a_token() {
  echo "Test: the bootstrap scripts refuse to run without BAO_TOKEN"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_failing_kubectl "$tmp" "$tmp/calls.log"

  local script name output exit_code
  for script in "$ENGINES_SCRIPT" "$AUTH_SCRIPT" "$SEED_SCRIPT"; do
    name="$(basename "$script")"
    output="$(
      PATH="$tmp:$PATH" env -u BAO_TOKEN bash "$script" 2>&1
    )"
    exit_code=$?
    assert_nonzero_exit "$name exits non-zero without a token" "$exit_code"
    assert_contains "$name names the missing token" \
      "$output" "BAO_TOKEN must be set"
  done

  assert_eq "no script reaches the OpenBao pod without a token" \
    "" "$(cat "$tmp/calls.log")"
}

# ---------------------------------------------------------------------------
# Test: an unreachable OpenBao aborts the engine setup
# ---------------------------------------------------------------------------
# The failing `bao secrets list` itself does not abort: it sits in an `if`
# condition, where set -e does not fire, and an unreadable list is
# indistinguishable from an empty one. The abort comes from the first enable
# that fails, and it must be an abort — continuing would walk through the
# remaining mounts reporting success for engines that were never created.
test_aborts_when_openbao_is_unreachable() {
  echo "Test: setup-secret-engines.sh aborts when OpenBao is unreachable"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_failing_kubectl "$tmp" "$tmp/calls.log"

  local output exit_code calls
  output="$(run_bootstrap "$tmp" "$ENGINES_SCRIPT")"
  exit_code=$?
  calls="$(cat "$tmp/calls.log")"

  assert_nonzero_exit "the script exits non-zero" "$exit_code"
  assert_not_contains "the barbican mount is not attempted after the failure" \
    "$calls" "bao secrets enable -path=barbican"
  assert_not_contains "no further mount is attempted after the failure" \
    "$calls" "bao secrets enable -path=pki"
  assert_not_contains "the run does not report success" "$output" "=== Done ==="
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_mounts_barbican_kv_when_absent
test_mounts_barbican_kv_on_an_empty_engine_list
test_skips_the_barbican_mount_when_it_exists
test_writes_the_barbican_approle_role
test_barbican_secretstore_policy_backs_the_role
test_seeds_the_unseal_key_when_it_is_missing
test_never_rewrites_an_existing_unseal_key
test_every_bootstrap_script_demands_a_token
test_aborts_when_openbao_is_unreachable

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
