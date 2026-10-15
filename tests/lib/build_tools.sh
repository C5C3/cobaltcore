#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Shared check that a Nix-built image ships no build tools, for the verify
# scripts of the service and Tempest images under tests/container-images/.
#
# A probe on PATH sees only what the image links into its root, while every
# file of a Nix-built image is a store path under /nix/store: a compiler,
# binutils or uv that a retained reference pulls into the closure sits there
# without being on PATH. The check therefore probes PATH for the tools and
# lists the store for their packages. The lib and libgcc outputs of gcc are
# the runtime libraries libgcc_s and libstdc++, which compiled extensions
# load; they are no compiler. python3-config ships inside the interpreter's
# store path, so only its absence from PATH is checked.
#
# Kept here so a change to the tool list is edited once instead of once per
# image, where missing one checks that image against a stale list.

# assert_no_build_tools
# Assert that sh runs, that gcc, cc, uv and python3-config are not on PATH,
# and that /nix/store holds no compiler, binutils or uv package. Needs $IMAGE
# and the caller's PASS/FAIL counters.
assert_no_build_tools() {
  local tool exit_code=0 store offenders
  # Without a shell every probe below exits non-zero and passes vacuously.
  docker run --rm "$IMAGE" sh -c true > /dev/null 2>&1 || exit_code=$?
  if [ "$exit_code" -ne 0 ]; then
    echo "  FAIL: sh does not run, so the probes below would pass vacuously"
    FAIL=$((FAIL + 1))
    return
  fi
  # "command" is a shell builtin, so each probe runs in sh.
  for tool in gcc cc uv python3-config; do
    exit_code=0
    docker run --rm "$IMAGE" sh -c "command -v $tool" > /dev/null 2>&1 || exit_code=$?
    assert_nonzero_exit "$tool is not on PATH" "$exit_code"
  done

  exit_code=0
  store=$(docker run --rm "$IMAGE" ls /nix/store 2>&1) || exit_code=$?
  assert_eq "ls /nix/store exits 0" "0" "$exit_code"
  offenders=$(grep -E -- '-(gcc|gcc-wrapper|clang|clang-wrapper|binutils|binutils-wrapper|uv)-[0-9]' <<<"$store" |
    grep -vE -- '-(lib|libgcc)$' || true)
  assert_eq "/nix/store holds no compiler, binutils or uv package" "" "$offenders"
}
