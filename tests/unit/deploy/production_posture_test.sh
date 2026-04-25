#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the CC-0088 Envoy Gateway work did NOT alter the production overlay
# (CC-0088, REQ-012). The operator stays platform-agnostic; kind-only
# addons must live under deploy/kind/ and deploy/flux-system/ must be
# byte-identical to origin/main for the following paths:
#
#   - deploy/flux-system/kustomization.yaml
#   - deploy/flux-system/fluxinstance.yaml
#   - deploy/flux-system/releases/*
#
# In addition, any net-new file under deploy/flux-system/sources/ must contain
# ONLY HelmRepository objects (no HelmRelease / Gateway controller leakage).
#
# Skipped when the run is not inside a git worktree (e.g., shipped tarballs)
# or when origin/main is unavailable.
#
# Usage: bash tests/unit/deploy/production_posture_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Preconditions — skip gracefully when git context is unavailable.
# ---------------------------------------------------------------------------
preflight() {
  if ! command -v git >/dev/null 2>&1; then
    return 1
  fi
  if ! git -C "$PROJECT_ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    return 1
  fi
  # Need a local ref for origin/main; CI clones should have it.
  if ! git -C "$PROJECT_ROOT" rev-parse --verify --quiet origin/main >/dev/null 2>&1; then
    return 1
  fi
  return 0
}

# --- Test 1: tracked production-overlay files are unchanged vs origin/main
#             (CC-0088, REQ-012) ---
test_production_overlay_unchanged() {
  echo "Test: deploy/flux-system/{kustomization,fluxinstance,releases/*} unchanged vs origin/main (CC-0088, REQ-012)"

  if ! preflight; then
    echo "  SKIP: git / origin/main unavailable (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  # Compare each path deterministically; `git diff --exit-code` returns
  # non-zero when the worktree differs from the ref.
  local paths=(
    "deploy/flux-system/kustomization.yaml"
    "deploy/flux-system/fluxinstance.yaml"
    "deploy/flux-system/releases"
  )

  local diff_output=""
  local status=0
  for path in "${paths[@]}"; do
    if ! git -C "$PROJECT_ROOT" diff --quiet origin/main -- "$path"; then
      status=1
      diff_output+=$'\n'"--- diff in $path ---"$'\n'
      diff_output+="$(git -C "$PROJECT_ROOT" diff origin/main -- "$path")"
    fi
  done

  if [[ "$status" -ne 0 ]]; then
    echo "  FAIL: production overlay has unexpected changes vs origin/main"
    echo "$diff_output" | head -80
    FAIL=$((FAIL + 1))
    return
  fi

  echo "  PASS: production overlay paths are byte-identical to origin/main"
  PASS=$((PASS + 1))
}

# --- Test 2: any added file under deploy/flux-system/sources/ contains
#             only HelmRepository objects (CC-0088, REQ-012) ---
test_added_sources_are_helmrepository_only() {
  echo "Test: added deploy/flux-system/sources/* files contain only HelmRepository objects (CC-0088, REQ-012)"

  if ! preflight; then
    echo "  SKIP: git / origin/main unavailable (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  # `git diff --name-status --diff-filter=A` lists files that exist in the
  # worktree but not in origin/main.
  local added
  added="$(git -C "$PROJECT_ROOT" diff --name-only --diff-filter=A origin/main -- \
    'deploy/flux-system/sources/' 2>/dev/null || true)"

  if [[ -z "$added" ]]; then
    echo "  PASS: no new files added under deploy/flux-system/sources/ (nothing to check)"
    PASS=$((PASS + 1))
    return
  fi

  local violations=0
  while IFS= read -r path; do
    [[ -z "$path" ]] && continue
    local abs="$PROJECT_ROOT/$path"
    if [[ ! -f "$abs" ]]; then
      continue
    fi
    # Extract all `kind:` values from the file; any non-HelmRepository value
    # is a policy violation per REQ-012.
    local offending
    offending="$(awk '/^kind:[[:space:]]+/{print $2}' "$abs" | grep -vE '^HelmRepository$' || true)"
    if [[ -n "$offending" ]]; then
      echo "  FAIL: $path contains non-HelmRepository kinds: $(echo "$offending" | tr '\n' ',' )"
      violations=$((violations + 1))
    fi
  done <<< "$added"

  if [[ "$violations" -gt 0 ]]; then
    FAIL=$((FAIL + 1))
    return
  fi

  echo "  PASS: all added sources files contain only HelmRepository objects"
  PASS=$((PASS + 1))
}

# --- Run ---
test_production_overlay_unchanged
test_added_sources_are_helmrepository_only

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
