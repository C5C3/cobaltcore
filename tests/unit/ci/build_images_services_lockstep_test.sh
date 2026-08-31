#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the service list of the build-images changes job stays in lockstep
# with the releases it builds from: ALL_SERVICES equals the union of the keys
# in releases/*/source-refs.yaml, and every name in it has a svc_<service>
# paths filter and a FILTER_svc_<service> env line.
#
# ALL_SERVICES is a hand-maintained list, the way ci.yaml's ALL_OPERATORS is,
# because dorny/paths-filter cannot take dynamic filter names. Nothing else
# notices when the two drift: a service added to source-refs.yaml without a
# filter simply never resolves to true, so its images stop being built on pull
# requests while the pipeline stays green. This test is what makes the static
# list safe, and it runs under make test-shell on every pull request.
#
# Usage: bash tests/unit/ci/build_images_services_lockstep_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
WORKFLOW="$PROJECT_ROOT/.github/workflows/build-images.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# all_services — the ALL_SERVICES value from the changes job's resolve step,
# one name per line.
all_services() {
  grep -E '^          ALL_SERVICES: ' "$WORKFLOW" |
    sed 's/^ *ALL_SERVICES: //' |
    tr ' ' '\n' |
    grep -v '^$'
}

# release_services — the union of the keys of every releases/*/source-refs.yaml,
# one name per line.
release_services() {
  local file
  for file in "$PROJECT_ROOT"/releases/*/source-refs.yaml; do
    [ -f "$file" ] || continue
    yq -r 'keys | .[]' "$file"
  done | sort -u
}

# ---------------------------------------------------------------------------
# Test 1: ALL_SERVICES is the union of the source-refs keys, both ways
# ---------------------------------------------------------------------------
test_all_services_matches_the_releases() {
  echo "Test: ALL_SERVICES equals the union of the releases/*/source-refs.yaml keys"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local declared from_releases
  declared=$(all_services | sort)
  from_releases=$(release_services)

  assert_not_empty "the changes job declares ALL_SERVICES" "$declared"
  assert_not_empty "releases/*/source-refs.yaml declare services" "$from_releases"
  assert_eq "no service is missing from ALL_SERVICES, and none is invented" \
    "$from_releases" "$declared"
}

# ---------------------------------------------------------------------------
# Test 2: every service has its filter and its env line
# ---------------------------------------------------------------------------
test_every_service_is_wired() {
  echo "Test: every service in ALL_SERVICES has a filter and a FILTER_ env line"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (checks skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local service
  while IFS= read -r service; do
    assert_file_contains "svc_${service} paths filter exists" "$WORKFLOW" \
      "^ *svc_${service}:$"
    assert_file_contains "FILTER_svc_${service} reaches the resolver" "$WORKFLOW" \
      "FILTER_svc_${service}: \${{ steps.filter.outputs.svc_${service} }}"
  done < <(all_services)
}

# ---------------------------------------------------------------------------
# Test 3: no filter names a service ALL_SERVICES does not carry
# ---------------------------------------------------------------------------
test_no_orphan_service_filter() {
  echo "Test: no svc_ filter names a service outside ALL_SERVICES"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local declared filters
  declared=$(all_services | sort)
  filters=$(grep -oE '^ *svc_[a-z0-9]+:$' "$WORKFLOW" |
    sed 's/^ *svc_//;s/:$//' | sort -u)

  assert_eq "the svc_ filters are exactly the ALL_SERVICES names" \
    "$declared" "$filters"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_all_services_matches_the_releases
test_every_service_is_wired
test_no_orphan_service_filter

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
