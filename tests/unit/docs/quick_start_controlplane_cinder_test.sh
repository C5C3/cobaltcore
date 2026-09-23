#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the optional block-storage path in docs/quick-start-controlplane.md:
#   - the `### Create a first volume` check exists in Step 6
#   - the Step 6 chain runs NeutronReady -> CinderReady -> NovaReady
#   - Step 4 carries the `::: details` container for the optional block
#   - Step 2 documents the WITH_NFS=true bring-up
#   - the `# block-storage.yaml` fragment names the two kind NFS exports and
#     the gateway hostname the guides under docs/guides/cinder/ rely on
#
# The YAML assertions run through `yq`, so they stay structural. Without `yq`
# on PATH they are skipped and the textual assertions still run.
#
# Usage: bash tests/unit/docs/quick_start_controlplane_cinder_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

QUICK_START_DOC="${QUICK_START_DOC:-$PROJECT_ROOT/docs/quick-start-controlplane.md}"

if [[ ! -f "$QUICK_START_DOC" ]]; then
  echo "FAIL: $QUICK_START_DOC does not exist"
  exit 1
fi

# --- Test 1: the Step 7 walkthrough heading ---
test_volume_heading() {
  echo "Test: Step 7 carries the '### Create a first volume' check"
  assert_file_contains "'### Create a first volume' heading present" \
    "$QUICK_START_DOC" \
    '^### Create a first volume$'
}

# --- Test 2: the Step 6 condition chain ---
test_condition_chain() {
  echo "Test: the chain runs NeutronReady -> CinderReady -> NovaReady"
  assert_file_contains "CinderReady sits between NeutronReady and NovaReady" \
    "$QUICK_START_DOC" \
    'NeutronReady → CinderReady → NovaReady'
}

# --- Test 3: the Step 4 details container ---
test_details_container() {
  echo "Test: Step 4 opens the optional block-storage container"
  assert_file_contains "'::: details Optional: block storage' container present" \
    "$QUICK_START_DOC" \
    '^::: details Optional: block storage (needs WITH_NFS=true in Step 2)$'
}

# --- Test 4: the Step 2 opt-in bring-up ---
test_with_nfs_bring_up() {
  echo "Test: Step 2 documents the WITH_NFS bring-up"
  assert_file_contains "WITH_NFS=true make deploy-infra is documented" \
    "$QUICK_START_DOC" \
    'WITH_NFS=true make deploy-infra'
}

test_volume_heading
test_condition_chain
test_details_container
test_with_nfs_bring_up

# Extract the first fenced yaml block that begins with the `# block-storage.yaml`
# filename marker (the optional Step 4 fragment).
BLOCK_STORAGE_YAML="$(mktemp)"
trap 'rm -f "$BLOCK_STORAGE_YAML"' EXIT

awk '
  BEGIN { in_block = 0; found = 0 }
  /^```yaml[[:space:]]*$/ {
    # Peek the next line; only enter the block if it is "# block-storage.yaml".
    if ((getline next_line) > 0) {
      if (next_line ~ /^# block-storage\.yaml[[:space:]]*$/) {
        in_block = 1
        found = 1
        next
      } else {
        # not our block — discard and continue
        next
      }
    }
    next
  }
  in_block && /^```[[:space:]]*$/ { in_block = 0; exit }
  in_block { print }
  END { if (!found) exit 1 }
' "$QUICK_START_DOC" > "$BLOCK_STORAGE_YAML" || {
  echo "FAIL: could not locate the block-storage yaml block (looking for the \`# block-storage.yaml\` marker)"
  exit 1
}

if [[ ! -s "$BLOCK_STORAGE_YAML" ]]; then
  echo "FAIL: extracted block-storage yaml block is empty"
  exit 1
fi

# --- Tests 5-8: the fragment's backend, backup and gateway values ---
assert_yaml_value() {
  local description="$1" expression="$2" expected="$3"
  echo "Test: $expression == $expected"
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq is not installed, skipping the YAML assertions"
    SKIP=$((SKIP + 1))
    return 0
  fi
  local actual
  actual="$(yq -r "$expression" "$BLOCK_STORAGE_YAML" 2>/dev/null)"
  assert_eq "$description" "$expected" "$actual"
}

assert_yaml_value "the volume backend's NFS server" \
  '.cinder.backends[0].nfs.server' 'nfs-server.openstack.svc.cluster.local'
assert_yaml_value "the volume backend's export path" \
  '.cinder.backends[0].nfs.path' '/volumes'
assert_yaml_value "the backup backend's export path" \
  '.cinder.backupBackend.nfs.path' '/backups'
assert_yaml_value "the Cinder gateway hostname" \
  '.cinder.gateway.hostname' 'cinder.127-0-0-1.nip.io'

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
