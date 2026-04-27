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
# Retry the create call to absorb ghcr.io's eventual-consistency window
# between the per-arch `docker buildx --push` and the manifest merge: the
# registry has occasionally returned 404 on a freshly-pushed digest the
# same workflow consumed seconds earlier as a `--build-context` source.
# Backoff: 5s, 15s, 30s — total worst-case 50s before the third attempt.
src_refs=()
for d in *; do
  src_refs+=("${IMAGE}@sha256:${d}")
done

attempts=3
delay=5
for attempt in $(seq 1 "${attempts}"); do
  if docker buildx imagetools create "${tag_args[@]}" "${src_refs[@]}"; then
    break
  fi
  if [[ "${attempt}" -eq "${attempts}" ]]; then
    echo "::error::imagetools create failed after ${attempts} attempts" >&2
    exit 1
  fi
  echo "::warning::imagetools create attempt ${attempt} failed; retrying in ${delay}s"
  sleep "${delay}"
  delay=$((delay * 3))
done

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
