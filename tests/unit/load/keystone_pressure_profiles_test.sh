#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the dizzy keystone pressure profiles under tests/load/keystone-pressure/:
#   1. Every profile carries the SPDX pair, names the patch issue it measures
#      in its header, and has a name equal to its file name, a seed and a
#      pressure block with a duration. `dizzy keystone report --compare`
#      checks the scenario name and the seed of two records, so a profile
#      whose name drifts from its file still compares, but under a name the
#      reader cannot find.
#   2. A profile that stages fan assignments declares fan users, and one with
#      grant_churn off reserves no churn user, the two checks dizzy makes only
#      after provisioning.
#   3. Every profile has a row in docs/reference/testing/keystone-pressure-testing.md,
#      and every profile that row names exists.
#
# Usage: bash tests/unit/load/keystone_pressure_profiles_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
PROFILE_DIR="$PROJECT_ROOT/tests/load/keystone-pressure"
DOC="$PROJECT_ROOT/docs/reference/testing/keystone-pressure-testing.md"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# The profile names of the doc's table: the first cell of each row that holds
# a name in backticks.
doc_profiles() {
  sed -n 's/^| `\([a-z0-9-]*\)` | #1[0-9]*.*/\1/p' "$DOC"
}

test_profiles_are_well_formed() {
  echo "Test: every profile is named after its file and carries a seed and a duration"

  local file profile count=0
  for file in "$PROFILE_DIR"/*.yaml; do
    [[ -f "$file" ]] || continue
    count=$((count + 1))
    profile="$(basename "$file" .yaml)"
    assert_file_contains "${profile} carries the SPDX copyright" "$file" \
      "^# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company$"
    assert_file_contains "${profile} carries the SPDX license" "$file" \
      "^# SPDX-License-Identifier: Apache-2.0$"
    assert_file_contains "${profile} names the patch issue it measures" "$file" \
      "^# .*#14[0-9][0-9]"
    assert_eq "${profile} is named after its file" "$profile" "$(yq -r '.name' "$file")"
    assert_not_empty "${profile} sets a seed" "$(yq -r '.seed // ""' "$file")"
    assert_not_empty "${profile} sets a pressure duration" "$(yq -r '.pressure.duration // ""' "$file")"

    if [[ "$(yq -r '[.pressure.stages[]? | select(has("fan_assignments"))] | length' "$file")" != "0" ]]; then
      assert_gte "${profile} stages fan assignments for fan users it declares" \
        "$(yq -r '.distribution.fan_users // 0' "$file")" 1
    fi
    if [[ "$(yq -r '.pressure.mix.grant_churn // 2' "$file")" == "0" ]]; then
      assert_eq "${profile} reserves no churn user while grant_churn is off" \
        "0" "$(yq -r '.pressure.churn_users // 1' "$file")"
    fi
  done
  assert_gte "the directory holds profiles" "$count" 1
}

test_doc_lists_every_profile() {
  echo "Test: the doc's profile table and the directory name the same profiles"

  local listed file profile
  listed="$(doc_profiles)"
  assert_not_empty "the doc has a profile table" "$listed"
  for file in "$PROFILE_DIR"/*.yaml; do
    [[ -f "$file" ]] || continue
    profile="$(basename "$file" .yaml)"
    assert_contains "the doc lists ${profile}" "$listed" "$profile"
  done
  for profile in $listed; do
    assert_eq "the doc's ${profile} has a file" "true" \
      "$([[ -f "$PROFILE_DIR/${profile}.yaml" ]] && echo true || echo false)"
  done
}

if ! command -v yq >/dev/null 2>&1; then
  echo "SKIP: yq not installed"
  exit 0
fi

test_profiles_are_well_formed
test_doc_lists_every_profile

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
