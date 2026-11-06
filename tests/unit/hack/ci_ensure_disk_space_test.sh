#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-ensure-disk-space.sh secures the free space an e2e job needs
# on the runner's Docker disk (#1618): it leaves a runner with enough free
# alone, prunes unused images and the build cache on one without, and fails
# before the job's image loads when even the prune leaves too little.
#
# The multinode chaos leg of run 42148206213 started with 43 GiB free, needed
# about 53, and died loading its last image into a worker with "no space left
# on device", fifteen minutes into the job and with no suite run. The stubs
# model the two facts the script acts on: df reports a free amount, and an
# image prune raises it by what the unused images held. A script that skips the
# measure, prunes on a runner with room to spare (and throws away the image
# cache the next jobs pull from), or touches containers or volumes (the job's
# own kind nodes) fails here.
#
# Usage: bash tests/unit/hack/ci_ensure_disk_space_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
ENSURE_SH="$PROJECT_ROOT/hack/ci-ensure-disk-space.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

GIB=1048576

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# write_stubs <dir> <free-file> <log>
# Writes df and docker stubs. <free-file> holds the free KiB df reports; every
# stub appends its invocation to <log>.
#
#   STUB_PRUNE_FREES_KIB   — KiB an image prune adds to the free space (0)
#   STUB_IMAGE_PRUNE_EXIT  — exit code of `docker image prune` (0)
#   STUB_BUILDER_PRUNE_EXIT — exit code of `docker builder prune` (0)
#   STUB_ROOT_DIR          — what `docker info` prints as DockerRootDir
write_stubs() {
  local dir="$1" free="$2" log="$3"
  mkdir -p "$dir"

  cat >"$dir/df" <<STUB
#!/bin/bash
# Test stub df — POSIX output for the one path asked, its free KiB from the
# state file. See write_stubs() in tests/unit/hack/ci_ensure_disk_space_test.sh.
echo "df \$*" >>"$log"
avail="\$(cat "$free")"
echo "Filesystem 1024-blocks Used Available Capacity Mounted on"
if [ "\$1" = -Ph ]; then
  echo "/dev/sda1 193G 150G \$((avail / $GIB))G 78% /"
else
  echo "/dev/sda1 202000000 158000000 \${avail} 78% /"
fi
STUB
  chmod +x "$dir/df"

  cat >"$dir/docker" <<STUB
#!/bin/bash
# Test stub docker — info, image prune and builder prune.
echo "docker \$*" >>"$log"
case "\$1 \$2" in
  "info --format")
    echo "\${STUB_ROOT_DIR:-/var/lib/docker}"
    ;;
  "image prune")
    if [ "\${STUB_IMAGE_PRUNE_EXIT:-0}" != 0 ]; then
      echo "Error response from daemon: a prune operation is already running" >&2
      exit "\${STUB_IMAGE_PRUNE_EXIT}"
    fi
    echo \$((\$(cat "$free") + \${STUB_PRUNE_FREES_KIB:-0})) >"$free"
    echo "Deleted Images:"
    echo "Total reclaimed space: 61.2GB"
    ;;
  "builder prune")
    if [ "\${STUB_BUILDER_PRUNE_EXIT:-0}" != 0 ]; then
      echo "ERROR: no builder found" >&2
      exit "\${STUB_BUILDER_PRUNE_EXIT}"
    fi
    echo "Total:	2.1GB"
    ;;
  *)
    echo "[docker-stub] unexpected invocation: \$*" >&2
    exit 64
    ;;
esac
exit 0
STUB
  chmod +x "$dir/docker"
}

# new_case <free-gib> — Fresh temp dir whose df reports <free-gib> GiB free.
new_case() {
  local tmp
  tmp="$(mktemp -d)"
  echo $(($1 * GIB)) >"$tmp/free"
  write_stubs "$tmp/bin" "$tmp/free" "$tmp/log"
  : >"$tmp/log"
  echo "$tmp"
}

# run_ensure <tmp> [env assignments…] — Run the script under test with PATH
# led by the stubs. Echoes combined stdout/stderr; returns its exit code.
run_ensure() {
  local tmp="$1"
  shift
  (
    export PATH="$tmp/bin:/usr/bin:/bin"
    env -u DISK_SPACE_REQUIRED_GIB -u DISK_SPACE_PATH "$@" bash "$ENSURE_SH"
  ) 2>&1
}

