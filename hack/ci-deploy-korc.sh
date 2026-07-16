#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-deploy-korc.sh — Deploy K-ORC (OpenStack Resource Controller) into a
# kind cluster from a pinned upstream commit.
#
# K-ORC does not publish a Helm chart. At release time upstream ships a single
# flattened installer manifest as a release asset, but the c5c3-operator now
# Owns() the RoleAssignment kind, which no released K-ORC ships and which only
# upstream main's controller reconciles. The Flux source
# (deploy/flux-system/sources/k-orc.yaml) is therefore pinned to an upstream MAIN
# COMMIT, and its Kustomization builds ./config/default with an image override
# instead of applying the release-only ./dist/install.yaml.
#
# This script mirrors that exactly: it parses the pinned commit from the Flux
# GitRepository manifest and the container image tag from the Flux Kustomization
# manifest (both at runtime, so the CI installer and the Flux stack never drift),
# git-clones K-ORC at that commit, `kubectl apply --server-side -k`s a generated
# kustomization that builds ./config/default with the same image override, then
# waits for the orc-system controller Deployment to become Available.
#
# Integrity. Two things must be pinned, and each has its own content address:
#   - the SOURCE tree, by the 40-char commit (a commit cannot be re-pointed, so the
#     detached checkout pins it exactly — this is what replaced the old release-
#     asset SHA-256, which guarded a MUTABLE release tag); and
#   - the controller IMAGE, by digest. A quay tag IS mutable, so the per-commit
#     commit-<short sha> tag pins nothing on its own; a re-pointed tag would run a
#     substituted controller under K-ORC's cluster RBAC. The digest is parsed from
#     the Flux Kustomization and applied verbatim, so CI and Flux schedule the same
#     bytes.
# On top of that a drift guard asserts the (documentation-only) image tag equals
# commit-<first 7 chars of the pinned commit>, so a Renovate commit bump that
# forgets to re-pin the image fails this step loudly on the bump PR.
#
# Used by the e2e-controlplane CI job, which deploys the c5c3 + keystone
# operators as local dev images and needs K-ORC's CRDs + controller alongside
# them (the Flux ControlPlane stack is suspended on that path).
#
# Optional env vars:
#   KORC_SOURCE  — Flux GitRepository manifest holding the pinned commit
#                  (default: deploy/flux-system/sources/k-orc.yaml).
#   KORC_RELEASE — Flux Kustomization manifest holding the image override
#                  (default: deploy/flux-system/releases/k-orc.yaml).
#   WAIT_TIMEOUT — kubectl wait timeout for the controller Deployment
#                  (default: 180s).
#
# set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

KORC_SOURCE="${KORC_SOURCE:-deploy/flux-system/sources/k-orc.yaml}"
KORC_RELEASE="${KORC_RELEASE:-deploy/flux-system/releases/k-orc.yaml}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-180s}"
KORC_REPO_URL="https://github.com/k-orc/openstack-resource-controller"

if [[ ! -f "${KORC_SOURCE}" ]]; then
  echo "::error::K-ORC source manifest not found: ${KORC_SOURCE}"
  exit 1
fi
if [[ ! -f "${KORC_RELEASE}" ]]; then
  echo "::error::K-ORC release manifest not found: ${KORC_RELEASE}"
  exit 1
fi

# Parse the pinned commit (ref.commit) from the GitRepository manifest and the
# image tag + digest (spec.images[].newTag / .digest) from the Kustomization
# manifest. Use awk (POSIX) so no yq dependency is required on this early CI step.
# The commit is what pins the source tree, so require it in the canonical 40-char
# form: `checkout --detach main` and `--detach v2.6.0` both succeed but follow a
# MUTABLE ref, which would void the pin this whole script rests on.
KORC_COMMIT="$(awk '/^[[:space:]]*commit:[[:space:]]*/{print $2; exit}' "${KORC_SOURCE}")"
if [[ ! "${KORC_COMMIT}" =~ ^[0-9a-f]{40}$ ]]; then
  echo "::error::Could not parse a 'commit: <40 hex>' value from ${KORC_SOURCE}; the K-ORC source MUST be pinned by a full commit SHA (a branch or tag is mutable)"
  exit 1
