#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the eso-tenant policy grants the backup path of RabbitMQVhost orders,
# and nothing more under openstack/rabbitmq.
#
#   - The backup PushSecret of an order lives in the ControlPlane's namespace and
#     pushes openstack/rabbitmq/{ns}/<name>-<hash>-vhost/credentials through that
#     namespace's own store, which authenticates as eso-tenant. Without the data
#     grant the push fails with 403 and the order holds on BackupNotSynced.
#   - The PushSecret runs DeletionPolicy=Delete, so the data path carries
#     delete. The metadata path carries create/update/read and no delete, the
#     asymmetry the service-account pair of the same file keeps.
#   - Every path under openstack/rabbitmq is scoped by the ACL identity template
#     to the caller's own namespace. A path without it would let one tenant's
#     store write another tenant's broker credentials.
#
# The test reads the policy file only; nothing touches a cluster or an OpenBao.
#
# Usage: bash tests/unit/deploy/eso_tenant_rabbitmq_policy_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
ESO_TENANT_POLICY="$PROJECT_ROOT/deploy/openbao/policies/eso-tenant.hcl"

# The ACL template every per-tenant policy path is scoped by.
NS_TEMPLATE='{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}'

DATA_PATH="kv-v2/data/openstack/rabbitmq/${NS_TEMPLATE}/+/credentials"
METADATA_PATH="kv-v2/metadata/openstack/rabbitmq/${NS_TEMPLATE}/+/credentials"

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

# rabbitmq_paths prints every path the policy grants under openstack/rabbitmq.
rabbitmq_paths() {
  grep -E '^path "kv-v2/(data|metadata)/openstack/rabbitmq/' "$ESO_TENANT_POLICY" \
    | sed -E 's/^path "([^"]+)".*/\1/'
}

# ---------------------------------------------------------------------------
# Test: the backup data path carries the write verbs and delete
# ---------------------------------------------------------------------------
test_data_path_is_granted() {
  echo "Test: eso-tenant.hcl grants the RabbitMQVhost backup data path"

  assert_eq "the data path carries create, update, read and delete" \
    '"create", "update", "read", "delete"' "$(capabilities_of "$DATA_PATH")"
}

# ---------------------------------------------------------------------------
# Test: the metadata twin carries no delete
# ---------------------------------------------------------------------------
test_metadata_path_is_granted() {
  echo "Test: eso-tenant.hcl grants the RabbitMQVhost backup metadata path"

  assert_eq "the metadata path carries create, update and read" \
    '"create", "update", "read"' "$(capabilities_of "$METADATA_PATH")"
}

# ---------------------------------------------------------------------------
# Test: every openstack/rabbitmq path is namespace-templated
# ---------------------------------------------------------------------------
# The template is what keeps one tenant's store out of another tenant's broker
# credentials, so a path without it is a cross-tenant grant.
test_every_rabbitmq_path_is_namespace_scoped() {
  echo "Test: every openstack/rabbitmq path of eso-tenant.hcl is scoped to the caller's namespace"

  local paths
  paths="$(rabbitmq_paths)"
  assert_eq "the policy grants exactly two openstack/rabbitmq paths" \
    "2" "$(printf '%s\n' "$paths" | grep -c .)"

  local path unscoped=""
  while IFS= read -r path; do
    [ -z "$path" ] && continue
    case "$path" in
      */openstack/rabbitmq/"${NS_TEMPLATE}"/*) ;;
      *) unscoped="${unscoped}${path} " ;;
    esac
  done <<<"$paths"
  assert_eq "no openstack/rabbitmq path lacks the namespace template" "" "$unscoped"
}

# ---------------------------------------------------------------------------
# Test: the helper answers nothing for a path the policy does not grant
# ---------------------------------------------------------------------------
# Guards the parser itself: a helper that echoed a capability list for any path
# would let the two grant tests pass on a policy without the stanzas.
test_ungranted_path_reads_empty() {
  echo "Test: a path eso-tenant.hcl does not grant reads as no capabilities"

  assert_eq "an unscoped rabbitmq path is not granted" \
    "" "$(capabilities_of "kv-v2/data/openstack/rabbitmq/+/+/credentials")"
}

test_data_path_is_granted
test_metadata_path_is_granted
test_every_rabbitmq_path_is_namespace_scoped
test_ungranted_path_reads_empty

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
