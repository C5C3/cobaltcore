#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the LiveMigrateOpts check in the build step of
# images/openstack-hypervisor-operator/Dockerfile.
#
# Patch 0001 replaces the gophercloud servers.LiveMigrateOpts that upstream's
# Eviction sends, because its BlockMigration is a *bool and cannot carry
# "auto", and the build fails while Go code outside the tests still names the
# type. A pin move can bring another live migration written any way Go
# allows, so the check has to fail the build on a literal, a 'var', new(), a
# pointer and an aliased import, also after a '/' in a string or a division
# on the same line, and pass on the code patch 0001 leaves, on comments, on
# the LiveMigrateOptsBuilder interface and on _test.go files.
# The test reads the grep command from the Dockerfile and runs it against a
# Go file written for each case.
#
# Usage: bash tests/unit/images/hvo_live_migrate_opts_check_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
HVO_DIR="$PROJECT_ROOT/images/openstack-hypervisor-operator"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# CHECK is the grep command of the check, the text between 'if ' and '; then'
# on the Dockerfile line that greps for LiveMigrateOpts.
CHECK_LINE="$(grep -m1 -E '^[[:space:]]*if grep .*LiveMigrateOpts' "$HVO_DIR/Dockerfile")"
CHECK="${CHECK_LINE#*if }"
CHECK="${CHECK%%; then*}"

# check_rc <file> <go source>
# Writes <go source> to <file> in a fresh tree, runs the check there and
# prints grep's exit status: 0 when it matches, which fails the build, and 1
# when it matches nothing. A grep error (2) would pass the build too, so the
# passing cases expect exactly 1.
check_rc() {
  local tree
  tree="$(mktemp -d "$TMP_DIR/tree.XXXXXX")"
  mkdir -p "$tree/$(dirname "$1")"
  printf '%s\n' "$2" > "$tree/$1"
  (cd "$tree" && eval "$CHECK" > /dev/null)
  echo "$?"
}

# patch_code prints what patch 0001 leaves in the files outside the tests: the
# context and added lines of their hunks.
patch_code() {
  awk '
    /^diff --git / { code = ($NF !~ /_test\.go$/); hunk = 0; next }
    /^@@ / { hunk = 1; next }
    hunk && code && /^[ +]/ { print substr($0, 2) }
  ' "$HVO_DIR"/patches/0001-*.patch
}

# --- Test 1: the Dockerfile carries the check ---
test_dockerfile_carries_the_check() {
  echo "Test: the Dockerfile's build step greps for LiveMigrateOpts"

  assert_starts_with "the check is a recursive grep over the Go files" "$CHECK" "grep -r"
  assert_contains "the check skips the tests" "$CHECK" "--exclude='*_test.go'"
}

# --- Test 2: every way of naming the type fails the build ---
test_every_way_of_naming_the_type_fails_the_build() {
  echo "Test: a literal, a var, new(), a pointer, an aliased import and a '/' before the type fail the build"

  assert_eq "upstream's literal fails the build" "0" "$(check_rc internal/controller/eviction/controller.go '
package eviction

	liveMigrateOpts := servers.LiveMigrateOpts{
		BlockMigration: &[]bool{false}[0],
	}')"
  assert_eq "a var declaration fails the build" "0" "$(check_rc internal/controller/maintenance/controller.go '
package maintenance

	var opts servers.LiveMigrateOpts
	opts.BlockMigration = ptr.To(false)')"
  assert_eq "new() fails the build" "0" "$(check_rc internal/controller/offboarding/controller.go '
package offboarding

	opts := new(servers.LiveMigrateOpts)')"
  assert_eq "a pointer type fails the build" "0" "$(check_rc internal/openstack/migrate.go '
package openstack

func migrateOpts() *servers.LiveMigrateOpts {')"
  assert_eq "an aliased import fails the build" "0" "$(check_rc internal/openstack/evacuate.go '
package openstack

import compute "github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"

	opts := compute.LiveMigrateOpts{Host: &host}')"
  assert_eq "a '/' in a string before the type fails the build" "0" "$(check_rc internal/controller/eviction/controller.go '
package eviction

	r.log.Info("live-migrate", "path", "a/b", "opts", servers.LiveMigrateOpts{Host: &host})')"
  assert_eq "a division before the type fails the build" "0" "$(check_rc internal/controller/eviction/controller.go '
package eviction

	opts := migrate(d/2, servers.LiveMigrateOpts{})')"
  assert_eq "an escaped quote does not end the string" "0" "$(check_rc internal/controller/eviction/controller.go '
package eviction

	msg := fmt.Sprintf("a\" // %v", servers.LiveMigrateOpts{})')"
}

# --- Test 3: the patched code, comments, the builder and tests pass ---
test_patched_code_comments_builder_and_tests_pass() {
  echo "Test: patch 0001's code, comments, the builder interface and _test.go files pass"

  local code
  code="$(patch_code)"
  # An awk that finds no hunk would pass the check below vacuously.
  assert_contains "patch 0001 leaves its liveMigrateAuto body" "$code" "type liveMigrateAuto struct{}"
  assert_contains "patch 0001 leaves the comment that names the type" "$code" "servers.LiveMigrateOpts cannot express it"
  assert_eq "the code patch 0001 leaves passes" "1" \
    "$(check_rc internal/controller/eviction/patched.go "$code")"

  assert_eq "a trailing comment passes" "1" "$(check_rc internal/controller/eviction/controller.go '
package eviction

	res := servers.LiveMigrate(ctx, r.computeClient, uuid, opts) // not servers.LiveMigrateOpts')"
  assert_eq "a trailing comment after a '//' in a string passes" "1" "$(check_rc internal/controller/eviction/controller.go '
package eviction

	u, err := url.Parse("http://nova") // servers.LiveMigrateOpts')"
  assert_eq "the LiveMigrateOptsBuilder interface passes" "1" "$(check_rc internal/controller/eviction/builder.go '
package eviction

var _ servers.LiveMigrateOptsBuilder = liveMigrateAuto{}')"
  assert_eq "a _test.go file passes" "1" "$(check_rc internal/controller/eviction/controller_test.go '
package eviction

	opts := servers.LiveMigrateOpts{BlockMigration: &[]bool{false}[0]}')"
}

# --- Run ---
test_dockerfile_carries_the_check
test_every_way_of_naming_the_type_fails_the_build
test_patched_code_comments_builder_and_tests_pass

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