fi

KORC_IMAGE_TAG="$(awk '/^[[:space:]]*newTag:[[:space:]]*/{print $2; exit}' "${KORC_RELEASE}")"
if [[ -z "${KORC_IMAGE_TAG}" ]]; then
  echo "::error::Could not parse a 'newTag:' value from ${KORC_RELEASE}"
  exit 1
fi

# The digest is what resolves the pull, so require it in the canonical
# sha256:<64 hex> form: dropping or mistyping it must fail here rather than
# silently degrade the deploy to a mutable-tag pull.
KORC_IMAGE_DIGEST="$(awk '/^[[:space:]]*digest:[[:space:]]*/{print $2; exit}' "${KORC_RELEASE}")"
if [[ ! "${KORC_IMAGE_DIGEST}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo "::error::Could not parse a 'digest: sha256:<64 hex>' value from ${KORC_RELEASE}; the K-ORC controller image MUST be pinned by digest (a quay tag is mutable)"
  exit 1
fi

# Drift guard: upstream publishes a deterministic per-commit image tagged
# commit-<first 7 chars of the commit SHA>. If the Flux image override was not
# bumped in lockstep with the source commit, fail loudly with an actionable
# message rather than silently deploying a mismatched controller image.
KORC_EXPECTED_TAG="commit-${KORC_COMMIT:0:7}"
if [[ "${KORC_IMAGE_TAG}" != "${KORC_EXPECTED_TAG}" ]]; then
  echo "::error::K-ORC image tag ${KORC_IMAGE_TAG} in ${KORC_RELEASE} does not match the pinned commit ${KORC_COMMIT} in ${KORC_SOURCE} (expected ${KORC_EXPECTED_TAG}); bump spec.images[].newTag in lockstep with ref.commit"
  exit 1
fi

# Fetch the pinned tree. --filter=blob:none keeps the clone small (blobs are
# fetched on demand); the detached checkout of the 40-char SHA pins the exact
# source. Two throwaway subdirs live under one temp workdir: the checkout, and a
# build dir holding the generated kustomization.
KORC_WORKDIR="$(mktemp -d)"
trap 'rm -rf "${KORC_WORKDIR}"' EXIT
KORC_CHECKOUT="${KORC_WORKDIR}/checkout"
KORC_BUILD="${KORC_WORKDIR}/build"
mkdir -p "${KORC_BUILD}"

echo "Cloning K-ORC ${KORC_COMMIT} from ${KORC_REPO_URL}"
git clone --filter=blob:none --no-checkout "${KORC_REPO_URL}" "${KORC_CHECKOUT}"
git -C "${KORC_CHECKOUT}" checkout --detach "${KORC_COMMIT}"

# Generate a kustomization that builds the upstream ./config/default base with the
# same digest-pinned image override the Flux Kustomization applies. kustomize
# rejects an absolute directory as a resource root, so reference the checkout via a
# relative path from this build dir.
cat >"${KORC_BUILD}/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../checkout/config/default
images:
  - name: controller
    newName: quay.io/orc/openstack-resource-controller
    digest: ${KORC_IMAGE_DIGEST}
EOF

echo "Applying K-ORC ${KORC_COMMIT} (image ${KORC_IMAGE_TAG}@${KORC_IMAGE_DIGEST}) from ./config/default"
kubectl apply --server-side -k "${KORC_BUILD}"

echo "Waiting for the K-ORC controller Deployment in orc-system to become Available..."
kubectl wait --for=condition=Available deployment --all \
  -n orc-system --timeout="${WAIT_TIMEOUT}"
echo "K-ORC ${KORC_COMMIT} is deployed and Available."
