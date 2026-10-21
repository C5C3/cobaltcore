#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-fetch-released-operator.sh — Fetch the released chart and image an
# operator helm-upgrade-in-place E2E suite (tests/e2e-operator-upgrade/) starts
# from.
#
# The suite's baseline.yaml pins one chart version V. The chart is
# oci://${REGISTRY}/c5c3/charts/<op>-operator at V, pushed by the helm-push
# job; charts are never pruned. The image is ${REGISTRY}/c5c3/<op>-operator:V:
# merge-operator-images tags every operator image it publishes from main with
# the operator's Chart.yaml version, and hack/ghcr-prune-stale-versions.py
# keeps that tag. While V is the version the checkout's Chart.yaml names, main
# still publishes it, so its chart and image move with every push until
# Chart.yaml moves past V; the script prints a notice in that case.
#
# The pulled chart is untarred under _output/operator-upgrade/<op>-operator,
# and the operator image is loaded into the kind cluster so the baseline install
# runs with imagePullPolicy=Never. A re-run is safe: the chart directory is
# cleared before the pull.
#
# Required env vars:
#   OPERATOR       — Operator name (e.g. keystone)
#
# Optional env vars:
#   BASELINE_FILE  — The pin file (default:
#                    tests/e2e-operator-upgrade/${OPERATOR}-helm-upgrade/baseline.yaml).
#                    Its first `version:` line names the chart version.
#   KIND_CLUSTER   — kind cluster name to load the image into (default: cobaltcore).
#   REGISTRY       — OCI registry host (default: ghcr.io).
#
# Emits `chart_dir=<path>` and `image_tag=<version>` to $GITHUB_OUTPUT (when
# set) so the workflow can pass them to ci-deploy-operator.sh via CHART_DIR and
# IMAGE_TAG.
#
# set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

# ---------------------------------------------------------------------------
# 1. Inputs, all checked before anything is pulled
# ---------------------------------------------------------------------------
OPERATOR="${OPERATOR:-}"
if [[ -z "${OPERATOR}" ]]; then
  echo "::error::OPERATOR is required (e.g. keystone)"
  exit 1
fi
if [[ ! "${OPERATOR}" =~ ^[a-z][a-z0-9-]*$ ]]; then
  echo "::error::OPERATOR must be lowercase alphanumeric (with hyphens), got '${OPERATOR}'"
  exit 1
fi
KIND_CLUSTER="${KIND_CLUSTER:-cobaltcore}"
REGISTRY="${REGISTRY:-ghcr.io}"
BASELINE_FILE="${BASELINE_FILE:-tests/e2e-operator-upgrade/${OPERATOR}-helm-upgrade/baseline.yaml}"

# The one reader of a chart version; it prints nothing for a missing file.
READ_VERSION_SH="${SCRIPT_DIR}/ci-read-chart-version.sh"

if [[ ! -f "${BASELINE_FILE}" ]]; then
  echo "::error::no baseline pin at ${BASELINE_FILE}"
  exit 1
fi
VERSION="$("${READ_VERSION_SH}" "${BASELINE_FILE}")"
if [[ -z "${VERSION}" ]]; then
  echo "::error::${BASELINE_FILE} has no version"
  exit 1
fi
if [[ ! "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "::error::${BASELINE_FILE}: version '${VERSION}' is not X.Y.Z"
  exit 1
fi

CHART_REF="oci://${REGISTRY}/c5c3/charts/${OPERATOR}-operator"
IMAGE_REF="${REGISTRY}/c5c3/${OPERATOR}-operator:${VERSION}"
OUT_DIR="_output/operator-upgrade"
CHART_DIR="${OUT_DIR}/${OPERATOR}-operator"

# A pin that main still publishes is not a fixed baseline yet.
CURRENT_VERSION="$("${READ_VERSION_SH}" "operators/${OPERATOR}/helm/${OPERATOR}-operator/Chart.yaml")"
if [[ "${VERSION}" == "${CURRENT_VERSION}" ]]; then
  echo "::notice::baseline ${OPERATOR}-operator ${VERSION} is the version main still publishes; its chart and image move with every push until Chart.yaml moves past ${VERSION}"
else
  echo "Baseline ${OPERATOR}-operator ${VERSION} is frozen (main publishes ${CURRENT_VERSION:-<none>})"
fi

# ---------------------------------------------------------------------------
# 2. Pull the pinned chart from GHCR
# ---------------------------------------------------------------------------
rm -rf "${CHART_DIR}"
mkdir -p "${OUT_DIR}"

echo "Pulling released ${OPERATOR}-operator chart ${CHART_REF} --version ${VERSION}..."
if ! helm_out="$(helm pull "${CHART_REF}" --version "${VERSION}" --untar --untardir "${OUT_DIR}" 2>&1)"; then
  echo "${helm_out}"
  echo "::error::failed to pull ${CHART_REF} --version ${VERSION}: $(tail -n1 <<<"${helm_out}")"
  exit 1
fi
echo "${helm_out}"

if [[ ! -f "${CHART_DIR}/Chart.yaml" ]]; then
  echo "::error::pulled chart missing ${CHART_DIR}/Chart.yaml"
  exit 1
fi

PULLED_VERSION="$("${READ_VERSION_SH}" "${CHART_DIR}/Chart.yaml")"
if [[ "${PULLED_VERSION}" != "${VERSION}" ]]; then
  echo "::error::pulled chart reports version ${PULLED_VERSION}, pinned ${VERSION}"
  exit 1
fi
echo "Resolved released chart version: ${PULLED_VERSION}"

# ---------------------------------------------------------------------------
# 3. Pull the matching operator image and load it into the kind cluster
# ---------------------------------------------------------------------------
echo "Pulling released operator image ${IMAGE_REF}..."
if ! docker_out="$(docker pull "${IMAGE_REF}" 2>&1)"; then
  echo "${docker_out}"
  if grep -qE 'not found|manifest unknown' <<<"${docker_out}"; then
    echo "::error::${IMAGE_REF} does not exist; main tags every operator image it publishes with its chart version since #1495, so pin a version main has published since"
  else
    echo "::error::failed to pull ${IMAGE_REF} — is the operator image published to GHCR yet?"
  fi
  exit 1
fi
echo "${docker_out}"

echo "Loading ${IMAGE_REF} into kind cluster '${KIND_CLUSTER}'..."
if ! kind load docker-image "${IMAGE_REF}" --name "${KIND_CLUSTER}"; then
  echo "::error::failed to load ${IMAGE_REF} into kind cluster '${KIND_CLUSTER}'"
  exit 1
fi

# ---------------------------------------------------------------------------
# 4. Emit the chart dir and the image tag for the workflow
# ---------------------------------------------------------------------------
if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  {
    echo "chart_dir=${CHART_DIR}"
    echo "image_tag=${VERSION}"
  } >>"${GITHUB_OUTPUT}"
fi
echo "Baseline chart ready at ${CHART_DIR} (version ${VERSION}), image ${IMAGE_REF}"
