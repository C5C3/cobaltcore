#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify keystone-federation-proxy container image meets requirements
# Usage: bash tests/container-images/verify_keystone_federation_proxy.sh [image_name]
# Default image: keystone-federation-proxy
# Requires: Docker daemon running

set -euo pipefail

IMAGE="${1:-keystone-federation-proxy}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# --- Test 1: apache2 -v outputs version and exits 0 ---
test_apache_version() {
  echo "Test: apache2 -v succeeds"
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" apache2 -v 2>&1) || exit_code=$?

  assert_eq "apache2 -v exits 0" "0" "$exit_code"
  assert_contains "version output names Apache" "$version" "Apache"
}

# --- Test 2: mod_auth_openidc shared object is present ---
test_mod_auth_openidc_present() {
  echo "Test: mod_auth_openidc.so is present"
  local exit_code=0
  docker run --rm "$IMAGE" test -f /usr/lib/apache2/modules/mod_auth_openidc.so || exit_code=$?

  assert_eq "mod_auth_openidc.so exists" "0" "$exit_code"
}

# --- Test 3: the static scaffold passes the syntax check as the container user ---
test_base_config_syntax() {
  echo "Test: httpd-base.conf passes apache2 -t"
  local output exit_code=0
  output=$(docker run --rm "$IMAGE" apache2 -t -f /etc/keystone-federation-proxy/httpd-base.conf 2>&1) || exit_code=$?

  assert_eq "apache2 -t exits 0" "0" "$exit_code"
  assert_contains "syntax check reports OK" "$output" "Syntax OK"
}

# --- Test 4: the conf.d and metadata mount points exist ---
test_mount_points_exist() {
  echo "Test: conf.d and metadata mount points exist"
  local confd_exit=0 metadata_exit=0
  docker run --rm "$IMAGE" test -d /etc/keystone-federation-proxy/conf.d || confd_exit=$?
  docker run --rm "$IMAGE" test -d /etc/keystone-federation-proxy/metadata || metadata_exit=$?

  assert_eq "conf.d directory exists" "0" "$confd_exit"
  assert_eq "metadata directory exists" "0" "$metadata_exit"
}

# --- Test 5: runs as openstack user (UID 42424) ---
test_runs_as_openstack_user() {
  echo "Test: container runs as openstack user"
  local id_output exit_code=0
  id_output=$(docker run --rm "$IMAGE" id 2>&1) || exit_code=$?

  assert_eq "id exits 0" "0" "$exit_code"
  assert_contains "id reports uid 42424 (openstack)" "$id_output" "uid=42424(openstack)"
}

# --- Test 6: mod_auth_mellon is NOT installed (reserved for the SAML phase) ---
test_no_mellon_in_image() {
  echo "Test: mod_auth_mellon not installed"
  local mellon_exit=0
  docker run --rm "$IMAGE" test -f /usr/lib/apache2/modules/mod_auth_mellon.so || mellon_exit=$?
  assert_nonzero_exit "mod_auth_mellon.so not found" "$mellon_exit"
}

# --- Run all tests ---
echo "=== keystone-federation-proxy container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_apache_version
echo ""
test_mod_auth_openidc_present
echo ""
test_base_config_syntax
echo ""
test_mount_points_exist
echo ""
test_runs_as_openstack_user
echo ""
test_no_mellon_in_image
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
