#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json holds the github.com/cert-manager/cert-manager Go
# module on the v1.20 line while the mariadb-operator API types need it:
#   - a gomod packageRule sets allowedVersions "<1.21.0" for the module and
#     does not disable it, so the v1.20 patch releases still arrive
#   - every workspace module pins cert-manager on v1.20, in lockstep
#   - the cause of the hold is still present: a workspace module requires
#     github.com/mariadb-operator/mariadb-operator (the v0 module path)
#
# cert-manager v1.21.0 removed ObjectReference from pkg/apis/meta/v1, which
# the API types of mariadb-operator v0.38.1 use, so no module builds on
# v1.21. Without the hold Renovate proposes only the newest v1.21 release,
# a pull request that cannot merge, and no v1.20 patch release ever lands:
# v1.20.3 sat unmerged for four months until GO-2026-6889 turned the
# govulncheck job red on every Go pull request. The mariadb-operator v26
# module uses IssuerReference only; the last test fails once the workspace
# moves to it, so the hold and this file go with that move.
#
# Usage: bash tests/unit/renovate/cert_manager_go_hold_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
GO_WORK_FILE="$PROJECT_ROOT/go.work"
MODULE="github.com/cert-manager/cert-manager"

# workspace_go_mods prints the go.mod path of every module go.work uses,
# one per line, relative to the project root.
workspace_go_mods() {
  awk '
    /^use[[:space:]]*\(/ { in_use = 1; next }
    in_use && /^\)/      { in_use = 0; next }
    in_use && $1 ~ /^\.\// { sub(/^\.\//, "", $1); print $1 "/go.mod" }
  ' "$GO_WORK_FILE"
}

test_hold_rule() {
  echo "Test: a gomod packageRule holds $MODULE below 1.21.0"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local rule
  rule="$(jq -c --arg m "$MODULE" '.packageRules[]
    | select(
        ((.matchManagers // []) | index("gomod")) != null
        and (((.matchPackageNames // []) | index($m)) != null)
        and has("allowedVersions")
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$rule" ]; then
    echo "  FAIL: no gomod packageRule with allowedVersions matches $MODULE"
    FAIL=$((FAIL + 2))
    return
  fi

  assert_eq "the hold allows only versions below 1.21.0" \
    "<1.21.0" \
    "$(jq -r '.allowedVersions' <<<"$rule")"
  assert_eq "the hold does not disable the module's updates" \
    "null" \
    "$(jq -r '.enabled // "null"' <<<"$rule")"
}

test_modules_pin_v1_20() {
  echo "Test: every workspace module pins $MODULE on v1.20"

  local mods mod pin found=0
  mods="$(workspace_go_mods)"
  if [ -z "$mods" ]; then
    echo "  FAIL: go.work lists no modules"
    FAIL=$((FAIL + 1))
    return
  fi

  while IFS= read -r mod; do
    pin="$(awk -v m="$MODULE" '$1 == m { print $2; exit }' "$PROJECT_ROOT/$mod")"
    if [ -z "$pin" ]; then
      continue
    fi
    found=$((found + 1))
    if [[ "$pin" =~ ^v1\.20\.[0-9]+$ ]]; then
      echo "  PASS: $mod pins $MODULE $pin"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $mod pins $MODULE $pin, outside the v1.20 line the hold keeps"
      FAIL=$((FAIL + 1))
    fi
  done <<<"$mods"

  assert_gte "at least one workspace module requires $MODULE" "$found" 1
}

test_hold_still_needed() {
  echo "Test: a workspace module still requires the v0 mariadb-operator module"

  local mods mod users=""
  mods="$(workspace_go_mods)"
  while IFS= read -r mod; do
    if awk '$1 == "github.com/mariadb-operator/mariadb-operator" { f = 1 } END { exit !f }' \
      "$PROJECT_ROOT/$mod"; then
      users="$users $mod"
    fi
  done <<<"$mods"

  if [ -n "$users" ]; then
    echo "  PASS: the hold's cause is present in:$users"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: no workspace module requires github.com/mariadb-operator/mariadb-operator any more;"
    echo "        drop the $MODULE hold from renovate.json and this test"
    FAIL=$((FAIL + 1))
  fi
}

test_hold_rule
test_modules_pin_v1_20
test_hold_still_needed

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
