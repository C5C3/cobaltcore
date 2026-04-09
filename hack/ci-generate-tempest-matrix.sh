#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-generate-tempest-matrix.sh — Generate Tempest release matrix from releases/ directories.
# Feature: CC-0051
#
# Scans releases/*/ directories and builds a JSON matrix for the Tempest CI job.
# Each release requires a matching Tempest config directory at
# tests/tempest/keystone-<slug>/ (e.g. keystone-2025-2 for release 2025.2).
#
# Required env vars:
#   GITHUB_OUTPUT — GitHub Actions output file (set automatically by Actions)
#
# REQ-001: Extracted from ci.yaml inline script (CC-0051, review #2).
# REQ-007: set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

# Default GITHUB_OUTPUT to /dev/null for local execution (CC-0051).
: "${GITHUB_OUTPUT:=/dev/null}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---------------------------------------------------------------------------
# 1. Discover release directories
# ---------------------------------------------------------------------------
shopt -s nullglob
dirs=("${REPO_ROOT}"/releases/*/)
entries=()

for d in "${dirs[@]}"; do
  release="${d%/}"
  release="${release##*/}"
  slug="${release//./-}"
  config_dir="tests/tempest/keystone-${slug}"
  if [[ ! -d "${REPO_ROOT}/${config_dir}" ]]; then
    echo "::error::Missing Tempest config directory: ${config_dir} (for release ${release})"
    exit 1
  fi
  cr_name="keystone-tempest-${slug}"
  svc_name="keystone-tempest-${slug}-api"
  entries+=("{\"release\":\"${release}\",\"config-dir\":\"${config_dir}\",\"cr-name\":\"${cr_name}\",\"service-k8s-name\":\"${svc_name}\"}")
done

# ---------------------------------------------------------------------------
# 2. Emit matrix JSON to GITHUB_OUTPUT
# ---------------------------------------------------------------------------
if [[ ${#entries[@]} -eq 0 ]]; then
  echo 'tempest-releases={"include":[]}' >> "$GITHUB_OUTPUT"
else
  matrix=$(printf '%s\n' "${entries[@]}" | jq -sc '{"include": .}')
  echo "tempest-releases=${matrix}" >> "$GITHUB_OUTPUT"
fi
