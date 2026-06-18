#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify venv-builder container image meets requirements
# Usage: bash tests/container-images/verify_venv_builder.sh [image_name]
# Default image: c5c3/venv-builder:3.12-noble
# Requires: Docker daemon running

set -euo pipefail

IMAGE="${1:-c5c3/venv-builder:3.12-noble}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# --- Test 1: uv version matches Dockerfile ---
EXPECTED_UV_VERSION=$(sed -n 's/.*COPY --from=ghcr\.io\/astral-sh\/uv:\([^@ ]*\).*/\1/p' \
  "$SCRIPT_DIR/../../images/venv-builder/Dockerfile" 2>/dev/null | head -1) || true
if [[ -z "$EXPECTED_UV_VERSION" ]]; then
  echo "ERROR: Could not extract uv version from $SCRIPT_DIR/../../images/venv-builder/Dockerfile" >&2
  exit 1
fi

# Source of truth for the pinned base-package versions asserted by Test 3.
REQUIREMENTS="$SCRIPT_DIR/../../images/venv-builder/requirements.txt"
test_uv_version_is_pinned() {
  echo "Test: uv version is $EXPECTED_UV_VERSION"
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" uv --version 2>&1) || exit_code=$?

  assert_eq "uv --version exits 0" "0" "$exit_code"
  assert_contains "uv version is $EXPECTED_UV_VERSION" "$version" "$EXPECTED_UV_VERSION"
}

# --- Test 2: virtualenv exists at /var/lib/openstack ---
test_virtualenv_exists() {
  echo "Test: virtualenv exists at /var/lib/openstack"
  local exit_code=0
  docker run --rm "$IMAGE" test -x /var/lib/openstack/bin/python3 || exit_code=$?

  assert_eq "/var/lib/openstack/bin/python3 is executable" "0" "$exit_code"
}

# --- Test 3: common packages are installed at their pinned versions ---
# Asserts every pin in images/venv-builder/requirements.txt is installed at
# exactly that version, so a drift back to "whatever is latest on PyPI" is
# caught. Expected values are derived from the requirements file (the source
# of truth), not hard-coded here.
test_common_packages_pinned() {
  echo "Test: common packages are installed at their pinned versions"

  if [[ ! -f "$REQUIREMENTS" ]]; then
    echo "  FAIL: requirements file not found at $REQUIREMENTS"
    FAIL=$((FAIL + 1))
    return
  fi

  local freeze exit_code=0
  freeze=$(docker run --rm "$IMAGE" /var/lib/openstack/bin/pip list --format=freeze 2>&1) || exit_code=$?
  assert_eq "pip list exits 0" "0" "$exit_code"

  # Collect "name==version" pins (skip SPDX header, comments, and blanks).
  local pins=() line
  while IFS= read -r line; do
    [[ "$line" =~ ^[A-Za-z0-9._-]+==[^[:space:]]+$ ]] && pins+=("$line")
  done < "$REQUIREMENTS"

  if [[ "${#pins[@]}" -eq 0 ]]; then
    echo "  FAIL: no pins parsed from $REQUIREMENTS"
    FAIL=$((FAIL + 1))
    return
  fi

  # Match each pin against a whole freeze line, anchored, so a pin can't match
  # a substring of another package (e.g. "pymysql==1.2.0" inside
  # "pymysql==1.2.0.post1", or a different project whose line merely contains
  # the string). pip canonicalises name casing (PyMySQL, uWSGI), so match
  # case-insensitively; "." is escaped so it stays literal in the ERE.
  local pin name version pattern
  for pin in "${pins[@]}"; do
    name="${pin%%==*}"
    version="${pin#*==}"
    pattern="^${name//./\\.}==${version//./\\.}$"
    if echo "$freeze" | grep -iqE "$pattern"; then
      echo "  PASS: $pin installed at pinned version"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $pin not installed at pinned version"
      echo "    pip freeze: $freeze"
      FAIL=$((FAIL + 1))
    fi
  done
}

# --- Run all tests ---
echo "=== venv-builder container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_uv_version_is_pinned
echo ""
test_virtualenv_exists
echo ""
test_common_packages_pinned
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
