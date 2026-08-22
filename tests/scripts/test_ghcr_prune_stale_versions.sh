#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Shell tests for hack/ghcr-prune-stale-versions.py
#
# The script's retention decision is the part worth testing: which package
# versions survive, given a tag set, a manifest tree and an age. Its --plan-from
# mode reads that input from a fixture instead of the GHCR API, so every case
# below runs offline.
#
# Usage: bash tests/scripts/test_ghcr_prune_stale_versions.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
SCRIPT_UNDER_TEST="$PROJECT_ROOT/hack/ghcr-prune-stale-versions.py"

PASS=0
FAIL=0
TMPDIR_BASE=$(mktemp -d)

cleanup() {
  rm -rf "$TMPDIR_BASE"
}
trap cleanup EXIT

# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# Digests are only compared for equality, so a repeated nibble per manifest keeps
# the fixtures readable while staying the right length for the referrer regex.
digest() { printf 'sha256:%s' "$(printf '%.0s'"$1" {1..64})"; }
referrer_tag() { printf 'sha256-%s' "$(printf '%.0s'"$1" {1..64})"; }

CURRENT_INDEX=$(digest a)
CURRENT_CHILD=$(digest b)
STALE_INDEX=$(digest c)
STALE_CHILD=$(digest d)
CURRENT_SIG=$(digest e)
STALE_SIG=$(digest f)
FRESH_E2E=$(digest 9)

# One package as CI actually leaves it behind: the current main build carrying
# release, version, composite and SHA tags on a single manifest; the build before
# it left with only its composite and SHA tags; a cosign artifact hanging off
# each; and a run-scoped tag from a CI run that is still in flight.
write_fixture() {
  cat > "$1" <<EOF
{
  "now": "2026-08-22T00:00:00Z",
  "versions": [
    {"id": 1, "name": "${CURRENT_INDEX}", "created_at": "2026-08-01T00:00:00Z",
     "metadata": {"container": {"tags": ["2026.1", "32.0.0", "32.0.0-p0-main-1111111", "1111111"]}}},
    {"id": 2, "name": "${CURRENT_CHILD}", "created_at": "2026-08-01T00:00:00Z",
     "metadata": {"container": {"tags": []}}},
    {"id": 3, "name": "${STALE_INDEX}", "created_at": "2026-07-01T00:00:00Z",
     "metadata": {"container": {"tags": ["32.0.0-p0-main-2222222", "2222222"]}}},
    {"id": 4, "name": "${STALE_CHILD}", "created_at": "2026-07-01T00:00:00Z",
     "metadata": {"container": {"tags": []}}},
    {"id": 5, "name": "${CURRENT_SIG}", "created_at": "2026-08-01T00:00:00Z",
     "metadata": {"container": {"tags": ["$(referrer_tag b)"]}}},
    {"id": 6, "name": "${STALE_SIG}", "created_at": "2026-07-01T00:00:00Z",
     "metadata": {"container": {"tags": ["$(referrer_tag d)"]}}},
    {"id": 7, "name": "${FRESH_E2E}", "created_at": "2026-08-21T23:00:00Z",
     "metadata": {"container": {"tags": ["e2e-42-dev"]}}}
  ],
  "manifests": {
    "${CURRENT_INDEX}": {"manifests": [{"digest": "${CURRENT_CHILD}"}]},
    "${STALE_INDEX}": {"manifests": [{"digest": "${STALE_CHILD}"}]}
  }
}
EOF
}

# The script keeps the plan document on stdout and its diagnostics on stderr, so
# the two helpers below pick whichever stream a case is asserting against.
plan() {
  local fixture="$1"
  shift
  python3 "$SCRIPT_UNDER_TEST" --plan-from "$fixture" --package demo --plan-json "$@" 2>/dev/null
}

plan_messages() {
  local fixture="$1"
  shift
  python3 "$SCRIPT_UNDER_TEST" --plan-from "$fixture" --package demo "$@" 2>&1 >/dev/null
}

