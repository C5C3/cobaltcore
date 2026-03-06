#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify release configuration files are valid YAML with expected structure (CC-0006 REQ-004, REQ-005)
# Usage: bash tests/container-images/verify_release_config.sh
# Requires: yq

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

PASS=0
FAIL=0

# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# --- Test 1: source-refs.yaml is valid YAML with keystone ---
test_source_refs_valid_yaml_with_keystone() {
  echo "Test: source-refs.yaml is valid YAML with keystone"

  local source_refs="$PROJECT_ROOT/releases/2025.2/source-refs.yaml"

  # Verify file exists
  if [ ! -f "$source_refs" ]; then
    echo "  FAIL: source-refs.yaml does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  # Verify valid YAML (yq exits non-zero on invalid YAML)
  if yq '.' "$source_refs" > /dev/null 2>&1; then
    echo "  PASS: source-refs.yaml is valid YAML"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: source-refs.yaml is not valid YAML"
    FAIL=$((FAIL + 1))
    return
  fi

  # Verify keystone version
  local keystone_version
  keystone_version=$(yq '.keystone' "$source_refs" | tr -d '"')
  assert_eq "keystone version is 28.0.0" "28.0.0" "$keystone_version"
}

# --- Test 2: extra-packages.yaml has expected structure ---
test_extra_packages_valid_yaml_structure() {
  echo "Test: extra-packages.yaml has valid YAML structure"

  local extra_packages="$PROJECT_ROOT/releases/2025.2/extra-packages.yaml"

  # Verify file exists
  if [ ! -f "$extra_packages" ]; then
    echo "  FAIL: extra-packages.yaml does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  # Verify valid YAML
  if yq '.' "$extra_packages" > /dev/null 2>&1; then
    echo "  PASS: extra-packages.yaml is valid YAML"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: extra-packages.yaml is not valid YAML"
    FAIL=$((FAIL + 1))
    return
  fi

  # Verify pip_packages count (3 items)
  local pip_count
  pip_count=$(yq '.keystone.pip_packages | length' "$extra_packages")
  assert_eq "keystone.pip_packages has 3 items" "3" "$pip_count"

  # Verify apt_packages count (4 items)
  local apt_count
  apt_count=$(yq '.keystone.apt_packages | length' "$extra_packages")
  assert_eq "keystone.apt_packages has 4 items" "4" "$apt_count"
}

# --- Test 3: extra-packages.yaml pip extras match Dockerfile (CC-0006) ---
test_extra_packages_match_dockerfile() {
  echo "Test: extra-packages.yaml pip extras match Dockerfile"

  local extra_packages="$PROJECT_ROOT/releases/2025.2/extra-packages.yaml"
  local dockerfile="$PROJECT_ROOT/images/keystone/Dockerfile"

  if [ ! -f "$extra_packages" ] || [ ! -f "$dockerfile" ]; then
    echo "  FAIL: required files missing"
    FAIL=$((FAIL + 1))
    return
  fi

  # Extract extras from extra-packages.yaml (e.g. "ldap", "memcache_pool", "oauth1")
  local yaml_extras
  yaml_extras=$(yq '.keystone.pip_packages[]' "$extra_packages" \
    | sed -n 's/.*\[\(.*\)\].*/\1/p' | sort | tr '\n' ',' | sed 's/,$//')

  # Extract extras from Dockerfile uv pip install line (e.g. "ldap,memcache_pool,oauth1")
  local dockerfile_extras
  dockerfile_extras=$(grep -o 'keystone\[[^]]*\]' "$dockerfile" \
    | sed 's/.*\[\(.*\)\]/\1/' | tr ',' '\n' | sort | tr '\n' ',' | sed 's/,$//')

  assert_eq "pip extras match Dockerfile" "$yaml_extras" "$dockerfile_extras"
}

# --- Run all tests ---
echo "=== Release config verification tests ==="
echo ""
test_source_refs_valid_yaml_with_keystone
echo ""
test_extra_packages_valid_yaml_structure
echo ""
test_extra_packages_match_dockerfile
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