# docker_calls <tmp> — The docker invocations, one per line.
docker_calls() {
  grep '^docker ' "$1/log" || true
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_enough_free_changes_nothing() {
  echo "Test: a runner with enough free keeps its images"
  local tmp output rc
  tmp="$(new_case 90)"
  output="$(run_ensure "$tmp" DISK_SPACE_REQUIRED_GIB=75)"
  rc=$?
  assert_eq "exits 0" "0" "$rc"
  assert_contains "it reports the free space and the need" "$output" \
    "OK: /var/lib/docker has 90 GiB free, the job needs 75 GiB; nothing removed."
  assert_not_contains "it prunes nothing" "$(docker_calls "$tmp")" "prune"
  assert_contains "it measures Docker's data root" "$(cat "$tmp/log")" "df -Pk /var/lib/docker"
  rm -rf "$tmp"
}

test_exactly_the_need_is_enough() {
  echo "Test: free space equal to the need is enough"
  local tmp rc
  tmp="$(new_case 75)"
  run_ensure "$tmp" DISK_SPACE_REQUIRED_GIB=75 >/dev/null
  rc=$?
  assert_eq "exits 0" "0" "$rc"
  assert_not_contains "it prunes nothing" "$(docker_calls "$tmp")" "prune"
  rm -rf "$tmp"
}

test_short_runner_prunes_unused_images() {
  echo "Test: the runner of run 42148206213 (43 GiB free) prunes, then has room"
  local tmp output rc calls
  tmp="$(new_case 43)"
  output="$(run_ensure "$tmp" DISK_SPACE_REQUIRED_GIB=75 STUB_PRUNE_FREES_KIB=$((60 * GIB)))"
  rc=$?
  assert_eq "exits 0" "0" "$rc"
  calls="$(docker_calls "$tmp")"
  assert_contains "it removes every image no container uses" "$calls" "docker image prune --all --force"
  assert_contains "and the build cache" "$calls" "docker builder prune --all --force"
  assert_contains "it says why it prunes" "$output" \
    "/var/lib/docker has 43 GiB free, the job needs 75 GiB: removing the images no container uses and the build cache."
  assert_contains "it prints what the prune reclaimed" "$output" "Total reclaimed space: 61.2GB"
  assert_contains "it measures again" "$output" \
    "OK: /var/lib/docker has 103 GiB free after the prune, the job needs 75 GiB."
  # The job's own kind nodes are containers, and a registry cache is a named
  # volume: neither may go.
  assert_not_contains "it removes no container" "$calls" "container"
  assert_not_contains "it removes no volume" "$calls" "volume"
  assert_not_contains "it runs no system prune" "$calls" "system"
  assert_not_contains "it removes nothing by hand" "$calls" "docker rm"
  rm -rf "$tmp"
}

test_still_short_after_the_prune_fails() {
  echo "Test: a runner still short after the prune fails before the job loads anything"
  local tmp output rc
  tmp="$(new_case 43)"
  output="$(run_ensure "$tmp" DISK_SPACE_REQUIRED_GIB=75 STUB_PRUNE_FREES_KIB=$((20 * GIB)))"
  rc=$?
  assert_eq "exits 1" "1" "$rc"
  assert_contains "it annotates the run with the numbers" "$output" \
    "::error::/var/lib/docker has 63 GiB free after removing every unused image and the build cache; the job needs 75 GiB"
  rm -rf "$tmp"
}

test_a_failed_image_prune_fails() {
  echo "Test: a failed image prune fails the script"
  local tmp output rc
  tmp="$(new_case 43)"
  output="$(run_ensure "$tmp" DISK_SPACE_REQUIRED_GIB=75 STUB_IMAGE_PRUNE_EXIT=1)"
  rc=$?
  assert_eq "exits 1" "1" "$rc"
  assert_contains "it annotates the prune's error" "$output" "::error::docker image prune failed"
  rm -rf "$tmp"
}

test_a_failed_builder_prune_is_logged() {
  echo "Test: a host without a builder still gets its images pruned"
  local tmp output rc
  tmp="$(new_case 43)"
  output="$(run_ensure "$tmp" DISK_SPACE_REQUIRED_GIB=75 STUB_PRUNE_FREES_KIB=$((60 * GIB)) STUB_BUILDER_PRUNE_EXIT=1)"
  rc=$?
  assert_eq "exits 0 on the image prune's space" "0" "$rc"
  assert_contains "it logs the builder failure" "$output" "WARNING: docker builder prune failed, carrying on"
  rm -rf "$tmp"
}

test_the_path_follows_docker() {
  echo "Test: the measured path is Docker's data root, or the override"
  local tmp
  tmp="$(new_case 90)"
  run_ensure "$tmp" DISK_SPACE_REQUIRED_GIB=75 STUB_ROOT_DIR=/srv/docker >/dev/null
  assert_contains "it reads DockerRootDir" "$(cat "$tmp/log")" "df -Pk /srv/docker"
  : >"$tmp/log"
  run_ensure "$tmp" DISK_SPACE_REQUIRED_GIB=75 DISK_SPACE_PATH=/mnt >/dev/null
  assert_contains "DISK_SPACE_PATH wins" "$(cat "$tmp/log")" "df -Pk /mnt"
  assert_not_contains "and docker info is not asked" "$(docker_calls "$tmp")" "docker info"
  rm -rf "$tmp"
}

test_usage_errors() {
  echo "Test: a missing or malformed need exits 2 before any docker call"
  local tmp value output rc
  tmp="$(new_case 10)"
  output="$(run_ensure "$tmp")"
  rc=$?
  assert_eq "unset: exits 2" "2" "$rc"
  assert_contains "unset: annotates the error" "$output" "::error::hack/ci-ensure-disk-space.sh: DISK_SPACE_REQUIRED_GIB must be a positive integer"
  for value in 0 abc 75G -5; do
    run_ensure "$tmp" DISK_SPACE_REQUIRED_GIB="$value" >/dev/null
    rc=$?
    assert_eq "'${value}': exits 2" "2" "$rc"
  done
  assert_eq "no docker call was made" "" "$(docker_calls "$tmp")"
  rm -rf "$tmp"
}

test_unmeasurable_disk_exits_2() {
  echo "Test: a df that prints no free space exits 2"
  local tmp output rc
  tmp="$(new_case 10)"
  echo "unknown" >"$tmp/free"
  output="$(run_ensure "$tmp" DISK_SPACE_REQUIRED_GIB=75)"
  rc=$?
  assert_eq "exits 2" "2" "$rc"
  assert_contains "it names the measure" "$output" "df on /var/lib/docker printed no free space"
  assert_not_contains "it prunes nothing" "$(docker_calls "$tmp")" "prune"
  rm -rf "$tmp"
}

test_enough_free_changes_nothing
test_exactly_the_need_is_enough
test_short_runner_prunes_unused_images
test_still_short_after_the_prune_fails
test_a_failed_image_prune_fails
test_a_failed_builder_prune_is_logged
test_the_path_follows_docker
test_usage_errors
test_unmeasurable_disk_exits_2

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
