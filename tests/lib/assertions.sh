#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Shared shell test assertion helpers
# Source this file from test scripts after defining PASS=0 and FAIL=0.

assert_eq() {
  local description="$1" expected="$2" actual="$3"
  if [ "$expected" = "$actual" ]; then
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $description"
    echo "    expected: $expected"
    echo "    actual:   $actual"
    FAIL=$((FAIL + 1))
  fi
}

assert_not_empty() {
  local description="$1" value="$2"
  if [ -n "$value" ]; then
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $description (expected non-empty value)"
    FAIL=$((FAIL + 1))
  fi
}

assert_contains() {
  local description="$1" haystack="$2" needle="$3"
  if [[ "$haystack" == *"$needle"* ]]; then
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $description"
    echo "    expected to contain: $needle"
    echo "    actual: $haystack"
    FAIL=$((FAIL + 1))
  fi
}

assert_starts_with() {
  local description="$1" actual="$2" prefix="$3"
  if [[ "$actual" == "$prefix"* ]]; then
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $description"
    echo "    expected to start with: $prefix"
    echo "    actual: $actual"
    FAIL=$((FAIL + 1))
  fi
}

assert_nonzero_exit() {
  local description="$1" exit_code="$2"
  if [ "$exit_code" -ne 0 ]; then
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $description (expected non-zero exit, got 0)"
    FAIL=$((FAIL + 1))
  fi
}

assert_file_contains() {
  local description="$1" file="$2" pattern="$3"
  if grep -q "$pattern" "$file"; then
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $description (pattern '$pattern' not found in $file)"
    FAIL=$((FAIL + 1))
  fi
}

assert_gte() {
  local description="$1" actual="$2" expected_min="$3"
  if [[ "$actual" -ge "$expected_min" ]]; then
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $description"
    echo "    expected: >= $expected_min"
    echo "    actual:   $actual"
    FAIL=$((FAIL + 1))
  fi
}

assert_not_contains() {
  local description="$1" haystack="$2" needle="$3"
  if [[ "$haystack" != *"$needle"* ]]; then
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $description"
    echo "    expected to NOT contain: $needle"
    echo "    actual: $haystack"
    FAIL=$((FAIL + 1))
  fi
}

assert_file_not_contains() {
  local description="$1" file="$2" pattern="$3"
  if ! grep -q "$pattern" "$file"; then
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $description (pattern '$pattern' unexpectedly found in $file)"
    FAIL=$((FAIL + 1))
  fi
}
