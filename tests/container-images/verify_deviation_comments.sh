#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify DEVIATION comments are present in Dockerfiles (CC-0006 REQ-010)
# Usage: bash tests/container-images/verify_deviation_comments.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

PASS=0
FAIL=0

# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# --- Test 1: python-base has DEVIATION comment ---
test_python_base_deviation_comment() {
  echo "Test: python-base Dockerfile has DEVIATION comment"

  local dockerfile="$PROJECT_ROOT/images/python-base/Dockerfile"

  assert_file_contains "python-base/Dockerfile contains DEVIATION comment" "$dockerfile" "# DEVIATION"
  assert_file_contains "DEVIATION comment references generic openstack user" "$dockerfile" "openstack"
}

# --- Test 2: keystone has DEVIATION comment ---
test_keystone_deviation_comment() {
  echo "Test: keystone Dockerfile has DEVIATION comment"

  local dockerfile="$PROJECT_ROOT/images/keystone/Dockerfile"

  assert_file_contains "keystone/Dockerfile contains DEVIATION comment" "$dockerfile" "# DEVIATION"
  assert_file_contains "DEVIATION comment references generic user vs per-service" "$dockerfile" "openstack"
}

# --- Run all tests ---
echo "=== DEVIATION comment verification tests ==="
echo ""
test_python_base_deviation_comment
echo ""
test_keystone_deviation_comment
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
