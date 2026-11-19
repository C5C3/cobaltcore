#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Source and controller image digest updates must share one group. The image
# digest must come from the commit-tag index of the branch-main build, never
# from the docker datasource: quay drops the digest branch-main resolves to in
# the second the tag moves, so a pin taken from it dies with the next upstream
# build.
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
check_manager quay.io/orc/openstack-resource-controller custom.korc-commit-image branch-main deploy/bases/operators/releases/k-orc.yaml digest

# The image datasource. Renovate asks a datasource for releases only when the
# current value is a valid version; "branch-main" is one under exact
# versioning alone, and without it the lookup ends in "Could not determine new
# digest" with no request sent.
image_entry="$(jq -c '.customManagers[] | select(.depNameTemplate == "quay.io/orc/openstack-resource-controller")' "$RENOVATE_FILE")"
assert_eq "the image manager uses exact versioning" exact "$(jq -r .versioningTemplate <<<"$image_entry")"
datasource="$(jq -c '.customDatasources["korc-commit-image"] // empty' "$RENOVATE_FILE")"
assert_not_empty "the korc-commit-image datasource exists" "$datasource"
assert_eq "the datasource reads quay's active tags of the K-ORC image" \
  'https://quay.io/api/v1/repository/orc/openstack-resource-controller/tag/?onlyActiveTags=true&limit=100' \
  "$(jq -r .defaultRegistryUrlTemplate <<<"$datasource")"
assert_eq "the datasource parses JSON" json "$(jq -r .format <<<"$datasource")"
assert_eq "the datasource has one transform" 1 "$(jq -r '.transformTemplates | length' <<<"$datasource")"
transform="$(jq -r '.transformTemplates[0]' <<<"$datasource")"
assert_contains "the transform anchors on the branch-main tag" "$transform" 'tags[name = "branch-main"]'
# shellcheck disable=SC2016 # JSONata variables, not shell expansions
assert_contains "the transform selects commit tags" "$transform" '$substring(name, 0, 7) = "commit-"'
# shellcheck disable=SC2016 # JSONata variables, not shell expansions
assert_contains "the transform bounds the pair to 300 seconds" "$transform" '$abs(start_ts - $m.start_ts) <= 300'
assert_contains "the transform prefers shared platform manifests" "$transform" 'child_manifests_presence'
assert_contains "the release is named after the manager's current value" "$transform" '"version": "branch-main"'
# shellcheck disable=SC2016 # JSONata variables, not shell expansions
assert_contains "the release carries the commit tag's index digest" "$transform" '"digest": $c.manifest_digest'
assert_contains "no pair yields no release" "$transform" ': []'
assert_eq "no K-ORC manager uses the docker datasource" 0 \
  "$(jq '[.customManagers[] | select((.depNameTemplate // "") | test("orc/openstack-resource-controller")) | select(.datasourceTemplate == "docker")] | length' "$RENOVATE_FILE")"

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
