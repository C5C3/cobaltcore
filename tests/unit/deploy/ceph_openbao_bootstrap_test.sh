#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the OpenBao bootstrap legs of the Ceph key hand-off
# (deploy/overlays/metal-stack/ceph/cluster/keys-push.yaml and keys-pull.yaml).
#
#   - setup-auth.sh must write each of the four Ceph key roles of CEPH_ROLES
#     once. push-ceph-keys and read-ceph-keys on kubernetes/management carry
#     the lab, whose Ceph runs in the management cluster. push-ceph-keys on
#     kubernetes/storage and read-ceph-client-cinder on kubernetes/hypervisor
#     serve a separate storage and a separate hypervisor cluster. Each role
#     binds one ServiceAccount in one namespace and never "*": the Ceph paths
#     are not namespace-templated, so a wildcard would hand every namespace
#     every Ceph key its policy grants.
#   - setup-auth.sh enables a mount before it writes a role on it, a re-run
#     against a bootstrapped OpenBao writes the four roles and enables no
#     mount, and a refused write of a role stops the script before the next.
#   - each role must bind the policy it names and nothing else, and
#     setup-policies.sh names each policy after its file's basename, so the
#     file must exist and policies/kustomization.yaml must list it.
#   - read-ceph-keys.hcl must grant read and nothing more, and only below
#     kv-v2/data/ceph/.
#   - read-ceph-client-cinder.hcl must grant read on
#     kv-v2/data/ceph/client-cinder alone: libvirt needs the cinder key, and a
#     hypervisor never uses the key of cinder-backup.
#   - eso-storage.hcl and eso-hypervisor.hcl, the policies of the shared ESO
#     identity of the storage and the hypervisor cluster, must grant read and
#     nothing more. eso-storage reads below kv-v2/data/ceph/ and holds no KV
#     metadata path, so a push as eso-storage fails and goes through
#     push-ceph-keys; eso-hypervisor reads the Nova compute configuration
#     alone. The eso-storage and eso-hypervisor roles bind their own policy
#     and nothing else, and no role but the two push-ceph-keys roles binds
#     push-ceph-keys.
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
AUTH_SCRIPT="$PROJECT_ROOT/deploy/bases/shared-openbao/bootstrap/setup-auth.sh"
POLICY_DIR="$PROJECT_ROOT/deploy/bases/shared-openbao/policies"
READ_POLICY="$POLICY_DIR/read-ceph-keys.hcl"
PUSH_POLICY="$POLICY_DIR/push-ceph-keys.hcl"
CINDER_POLICY="$POLICY_DIR/read-ceph-client-cinder.hcl"
ESO_STORAGE_POLICY="$POLICY_DIR/eso-storage.hcl"
ESO_HYPERVISOR_POLICY="$POLICY_DIR/eso-hypervisor.hcl"

# The four Ceph key roles of setup-auth.sh, one per entry as
# <mount>:<role>:<ServiceAccount>:<namespace>:<policy>.
CEPH_ROLES=(
  management:push-ceph-keys:ceph-keys-push:rook-ceph:push-ceph-keys
  management:read-ceph-keys:ceph-keys-read:openstack:read-ceph-keys
  storage:push-ceph-keys:ceph-keys-push:rook-ceph:push-ceph-keys
  hypervisor:read-ceph-client-cinder:ceph-keys-read:openstack:read-ceph-client-cinder
)

# A typical engine map of an already-bootstrapped instance.
ENGINES='{"kv-v2/":{},"pki/":{},"database/mariadb/":{}}'

