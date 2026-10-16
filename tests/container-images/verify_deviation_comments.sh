#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify DEVIATION comments are present in the Nix image expressions
# Usage: bash tests/container-images/verify_deviation_comments.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

PASS=0
FAIL=0

# Service images built by Nix: each images/<svc>/image.nix carries the
# DEVIATION comment and points at the full paragraph in
# nix/lib/service-image.nix, which builds them all on the user entry
# nix/lib/fhs-layout.nix writes. nova-compute is built on the same library
# without a source-refs.yaml key of its own, so it has its own function. ovn
# and backup-shifter carry the same decision but are built by Nix without
# service-image.nix: their functions check the comment in
# images/<name>/image.nix and the user entry nix/lib/fhs-layout.nix writes.
# libvirt keeps root and creates no user at all, and has its own function
# too. The openstack-hypervisor-operator image
# is a static Go binary that Nix builds on the distroless-equivalent image of
# nix/lib/static-image.nix; it runs as uid 65532 and creates no user either,
# and has its own function. The kvm-node-agent image is built the same way
# but runs as root, and has its own function as well.
SERVICES="keystone horizon glance placement barbican neutron cinder nova"

# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# Print the DEVIATION comment block: the '# DEVIATION' line plus the comment
# lines that follow it, stopping at the first non-comment line. Scoping the
# rationale assertion to this block matters — 'openstack' also appears in
# /var/lib/openstack paths and in the user entries, so grepping the whole
# file would pass no matter what the comment says.
deviation_block() {
  awk '/^# DEVIATION/{f=1} f&&!/^#/{exit} f' "$1"
}

# --- Test 2: every Nix-built service image has a DEVIATION comment ---
test_service_deviation_comment() {
  local service="$1"
  echo "Test: $service image.nix has DEVIATION comment"

  local nix="$PROJECT_ROOT/images/${service}/image.nix"

  assert_file_contains "$service/image.nix contains DEVIATION comment" "$nix" "# DEVIATION"
  assert_contains "DEVIATION comment references generic user vs per-service" \
    "$(deviation_block "$nix")" "openstack"
}

# --- Test 2b: the library the service images share carries the rationale ---
test_service_image_library_deviation_comment() {
  echo "Test: nix/lib/service-image.nix has DEVIATION comment"

  local nix="$PROJECT_ROOT/nix/lib/service-image.nix"

  assert_file_contains "service-image.nix contains DEVIATION comment" "$nix" "# DEVIATION"
  assert_contains "DEVIATION comment references the generic openstack user" \
    "$(deviation_block "$nix")" "openstack"
}

# --- Test 2c: nova-compute is built on the same library ---
test_nova_compute_deviation_comment() {
  echo "Test: nova-compute image.nix has DEVIATION comment"

  local nix="$PROJECT_ROOT/images/nova-compute/image.nix"

  assert_file_contains "nova-compute/image.nix contains DEVIATION comment" "$nix" "# DEVIATION"
  assert_contains "DEVIATION comment references generic user vs per-service" \
    "$(deviation_block "$nix")" "openstack"
}

# --- Test 3: the Nix-built ovn image carries the generic user itself ---
test_ovn_deviation_comment() {
  echo "Test: ovn image.nix has DEVIATION comment (user written by the FHS façade)"

  local nix="$PROJECT_ROOT/images/ovn/image.nix"
  local fhs="$PROJECT_ROOT/nix/lib/fhs-layout.nix"

  assert_file_contains "ovn/image.nix contains DEVIATION comment" "$nix" "# DEVIATION"
  assert_contains "DEVIATION comment references the generic openstack user" \
    "$(deviation_block "$nix")" "openstack"
  assert_file_contains_fixed "fhs-layout.nix writes the openstack user" \
    "$fhs" 'openstack:x:${uid}:${gid}'
}

