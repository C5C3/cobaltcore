#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-resolve-kna-commit.sh — Resolve the pinned kvm-node-agent commit.
#
# Reads the "rev" key of
# images/kvm-node-agent/source.json and prints the 40-character
# upstream commit on stdout. The image build workflow and
# tests/container-images/verify_kna.sh call this script instead of reading
# the pin themselves; the Nix build reads the file itself. Failures print
# an ::error:: annotation on stderr, so stdout carries nothing but the
# commit.
#
# Required env vars:
#   (none — all have sensible defaults)
#
# Optional env vars:
#   KNA_SOURCE — pin file to read
#                (default: images/kvm-node-agent/source.json)
#
# Requires jq on PATH.
#
# Reusable version resolution script.
# set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

KNA_SOURCE="${KNA_SOURCE:-${REPO_ROOT}/images/kvm-node-agent/source.json}"

if ! command -v jq >/dev/null 2>&1; then
  echo "::error::jq not found on PATH" >&2
  exit 1
fi

if [[ ! -f "${KNA_SOURCE}" ]]; then
  echo "::error::kna pin not found: ${KNA_SOURCE}" >&2
  exit 1
fi

if ! jq empty "${KNA_SOURCE}" >/dev/null 2>&1; then
  echo "::error::${KNA_SOURCE} is not valid JSON" >&2
  exit 1
fi

pin="$(jq -r '.rev // empty' "${KNA_SOURCE}")"

if [[ -z "${pin}" ]]; then
  echo "::error::no \"rev\" in ${KNA_SOURCE}" >&2
  exit 1
fi

if [[ ! "${pin}" =~ ^[0-9a-f]{40}$ ]]; then
  echo "::error::\"rev\" in ${KNA_SOURCE} is not a 40-character commit: '${pin}'" >&2
  exit 1
fi

echo "${pin}"
