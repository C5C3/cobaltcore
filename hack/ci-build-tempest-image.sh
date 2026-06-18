#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-build-tempest-image.sh — Build the Tempest test container image.
#
# Resolves Tempest and plugin version refs from the release config, then builds
# the Tempest Docker image with pinned versions.
#
# Required env vars:
#   (none — all have sensible defaults)
#
# Optional env vars:
#   RELEASE         — Release directory name (default: 2025.2)
#   TEMPEST_IMAGE   — Target image name:tag (default: c5c3/tempest:local)
#
# Reusable image build script.
# set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
RELEASE="${RELEASE:-2025.2}"
TEMPEST_IMAGE="${TEMPEST_IMAGE:-c5c3/tempest:local}"

# ---------------------------------------------------------------------------
# 1. Resolve test component versions from release config
# ---------------------------------------------------------------------------
TEMPEST_VERSION=$("${REPO_ROOT}/hack/resolve-test-ref.sh" "releases/${RELEASE}/test-refs.yaml" tempest)
KTP_VERSION=$("${REPO_ROOT}/hack/resolve-test-ref.sh" "releases/${RELEASE}/test-refs.yaml" keystone-tempest-plugin)

echo "Building Tempest image (tempest=${TEMPEST_VERSION}, keystone-tempest-plugin=${KTP_VERSION})"

# ---------------------------------------------------------------------------
# 2. Build Tempest image
# ---------------------------------------------------------------------------
# Optional GHA cache flags — set by CI when buildx + type=gha is available.
cache_args=()
[[ -n "${DOCKER_BUILD_CACHE_FROM:-}" ]] && cache_args+=(--cache-from "${DOCKER_BUILD_CACHE_FROM}")
[[ -n "${DOCKER_BUILD_CACHE_TO:-}" ]] && cache_args+=(--cache-to "${DOCKER_BUILD_CACHE_TO}")

docker build \
  -t "${TEMPEST_IMAGE}" \
  -f "${REPO_ROOT}/images/tempest/Dockerfile" \
  "${cache_args[@]}" \
  --build-arg "TEMPEST_VERSION=${TEMPEST_VERSION}" \
  --build-arg "KEYSTONE_TEMPEST_PLUGIN_VERSION=${KTP_VERSION}" \
  --build-context "upper-constraints=${REPO_ROOT}/releases/${RELEASE}/" \
  "${REPO_ROOT}/images/tempest/"
