#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify kvm-node-agent container image meets requirements
# Usage: bash tests/container-images/verify_kna.sh [image_name]
# Default image: kvm-node-agent
# Requires: Docker daemon running
# The expected upstream commit comes from hack/ci-resolve-kna-commit.sh, which
# reads the single pin in images/kvm-node-agent/Dockerfile. Set
# KNA_DOCKERFILE to point that resolver at a different Dockerfile.

set -euo pipefail

IMAGE="${1:-kvm-node-agent}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# The resolver is the only parser of the 'ARG KNA_COMMIT=' pin, so this
# script never reads the Dockerfile itself.
EXPECTED=$("$SCRIPT_DIR/../../hack/ci-resolve-kna-commit.sh")

# --- Test 1: the binary reports the pinned commit ---
test_version() {
  echo "Test: manager --version reports sha-$EXPECTED"
  # Upstream prints "<binName> version <version> (<build date>)". The build
  # sets binName to manager and the version to the tag the image is
  # published under (sha-<commit>), and leaves the build date unset.
  local output first_line exit_code=0

  output=$(docker run --rm "$IMAGE" --version 2>&1) || exit_code=$?
  first_line="${output%%$'\n'*}"
  assert_eq "--version exits 0" "0" "$exit_code"
  assert_eq "--version names manager and the sha- tag" "manager version sha-$EXPECTED ()" "$first_line"
}

# --- Test 2: the static binary starts on the distroless base ---
test_static_binary_starts() {
  echo "Test: manager --help exits 0 and lists -health-probe-bind-address"
  # The chart passes -health-probe-bind-address. A binary that needed a C
  # library would not start on the static base at all.
  local output exit_code=0

  output=$(docker run --rm "$IMAGE" --help 2>&1) || exit_code=$?
  assert_eq "--help exits 0" "0" "$exit_code"
  assert_contains "--help lists -health-probe-bind-address" "$output" "-health-probe-bind-address"
}

# --- Test 3: root user and the manager entrypoint ---
test_user_and_entrypoint() {
  echo "Test: image runs /usr/bin/manager as 0:0"
  # kna talks to the host's system bus as root (#1167), and upstream's chart
  # sets no command, so the image carries both the user and the entrypoint.
  local user entrypoint

  user=$(docker inspect --format '{{.Config.User}}' "$IMAGE")
  entrypoint=$(docker inspect --format '{{.Config.Entrypoint}}' "$IMAGE")
  assert_eq "Config.User is 0:0" "0:0" "$user"
  assert_eq "Config.Entrypoint is /usr/bin/manager" "[/usr/bin/manager]" "$entrypoint"
}

# --- Test 4: the upstream-commit label names the pin ---
test_upstream_commit_label() {
  echo "Test: io.c5c3.upstream-commit label is $EXPECTED"
  local label

  label=$(docker inspect --format '{{ index .Config.Labels "io.c5c3.upstream-commit" }}' "$IMAGE")
  assert_eq "io.c5c3.upstream-commit equals the pinned commit" "$EXPECTED" "$label"
}

# --- Run all tests ---
echo "=== kvm-node-agent container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_version
echo ""
test_static_binary_starts
echo ""
test_user_and_entrypoint
echo ""
test_upstream_commit_label
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
