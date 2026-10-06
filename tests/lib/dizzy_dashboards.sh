#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Shared guard for the tests that render an overlay reading the git-ignored
# deploy/kind/dizzy/dashboards/ (tests/unit/deploy/dizzy_overlay_test.sh,
# tests/unit/deploy/metal_stack_dizzy_test.sh).
#
# A developer may have real dashboards staged there (hack/dizzy.sh
# stage-dashboards writes them). guard_setup_dashboards moves the directory
# aside and guard_restore_dashboards, registered on EXIT, puts it back, so a
# test never destroys staged dashboards, also when a check fails. Kept here so
# both tests use one guard: two copies could drift, and one of them would then
# delete what the other moved aside.
#
# Source this after PROJECT_ROOT is set.

DASHBOARDS_DIR="$PROJECT_ROOT/deploy/kind/dizzy/dashboards"
DASHBOARD_FILES=(overview.json api-operations.json time-to-ready.json)

# Backup location for a developer's pre-existing dashboards/ dir, empty when
# none existed at start.
DASHBOARDS_BACKUP=""

# Move any pre-existing dashboards/ dir into a temp backup so the render tests
# operate on a known-empty slate. Recorded in DASHBOARDS_BACKUP for restore.
guard_setup_dashboards() {
  if [[ -e "$DASHBOARDS_DIR" ]]; then
    DASHBOARDS_BACKUP="$(mktemp -d)"
    mv "$DASHBOARDS_DIR" "$DASHBOARDS_BACKUP/dashboards"
  fi
}

# Remove any test-created dashboards/ dir, then restore the developer's original
# (moved aside in guard_setup_dashboards) byte-for-byte. Registered on EXIT so a
# mid-test failure still restores it.
guard_restore_dashboards() {
  rm -rf "$DASHBOARDS_DIR"
  if [[ -n "$DASHBOARDS_BACKUP" && -d "$DASHBOARDS_BACKUP/dashboards" ]]; then
    mv "$DASHBOARDS_BACKUP/dashboards" "$DASHBOARDS_DIR"
    rmdir "$DASHBOARDS_BACKUP" 2>/dev/null || true
  fi
}

# Stage the three placeholder dashboard JSONs so the configMapGenerator's
# `files:` resolve. Content is `{}` — the render assertions only care that the
# files exist and become ConfigMap data keys.
stage_placeholder_dashboards() {
  mkdir -p "$DASHBOARDS_DIR"
  local f
  for f in "${DASHBOARD_FILES[@]}"; do
    printf '{}' > "$DASHBOARDS_DIR/$f"
  done
}
