#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-resolve-hvo-commit.sh — Resolve the pinned openstack-hypervisor-operator commit.
#
# Reads the single valued 'ARG HVO_COMMIT=' line of
# images/openstack-hypervisor-operator/Dockerfile and prints the 40-character
# upstream commit on stdout. A bare 'ARG HVO_COMMIT' re-declaration in a build
# stage is not a pin and is ignored. This script is the only parser of that
# line: the image build workflow and tests/container-images/verify_hvo.sh call
# it instead of reading the Dockerfile themselves. Failures print an ::error::
# annotation on stderr, so stdout carries nothing but the commit.
#
# Required env vars:
#   (none — all have sensible defaults)
#
# Optional env vars:
#   HVO_DOCKERFILE — Dockerfile to parse
#                    (default: images/openstack-hypervisor-operator/Dockerfile)
#
# Reusable version resolution script.
# set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

HVO_DOCKERFILE="${HVO_DOCKERFILE:-${REPO_ROOT}/images/openstack-hypervisor-operator/Dockerfile}"

if [[ ! -f "${HVO_DOCKERFILE}" ]]; then
  echo "::error::hvo Dockerfile not found: ${HVO_DOCKERFILE}" >&2
  exit 1
fi

pin="$(sed -nE 's/^ARG HVO_COMMIT=//p' "${HVO_DOCKERFILE}")"

if [[ -z "${pin}" ]]; then
  echo "::error::no 'ARG HVO_COMMIT=' line in ${HVO_DOCKERFILE}" >&2
  exit 1
fi

# A second valued ARG line makes pin multi-line, which fails this anchored
# match too.
if [[ ! "${pin}" =~ ^[0-9a-f]{40}$ ]]; then
  echo "::error::ARG HVO_COMMIT in ${HVO_DOCKERFILE} is not a 40-character commit: '${pin}'" >&2
  exit 1
fi

echo "${pin}"