# --- Test 1: a manifest carrying a release tag survives, with its children ---
test_keeper_tag_protects_manifest_and_children() {
  echo "Test: a release-tagged manifest and its index children are kept"
  local fixture="$TMPDIR_BASE/keep.json" output
  write_fixture "$fixture"
  output=$(plan "$fixture")

  assert_contains "the current index is kept" "$(echo "$output" | jq -c '.keep')" "$CURRENT_INDEX"
  assert_contains "its per-platform child is kept" "$(echo "$output" | jq -c '.keep')" "$CURRENT_CHILD"
  assert_not_contains \
    "the composite tag on that manifest does not drag it into the delete set" \
    "$(echo "$output" | jq -c '.delete')" "$CURRENT_INDEX"
  assert_not_contains \
    "neither does the SHA tag sharing the manifest" \
    "$(echo "$output" | jq -c '[.delete[].tags[]]')" "1111111"
}

# --- Test 2: composite and SHA tags with no version tag are deletable ---
test_superseded_build_is_deleted() {
  echo "Test: a superseded build keeps only throwaway tags and is deleted"
  local fixture="$TMPDIR_BASE/stale.json" output deleted
  write_fixture "$fixture"
  output=$(plan "$fixture")
  deleted=$(echo "$output" | jq -c '[.delete[].digest]')

  assert_contains "the superseded index is deleted" "$deleted" "$STALE_INDEX"
  assert_contains "its orphaned child is deleted too" "$deleted" "$STALE_CHILD"
}

# --- Test 3: referrer artifacts follow their subject ---
test_referrers_follow_their_subject() {
  echo "Test: cosign artifacts are kept or deleted with their subject"
  local fixture="$TMPDIR_BASE/referrer.json" output
  write_fixture "$fixture"
  output=$(plan "$fixture")

  assert_contains \
    "the artifact attached to a kept child is kept" \
    "$(echo "$output" | jq -c '.keep')" "$CURRENT_SIG"
  assert_contains \
    "the artifact attached to a deleted child is deleted" \
    "$(echo "$output" | jq -c '[.delete[].digest]')" "$STALE_SIG"
}

# --- Test 4: the age guard protects in-flight CI runs ---
test_min_age_protects_recent_versions() {
  echo "Test: versions younger than --min-age-hours are left alone"
  local fixture="$TMPDIR_BASE/age.json" output
  write_fixture "$fixture"

  output=$(plan "$fixture")
  assert_not_contains \
    "a one-hour-old e2e tag survives the default 24h guard" \
    "$(echo "$output" | jq -c '[.delete[].digest]')" "$FRESH_E2E"
  assert_eq "and is reported as skipped" "1" "$(echo "$output" | jq -r '.too_young')"

  output=$(plan "$fixture" --min-age-hours 0)
  assert_contains \
    "with the guard disabled it becomes a candidate" \
    "$(echo "$output" | jq -c '[.delete[].digest]')" "$FRESH_E2E"
}

# --- Test 5: --only-tag-pattern narrows the candidate set ---
test_only_tag_pattern_narrows_candidates() {
  echo "Test: --only-tag-pattern restricts deletion to matching tag sets"
  local fixture="$TMPDIR_BASE/only.json" output deleted
  write_fixture "$fixture"
  output=$(plan "$fixture" --min-age-hours 0 --only-tag-pattern '^e2e-')
  deleted=$(echo "$output" | jq -c '[.delete[].digest]')

  assert_contains "the run-scoped tag is deleted" "$deleted" "$FRESH_E2E"
  assert_not_contains "the superseded index is out of scope" "$deleted" "$STALE_INDEX"
  assert_not_contains \
    "untagged versions are never touched in this mode (GH-312)" \
    "$deleted" "$STALE_CHILD"
}

