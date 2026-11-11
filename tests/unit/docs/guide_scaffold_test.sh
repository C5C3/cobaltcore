#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Check that generated guides select the deployment mode and resource names
# their prerequisites actually create, and reject incompatible combinations.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SCAFFOLD="$PROJECT_ROOT/.claude/skills/prepare-new-guide/scripts/scaffold-guide.sh"
VALIDATOR="$PROJECT_ROOT/.claude/skills/prepare-new-guide/scripts/validate-guide.sh"
PASS=0
FAIL=0
SKIP=0
source "$PROJECT_ROOT/tests/lib/assertions.sh"
record() { assert_eq "$2" "1" "$1"; }
TEST_TMP="$(mktemp -d)"
trap 'rm -rf "$TEST_TMP"' EXIT

for devstack in quick-start quick-start-extended quick-start-controlplane quick-start-metal-stack; do
  if bash "$SCAFFOLD" verify-access --devstack "$devstack" \
    --suite tests/e2e/c5c3/keystone-user > "$TEST_TMP/$devstack.md" 2> "$TEST_TMP/scaffold.log"; then
    record 1 "$devstack generates a guide"
  else
    record 0 "$devstack generates a guide"
  fi
  if bash "$VALIDATOR" "$TEST_TMP/$devstack.md" > "$TEST_TMP/validate.log" 2>&1; then
    record 1 "$devstack generated prerequisites pass validation"
  else
    record 0 "$devstack generated prerequisites pass validation"
    cat "$TEST_TMP/validate.log"
  fi
done

assert_file_contains_fixed "Quick Start creates a ControlPlane automatically" \
  "$TEST_TMP/quick-start.md" \
  'KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_CONTROLPLANE_CR=true make deploy-infra'
assert_file_contains_fixed "Quick Start uses the projected Keystone name" \
  "$TEST_TMP/quick-start.md" 'controlplane-keystone'
assert_file_not_contains "standalone guide does not enable ControlPlane mode" \
  "$TEST_TMP/quick-start-extended.md" 'WITH_CONTROLPLANE=true'
assert_file_contains_fixed "standalone guide keeps its user-owned Keystone name" \
  "$TEST_TMP/quick-start-extended.md" 'Keystone CR named `keystone`'

sed 's/WITH_CONTROLPLANE_CR=true //g' "$TEST_TMP/quick-start.md" > "$TEST_TMP/missing-cr.md"
if bash "$VALIDATOR" "$TEST_TMP/missing-cr.md" > "$TEST_TMP/validate.log" 2>&1; then
  record 0 "Quick Start rejects prerequisites that omit ControlPlane creation"
else
  record 1 "Quick Start rejects prerequisites that omit ControlPlane creation"
fi
sed 's/WITH_CONTROLPLANE=true //g' "$TEST_TMP/quick-start.md" > "$TEST_TMP/missing-mode.md"
if bash "$VALIDATOR" "$TEST_TMP/missing-mode.md" > "$TEST_TMP/validate.log" 2>&1; then
  record 0 "Quick Start rejects prerequisites that omit ControlPlane mode"
else
  record 1 "Quick Start rejects prerequisites that omit ControlPlane mode"
fi
sed 's@quick-start.md)@quick-start-extended.md)@g' "$TEST_TMP/quick-start.md" > "$TEST_TMP/wrong-guide.md"
if bash "$VALIDATOR" "$TEST_TMP/wrong-guide.md" > "$TEST_TMP/validate.log" 2>&1; then
  record 0 "standalone guide rejects ControlPlane bring-up flags"
else
  record 1 "standalone guide rejects ControlPlane bring-up flags"
fi

if bash "$SCAFFOLD" verify-dashboard --devstack quick-start --service horizon \
  --opt-in WITH_PROMETHEUS=true --suite tests/e2e/c5c3/keystone-user \
  > "$TEST_TMP/horizon.md" 2> "$TEST_TMP/scaffold.log"; then
  record 1 "Quick Start supports its Horizon service and optional monitoring"
  assert_file_contains_fixed "Horizon uses its projected child name" \
    "$TEST_TMP/horizon.md" 'controlplane-horizon'
  assert_file_contains_fixed "optional monitoring preserves ControlPlane creation" \
    "$TEST_TMP/horizon.md" 'WITH_CONTROLPLANE_CR=true WITH_PROMETHEUS=true'
else
  record 0 "Quick Start supports its Horizon service and optional monitoring"
fi
if bash "$SCAFFOLD" verify-images --devstack quick-start --service glance \
  > "$TEST_TMP/unsupported.md" 2> "$TEST_TMP/scaffold.log"; then
  record 0 "Quick Start rejects services its default deployment does not create"
else
  record 1 "Quick Start rejects services its default deployment does not create"
fi
if bash "$SCAFFOLD" verify-access --devstack quick-start \
  --opt-in WITH_CONTROLPLANE_CR=false > "$TEST_TMP/override.md" 2> "$TEST_TMP/scaffold.log"; then
  record 0 "opt-ins cannot override the selected ControlPlane creation mode"
else
  record 1 "opt-ins cannot override the selected ControlPlane creation mode"
fi

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
[[ "$FAIL" -eq 0 ]]
