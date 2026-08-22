#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-generate-cleanup-matrix.sh — Derive the GHCR package lists for the
# image cleanup jobs from the source tree.
#
# Every container package this repo publishes has a directory: images/<name>/
# becomes ghcr.io/<owner>/<name>, operators/<name>/ becomes
# ghcr.io/<owner>/<name>-operator. Deriving the lists from those directories
# keeps a newly onboarded service from silently losing its cleanup coverage --
# keystone-federation-proxy sat outside every hardcoded matrix and accumulated
# 352 stale e2e tags before anyone noticed.
#
# Helm chart packages (charts/<op>-operator) are deliberately out of scope: they
# carry nothing but release semver tags.
#
# Optional env vars:
#   CLEANUP_PACKAGES — space- or comma-separated subset to restrict both lists
#                      to, for a targeted workflow_dispatch run
#
# Outputs written to GITHUB_OUTPUT (printed to stdout when run outside CI):
#   cleanup-packages     — every published container package
#   cleanup-e2e-packages — the subset build-e2e-images pushes run-scoped tags
#                          for; the base images are built inline, never tagged
#                          per run (see the "Push E2E images to GHCR" step)

set -euo pipefail

# Base images are consumed as build contexts, not deployed into E2E clusters,
# so build-e2e-images never gives them an e2e-<run_id>-* tag.
E2E_EXCLUDED="python-base venv-builder"

shopt -s nullglob

packages=()

for image_dir in images/*/; do
  packages+=("$(basename "${image_dir}")")
done

for operator_dir in operators/*/; do
  operator="$(basename "${operator_dir}")"
  # operators/shared/ holds cross-operator Go packages, not an operator image.
  [[ "${operator}" == "shared" ]] && continue
  packages+=("${operator}-operator")
done

if [[ ${#packages[@]} -eq 0 ]]; then
  echo "::error::No container packages found under images/ or operators/"
  exit 1
fi

# A workflow_dispatch run may target a subset; an unknown name is a typo worth
# failing on rather than a silently empty matrix.
if [[ -n "${CLEANUP_PACKAGES:-}" ]]; then
  requested=()
  for name in ${CLEANUP_PACKAGES//,/ }; do
    known=false
    for package in "${packages[@]}"; do
      [[ "${package}" == "${name}" ]] && known=true
    done
    if [[ "${known}" == false ]]; then
      echo "::error::Unknown package '${name}'; known packages: ${packages[*]}"
      exit 1
    fi
    requested+=("${name}")
  done
  packages=("${requested[@]}")
fi

e2e_packages=()
for package in "${packages[@]}"; do
  skip=false
  for excluded in ${E2E_EXCLUDED}; do
    [[ "${package}" == "${excluded}" ]] && skip=true
  done
  [[ "${skip}" == true ]] && continue
  e2e_packages+=("${package}")
done

to_json_array() {
  printf '%s\n' "$@" | sort -u | jq -Rsc 'split("\n") | map(select(length > 0))'
}

all_json=$(to_json_array "${packages[@]}")
e2e_json=$(to_json_array "${e2e_packages[@]}")

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  {
    echo "cleanup-packages=${all_json}"
    echo "cleanup-e2e-packages=${e2e_json}"
  } >> "${GITHUB_OUTPUT}"
fi

echo "cleanup-packages=${all_json}"
echo "cleanup-e2e-packages=${e2e_json}"
