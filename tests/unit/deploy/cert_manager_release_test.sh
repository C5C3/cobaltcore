#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify that the cert-manager HelmRelease hack/deploy-infra.sh applies turns
# Ready only once its webhook admits a request:
#
#   1. The kind base overlay and the metal-stack lab base overlay render the
#      release with the chart's startupapicheck Job enabled.
#   2. Neither render disables install hooks, which is what runs the Job.
#
# Every release that depends on cert-manager creates an Issuer or a
# Certificate. With the check off, the release was Ready up to 78 seconds
# before the webhook admitted on the metal-stack lab, and a dependent release
# used up its install retries and stayed Stalled (#1189). The production
# release (deploy/flux-system/releases/cert-manager.yaml) keeps the check off;
# the kind base overlay patches it.
#
# The checks are counted as SKIP when kustomize or yq is not on PATH.
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

# --- Tests 1 and 2: the overlays deploy-infra applies ---
test_overlays_enable_the_check() {
  echo "Test: the kind and the lab base overlay enable the startup API check"
  local dir
  for dir in deploy/kind/base deploy/lab/metal-stack/base; do
    render "$PROJECT_ROOT/$dir" 2 || continue
    assert_eq "$dir renders HelmRelease/cert-manager with the check enabled" "true" \
      "$(val HelmRelease cert-manager '.spec.values.startupapicheck.enabled')"
    assert_eq "$dir does not disable the release's install hooks" "false" \
      "$(val HelmRelease cert-manager '.spec.install.disableHooks // false')"
  done
}

test_overlays_enable_the_check

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
