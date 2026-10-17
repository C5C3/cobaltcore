#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify keystone-federation-proxy container image meets requirements
# Usage: bash tests/container-images/verify_keystone_federation_proxy.sh [image_name]
# Default image: keystone-federation-proxy
# Requires: Docker daemon running, jq

set -euo pipefail

IMAGE="${1:-keystone-federation-proxy}"
BASE_CONF=/etc/keystone-federation-proxy/httpd-base.conf

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"
# shellcheck source=tests/lib/build_tools.sh
source "$SCRIPT_DIR/../lib/build_tools.sh"

# The mod_auth_openidc release images/keystone-federation-proxy/source.json
# pins, without its leading 'v' (v2.4.20.3 becomes 2.4.20.3).
EXPECTED_OPENIDC=$(jq -r '.version | ltrimstr("v")' "$SCRIPT_DIR/../../images/keystone-federation-proxy/source.json")

# assert_module_loaded <module> checks that apache2 -M lists <module> as a
# shared module of httpd-base.conf.
assert_module_loaded() {
  local output exit_code=0
  output=$(docker run --rm "$IMAGE" apache2 -M -f "$BASE_CONF" 2>&1) || exit_code=$?

  assert_eq "apache2 -M exits 0" "0" "$exit_code"
  assert_contains "$1 is loaded" "$output" "$1 (shared)"
}

# --- Test 1: apache2 -v outputs version and exits 0 ---
test_apache_version() {
  echo "Test: apache2 -v succeeds"
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" apache2 -v 2>&1) || exit_code=$?

  assert_eq "apache2 -v exits 0" "0" "$exit_code"
  assert_contains "version output names Apache" "$version" "Apache"
}

# --- Test 2: httpd-base.conf loads mod_auth_openidc ---
test_mod_auth_openidc_loaded() {
  echo "Test: apache2 -M lists mod_auth_openidc"
  assert_module_loaded auth_openidc_module
}

# --- Test 3: the static scaffold passes the syntax check as the container user ---
test_base_config_syntax() {
  echo "Test: httpd-base.conf passes apache2 -t"
  local output exit_code=0
  output=$(docker run --rm "$IMAGE" apache2 -t -f "$BASE_CONF" 2>&1) || exit_code=$?

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

# --- Test 6: httpd-base.conf loads mod_auth_mellon (SAML phase) ---
test_mellon_module_loaded() {
  echo "Test: apache2 -M lists mod_auth_mellon"
  assert_module_loaded auth_mellon_module
}

# --- Test 7: the server starts as the pod starts it ---
# The read-only root and the /tmp mount of the keystone pod. apache2 -t
# passes without mod_unixd, but the server then refuses to start (AH00136);
# a server still running when the timeout hits exits 124.
test_starts_on_a_read_only_root() {
  echo "Test: apache2 -DFOREGROUND serves on a read-only root"
  local output exit_code=0
  output=$(docker run --rm --read-only --tmpfs /tmp "$IMAGE" \
    timeout 5 apache2 -DFOREGROUND -f "$BASE_CONF" 2>&1) || exit_code=$?

  assert_eq "apache2 is still serving after 5 seconds" "124" "$exit_code"
  assert_contains "apache2 reports resuming normal operations" "$output" "AH00489"
}

# --- Test 8: the CA bundle the IdP TLS connections verify against ---
test_ca_bundle_present() {
  echo "Test: the CA bundle is present"
  local exit_code=0
  docker run --rm "$IMAGE" test -s /etc/ssl/certs/ca-certificates.crt || exit_code=$?

  assert_eq "/etc/ssl/certs/ca-certificates.crt is not empty" "0" "$exit_code"
}

# --- Test 9: no compiler, binutils or uv on PATH or in /nix/store ---
test_no_build_tooling() {
  echo "Test: no build tooling in the image"
  assert_no_build_tools
}

# --- Test 10: the loaded mod_auth_openidc is the pinned release ---
# The module embeds "mod_auth_openidc-<version>", the name and version of the
# AC_INIT line of configure.ac, which mod_auth_openidc.nix checks against the
# pin. The module read is the one the LoadModule line of httpd-base.conf names.
test_mod_auth_openidc_version() {
  echo "Test: the loaded mod_auth_openidc is $EXPECTED_OPENIDC"
  local so embedded
  so=$(docker run --rm "$IMAGE" cat "$BASE_CONF" | awk '$2 == "auth_openidc_module" { print $3 }' || true)
  assert_not_empty "httpd-base.conf names the mod_auth_openidc module" "$so"

  embedded=$(docker run --rm "$IMAGE" cat "$so" |
    LC_ALL=C grep -a -o 'mod_auth_openidc-[0-9][0-9.]*[0-9]' | sort -u || true)
  assert_eq "the module embeds the pinned version" "mod_auth_openidc-$EXPECTED_OPENIDC" "$embedded"
}

# --- Test 11: the OCI labels of image.nix ---
# The licence and vendor labels of nix/lib/image-defaults.nix plus the title
# and the description of the image.
test_oci_labels() {
  echo "Test: the image carries its OCI labels"
  local pair key
  for pair in \
    "org.opencontainers.image.title=keystone-federation-proxy" \
    "org.opencontainers.image.licenses=Apache-2.0" \
    "org.opencontainers.image.vendor=SAP SE"; do
    key="${pair%%=*}"
    assert_eq "$key is ${pair#*=}" "${pair#*=}" \
      "$(docker inspect --format "{{ index .Config.Labels \"$key\" }}" "$IMAGE")"
  done
  assert_not_empty "org.opencontainers.image.description is set" \
    "$(docker inspect --format '{{ index .Config.Labels "org.opencontainers.image.description" }}' "$IMAGE")"
}

# --- Run all tests ---
echo "=== keystone-federation-proxy container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_apache_version
echo ""
test_mod_auth_openidc_loaded
echo ""
test_base_config_syntax
echo ""
test_mount_points_exist
echo ""
test_runs_as_openstack_user
echo ""
test_mellon_module_loaded
echo ""
test_starts_on_a_read_only_root
echo ""
test_ca_bundle_present
echo ""
test_no_build_tooling
echo ""
test_mod_auth_openidc_version
echo ""
test_oci_labels
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
