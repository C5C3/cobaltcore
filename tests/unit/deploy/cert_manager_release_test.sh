#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify that the cert-manager HelmRelease turns Ready only once its webhook
# admits a request:
#
#   1. The production release (deploy/flux-system/releases/cert-manager.yaml)
#      enables the chart's startupapicheck Job, and the three renders that
#      carry it (deploy/flux-system, the kind base overlay and the metal-stack
#      lab base overlay) show the value.
#   2. No render disables install hooks, which is what runs the Job.
#   3. Neither base overlay carries a startupapicheck patch of its own: both
#      inherit the value from the production release.
#
# Every release that depends on cert-manager creates an Issuer or a
# Certificate. With the check off, the release was Ready up to 78 seconds
# before the webhook admitted on the metal-stack lab, and a dependent release
# used up its install retries and stayed Stalled (#1189, #1207).
#
# The render checks are counted as SKIP when kustomize or yq is not on PATH;
# the two file checks always run.
#
# Usage: bash tests/unit/deploy/cert_manager_release_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/kustomize_render.sh
source "$PROJECT_ROOT/tests/lib/kustomize_render.sh"

RENDERED=""

# --- Tests 1 and 2: the production release and the two base overlays ---
test_renders_enable_the_check() {
  echo "Test: the production, the kind and the lab base render enable the startup API check"
  local dir
  for dir in deploy/flux-system deploy/kind/base deploy/lab/metal-stack/base; do
    render "$PROJECT_ROOT/$dir" 2 || continue
    assert_eq "$dir renders HelmRelease/cert-manager with the check enabled" "true" \
      "$(val HelmRelease cert-manager '.spec.values.startupapicheck.enabled')"
    assert_eq "$dir does not disable the release's install hooks" "false" \
      "$(val HelmRelease cert-manager '.spec.install.disableHooks // false')"
  done
}

# --- Test 3: neither overlay patches the value ---
test_overlays_carry_no_patch() {
  echo "Test: neither base overlay carries a startupapicheck patch"
  local file
  for file in deploy/kind/base/kustomization.yaml deploy/lab/metal-stack/base/kustomization.yaml; do
    assert_file_not_contains "$file carries no startupapicheck patch" \
      "$PROJECT_ROOT/$file" "startupapicheck"
  done
}

test_renders_enable_the_check
test_overlays_carry_no_patch

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
