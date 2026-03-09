#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify SPDX license headers on all Dockerfiles, YAML configs, and scripts (CC-0006 REQ-008)
# Usage: bash tests/container-images/verify_spdx_headers.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

PASS=0
FAIL=0

# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# --- Test 1: All Dockerfiles have SPDX headers ---
test_all_dockerfiles_have_spdx() {
  echo "Test: all Dockerfiles have SPDX headers"
  local all_pass=true

  for dockerfile in "$PROJECT_ROOT"/images/*/Dockerfile; do
    local name
    name="$(basename "$(dirname "$dockerfile")")"
    local line1 line3
    line1=$(sed -n '1p' "$dockerfile")
    line3=$(sed -n '3p' "$dockerfile")

    if [[ "$line1" == *"SPDX-FileCopyrightText"* ]] && [[ "$line3" == *"SPDX-License-Identifier"* ]]; then
      echo "  PASS: images/$name/Dockerfile has SPDX header"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: images/$name/Dockerfile missing SPDX header"
      echo "    line 1: $line1"
      echo "    line 3: $line3"
      FAIL=$((FAIL + 1))
      all_pass=false
    fi
  done

  if $all_pass; then
    echo "  All Dockerfiles passed SPDX check"
  fi
}

# --- Test 2: YAML configs have SPDX headers ---
test_yaml_configs_have_spdx() {
  echo "Test: YAML configs have SPDX headers"

  for yaml_file in "$PROJECT_ROOT"/releases/*/source-refs.yaml \
                   "$PROJECT_ROOT"/releases/*/extra-packages.yaml; do

    local basename_file
    basename_file="$(echo "$yaml_file" | sed "s|$PROJECT_ROOT/||")"
    local line1 line3
    line1=$(sed -n '1p' "$yaml_file")
    line3=$(sed -n '3p' "$yaml_file")

    if [[ "$line1" == *"SPDX-FileCopyrightText"* ]] && [[ "$line3" == *"SPDX-License-Identifier"* ]]; then
      echo "  PASS: $basename_file has SPDX header"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $basename_file missing SPDX header"
      echo "    line 1: $line1"
      echo "    line 3: $line3"
      FAIL=$((FAIL + 1))
    fi
  done
}

# --- Test 3: Script has SPDX after shebang ---
test_script_has_spdx_after_shebang() {
  echo "Test: script has SPDX after shebang"

  local script="$PROJECT_ROOT/scripts/apply-constraint-overrides.sh"
  local line1 line2 line4
  line1=$(sed -n '1p' "$script")
  line2=$(sed -n '2p' "$script")
  line4=$(sed -n '4p' "$script")

  assert_eq "line 1 is shebang" "#!/bin/bash" "$line1"

  if [[ "$line2" == *"SPDX-FileCopyrightText"* ]]; then
    echo "  PASS: line 2 has SPDX-FileCopyrightText"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: line 2 missing SPDX-FileCopyrightText"
    echo "    actual: $line2"
    FAIL=$((FAIL + 1))
  fi

  if [[ "$line4" == *"SPDX-License-Identifier"* ]]; then
    echo "  PASS: line 4 has SPDX-License-Identifier"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: line 4 missing SPDX-License-Identifier"
    echo "    actual: $line4"
    FAIL=$((FAIL + 1))
  fi
}

# --- Run all tests ---
echo "=== SPDX header verification tests ==="
echo ""
test_all_dockerfiles_have_spdx
echo ""
test_yaml_configs_have_spdx
echo ""
test_script_has_spdx_after_shebang
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
