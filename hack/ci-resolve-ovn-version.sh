#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-resolve-ovn-version.sh — Resolve the pinned OVN version.
#
# Reads the single 'ARG OVN_VERSION=' line of images/ovn/Dockerfile and prints
# the tag without its leading 'v' (v26.03.2 becomes 26.03.2) on stdout. This
# script is the only parser of that line: the image build workflow,
# hack/ci-build-ovn-image.sh and tests/container-images/verify_ovn.sh call it
# instead of reading the Dockerfile themselves. Failures print an ::error::
# annotation on stderr, so stdout carries nothing but the version.
#
# Required env vars:
#   (none — all have sensible defaults)
#
# Optional env vars:
#   OVN_DOCKERFILE — Dockerfile to parse (default: images/ovn/Dockerfile)
#
# Reusable version resolution script.
# set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

OVN_DOCKERFILE="${OVN_DOCKERFILE:-${REPO_ROOT}/images/ovn/Dockerfile}"

if [[ ! -f "${OVN_DOCKERFILE}" ]]; then
  echo "::error::OVN Dockerfile not found: ${OVN_DOCKERFILE}" >&2
  exit 1
fi

pin="$(sed -nE 's/^ARG OVN_VERSION=//p' "${OVN_DOCKERFILE}")"

if [[ -z "${pin}" ]]; then
  echo "::error::no 'ARG OVN_VERSION=' line in ${OVN_DOCKERFILE}" >&2
  exit 1
fi

# A second ARG line makes pin multi-line, which fails this anchored match too.
if [[ ! "${pin}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "::error::ARG OVN_VERSION in ${OVN_DOCKERFILE} is not a vX.Y.Z tag: '${pin}'" >&2
  exit 1
fi

echo "${pin#v}"
