#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-build-ovn-image.sh assembles the right `docker build` argv,
# using a recording docker stub on PATH:
#   - the default run tags c5c3/ovn:<resolved version> and builds
#     images/ovn/Dockerfile with images/ovn/ as the context;
#   - OVN_IMAGE overrides the tag;
#   - the DOCKER_BUILD_CACHE_* vars add --cache-from/--cache-to;
#   - OVN_DOCKERFILE moves the resolved pin and the built file together;
#   - a Dockerfile without the pin aborts before docker is invoked.
#
# Follows the project-native bash test pattern (tests/lib/assertions.sh),
# mirroring tests/unit/hack/ci_fetch_released_operator_test.sh.
#
# Usage: bash tests/unit/hack/ci_build_ovn_image_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
BUILD_SH="$PROJECT_ROOT/hack/ci-build-ovn-image.sh"
RESOLVE_SH="$PROJECT_ROOT/hack/ci-resolve-ovn-version.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

STUB_DIR="$TMP_DIR/bin"
DOCKER_LOG="$TMP_DIR/docker.log"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_docker_stub <dir>
# A recording docker stub: it appends its argv to $DOCKER_LOG and succeeds.
make_docker_stub() {
  local dir="$1"
  mkdir -p "$dir"

  cat >"$dir/docker" <<'STUB'
#!/bin/bash
echo "docker $*" >> "$DOCKER_LOG"
exit 0
STUB
  chmod +x "$dir/docker"
}

# run_build [VAR=value ...]
# Runs the builder with the docker stub prepended to PATH (so coreutils still
# resolve) and a fresh log. OVN_IMAGE, OVN_DOCKERFILE and the cache vars start
# out unset; the arguments are exported on top of that. Stores the combined
# stdout/stderr in OUTPUT, the exit status in RC and the recorded argv in LOG.
run_build() {
  RC=0
  rm -f "$DOCKER_LOG"
  OUTPUT="$(
    unset OVN_IMAGE OVN_DOCKERFILE DOCKER_BUILD_CACHE_FROM DOCKER_BUILD_CACHE_TO
    for assignment in "$@"; do
      export "${assignment?}"
    done
    PATH="$STUB_DIR:$PATH" DOCKER_LOG="$DOCKER_LOG" bash "$BUILD_SH" 2>&1
  )" || RC=$?

  LOG=""
  if [ -f "$DOCKER_LOG" ]; then
    LOG="$(cat "$DOCKER_LOG")"
  fi
}

make_docker_stub "$STUB_DIR"

# ---------------------------------------------------------------------------
# Test 1: the default run builds the pinned tag from images/ovn/
# ---------------------------------------------------------------------------
test_default_build() {
  echo "Test: default run builds c5c3/ovn:<resolved version> from images/ovn/"

  local expected_version
  expected_version="$(bash "$RESOLVE_SH")"

  run_build

  local expected="docker build -t c5c3/ovn:${expected_version} -f ${PROJECT_ROOT}/images/ovn/Dockerfile ${PROJECT_ROOT}/images/ovn/"

  assert_eq "builder exits 0 on the default run" "0" "$RC"
  assert_not_empty "the resolver yields a version" "$expected_version"
  assert_eq "builder records exactly the expected docker argv" "$expected" "$LOG"
  assert_eq "the build context is the last argument" "${PROJECT_ROOT}/images/ovn/" "${LOG##* }"
  assert_not_contains "no --cache-from without DOCKER_BUILD_CACHE_FROM" "$LOG" "--cache-from"
  assert_not_contains "no --cache-to without DOCKER_BUILD_CACHE_TO" "$LOG" "--cache-to"
}

# ---------------------------------------------------------------------------
# Test 2: OVN_IMAGE overrides the tag
# ---------------------------------------------------------------------------
test_ovn_image_override() {
  echo "Test: OVN_IMAGE overrides the default tag"

  run_build "OVN_IMAGE=x/y:z"

  assert_eq "builder exits 0 with an overridden tag" "0" "$RC"
  assert_contains "builder tags the overridden image" "$LOG" "-t x/y:z"
  assert_not_contains "builder drops the default tag" "$LOG" "-t c5c3/ovn:"
}

# ---------------------------------------------------------------------------
# Test 3: the cache vars add --cache-from/--cache-to
# ---------------------------------------------------------------------------
test_cache_args() {
  echo "Test: DOCKER_BUILD_CACHE_FROM/TO add the buildx cache flags"

  run_build "DOCKER_BUILD_CACHE_FROM=type=gha,scope=a" "DOCKER_BUILD_CACHE_TO=type=gha,scope=b,mode=max"

  assert_eq "builder exits 0 with cache flags" "0" "$RC"
  assert_contains "builder passes --cache-from" "$LOG" "--cache-from type=gha,scope=a"
  assert_contains "builder passes --cache-to" "$LOG" "--cache-to type=gha,scope=b,mode=max"
}

# ---------------------------------------------------------------------------
# Test 4: OVN_DOCKERFILE selects the file the tag is read from AND built
# ---------------------------------------------------------------------------
test_ovn_dockerfile_override() {
  echo "Test: OVN_DOCKERFILE builds the same file the version is resolved from"

  # A pin that differs from the checked-in one, so a builder that resolves the
  # override but builds images/ovn/Dockerfile produces a tag naming a version
  # the image does not contain.
  local fixture_dir="$TMP_DIR/alt"
  mkdir -p "$fixture_dir"
  local fixture="$fixture_dir/Dockerfile"
  printf 'FROM ubuntu:noble\nARG OVN_VERSION=v25.03.4\n' >"$fixture"

  run_build "OVN_DOCKERFILE=$fixture"

  assert_eq "builder exits 0 with an overridden Dockerfile" "0" "$RC"
  assert_contains "builder tags the version from the overridden Dockerfile" "$LOG" "-t c5c3/ovn:25.03.4"
  assert_contains "builder builds the overridden Dockerfile" "$LOG" "-f $fixture"
  assert_not_contains "builder does not build the checked-in Dockerfile" \
    "$LOG" "-f ${PROJECT_ROOT}/images/ovn/Dockerfile"
  assert_eq "the build context is the overridden Dockerfile's directory" \
    "$fixture_dir/" "${LOG##* }"
}

# ---------------------------------------------------------------------------
# Test 5: an unresolvable pin aborts before docker runs
# ---------------------------------------------------------------------------
test_unresolvable_pin_aborts() {
  echo "Test: a Dockerfile without the pin aborts before docker is invoked"

  local fixture="$TMP_DIR/Dockerfile.noarg"
  printf 'FROM ubuntu:noble\nRUN echo build\n' >"$fixture"

  run_build "OVN_DOCKERFILE=$fixture"

  assert_nonzero_exit "builder exits non-zero when the pin is unresolvable" "$RC"
  assert_contains "builder surfaces the resolver ::error::" "$OUTPUT" "::error::"
  assert_eq "builder invokes no docker build" "" "$LOG"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_default_build
test_ovn_image_override
test_cache_args
test_ovn_dockerfile_override
test_unresolvable_pin_aborts

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
