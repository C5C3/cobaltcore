#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Shell tests for scripts/apply-constraint-overrides.sh
# Usage: bash tests/scripts/test_apply_constraint_overrides.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
SCRIPT_UNDER_TEST="$PROJECT_ROOT/scripts/apply-constraint-overrides.sh"

PASS=0
FAIL=0
TMPDIR_BASE=$(mktemp -d)

cleanup() {
  rm -rf "$TMPDIR_BASE"
}
trap cleanup EXIT

# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# --- Test 1: No override file exits cleanly ---
test_no_override_file_exits_cleanly() {
  echo "Test: no override file exits cleanly"
  local workdir="$TMPDIR_BASE/test1"
  mkdir -p "$workdir/releases/2025.2"
  cat > "$workdir/releases/2025.2/upper-constraints.txt" <<'EOF'
cryptography===44.0.0
oslo.config===9.7.0
EOF
  local original
  original=$(cat "$workdir/releases/2025.2/upper-constraints.txt")

  local exit_code=0
  (cd "$workdir" && bash "$SCRIPT_UNDER_TEST" "2025.2") || exit_code=$?

  assert_eq "exit code is 0" "0" "$exit_code"
  assert_eq "file unchanged" "$original" "$(cat "$workdir/releases/2025.2/upper-constraints.txt")"
}

# --- Test 2: Version replacement updates constraint ---
test_version_replacement_updates_constraint() {
  echo "Test: version replacement updates constraint"
  local workdir="$TMPDIR_BASE/test2"
  mkdir -p "$workdir/overrides/2025.2" "$workdir/releases/2025.2"
  cat > "$workdir/releases/2025.2/upper-constraints.txt" <<'EOF'
cryptography===44.0.0
oslo.config===9.7.0
oslo.messaging===14.9.0
EOF
  echo "cryptography===44.0.1" > "$workdir/overrides/2025.2/constraints.txt"

  local exit_code=0
  (cd "$workdir" && bash "$SCRIPT_UNDER_TEST" "2025.2") || exit_code=$?

  assert_eq "exit code is 0" "0" "$exit_code"
  assert_file_contains "cryptography updated" "$workdir/releases/2025.2/upper-constraints.txt" "^cryptography===44.0.1$"
  assert_file_not_contains "old version gone" "$workdir/releases/2025.2/upper-constraints.txt" "cryptography===44.0.0"
  assert_file_contains "oslo.config untouched" "$workdir/releases/2025.2/upper-constraints.txt" "^oslo.config===9.7.0$"
}

# --- Test 3: Package removal deletes line ---
test_package_removal_deletes_line() {
  echo "Test: package removal deletes line"
  local workdir="$TMPDIR_BASE/test3"
  mkdir -p "$workdir/overrides/2025.2" "$workdir/releases/2025.2"
  cat > "$workdir/releases/2025.2/upper-constraints.txt" <<'EOF'
cryptography===44.0.0
oslo.messaging===14.9.0
oslo.config===9.7.0
EOF
  echo "-oslo.messaging" > "$workdir/overrides/2025.2/constraints.txt"

  local exit_code=0
  (cd "$workdir" && bash "$SCRIPT_UNDER_TEST" "2025.2") || exit_code=$?

  assert_eq "exit code is 0" "0" "$exit_code"
  assert_file_not_contains "oslo.messaging removed" "$workdir/releases/2025.2/upper-constraints.txt" "oslo.messaging"
  assert_file_contains "cryptography untouched" "$workdir/releases/2025.2/upper-constraints.txt" "^cryptography===44.0.0$"
  assert_file_contains "oslo.config untouched" "$workdir/releases/2025.2/upper-constraints.txt" "^oslo.config===9.7.0$"
}

# --- Test 4: Comments and blank lines skipped ---
test_comments_and_blanks_skipped() {
  echo "Test: comments and blank lines skipped"
  local workdir="$TMPDIR_BASE/test4"
  mkdir -p "$workdir/overrides/2025.2" "$workdir/releases/2025.2"
  cat > "$workdir/releases/2025.2/upper-constraints.txt" <<'EOF'
cryptography===44.0.0
oslo.config===9.7.0
EOF
  cat > "$workdir/overrides/2025.2/constraints.txt" <<'EOF'
# This is a comment
   # Indented comment

EOF
  local original
  original=$(cat "$workdir/releases/2025.2/upper-constraints.txt")

  local exit_code=0
  (cd "$workdir" && bash "$SCRIPT_UNDER_TEST" "2025.2") || exit_code=$?

  assert_eq "exit code is 0" "0" "$exit_code"
  assert_eq "file unchanged" "$original" "$(cat "$workdir/releases/2025.2/upper-constraints.txt")"
}

