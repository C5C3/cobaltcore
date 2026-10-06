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
# Usage: bash tests/unit/hack/ghcr_prune_stale_versions_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SCRIPT_UNDER_TEST="$PROJECT_ROOT/hack/ghcr-prune-stale-versions.py"

PASS=0
FAIL=0
TMPDIR_BASE=$(mktemp -d)

cleanup() {
  rm -rf "$TMPDIR_BASE"
}
trap cleanup EXIT

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

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

# --- Test 9: a package that was never published has nothing to prune ---
# The only case that needs the API path: the module is loaded with its request()
# replaced by a stub that answers 404, the way GHCR does for an unknown package.
run_against_missing_package() {
  python3 - "$SCRIPT_UNDER_TEST" "$@" <<'PYEOF'
import importlib.util
import sys

spec = importlib.util.spec_from_file_location("ghcr_prune", sys.argv[1])
prune = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prune)
prune.request = lambda url, **kwargs: (404, {}, b"")
sys.exit(prune.main(["--org", "demo-org", "--package", "demo", "--token", "t"] + sys.argv[2:]))
PYEOF
}

test_missing_package_is_fatal_only_for_a_full_sweep() {
  echo "Test: an unpublished package ends a narrowed run cleanly and fails a full sweep"
  local exit_code=0 output

  output=$(run_against_missing_package --only-tag-pattern '^e2e-42-' --min-age-hours 0 2>&1) || exit_code=$?
  assert_eq "narrowed mode exits 0" "0" "$exit_code"
  assert_contains "and says there was nothing to prune" "$output" "package not found: demo-org/demo; nothing to prune"

  exit_code=0
  output=$(run_against_missing_package 2>&1) || exit_code=$?
  assert_eq "a full sweep exits 1" "1" "$exit_code"
  assert_contains "and names the package" "$output" "package not found: demo-org/demo"
  assert_not_contains "without calling it prunable" "$output" "nothing to prune"
}

# --- Tests 10 and 11: a pinned upstream commit stays pullable ---
# openstack-hypervisor-operator as merge-hvo-image leaves it behind: the
# current pin's main build carries latest, sha-<pin>, upstream-<pin> and the
# composite sha-<pin>-<sha>; an older pin's main build was left with
# sha-<pin> and upstream-<pin> once a newer build took latest; a branch build
# carries only its composite tag. The upstream chart of the older pin still
# names sha-<pin>, so that manifest has to survive both modes.
HVO_CURRENT=$(digest 1)
HVO_OLD_PIN=$(digest 2)
HVO_BRANCH=$(digest 3)
PIN_A=$(printf '%.0sa' {1..40})
PIN_C=$(printf '%.0sc' {1..40})
SHA_B=$(printf '%.0sb' {1..40})
SHA_D=$(printf '%.0sd' {1..40})

write_hvo_fixture() {
  cat > "$1" <<EOF
{
  "now": "2026-08-22T00:00:00Z",
  "versions": [
    {"id": 1, "name": "${HVO_CURRENT}", "created_at": "2026-08-01T00:00:00Z",
     "metadata": {"container": {"tags": ["latest", "sha-${PIN_C}", "upstream-${PIN_C}", "sha-${PIN_C}-${SHA_D}"]}}},
    {"id": 2, "name": "${HVO_OLD_PIN}", "created_at": "2026-07-01T00:00:00Z",
     "metadata": {"container": {"tags": ["sha-${PIN_A}", "upstream-${PIN_A}"]}}},
    {"id": 3, "name": "${HVO_BRANCH}", "created_at": "2026-07-01T00:00:00Z",
     "metadata": {"container": {"tags": ["sha-${PIN_A}-${SHA_B}"]}}}
  ],
  "manifests": {}
}
EOF
}

