#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-build-ovn-image.sh — Build the OVN container image.
#
# Resolves the version pin with hack/ci-resolve-ovn-version.sh and builds
# images/ovn/Dockerfile. The Dockerfile clones OVN at the pinned tag and takes
# Open vSwitch from the 'ovs' submodule gitlink, so the build needs no host
# checkout and no --build-arg. Issue #905 wires this script into the
# build-e2e-images CI step.
#
# Required env vars:
#   (none — all have sensible defaults)
#
# Optional env vars:
#   GITHUB_TOKEN            — Authenticates the source fetches from github.com
#                             inside the build; mounted as the github_token
#                             BuildKit secret, never a build-arg, so it reaches
#                             no layer. CI passes the workflow token; a run
#                             without one fetches anonymously.
#   OVN_IMAGE               — Target image name:tag (default: c5c3/ovn:<version>)
#   OVN_DOCKERFILE          — Dockerfile to resolve the pin from and to build
#                             (default: images/ovn/Dockerfile). Its directory is
#                             the build context.
#   DOCKER_BUILD_CACHE_FROM — buildx --cache-from spec
#   DOCKER_BUILD_CACHE_TO   — buildx --cache-to spec
#
# Reusable image build script.
# set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---------------------------------------------------------------------------
# 1. Resolve the version pin from the Dockerfile
# ---------------------------------------------------------------------------
# Command substitution, so set -e aborts before docker runs when the pin is
# missing or malformed. The default is resolved here and exported rather than
# left to the resolver, so the tag below and the Dockerfile built further down
# can never come from two different files.
OVN_DOCKERFILE="${OVN_DOCKERFILE:-${REPO_ROOT}/images/ovn/Dockerfile}"
export OVN_DOCKERFILE
OVN_VERSION="$("${REPO_ROOT}/hack/ci-resolve-ovn-version.sh")"
OVN_IMAGE="${OVN_IMAGE:-c5c3/ovn:${OVN_VERSION}}"

echo "Building OVN image ${OVN_IMAGE} (OVN v${OVN_VERSION}, OVS from the ovs submodule gitlink)"

# ---------------------------------------------------------------------------
# 2. Build OVN image
# ---------------------------------------------------------------------------
# Optional GHA cache flags — set by CI when buildx + type=gha is available.
cache_args=()
[[ -n "${DOCKER_BUILD_CACHE_FROM:-}" ]] && cache_args+=(--cache-from "${DOCKER_BUILD_CACHE_FROM}")
[[ -n "${DOCKER_BUILD_CACHE_TO:-}" ]] && cache_args+=(--cache-to "${DOCKER_BUILD_CACHE_TO}")

# The token goes in as a BuildKit secret read from the environment: the
# Dockerfile mounts it into the one RUN that fetches from github.com, so it
# is in no build-arg, no layer and no image metadata.
secret_args=()
[[ -n "${GITHUB_TOKEN:-}" ]] && secret_args+=(--secret "id=github_token,env=GITHUB_TOKEN")

# ${cache_args[@]+"…"} guards the empty case: bash 3.2, which contributors run
# `make test-shell` under on macOS, aborts on an empty array under set -u.
docker build \
  -t "${OVN_IMAGE}" \
  -f "${OVN_DOCKERFILE}" \
  ${cache_args[@]+"${cache_args[@]}"} \
  ${secret_args[@]+"${secret_args[@]}"} \
  "$(dirname "${OVN_DOCKERFILE}")/"
