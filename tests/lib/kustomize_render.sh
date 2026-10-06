#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Shared kustomize render helpers for the tests that pin an overlay's rendered
# objects (tests/unit/deploy/metal_stack_overlay_test.sh,
# tests/unit/deploy/metal_stack_controlplane_test.sh).
#
# How a render that cannot be read is counted (SKIP without kustomize or yq,
# FAIL on a failing or empty build) is one rule. Kept here so a change to it is
# edited once instead of once per test.
#
# Source this after tests/lib/assertions.sh. render writes RENDERED and adds to
# SKIP and FAIL, which the sourcing test declares.

have() {
  command -v "$1" >/dev/null 2>&1
}

# render <dir> <checks>
# Renders <dir> into RENDERED. When the render cannot be read, counts the
# caller's checks as SKIP (kustomize or yq missing) or FAIL (the build failed,
# or it produced no document) and returns 1.
render() {
  local dir="$1" checks="$2"

  if ! have kustomize || ! have yq; then
    echo "  SKIP: kustomize or yq not installed ($checks checks skipped)"
    SKIP=$((SKIP + checks))
    return 1
  fi

  # No --load-restrictor: kubectl apply -k cannot pass one either.
  if ! RENDERED="$(kustomize build "$dir" 2>&1)"; then
    echo "  FAIL: kustomize build $dir failed (default LoadRestrictionsRootOnly):"
    echo "$RENDERED" | head -20
    FAIL=$((FAIL + checks))
    return 1
  fi

  local count
  count="$(printf '%s\n' "$RENDERED" | yq -N -r 'select(. != null) | .kind' - | grep -c .)"
  if [[ "$count" -eq 0 ]]; then
    echo "  FAIL: kustomize build $dir rendered no document"
    FAIL=$((FAIL + checks))
    return 1
  fi
}

# val <kind> <name> <expression>
# The value <expression> yields on the rendered object of <kind> named <name>.
val() {
  printf '%s\n' "$RENDERED" |
    yq -N -r "select(.kind == \"$1\" and .metadata.name == \"$2\") | $3" - | head -n 1
}

# count_named <kind> <name>
# How many rendered objects of <kind> are named <name>.
count_named() {
  printf '%s\n' "$RENDERED" |
    yq -N -r "select(.kind == \"$1\" and .metadata.name == \"$2\") | .metadata.name" - | grep -c .
}

# resource_entries <kustomization>
# The items of the top-level resources key; the range ends at the next
# top-level key.
resource_entries() {
  awk '
    /^resources:/ { in_list = 1; next }
    in_list && /^[^[:space:]#-]/ { in_list = 0 }
    in_list && /^[[:space:]]*-[[:space:]]+/ {
      sub(/^[[:space:]]*-[[:space:]]+/, ""); sub(/[[:space:]]+$/, ""); print
    }
  ' "$1"
}
