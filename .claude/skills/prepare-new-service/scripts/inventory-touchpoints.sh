#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# Inventory the onboarding touch points for an OpenStack service across the
# five layers (image, operator, CI/e2e/deploy, ControlPlane, docs).
#
# Usage: inventory-touchpoints.sh <service>
#
# Prints [DONE]/[TODO] per touch point plus gotcha warnings. This is an
# inventory, not a gate: a fresh service is all-[TODO] by design. Always
# exits 0 unless invoked incorrectly.

set -euo pipefail

if [[ $# -ne 1 ]] || [[ ! "$1" =~ ^[a-z][a-z0-9-]*$ ]]; then
  echo "usage: $0 <service>   (lowercase, e.g. horizon)" >&2
  exit 2
fi

SERVICE="$1"
REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "${REPO_ROOT}"

DONE_COUNT=0
TODO_COUNT=0

check() { # <description> <command...>
  local desc="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    printf '  [DONE] %s\n' "${desc}"
    DONE_COUNT=$((DONE_COUNT + 1))
  else
    printf '  [TODO] %s\n' "${desc}"
    TODO_COUNT=$((TODO_COUNT + 1))
  fi
}

header() { printf '\n== %s ==\n' "$1"; }

header "Layer 1: container image"
check "images/${SERVICE}/Dockerfile" test -f "images/${SERVICE}/Dockerfile"
for refs in releases/*/source-refs.yaml; do
  check "${refs}: '${SERVICE}:' key" grep -Eq "^${SERVICE}:" "${refs}"
done
for pkgs in releases/*/extra-packages.yaml; do
  check "${pkgs}: '${SERVICE}:' key" grep -Eq "^${SERVICE}:" "${pkgs}"
done
check "tests/container-images/verify_${SERVICE}.sh" test -f "tests/container-images/verify_${SERVICE}.sh"
check "build-images.yaml hadolint matrix entry (static list)" \
  grep -q "images/${SERVICE}/Dockerfile" .github/workflows/build-images.yaml

header "Layer 2: service operator"
check "operators/${SERVICE}/go.mod" test -f "operators/${SERVICE}/go.mod"
check "go.work 'use ./operators/${SERVICE}'" grep -Eq "operators/${SERVICE}(\s|$)" go.work
check "Makefile OPERATORS includes '${SERVICE}'" \
  grep -Eq "^OPERATORS \?=.*\b${SERVICE}\b" Makefile
check "operators/${SERVICE}/helm/${SERVICE}-operator chart" \
  test -f "operators/${SERVICE}/helm/${SERVICE}-operator/Chart.yaml"
check "CRD base under operators/${SERVICE}/config/crd/bases/" \
  bash -c "ls operators/${SERVICE}/config/crd/bases/*.yaml"

header "Layer 3: CI / e2e / deploy"
check "ci.yaml FILTER_${SERVICE} env" grep -q "FILTER_${SERVICE}" .github/workflows/ci.yaml
check "ci.yaml ALL_OPERATORS includes '${SERVICE}'" \
  bash -c "grep -E 'ALL_OPERATORS' .github/workflows/ci.yaml | grep -qw '${SERVICE}'"
check "tests/ci/verify_${SERVICE}_ci_pipeline.sh" test -f "tests/ci/verify_${SERVICE}_ci_pipeline.sh"
check "tests/e2e/${SERVICE}/ chainsaw suites" bash -c "ls tests/e2e/${SERVICE}/*/chainsaw-test.yaml"
check "tests/e2e/${SERVICE}-operator/ chart-level suites (optional)" \
  bash -c "ls tests/e2e/${SERVICE}-operator/*/chainsaw-test.yaml"
check "tests/e2e-chaos scenarios exercising a ${SERVICE} CR" \
  bash -c "ls tests/e2e-chaos/*/00-${SERVICE}-cr.yaml"
check "tests/tempest/${SERVICE}-*/ config (skip if no tempest plugin)" \
  bash -c "ls -d tests/tempest/${SERVICE}-*/"
check "deploy/flux-system/releases/${SERVICE}-operator.yaml" \
  test -f "deploy/flux-system/releases/${SERVICE}-operator.yaml"

header "Layer 4: ControlPlane (c5c3) integration"
check "ServicesSpec has Service…Spec for '${SERVICE}'" \
  grep -qi "Service${SERVICE}Spec" operators/c5c3/api/v1alpha1/controlplane_types.go
check "operators/c5c3/internal/controller/reconcile_${SERVICE}.go" \
  test -f "operators/c5c3/internal/controller/reconcile_${SERVICE}.go"
check "c5c3 helm _helpers.tpl rbacRules mention '${SERVICE}'" \
  grep -q "${SERVICE}" operators/c5c3/helm/c5c3-operator/templates/_helpers.tpl

header "Layer 5: documentation"
check "docs/reference/${SERVICE}/ pages" bash -c "ls docs/reference/${SERVICE}/*.md"
check "VitePress sidebar references reference/${SERVICE}/" \
  grep -q "reference/${SERVICE}/" docs/.vitepress/config.ts

header "Gotcha warnings"
for uc in releases/*/upper-constraints.txt; do
  if grep -Eq "^${SERVICE}===" "${uc}"; then
    pin="$(grep -E "^${SERVICE}===" "${uc}")"
    printf '  [WARN] %s pins %s — source install must match the pin or strip it via overrides/<release>/constraints.txt\n' \
      "${uc}" "${pin}"
  fi
done
if [[ ! -f "architecture/docs/index.md" && ! -d "architecture/docs" ]]; then
  printf '  [WARN] architecture/ submodule not initialized — architecture chapters live in a separate repo (git submodule update --init architecture)\n'
fi

printf '\nSummary: %d done, %d todo (inventory only — todo is expected for a fresh service)\n' \
  "${DONE_COUNT}" "${TODO_COUNT}"
exit 0
