#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify that every eso-tenant data path that carries delete has a metadata twin
# that carries delete too, and that every metadata grant with delete stays in the
# caller's own namespace.
#
#   - ESO's DeletionPolicy=Delete soft-deletes the data path and then deletes
#     the metadata path. Without delete on the metadata path the second request
#     gets 403 and the key stays in OpenBao with its versions and its
#     custom_metadata (#1860). The admin application credential, the
#     service-account backups and the RabbitMQVhost backup all run that policy.
#   - The scan is pinned to EXPECTED_DELETE_PATHS data paths with delete, so a
#     parser that finds nothing cannot pass the pairing check on an empty list.
#   - delete on a metadata path destroys every version of the key. A metadata
#     path without the ACL identity template would let one tenant's store
#     delete another tenant's keys.
#
# The test reads the policy file only; nothing touches a cluster or an OpenBao.
#
# Usage: bash tests/unit/deploy/eso_tenant_metadata_delete_policy_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
ESO_TENANT_POLICY="$PROJECT_ROOT/deploy/bases/shared-openbao/policies/eso-tenant.hcl"

# The ACL template every per-tenant policy path is scoped by.
NS_TEMPLATE='{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}'

# The data paths that carry delete: fernet-keys, credential-keys, bootstrap
# admin, admin app-credential, service-accounts, rabbitmq credentials.
EXPECTED_DELETE_PATHS=6

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# capabilities_of <path> prints the capabilities list of the stanza that grants
# exactly <path>, as written between the brackets, or nothing when no stanza
# grants it.
capabilities_of() {
  awk -v want="path \"$1\" {" '
    $0 == want { inside = 1; next }
    inside && /capabilities/ {
      sub(/.*\[/, ""); sub(/\].*/, ""); print; exit
    }
    inside && /^}/ { exit }
  ' "$ESO_TENANT_POLICY"
}

# stanzas prints one "<path><TAB><capabilities>" line per path stanza of the
# policy, the capabilities as written between the brackets.
stanzas() {
  awk '
    /^path "/ {
      path = $0; sub(/^path "/, "", path); sub(/" [{]$/, "", path)
      caps = ""; inside = 1; next
    }
    inside && caps == "" && /capabilities/ {
      caps = $0; sub(/.*\[/, "", caps); sub(/\].*/, "", caps)
    }
    inside && /^}/ { print path "\t" caps; inside = 0 }
  ' "$ESO_TENANT_POLICY"
}

# delete_paths <data|metadata> prints every kv-v2/<data|metadata>/ path whose
# capabilities contain "delete".
delete_paths() {
  stanzas | awk -F'\t' -v prefix="kv-v2/$1/" \
    'index($1, prefix) == 1 && $2 ~ /"delete"/ { print $1 }'
}

# ---------------------------------------------------------------------------
# Test: the three backup metadata paths carry delete
# ---------------------------------------------------------------------------
test_named_metadata_paths_grant_delete() {
  echo "Test: eso-tenant.hcl grants delete on the metadata paths of the three backup kinds"

  assert_eq "the admin application credential metadata path carries create, update, read and delete" \
    '"create", "update", "read", "delete"' \
    "$(capabilities_of "kv-v2/metadata/openstack/keystone/${NS_TEMPLATE}/+/admin/app-credential")"
  assert_eq "the service-account metadata path carries create, update, read and delete" \
    '"create", "update", "read", "delete"' \
    "$(capabilities_of "kv-v2/metadata/openstack/keystone/${NS_TEMPLATE}/+/service-accounts/+")"
  assert_eq "the RabbitMQ credentials metadata path carries create, update, read and delete" \
    '"create", "update", "read", "delete"' \
    "$(capabilities_of "kv-v2/metadata/openstack/rabbitmq/${NS_TEMPLATE}/+/credentials")"
}

# ---------------------------------------------------------------------------
# Test: every data path with delete has a metadata twin with delete
# ---------------------------------------------------------------------------
# ESO deletes the metadata path right after the data path, so a data grant with
# delete and a metadata twin without it leaves a soft-deleted key behind.
test_every_delete_on_data_has_delete_on_metadata() {
  echo "Test: every data path of eso-tenant.hcl that carries delete has a metadata twin that carries delete"

  local path twin caps offending=""
  while IFS= read -r path; do
    [ -z "$path" ] && continue
    twin="kv-v2/metadata/${path#kv-v2/data/}"
    caps="$(capabilities_of "$twin")"
    if [ -z "$caps" ]; then
      offending="${offending}${path} (no metadata twin) "
      continue
    fi
    case "$caps" in
      *'"delete"'*) ;;
      *) offending="${offending}${path} (metadata twin without delete) " ;;
    esac
  done <<<"$(delete_paths data)"
  assert_eq "no data path with delete lacks delete on its metadata twin" "" "$offending"
}

# ---------------------------------------------------------------------------
# Test: the scan finds every data path with delete
# ---------------------------------------------------------------------------
# Guards the pairing check: a scan that matched nothing would let it pass on an
# empty list.
test_scan_finds_every_delete_path() {
  echo "Test: the scan finds the ${EXPECTED_DELETE_PATHS} data paths of eso-tenant.hcl that carry delete"

  assert_eq "the scan finds every data path with delete" \
    "${EXPECTED_DELETE_PATHS}" "$(delete_paths data | grep -c .)"
}

# ---------------------------------------------------------------------------
# Test: every metadata path with delete is namespace-templated
# ---------------------------------------------------------------------------
# delete on a metadata path removes every version of the key, so a path without
# the template would let one tenant's store destroy another tenant's keys.
test_every_metadata_delete_is_namespace_scoped() {
  echo "Test: every metadata path of eso-tenant.hcl that carries delete is scoped to the caller's namespace"

  local paths
  paths="$(delete_paths metadata)"
  assert_eq "the policy grants delete on exactly ${EXPECTED_DELETE_PATHS} metadata paths" \
    "${EXPECTED_DELETE_PATHS}" "$(printf '%s\n' "$paths" | grep -c .)"

  local path unscoped=""
  while IFS= read -r path; do
    [ -z "$path" ] && continue
    case "$path" in
      */"${NS_TEMPLATE}"/*) ;;
      *) unscoped="${unscoped}${path} " ;;
    esac
  done <<<"$paths"
  assert_eq "no metadata path with delete lacks the namespace template" "" "$unscoped"
}

# ---------------------------------------------------------------------------
# Test: the helper answers nothing for a path the policy does not grant
# ---------------------------------------------------------------------------
# Guards the parser itself: a helper that echoed a capability list for any path
# would let the grant and pairing tests pass on a policy without the stanzas.
test_ungranted_path_reads_empty() {
  echo "Test: a path eso-tenant.hcl does not grant reads as no capabilities"

  assert_eq "an unscoped service-account metadata path is not granted" \
    "" "$(capabilities_of "kv-v2/metadata/openstack/keystone/+/+/service-accounts/+")"
}

test_named_metadata_paths_grant_delete
test_every_delete_on_data_has_delete_on_metadata
test_scan_finds_every_delete_path
test_every_metadata_delete_is_namespace_scoped
test_ungranted_path_reads_empty

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
