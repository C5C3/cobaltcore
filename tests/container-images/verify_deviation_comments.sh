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
# python-base, so each has its own function below.
SERVICES="keystone horizon glance placement barbican neutron"

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
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
