#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the reference-docs cross-links stay intact:
#   1. docs/reference/infrastructure/e2e-deployment.md — the step list of
#      "## Deployment Sequence" names the Gateway API and the Envoy Gateway
#      CRD installs and Gateway/openstack-gw, and both installs stand, in
#      that order, between item 2 and item 3 of the list.
#   2. docs/reference/keystone/keystone-crd.md — the Basic Gateway
#      Exposure example carries a kind-specific admonition that
#        (a) links to the Quick Start Extended Access section
#            (../../quick-start-extended.md#access-keystone-from-your-local-machine),
#        (b) names the openstack-gw Gateway and the nip.io hostname, and
#        (c) mentions that status.endpoint actually resolves on a Quick
#            Start cluster (phrase: "status.endpoint").
#
# Usage: bash tests/unit/docs/reference_cross_links_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

E2E_DOC="$PROJECT_ROOT/docs/reference/infrastructure/e2e-deployment.md"
CRD_DOC="$PROJECT_ROOT/docs/reference/keystone/keystone-crd.md"

# --- Test 1: the e2e-deployment.md step list names the Gateway installs ---
test_e2e_step_list_has_gateway_installs() {
  echo "Test: e2e-deployment.md step list names the Gateway API and Envoy Gateway CRD installs"

  if [[ ! -f "$E2E_DOC" ]]; then
    echo "  FAIL: $E2E_DOC does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_file_contains "step list names the Gateway API CRD install" \
    "$E2E_DOC" \
    'Install Gateway API standard CRDs'
  assert_file_contains "step list names the Envoy Gateway CRD install" \
    "$E2E_DOC" \
    'Install Envoy Gateway CRDs'
  assert_file_contains "step list names Gateway/openstack-gw" \
    "$E2E_DOC" \
    'Gateway/openstack-gw'
}

# --- Test 2: both CRD installs sit in item 2 of the step list ---
# The page has a second numbered list, the teardown steps, above
# "## Deployment Sequence", so the anchors are searched from that heading to
# the next line that starts with "#".
test_e2e_gateway_installs_in_step_2() {
  echo "Test: the Gateway API and Envoy Gateway CRD installs sit between item 2 and item 3 of the step list"

  # One awk pass prints the four line numbers, an empty field for a missing
  # anchor. awk reads UTF-8 bytes as text in every locale, grep does not.
  local step2_line crds_line envoy_line step3_line
  IFS=: read -r step2_line crds_line envoy_line step3_line <<<"$(awk '
    /^## Deployment Sequence$/ { in_section = 1; next }
    in_section && /^#/ { exit }
    in_section && !step2 && /^2\. / { step2 = NR }
    in_section && !crds && index($0, "Install Gateway API standard CRDs") { crds = NR }
    in_section && !envoy && index($0, "Install Envoy Gateway CRDs") { envoy = NR }
    in_section && !step3 && /^3\. / { step3 = NR }
    END { print step2 ":" crds ":" envoy ":" step3 }
  ' "$E2E_DOC")"

  if [[ -z "$step2_line" || -z "$crds_line" || -z "$envoy_line" || -z "$step3_line" ]]; then
    echo "  FAIL: missing anchor line(s); step2=${step2_line:-<none>} CRDs=${crds_line:-<none>} envoy=${envoy_line:-<none>} step3=${step3_line:-<none>}"
    FAIL=$((FAIL + 1))
    return
  fi

  if (( step2_line < crds_line && crds_line < envoy_line && envoy_line < step3_line )); then
    echo "  PASS: item 2 (line $step2_line) < CRDs (line $crds_line) < Envoy CRDs (line $envoy_line) < item 3 (line $step3_line)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: expected item 2 < CRDs < Envoy CRDs < item 3 (got step2=$step2_line crds=$crds_line envoy=$envoy_line step3=$step3_line)"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 3: keystone-crd.md admonition links Quick Start ---
test_crd_admonition_links_quick_start() {
  echo "Test: keystone-crd.md Basic Gateway Exposure admonition links Quick Start"

  if [[ ! -f "$CRD_DOC" ]]; then
    echo "  FAIL: $CRD_DOC does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_file_contains "admonition links Quick Start Extended Access section" \
    "$CRD_DOC" \
    '../../quick-start-extended.md#access-keystone-from-your-local-machine'
  assert_file_contains "admonition mentions status.endpoint" \
    "$CRD_DOC" \
    'status.endpoint'
  assert_file_contains "admonition names openstack-gw" \
    "$CRD_DOC" \
    'openstack-gw'
  assert_file_contains "admonition mentions the kind-only nip.io hostname" \
    "$CRD_DOC" \
    'keystone.127-0-0-1.nip.io'
}

# --- Test 4: admonition sits near the Basic Gateway Exposure example ---
test_crd_admonition_near_example_heading() {
  echo "Test: admonition precedes the Basic Gateway Exposure yaml block"

  local heading_line admonition_line yaml_line
  heading_line="$( { grep -nF '### Example — Basic Gateway Exposure' "$CRD_DOC" || true; } | head -n1 | cut -d: -f1)"
  if [[ -z "$heading_line" ]]; then
    echo "  FAIL: could not locate '### Example — Basic Gateway Exposure' heading"
    FAIL=$((FAIL + 1))
    return
  fi

  # First admonition line after the heading is the "kind Quick Start note" lead-in.
  admonition_line="$( { tail -n +"$heading_line" "$CRD_DOC" | grep -nF 'kind Quick Start note' || true; } | head -n1 | cut -d: -f1)"
  # Convert relative line number back to absolute.
  if [[ -n "$admonition_line" ]]; then
    admonition_line=$((heading_line + admonition_line - 1))
  fi

  yaml_line="$( { tail -n +"$heading_line" "$CRD_DOC" | grep -nE '^```yaml' || true; } | head -n1 | cut -d: -f1)"
  if [[ -n "$yaml_line" ]]; then
    yaml_line=$((heading_line + yaml_line - 1))
  fi

  if [[ -z "$admonition_line" || -z "$yaml_line" ]]; then
    echo "  FAIL: could not locate admonition line and/or yaml block after the heading (adm=${admonition_line:-<none>} yaml=${yaml_line:-<none>})"
    FAIL=$((FAIL + 1))
    return
  fi

  if (( heading_line < admonition_line && admonition_line < yaml_line )); then
    echo "  PASS: admonition (line $admonition_line) sits between heading ($heading_line) and yaml block ($yaml_line)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: expected heading < admonition < yaml (got heading=$heading_line adm=$admonition_line yaml=$yaml_line)"
    FAIL=$((FAIL + 1))
  fi
}

# --- Run ---
test_e2e_step_list_has_gateway_installs
test_e2e_gateway_installs_in_step_2
test_crd_admonition_links_quick_start
test_crd_admonition_near_example_heading

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