# --- Test 5: Multiple overrides in one run ---
test_multiple_overrides_applied_in_single_run() {
  echo "Test: multiple overrides applied in single run"
  local workdir="$TMPDIR_BASE/test5"
  mkdir -p "$workdir/overrides/2025.2" "$workdir/releases/2025.2"
  cat > "$workdir/releases/2025.2/upper-constraints.txt" <<'EOF'
cryptography===44.0.0
oslo.config===9.7.0
oslo.messaging===14.9.0
keystoneauth1===5.10.0
EOF
  cat > "$workdir/overrides/2025.2/constraints.txt" <<'EOF'
# CVE fix
cryptography===44.0.1
# Remove patched library
-oslo.messaging
# Upgrade keystoneauth1
keystoneauth1===5.11.0
EOF

  local exit_code=0
  (cd "$workdir" && bash "$SCRIPT_UNDER_TEST" "2025.2") || exit_code=$?

  assert_eq "exit code is 0" "0" "$exit_code"
  assert_file_contains "cryptography updated" "$workdir/releases/2025.2/upper-constraints.txt" "^cryptography===44.0.1$"
  assert_file_not_contains "old cryptography removed" "$workdir/releases/2025.2/upper-constraints.txt" "cryptography===44.0.0"
  assert_file_not_contains "oslo.messaging removed" "$workdir/releases/2025.2/upper-constraints.txt" "oslo.messaging"
  assert_file_contains "keystoneauth1 updated" "$workdir/releases/2025.2/upper-constraints.txt" "^keystoneauth1===5.11.0$"
  assert_file_not_contains "old keystoneauth1 removed" "$workdir/releases/2025.2/upper-constraints.txt" "keystoneauth1===5.10.0"
  assert_file_contains "oslo.config untouched" "$workdir/releases/2025.2/upper-constraints.txt" "^oslo.config===9.7.0$"
}

# --- Test 6: Missing constraints file fails with error ---
test_missing_constraints_file_fails() {
  echo "Test: missing constraints file fails with error"
  local workdir="$TMPDIR_BASE/test6"
  mkdir -p "$workdir"
  # Deliberately do NOT create releases/2025.2/upper-constraints.txt

  local exit_code=0
  local output
  output=$(cd "$workdir" && bash "$SCRIPT_UNDER_TEST" "2025.2" 2>&1) || exit_code=$?

  assert_nonzero_exit "exits with non-zero status" "$exit_code"
  assert_contains "error mentions missing file" "$output" "upper-constraints.txt"
}

# --- Test 7: Case-insensitive package matching ---
test_case_insensitive_package_matching() {
  echo "Test: case-insensitive package matching"
  local workdir="$TMPDIR_BASE/test7"
  mkdir -p "$workdir/overrides/2025.2" "$workdir/releases/2025.2"
  cat > "$workdir/releases/2025.2/upper-constraints.txt" <<'EOF'
PyMySQL===1.1.0
oslo.config===9.7.0
EOF
  # Override uses lowercase while constraint file uses mixed case
  echo "pymysql===1.1.1" > "$workdir/overrides/2025.2/constraints.txt"

  local exit_code=0
  (cd "$workdir" && bash "$SCRIPT_UNDER_TEST" "2025.2") || exit_code=$?

  assert_eq "exit code is 0" "0" "$exit_code"
  assert_file_contains "pymysql updated" "$workdir/releases/2025.2/upper-constraints.txt" "^pymysql===1.1.1$"
  assert_file_not_contains "old PyMySQL removed" "$workdir/releases/2025.2/upper-constraints.txt" "PyMySQL===1.1.0"
  assert_file_contains "oslo.config untouched" "$workdir/releases/2025.2/upper-constraints.txt" "^oslo.config===9.7.0$"
}

# --- Run all tests ---
echo "=== apply-constraint-overrides.sh tests ==="
echo ""
test_no_override_file_exits_cleanly
echo ""
test_version_replacement_updates_constraint
echo ""
test_package_removal_deletes_line
echo ""
test_comments_and_blanks_skipped
echo ""
test_multiple_overrides_applied_in_single_run
echo ""
test_missing_constraints_file_fails
echo ""
test_case_insensitive_package_matching
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
