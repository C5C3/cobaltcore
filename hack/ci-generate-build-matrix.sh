#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-generate-build-matrix.sh — Generate CI build matrices from release directories.
# Feature: CC-0055
#
# Scans releases/*/ directories, reads source-refs.yaml to build service×release
# matrices for build, test, and Tempest jobs.
#
# Required env vars:
#   GITHUB_EVENT_NAME — GitHub Actions event type (push, pull_request, etc.)
#
# Outputs written to GITHUB_OUTPUT:
#   matrix               — {service, release} pairs for test/verify jobs
#   build-matrix         — {service, release, platform, runner} for build jobs
#   tempest-matrix       — {release, platform, runner} for Tempest build jobs
#   tempest-release-matrix — {release} for Tempest merge jobs
#
# REQ-006: Extracted from inline workflow step to standalone script.

set -euo pipefail

GITHUB_EVENT_NAME="${GITHUB_EVENT_NAME:?GITHUB_EVENT_NAME is required}"

# ---------------------------------------------------------------------------
# 1. Discover release directories
# ---------------------------------------------------------------------------
shopt -s nullglob
dirs=(releases/*/)

if [[ ${#dirs[@]} -eq 0 ]]; then
  echo "::error::No release directories found under releases/"
  echo "matrix={\"include\":[]}" >> "$GITHUB_OUTPUT"
  echo "build-matrix={\"include\":[]}" >> "$GITHUB_OUTPUT"
  echo "tempest-matrix={\"include\":[]}" >> "$GITHUB_OUTPUT"
  echo "tempest-release-matrix={\"include\":[]}" >> "$GITHUB_OUTPUT"
  exit 0
fi

# ---------------------------------------------------------------------------
# 2. Collect {service, release} pairs
# ---------------------------------------------------------------------------
pairs=$(
  for release_dir in "${dirs[@]}"; do
    release="${release_dir%/}"
    release="${release#releases/}"
    while IFS= read -r service; do
      echo "{\"service\":\"${service}\",\"release\":\"${release}\"}"
    done < <(yq 'keys | .[]' "releases/${release}/source-refs.yaml")
  done | jq -sc '.'
)

echo "matrix=$(echo "$pairs" | jq -c '{"include": .}')" >> "$GITHUB_OUTPUT"

# ---------------------------------------------------------------------------
# 3. Build matrix: {service, release, platform, runner}
# ---------------------------------------------------------------------------
# ARM64 is excluded on pull_request to save CI time.
if [[ "${GITHUB_EVENT_NAME}" == "pull_request" ]]; then
  platforms='[{"platform":"linux/amd64","runner":"ubuntu-latest"}]'
else
  platforms='[{"platform":"linux/amd64","runner":"ubuntu-latest"},{"platform":"linux/arm64","runner":"ubuntu-24.04-arm"}]'
fi

build_matrix=$(echo "$pairs" | jq -c \
  --argjson p "$platforms" \
  '[.[] as $sr | $p[] | {service: $sr.service, release: $sr.release, platform: .platform, runner: .runner}] | {"include": .}')
echo "build-matrix=${build_matrix}" >> "$GITHUB_OUTPUT"

# ---------------------------------------------------------------------------
# 4. Tempest matrix: one image per release (not per service)
# ---------------------------------------------------------------------------
releases=$(
  for release_dir in "${dirs[@]}"; do
    release="${release_dir%/}"
    release="${release#releases/}"
    echo "{\"release\":\"${release}\"}"
  done | jq -sc '.'
)

echo "tempest-release-matrix=$(echo "$releases" | jq -c '{"include": .}')" >> "$GITHUB_OUTPUT"

tempest_matrix=$(echo "$releases" | jq -c \
  --argjson p "$platforms" \
  '[.[] as $r | $p[] | {release: $r.release, platform: .platform, runner: .runner}] | {"include": .}')
echo "tempest-matrix=${tempest_matrix}" >> "$GITHUB_OUTPUT"
