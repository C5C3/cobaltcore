#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Merges constraint overrides from overrides/<release>/constraints.txt into
# upper-constraints.txt. Supports version replacement (package===version) and
# package removal (-package). Idempotent: exits 0 when no override file exists.
#
# Usage: apply-constraint-overrides.sh <release>
# Example: apply-constraint-overrides.sh 2025.2

set -euo pipefail

RELEASE="${1:?Usage: $0 <release>}"
CONSTRAINTS="releases/${RELEASE}/upper-constraints.txt"
OVERRIDES="overrides/${RELEASE}/constraints.txt"

if [ ! -f "$CONSTRAINTS" ]; then
  echo "Error: '$CONSTRAINTS' not found. Run this script from the repository root." >&2
  exit 1
fi

if [ ! -f "$OVERRIDES" ]; then
  exit 0
fi

while IFS= read -r line; do
  # Skip comments and blank lines
  [[ "$line" =~ ^[[:space:]]*#.*$ || -z "${line//[[:space:]]/}" ]] && continue

  if [[ "$line" =~ ^- ]]; then
    # Remove package from constraints
    package="${line#-}"
    escaped_package="${package//./\\.}"
    sed -i "/^${escaped_package}===/Id" "$CONSTRAINTS"
    echo "Removed constraint: $package"
  else
    # Override constraint (replace existing line)
    package=$(echo "$line" | cut -d'=' -f1)
    escaped_package="${package//./\\.}"
    sed -i "/^${escaped_package}===/Id" "$CONSTRAINTS"
    echo "$line" >> "$CONSTRAINTS"
    echo "Updated constraint: $line"
  fi
done < "$OVERRIDES"
