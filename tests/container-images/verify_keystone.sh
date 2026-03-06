#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify keystone container image meets requirements (CC-0006 REQ-003, REQ-009)
# Usage: bash tests/container-images/verify_keystone.sh [image_name]
# Default image: c5c3/keystone:28.0.0
# Requires: Docker daemon running

set -euo pipefail

IMAGE="${1:-c5c3/keystone:28.0.0}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# --- Test 1: keystone-manage --version outputs version and exits 0 ---
test_keystone_manage_version() {
  echo "Test: keystone-manage --version succeeds"
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" keystone-manage --version 2>&1) || exit_code=$?

  assert_eq "keystone-manage --version exits 0" "0" "$exit_code"

  # Version string should be non-empty
  if [ -n "$version" ]; then
    echo "  PASS: version output is non-empty ($version)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: version output is empty"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 2: runs as openstack user ---
test_runs_as_openstack_user() {
  echo "Test: container runs as openstack user"
  local whoami_output exit_code=0
  whoami_output=$(docker run --rm "$IMAGE" whoami 2>&1) || exit_code=$?

  assert_eq "whoami exits 0" "0" "$exit_code"
  assert_eq "whoami outputs openstack" "openstack" "$whoami_output"
}

# --- Test 3: no build tools in final image ---
test_no_build_tools_in_final_image() {
  echo "Test: no build tools in final image"

  # gcc should not be present
  local gcc_exit=0
  docker run --rm "$IMAGE" which gcc > /dev/null 2>&1 || gcc_exit=$?
  assert_nonzero_exit "gcc not found" "$gcc_exit"

  # python3-dev should not be installed
  local pydev_exit=0
  docker run --rm "$IMAGE" dpkg -l python3-dev > /dev/null 2>&1 || pydev_exit=$?
  assert_nonzero_exit "python3-dev not installed" "$pydev_exit"

  # uv should not be present in the final image
  local uv_exit=0
  docker run --rm "$IMAGE" which uv > /dev/null 2>&1 || uv_exit=$?
  assert_nonzero_exit "uv not found" "$uv_exit"
}

# --- Test 4: runtime apt packages are installed ---
test_runtime_apt_packages_installed() {
  echo "Test: runtime apt packages are installed"
  local dpkg_output exit_code=0
  dpkg_output=$(docker run --rm "$IMAGE" dpkg -l 2>&1) || exit_code=$?

  assert_eq "dpkg -l exits 0" "0" "$exit_code"
  for pkg in libapache2-mod-wsgi-py3 libldap-2.5-0 libsasl2-2 libxml2; do
    assert_contains "$pkg is installed" "$dpkg_output" "$pkg"
  done
}

# --- Run all tests ---
echo "=== keystone container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_keystone_manage_version
echo ""
test_runs_as_openstack_user
echo ""
test_no_build_tools_in_final_image
echo ""
test_runtime_apt_packages_installed
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
