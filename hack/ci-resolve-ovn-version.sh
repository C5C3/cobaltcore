#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-resolve-ovn-version.sh — Resolve the pinned OVN version.
#
# Reads the "version" key of images/ovn/source.json and prints the tag
# without its leading 'v' (v26.03.2 becomes 26.03.2) on stdout. The image
# build workflow, the e2e legs of ci.yaml, hack/ci-resolve-e2e-images.sh and
# tests/container-images/verify_ovn.sh call this script instead of reading
# the pin themselves; the Nix build reads the file itself. Failures print an
# ::error:: annotation on stderr, so stdout carries nothing but the version.
#
# Required env vars:
#   (none — all have sensible defaults)
#
# Optional env vars:
#   OVN_SOURCE — pin file to read (default: images/ovn/source.json)
#
# Requires jq on PATH.
#
# Reusable version resolution script.
# set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

OVN_SOURCE="${OVN_SOURCE:-${REPO_ROOT}/images/ovn/source.json}"

if ! command -v jq >/dev/null 2>&1; then
  echo "::error::jq not found on PATH" >&2
  exit 1
fi

if [[ ! -f "${OVN_SOURCE}" ]]; then
  echo "::error::OVN pin not found: ${OVN_SOURCE}" >&2
  exit 1
fi

if ! jq empty "${OVN_SOURCE}" >/dev/null 2>&1; then
  echo "::error::${OVN_SOURCE} is not valid JSON" >&2
  exit 1
fi

pin="$(jq -r '.version // empty' "${OVN_SOURCE}")"

if [[ -z "${pin}" ]]; then
  echo "::error::no \"version\" in ${OVN_SOURCE}" >&2
  exit 1
fi

if [[ ! "${pin}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "::error::\"version\" in ${OVN_SOURCE} is not a vX.Y.Z tag: '${pin}'" >&2
  exit 1
fi

echo "${pin#v}"
