#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Source and controller image digest updates must share one group.
set -uo pipefail
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
PASS=0
FAIL=0
# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
RENOVATE_FILE="$PROJECT_ROOT/renovate.json"

if ! command -v jq >/dev/null || ! command -v perl >/dev/null; then
  echo 'SKIP: jq and perl are required'
  exit 0
fi

check_manager() {
  local package="$1" datasource="$2" ref="$3" file="$4" key="$5"
  local entry regex captured expected pattern
  entry="$(jq -c --arg pkg "$package" '.customManagers[] | select(.depNameTemplate == $pkg)' "$RENOVATE_FILE")"
  assert_not_empty "$package has a custom manager" "$entry"
  assert_eq "$package uses the expected datasource" "$datasource" "$(jq -r .datasourceTemplate <<<"$entry")"
  assert_eq "$package follows the main line" "$ref" "$(jq -r .currentValueTemplate <<<"$entry")"
  pattern="$(jq -r '.managerFilePatterns[0]' <<<"$entry")"
  pattern="${pattern#/}"; pattern="${pattern%/}"
  assert_eq "$package targets its manifest" 1 "$(PATTERN="$pattern" FILE="$file" perl -e 'print($ENV{FILE} =~ /$ENV{PATTERN}/ ? 1 : 0)')"
  regex="$(jq -r '.matchStrings[0]' <<<"$entry")"
  captured="$(REGEX="$regex" perl -0777 -ne 'if (/$ENV{REGEX}/) {print $+{currentDigest}}' "$PROJECT_ROOT/$file")"
  expected="$(awk -v k="$key:" '$1 == k {print $2; exit}' "$PROJECT_ROOT/$file")"
  assert_not_empty "$package extracts an immutable pin" "$captured"
  assert_eq "$package captures the committed digest" "$expected" "$captured"
}

check_manager https://github.com/k-orc/openstack-resource-controller git-refs main deploy/bases/operators/sources/k-orc.yaml commit
check_manager quay.io/orc/openstack-resource-controller docker branch-main deploy/bases/operators/releases/k-orc.yaml digest

for package in https://github.com/k-orc/openstack-resource-controller quay.io/orc/openstack-resource-controller; do
  rule="$(jq -c --arg pkg "$package" '.packageRules[] | select((.matchPackageNames // []) | index($pkg)) | select(.groupName == "k-orc")' "$RENOVATE_FILE")"
  assert_not_empty "$package joins the k-orc group" "$rule"
  assert_eq "$package enables automerge after required CI" true "$(jq -r .automerge <<<"$rule")"
  assert_eq "$package only automerges digest updates" digest "$(jq -r '.matchUpdateTypes | join(",")' <<<"$rule")"
  assert_eq "$package does not wait for a continuously moving branch to age" '0 days' "$(jq -r .minimumReleaseAge <<<"$rule")"
  assert_eq "$package uses the custom pin managers" 'custom.regex' "$(jq -r '.matchManagers | sort | join(",")' <<<"$rule")"
done

echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
