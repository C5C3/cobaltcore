#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the Envoy Gateway work did NOT alter the production overlay
# The operator stays platform-agnostic; kind-only
# addons must live under deploy/kind/ and deploy/flux-system/ must be
# byte-identical to origin/main for the following paths:
#
#   - deploy/flux-system/fluxinstance.yaml
#   - deploy/flux-system/releases/*
#
# deliberately removes chaos-mesh from the production overlay
# entirely:
#   - the `sources/chaos-mesh.yaml` and `releases/chaos-mesh.yaml` files
#     are relocated into the kind-only opt-in overlay at
#     deploy/kind/chaos-mesh/ so the overlay is self-contained (no
#     parent-directory references — kubectl#948); and
#   - kustomization.yaml drops the corresponding entries from `resources:`.
# kustomization.yaml and the two relocated chaos-mesh files are therefore
# excluded from the byte-identity check. The chaos_mesh_overlay_test.sh
# suite asserts the resulting posture (production renders zero chaos-mesh
# resources; the overlay renders the full bundle).
#
# enforces OpenBao mTLS by editing
# deploy/flux-system/releases/openbao.yaml in place:
#   - the HA raft listener requires & verifies client certs
#     (tls_client_ca_file + tls_require_and_verify_client_cert = true);
#   - all three retry_join stanzas carry leader_client_cert_file +
#     leader_client_key_file pointing at /openbao/client-tls; and
#   - server.volumes/volumeMounts mount the openbao-client-tls Secret
#     at /openbao/client-tls.
# openbao.yaml is therefore excluded from the byte-identity check.
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
#             ---
test_production_overlay_unchanged() {
  echo "Test: deploy/flux-system/{fluxinstance,releases/*} unchanged vs origin/main"

  if ! preflight; then
    echo "  SKIP: git / origin/main unavailable (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  # `git diff --exit-code` returns non-zero when the worktree differs from
  # the ref. The chaos-mesh relocation deliberately removes
  # deploy/flux-system/releases/chaos-mesh.yaml (now at
  # deploy/kind/chaos-mesh/release.yaml), so it is carved out per-path with
  # `:(exclude)…` pathspecs. fluxinstance.yaml has no carve-out — it must
  # remain byte-identical.
  local groups_label=(
    "deploy/flux-system/fluxinstance.yaml"
    "deploy/flux-system/releases (excluding chaos-mesh.yaml relocated, openbao.yaml edited in place, external-secrets.yaml edited in place, keystone-operator.yaml/horizon-operator.yaml edited in place for the image-digest valuesFrom, and the c5c3-operator.yaml/k-orc.yaml/garage-operator.yaml/glance-operator.yaml releases added)"
  )
  local groups_specs=(
    "deploy/flux-system/fluxinstance.yaml"
    "deploy/flux-system/releases :(exclude)deploy/flux-system/releases/chaos-mesh.yaml :(exclude)deploy/flux-system/releases/openbao.yaml :(exclude)deploy/flux-system/releases/external-secrets.yaml :(exclude)deploy/flux-system/releases/c5c3-operator.yaml :(exclude)deploy/flux-system/releases/k-orc.yaml :(exclude)deploy/flux-system/releases/garage-operator.yaml :(exclude)deploy/flux-system/releases/keystone-operator.yaml :(exclude)deploy/flux-system/releases/horizon-operator.yaml :(exclude)deploy/flux-system/releases/glance-operator.yaml"
  )

  local diff_output=""
  local status=0
  local i
  # shellcheck disable=SC2068  # intentional word-splitting of the pathspec list
  for i in "${!groups_specs[@]}"; do
    # Use read -a to split the space-separated spec into pathspec args.
    local -a specs
    read -r -a specs <<< "${groups_specs[$i]}"
    if ! git -C "$PROJECT_ROOT" diff --quiet origin/main -- "${specs[@]}"; then
      status=1
      diff_output+=$'\n'"--- diff in ${groups_label[$i]} ---"$'\n'
      diff_output+="$(git -C "$PROJECT_ROOT" diff origin/main -- "${specs[@]}")"
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
#             only HelmRepository objects ---
test_added_sources_are_helmrepository_only() {
  echo "Test: added deploy/flux-system/sources/* files contain only HelmRepository objects"

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
    # is a policy violation.
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