# --- Test 4: the Nix-built backup-shifter image carries the generic user itself ---
test_backup_shifter_deviation_comment() {
  echo "Test: backup-shifter image.nix has DEVIATION comment (user written by the FHS façade)"

  local nix="$PROJECT_ROOT/images/backup-shifter/image.nix"
  local fhs="$PROJECT_ROOT/nix/lib/fhs-layout.nix"

  assert_file_contains "backup-shifter/image.nix contains DEVIATION comment" "$nix" "# DEVIATION"
  assert_contains "DEVIATION comment references the generic openstack user" \
    "$(deviation_block "$nix")" "openstack"
  assert_file_contains_fixed "fhs-layout.nix writes the openstack user" \
    "$fhs" 'openstack:x:${uid}:${gid}'
}

# --- Test 5: the libvirt image keeps root and creates no user ---
test_libvirt_deviation_comment() {
  echo "Test: libvirt image.nix has DEVIATION comment (root, no openstack user)"

  local nix="$PROJECT_ROOT/images/libvirt/image.nix"

  assert_file_contains "libvirt/image.nix contains DEVIATION comment" "$nix" "# DEVIATION"
  assert_contains "DEVIATION comment says the image keeps root" \
    "$(deviation_block "$nix")" "root"
  assert_contains "DEVIATION comment names the openstack user it does not create" \
    "$(deviation_block "$nix")" "openstack"
  assert_file_not_contains "libvirt/image.nix sets no User" "$nix" '^ *User = '
}

# --- Test 6: the openstack-hypervisor-operator image runs as distroless nonroot ---
test_hvo_deviation_comment() {
  echo "Test: openstack-hypervisor-operator image.nix has DEVIATION comment (distroless, uid 65532)"

  local nix="$PROJECT_ROOT/images/openstack-hypervisor-operator/image.nix"

  assert_file_contains "openstack-hypervisor-operator/image.nix contains DEVIATION comment" \
    "$nix" "# DEVIATION"
  assert_contains "DEVIATION comment names the distroless base" \
    "$(deviation_block "$nix")" "distroless"
  assert_contains "DEVIATION comment names the openstack user it does not create" \
    "$(deviation_block "$nix")" "openstack"
  # The image takes the default user of static-image.nix, the base's nonroot.
  assert_file_not_contains "openstack-hypervisor-operator/image.nix sets no user of its own" \
    "$nix" '^ *user = '
  assert_file_contains_fixed "static-image.nix names nonroot 65532" \
    "$PROJECT_ROOT/nix/lib/static-image.nix" 'nonroot = "65532";'
  assert_file_contains_fixed "static-image.nix runs as nonroot by default" \
    "$PROJECT_ROOT/nix/lib/static-image.nix" 'user ? "${nonroot}:${nonroot}",'
}

# --- Test 7: the kvm-node-agent image runs as root on distroless ---
test_kna_deviation_comment() {
  echo "Test: kvm-node-agent image.nix has DEVIATION comment (distroless, root)"

  local nix="$PROJECT_ROOT/images/kvm-node-agent/image.nix"

  assert_file_contains "kvm-node-agent/image.nix contains DEVIATION comment" \
    "$nix" "# DEVIATION"
  assert_contains "DEVIATION comment names the distroless base" \
    "$(deviation_block "$nix")" "distroless"
  assert_contains "DEVIATION comment names the openstack user it does not create" \
    "$(deviation_block "$nix")" "openstack"
  assert_contains "DEVIATION comment says why the agent runs as root" \
    "$(deviation_block "$nix")" "root"
  assert_file_contains_fixed "kvm-node-agent/image.nix runs as 0:0" \
    "$nix" 'user = "0:0";'
}

# --- Run all tests ---
echo "=== DEVIATION comment verification tests ==="
echo ""
for service in $SERVICES; do
  echo ""
  test_service_deviation_comment "$service"
done
echo ""
test_service_image_library_deviation_comment
echo ""
test_nova_compute_deviation_comment
echo ""
test_ovn_deviation_comment
echo ""
test_backup_shifter_deviation_comment
echo ""
test_libvirt_deviation_comment
echo ""
test_hvo_deviation_comment
echo ""
test_kna_deviation_comment
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
