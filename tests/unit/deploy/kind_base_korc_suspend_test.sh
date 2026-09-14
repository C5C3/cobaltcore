#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify that the kind base overlay applies the K-ORC Flux Kustomization
# suspended, and that the flux path of hack/deploy-infra.sh still lifts that
# suspension:
#   - deploy/kind/base/kustomization.yaml carries a patch that sets
#     spec.suspend: true on Kustomization/k-orc in flux-system, the same way
#     it suspends the service-operator HelmReleases. Applied active, the
#     kustomize-controller reconciles K-ORC in the window between the base
#     overlay apply and the suspend patch in hack/deploy-infra.sh, and the
#     out-of-band hack/ci-deploy-korc.sh then conflicts with it on the
#     manager image under server-side apply.
#   - kustomize build deploy/kind/base/ renders that suspend.
#   - deploy/flux-system/ (production overlay) is NOT affected: its
#     Kustomization/k-orc renders without spec.suspend.
#   - hack/deploy-infra.sh un-suspends kustomization/k-orc on the
#     CONTROLPLANE_OPERATORS=flux path, so the base-level suspend does not
#     strand the Flux-driven ControlPlane deploy.
# Usage: bash tests/unit/deploy/kind_base_korc_suspend_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

KIND_BASE_DIR="$PROJECT_ROOT/deploy/kind/base"
KIND_KUSTOMIZATION="$KIND_BASE_DIR/kustomization.yaml"
FLUX_SYSTEM_DIR="$PROJECT_ROOT/deploy/flux-system"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"

# The rendered Kustomization/k-orc document, or empty when absent.
render_korc() {
  local dir="$1"
  kustomize build "$dir" | yq eval-all \
    'select(.kind == "Kustomization" and .apiVersion == "kustomize.toolkit.fluxcd.io/v1" and .metadata.name == "k-orc" and .metadata.namespace == "flux-system")' -
}

# --- Test 1: the kind base kustomization carries the k-orc suspend patch ---
#             (source-level, no tools required)
test_kind_base_declares_korc_suspend_patch() {
  echo "Test: deploy/kind/base/kustomization.yaml patches Kustomization/k-orc with spec.suspend: true"

  if [[ ! -f "$KIND_KUSTOMIZATION" ]]; then
    echo "  FAIL: $KIND_KUSTOMIZATION does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  # Walk the patches list entry by entry: an entry whose target names the
  # Flux Kustomization k-orc must carry suspend: true in its inline patch.
  local matched
  matched="$(awk '
    /^  - target:/ { if (hit && suspended) found = 1; hit = 0; suspended = 0; next }
    /^      group: kustomize\.toolkit\.fluxcd\.io$/ { group = 1; next }
    /^      kind: Kustomization$/ { if (group) kind = 1; next }
    /^      name: k-orc$/ { if (kind) hit = 1; group = 0; kind = 0; next }
    /^        suspend: true$/ { if (hit) suspended = 1; next }
    END { if (hit && suspended) found = 1; print found + 0 }
  ' "$KIND_KUSTOMIZATION")"

  assert_eq "a patches entry targets kustomize.toolkit.fluxcd.io Kustomization/k-orc and sets suspend: true" \
    "1" "$matched"
}

# --- Test 2: kustomize build of deploy/kind/base/ renders the suspend ---
test_kind_base_renders_korc_suspended() {
  echo "Test: kustomize build deploy/kind/base/ renders Kustomization/k-orc with spec.suspend true"

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local korc
  if ! korc="$(render_korc "$KIND_BASE_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $KIND_BASE_DIR failed:"
    echo "$korc" | head -20
    FAIL=$((FAIL + 2))
    return
  fi

  local count
  count="$(printf '%s\n' "$korc" | grep -c '^kind: Kustomization$' || true)"
  assert_eq "exactly one Kustomization/k-orc in flux-system" "1" "$count"

  assert_eq "kind overlay renders Kustomization/k-orc spec.suspend true" \
    "true" \
    "$(printf '%s\n' "$korc" | yq -r '.spec.suspend')"
}

# --- Test 3: the production overlay renders k-orc without a suspend ---
test_production_overlay_renders_korc_active() {
  echo "Test: kustomize build deploy/flux-system/ renders Kustomization/k-orc without spec.suspend"

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local korc
  if ! korc="$(render_korc "$FLUX_SYSTEM_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $FLUX_SYSTEM_DIR failed:"
    echo "$korc" | head -20
    FAIL=$((FAIL + 2))
    return
  fi

  local count
  count="$(printf '%s\n' "$korc" | grep -c '^kind: Kustomization$' || true)"
  assert_eq "exactly one Kustomization/k-orc in flux-system" "1" "$count"

  assert_eq "production overlay renders Kustomization/k-orc with no spec.suspend" \
    "null" \
    "$(printf '%s\n' "$korc" | yq -r '.spec.suspend')"
}

# --- Test 4: the flux path of hack/deploy-infra.sh still un-suspends k-orc ---
test_deploy_infra_unsuspends_korc_on_flux_path() {
  echo "Test: hack/deploy-infra.sh un-suspends kustomization/k-orc for CONTROLPLANE_OPERATORS=flux"

  if [[ ! -f "$DEPLOY_INFRA_SH" ]]; then
    echo "  FAIL: $DEPLOY_INFRA_SH does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  # The patch spans two lines (kubectl patch ... \ / -p '{"spec":{"suspend":false}}'),
  # so join the continuation before matching.
  local matched
  matched="$(awk '
    /kubectl patch kustomization\/k-orc -n flux-system/ { pending = 1; line = $0; next }
    pending { line = line " " $0; pending = 0
      if (line ~ /"suspend":false/) found = 1 }
    END { print found + 0 }
  ' "$DEPLOY_INFRA_SH")"

  assert_eq "a kubectl patch sets kustomization/k-orc spec.suspend false" "1" "$matched"
}

# --- Run ---
test_kind_base_declares_korc_suspend_patch
test_kind_base_renders_korc_suspended
test_production_overlay_renders_korc_active
test_deploy_infra_unsuspends_korc_on_flux_path

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
