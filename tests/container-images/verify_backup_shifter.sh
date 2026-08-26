#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify backup-shifter container image meets requirements
# Usage: bash tests/container-images/verify_backup_shifter.sh [image_name]
# Default image: backup-shifter
# Requires: Docker daemon running

set -euo pipefail

IMAGE="${1:-backup-shifter}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# --- Test 1: rclone version outputs a version and exits 0 ---
test_rclone_version() {
  echo "Test: rclone version succeeds"
  local version first_line exit_code=0
  version=$(docker run --rm "$IMAGE" rclone version 2>&1) || exit_code=$?
  first_line=$(printf '%s\n' "$version" | head -n 1)

  assert_eq "rclone version exits 0" "0" "$exit_code"
  if [[ "$first_line" =~ ^rclone\ v[0-9]+\.[0-9]+ ]]; then
    echo "  PASS: first line reports an rclone version"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: first line reports an rclone version"
    echo "    expected to match: ^rclone v[0-9]+\\.[0-9]+"
    echo "    actual: $first_line"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 2: runs as openstack user (UID 42424) ---
test_runs_as_openstack_user() {
  echo "Test: container runs as openstack user"
  local id_output exit_code=0
  id_output=$(docker run --rm "$IMAGE" id 2>&1) || exit_code=$?

  assert_eq "id exits 0" "0" "$exit_code"
  assert_contains "id reports uid 42424 (openstack)" "$id_output" "uid=42424(openstack)"
}

# --- Test 3: no build tooling in the runtime image ---
test_no_build_tooling() {
  echo "Test: build tooling is absent"
  local tool exit_code
  for tool in gcc git python3; do
    exit_code=0
    # Wrapped in a shell: "command" is a shell builtin and the image declares no
    # ENTRYPOINT, so "docker run <image> command -v gcc" execs a binary that does
    # not exist and exits non-zero whatever is installed — the assertion would
    # pass for every tool, always.
    docker run --rm "$IMAGE" sh -c "command -v $tool" > /dev/null 2>&1 || exit_code=$?
    assert_nonzero_exit "$tool is not installed" "$exit_code"
  done
}

# --- Test 4: /tmp is writable for rclone's temporary files ---
test_tmp_writable() {
  echo "Test: /tmp is writable"
  local exit_code=0
  docker run --rm "$IMAGE" test -w /tmp || exit_code=$?

  assert_eq "/tmp is writable" "0" "$exit_code"
}

# --- Run all tests ---
echo "=== backup-shifter container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_rclone_version
echo ""
test_runs_as_openstack_user
echo ""
test_no_build_tooling
echo ""
test_tmp_writable
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
