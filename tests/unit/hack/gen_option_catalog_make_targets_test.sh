#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the gen-option-catalogs and verify-option-catalogs Makefile targets
# propagate a failing hack/gen-option-catalog.sh run. Both recipes are nested
# shell loops, and the Makefile sets no SHELL and no .SHELLFLAGS, so they run
# under `/bin/sh -c` without -e: the exit status of a for loop is the status of
# its last command, so without an explicit `|| exit 1` only the final iteration
# decides the target's result. verify-option-catalogs is the only drift
# detection these committed catalogs have (it is deliberately wired into no CI
# job), so a masked failure means a stale catalog is committed against a clean
# run.
#
# The Makefile is copied into a scratch tree next to a releases/ fixture and a
# recording hack/gen-option-catalog.sh stub, so no docker, no network and no
# service image is involved.
#
# Lives under tests/unit/hack/ rather than tests/unit/Makefile/ because that is
# where the `make test-shell` glob picks tests up.
#
# Usage: bash tests/unit/hack/gen_option_catalog_make_targets_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
MAKEFILE="$PROJECT_ROOT/Makefile"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_tree <dir>
# Builds a scratch tree holding the real Makefile, two release directories the
# recipes' outer loop iterates over, and a hack/gen-option-catalog.sh stub that
# appends "<service> <release>" to $GEN_LOG for every call and exits 1 when the
# call matches $FAIL_SERVICE/$FAIL_RELEASE.
make_tree() {
  local dir="$1"
  mkdir -p "$dir/hack" "$dir/releases/2025.2" "$dir/releases/2026.1"
  cp "$MAKEFILE" "$dir/Makefile"

  cat > "$dir/hack/gen-option-catalog.sh" <<'STUB'
#!/bin/bash
# Test stub. Records the (service, release) pair the recipe asked for and
# fails only for the configured pair, so a masked failure shows up as a
# zero exit of the target plus a log that ran past the failing call.
if [ "$1" = "--check" ]; then
  shift
fi
echo "$1 $2" >> "$GEN_LOG"
if [ "$1" = "$FAIL_SERVICE" ] && [ "$2" = "$FAIL_RELEASE" ]; then
  echo "stub: $1 $2 drifted" >&2
  exit 1
fi
exit 0
STUB
  chmod +x "$dir/hack/gen-option-catalog.sh"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

# run_target <target> <fail-service> <fail-release>
# Runs <target> in a fresh scratch tree and echoes "<exit-code>|<last log line>".
run_target() {
  local target="$1" fail_service="$2" fail_release="$3"
  local dir log exit_code=0
  # The file runs without -e, so an unchecked failure would continue with an
  # empty directory name: make_tree would copy the Makefile to /Makefile and
  # plant /hack/gen-option-catalog.sh, the very path this repo invokes. The
  # sentinel below matches no assertion, so the caller reports red.
  dir="$(mktemp -d)" || {
    echo "mktemp -d could not create a scratch tree" >&2
    printf '1|mktemp-failed|0\n'
    return 1
  }
  log="$dir/calls.log"
  make_tree "$dir"
  : > "$log"

  GEN_LOG="$log" FAIL_SERVICE="$fail_service" FAIL_RELEASE="$fail_release" \
    make -C "$dir" "$target" >/dev/null 2>&1 || exit_code=$?

  printf '%s|%s|%s\n' "$exit_code" "$(tail -n 1 "$log")" "$(wc -l < "$log" | tr -d ' ')"
  rm -rf "$dir"
}

test_verify_target_reports_a_drifted_catalog() {
  echo "Test: verify-option-catalogs fails on a non-final drifted catalog"
  # The masking case: neutron 2025.2 drifts while the final iteration
  # (neutron 2026.1) passes.
  local result
  result="$(run_target verify-option-catalogs neutron 2025.2)"

  assert_nonzero_exit "the target exits non-zero" "$(echo "$result" | cut -d'|' -f1)"
  assert_eq "the loop stops at the failing catalog" \
    "neutron 2025.2" "$(echo "$result" | cut -d'|' -f2)"
}

test_gen_target_reports_a_failed_regeneration() {
  echo "Test: gen-option-catalogs fails on a non-final failed regeneration"
  local result
  result="$(run_target gen-option-catalogs keystone 2025.2)"

  assert_nonzero_exit "the target exits non-zero" "$(echo "$result" | cut -d'|' -f1)"
  assert_eq "the loop stops at the failing catalog" \
    "keystone 2025.2" "$(echo "$result" | cut -d'|' -f2)"
}

test_clean_run_covers_every_pair() {
  echo "Test: a clean verify-option-catalogs run covers every release/service pair"
  # Guards the propagation fix against the opposite regression: an `|| exit 1`
  # that fires on a passing call would truncate the matrix silently.
  local result
  result="$(run_target verify-option-catalogs none none)"

  assert_eq "the target exits 0" "0" "$(echo "$result" | cut -d'|' -f1)"
  assert_eq "both releases times all five services ran" \
    "10" "$(echo "$result" | cut -d'|' -f3)"
}

test_verify_target_reports_a_drifted_catalog
test_gen_target_reports_a_failed_regeneration
test_clean_run_covers_every_pair

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