# --- Test 6: --only-sha-tags covers every SHA shape the repo publishes ---
test_only_sha_tags_matches_all_publishing_paths() {
  echo "Test: --only-sha-tags matches short, long, sha- and release-SHA tags"
  local fixture="$TMPDIR_BASE/sha.json" output deleted
  cat > "$fixture" <<EOF
{
  "now": "2026-08-22T00:00:00Z",
  "versions": [
    {"id": 1, "name": "$(digest 1)", "created_at": "2026-07-01T00:00:00Z",
     "metadata": {"container": {"tags": ["latest"]}}},
    {"id": 2, "name": "$(digest 2)", "created_at": "2026-07-01T00:00:00Z",
     "metadata": {"container": {"tags": ["abc1234"]}}},
    {"id": 3, "name": "$(digest 3)", "created_at": "2026-07-01T00:00:00Z",
     "metadata": {"container": {"tags": ["$(printf '%.0sa' {1..40})"]}}},
    {"id": 4, "name": "$(digest 4)", "created_at": "2026-07-01T00:00:00Z",
     "metadata": {"container": {"tags": ["sha-$(printf '%.0sb' {1..40})"]}}},
    {"id": 5, "name": "$(digest 5)", "created_at": "2026-07-01T00:00:00Z",
     "metadata": {"container": {"tags": ["2026.1-$(printf '%.0sc' {1..40})"]}}},
    {"id": 6, "name": "$(digest 6)", "created_at": "2026-07-01T00:00:00Z",
     "metadata": {"container": {"tags": ["32.0.0-p0-main-9999999"]}}}
  ],
  "manifests": {}
}
EOF
  output=$(plan "$fixture" --only-sha-tags)
  deleted=$(echo "$output" | jq -c '[.delete[].tags[]]')

  assert_contains "short SHA (service images)" "$deleted" "abc1234"
  assert_contains "long SHA (base images)" "$deleted" "$(printf '%.0sa' {1..40})"
  assert_contains "sha- prefixed (operator images)" "$deleted" "sha-$(printf '%.0sb' {1..40})"
  assert_contains "release-scoped SHA (tempest)" "$deleted" "2026.1-$(printf '%.0sc' {1..40})"
  assert_not_contains "composite tags are left to the full sweep" "$deleted" "32.0.0-p0-main-9999999"
  assert_not_contains "latest is never a SHA tag" "$deleted" "latest"
}

# --- Test 7: a package with no keeper tag is not swept blank ---
test_empty_keep_set_aborts_full_sweep() {
  echo "Test: a full sweep refuses to run when nothing carries a keeper tag"
  local fixture="$TMPDIR_BASE/nokeep.json" exit_code=0 output
  cat > "$fixture" <<EOF
{
  "now": "2026-08-22T00:00:00Z",
  "versions": [
    {"id": 1, "name": "$(digest 7)", "created_at": "2026-07-01T00:00:00Z",
     "metadata": {"container": {"tags": ["e2e-1-dev"]}}}
  ],
  "manifests": {}
}
EOF
  output=$(plan_messages "$fixture") || exit_code=$?
  assert_eq "exit code is 1" "1" "$exit_code"
  assert_contains "and it says why" "$output" "refusing to sweep the whole package"

  # Narrowed mode is bounded by its own pattern, so it only warns.
  exit_code=0
  plan "$fixture" --min-age-hours 0 --only-tag-pattern '^e2e-' >/dev/null || exit_code=$?
  assert_eq "narrowed mode still runs" "0" "$exit_code"
}

# --- Test 8: the deletion cap is reported, not silent ---
test_deletion_cap_is_reported() {
  echo "Test: --max-deletions truncates and says how much is left"
  local fixture="$TMPDIR_BASE/cap.json" output
  write_fixture "$fixture"
  output=$(plan "$fixture" --max-deletions 1)

  assert_eq "only one version is planned for deletion" "1" "$(echo "$output" | jq -r '.delete | length')"
  assert_eq "the remainder is counted" "2" "$(echo "$output" | jq -r '.capped')"
  assert_contains "and surfaced as a warning" "$(plan_messages "$fixture" --max-deletions 1)" "deletion cap reached"
}

# --- Run all tests ---
echo "=== ghcr-prune-stale-versions.py tests ==="
echo ""
test_keeper_tag_protects_manifest_and_children
echo ""
test_superseded_build_is_deleted
echo ""
test_referrers_follow_their_subject
echo ""
test_min_age_protects_recent_versions
echo ""
test_only_tag_pattern_narrows_candidates
echo ""
test_only_sha_tags_matches_all_publishing_paths
echo ""
test_empty_keep_set_aborts_full_sweep
echo ""
test_deletion_cap_is_reported
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
