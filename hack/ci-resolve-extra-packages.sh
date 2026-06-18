#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-resolve-extra-packages.sh — Resolve extra packages for service image builds.
#
# Reads pip_extras, pip_packages, and apt_packages from release extra-packages.yaml.
#
# Required env vars:
#   MATRIX_SERVICE — OpenStack service name (e.g. keystone)
#   MATRIX_RELEASE — Release directory name (e.g. 2025.2)
#
# Outputs written to GITHUB_OUTPUT:
#   pip-extras    — Comma-separated pip extras (e.g. "ldap,memcache")
#   pip-packages  — Space-separated pip packages
#   apt-packages  — Space-separated apt packages
#
# Extracted from inline workflow step to standalone script.

set -euo pipefail

MATRIX_SERVICE="${MATRIX_SERVICE:?MATRIX_SERVICE is required (e.g. keystone)}"
MATRIX_RELEASE="${MATRIX_RELEASE:?MATRIX_RELEASE is required (e.g. 2025.2)}"

EXTRAS_FILE="releases/${MATRIX_RELEASE}/extra-packages.yaml"
if [ ! -f "$EXTRAS_FILE" ]; then
  echo "::error::Missing ${EXTRAS_FILE}. Each release directory must contain an extra-packages.yaml file (see docs/reference/build-images-workflow.md#adding-a-new-release)."
  exit 1
fi

pip_extras=$(yq -r ".\"${MATRIX_SERVICE}\".pip_extras // [] | join(\",\")" "$EXTRAS_FILE")
echo "pip-extras=${pip_extras}" >> "$GITHUB_OUTPUT"

pip_packages=$(yq -r ".\"${MATRIX_SERVICE}\".pip_packages // [] | join(\" \")" "$EXTRAS_FILE")
echo "pip-packages=${pip_packages}" >> "$GITHUB_OUTPUT"

apt_packages=$(yq -r ".\"${MATRIX_SERVICE}\".apt_packages // [] | join(\" \")" "$EXTRAS_FILE")
echo "apt-packages=${apt_packages}" >> "$GITHUB_OUTPUT"

echo "Resolved pip extras: ${pip_extras}"
echo "Resolved pip packages: ${pip_packages}"
echo "Resolved apt packages: ${apt_packages}"
