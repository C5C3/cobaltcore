#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify a CobaltCore operator image that nix/lib/operator-image.nix builds
# Usage: bash tests/container-images/verify_operator_image.sh <image> <operator> [cover]
# Example: bash tests/container-images/verify_operator_image.sh keystone-operator:latest keystone
# The third argument, cover, verifies the coverage-instrumented variant
# (<op>-operator-cover-image), whose description says so; the rest is the same.
# Requires: Docker daemon running
# The image carries the layout of nix/lib/static-image.nix, runs
# /usr/bin/manager as 65532:65532 and has the four static labels alone: the
# version, the commit and the source are annotations of the index
# merge-operator-images publishes, so the image digest moves only with an
# input.

set -euo pipefail

USAGE="usage: verify_operator_image.sh <image> <operator> [cover]"
IMAGE="${1:?$USAGE}"
OPERATOR="${2:?$USAGE}"
VARIANT="${3:-}"

case "$VARIANT" in
"") DESCRIPTION="CobaltCore ${OPERATOR} operator" ;;
cover) DESCRIPTION="CobaltCore ${OPERATOR} operator, coverage-instrumented" ;;
*)
  echo "$USAGE" >&2
  exit 1
  ;;
esac

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"
# shellcheck source=tests/lib/static_image.sh
source "$SCRIPT_DIR/../lib/static_image.sh"

label() {
  docker inspect --format "{{ index .Config.Labels \"$1\" }}" "$IMAGE"
}

# --- Test 1: non-root user and the manager entrypoint ---
test_user_and_entrypoint() {
  echo "Test: image runs /usr/bin/manager as 65532:65532"
  assert_eq "Config.User is 65532:65532" "65532:65532" \
    "$(docker inspect --format '{{.Config.User}}' "$IMAGE")"
  assert_eq "Config.Entrypoint is /usr/bin/manager" "[/usr/bin/manager]" \
    "$(docker inspect --format '{{.Config.Entrypoint}}' "$IMAGE")"
}

# --- Test 2: the four static labels and no other ---
test_static_labels() {
  echo "Test: the image carries the four static labels alone"
  assert_eq "the label keys" \
    "org.opencontainers.image.description org.opencontainers.image.licenses org.opencontainers.image.title org.opencontainers.image.vendor" \
    "$(docker inspect --format '{{range $k, $v := .Config.Labels}}{{$k}} {{end}}' "$IMAGE" | sed 's/ $//')"
  assert_eq "title is ${OPERATOR}-operator" "${OPERATOR}-operator" "$(label org.opencontainers.image.title)"
  assert_eq "description names the operator" "$DESCRIPTION" \
    "$(label org.opencontainers.image.description)"
  assert_eq "licenses is Apache-2.0" "Apache-2.0" "$(label org.opencontainers.image.licenses)"
  assert_eq "vendor is SAP SE" "SAP SE" "$(label org.opencontainers.image.vendor)"
}

# --- Run all tests ---
echo "=== ${OPERATOR}-operator container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_user_and_entrypoint
echo ""
test_static_labels
echo ""
test_static_image_layout
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
