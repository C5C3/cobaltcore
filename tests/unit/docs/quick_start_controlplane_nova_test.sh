#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the compute service in docs/quick-start-controlplane.md:
#   - Step 6 carries the `### Boot a first server` check
#   - the Step 5 chain runs CinderReady -> NovaReady -> ServiceAccountsReady, and
#     both the aggregate's count and the chain's length match the operator's
#     subConditionTypes
#   - the optional server boot applies the deploy/kind/fake-compute overlay and
#     boots with --nic none, since the devstack runs no OVN chassis
#   - the `# controlplane.yaml` CR of Step 3 publishes the compute API on
#     nova.127-0-0-1.nip.io with the :8443 public endpoint, carries none of the
#     optional nova blocks, and the hostname is a listener of the kind Gateway
#
# The YAML assertions run through `yq`, so they stay structural. Without `yq`
# on PATH they are skipped and the textual assertions still run.
#
# Usage: bash tests/unit/docs/quick_start_controlplane_nova_test.sh
#   QUICK_START_DOC=<path> overrides the page under test.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

QUICK_START_DOC="${QUICK_START_DOC:-$PROJECT_ROOT/docs/quick-start-controlplane.md}"
GATEWAY_MANIFEST="$PROJECT_ROOT/deploy/kind/base/openstack-gateway.yaml"
CONTROLPLANE_CONTROLLER="$PROJECT_ROOT/operators/c5c3/internal/controller/controlplane_controller.go"
NOVA_HOSTNAME="nova.127-0-0-1.nip.io"

if [[ ! -f "$QUICK_START_DOC" ]]; then
  echo "FAIL: $QUICK_START_DOC does not exist"
  exit 1
fi

# --- Test 1: the Step 6 walkthrough heading ---
test_boot_heading() {
  echo "Test: Step 6 carries the '### Boot a first server' check"
  assert_file_contains "'### Boot a first server' heading present" \
    "$QUICK_START_DOC" \
    '^### Boot a first server$'
}

# --- Test 2: the Step 5 condition chain ---
test_condition_chain() {
  echo "Test: the chain runs CinderReady -> NovaReady -> ServiceAccountsReady"
  assert_file_contains_fixed "NovaReady sits between CinderReady and ServiceAccountsReady" \
    "$QUICK_START_DOC" \
    'CinderReady → NovaReady → ServiceAccountsReady'

  # The count comes from the operator's subConditionTypes, the list the
  # aggregate Ready is computed over, so a service that adds a condition fails
  # here until the prose and the chain name it.
  local want chain
  want="$(awk '/^var subConditionTypes = \[\]string\{/{f=1;next} f&&/^\}/{exit} f&&/conditionType/{n++} END{print n+0}' \
    "$CONTROLPLANE_CONTROLLER" 2>/dev/null)"
  if [[ -z "$want" || "$want" -eq 0 ]]; then
    echo "  FAIL: could not count subConditionTypes in $CONTROLPLANE_CONTROLLER"
    FAIL=$((FAIL + 1))
    return
  fi
  assert_file_contains_fixed "the aggregate counts all $want sub-conditions" \
    "$QUICK_START_DOC" \
    "all $want sub-conditions"
  chain="$(grep -m1 '^SizingReady →' "$QUICK_START_DOC")"
  assert_eq "the Step 5 chain names all $want sub-conditions" \
    "$want" "$(( $(grep -o '→' <<<"$chain" | wc -l) + 1 ))"
}

# --- Test 3: the optional server boot ---
test_fake_compute_boot() {
  echo "Test: the optional server boot applies the fake compute and boots without a network"
  assert_file_contains_fixed "the fake-compute overlay is applied with kubectl apply -k" \
    "$QUICK_START_DOC" \
    'kubectl apply -k deploy/kind/fake-compute'
  assert_file_contains_fixed "the server boots with --nic none" \
    "$QUICK_START_DOC" \
    '--nic none'
}

test_boot_heading
test_condition_chain
test_fake_compute_boot

# Extract the first fenced yaml block that begins with the `# controlplane.yaml`
# filename marker: the Step 3 CR the reader applies (the fully expanded form
# further down carries the same marker and comes second).
CONTROLPLANE_YAML="$(mktemp)"
trap 'rm -f "$CONTROLPLANE_YAML"' EXIT

awk '
  BEGIN { in_block = 0; found = 0 }
  /^```yaml[[:space:]]*$/ {
    # Peek the next line; only enter the block if it is "# controlplane.yaml".
    if ((getline next_line) > 0) {
      if (next_line ~ /^# controlplane\.yaml[[:space:]]*$/) {
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
' "$QUICK_START_DOC" > "$CONTROLPLANE_YAML" || {
  echo "FAIL: could not locate the ControlPlane yaml block (looking for the \`# controlplane.yaml\` marker)"
  exit 1
}

if [[ ! -s "$CONTROLPLANE_YAML" ]]; then
  echo "FAIL: extracted ControlPlane yaml block is empty"
  exit 1
fi

# --- Tests 4-7: the nova block and the Gateway listener it needs ---
assert_yaml_value() {
  local description="$1" file="$2" expression="$3" expected="$4"
  echo "Test: $expression == $expected"
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq is not installed, skipping the YAML assertions"
    SKIP=$((SKIP + 1))
    return 0
  fi
  local actual
  actual="$(yq -N -r "$expression" "$file" 2>/dev/null)"
  assert_eq "$description" "$expected" "$actual"
}

assert_yaml_value "the Nova gateway hostname" "$CONTROLPLANE_YAML" \
  '.spec.services.nova.gateway.hostname' "$NOVA_HOSTNAME"
assert_yaml_value "the Nova public endpoint carries the :8443 host port" "$CONTROLPLANE_YAML" \
  '.spec.services.nova.publicEndpoint' "https://$NOVA_HOSTNAME:8443"
assert_yaml_value "the nova block sets none of the optional console, metadata or credential blocks" \
  "$CONTROLPLANE_YAML" \
  '.spec.services.nova | has("consoleProxy") or has("metadataGateway") or has("databaseCredentialsMode")' \
  'false'
assert_yaml_value "the kind Gateway carries a listener for the Nova hostname" "$GATEWAY_MANIFEST" \
  "select(.kind == \"Gateway\") | .spec.listeners[] | select(.hostname == \"$NOVA_HOSTNAME\") | .name" \
  'https-nova'

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