# The auth map of an instance setup-auth.sh has already run against, the same
# map as in tests/unit/deploy/openbao_mount_guard_test.sh.
AUTH_BOOTSTRAPPED='{"approle/":{},"kubernetes/control-plane/":{},"kubernetes/hypervisor/":{},"kubernetes/management/":{},"kubernetes/storage/":{},"token/":{}}'

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# make_kubectl <dir> <log> [<auth-json>] [<fail-pattern>]
# Installs a recording kubectl stub in <dir>. Every invocation's argument string
# lands in <log>. `bao secrets list` answers the engine map and `bao auth list`
# <auth-json>, an empty map by default, so setup-auth.sh enables its mounts. A
# call whose argument string contains <fail-pattern> is recorded, then prints
# OpenBao's 403 on stderr and exits 1; the default empty pattern fails nothing.
# Everything else is recorded and answered with exit 0.
make_kubectl() {
  local dir="$1" log="$2" auth_json="${3-}" fail_pattern="${4-}"
  [[ -n "$auth_json" ]] || auth_json='{}'
  mkdir -p "$dir"
  : >"$log"
  cat >"$dir/kubectl" <<STUB
#!/bin/bash
printf '%s\n' "\$*" >>"${log}"

fail='${fail_pattern}'
if [[ -n "\$fail" && "\$*" == *"\$fail"* ]]; then
  printf 'Error writing data: Error making API request.\n\nCode: 403. Errors:\n\n* permission denied\n' >&2
  exit 1
fi

case "\$*" in
  *"bao secrets list"*)
    printf '%s' '${ENGINES}'
    ;;
  *"bao auth list"*)
    printf '%s' '${auth_json}'
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
# Test: the four Ceph key roles are written, each bound to one ServiceAccount
# in one namespace
# ---------------------------------------------------------------------------
test_writes_the_ceph_key_roles() {
  echo "Test: setup-auth.sh writes the four Ceph key roles"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_kubectl "$tmp" "$tmp/calls.log"

  local output exit_code
  output="$(run_bootstrap "$tmp")"
  exit_code=$?
  assert_eq "the run succeeds" "0" "$exit_code"

  local row mount role sa ns policy role_call
  for row in "${CEPH_ROLES[@]}"; do
    IFS=: read -r mount role sa ns policy <<<"$row"
    role_call="$(grep "auth/kubernetes/${mount}/role/${role} " "$tmp/calls.log")"

    assert_eq "the ${role} role on kubernetes/${mount} is written exactly once" "1" \
      "$(grep -c . <<<"$role_call")"
    assert_contains "the ${role} role on kubernetes/${mount} is an upsert" \
      "$role_call" "bao write auth/kubernetes/${mount}/role/${role} "
    assert_eq "the ${role} role on kubernetes/${mount} binds the ServiceAccount ${sa} alone" \
      "$sa" "$(role_arg "$role_call" bound_service_account_names)"
    # Never "*": the Ceph paths are not namespace-templated.
    assert_eq "the ${role} role on kubernetes/${mount} binds the namespace ${ns} alone" \
      "$ns" "$(role_arg "$role_call" bound_service_account_namespaces)"
    assert_eq "the ${role} role on kubernetes/${mount} binds the policy ${policy} and nothing else" \
      "$policy" "$(role_arg "$role_call" token_policies)"
    assert_eq "the ${role} token on kubernetes/${mount} lives for an hour" "1h" \
      "$(role_arg "$role_call" token_ttl)"
    assert_eq "the ${role} token on kubernetes/${mount} cannot be renewed past four hours" "4h" \
      "$(role_arg "$role_call" token_max_ttl)"
  done

  assert_eq "the call log holds four Ceph key role writes and no other" "4" \
    "$(grep -cE 'auth/kubernetes/[a-z-]+/role/(push|read)-ceph' "$tmp/calls.log")"

  # A token gets the grants of the policies its role binds, so the shared ESO
  # identity of the storage and the hypervisor cluster binds its own read-only
  # policy alone, and only the two push-ceph-keys roles bind the Ceph write
  # policy.
  local cluster eso_call
  for cluster in storage hypervisor; do
    eso_call="$(grep "auth/kubernetes/${cluster}/role/eso-${cluster} " "$tmp/calls.log")"
    assert_eq "the eso-${cluster} role binds the policy eso-${cluster} and nothing else" \
      "eso-${cluster}" "$(role_arg "$eso_call" token_policies)"
  done
  assert_eq "only the two push-ceph-keys roles bind the push-ceph-keys policy" "2" \
    "$(grep -cE 'token_policies=([^ ]*,)?push-ceph-keys(,| |$)' "$tmp/calls.log")"

  # The storage and hypervisor mounts carry no role until setup-auth.sh has
  # enabled them in the same run.
  local pair enabled written
  for pair in storage:push-ceph-keys hypervisor:read-ceph-client-cinder; do
    IFS=: read -r mount role <<<"$pair"
    enabled="$(grep -m1 -n -F -- "bao auth enable -path=kubernetes/${mount} kubernetes" "$tmp/calls.log" | cut -d: -f1)"
    written="$(grep -m1 -n -F -- "bao write auth/kubernetes/${mount}/role/${role} " "$tmp/calls.log" | cut -d: -f1)"
    assert_eq "kubernetes/${mount} is enabled before its ${role} role is written" "yes" \
      "$([[ -n "$enabled" && -n "$written" && "$enabled" -lt "$written" ]] && echo yes || echo no)"
  done

  assert_contains "the run reports the push-ceph-keys role on kubernetes/storage" \
    "$output" "push-ceph-keys role on kubernetes/storage written."
  assert_contains "the run reports the read-ceph-client-cinder role on kubernetes/hypervisor" \
    "$output" "read-ceph-client-cinder role on kubernetes/hypervisor written."
}

