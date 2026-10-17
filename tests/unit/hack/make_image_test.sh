#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the `make image` recipe: it needs OPERATOR, builds the flake
# attribute <op>-operator-image, streams the image into `docker load` and tags
# <op>-operator:latest, the name and tag nix/lib/operator-image.nix gives the
# image, as IMG. A failed nix build ends the target with a non-zero exit and
# runs no docker at all.
#
# Stub nix and docker commands first on PATH log their arguments: nix prints
# the path of a stream script that writes "stream", or fails like nix when
# NIX_STUB_FAIL is set; docker logs what `docker load` reads.
#
# Lives under tests/unit/hack/ rather than tests/unit/Makefile/ because that is
# where the `make test-shell` glob picks tests up.
#
# Usage: bash tests/unit/hack/make_image_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

STUB_DIR="$TMP_DIR/bin"
export NIX_LOG="$TMP_DIR/nix.log" DOCKER_LOG="$TMP_DIR/docker.log" STREAM="$TMP_DIR/stream"
mkdir -p "$STUB_DIR"

cat >"$STUB_DIR/nix" <<'EOF'
#!/bin/bash
echo "$*" >>"$NIX_LOG"
if [ -n "${NIX_STUB_FAIL:-}" ]; then
  echo "error: flake 'git+file:///src' does not provide attribute 'packages.x86_64-linux.nope-operator-image'" >&2
  exit 1
fi
printf '#!/bin/sh\necho stream\n' >"$STREAM"
chmod +x "$STREAM"
echo "$STREAM"
EOF

cat >"$STUB_DIR/docker" <<'EOF'
#!/bin/bash
echo "$*" >>"$DOCKER_LOG"
if [ "$1" = load ]; then
  echo "load read: $(cat)" >>"$DOCKER_LOG"
fi
EOF
chmod +x "$STUB_DIR/nix" "$STUB_DIR/docker"

# run_make [make args...] runs `make image` of the repository with the stubs
# first on PATH and fresh logs. Stores the combined output in OUTPUT and the
# exit status in RC.
run_make() {
  : >"$NIX_LOG"
  : >"$DOCKER_LOG"
  RC=0
  OUTPUT=$(PATH="$STUB_DIR:/usr/bin:/bin" make --no-print-directory -C "$PROJECT_ROOT" image "$@" 2>&1) || RC=$?
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_image_requires_an_operator() {
  echo "Test: make image without OPERATOR fails before anything runs"

  run_make
  assert_nonzero_exit "make image exits non-zero" "$RC"
  assert_contains "the message names the usage" "$OUTPUT" \
    "image requires OPERATOR, e.g. make image OPERATOR=keystone"
  assert_eq "nix was not called" "" "$(cat "$NIX_LOG")"
}

test_image_builds_loads_and_tags() {
  echo "Test: make image builds the flake attribute, loads its stream and tags it IMG"

  run_make OPERATOR=keystone
  [ "$RC" -eq 0 ] || echo "$OUTPUT"
  assert_eq "make image exits 0" "0" "$RC"
  assert_eq "nix builds the operator's image attribute" \
    "build --no-link --print-out-paths .#keystone-operator-image" "$(cat "$NIX_LOG")"
  assert_eq "docker loads the stream, then tags the loaded image with the default IMG" \
    "load
load read: stream
tag keystone-operator:latest ghcr.io/c5c3/keystone-operator:latest" "$(cat "$DOCKER_LOG")"

  run_make OPERATOR=keystone IMG=ghcr.io/c5c3/keystone-operator:dev
  assert_contains "a given IMG is the tag" "$(cat "$DOCKER_LOG")" \
    "tag keystone-operator:latest ghcr.io/c5c3/keystone-operator:dev"
}

test_a_failed_build_runs_no_docker() {
  echo "Test: a failed nix build fails make image with nix's error and runs no docker"

  NIX_STUB_FAIL=1 run_make OPERATOR=nope
  assert_nonzero_exit "make image exits non-zero" "$RC"
  assert_contains "nix's error is shown" "$OUTPUT" "does not provide attribute"
  assert_eq "docker was not called" "" "$(cat "$DOCKER_LOG")"
}

test_the_recipe_tag_is_the_image_tag() {
  echo "Test: the image the recipe tags is the name and tag nix/lib/operator-image.nix gives the image"

  local expr="$PROJECT_ROOT/nix/lib/operator-image.nix"
  assert_file_contains_fixed "the image is named <op>-operator" "$expr" 'name = "${name}-operator";'
  assert_file_contains_fixed "the image is tagged latest" "$expr" 'tag = "latest";'
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if ! command -v make >/dev/null 2>&1; then
  echo "SKIP: make not installed (all checks skipped)"
  echo ""
  echo "Results: 0 passed, 0 failed, 1 skipped"
  exit 0
fi

test_image_requires_an_operator
test_image_builds_loads_and_tags
test_a_failed_build_runs_no_docker
test_the_recipe_tag_is_the_image_tag

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
