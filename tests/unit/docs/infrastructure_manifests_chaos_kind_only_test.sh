#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify docs/reference/infrastructure-manifests.md contracts for CC-0097:
#   - The Namespaces table no longer lists chaos-mesh as an always-on
#     production namespace.
#   - The HelmRelease–HelmRepository cross-reference table no longer lists
#     chaos-mesh as an always-on entry.
#   - A `### Chaos Mesh (kind-only opt-in)` subsection exists under the
#     `## Kind Overlay Demo Addons` section and points at
#     `deploy/kind/chaos-mesh/kustomization.yaml`.
#   - The kind-only subsection documents the WITH_CHAOS_MESH opt-in flag.
#
# (CC-0097, REQ-006)
#
# Usage: bash tests/unit/docs/infrastructure_manifests_chaos_kind_only_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

DOC="$PROJECT_ROOT/docs/reference/infrastructure-manifests.md"

if [[ ! -f "$DOC" ]]; then
  echo "FAIL: $DOC does not exist"
  exit 1
fi

# Extract the section between `## Namespaces` and the next `## ` heading.
extract_namespaces_section() {
  awk '
    /^## Namespaces[[:space:]]*$/ { in_block = 1; print; next }
    in_block && /^## / { exit }
    in_block { print }
  ' "$DOC"
}

# Extract the section between `## HelmRelease–HelmRepository Cross-Reference`
# and the next `## ` heading. The dash is a Unicode en-dash (U+2013) which
# encodes as three bytes in UTF-8 — POSIX awk's `.` matches a single byte,
# so use `.+` to span the multi-byte separator portably across awk impls.
extract_cross_reference_section() {
  awk '
    /^## HelmRelease.+HelmRepository Cross-Reference/ { in_block = 1; print; next }
    in_block && /^## / { exit }
    in_block { print }
  ' "$DOC"
}

# Extract the section between `## Kind Overlay Demo Addons` and EOF (or
# the next `## ` heading if one is added later).
extract_kind_overlay_section() {
  awk '
    /^## Kind Overlay Demo Addons[[:space:]]*$/ { in_block = 1; print; next }
    in_block && /^## / { exit }
    in_block { print }
  ' "$DOC"
}

# --- Test 1: Namespaces table has no chaos-mesh row (CC-0097, REQ-006) ---
test_namespaces_table_has_no_chaos_mesh_row() {
  echo "Test: Namespaces table no longer lists chaos-mesh as an always-on namespace (CC-0097, REQ-006)"

  local namespaces_section
  namespaces_section="$(extract_namespaces_section)"

  if [[ -z "$namespaces_section" ]]; then
    echo "  FAIL: '## Namespaces' section not found"
    FAIL=$((FAIL + 1))
    return
  fi

  # A namespace row uses the form `| `chaos-mesh` | <description> |`. We
  # anchor on the leading backticked name in the first column so prose
  # mentions of chaos-mesh in the surrounding paragraphs do not match.
  if printf '%s\n' "$namespaces_section" | grep -qE '^\|[[:space:]]*`chaos-mesh`'; then
    echo "  FAIL: Namespaces table still has a chaos-mesh row"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: Namespaces table has no chaos-mesh row"
    PASS=$((PASS + 1))
  fi
}

# --- Test 2: Cross-reference table has no chaos-mesh row (CC-0097, REQ-006) ---
test_cross_reference_table_has_no_chaos_mesh_row() {
  echo "Test: HelmRelease–HelmRepository cross-reference table has no chaos-mesh row (CC-0097, REQ-006)"

  local xref_section
  xref_section="$(extract_cross_reference_section)"

  if [[ -z "$xref_section" ]]; then
    echo "  FAIL: 'HelmRelease–HelmRepository Cross-Reference' section not found"
    FAIL=$((FAIL + 1))
    return
  fi

  # First column is the HelmRelease name in backticks. Prose mentions of
  # chaos-mesh outside table rows do not satisfy this regex.
  if printf '%s\n' "$xref_section" | grep -qE '^\|[[:space:]]*`chaos-mesh`'; then
    echo "  FAIL: Cross-reference table still has a chaos-mesh row"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: Cross-reference table has no chaos-mesh row"
    PASS=$((PASS + 1))
  fi
}

# --- Test 3: Kind-only opt-in subsection exists (CC-0097, REQ-006) ---
test_kind_only_subsection_present() {
  echo "Test: '### Chaos Mesh (kind-only opt-in)' subsection exists in 'Kind Overlay Demo Addons' (CC-0097, REQ-006)"

  local kind_section
  kind_section="$(extract_kind_overlay_section)"

  if [[ -z "$kind_section" ]]; then
    echo "  FAIL: '## Kind Overlay Demo Addons' section not found"
    FAIL=$((FAIL + 1))
    return
  fi

  if printf '%s\n' "$kind_section" | grep -qE '^### Chaos Mesh \(kind-only opt-in\)'; then
    echo "  PASS: '### Chaos Mesh (kind-only opt-in)' subsection found"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: '### Chaos Mesh (kind-only opt-in)' subsection missing"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 4: Subsection points at deploy/kind/chaos-mesh/kustomization.yaml (CC-0097, REQ-006) ---
test_kind_only_subsection_points_at_overlay_file() {
  echo "Test: Kind-only subsection 'File:' line points at deploy/kind/chaos-mesh/kustomization.yaml (CC-0097, REQ-006)"

  local subsection
  subsection="$(awk '
    /^### Chaos Mesh \(kind-only opt-in\)/ { in_block = 1 }
    in_block && /^### / && !/Chaos Mesh \(kind-only opt-in\)/ { exit }
    in_block && /^## / && !/Kind Overlay Demo Addons/ { exit }
    in_block { print }
  ' "$DOC")"

  if [[ -z "$subsection" ]]; then
    echo "  FAIL: Kind-only Chaos Mesh subsection not found — cannot check file pointer"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_contains \
    "Kind-only subsection points at deploy/kind/chaos-mesh/kustomization.yaml" \
    "$subsection" \
    "deploy/kind/chaos-mesh/kustomization.yaml"

  # Also check that the obsolete deploy/flux-system/releases/chaos-mesh.yaml
  # path (used pre-CC-0097) is NOT cited as the primary file in the
  # subsection's "File:" line. The on-disk file is reused from the kind
  # overlay, which the subsection prose can mention, but the **File:**
  # marker should point at the new overlay-root kustomization.
  if printf '%s\n' "$subsection" | grep -qE '^\*\*File:\*\*[[:space:]]+`deploy/flux-system/releases/chaos-mesh\.yaml`'; then
    echo "  FAIL: subsection 'File:' marker still points at the legacy production-base release path"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: subsection 'File:' marker no longer pins to the legacy production-base release path"
    PASS=$((PASS + 1))
  fi
}

# --- Test 5: Subsection documents WITH_CHAOS_MESH opt-in flag (CC-0097, REQ-006) ---
test_kind_only_subsection_documents_optin_flag() {
  echo "Test: Kind-only subsection documents 'WITH_CHAOS_MESH=true make deploy-infra' (CC-0097, REQ-006)"

  assert_file_contains \
    "infrastructure-manifests.md cites WITH_CHAOS_MESH=true make deploy-infra" \
    "$DOC" \
    'WITH_CHAOS_MESH=true make deploy-infra'
}

# --- Run ---
test_namespaces_table_has_no_chaos_mesh_row
test_cross_reference_table_has_no_chaos_mesh_row
test_kind_only_subsection_present
test_kind_only_subsection_points_at_overlay_file
test_kind_only_subsection_documents_optin_flag

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
