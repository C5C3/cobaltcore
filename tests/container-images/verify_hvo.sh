#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify openstack-hypervisor-operator container image meets requirements
# Usage: bash tests/container-images/verify_hvo.sh [image_name]
# Default image: openstack-hypervisor-operator
# Requires: Docker daemon running
# The expected upstream commit comes from hack/ci-resolve-hvo-commit.sh, which
# reads the single pin in images/openstack-hypervisor-operator/Dockerfile. Set
# HVO_DOCKERFILE to point that resolver at a different Dockerfile.

set -euo pipefail

IMAGE="${1:-openstack-hypervisor-operator}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# The resolver is the only parser of the 'ARG HVO_COMMIT=' pin, so this
# script never reads the Dockerfile itself.
EXPECTED=$("$SCRIPT_DIR/../../hack/ci-resolve-hvo-commit.sh")

# --- Test 1: the binary reports the pinned commit ---
test_version() {
  echo "Test: manager --version reports commit $EXPECTED"
  # Upstream prints "<binName> <version> (<os>/<arch>) <commit>". The build
  # sets binName to manager, the version to the tag the image is published
  # under (sha-<commit>) and the commit to the pin.
  local output first_line ends exit_code=0

  output=$(docker run --rm "$IMAGE" --version 2>&1) || exit_code=$?
  first_line="${output%%$'\n'*}"
  assert_eq "--version exits 0" "0" "$exit_code"
  assert_starts_with "--version names manager and the sha- tag" "$first_line" "manager sha-$EXPECTED "

  ends=no
  if [[ "$first_line" == *" $EXPECTED" ]]; then
    ends=yes
  fi
  assert_eq "--version ends with the pinned commit (got '$first_line')" "yes" "$ends"
}

# --- Test 2: the binary is a main build ---
test_main_build_flags() {
  echo "Test: manager --help lists the flags only main carries"
  # v1.2.3 has neither flag, so their presence proves a main build. The check
  # reads the usage text and not a start without flags: upstream logs its
  # "--agent-namespaces is required" error before it installs a logger
  # (cmd/main.go), so that message never reaches the output.
  local output

  output=$(docker run --rm "$IMAGE" --help 2>&1 || true)
  assert_contains "--help lists -agent-namespaces" "$output" "-agent-namespaces"
  assert_contains "--help lists -eviction-concurrency" "$output" "-eviction-concurrency"
}

# --- Test 3: non-root user and the manager entrypoint ---
test_user_and_entrypoint() {
  echo "Test: image runs /usr/bin/manager as 65532:65532"
  # Upstream's chart sets runAsNonRoot and no command, so the image has to
  # carry both a numeric non-root user and the entrypoint.
  local user entrypoint

  user=$(docker inspect --format '{{.Config.User}}' "$IMAGE")
  entrypoint=$(docker inspect --format '{{.Config.Entrypoint}}' "$IMAGE")
  assert_eq "Config.User is 65532:65532" "65532:65532" "$user"
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
echo "=== openstack-hypervisor-operator container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_version
echo ""
test_main_build_flags
echo ""
test_user_and_entrypoint
echo ""
test_upstream_commit_label
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
