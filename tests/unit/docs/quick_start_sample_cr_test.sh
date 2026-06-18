#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the Step 7 sample Keystone CR in docs/quick-start.md carries the
# `spec.gateway` block introduced by (/):
#   - spec.gateway.parentRef.name == "openstack-gw"
#   - spec.gateway.hostname       == "keystone.127-0-0-1.nip.io"
#   - spec.gateway.path           == "/"
#
# The test extracts the first ```yaml block immediately following the
# `# keystone.yaml` filename marker and pipes it through `yq` so the
# assertion remains structural (not a brittle regex).
#
# Usage: bash tests/unit/docs/quick_start_sample_cr_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

QUICK_START="$PROJECT_ROOT/docs/quick-start.md"

if [[ ! -f "$QUICK_START" ]]; then
  echo "FAIL: $QUICK_START does not exist"
  exit 1
fi

if ! command -v yq >/dev/null 2>&1; then
  echo "SKIP: yq is not installed; skipping Step 7 sample-CR structural checks"
  exit 0
fi

# Extract the first fenced yaml block that begins with the `# keystone.yaml`
# filename marker (the Step 7 sample CR).
SAMPLE_YAML="$(mktemp)"
trap 'rm -f "$SAMPLE_YAML"' EXIT

awk '
  BEGIN { in_block = 0; found = 0 }
  /^```yaml[[:space:]]*$/ {
    # Peek the next line; only enter the block if it is "# keystone.yaml".
    if ((getline next_line) > 0) {
      if (next_line ~ /^# keystone\.yaml[[:space:]]*$/) {
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
' "$QUICK_START" > "$SAMPLE_YAML" || {
  echo "FAIL: could not locate the Step 7 sample CR yaml block (looking for the \`# keystone.yaml\` marker)"
  exit 1
}

if [[ ! -s "$SAMPLE_YAML" ]]; then
  echo "FAIL: extracted sample CR yaml block is empty"
  exit 1
fi

# --- Test 1: parentRef.name ---
test_parent_ref_name() {
  echo "Test: spec.gateway.parentRef.name == openstack-gw"
  local actual
  actual="$(yq -r '.spec.gateway.parentRef.name' "$SAMPLE_YAML" 2>/dev/null)"
  assert_eq "spec.gateway.parentRef.name value" "openstack-gw" "$actual"
}

# --- Test 2: hostname ---
test_hostname() {
  echo "Test: spec.gateway.hostname == keystone.127-0-0-1.nip.io"
  local actual
  actual="$(yq -r '.spec.gateway.hostname' "$SAMPLE_YAML" 2>/dev/null)"
  assert_eq "spec.gateway.hostname value" "keystone.127-0-0-1.nip.io" "$actual"
}

# --- Test 3: path ---
test_path() {
  echo "Test: spec.gateway.path == /"
  local actual
  actual="$(yq -r '.spec.gateway.path' "$SAMPLE_YAML" 2>/dev/null)"
  assert_eq "spec.gateway.path value" "/" "$actual"
}

# --- Run ---
test_parent_ref_name
test_hostname
test_path

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
