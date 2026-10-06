#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify DEVIATION comments are present in Dockerfiles
# Usage: bash tests/container-images/verify_deviation_comments.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

PASS=0
FAIL=0

# Service images that inherit the generic user instead of creating their own.
# python-base is checked separately: it is where the user is created. ovn and
# backup-shifter create the user themselves because they do not derive from
# python-base, so each has its own function below. libvirt keeps root and
# creates no user at all, and has its own function too. The
# openstack-hypervisor-operator image is a static Go binary on distroless that
# runs as uid 65532 and creates no user either; it has its own function. The
# kvm-node-agent image is a static Go binary on distroless too, but runs as
# root, and has its own function as well.
SERVICES="keystone horizon glance placement barbican neutron cinder nova nova-compute"

# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# Print the DEVIATION comment block: the '# DEVIATION' line plus the comment
# lines that follow it, stopping at the first non-comment line. Scoping the
# rationale assertion to this block matters — 'openstack' also appears in
# /var/lib/openstack paths and USER openstack, so grepping the whole
# Dockerfile would pass no matter what the comment says.
deviation_block() {
  awk '/^# DEVIATION/{f=1} f&&!/^#/{exit} f' "$1"
}

# --- Test 1: python-base has DEVIATION comment ---
test_python_base_deviation_comment() {
  echo "Test: python-base Dockerfile has DEVIATION comment"

  local dockerfile="$PROJECT_ROOT/images/python-base/Dockerfile"

  assert_file_contains "python-base/Dockerfile contains DEVIATION comment" "$dockerfile" "# DEVIATION"
  assert_contains "DEVIATION comment references generic openstack user" \
    "$(deviation_block "$dockerfile")" "openstack"
}

# --- Test 2: every service image has a DEVIATION comment ---
test_service_deviation_comment() {
  local service="$1"
  echo "Test: $service Dockerfile has DEVIATION comment"

  local dockerfile="$PROJECT_ROOT/images/${service}/Dockerfile"

  assert_file_contains "$service/Dockerfile contains DEVIATION comment" "$dockerfile" "# DEVIATION"
  assert_contains "DEVIATION comment references generic user vs per-service" \
    "$(deviation_block "$dockerfile")" "openstack"
}

# --- Test 3: the ovn image creates the generic user itself ---
test_ovn_deviation_comment() {
  echo "Test: ovn Dockerfile has DEVIATION comment (user created locally)"

  local dockerfile="$PROJECT_ROOT/images/ovn/Dockerfile"

  assert_file_contains "ovn/Dockerfile contains DEVIATION comment" "$dockerfile" "# DEVIATION"
  assert_contains "DEVIATION comment references the generic openstack user" \
    "$(deviation_block "$dockerfile")" "openstack"
  assert_file_contains "ovn/Dockerfile creates the user itself (not inherited from python-base)" \
    "$dockerfile" "useradd -u 42424"
}

# --- Test 4: the backup-shifter image creates the generic user itself ---
test_backup_shifter_deviation_comment() {
  echo "Test: backup-shifter Dockerfile has DEVIATION comment (user created locally)"

  local dockerfile="$PROJECT_ROOT/images/backup-shifter/Dockerfile"

  assert_file_contains "backup-shifter/Dockerfile contains DEVIATION comment" "$dockerfile" "# DEVIATION"
  assert_contains "DEVIATION comment references the generic openstack user" \
    "$(deviation_block "$dockerfile")" "openstack"
  assert_file_contains "backup-shifter/Dockerfile creates the user itself (not inherited from python-base)" \
    "$dockerfile" "useradd -u 42424"
}

# --- Test 5: the libvirt image keeps root and creates no user ---
test_libvirt_deviation_comment() {
  echo "Test: libvirt Dockerfile has DEVIATION comment (root, no openstack user)"

  local dockerfile="$PROJECT_ROOT/images/libvirt/Dockerfile"

  assert_file_contains "libvirt/Dockerfile contains DEVIATION comment" "$dockerfile" "# DEVIATION"
  assert_contains "DEVIATION comment says the image keeps root" \
    "$(deviation_block "$dockerfile")" "root"
  assert_contains "DEVIATION comment names the openstack user it does not create" \
    "$(deviation_block "$dockerfile")" "openstack"
  assert_file_not_contains "libvirt/Dockerfile has no USER instruction" "$dockerfile" '^USER '
}

# --- Test 6: the openstack-hypervisor-operator image runs as distroless nonroot ---
test_hvo_deviation_comment() {
  echo "Test: openstack-hypervisor-operator Dockerfile has DEVIATION comment (distroless, uid 65532)"

  local dockerfile="$PROJECT_ROOT/images/openstack-hypervisor-operator/Dockerfile"

  assert_file_contains "openstack-hypervisor-operator/Dockerfile contains DEVIATION comment" \
    "$dockerfile" "# DEVIATION"
  assert_contains "DEVIATION comment names the distroless base" \
    "$(deviation_block "$dockerfile")" "distroless"
  assert_contains "DEVIATION comment names the openstack user it does not create" \
    "$(deviation_block "$dockerfile")" "openstack"
  assert_file_contains "openstack-hypervisor-operator/Dockerfile runs as 65532:65532" \
    "$dockerfile" '^USER 65532:65532$'
}

# --- Test 7: the kvm-node-agent image runs as root on distroless ---
test_kna_deviation_comment() {
  echo "Test: kvm-node-agent Dockerfile has DEVIATION comment (distroless, root)"

  local dockerfile="$PROJECT_ROOT/images/kvm-node-agent/Dockerfile"

  assert_file_contains "kvm-node-agent/Dockerfile contains DEVIATION comment" \
    "$dockerfile" "# DEVIATION"
  assert_contains "DEVIATION comment names the distroless base" \
    "$(deviation_block "$dockerfile")" "distroless"
  assert_contains "DEVIATION comment names the openstack user it does not create" \
    "$(deviation_block "$dockerfile")" "openstack"
  assert_contains "DEVIATION comment says why the agent runs as root" \
    "$(deviation_block "$dockerfile")" "root"
  assert_file_contains "kvm-node-agent/Dockerfile ignores hadolint's root-user rule" \
    "$dockerfile" '^# hadolint ignore=DL3002$'
  assert_file_contains "kvm-node-agent/Dockerfile runs as 0:0" \
    "$dockerfile" '^USER 0:0$'
}

# --- Run all tests ---
echo "=== DEVIATION comment verification tests ==="
echo ""
test_python_base_deviation_comment
for service in $SERVICES; do
  echo ""
  test_service_deviation_comment "$service"
done
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
