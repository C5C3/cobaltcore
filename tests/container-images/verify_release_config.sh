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

  # Verify keystone version is a valid semver tag
  local keystone_version
  keystone_version=$(yq '.keystone' "$source_refs" | tr -d '"')
  if [[ "$keystone_version" == "null" || -z "$keystone_version" ]]; then
    echo "  FAIL: keystone key is missing from source-refs.yaml"
    FAIL=$((FAIL + 1))
  elif [[ "$keystone_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "  PASS: keystone version is valid semver ($keystone_version)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: keystone version is not valid semver: $keystone_version"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 2: extra-packages.yaml has expected structure (CC-0027 REQ-007) ---
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

  # Verify pip_extras count >= 1
  local pip_extras_count
  pip_extras_count=$(yq '.keystone.pip_extras | length' "$extra_packages")
  if [ "$pip_extras_count" -ge 1 ]; then
    echo "  PASS: keystone.pip_extras has >= 1 items ($pip_extras_count)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: keystone.pip_extras must have >= 1 items (got $pip_extras_count)"
    FAIL=$((FAIL + 1))
  fi

  # Verify apt_packages count >= 1
  local apt_count
  apt_count=$(yq '.keystone.apt_packages | length' "$extra_packages")
  if [ "$apt_count" -ge 1 ]; then
    echo "  PASS: keystone.apt_packages has >= 1 items ($apt_count)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: keystone.apt_packages must have >= 1 items (got $apt_count)"
    FAIL=$((FAIL + 1))
  fi

  # Verify pip_packages entries are valid if present (optional field)
  local pip_pkg_count
  pip_pkg_count=$(yq '.keystone.pip_packages | length // 0' "$extra_packages" 2>/dev/null || echo "0")
  if [ "$pip_pkg_count" -gt 0 ]; then
    local bad_pip_pkgs
    bad_pip_pkgs=$(yq '.keystone.pip_packages[]' "$extra_packages" \
      | tr -d '"' | grep -vE '^[a-zA-Z0-9][a-zA-Z0-9._-]*$' || true)
    if [ -z "$bad_pip_pkgs" ]; then
      echo "  PASS: pip_packages entries are valid ($pip_pkg_count)"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: pip_packages entries contain invalid names: $bad_pip_pkgs"
      FAIL=$((FAIL + 1))
    fi
  else
    echo "  PASS: pip_packages is empty or absent (optional)"
    PASS=$((PASS + 1))
  fi

  # Validate pip_extras entries match bare Python extra name pattern
  local bad_extras
  bad_extras=$(yq '.keystone.pip_extras[]' "$extra_packages" \
    | tr -d '"' | grep -vE '^[a-z][a-z0-9_-]*$' || true)
  if [ -z "$bad_extras" ]; then
    echo "  PASS: pip_extras entries match naming pattern"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: pip_extras entries violate pattern ^[a-z][a-z0-9_-]*\$: $bad_extras"
    FAIL=$((FAIL + 1))
  fi

  # Validate apt_packages entries match Debian package name pattern
  local bad_apt
  bad_apt=$(yq '.keystone.apt_packages[]' "$extra_packages" \
    | tr -d '"' | grep -vE '^[a-z0-9][a-z0-9.+-]+$' || true)
  if [ -z "$bad_apt" ]; then
    echo "  PASS: apt_packages entries match naming pattern"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: apt_packages entries violate pattern ^[a-z0-9][a-z0-9.+-]+\$: $bad_apt"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 3: Dockerfile and CI workflow support extra-packages.yaml (CC-0027) ---
test_extra_packages_build_wiring() {
  echo "Test: Dockerfile and CI workflow support extra-packages.yaml"

  local dockerfile="$PROJECT_ROOT/images/keystone/Dockerfile"
  local workflow="$PROJECT_ROOT/.github/workflows/build-images.yaml"

  if [ ! -f "$dockerfile" ] || [ ! -f "$workflow" ]; then
    echo "  FAIL: required files missing"
    FAIL=$((FAIL + 1))
    return
  fi

  # Verify Dockerfile declares ARG PIP_EXTRAS
  if grep -q '^ARG PIP_EXTRAS=' "$dockerfile"; then
    echo "  PASS: Dockerfile declares ARG PIP_EXTRAS"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: Dockerfile missing ARG PIP_EXTRAS"
    FAIL=$((FAIL + 1))
  fi

  # Verify Dockerfile declares ARG PIP_PACKAGES
  if grep -q '^ARG PIP_PACKAGES=' "$dockerfile"; then
    echo "  PASS: Dockerfile declares ARG PIP_PACKAGES"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: Dockerfile missing ARG PIP_PACKAGES"
    FAIL=$((FAIL + 1))
  fi

  # Verify Dockerfile declares ARG EXTRA_APT_PACKAGES
  if grep -q '^ARG EXTRA_APT_PACKAGES=' "$dockerfile"; then
    echo "  PASS: Dockerfile declares ARG EXTRA_APT_PACKAGES"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: Dockerfile missing ARG EXTRA_APT_PACKAGES"
    FAIL=$((FAIL + 1))
  fi

  # Verify CI workflow reads from extra-packages.yaml
  if grep -q 'extra-packages.yaml' "$workflow"; then
    echo "  PASS: CI workflow references extra-packages.yaml"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: CI workflow does not reference extra-packages.yaml"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 4: Dockerfile does not hardcode apt package names (CC-0027) ---
test_no_hardcoded_apt_packages() {
  echo "Test: Dockerfile does not hardcode apt package names"

  local dockerfile="$PROJECT_ROOT/images/keystone/Dockerfile"
  local extra_packages="$PROJECT_ROOT/releases/2025.2/extra-packages.yaml"

  if [ ! -f "$dockerfile" ] || [ ! -f "$extra_packages" ]; then
    echo "  FAIL: required files missing"
    FAIL=$((FAIL + 1))
    return
  fi

  # Verify the apt-get install line uses the build arg rather than hardcoded package names
  if grep -q 'apt-get install.*\${EXTRA_APT_PACKAGES}' "$dockerfile"; then
    echo "  PASS: Dockerfile apt-get install uses \${EXTRA_APT_PACKAGES}"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: Dockerfile apt-get install does not use \${EXTRA_APT_PACKAGES}"
    FAIL=$((FAIL + 1))
  fi
}

# --- Run all tests ---
echo "=== Release config verification tests ==="
echo ""
test_source_refs_valid_yaml_with_keystone
echo ""
test_extra_packages_valid_yaml_structure
echo ""
test_extra_packages_build_wiring
echo ""
test_no_hardcoded_apt_packages
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
