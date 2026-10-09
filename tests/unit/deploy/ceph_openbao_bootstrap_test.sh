#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the OpenBao bootstrap legs of the lab Ceph's key hand-off
# (deploy/lab/metal-stack/ceph/cluster/keys-push.yaml and keys-pull.yaml).
#
#   - setup-auth.sh must write BOTH kubernetes/management roles once each:
#     push-ceph-keys bound to the ServiceAccount ceph-keys-push in rook-ceph,
#     and read-ceph-keys bound to ceph-keys-read in openstack. Each binds one
#     namespace and never "*": the Ceph paths are not namespace-templated, so
#     a wildcard would hand every namespace every Ceph key.
#   - each role must bind the policy of its own name and nothing else, and
#     setup-policies.sh names each policy after its file's basename, so the
#     file must exist.
#   - read-ceph-keys.hcl must grant read and nothing more, and only below
#     kv-v2/data/ceph/.
#   - push-ceph-keys.hcl must grant the KV v2 metadata of ceph/ beside its
#     data: ESO writes the custom metadata of every secret it pushes, and the
#     push fails with 403 without it.
#
# setup-auth.sh is driven with a recording kubectl stub, so nothing touches a
# cluster or an OpenBao. No CI job authenticates with these roles: preflight
# refuses WITH_CEPH=true in kind mode.
#
# Usage: bash tests/unit/deploy/ceph_openbao_bootstrap_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
AUTH_SCRIPT="$PROJECT_ROOT/deploy/openbao/bootstrap/setup-auth.sh"
POLICY_DIR="$PROJECT_ROOT/deploy/openbao/policies"
READ_POLICY="$POLICY_DIR/read-ceph-keys.hcl"
PUSH_POLICY="$POLICY_DIR/push-ceph-keys.hcl"

# A typical engine map of an already-bootstrapped instance.
ENGINES='{"kv-v2/":{},"pki/":{},"database/mariadb/":{}}'

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# make_kubectl <dir> <log>
# Installs a recording kubectl stub in <dir>. Every invocation's argument string
# lands in <log>. `bao secrets list` answers the engine map and `bao auth list`
# an empty map, so setup-auth.sh enables its mounts; everything else is
# recorded and answered with exit 0.
make_kubectl() {
  local dir="$1" log="$2"
  mkdir -p "$dir"
  : >"$log"
  cat >"$dir/kubectl" <<STUB
#!/bin/bash
printf '%s\n' "\$*" >>"${log}"

case "\$*" in
  *"bao secrets list"*)
    printf '%s' '${ENGINES}'
    ;;
  *"bao auth list"*)
    printf '%s' '{}'
    ;;
esac
exit 0
STUB
  chmod +x "$dir/kubectl"
}

# run_bootstrap <stub_dir>
# Runs setup-auth.sh with the kubectl stub prepended to PATH. Echoes the
# combined stdout/stderr; returns the script's exit code.
run_bootstrap() {
  local stub_dir="$1"
  (
    PATH="$stub_dir:$PATH"
    BAO_TOKEN="dummy-token"
    KORC_CONTROLPLANES="openstack/controlplane"
    export PATH BAO_TOKEN KORC_CONTROLPLANES
    bash "$AUTH_SCRIPT"
  ) 2>&1
}

# role_arg <role call> <name>
# Prints the value of the argument <name>=... of a recorded role write.
role_arg() {
  tr ' ' '\n' <<<"$1" | sed -n "s/^$2=//p"
}

# ---------------------------------------------------------------------------
# Test: both Ceph key roles are written, each bound to one ServiceAccount in
# one namespace
# ---------------------------------------------------------------------------
test_writes_both_ceph_key_roles() {
  echo "Test: setup-auth.sh writes the push-ceph-keys and read-ceph-keys roles"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_kubectl "$tmp" "$tmp/calls.log"

  local exit_code
  run_bootstrap "$tmp" >/dev/null
  exit_code=$?
  assert_eq "the run succeeds" "0" "$exit_code"

  local pair role sa ns role_call
  for pair in push-ceph-keys:ceph-keys-push:rook-ceph read-ceph-keys:ceph-keys-read:openstack; do
    IFS=: read -r role sa ns <<<"$pair"
    role_call="$(grep "auth/kubernetes/management/role/${role} " "$tmp/calls.log")"

    assert_eq "the ${role} role is written exactly once" "1" "$(grep -c . <<<"$role_call")"
    assert_contains "the ${role} role is an upsert on the management mount" \
      "$role_call" "bao write auth/kubernetes/management/role/${role} "
    assert_eq "the ${role} role binds the ServiceAccount ${sa} alone" \
      "$sa" "$(role_arg "$role_call" bound_service_account_names)"
    # Never "*": the Ceph paths are not namespace-templated.
    assert_eq "the ${role} role binds the namespace ${ns} alone" \
      "$ns" "$(role_arg "$role_call" bound_service_account_namespaces)"
    assert_eq "the ${role} role binds the policy of its own name and nothing else" \
      "$role" "$(role_arg "$role_call" token_policies)"
    assert_eq "the ${role} token lives for an hour" "1h" "$(role_arg "$role_call" token_ttl)"
    assert_eq "the ${role} token cannot be renewed past four hours" "4h" \
      "$(role_arg "$role_call" token_max_ttl)"
  done
}

# ---------------------------------------------------------------------------
# Test: the policies behind the two roles
# ---------------------------------------------------------------------------
# setup-policies.sh names each policy after its file's basename, so
# token_policies=<role> resolves to <role>.hcl and nothing else.
test_ceph_policies_back_the_roles() {
  echo "Test: read-ceph-keys.hcl reads below ceph/ alone and push-ceph-keys.hcl covers the KV metadata"

  local policy
  for policy in "$READ_POLICY" "$PUSH_POLICY"; do
    if [[ ! -f "$policy" ]]; then
      echo "  FAIL: $policy does not exist, so its role is bound to a policy that is never written"
      FAIL=$((FAIL + 1))
      return
    fi
  done

  assert_eq "read-ceph-keys grants paths below kv-v2/data/ceph/ alone" "" \
    "$(grep '^path ' "$READ_POLICY" | grep -v '^path "kv-v2/data/ceph/' || true)"
  assert_eq "read-ceph-keys grants read and nothing more" 'capabilities = ["read"]' \
    "$(grep 'capabilities' "$READ_POLICY" | sed 's/^[[:space:]]*//' | sort -u)"
  assert_file_contains "push-ceph-keys writes the KV data of ceph/" \
    "$PUSH_POLICY" 'path "kv-v2/data/ceph/\*"'
  assert_file_contains "push-ceph-keys writes the KV metadata of ceph/, which ESO sets on every push" \
    "$PUSH_POLICY" 'path "kv-v2/metadata/ceph/\*"'
  assert_eq "push-ceph-keys never deletes" "" "$(grep -E 'capabilities.*"(delete|sudo)"' "$PUSH_POLICY" || true)"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_writes_both_ceph_key_roles
test_ceph_policies_back_the_roles

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
