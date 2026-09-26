#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the sizing patches of the kind base overlay, which keep the devstack
# within the one 4 vCPU / 16 GiB node that the e2e-controlplane job checks
# with hack/ci-check-node-budget.sh:
#   - every CobaltCore operator HelmRelease renders spec.values.replicas: 1;
#     the nine service operators stay suspended, and c5c3-operator stays
#     active because the CONTROLPLANE_OPERATORS=flux path of
#     hack/deploy-infra.sh relies on the base applying it active;
#   - the operator patches keep the production values they merge into;
#   - OpenBao renders a 100m CPU request and keeps its memory request, its
#     memory limit and standalone mode;
#   - FluxInstance/flux carries one spec.kustomize.patches entry per Flux
#     controller, each a JSON6902 add of 25m at the CPU request;
#   - deploy/flux-system/ (production) keeps two operator replicas, OpenBao's
#     250m and a FluxInstance without spec.kustomize.
#
# Usage: bash tests/unit/deploy/kind_base_sizing_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

KIND_BASE_DIR="$PROJECT_ROOT/deploy/kind/base"
FLUX_SYSTEM_DIR="$PROJECT_ROOT/deploy/flux-system"

SERVICE_OPERATORS="keystone horizon glance placement barbican ovn neutron cinder nova"
FLUX_CONTROLLERS="source-controller kustomize-controller helm-controller notification-controller"

# render <dir> <file>
# Renders <dir> into <file>; prints kustomize's stderr and returns non-zero on
# a failed build.
render() {
  local dir="$1" out="$2" err
  if ! err="$(kustomize build "$dir" 2>&1 >"$out")"; then
    echo "  FAIL: kustomize build $dir failed:"
    echo "$err" | head -20
    return 1
  fi
}

# field <file> <selector> <path>
# Prints <path> of the documents in <file> that <selector> matches.
field() {
  yq eval-all "select($2) | $3" "$1"
}

helmrelease() {
  echo ".kind == \"HelmRelease\" and .metadata.name == \"$1\""
}

# ---------------------------------------------------------------------------
# Test 1: every operator runs one replica; the service operators stay suspended
# ---------------------------------------------------------------------------
test_operator_replicas() {
  local file="$1" op
  echo "Test: the kind base runs one replica per operator"

  for op in $SERVICE_OPERATORS; do
    assert_eq "${op}-operator renders spec.values.replicas: 1" \
      "1" "$(field "$file" "$(helmrelease "${op}-operator")" '.spec.values.replicas')"
    assert_eq "${op}-operator stays suspended" \
      "true" "$(field "$file" "$(helmrelease "${op}-operator")" '.spec.suspend')"
  done

  assert_eq "c5c3-operator renders spec.values.replicas: 1" \
    "1" "$(field "$file" "$(helmrelease c5c3-operator)" '.spec.values.replicas')"
  assert_eq "c5c3-operator is not suspended by the base (the flux path needs it active)" \
    "null" "$(field "$file" "$(helmrelease c5c3-operator)" '.spec.suspend')"

  assert_eq "keystone-operator keeps leaderElection.enabled from production" \
    "true" "$(field "$file" "$(helmrelease keystone-operator)" '.spec.values.leaderElection.enabled')"
  assert_eq "keystone-operator keeps image.tag from production" \
    "latest" "$(field "$file" "$(helmrelease keystone-operator)" '.spec.values.image.tag')"
  assert_eq "keystone-operator keeps its valuesFrom entry" \
    "1" "$(field "$file" "$(helmrelease keystone-operator)" '.spec.valuesFrom | length')"
}

