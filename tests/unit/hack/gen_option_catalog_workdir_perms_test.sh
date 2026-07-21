#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/gen-option-catalog.sh opens up its mktemp work directory before
# bind-mounting it into the service image. mktemp -d creates the directory
# 0700 for the invoking user, but oslo-config-generator reads /work as the
# image's non-root user; on a Linux bind mount that user cannot traverse a
# 0700 host directory, so gen.conf must be world-readable behind a
# world-searchable directory. Docker Desktop's file sharing masks this on
# macOS, which is why only CI saw the failure.
#
# The script is exercised with a recording docker stub prepended to PATH: at
# the moment of the first `docker run` the stub captures the permission bits
# of the mount source and of gen.conf inside it, then exits non-zero so the
# script stops there. No image is pulled and no container runs.
#
# Usage: bash tests/unit/hack/gen_option_catalog_workdir_perms_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
GEN_CATALOG_SH="$PROJECT_ROOT/hack/gen-option-catalog.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stubs <dir>
# Writes yq and docker shims into <dir>:
#   yq     — resolves any source-ref lookup to a fixed ref.
#   docker — `docker image inspect` succeeds so nothing is pulled;
#            `docker run` records `ls`-style permission strings of the -v
#            mount source and its gen.conf to $PERM_LOG, then exits 1 so the
#            script under test stops after the recording.
make_stubs() {
  local dir="$1"

  cat > "$dir/yq" <<'EOF'
#!/bin/bash
echo "stable/2025.2"
EOF

  cat > "$dir/docker" <<'EOF'
#!/bin/bash
if [ "$1" = "image" ]; then
  exit 0
fi
if [ "$1" = "run" ]; then
  src=""
  prev=""
  for arg in "$@"; do
    if [ "$prev" = "-v" ]; then
      src="${arg%%:*}"
    fi
    prev="$arg"
  done
  {
    echo "workdir $(ls -ld "$src" | cut -c1-10)"
    echo "genconf $(ls -l "$src/gen.conf" | cut -c1-10)"
  } >> "$PERM_LOG"
  exit 1
fi
exit 0
EOF

  chmod +x "$dir/yq" "$dir/docker"
}

# run_script <perm-log>
# Runs gen-option-catalog.sh for keystone 2025.2 with the stubs active and a
# local SOURCE_DIR fixture (so no curl happens). The script is expected to
# exit non-zero because the docker stub aborts the generator run.
run_script() {
  local perm_log="$1"
  local stub_dir src_dir
  stub_dir="$(mktemp -d)"
  src_dir="$(mktemp -d)"
  make_stubs "$stub_dir"
  mkdir -p "$src_dir/config-generator"
  cat > "$src_dir/config-generator/keystone.conf" <<'EOF'
[DEFAULT]
namespace = keystone
EOF

  PATH="$stub_dir:$PATH" PERM_LOG="$perm_log" SOURCE_DIR="$src_dir" \
    bash "$GEN_CATALOG_SH" keystone 2025.2 ghcr.io/c5c3/keystone:test >/dev/null 2>&1
  rm -rf "$stub_dir" "$src_dir"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_workdir_world_searchable() {
  echo "Test: bind-mounted work directory is world-searchable"
  local perm_log
  perm_log="$(mktemp)"
  run_script "$perm_log"

  local dir_perm conf_perm
  dir_perm="$(awk '/^workdir /{print $2}' "$perm_log")"
  conf_perm="$(awk '/^genconf /{print $2}' "$perm_log")"

  assert_not_empty "docker stub recorded the mount source permissions" "$dir_perm"
  assert_eq "work directory grants other r-x" "r-x" "${dir_perm:7:3}"
  assert_eq "gen.conf grants other read" "r" "${conf_perm:7:1}"
  rm -f "$perm_log"
}

test_workdir_world_searchable

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
