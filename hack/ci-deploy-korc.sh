#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-deploy-korc.sh — Deploy K-ORC (OpenStack Resource Controller) into a
# kind cluster from the upstream released installer manifest.
#
# K-ORC does not publish a Helm chart; upstream ships a single flattened
# installer manifest as a release asset
# (releases/download/<tag>/install.yaml). This script pins to the SAME tag the
# Flux GitRepository uses (deploy/flux-system/sources/k-orc.yaml) — parsed at
# runtime rather than hardcoded so the two never drift — verifies the downloaded
# bytes against a pinned SHA-256 (release assets are mutable, so an unverified
# `kubectl apply` would grant cluster-admin to attacker-substituted objects),
# applies the verified local copy, then waits for the orc-system controller
# Deployment to become Available.
#
# Used by the e2e-controlplane CI job, which deploys the c5c3 + keystone
# operators as local dev images and needs K-ORC's CRDs + controller alongside
# them (the Flux ControlPlane stack is suspended on that path).
#
# Optional env vars:
#   KORC_SOURCE  — Path to the Flux GitRepository manifest holding the pinned
#                  tag (default: deploy/flux-system/sources/k-orc.yaml).
#   WAIT_TIMEOUT — kubectl wait timeout for the controller Deployment
#                  (default: 180s).
#
# set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

KORC_SOURCE="${KORC_SOURCE:-deploy/flux-system/sources/k-orc.yaml}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-180s}"

# install.yaml is fetched from a GitHub release asset, which is MUTABLE — an
# upstream compromise or a re-uploaded asset at the same tag could otherwise run
# attacker-authored Kubernetes objects when applied with cluster-admin. The
# downloaded bytes are therefore verified against a pinned SHA-256 before apply.
# The digest is pinned to KORC_PINNED_TAG; when the tag in ${KORC_SOURCE} is
# bumped (by Renovate or by hand), re-pin BOTH values below. The e2e-controlplane
# job re-runs on any deploy/** or hack/** change, so a stale pin fails this step
# loudly on the bump PR, forcing a human to re-verify the new upstream installer.
# Recompute with:
#   curl -fsSL https://github.com/k-orc/openstack-resource-controller/releases/download/<tag>/install.yaml | sha256sum
KORC_PINNED_TAG="v2.6.0"
KORC_INSTALL_SHA256="affe57ece8c81001d4dacd35fe3bd5bf35662ed6b5c4b1da59f334c685bba109"

if [[ ! -f "${KORC_SOURCE}" ]]; then
  echo "::error::K-ORC source manifest not found: ${KORC_SOURCE}"
  exit 1
fi

# Parse the pinned tag (e.g. "v2.5.0") from the GitRepository ref.tag line. Use
# awk (POSIX) so no yq dependency is required on this early CI step.
KORC_TAG="$(awk '/^[[:space:]]*tag:[[:space:]]*/{print $2; exit}' "${KORC_SOURCE}")"
if [[ -z "${KORC_TAG}" ]]; then
  echo "::error::Could not parse a 'tag:' value from ${KORC_SOURCE}"
  exit 1
fi

# Guard the pin against a tag bump that forgot to re-pin the checksum, so the
# failure is an actionable message rather than a raw hash mismatch.
if [[ "${KORC_TAG}" != "${KORC_PINNED_TAG}" ]]; then
  echo "::error::K-ORC tag ${KORC_TAG} in ${KORC_SOURCE} does not match the pinned checksum tag ${KORC_PINNED_TAG}; re-pin KORC_PINNED_TAG and KORC_INSTALL_SHA256 in $(basename "${BASH_SOURCE[0]}") to the SHA-256 of the new install.yaml"
  exit 1
fi

INSTALL_URL="https://github.com/k-orc/openstack-resource-controller/releases/download/${KORC_TAG}/install.yaml"
INSTALL_FILE="$(mktemp)"
trap 'rm -f "${INSTALL_FILE}"' EXIT

echo "Downloading K-ORC ${KORC_TAG} installer from ${INSTALL_URL}"
curl -fsSL "${INSTALL_URL}" -o "${INSTALL_FILE}"

# Verify the downloaded installer against the pinned digest before it is applied
# with cluster-admin. Portable across GNU coreutils (sha256sum) and BSD/macOS
# (shasum -a 256), matching hack/install-test-deps.sh.
if command -v sha256sum &>/dev/null; then
  ACTUAL_SHA256="$(sha256sum "${INSTALL_FILE}" | awk '{print $1}')"
else
  ACTUAL_SHA256="$(shasum -a 256 "${INSTALL_FILE}" | awk '{print $1}')"
fi
if [[ "${ACTUAL_SHA256}" != "${KORC_INSTALL_SHA256}" ]]; then
  echo "::error::K-ORC installer checksum mismatch for ${INSTALL_URL}: expected ${KORC_INSTALL_SHA256}, got ${ACTUAL_SHA256}; the release asset may have been re-uploaded or tampered with, refusing to apply. If the tag was intentionally bumped, re-pin KORC_INSTALL_SHA256 in $(basename "${BASH_SOURCE[0]}")"
  exit 1
fi

echo "Verified K-ORC installer SHA-256 ${ACTUAL_SHA256}; applying ${INSTALL_FILE}"
kubectl apply --server-side -f "${INSTALL_FILE}"

echo "Waiting for the K-ORC controller Deployment in orc-system to become Available..."
kubectl wait --for=condition=Available deployment --all \
  -n orc-system --timeout="${WAIT_TIMEOUT}"
echo "K-ORC ${KORC_TAG} is deployed and Available."
