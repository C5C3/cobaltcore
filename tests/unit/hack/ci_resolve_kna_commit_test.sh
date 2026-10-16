#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-resolve-kna-commit.sh reads the rev key of
# images/kvm-node-agent/source.json:
#   - the committed pin resolves to a 40-character commit, and a well-formed fixture
#     prints the commit;
#   - a missing file, invalid JSON, a missing "rev" key, a 39-character commit
#     and a PATH without jq each exit 1 with an ::error:: annotation that
#     names the fault and print nothing on stdout.
#
# Follows the project-native bash test pattern (tests/lib/assertions.sh).
#
# Usage: bash tests/unit/hack/ci_resolve_kna_commit_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
RESOLVE_SH="$PROJECT_ROOT/hack/ci-resolve-kna-commit.sh"
PIN="$PROJECT_ROOT/images/kvm-node-agent/source.json"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# run_resolve <pin file>
# Runs the resolver against <pin file>, or against its own default when
# <pin file> is "-" (KNA_SOURCE left unset). Stores stdout in STDOUT, stderr in
# STDERR and the exit status in RC.
run_resolve() {
  RC=0
  if [ "$1" = "-" ]; then
    STDOUT="$(unset KNA_SOURCE; bash "$RESOLVE_SH" 2>"$TMP_DIR/stderr")" || RC=$?
  else
    STDOUT="$(KNA_SOURCE="$1" bash "$RESOLVE_SH" 2>"$TMP_DIR/stderr")" || RC=$?
  fi
  STDERR="$(cat "$TMP_DIR/stderr")"
}

# fixture <name> <content> writes <content> to a file under TMP_DIR and
# prints its path.
fixture() {
  printf '%s\n' "$2" >"$TMP_DIR/$1"
  echo "$TMP_DIR/$1"
}

# assert_fails <description> <expected ::error:: line>
# Asserts the last run exited 1, printed <expected> on stderr and nothing on
# stdout.
assert_fails() {
  assert_eq "$1 exits 1" "1" "$RC"
  assert_eq "$1 prints the annotation" "$2" "$STDERR"
  assert_eq "$1 prints nothing on stdout" "" "$STDOUT"
}

# ---------------------------------------------------------------------------
# Test 1: the committed pin resolves
# ---------------------------------------------------------------------------
test_committed_pin() {
  echo "Test: the committed pin resolves to a 40-character commit"

  run_resolve -
  assert_eq "resolver exits 0 on the committed pin" "0" "$RC"
  assert_eq "resolver prints the pinned value" "$(jq -r '.rev' "$PIN")" "$STDOUT"
  if [[ "$STDOUT" =~ ^[0-9a-f]{40}$ ]]; then
    assert_eq "the value is a 40-character commit" "yes" "yes"
  else
    assert_eq "the value is a 40-character commit" "yes" "no: '$STDOUT'"
  fi
}

# ---------------------------------------------------------------------------
# Test 2: a well-formed fixture prints its value
# ---------------------------------------------------------------------------
test_valid_fixture() {
  echo "Test: a well-formed pin prints the commit"

  run_resolve "$(fixture valid.json '{"rev": "1e4e4b8e7050bbfa2d5a990aa21b849ba7b51700", "hash": "sha256-x", "vendorHash": "sha256-y"}')"
  assert_eq "resolver exits 0 on a valid pin" "0" "$RC"
  assert_eq "resolver prints the commit" "1e4e4b8e7050bbfa2d5a990aa21b849ba7b51700" "$STDOUT"
  assert_eq "resolver prints nothing on stderr" "" "$STDERR"
}

# ---------------------------------------------------------------------------
# Test 3: a missing file fails and names the path
# ---------------------------------------------------------------------------
test_missing_file() {
  echo "Test: a missing pin file exits 1 and names the path"

  run_resolve "$TMP_DIR/missing.json"
  assert_fails "a missing file" "::error::kna pin not found: $TMP_DIR/missing.json"
}

# ---------------------------------------------------------------------------
# Test 4: invalid JSON fails
# ---------------------------------------------------------------------------
test_invalid_json() {
  echo "Test: a pin that is not valid JSON exits 1"

  local pin
  pin="$(fixture invalid.json '{')"
  run_resolve "$pin"
  assert_fails "invalid JSON" "::error::$pin is not valid JSON"
}

# ---------------------------------------------------------------------------
# Test 5: a pin without the key fails
# ---------------------------------------------------------------------------
test_missing_key() {
  echo "Test: a pin without \"rev\" exits 1"

  local pin
  pin="$(fixture nokey.json '{"hash": "sha256-x"}')"
  run_resolve "$pin"
  assert_fails "a missing key" "::error::no \"rev\" in $pin"
}

# ---------------------------------------------------------------------------
# Test 6: a malformed value fails and names it
# ---------------------------------------------------------------------------
test_malformed_value() {
  echo "Test: a 39-character commit exits 1 and names the value"

  local pin
  pin="$(fixture malformed.json '{"rev": "0123456789abcdef0123456789abcdef0123456"}')"
  run_resolve "$pin"
  assert_fails "a malformed value" \
    "::error::\"rev\" in $pin is not a 40-character commit: '0123456789abcdef0123456789abcdef0123456'"
}

# ---------------------------------------------------------------------------
# Test 7: a PATH without jq fails and names jq
# ---------------------------------------------------------------------------
test_missing_jq() {
  echo "Test: a PATH without jq exits 1 and names jq"

  local stubs bash_bin
  stubs="$(mktemp -d "$TMP_DIR/stubs.XXXXXX")"
  ln -s "$(command -v dirname)" "$stubs/dirname"
  bash_bin="$(command -v bash)"
  RC=0
  STDOUT="$(PATH="$stubs" "$bash_bin" "$RESOLVE_SH" 2>"$TMP_DIR/stderr")" || RC=$?
  STDERR="$(cat "$TMP_DIR/stderr")"
  assert_fails "a PATH without jq" "::error::jq not found on PATH"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if ! command -v jq >/dev/null 2>&1; then
  echo "  SKIP: jq not installed (7 tests skipped)"
  SKIP=$((SKIP + 7))
else
  test_committed_pin
  test_valid_fixture
  test_missing_file
  test_invalid_json
  test_missing_key
  test_malformed_value
  test_missing_jq
fi

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
