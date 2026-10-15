#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify keystone container image meets requirements
# Usage: bash tests/container-images/verify_keystone.sh [image_name]
# Default image: c5c3/keystone:29.0.0
# Requires: Docker daemon running

set -euo pipefail

IMAGE="${1:-c5c3/keystone:29.0.0}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"
# shellcheck source=tests/lib/build_tools.sh
source "$SCRIPT_DIR/../lib/build_tools.sh"
# shellcheck source=tests/lib/uwsgi.sh
source "$SCRIPT_DIR/../lib/uwsgi.sh"

# --- Test 1: keystone-manage --version outputs version and exits 0 ---
test_keystone_manage_version() {
  echo "Test: keystone-manage --version succeeds"
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" keystone-manage --version 2>&1) || exit_code=$?

  assert_eq "keystone-manage --version exits 0" "0" "$exit_code"

  assert_not_empty "version output is non-empty" "$version"
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
  assert_no_build_tools
}

# --- Test 4: the runtime libraries and entry points work ---
test_runtime_libraries_present() {
  echo "Test: the runtime libraries and entry points work"
  # python-ldap links libldap and libsasl, which its closure carries; the
  # ldap extra of releases/<release>/extra-packages.yaml installs it. uWSGI
  # loads keystone-wsgi-public (operators/keystone, --wsgi-file). Stderr is
  # captured, so a missing library is named on failure.
  local output exit_code=0
  output=$(docker run --rm "$IMAGE" python -c 'import ldap' 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $output"
  assert_eq "import ldap exits 0" "0" "$exit_code"

  exit_code=0
  docker run --rm "$IMAGE" sh -c 'test -x /var/lib/openstack/bin/keystone-wsgi-public' || exit_code=$?
  assert_eq "keystone-wsgi-public is executable" "0" "$exit_code"

  exit_code=0
  output=$(docker run --rm "$IMAGE" uwsgi --version 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $output"
  assert_eq "uwsgi --version exits 0" "0" "$exit_code"
  assert_uwsgi_imports keystone
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
test_runtime_libraries_present
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
