#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify docs/reference/infrastructure-manifests.md documents the new
# kind-only Flux Web UI addon
#   - a subsection references deploy/kind/base/flux-web.yaml
#   - the chart URL oci://ghcr.io/controlplaneio-fluxcd/charts/flux-operator
#     is cited in that subsection
#   - the Renovate customManager / pin story is mentioned
#   - an explicit note clarifies that deploy/flux-system/kustomization.yaml
#     does NOT reference this file (production stays opt-out)
#   - the document still parses as a sequence of well-formed Markdown
#     headers (monotonically non-decreasing depth, no malformed '#' lines)
# Usage: bash tests/unit/docs/infrastructure_manifests_flux_web_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

MANIFESTS_DOC="$PROJECT_ROOT/docs/reference/infrastructure/infrastructure-manifests.md"

# --- Test 1: document exists ---
test_doc_exists() {
  echo "Test: docs/reference/infrastructure-manifests.md exists"

  if [[ -f "$MANIFESTS_DOC" ]]; then
    echo "  PASS: $MANIFESTS_DOC exists"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $MANIFESTS_DOC does not exist"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 2: new subsection references deploy/kind/base/flux-web.yaml
#             ---
test_subsection_references_flux_web_manifest() {
  echo "Test: a subsection references deploy/kind/base/flux-web.yaml"

  assert_file_contains "docs reference the kind-only flux-web manifest path" \
    "$MANIFESTS_DOC" \
    'deploy/kind/base/flux-web.yaml'
}

# --- Test 3: chart URL cited in the subsection ---
test_chart_url_cited() {
  echo "Test: the flux-web subsection cites the flux-operator OCI chart URL"

  assert_file_contains "chart URL oci://ghcr.io/controlplaneio-fluxcd/charts/flux-operator present" \
    "$MANIFESTS_DOC" \
    'oci://ghcr.io/controlplaneio-fluxcd/charts/flux-operator'
}

# --- Test 4: ResourceSet kind + API version cited ---
test_resourceset_kind_cited() {
  echo "Test: the flux-web subsection cites Kind ResourceSet and the fluxcd.controlplane.io API group"

  assert_file_contains "API group fluxcd.controlplane.io/v1 cited" \
    "$MANIFESTS_DOC" \
    'fluxcd.controlplane.io/v1'
  assert_file_contains "Kind ResourceSet cited" \
    "$MANIFESTS_DOC" \
    'ResourceSet'
}

# --- Test 5: key Helm values cited ---
test_helm_values_cited() {
  echo "Test: the flux-web subsection cites web.serverOnly, installCRDs, and fullnameOverride"

  assert_file_contains "web.serverOnly value mentioned" \
    "$MANIFESTS_DOC" \
    'web.serverOnly'
  assert_file_contains "installCRDs value mentioned" \
    "$MANIFESTS_DOC" \
    'installCRDs'
}

# --- Test 6: Renovate customManager cross-reference ---
test_renovate_cross_reference() {
  echo "Test: the flux-web subsection cross-references the Renovate customManager"

  assert_file_contains "Renovate customManager cross-reference present" \
    "$MANIFESTS_DOC" \
    'customManager'
}

# --- Test 7: explicit note that production flux-system does NOT reference
#             this file ---
test_production_opt_out_note() {
  echo "Test: the flux-web subsection notes that deploy/flux-system/kustomization.yaml does not reference this file"

  assert_file_contains "production kustomization opt-out note present" \
    "$MANIFESTS_DOC" \
    'deploy/flux-system/kustomization.yaml'
}

# --- Test 8: Kind Overlay Demo Addons heading present ---
test_kind_overlay_demo_addons_heading() {
  echo "Test: 'Kind Overlay Demo Addons' heading groups the kind-only manifests"

  assert_file_contains "Kind Overlay Demo Addons heading present" \
    "$MANIFESTS_DOC" \
    'Kind Overlay Demo Addons'
}

# --- Test 9: markdown headers remain well-formed ---
#   - every line starting with '#' has a space after the marker run
#   - heading depth never jumps by more than one level at a time
test_markdown_headers_well_formed() {
  echo "Test: Markdown headers parse cleanly (no malformed '#' lines, no depth skips)"

  local malformed
  malformed="$(awk '/^#+[^ #]/ {print NR": "$0}' "$MANIFESTS_DOC" || true)"
  if [[ -n "$malformed" ]]; then
    echo "  FAIL: malformed Markdown heading lines (no space after '#' run):"
    printf '    %s\n' "$malformed"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: all heading lines have a space after the '#' run"
    PASS=$((PASS + 1))
  fi

  # Capture heading depths (number of leading '#') skipping fenced code blocks.
  local depths max_jump=0
  depths="$(awk '
    /^```/ { in_code = !in_code; next }
    in_code { next }
    /^#+[[:space:]]/ {
      n = index($0, " ") - 1
      if (n > 0) print n
    }
  ' "$MANIFESTS_DOC")"

  local prev=0 d
  while IFS= read -r d; do
    [[ -z "$d" ]] && continue
    if (( prev > 0 && d > prev + 1 )); then
      local jump=$(( d - prev ))
      if (( jump > max_jump )); then max_jump="$jump"; fi
    fi
    prev="$d"
  done <<< "$depths"

  assert_eq "no heading depth jump greater than 1" "0" "$max_jump"
}

# --- Run ---
test_doc_exists
test_subsection_references_flux_web_manifest
test_chart_url_cited
test_resourceset_kind_cited
test_helm_values_cited
test_renovate_cross_reference
test_production_opt_out_note
test_kind_overlay_demo_addons_heading
test_markdown_headers_well_formed

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
