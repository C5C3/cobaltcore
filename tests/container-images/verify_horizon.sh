#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify horizon container image meets requirements
# Usage: bash tests/container-images/verify_horizon.sh [image_name]
# Default image: c5c3/horizon:25.5.1
# Requires: Docker daemon running

set -euo pipefail

IMAGE="${1:-c5c3/horizon:25.5.1}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# --- Test 1: openstack_dashboard is importable ---
test_openstack_dashboard_importable() {
  echo "Test: openstack_dashboard imports cleanly"
  local exit_code=0
  docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c "import openstack_dashboard" > /dev/null 2>&1 || exit_code=$?

  assert_eq "import openstack_dashboard exits 0" "0" "$exit_code"
}

# --- Test 2: static assets are pre-built at image-build time ---
test_static_assets_present() {
  echo "Test: static assets are pre-built"

  local asset_count exit_code=0
  asset_count=$(docker run --rm "$IMAGE" \
    sh -c 'find /var/lib/openstack/horizon-static -type f | wc -l' 2>&1 | tr -d '[:space:]') || exit_code=$?

  assert_eq "static root listing exits 0" "0" "$exit_code"
  if [ "${asset_count:-0}" -gt 0 ]; then
    echo "  PASS: /var/lib/openstack/horizon-static contains files ($asset_count)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: /var/lib/openstack/horizon-static is empty"
    FAIL=$((FAIL + 1))
  fi

  # django-compressor offline manifest proves `compress --force` ran.
  local manifest_exit=0
  docker run --rm "$IMAGE" \
    test -f /var/lib/openstack/horizon-static/dashboard/manifest.json > /dev/null 2>&1 || manifest_exit=$?
  assert_eq "offline compression manifest exists" "0" "$manifest_exit"
}

# --- Test 3: runs as openstack user ---
test_runs_as_openstack_user() {
  echo "Test: container runs as openstack user"
  local whoami_output exit_code=0
  whoami_output=$(docker run --rm "$IMAGE" whoami 2>&1) || exit_code=$?

  assert_eq "whoami exits 0" "0" "$exit_code"
  assert_eq "whoami outputs openstack" "openstack" "$whoami_output"
}

# --- Test 4: no build tools in final image ---
test_no_build_tools_in_final_image() {
  echo "Test: no build tools in final image"

  # gcc should not be present
  local gcc_exit=0
  docker run --rm "$IMAGE" which gcc > /dev/null 2>&1 || gcc_exit=$?
  assert_nonzero_exit "gcc not found" "$gcc_exit"

  # python3-dev should not be installed
  local pydev_exit=0
  docker run --rm "$IMAGE" dpkg -s python3-dev > /dev/null 2>&1 || pydev_exit=$?
  assert_nonzero_exit "python3-dev not installed" "$pydev_exit"

  # uv should not be present in the final image
  local uv_exit=0
  docker run --rm "$IMAGE" which uv > /dev/null 2>&1 || uv_exit=$?
  assert_nonzero_exit "uv not found" "$uv_exit"
}

# --- Test 5: uwsgi is runnable (serves openstack_dashboard.wsgi at runtime) ---
test_uwsgi_runnable() {
  echo "Test: uwsgi --version succeeds"
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" /var/lib/openstack/bin/uwsgi --version 2>&1) || exit_code=$?

  assert_eq "uwsgi --version exits 0" "0" "$exit_code"
  assert_not_empty "uwsgi version output is non-empty" "$version"
}

# --- Run all tests ---
echo "=== horizon container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_openstack_dashboard_importable
echo ""
test_static_assets_present
echo ""
test_runs_as_openstack_user
echo ""
test_no_build_tools_in_final_image
echo ""
test_uwsgi_runnable
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
