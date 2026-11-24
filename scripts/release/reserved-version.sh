#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# The first distribution RC reserves the stable OCI version for promotion.
set -euo pipefail
version=$(hack/ci-read-chart-version.sh "$1")
if [[ -n "$version" ]]; then
  tags=$(git ls-remote --refs origin "refs/tags/v${version}" "refs/tags/v${version}-rc.*")
  if [[ -n "$tags" ]]; then
    echo "Distribution v${version} owns this version in GHCR"
    echo "reserved=true" >> "$GITHUB_OUTPUT"
  fi
fi
