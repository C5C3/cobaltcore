#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify build_tempest_image of hack/run-tempest.sh, the image step of
# `make tempest`, with stub nix, docker and uname commands as the only
# commands on PATH besides cat, chmod, date and dirname:
#   - on x86_64-linux and aarch64-linux it builds the flake attribute
#     packages.<system>.tempest-<rel_>-image of the repository into
#     result-tempest, pipes that stream into docker load, and tags the loaded
#     tempest:<release> as TEMPEST_IMAGE;
#   - on any other host (macOS) it exits 1 before nix runs and names the
#     published image to run with instead;
#   - without nix on PATH it exits 1 with the same hint and docker never runs.
#
# The script is sourced in a subshell (its BASH_SOURCE guard keeps main()
# from running) with errexit in effect, and with REPO_ROOT pointed at a
# scratch directory.
#
# Usage: bash tests/unit/hack/run_tempest_build_image_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SCRIPT="$PROJECT_ROOT/hack/run-tempest.sh"

PASS=0
FAIL=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# Two command directories: one with a nix stub, one without.
STUBS="$TMP_DIR/stubs"
NO_NIX="$TMP_DIR/no-nix"
mkdir -p "$STUBS" "$NO_NIX"
for tool in cat chmod date dirname; do
  ln -s "$(command -v "$tool")" "$STUBS/$tool"
  ln -s "$(command -v "$tool")" "$NO_NIX/$tool"
done

# nix logs its arguments and writes, at the --out-link path, an executable
# that prints "stream".
cat >"$STUBS/nix" <<'STUB'
#!/bin/bash
echo "nix $*" >>"$CALL_LOG"
while [ "$#" -gt 0 ]; do
  if [ "$1" = --out-link ]; then
    printf '#!/bin/bash\necho stream\n' >"$2"
    chmod +x "$2"
  fi
  shift
done
STUB
# docker logs its arguments, and what docker load reads from stdin.
cat >"$STUBS/docker" <<'STUB'
#!/bin/bash
if [ "$1" = load ]; then
  echo "docker load <<< $(cat)" >>"$CALL_LOG"
else
  echo "docker $*" >>"$CALL_LOG"
fi
STUB
# uname answers -s and -m from UNAME_S and UNAME_M.
cat >"$STUBS/uname" <<'STUB'
#!/bin/bash
case "$1" in
  -s) echo "$UNAME_S" ;;
  -m) echo "$UNAME_M" ;;
esac
STUB
chmod +x "$STUBS/nix" "$STUBS/docker" "$STUBS/uname"
cp "$STUBS/docker" "$STUBS/uname" "$NO_NIX/"

# build_image <path> <uname -s> <uname -m> runs build_tempest_image for
# RELEASE=2026.2 with <path> as PATH and sets OUTPUT, RC and CALLS. The
# substitution is not tested by || or if, which would switch off the
# script's set -e inside it.
build_image() {
  local path="$1"
  export CALL_LOG="$TMP_DIR/calls.log" UNAME_S="$2" UNAME_M="$3"
  : >"$CALL_LOG"
  OUTPUT=$(
    PATH="$path"
    RELEASE=2026.2
    unset TEMPEST_IMAGE
    # shellcheck source=hack/run-tempest.sh
    source "$SCRIPT"
    REPO_ROOT="$TMP_DIR/repo"
    build_tempest_image 2>&1
  )
  RC=$?
  CALLS="$(cat "$CALL_LOG")"
}

test_linux_builds_loads_and_tags() {
  echo "Test: on x86_64-linux the image is built from the flake, loaded and tagged"
  mkdir -p "$TMP_DIR/repo"

  build_image "$STUBS" Linux x86_64

  assert_eq "build_tempest_image exits 0" "0" "$RC"
  assert_eq "nix builds, docker loads the stream and tags the release image" \
    "nix --extra-experimental-features nix-command flakes build $TMP_DIR/repo#packages.x86_64-linux.tempest-2026_2-image --out-link $TMP_DIR/repo/result-tempest
docker load <<< stream
docker tag tempest:2026.2 c5c3/tempest:local" "$CALLS"
}

test_aarch64_linux_builds_its_own_attribute() {
  echo "Test: on aarch64-linux the aarch64-linux attribute is built"

  build_image "$STUBS" Linux aarch64

  assert_eq "build_tempest_image exits 0" "0" "$RC"
  assert_contains "the aarch64-linux attribute is built" "$CALLS" \
    "$TMP_DIR/repo#packages.aarch64-linux.tempest-2026_2-image"
}

test_other_hosts_get_the_published_image() {
  echo "Test: on macOS the build stops before nix with the published image named"

  build_image "$STUBS" Darwin arm64

  assert_eq "build_tempest_image exits 1" "1" "$RC"
  assert_contains "the hint names the host and the published image" "$OUTPUT" \
    "on Darwin-arm64 rerun with BUILD_IMAGE=false TEMPEST_IMAGE=ghcr.io/c5c3/tempest:2026.2"
  assert_eq "neither nix nor docker runs" "" "$CALLS"
}

test_missing_nix_is_named() {
  echo "Test: without nix on PATH the build stops before docker"

  build_image "$NO_NIX" Linux x86_64

  assert_eq "build_tempest_image exits 1" "1" "$RC"
  assert_contains "the message names nix and the published image" "$OUTPUT" \
    "run-tempest.sh: nix not found; install Nix or rerun with BUILD_IMAGE=false TEMPEST_IMAGE=ghcr.io/c5c3/tempest:2026.2"
  assert_eq "docker never runs" "" "$CALLS"
}

test_linux_builds_loads_and_tags
test_aarch64_linux_builds_its_own_attribute
test_other_hosts_get_the_published_image
test_missing_nix_is_named

echo ""
echo "Results: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