test_upstream_pin_tag_is_a_keeper() {
  echo "Test: an upstream-<commit> tag keeps its manifest without latest"
  local fixture="$TMPDIR_BASE/hvo-keep.json" output mode
  write_hvo_fixture "$fixture"

  for mode in --only-sha-tags full; do
    if [ "$mode" = "full" ]; then
      output=$(plan "$fixture")
    else
      output=$(plan "$fixture" "$mode")
    fi
    assert_contains "the old pin is kept ($mode)" \
      "$(echo "$output" | jq -c '.keep')" "$HVO_OLD_PIN"
    assert_not_contains "the old pin is not deleted ($mode)" \
      "$(echo "$output" | jq -c '[.delete[].digest]')" "$HVO_OLD_PIN"
  done
}

test_composite_only_hvo_build_is_deleted() {
  echo "Test: a build with only sha-<commit>-<sha> is deleted by the full sweep"
  local fixture="$TMPDIR_BASE/hvo-composite.json" output deleted
  write_hvo_fixture "$fixture"
  output=$(plan "$fixture")
  deleted=$(echo "$output" | jq -c '[.delete[].digest]')

  assert_contains "the composite-only build is deleted" "$deleted" "$HVO_BRANCH"
  assert_not_contains "the current pin is not deleted" "$deleted" "$HVO_CURRENT"
}

# --- Test 12: the libvirt keeper tag keeps the build the lab pins ---
# libvirt as merge-libvirt-image leaves it behind: the current main build
# carries latest, its <sha40> and the keeper tag hack/ci-tag-libvirt-keeper.sh
# minted for a newer package version; an older build kept only its <sha40>
# and the keeper tag the lab manifests pin; the first build kept its <sha40>
# and a keeper tag whose Ubuntu revision has no dot; a superseded build carries
# its <sha40> alone. A tag that is the package version without -r<N> is no
# keeper. The five share one fixture, because a package without any keeper
# trips the full sweep's refusal of Test 7.
LIBVIRT_CURRENT=$(digest 5)
LIBVIRT_PINNED=$(digest 6)
LIBVIRT_SUPERSEDED=$(digest 7)
LIBVIRT_BARE_VERSION=$(digest 8)
LIBVIRT_UNDOTTED=$(digest 4)
SHA_E=$(printf '%.0se' {1..40})

test_libvirt_keeper_tag_keeps_the_pinned_build() {
  echo "Test: a <libvirt-package-version>-r<N> tag keeps its manifest without latest"
  local fixture="$TMPDIR_BASE/libvirt.json" output mode pinned_tag
  pinned_tag="$(grep -hoE 'image: ghcr\.io/c5c3/libvirt:[^@[:space:]]+' \
    "$PROJECT_ROOT/deploy/lab/metal-stack/hypervisor/libvirt-daemonset.yaml" | head -1 | sed 's/.*libvirt://' || true)"
  assert_not_empty "the DaemonSet carries a pinned tag to test" "$pinned_tag"
  cat > "$fixture" <<EOF
{
  "now": "2026-10-04T00:00:00Z",
  "versions": [
    {"id": 1, "name": "${LIBVIRT_CURRENT}", "created_at": "2026-10-02T00:00:00Z",
     "metadata": {"container": {"tags": ["latest", "${SHA_D}", "10.0.0-2ubuntu8.20-r1"]}}},
    {"id": 2, "name": "${LIBVIRT_PINNED}", "created_at": "2026-09-01T00:00:00Z",
     "metadata": {"container": {"tags": ["${SHA_B}", "${pinned_tag}"]}}},
    {"id": 3, "name": "${LIBVIRT_SUPERSEDED}", "created_at": "2026-09-15T00:00:00Z",
     "metadata": {"container": {"tags": ["${PIN_A}"]}}},
    {"id": 4, "name": "${LIBVIRT_BARE_VERSION}", "created_at": "2026-09-20T00:00:00Z",
     "metadata": {"container": {"tags": ["${PIN_C}", "10.0.0-2ubuntu8.19"]}}},
    {"id": 5, "name": "${LIBVIRT_UNDOTTED}", "created_at": "2026-08-01T00:00:00Z",
     "metadata": {"container": {"tags": ["${SHA_E}", "10.0.0-2ubuntu8-r1"]}}}
  ],
  "manifests": {}
}
EOF

  for mode in --only-sha-tags full; do
    if [ "$mode" = "full" ]; then
      output=$(plan "$fixture")
    else
      output=$(plan "$fixture" "$mode")
    fi
    assert_contains "the current build is kept ($mode)" \
      "$(echo "$output" | jq -c '.keep')" "$LIBVIRT_CURRENT"
    assert_not_contains "the current build is not deleted ($mode)" \
      "$(echo "$output" | jq -c '[.delete[].digest]')" "$LIBVIRT_CURRENT"
    assert_contains "the pinned build is kept ($mode)" \
      "$(echo "$output" | jq -c '.keep')" "$LIBVIRT_PINNED"
    assert_not_contains "the pinned build is not deleted ($mode)" \
      "$(echo "$output" | jq -c '[.delete[].digest]')" "$LIBVIRT_PINNED"
    assert_contains "the build with an undotted Ubuntu revision is kept ($mode)" \
      "$(echo "$output" | jq -c '.keep')" "$LIBVIRT_UNDOTTED"
    assert_contains "the superseded build is deleted ($mode)" \
      "$(echo "$output" | jq -c '[.delete[].digest]')" "$LIBVIRT_SUPERSEDED"
  done

  output=$(plan "$fixture")
  assert_not_contains "the package version alone is no keeper" \
    "$(echo "$output" | jq -c '.keep')" "$LIBVIRT_BARE_VERSION"
  assert_contains "the full sweep deletes a build tagged with the package version alone" \
    "$(echo "$output" | jq -c '[.delete[].digest]')" "$LIBVIRT_BARE_VERSION"
}

