#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify that hack/deploy-infra.sh preloads the Keystone image of the release
# the bundled ControlPlane CR asks for.
#
# With WITH_CONTROLPLANE=true the script pulls ghcr.io/c5c3/keystone:<release>
# and loads it into kind, so the Keystone the ControlPlane projects starts
# without an in-cluster pull. The release is the literal cp_release, a copy of
# spec.openStackRelease in deploy/kind/controlplane/controlplane.yaml that the
# release-wiring audit and the new-release inventory read. The preload is
# best-effort and silent when it fails, so a copy that falls behind the CR
# loads an image nothing runs and leaves kind to pull the right one, with no
# error to point at the drift. This test holds the two equal.
#
# Usage: bash tests/unit/hack/deploy_infra_keystone_preload_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"
CONTROLPLANE_YAML="$PROJECT_ROOT/deploy/kind/controlplane/controlplane.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_the_preload_pulls_and_loads_cp_release() {
  echo "Test: the preload pulls and loads the Keystone image of cp_release"
  assert_file_contains_fixed "the preload pulls keystone:\${cp_release}" \
    "$DEPLOY_INFRA_SH" 'docker pull "ghcr.io/c5c3/keystone:${cp_release}"'
  assert_file_contains_fixed "and loads that image into kind" \
    "$DEPLOY_INFRA_SH" 'kind load docker-image "ghcr.io/c5c3/keystone:${cp_release}"'
}

test_cp_release_is_the_bundled_controlplane_release() {
  echo "Test: cp_release is the release of the bundled ControlPlane CR"
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed"
    SKIP=$((SKIP + 1))
    return
  fi
  local cp_release cr_release
  cp_release="$(sed -nE 's/^[[:space:]]*local cp_release="([^"]*)"$/\1/p' "$DEPLOY_INFRA_SH")"
  cr_release="$(yq -r 'select(.kind == "ControlPlane") | .spec.openStackRelease' "$CONTROLPLANE_YAML")"
  assert_not_empty "deploy-infra.sh sets cp_release" "$cp_release"
  assert_not_empty "controlplane.yaml sets spec.openStackRelease" "$cr_release"
  assert_eq "cp_release equals the CR's spec.openStackRelease" "$cr_release" "$cp_release"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------

test_the_preload_pulls_and_loads_cp_release
test_cp_release_is_the_bundled_controlplane_release

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
