#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify tempest container image meets requirements
# Usage: bash tests/container-images/verify_tempest.sh [image_name]
# Default image: c5c3/tempest:46.3.0
# Requires: Docker daemon running

set -euo pipefail

IMAGE="${1:-c5c3/tempest:46.3.0}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# --- Test 1: tempest --version outputs version and exits 0 ---
test_tempest_version() {
  echo "Test: tempest --version succeeds"
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" tempest --version 2>&1) || exit_code=$?

  assert_eq "tempest --version exits 0" "0" "$exit_code"

  assert_not_empty "version output is non-empty" "$version"
}

# --- Test 2: keystone-tempest-plugin is importable ---
test_keystone_tempest_plugin_importable() {
  echo "Test: keystone-tempest-plugin is importable"
  local exit_code=0
  docker run --rm "$IMAGE" python3 -c 'import keystone_tempest_plugin' > /dev/null 2>&1 || exit_code=$?

  assert_eq "import keystone_tempest_plugin exits 0" "0" "$exit_code"
}

# --- Test 3: openstack CLI is available (used by the E2E verify Job) ---
test_openstack_cli_available() {
  echo "Test: openstack --version succeeds"
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" openstack --version 2>&1) || exit_code=$?

  assert_eq "openstack --version exits 0" "0" "$exit_code"

  assert_not_empty "openstack version output is non-empty" "$version"
}

# --- Test 4: subunit2junitxml is available on PATH ---
test_subunit2junitxml_available() {
  echo "Test: subunit2junitxml is available"
  local path_output exit_code=0
  path_output=$(docker run --rm "$IMAGE" which subunit2junitxml 2>&1) || exit_code=$?

  assert_eq "which subunit2junitxml exits 0" "0" "$exit_code"
  assert_not_empty "subunit2junitxml path is non-empty" "$path_output"
}

# --- Test 5: runs as openstack user ---
test_runs_as_openstack_user() {
  echo "Test: container runs as openstack user"
  local whoami_output exit_code=0
  whoami_output=$(docker run --rm "$IMAGE" whoami 2>&1) || exit_code=$?

  assert_eq "whoami exits 0" "0" "$exit_code"
  assert_eq "whoami outputs openstack" "openstack" "$whoami_output"
}

# --- Test 6: no build tools in final image ---
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

# --- Test 7: osc-placement registers its openstack CLI commands ---
test_osc_placement_cli_registered() {
  echo "Test: osc-placement registers its openstack CLI commands"
  # osc-placement is installed for the `openstack resource class list`
  # round-trip, which needs its openstack.cli.extension entry points to
  # resolve — importing the module would only prove the wheel is on sys.path.
  # --help builds the command without contacting a cloud, so no credentials
  # are needed. cliff prints "Unknown command" and can still exit 0 for an
  # unregistered command, so assert on the rendered usage line too.
  local help_output exit_code=0
  help_output=$(docker run --rm "$IMAGE" openstack resource class list --help 2>&1) || exit_code=$?

  assert_eq "openstack resource class list --help exits 0" "0" "$exit_code"
  assert_contains "resource class list is a registered command" "$help_output" \
    "usage: openstack resource class list"
}

# --- Test 8: barbican-tempest-plugin is importable ---
test_barbican_tempest_plugin_importable() {
  echo "Test: barbican-tempest-plugin is importable"
  local exit_code=0
  docker run --rm "$IMAGE" python3 -c 'import barbican_tempest_plugin' > /dev/null 2>&1 || exit_code=$?

  assert_eq "import barbican_tempest_plugin exits 0" "0" "$exit_code"
}

# --- Run all tests ---
echo "=== tempest container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_tempest_version
echo ""
test_keystone_tempest_plugin_importable
echo ""
test_openstack_cli_available
echo ""
test_subunit2junitxml_available
echo ""
test_runs_as_openstack_user
echo ""
test_no_build_tools_in_final_image
echo ""
test_osc_placement_cli_registered
echo ""
test_barbican_tempest_plugin_importable
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
