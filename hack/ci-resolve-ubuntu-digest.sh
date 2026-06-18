#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-resolve-ubuntu-digest.sh — Resolve ubuntu:noble pinned manifest digest.
#
# Resolves the current manifest digest for ubuntu:noble to pin base image builds.
#
# Outputs written to GITHUB_OUTPUT:
#   digest — The resolved manifest digest (sha256:...)
#
# Extracted from inline workflow step to standalone script.

set -euo pipefail

digest=$(docker buildx imagetools inspect ubuntu:noble --format '{{json .Manifest.Digest}}' | tr -d '"')
if [[ -z "${digest}" ]]; then
  echo "::error::Failed to resolve ubuntu:noble digest"
  exit 1
fi
echo "digest=${digest}" >> "$GITHUB_OUTPUT"
echo "Resolved ubuntu:noble digest: ${digest}"
