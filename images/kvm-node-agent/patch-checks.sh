#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# images/kvm-node-agent/patch-checks.sh — The checks of the patches under
# patches/, run by the checkPhase of manager.nix in the patched source tree
# with the vendored modules.
#
# The two tests are patch 0001's checks. A test binary exits 0 with
# "testing: warning: no tests to run" when the test is missing, so
# require_pass, the grep for each PASS line, is what fails a patch that lost
# its test hunk; the space after the test name keeps a subtest's PASS line
# from matching. A root process may give a file any group, so
# TestUpdateTLSCertificateKeyGroupNotPermitted skips as root and prints
# '--- SKIP', which fails its require_pass. Nix builds as a build user, and
# refuse_root fails a build that runs as root anyway before a test runs.
#
# Once upstream carries the change, both checks go with the patch; a later
# patch adds its file under patches/ and its check here.
# tests/unit/images/kna_patch_test_guard_test.sh sources this file.

set -euo pipefail

# refuse_root returns 1 when the checks run as root.
refuse_root() {
  if [ "$(id -u)" = 0 ]; then
    echo "patch-checks: TestUpdateTLSCertificateKeyGroupNotPermitted skips as root; build as a Nix build user" >&2
    return 1
  fi
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
  refuse_root
  local status=0

  # require_pass names a test that failed, skipped or is missing: each leaves
  # its PASS line out of the log. The status catches a package that fails
  # after its tests printed PASS, such as a panic in a goroutine a test left
  # behind or a TestMain that fails after m.Run().
  go test -count=1 -v \
    -run '^(TestUpdateTLSCertificateKeyMode|TestUpdateTLSCertificateKeyGroupNotPermitted)$' \
    ./internal/certificates/ 2>&1 | tee "$TMPDIR/patch-0001.log" || status=1

  require_pass "$TMPDIR/patch-0001.log" TestUpdateTLSCertificateKeyMode
  require_pass "$TMPDIR/patch-0001.log" TestUpdateTLSCertificateKeyGroupNotPermitted

  if [ "$status" -ne 0 ]; then
    echo "patch-checks: go test exited non-zero after its tests passed (see $TMPDIR/patch-0001.log)" >&2
    return 1
  fi
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
