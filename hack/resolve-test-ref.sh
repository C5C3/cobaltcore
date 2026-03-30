#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/resolve-test-ref.sh — Resolve a version from test-refs.yaml.
#
# Usage: hack/resolve-test-ref.sh <test-refs.yaml> <key>
#
# Prints the value for the given key. Exits 1 if the key is missing or null.

set -euo pipefail

test_refs="${1:?Usage: hack/resolve-test-ref.sh <test-refs.yaml> <key>}"
key="${2:?Usage: hack/resolve-test-ref.sh <test-refs.yaml> <key>}"

value=$(yq -r ".[\"${key}\"]" "${test_refs}")
if [ -z "${value}" ] || [ "${value}" = "null" ]; then
  echo "ERROR: No '${key}' found in ${test_refs}" >&2
  exit 1
fi
echo "${value}"
