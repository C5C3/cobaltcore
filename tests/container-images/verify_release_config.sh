#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify release configuration files are valid YAML with expected structure
# Usage: bash tests/container-images/verify_release_config.sh
# Requires: yq

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

PASS=0
FAIL=0

# Services that every release must register in source-refs.yaml and
# extra-packages.yaml, each with a matching images/<service>/Dockerfile.
SERVICES="keystone horizon"

# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# --- Test 1: source-refs.yaml is valid YAML with all services ---
test_source_refs_valid_yaml_with_services() {
  echo "Test: source-refs.yaml is valid YAML with all services ($SERVICES)"

  local found_any=false
  for source_refs in "$PROJECT_ROOT"/releases/*/source-refs.yaml; do
    [ -f "$source_refs" ] || continue
    found_any=true
    local rel_path="${source_refs#"$PROJECT_ROOT"/}"
    local release_name
    release_name=$(basename "$(dirname "$source_refs")")

    # Verify valid YAML (yq exits non-zero on invalid YAML)
    if yq '.' "$source_refs" > /dev/null 2>&1; then
      echo "  PASS: [$release_name] $rel_path is valid YAML"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: [$release_name] $rel_path is not valid YAML"
      FAIL=$((FAIL + 1))
      continue
    fi

    # Verify each service version is a valid semver tag
    local service
    for service in $SERVICES; do
      local version
      version=$(yq ".${service}" "$source_refs" | tr -d '"')
      if [[ "$version" == "null" || -z "$version" ]]; then
        echo "  FAIL: [$release_name] $service key is missing from $rel_path"
        FAIL=$((FAIL + 1))
      elif [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        echo "  PASS: [$release_name] $service version is valid semver ($version)"
        PASS=$((PASS + 1))
      else
        echo "  FAIL: [$release_name] $service version is not valid semver: $version"
        FAIL=$((FAIL + 1))
      fi
    done
  done

  if [ "$found_any" = false ]; then
    echo "  FAIL: no releases/*/source-refs.yaml files found"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 2: extra-packages.yaml has expected structure ---
test_extra_packages_valid_yaml_structure() {
  echo "Test: extra-packages.yaml has valid YAML structure"

  local found_any=false
  for extra_packages in "$PROJECT_ROOT"/releases/*/extra-packages.yaml; do
    [ -f "$extra_packages" ] || continue
    found_any=true
    local rel_path="${extra_packages#"$PROJECT_ROOT"/}"
    local release_name
    release_name=$(basename "$(dirname "$extra_packages")")

    # Verify valid YAML
    if yq '.' "$extra_packages" > /dev/null 2>&1; then
      echo "  PASS: [$release_name] $rel_path is valid YAML"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: [$release_name] $rel_path is not valid YAML"
      FAIL=$((FAIL + 1))
      continue
    fi

    local service
    for service in $SERVICES; do
      # Verify <service>.pip_extras exists and is an array (: allow empty lists per review #1)
      local pip_extras_tag
      pip_extras_tag=$(yq ".${service}.pip_extras | tag" "$extra_packages")
      if [[ "$pip_extras_tag" == "!!seq" ]]; then
        local pip_extras_count
        pip_extras_count=$(yq ".${service}.pip_extras | length" "$extra_packages")
        echo "  PASS: [$release_name] $service.pip_extras is a valid array ($pip_extras_count items)"
        PASS=$((PASS + 1))
      else
        echo "  FAIL: [$release_name] $service.pip_extras must be an array (got $pip_extras_tag)"
        FAIL=$((FAIL + 1))
      fi

      # Verify <service>.apt_packages exists and is an array (: allow empty lists per review #1)
      local apt_tag
      apt_tag=$(yq ".${service}.apt_packages | tag" "$extra_packages")
      if [[ "$apt_tag" == "!!seq" ]]; then
        local apt_count
        apt_count=$(yq ".${service}.apt_packages | length" "$extra_packages")
        echo "  PASS: [$release_name] $service.apt_packages is a valid array ($apt_count items)"
        PASS=$((PASS + 1))
      else
        echo "  FAIL: [$release_name] $service.apt_packages must be an array (got $apt_tag)"
        FAIL=$((FAIL + 1))
      fi

      # Verify pip_packages entries are valid if present (optional field)
      local pip_pkg_count
      pip_pkg_count=$(yq ".${service}.pip_packages | length // 0" "$extra_packages" 2>/dev/null || echo "0")
      if [ "$pip_pkg_count" -gt 0 ]; then
        local bad_pip_pkgs
        bad_pip_pkgs=$(yq ".${service}.pip_packages[]" "$extra_packages" \
          | tr -d '"' | grep -vE '^[a-zA-Z0-9][a-zA-Z0-9._-]*$' || true)
        if [ -z "$bad_pip_pkgs" ]; then
          echo "  PASS: [$release_name] $service pip_packages entries are valid ($pip_pkg_count)"
          PASS=$((PASS + 1))
        else
          echo "  FAIL: [$release_name] $service pip_packages entries contain invalid names: $bad_pip_pkgs"
          FAIL=$((FAIL + 1))
        fi
      else
        echo "  PASS: [$release_name] $service pip_packages is empty or absent (optional)"
        PASS=$((PASS + 1))
      fi

      # Validate pip_extras entries match bare Python extra name pattern
      local bad_extras
      bad_extras=$(yq ".${service}.pip_extras[]" "$extra_packages" \
        | tr -d '"' | grep -vE '^[a-z][a-z0-9_-]*$' || true)
      if [ -z "$bad_extras" ]; then
        echo "  PASS: [$release_name] $service.pip_extras entries match naming pattern"
        PASS=$((PASS + 1))
      else
        echo "  FAIL: [$release_name] $service.pip_extras entries violate pattern ^[a-z][a-z0-9_-]*\$: $bad_extras"
        FAIL=$((FAIL + 1))
      fi

      # Validate apt_packages entries match Debian package name pattern
      local bad_apt
      bad_apt=$(yq ".${service}.apt_packages[]" "$extra_packages" \
        | tr -d '"' | grep -vE '^[a-z0-9][a-z0-9.+-]+$' || true)
      if [ -z "$bad_apt" ]; then
        echo "  PASS: [$release_name] $service.apt_packages entries match naming pattern"
        PASS=$((PASS + 1))
      else
        echo "  FAIL: [$release_name] $service.apt_packages entries violate pattern ^[a-z0-9][a-z0-9.+-]+\$: $bad_apt"
        FAIL=$((FAIL + 1))
      fi
    done
  done

  if [ "$found_any" = false ]; then
    echo "  FAIL: no releases/*/extra-packages.yaml files found"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 3: Dockerfile and CI workflow support extra-packages.yaml ---
test_extra_packages_build_wiring() {
  echo "Test: Dockerfiles and CI workflow support extra-packages.yaml"

  local workflow="$PROJECT_ROOT/.github/workflows/build-images.yaml"

  if [ ! -f "$workflow" ]; then
    echo "  FAIL: required files missing"
    FAIL=$((FAIL + 1))
    return
  fi

  local service
  for service in $SERVICES; do
    local dockerfile="$PROJECT_ROOT/images/${service}/Dockerfile"

    if [ ! -f "$dockerfile" ]; then
      echo "  FAIL: [$service] images/$service/Dockerfile missing"
      FAIL=$((FAIL + 1))
      continue
    fi

    # Verify Dockerfile declares ARG PIP_EXTRAS
    if grep -q '^ARG PIP_EXTRAS=' "$dockerfile"; then
      echo "  PASS: [$service] Dockerfile declares ARG PIP_EXTRAS"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: [$service] Dockerfile missing ARG PIP_EXTRAS"
      FAIL=$((FAIL + 1))
    fi

    # Verify Dockerfile declares ARG PIP_PACKAGES
    if grep -q '^ARG PIP_PACKAGES=' "$dockerfile"; then
      echo "  PASS: [$service] Dockerfile declares ARG PIP_PACKAGES"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: [$service] Dockerfile missing ARG PIP_PACKAGES"
      FAIL=$((FAIL + 1))
    fi

    # Verify Dockerfile declares ARG EXTRA_APT_PACKAGES
    if grep -q '^ARG EXTRA_APT_PACKAGES=' "$dockerfile"; then
      echo "  PASS: [$service] Dockerfile declares ARG EXTRA_APT_PACKAGES"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: [$service] Dockerfile missing ARG EXTRA_APT_PACKAGES"
      FAIL=$((FAIL + 1))
    fi
  done

  # Verify CI workflow reads from extra-packages.yaml
  if grep -q 'extra-packages.yaml' "$workflow"; then
    echo "  PASS: CI workflow references extra-packages.yaml"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: CI workflow does not reference extra-packages.yaml"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 4: Dockerfile does not hardcode apt package names ---
test_no_hardcoded_apt_packages() {
  echo "Test: Dockerfiles do not hardcode apt package names"

  local service
  for service in $SERVICES; do
    local dockerfile="$PROJECT_ROOT/images/${service}/Dockerfile"

    if [ ! -f "$dockerfile" ]; then
      echo "  FAIL: [$service] Dockerfile not found"
      FAIL=$((FAIL + 1))
      continue
    fi

    # Verify the apt-get install line uses the build arg rather than hardcoded package names
    # (release-independent — only needs to run once)
    if grep -q 'apt-get install.*\${EXTRA_APT_PACKAGES}' "$dockerfile"; then
      echo "  PASS: [$service] Dockerfile apt-get install uses \${EXTRA_APT_PACKAGES}"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: [$service] Dockerfile apt-get install does not use \${EXTRA_APT_PACKAGES}"
      FAIL=$((FAIL + 1))
    fi
  done

  # Verify extra-packages.yaml exists for each release
  local found_any=false
  for extra_packages in "$PROJECT_ROOT"/releases/*/extra-packages.yaml; do
    [ -f "$extra_packages" ] || continue
    found_any=true
    local release_name
    release_name=$(basename "$(dirname "$extra_packages")")
    local rel_path="${extra_packages#"$PROJECT_ROOT"/}"
    echo "  PASS: [$release_name] $rel_path exists"
    PASS=$((PASS + 1))
  done

  if [ "$found_any" = false ]; then
    echo "  FAIL: no releases/*/extra-packages.yaml files found"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 5: test-excludes/*.txt files have valid stestr exclude-list format ---
test_test_excludes_file_format() {
  echo "Test: test-excludes/*.txt files have valid stestr exclude-list format"

  local found_any=false
  for excludes_file in "$PROJECT_ROOT"/releases/*/test-excludes/*.txt; do
    [ -f "$excludes_file" ] || continue
    found_any=true
    local rel_path="${excludes_file#"$PROJECT_ROOT"/}"

    # Verify file is not empty (must have at least one non-blank, non-comment line or comment)
    local content_lines
    content_lines=$(grep -cE '^.' "$excludes_file" || true)
    if [ "$content_lines" -gt 0 ]; then
      echo "  PASS: $rel_path has content ($content_lines lines)"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $rel_path is empty"
      FAIL=$((FAIL + 1))
      continue
    fi

    # Verify format: every non-blank line is either a comment (starts with #) or
    # a non-empty pattern.  stestr exclude-list accepts any valid regex, so we do
    # NOT constrain the first character — patterns may start with ^, ., digits,
    # metacharacters, etc.  We only flag whitespace-only lines (likely mistakes).
    local bad_lines
    bad_lines=$(grep -nE '^[[:space:]]+$' "$excludes_file" || true)
    if [ -z "$bad_lines" ]; then
      echo "  PASS: $rel_path has valid stestr exclude-list format"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $rel_path has whitespace-only lines (use blank lines or # comments instead):"
      echo "    $bad_lines"
      FAIL=$((FAIL + 1))
    fi

    # Verify file contains at least one comment line (documentation)
    if grep -qE '^#' "$excludes_file"; then
      echo "  PASS: $rel_path contains comment lines"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $rel_path has no comment lines (expected documentation comments)"
      FAIL=$((FAIL + 1))
    fi
  done

  if [ "$found_any" = false ]; then
    echo "  PASS: no test-excludes/*.txt files found (optional)"
    PASS=$((PASS + 1))
  fi
}

# --- Test 6: test-excludes directory structure is valid ---
test_test_excludes_directory_structure() {
  echo "Test: test-excludes directory structure is valid"

  local found_any=false
  for excludes_dir in "$PROJECT_ROOT"/releases/*/test-excludes; do
    [ -d "$excludes_dir" ] || continue
    found_any=true
    local rel_dir="${excludes_dir#"$PROJECT_ROOT"/}"

    echo "  PASS: $rel_dir/ exists"
    PASS=$((PASS + 1))

    # All files must be .txt
    local non_txt_files
    non_txt_files=$(find "$excludes_dir" -maxdepth 1 -type f ! -name '*.txt' || true)
    if [ -z "$non_txt_files" ]; then
      echo "  PASS: all files in $rel_dir/ are .txt"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: non-.txt files found in $rel_dir/:"
      echo "    $non_txt_files"
      FAIL=$((FAIL + 1))
    fi
  done

  if [ "$found_any" = false ]; then
    echo "  PASS: no test-excludes/ directories found (optional)"
    PASS=$((PASS + 1))
  fi
}

# --- Test 7: test-excludes filenames match services in source-refs.yaml ---
test_test_excludes_files_match_services() {
  echo "Test: test-excludes filenames match services in source-refs.yaml"

  local found_any=false
  for excludes_dir in "$PROJECT_ROOT"/releases/*/test-excludes; do
    [ -d "$excludes_dir" ] || continue
    found_any=true
    local release_dir
    release_dir=$(dirname "$excludes_dir")
    local release_name
    release_name=$(basename "$release_dir")
    local source_refs="$release_dir/source-refs.yaml"

    if [ ! -f "$source_refs" ]; then
      echo "  FAIL: $release_name/source-refs.yaml not found (needed to validate test-excludes filenames)"
      FAIL=$((FAIL + 1))
      continue
    fi

    # Get list of service keys from source-refs.yaml
    local services
    services=$(yq 'keys | .[]' "$source_refs" | tr -d '"')

    # Check each .txt file matches a service key
    local all_match=true
    for file in "$excludes_dir"/*.txt; do
      [ -f "$file" ] || continue
      local basename
      basename=$(basename "$file" .txt)
      if echo "$services" | grep -qx "$basename"; then
        echo "  PASS: $release_name/test-excludes/$basename.txt matches service key in source-refs.yaml"
        PASS=$((PASS + 1))
      else
        echo "  FAIL: $release_name/test-excludes/$basename.txt does not match any service key in source-refs.yaml"
        FAIL=$((FAIL + 1))
        all_match=false
      fi
    done

    if [ "$all_match" = true ]; then
      echo "  PASS: all $release_name/test-excludes filenames correspond to services"
      PASS=$((PASS + 1))
    fi
  done

  if [ "$found_any" = false ]; then
    echo "  PASS: no test-excludes/ directories found (optional)"
    PASS=$((PASS + 1))
  fi
}

# --- Run all tests ---
echo "=== Release config verification tests ==="
echo ""
test_source_refs_valid_yaml_with_services
echo ""
test_extra_packages_valid_yaml_structure
echo ""
test_extra_packages_build_wiring
echo ""
test_no_hardcoded_apt_packages
echo ""
test_test_excludes_file_format
echo ""
test_test_excludes_directory_structure
echo ""
test_test_excludes_files_match_services
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
