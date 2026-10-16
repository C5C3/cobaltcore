#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# images/openstack-hypervisor-operator/patch-checks.sh — The checks of the
# patches under patches/, run by the checkPhase of manager.nix in the patched
# source tree with the vendored modules.
#
# check_live_migrate_opts and the first go test are patch 0001's checks. No
# code outside the tests may still name gophercloud's servers.LiveMigrateOpts:
# its BlockMigration is a *bool and cannot carry "auto", so the check catches
# a live migration that a pin move adds or reshapes, and a re-cut that lost
# the controller.go hunk. The grep matches the bare type name, not one way of
# writing it, so a literal, a 'var', new(), a pointer and an aliased import
# all fail the build. Only a '//' outside a double-quoted string before the
# name on its line lets the line pass, so a comment passes while a '/' in a
# string or a division does not. The vendor/ directory buildGoModule copies
# into the tree is skipped: gophercloud defines the type there. A grep error
# fails the build too. The test the patch brings proves its body; 'go test
# -run' exits 0 with "[no tests to run]" when the test is missing, so
# require_pass, the grep for its PASS line, is what fails a patch that lost
# its test hunk.
#
# The second go test is the check of patches 0002 and 0003, one test each.
# Its -run expression does not match the package's Ginkgo entry point
# (TestControllers in suite_test.go), so no envtest binary is needed, and the
# space after each test name keeps a subtest's PASS line from matching. The
# third go test is patch 0004's check; its -run expression does not match the
# openstack package's Ginkgo entry point (TestOpenstack in suite_test.go), so
# that suite does not run either.
#
# Once upstream carries a change, drop its patch together with its checks
# here; a later patch adds its file under patches/ and its check here.
# tests/unit/images/hvo_live_migrate_opts_check_test.sh sources this file.

set -euo pipefail

# check_live_migrate_opts <dir> returns 0 when no Go file outside the tests
# under <dir> names LiveMigrateOpts, 1 when one does, and 2 when grep fails.
check_live_migrate_opts() {
  local matches status=0
  matches=$(grep -rnE --include='*.go' --exclude='*_test.go' --exclude-dir=vendor \
    '^([^/"]|/[^/]|"([^"\\]|\\.)*")*\bLiveMigrateOpts\b' "$1") || status=$?
  case "$status" in
    0)
      echo "$matches" >&2
      echo "a live migration still sends servers.LiveMigrateOpts; extend patch 0001" >&2
      return 1
      ;;
    1) return 0 ;;
    *)
      echo "patch-checks: grep exited ${status}" >&2
      return 2
      ;;
  esac
}

# require_pass <log> <test> returns 0 when <log> holds the top-level PASS line
# of <test>.
require_pass() {
  if ! grep -q -- "--- PASS: $2 " "$1"; then
    echo "patch-checks: $2 printed no top-level PASS line (see $1)" >&2
    return 1
  fi
}

main() {
  local status=0
  check_live_migrate_opts .

  # require_pass names a test that failed, is missing or did not build: each
  # leaves its PASS line out of the log. The status catches a package that
  # fails after its tests printed PASS, such as a panic in a goroutine a test
  # left behind or a TestMain that fails after m.Run().
  go test -count=1 -v -run '^TestLiveMigrateAutoBody$' ./internal/controller/eviction/ \
    2>&1 | tee "$TMPDIR/patch-0001.log" || status=1
  go test -count=1 -v \
    -run '^(TestHypervisorCreatedWithDefaultHighAvailability|TestTraitsInSyncSetsTraitsUpdated)$' \
    ./internal/controller/ 2>&1 | tee "$TMPDIR/patch-0002-0003.log" || status=1
  go test -count=1 -v -run '^TestServiceClientInterface$' ./internal/openstack/ \
    2>&1 | tee "$TMPDIR/patch-0004.log" || status=1

  require_pass "$TMPDIR/patch-0001.log" TestLiveMigrateAutoBody
  require_pass "$TMPDIR/patch-0002-0003.log" TestHypervisorCreatedWithDefaultHighAvailability
  require_pass "$TMPDIR/patch-0002-0003.log" TestTraitsInSyncSetsTraitsUpdated
  require_pass "$TMPDIR/patch-0004.log" TestServiceClientInterface

  if [ "$status" -ne 0 ]; then
    echo "patch-checks: a go test run exited non-zero after its tests passed (see $TMPDIR/patch-*.log)" >&2
    return 1
  fi
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
