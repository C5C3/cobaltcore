#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify venv-builder container image meets requirements (CC-0006 REQ-002, REQ-009)
# Usage: bash tests/container-images/verify_venv_builder.sh [image_name]
# Default image: c5c3/venv-builder:3.12-noble
# Requires: Docker daemon running

set -euo pipefail

IMAGE="${1:-c5c3/venv-builder:3.12-noble}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# --- Test 1: uv version is pinned at 0.6.3 ---
test_uv_version_is_pinned() {
  echo "Test: uv version is 0.6.3"
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" uv --version 2>&1) || exit_code=$?

  assert_eq "uv --version exits 0" "0" "$exit_code"
  assert_contains "uv version is 0.6.3" "$version" "0.6.3"
}

# --- Test 2: virtualenv exists at /var/lib/openstack ---
test_virtualenv_exists() {
  echo "Test: virtualenv exists at /var/lib/openstack"
  local exit_code=0
  docker run --rm "$IMAGE" test -x /var/lib/openstack/bin/python3 || exit_code=$?

  assert_eq "/var/lib/openstack/bin/python3 is executable" "0" "$exit_code"
}

# --- Test 3: common packages are installed ---
test_common_packages_installed() {
  echo "Test: common packages are installed in virtualenv"
  local pip_list exit_code=0
  pip_list=$(docker run --rm "$IMAGE" /var/lib/openstack/bin/pip list 2>&1) || exit_code=$?

  assert_eq "pip list exits 0" "0" "$exit_code"
  assert_contains "cryptography installed" "$pip_list" "cryptography"
  assert_contains "pymysql installed" "$pip_list" "PyMySQL"
  assert_contains "python-memcached installed" "$pip_list" "python-memcached"
  assert_contains "uwsgi installed" "$pip_list" "uWSGI"
}

# --- Run all tests ---
echo "=== venv-builder container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_uv_version_is_pinned
echo ""
test_virtualenv_exists
echo ""
test_common_packages_installed
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