# ---------------------------------------------------------------------------
# Test 2: OpenBao asks 100m CPU and keeps everything else
# ---------------------------------------------------------------------------
test_openbao_cpu() {
  local file="$1" sel
  sel="$(helmrelease openbao)"
  echo "Test: the kind base lowers OpenBao's CPU request only"

  assert_eq "OpenBao requests 100m CPU" \
    "100m" "$(field "$file" "$sel" '.spec.values.server.resources.requests.cpu')"
  assert_eq "OpenBao keeps its 256Mi memory request" \
    "256Mi" "$(field "$file" "$sel" '.spec.values.server.resources.requests.memory')"
  assert_eq "OpenBao keeps its 512Mi memory limit" \
    "512Mi" "$(field "$file" "$sel" '.spec.values.server.resources.limits.memory')"
  assert_eq "OpenBao has no CPU limit" \
    "null" "$(field "$file" "$sel" '.spec.values.server.resources.limits.cpu')"
  assert_eq "OpenBao stays in standalone mode" \
    "true" "$(field "$file" "$sel" '.spec.values.server.standalone.enabled')"
}

# ---------------------------------------------------------------------------
# Test 3: FluxInstance/flux lowers each controller's CPU request to 25m
# ---------------------------------------------------------------------------
test_flux_controllers() {
  local file="$1" sel ctrl patch
  sel='.kind == "FluxInstance" and .metadata.name == "flux"'
  echo "Test: FluxInstance/flux carries one CPU patch per controller"

  assert_eq "four spec.kustomize.patches entries" \
    "4" "$(field "$file" "$sel" '.spec.kustomize.patches | length')"
  assert_eq "the entries target the four controllers" \
    "$FLUX_CONTROLLERS" \
    "$(field "$file" "$sel" '[.spec.kustomize.patches[].target.name] | join(" ")')"
  assert_eq "every entry targets a Deployment" \
    "Deployment" \
    "$(field "$file" "$sel" '[.spec.kustomize.patches[].target.kind] | unique | join(" ")')"

  for ctrl in $FLUX_CONTROLLERS; do
    patch="$(field "$file" "$sel" ".spec.kustomize.patches[] | select(.target.name == \"$ctrl\") | .patch")"
    assert_eq "$ctrl: the patch is one add of 25m at the CPU request" \
      "add /spec/template/spec/containers/0/resources/requests/cpu 25m" \
      "$(printf '%s\n' "$patch" | yq '.[0].op + " " + .[0].path + " " + .[0].value')"
    assert_eq "$ctrl: the patch holds exactly one operation" \
      "1" "$(printf '%s\n' "$patch" | yq 'length')"
  done

  assert_eq "the Flux distribution is untouched" \
    "2.x" "$(field "$file" "$sel" '.spec.distribution.version')"
}

# ---------------------------------------------------------------------------
# Test 4: the production overlay keeps its own sizing
# ---------------------------------------------------------------------------
test_production_unchanged() {
  local file="$1"
  echo "Test: deploy/flux-system/ keeps the production sizing"

  assert_eq "production keystone-operator keeps two replicas" \
    "2" "$(field "$file" "$(helmrelease keystone-operator)" '.spec.values.replicas')"
  assert_eq "production c5c3-operator keeps two replicas" \
    "2" "$(field "$file" "$(helmrelease c5c3-operator)" '.spec.values.replicas')"
  assert_eq "production OpenBao keeps its 250m CPU request" \
    "250m" "$(field "$file" "$(helmrelease openbao)" '.spec.values.server.resources.requests.cpu')"
  assert_eq "production FluxInstance has no spec.kustomize" \
    "null" "$(field "$file" '.kind == "FluxInstance"' '.spec.kustomize')"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if ! command -v kustomize >/dev/null 2>&1 || ! command -v yq >/dev/null 2>&1; then
  echo "SKIP: kustomize or yq not installed"
  SKIP=$((SKIP + 1))
else
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  if render "$KIND_BASE_DIR" "$tmp/kind.yaml"; then
    test_operator_replicas "$tmp/kind.yaml"
    test_openbao_cpu "$tmp/kind.yaml"
    test_flux_controllers "$tmp/kind.yaml"
  else
    FAIL=$((FAIL + 1))
  fi
  if render "$FLUX_SYSTEM_DIR" "$tmp/prod.yaml"; then
    test_production_unchanged "$tmp/prod.yaml"
  else
    FAIL=$((FAIL + 1))
  fi
fi

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
