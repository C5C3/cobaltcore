#!/usr/bin/env bats
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
# Usage: bin/bats tests/unit/load/keystone_pressure_profiles.bats

load '../../lib/bats/common'

PROFILE_DIR="$PROJECT_ROOT/tests/load/keystone-pressure"
DOC="$PROJECT_ROOT/docs/reference/testing/keystone-pressure-testing.md"

# The profile names of the doc's table: the first cell of each row that holds
# a name in backticks.
doc_profiles() {
  sed -n 's/^| `\([a-z0-9-]*\)` | #1[0-9]*.*/\1/p' "$DOC"
}

setup() {
  require_tool yq
}

@test "every profile is named after its file and carries a seed and a duration" {
  local file profile count=0
  for file in "$PROFILE_DIR"/*.yaml; do
    [[ -f "$file" ]] || continue
    count=$((count + 1))
    profile="$(basename "$file" .yaml)"
    soft assert_file_matches "${profile} carries the SPDX copyright" "$file" \
      "^# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company$"
    soft assert_file_matches "${profile} carries the SPDX license" "$file" \
      "^# SPDX-License-Identifier: Apache-2.0$"
    soft assert_file_matches "${profile} names the patch issue it measures" "$file" \
      "^# .*#14[0-9][0-9]"
    soft assert_eq "${profile} is named after its file" "$profile" "$(yq -r '.name' "$file")"
    soft assert_not_empty "${profile} sets a seed" "$(yq -r '.seed // ""' "$file")"
    soft assert_not_empty "${profile} sets a pressure duration" "$(yq -r '.pressure.duration // ""' "$file")"

    if [[ "$(yq -r '[.pressure.stages[]? | select(has("fan_assignments"))] | length' "$file")" != "0" ]]; then
      soft assert_gte "${profile} stages fan assignments for fan users it declares" \
        "$(yq -r '.distribution.fan_users // 0' "$file")" 1
    fi
    if [[ "$(yq -r '.pressure.mix.grant_churn // 2' "$file")" == "0" ]]; then
      soft assert_eq "${profile} reserves no churn user while grant_churn is off" \
        "0" "$(yq -r '.pressure.churn_users // 1' "$file")"
    fi
  done
  assert_gte "the directory holds profiles" "$count" 1
}

@test "the doc's profile table and the directory name the same profiles" {
  local listed file profile
  listed="$(doc_profiles)"
  assert_not_empty "the doc has a profile table" "$listed"
  for file in "$PROFILE_DIR"/*.yaml; do
    [[ -f "$file" ]] || continue
    profile="$(basename "$file" .yaml)"
    soft assert_contains "the doc lists ${profile}" "$listed" "$profile"
  done
  for profile in $listed; do
    soft assert_eq "the doc's ${profile} has a file" "true" \
      "$([[ -f "$PROFILE_DIR/${profile}.yaml" ]] && echo true || echo false)"
  done
}
