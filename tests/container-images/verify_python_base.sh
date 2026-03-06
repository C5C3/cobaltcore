#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify python-base container image meets requirements (CC-0006 REQ-001, REQ-009)
# Usage: bash tests/container-images/verify_python_base.sh [image_name]
# Default image: c5c3/python-base:3.12-noble
# Requires: Docker daemon running

set -euo pipefail

IMAGE="${1:-c5c3/python-base:3.12-noble}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# --- Test 1: python3 is available and shows 3.12.x ---
test_python3_available() {
  echo "Test: python3 is available"
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" python3 --version 2>&1) || exit_code=$?

  assert_eq "python3 --version exits 0" "0" "$exit_code"
  assert_contains "python version is 3.12.x" "$version" "Python 3.12"
}

# --- Test 2: openstack user UID/GID ---
test_openstack_user_uid_gid() {
  echo "Test: openstack user has UID/GID 42424"
  local id_output exit_code=0
  id_output=$(docker run --rm "$IMAGE" id openstack 2>&1) || exit_code=$?

  assert_eq "id openstack exits 0" "0" "$exit_code"
  assert_contains "uid is 42424" "$id_output" "uid=42424(openstack)"
  assert_contains "gid is 42424" "$id_output" "gid=42424(openstack)"
}

# --- Test 3: PATH includes venv bin ---
test_path_includes_venv_bin() {
  echo "Test: PATH starts with /var/lib/openstack/bin"
  local path_output exit_code=0
  path_output=$(docker run --rm "$IMAGE" sh -c 'echo $PATH' 2>&1) || exit_code=$?

  assert_eq "echo PATH exits 0" "0" "$exit_code"
  assert_starts_with "PATH starts with /var/lib/openstack/bin" "$path_output" "/var/lib/openstack/bin"
}

# --- Run all tests ---
echo "=== python-base container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_python3_available
echo ""
test_openstack_user_uid_gid
echo ""
test_path_includes_venv_bin
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
