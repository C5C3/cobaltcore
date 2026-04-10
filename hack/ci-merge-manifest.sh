#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-merge-manifest.sh — Merge per-platform digests into a multi-arch manifest.
# Feature: CC-0055
#
# Collects per-platform digest files from DIGEST_DIR, creates a multi-arch
# manifest using docker buildx imagetools, and outputs the merged digest.
#
# Required env vars:
#   IMAGE       — Full image name without tag (e.g. ghcr.io/c5c3/python-base)
#   DIGEST_DIR  — Path to directory containing digest files (filenames are hex digests)
#   TAGS        — Space-separated list of full tag references to apply
#
# Optional env vars:
#   INSPECT_TAG — Tag for post-creation digest inspection (default: first TAGS entry)

set -euo pipefail

# ---------------------------------------------------------------------------
# Validate required env vars
# ---------------------------------------------------------------------------
IMAGE="${IMAGE:?IMAGE is required (e.g. ghcr.io/c5c3/python-base)}"
DIGEST_DIR="${DIGEST_DIR:?DIGEST_DIR is required (path to digest directory)}"
TAGS="${TAGS:?TAGS is required (space-separated list of full tag references)}"

# ---------------------------------------------------------------------------
# 1. Enter digest directory and validate at least one digest exists
# ---------------------------------------------------------------------------
cd "$DIGEST_DIR"
shopt -s nullglob
files=(*)
if [[ ${#files[@]} -eq 0 ]]; then
  echo "::error::No digests found in ${DIGEST_DIR}"
  exit 1
fi

# ---------------------------------------------------------------------------
# 2. Build tag arguments from TAGS
# ---------------------------------------------------------------------------
tag_args=()
for tag in $TAGS; do
  tag_args+=("-t" "${tag}")
done

# ---------------------------------------------------------------------------
# 3. Create multi-arch manifest
# ---------------------------------------------------------------------------
# shellcheck disable=SC2046
docker buildx imagetools create \
  "${tag_args[@]}" \
  $(for d in *; do printf '%s ' "${IMAGE}@sha256:${d}"; done)

# ---------------------------------------------------------------------------
# 4. Inspect merged manifest to get digest
# ---------------------------------------------------------------------------
# Use INSPECT_TAG if set, otherwise use the first tag from TAGS.
read -r first_tag _ <<< "$TAGS"
inspect_tag="${INSPECT_TAG:-${first_tag}}"

digest=$(docker buildx imagetools inspect "${inspect_tag}" \
  --format '{{json .Manifest.Digest}}' | tr -d '"')

if [[ -z "${digest}" ]]; then
  echo "::error::Failed to extract digest from ${inspect_tag}"
  exit 1
fi

# ---------------------------------------------------------------------------
# 5. Write output and confirmation
# ---------------------------------------------------------------------------
echo "digest=${digest}" >> "$GITHUB_OUTPUT"
echo "Created manifest for ${IMAGE}: ${digest}"
