#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# validate-guide.sh — mechanical guide-convention checks for docs/guides/.
# Verifies the conventions from docs/contributing/guide-conventions.md that
# the docs gate (tests/unit/docs/guide_devstack_and_tested_by_test.sh) does
# not cover:
#   V1  no banned placeholder name ('keystone-default') — the general
#       "example names no tutorial produces" judgment is prose-level and
#       belongs to the SKILL.md checklist
#   V2  devstack link and WITH_CONTROLPLANE=true flag agree inside the
#       '::: info Devstack' container
#   V3  a bash fence running 'helm upgrade'/'helm install' against the
#       published operator chart (oci://ghcr.io/c5c3/charts/) requires the
#       guide to frame Flux ownership (a 'HelmRelease' mention)
#   V4  a mutating kubectl verb on an operator-projected child CR
#       (kind keystone/horizon, name controlplane-keystone/-horizon)
#       requires a '::: warning' container with revert/projected language
#
# The missing-devstack-block / missing-tested-by half of the conventions is
# owned by the docs gate; pass --full to chain it and trust its verdict.
# A multi-line 'kubectl apply' that overwrites a projected child is not
# mechanically detectable — the SKILL.md prose checklist owns that case.
#
# NOTE: the devstack/flag mapping in V2 MUST stay in sync with the devstack
# table in docs/contributing/guide-conventions.md — that page is the prose
# source of truth; this script is the machine copy.
#
# Usage: validate-guide.sh [--full] [<guide.md>...]
# Defaults to every docs/guides/*.md. Exit code 1 on any [FAIL].

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
GATE="tests/unit/docs/guide_devstack_and_tested_by_test.sh"

FULL=0
FILES=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --full)
      FULL=1
      ;;
    -*)
      echo "usage: $0 [--full] [<guide.md>...]" >&2
      exit 2
      ;;
    *)
      if [[ ! -f "$1" ]]; then
        echo "error: no such guide: $1" >&2
        exit 2
      fi
      FILES+=("$1")
      ;;
  esac
  shift
