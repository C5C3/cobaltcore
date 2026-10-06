#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify that no Markdown page under DOCS_DIR (outside .vitepress/) holds a
# merge conflict marker: a line that starts with seven `<` or seven `>`
# followed by a space. The `<<< @/` code imports have three and do not match.
# Each hit names the file and line.
#
# DOCS_DIR overrides the scanned tree.
#
# Usage: bash tests/unit/docs/no_conflict_markers_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

DOCS_DIR="${DOCS_DIR:-$PROJECT_ROOT/docs}"

if [[ ! -d "$DOCS_DIR" ]]; then
  echo "FAIL: $DOCS_DIR does not exist"
  exit 1
fi
if [[ -z "$(find "$DOCS_DIR" -path "$DOCS_DIR/.vitepress" -prune -o -type f -name '*.md' -print -quit)" ]]; then
  echo "FAIL: no markdown files found under $DOCS_DIR"
  exit 1
fi

# --- Test 1: no conflict markers under DOCS_DIR ---
test_no_conflict_markers() {
  echo "Test: no Markdown page under $DOCS_DIR holds a conflict marker"
  local hits hit file rest
  hits="$(find "$DOCS_DIR" -path "$DOCS_DIR/.vitepress" -prune -o -type f -name '*.md' \
    -exec grep -HnE '^(<{7}|>{7}) ' {} + || true)"
  if [[ -z "$hits" ]]; then
    echo "  PASS: no conflict markers"
    PASS=$((PASS + 1))
    return
  fi
  while IFS= read -r hit; do
    file="${hit%%:*}"
    rest="${hit#*:}"
    echo "  FAIL: conflict marker in ${file#"$DOCS_DIR"/}:${rest%%:*}"
    FAIL=$((FAIL + 1))
  done <<<"$hits"
}

test_no_conflict_markers

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