# ---------------------------------------------------------------------------
# Test: a re-run against a bootstrapped OpenBao writes the four roles again
# and enables no mount
# ---------------------------------------------------------------------------
test_rerun_writes_the_roles_without_enabling_a_mount() {
  echo "Test: a re-run against a bootstrapped OpenBao writes the four Ceph key roles"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_kubectl "$tmp" "$tmp/calls.log" "$AUTH_BOOTSTRAPPED"

  local exit_code
  run_bootstrap "$tmp" >/dev/null
  exit_code=$?
  assert_eq "the re-run succeeds" "0" "$exit_code"
  assert_file_not_contains "the re-run enables no auth mount" "$tmp/calls.log" "bao auth enable"

  local row mount role
  for row in "${CEPH_ROLES[@]}"; do
    IFS=: read -r mount role _ <<<"$row"
    assert_eq "the re-run writes the ${role} role on kubernetes/${mount} once" "1" \
      "$(grep -c "auth/kubernetes/${mount}/role/${role} " "$tmp/calls.log")"
  done
}

# ---------------------------------------------------------------------------
# Test: a refused write of the storage push role stops the bootstrap
# ---------------------------------------------------------------------------
test_a_failed_storage_role_write_stops_the_bootstrap() {
  echo "Test: a refused write of the push-ceph-keys role on kubernetes/storage stops setup-auth.sh"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_kubectl "$tmp" "$tmp/calls.log" '{}' 'auth/kubernetes/storage/role/push-ceph-keys'

  local output exit_code
  output="$(run_bootstrap "$tmp")"
  exit_code=$?
  # set -e ends setup-auth.sh with kubectl's exit code.
  assert_eq "the run fails with kubectl's exit code" "1" "$exit_code"
  assert_contains "the run shows OpenBao's refusal" "$output" "permission denied"
  assert_not_contains "the run does not report the refused role as written" \
    "$output" "push-ceph-keys role on kubernetes/storage written."
  assert_file_contains_fixed "the refused call is the write of the storage push role" \
    "$tmp/calls.log" "bao write auth/kubernetes/storage/role/push-ceph-keys "
  assert_file_not_contains "the run stops before the read-ceph-client-cinder role on kubernetes/hypervisor" \
    "$tmp/calls.log" "auth/kubernetes/hypervisor/role/read-ceph-client-cinder"
}

# ---------------------------------------------------------------------------
# Test: the policies behind the roles
# ---------------------------------------------------------------------------
# setup-policies.sh names each policy after its file's basename, so
# token_policies=<policy> resolves to <policy>.hcl and nothing else.
test_ceph_policies_back_the_roles() {
  echo "Test: the Ceph key policies grant what their roles need and no more"

  local policy
  for policy in "$READ_POLICY" "$PUSH_POLICY" "$CINDER_POLICY" \
    "$ESO_STORAGE_POLICY" "$ESO_HYPERVISOR_POLICY"; do
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

  assert_eq "read-ceph-client-cinder grants the cinder client key alone" \
    'path "kv-v2/data/ceph/client-cinder" {' "$(grep '^path ' "$CINDER_POLICY")"
  assert_eq "read-ceph-client-cinder grants read and nothing more" 'capabilities = ["read"]' \
    "$(grep '^[[:space:]]*capabilities' "$CINDER_POLICY" | sed 's/^[[:space:]]*//' | sort -u)"

  assert_eq "eso-storage grants paths below kv-v2/data/ceph/ alone" "" \
    "$(grep '^path ' "$ESO_STORAGE_POLICY" | grep -v '^path "kv-v2/data/ceph/' || true)"
  assert_eq "eso-storage grants read and nothing more" 'capabilities = ["read"]' \
    "$(grep '^[[:space:]]*capabilities' "$ESO_STORAGE_POLICY" | sed 's/^[[:space:]]*//' | sort -u)"
  assert_file_not_contains "eso-storage holds no KV metadata path, so no PushSecret goes through it" \
    "$ESO_STORAGE_POLICY" 'kv-v2/metadata'
  assert_eq "eso-hypervisor reads the Nova compute configuration alone" \
    'path "kv-v2/data/openstack/nova/compute-*" {' "$(grep '^path ' "$ESO_HYPERVISOR_POLICY")"
  assert_eq "eso-hypervisor grants read and nothing more" 'capabilities = ["read"]' \
    "$(grep '^[[:space:]]*capabilities' "$ESO_HYPERVISOR_POLICY" | sed 's/^[[:space:]]*//' | sort -u)"

  # The configMapGenerator of policies/kustomization.yaml ships a file to
  # setup-policies.sh only when its files list names it.
  local on_disk listed
  on_disk="$(cd "$POLICY_DIR" && printf '%s\n' *.hcl | LC_ALL=C sort)"
  listed="$(sed -n 's/^[[:space:]]*-[[:space:]]*\([^[:space:]]*\.hcl\)[[:space:]]*$/\1/p' \
    "$POLICY_DIR/kustomization.yaml" | LC_ALL=C sort)"
  assert_eq "every policy file is in policies/kustomization.yaml and nothing else is" \
    "$on_disk" "$listed"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_writes_the_ceph_key_roles
test_ceph_policies_back_the_roles
test_rerun_writes_the_roles_without_enabling_a_mount
test_a_failed_storage_role_write_stops_the_bootstrap

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