done
if [[ "${#FILES[@]}" -eq 0 ]]; then
  FILES=("${REPO_ROOT}"/docs/guides/*.md)
fi

FAIL_COUNT=0
fail() { echo "[FAIL] $*"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
pass() { echo "[PASS] $*"; }
info() { echo "[INFO] $*"; }

# Body of the first "::: info Devstack" container (same extractor as the
# docs gate), including the fence lines.
devstack_container() {
  awk '
    /^::: info Devstack/ { f = 1; print; next }
    f && /^:::[[:space:]]*$/ { print; exit }
    f { print }
  ' "$1"
}

check_guide() {
  local file="$1"

  # V1 — banned placeholder names (guide-conventions.md "ControlPlane-first
  # naming"): 'keystone-default' is the retired placeholder no tutorial
  # produces.
  local hits h
  hits="$(grep -n 'keystone-default' "${file}" || true)"
  if [[ -n "${hits}" ]]; then
    while IFS= read -r h; do
      fail "V1 ${file}:${h%%:*}: banned placeholder name 'keystone-default' — no tutorial produces it"
    done <<< "${hits}"
  else
    pass "V1 ${file}: no banned placeholder names"
  fi

  # V2 — devstack link and bring-up flag agree (guide-conventions.md "One
  # devstack per guide"): the ControlPlane devstack link and the
  # WITH_CONTROLPLANE=true flag imply each other.
  local container links_cp=0 has_flag=0
  container="$(devstack_container "${file}")"
  if [[ -z "${container}" ]]; then
    info "V2 ${file}: no '::: info Devstack' container — structure is owned by ${GATE} (run --full)"
  else
    printf '%s\n' "${container}" | grep -qF '](../quick-start-controlplane.md' && links_cp=1
    printf '%s\n' "${container}" | grep -qF 'WITH_CONTROLPLANE=true' && has_flag=1
    if [[ "${links_cp}" -eq 1 && "${has_flag}" -eq 0 ]]; then
      fail "V2 ${file}: devstack links quick-start-controlplane.md but the bring-up command lacks WITH_CONTROLPLANE=true"
    elif [[ "${links_cp}" -eq 0 && "${has_flag}" -eq 1 ]]; then
      fail "V2 ${file}: bring-up command names WITH_CONTROLPLANE=true but the devstack link is not quick-start-controlplane.md"
    else
      pass "V2 ${file}: devstack link and WITH_CONTROLPLANE flag agree"
    fi
  fi

  # V3 — raw helm against Flux-owned releases (guide-conventions.md via the
  # deployment model): a bash fence that runs helm upgrade/install against
  # the published OCI operator chart is the canonical non-kind path ONLY when
  # the guide frames Flux ownership — on the kind devstacks the operator is a
  # Flux-owned HelmRelease and a raw helm upgrade is reverted. An oci://
  # mention in prose (outside a helm fence) does not trigger.
  local fence_lines line
  fence_lines="$(awk '
    /^```bash/ { infence = 1; start = NR; helm = 0; oci = 0; next }
    infence && /^```[[:space:]]*$/ {
      if (helm && oci) print start
      infence = 0
      next
    }
    infence && /helm (upgrade|install)/ { helm = 1 }
    infence && /oci:\/\/ghcr\.io\/c5c3\/charts\// { oci = 1 }
  ' "${file}")"
  if [[ -z "${fence_lines}" ]]; then
    pass "V3 ${file}: no raw-helm fence against the published operator chart"
  elif grep -q 'HelmRelease' "${file}"; then
    pass "V3 ${file}: raw-helm fence(s) present and the guide frames Flux ownership (mentions HelmRelease)"
  else
    while IFS= read -r line; do
      fail "V3 ${file}:${line}: bash fence runs raw helm against oci://ghcr.io/c5c3/charts/ but the guide never mentions HelmRelease (Flux-ownership framing)"
    done <<< "${fence_lines}"
  fi

  # V4 — projected-child edits need a revert warning (guide-conventions.md
  # "Never edit operator-projected children"): a mutating kubectl verb on the
  # projected keystone/horizon CR requires a '::: warning' container with
  # projected/revert language. The kind token must sit adjacent to the child
  # name so Secrets/ExternalSecrets named controlplane-keystone-* do not
  # trigger.
  local mut_re='kubectl[^`]*[[:space:]](patch|edit|scale|set|annotate|label|delete)[[:space:]](.*[[:space:]])?(keystone|horizon)s?(\.[a-z0-9.]*)?[[:space:]]+(controlplane-keystone|controlplane-horizon)([^a-z0-9-]|$)'
  hits="$(grep -nE "${mut_re}" "${file}" || true)"
  if [[ -z "${hits}" ]]; then
    pass "V4 ${file}: no mutating kubectl command on a projected child CR"
  elif grep -qE '^::: warning' "${file}" && grep -qiE '(revert|projected)' "${file}"; then
    pass "V4 ${file}: projected-child mutation(s) covered by a '::: warning' container with revert/projected language"
  else
    while IFS= read -r h; do
      fail "V4 ${file}:${h%%:*}: mutating kubectl on a projected child CR without a '::: warning' revert warning"
    done <<< "${hits}"
  fi
}

for f in "${FILES[@]}"; do
  echo "== ${f} =="
  check_guide "${f}"
done

if [[ "${FULL}" -eq 1 ]]; then
  echo
  echo "== authoritative gate: ${GATE} =="
  if bash "${REPO_ROOT}/${GATE}"; then
    pass "authoritative gate green"
  else
    fail "authoritative gate red — trust its verdict over this script's"
  fi
fi

echo
if [[ "${FAIL_COUNT}" -gt 0 ]]; then
  echo "Result: ${FAIL_COUNT} FAIL"
  exit 1
fi
echo "Result: all checks passed"
exit 0
