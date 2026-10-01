#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify that the K-ORC Flux source checks out only the directory its
# Kustomization builds:
#   - deploy/flux-system/sources/k-orc.yaml sets spec.sparseCheckout. Without
#     it the source-controller populates the worktree with the tip of upstream
#     main and then hard-resets to the pinned commit, and that reset removes
#     every path the tip has and the pin lacks. Upstream added the symlink
#     charts/orc-crds/crds -> ../../config/crd/bases after the pin; go-git
#     fails to remove it ("remove .../config/crd/bases: directory not empty"),
#     the source never becomes Ready, and the c5c3-operator crash-loops for
#     want of the K-ORC CRDs. A sparse checkout starts from an empty worktree,
#     so nothing outside the listed directories is ever removed.
#   - the spec.path of deploy/flux-system/releases/k-orc.yaml lies inside a
#     sparse directory, so a path change (the planned return to ./dist) cannot
#     leave the Kustomization pointing at a directory the checkout skipped.
#   - the production overlay, the kind base and the metal-stack base all
#     render the field.
# Usage: bash tests/unit/deploy/korc_flux_source_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

KORC_SOURCE_YAML="$PROJECT_ROOT/deploy/flux-system/sources/k-orc.yaml"
KORC_RELEASE_YAML="$PROJECT_ROOT/deploy/flux-system/releases/k-orc.yaml"

# The entries of spec.sparseCheckout in the source manifest, one per line.
sparse_dirs() {
  awk '
    /^  sparseCheckout:[[:space:]]*$/ { in_list = 1; next }
    in_list && /^[[:space:]]*#/ { next }
    in_list && /^    - / { sub(/^    - /, ""); gsub(/["'\'']/, ""); print; next }
    in_list { in_list = 0 }
  ' "$KORC_SOURCE_YAML"
}

# --- Test 1: the source declares a sparse checkout ---
#             (source-level, no tools required)
test_source_declares_sparse_checkout() {
  echo "Test: deploy/flux-system/sources/k-orc.yaml sets spec.sparseCheckout"

  assert_not_empty "GitRepository/k-orc lists at least one sparse checkout directory" \
    "$(sparse_dirs)"
}

# --- Test 2: the Kustomization path lies inside a sparse directory ---
#             (source-level, no tools required)
test_kustomization_path_is_checked_out() {
  echo "Test: the k-orc Kustomization builds a path the sparse checkout materialises"

  local path dir covered=0
  path="$(awk '/^  path:[[:space:]]*/{print $2; exit}' "$KORC_RELEASE_YAML")"
  assert_not_empty "Kustomization/k-orc carries a spec.path" "$path"
  path="${path#./}"

  while IFS= read -r dir; do
    [[ -z "$dir" ]] && continue
    dir="${dir#./}"
    dir="${dir%/}"
    if [[ "$path" == "$dir" || "$path" == "$dir"/* ]]; then
      covered=1
    fi
  done < <(sparse_dirs)

  assert_eq "spec.path '${path}' is inside a spec.sparseCheckout directory" "1" "$covered"
}

# --- Test 3: every overlay that ships the source renders the field ---
test_overlays_render_sparse_checkout() {
  echo "Test: the production, kind and metal-stack overlays render GitRepository/k-orc with spec.sparseCheckout"

  local overlays=(deploy/flux-system deploy/kind/base deploy/lab/metal-stack/base)

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (${#overlays[@]} checks skipped)"
    SKIP=$((SKIP + ${#overlays[@]}))
    return
  fi
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (${#overlays[@]} checks skipped)"
    SKIP=$((SKIP + ${#overlays[@]}))
    return
  fi

  # An empty source list must not match an overlay that renders none either.
  local expected overlay rendered
  expected="$(sparse_dirs | paste -sd, -)"
  expected="${expected:-<no spec.sparseCheckout in the source manifest>}"

  for overlay in "${overlays[@]}"; do
    if ! rendered="$(kustomize build "$PROJECT_ROOT/$overlay" 2>&1)"; then
      echo "  FAIL: kustomize build $overlay failed:"
      echo "$rendered" | head -20
      FAIL=$((FAIL + 1))
      continue
    fi
    assert_eq "$overlay renders GitRepository/k-orc spec.sparseCheckout" \
      "$expected" \
      "$(printf '%s\n' "$rendered" | yq eval-all -r \
        'select(.kind == "GitRepository" and .metadata.name == "k-orc" and .metadata.namespace == "flux-system") | .spec.sparseCheckout // [] | join(",")' -)"
  done
}

# --- Run ---
test_source_declares_sparse_checkout
test_kustomization_path_is_checked_out
test_overlays_render_sparse_checkout

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
