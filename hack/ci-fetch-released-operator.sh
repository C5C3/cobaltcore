#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-fetch-released-operator.sh — Fetch the last released keystone-operator
# chart and image from GHCR as the baseline for the operator helm-upgrade-in-place
# E2E suite (tests/e2e-operator-upgrade/).
#
# "Last released" is the highest semver tag published to
# oci://ghcr.io/c5c3/charts/keystone-operator by the helm-push job on every main
# push (the repo has no v* git tags yet). `helm pull` without --version resolves
# that highest tag. The matching operator image is ghcr.io/c5c3/keystone-operator:latest,
# pushed by merge-operator-images on every main push.
#
# The pulled chart is untarred under _output/operator-upgrade/keystone-operator,
# and the operator image is loaded into the kind cluster so the baseline install
# runs with imagePullPolicy=Never.
#
# Optional env vars:
#   KIND_CLUSTER  — kind cluster name to load the image into (default: forge).
#   REGISTRY      — OCI registry host (default: ghcr.io).
#
# Emits `chart_dir=<path>` to $GITHUB_OUTPUT (when set) so the workflow can pass
# it to ci-deploy-operator.sh via CHART_DIR.
#
# set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

KIND_CLUSTER="${KIND_CLUSTER:-forge}"
REGISTRY="${REGISTRY:-ghcr.io}"

CHART_REF="oci://${REGISTRY}/c5c3/charts/keystone-operator"
IMAGE_REF="${REGISTRY}/c5c3/keystone-operator:latest"
OUT_DIR="_output/operator-upgrade"
CHART_DIR="${OUT_DIR}/keystone-operator"

# ---------------------------------------------------------------------------
# 1. Pull the last released chart (highest semver tag) from GHCR.
# ---------------------------------------------------------------------------
rm -rf "${OUT_DIR}"
mkdir -p "${OUT_DIR}"

echo "Pulling released keystone-operator chart from ${CHART_REF}..."
if ! helm pull "${CHART_REF}" --untar --untardir "${OUT_DIR}"; then
  echo "::error::failed to pull ${CHART_REF} — is a chart published to GHCR yet? (helm registry login required for private packages)"
  exit 1
fi

if [[ ! -f "${CHART_DIR}/Chart.yaml" ]]; then
  echo "::error::pulled chart missing ${CHART_DIR}/Chart.yaml"
  exit 1
fi

# Read and echo the resolved chart version for the CI log / traceability.
CHART_VERSION="$(grep -E '^version:' "${CHART_DIR}/Chart.yaml" | head -1 | awk '{print $2}' | tr -d '"')"
if [[ -z "${CHART_VERSION}" ]]; then
  echo "::error::could not read version from ${CHART_DIR}/Chart.yaml"
  exit 1
fi
echo "Resolved released chart version: ${CHART_VERSION}"

# ---------------------------------------------------------------------------
# 2. Pull the matching operator image and load it into the kind cluster.
# ---------------------------------------------------------------------------
echo "Pulling released operator image ${IMAGE_REF}..."
if ! docker pull "${IMAGE_REF}"; then
  echo "::error::failed to pull ${IMAGE_REF} — is the operator image published to GHCR yet?"
  exit 1
fi

echo "Loading ${IMAGE_REF} into kind cluster '${KIND_CLUSTER}'..."
if ! kind load docker-image "${IMAGE_REF}" --name "${KIND_CLUSTER}"; then
  echo "::error::failed to load ${IMAGE_REF} into kind cluster '${KIND_CLUSTER}'"
  exit 1
fi

# ---------------------------------------------------------------------------
# 3. Emit the chart dir for the workflow.
# ---------------------------------------------------------------------------
if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  echo "chart_dir=${CHART_DIR}" >> "${GITHUB_OUTPUT}"
fi
echo "Baseline chart ready at ${CHART_DIR} (version ${CHART_VERSION})"