# --- Test 13: an unreadable kept manifest stops the sweep ---
# The API path again: request() lists a kept index and an untagged
# per-platform manifest, and answers the read of the index's manifest with a
# 401. Without the index's child list the sweep cannot tell which untagged
# versions the index still needs, so it has to delete nothing.
run_against_unreadable_manifest() {
  python3 - "$SCRIPT_UNDER_TEST" <<'PYEOF'
import importlib.util
import json
import sys
import urllib.error

spec = importlib.util.spec_from_file_location("ghcr_prune", sys.argv[1])
prune = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prune)

versions = [
    {"id": 1, "name": "sha256:" + "a" * 64, "created_at": "2026-01-01T00:00:00Z",
     "metadata": {"container": {"tags": ["latest"]}}},
    {"id": 2, "name": "sha256:" + "b" * 64, "created_at": "2026-01-01T00:00:00Z",
     "metadata": {"container": {"tags": []}}},
]


def request(url, token=None, method="GET", accept=None):
    if method == "DELETE":
        print(f"DELETE {url}")
        return 204, {}, b""
    if "/manifests/" in url:
        raise urllib.error.HTTPError(url, 401, "Unauthorized", {}, None)
    return 200, {}, json.dumps(versions).encode()


prune.request = request
prune.Registry._registry_token = lambda self, github_token: "registry-token"
try:
    sys.exit(prune.main(["--org", "demo-org", "--package", "demo", "--token", "t"]))
except urllib.error.HTTPError as exc:
    print(f"aborted on {exc.code}")
    sys.exit(1)
PYEOF
}

test_unreadable_kept_manifest_stops_the_sweep() {
  echo "Test: a kept index whose manifest read fails with a 401 stops the sweep before any deletion"
  local exit_code=0 output

  output=$(run_against_unreadable_manifest 2>&1) || exit_code=$?
  assert_eq "the sweep exits 1" "1" "$exit_code"
  assert_contains "on the registry's 401" "$output" "aborted on 401"
  assert_not_contains "and deletes no version" "$output" "DELETE"
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
test_missing_package_is_fatal_only_for_a_full_sweep
echo ""
test_upstream_pin_tag_is_a_keeper
echo ""
test_composite_only_hvo_build_is_deleted
echo ""
test_libvirt_keeper_tag_keeps_the_pinned_build
echo ""
test_unreadable_kept_manifest_stops_the_sweep
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
