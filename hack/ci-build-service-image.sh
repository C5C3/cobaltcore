#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-build-service-image.sh — Build an OpenStack service container image.
# Feature: CC-0050
#
# Resolves upstream source refs, clones the project, applies constraint
# overrides, and builds the full image chain: python-base -> venv-builder ->
# service image.
#
# Required env vars:
#   OPERATOR      — OpenStack service name (e.g. keystone)
#   IMAGE_PREFIX  — Container image prefix (e.g. ghcr.io/c5c3)
#
# Optional env vars:
#   RELEASE       — Release directory name (default: 2025.2)
#
# REQ-002: Reusable service image build script.
# REQ-007: set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
OPERATOR="${OPERATOR:?OPERATOR is required (e.g. keystone)}"
if [[ ! "${OPERATOR}" =~ ^[a-z][a-z0-9-]*$ ]]; then
  echo "::error::OPERATOR must be lowercase alphanumeric (with hyphens), got '${OPERATOR}'"
  exit 1
fi
IMAGE_PREFIX="${IMAGE_PREFIX:?IMAGE_PREFIX is required (e.g. ghcr.io/c5c3)}"
RELEASE="${RELEASE:-2025.2}"

# ---------------------------------------------------------------------------
# 1. Resolve upstream source ref
# ---------------------------------------------------------------------------
SERVICE_REF=$(yq ".\"${OPERATOR}\"" "${REPO_ROOT}/releases/${RELEASE}/source-refs.yaml")
if [ -z "${SERVICE_REF}" ] || [ "${SERVICE_REF}" = "null" ]; then
  echo "::error::SERVICE_REF for '${OPERATOR}' is null or empty in releases/${RELEASE}/source-refs.yaml"
  exit 1
fi

# ---------------------------------------------------------------------------
# 2. Read extra packages from release config
# ---------------------------------------------------------------------------
EXTRA_PKG_FILE="${REPO_ROOT}/releases/${RELEASE}/extra-packages.yaml"
PIP_EXTRAS=$(yq -r ".\"${OPERATOR}\".pip_extras // [] | join(\",\")" "${EXTRA_PKG_FILE}")
PIP_PACKAGES=$(yq -r ".\"${OPERATOR}\".pip_packages // [] | join(\" \")" "${EXTRA_PKG_FILE}")
APT_PACKAGES=$(yq -r ".\"${OPERATOR}\".apt_packages // [] | join(\" \")" "${EXTRA_PKG_FILE}")

# ---------------------------------------------------------------------------
# 3. Clone upstream at pinned ref
# ---------------------------------------------------------------------------
# NOTE: Uses mktemp to avoid race conditions during local parallel debugging.
# Each invocation gets a unique directory (CC-0050, review #2 comment 9).
SRC_DIR=$(mktemp -d "/tmp/${OPERATOR}-src.XXXXXX")
trap 'rm -rf "${SRC_DIR}"' EXIT
git clone --depth 1 --branch "${SERVICE_REF}" \
  "https://github.com/openstack/${OPERATOR}.git" "${SRC_DIR}"

# ---------------------------------------------------------------------------
# 4. Apply constraint overrides (idempotent, exits 0 if no overrides)
# ---------------------------------------------------------------------------
"${REPO_ROOT}/scripts/apply-constraint-overrides.sh" "${RELEASE}"

# ---------------------------------------------------------------------------
# 5. Build image chain: python-base -> venv-builder -> service
# ---------------------------------------------------------------------------
# Reuse existing base images when available (e.g. when a prior invocation in
# the same job or an artifact load already created them).  Saves ~30s per
# redundant rebuild within e2e-operator (which calls this script twice for
# different releases).
if ! docker image inspect python-base >/dev/null 2>&1; then
  docker build -t python-base "${REPO_ROOT}/images/python-base/"
else
  echo "Reusing existing python-base image"
fi
if ! docker image inspect venv-builder >/dev/null 2>&1; then
  docker build -t venv-builder "${REPO_ROOT}/images/venv-builder/"
else
  echo "Reusing existing venv-builder image"
fi
docker build -t "${IMAGE_PREFIX}/${OPERATOR}:${RELEASE}" \
  --build-arg "PIP_EXTRAS=${PIP_EXTRAS}" \
  --build-arg "PIP_PACKAGES=${PIP_PACKAGES}" \
  --build-arg "EXTRA_APT_PACKAGES=${APT_PACKAGES}" \
  --build-context "${OPERATOR}=${SRC_DIR}" \
  --build-context "upper-constraints=${REPO_ROOT}/releases/${RELEASE}" \
  "${REPO_ROOT}/images/${OPERATOR}/"
