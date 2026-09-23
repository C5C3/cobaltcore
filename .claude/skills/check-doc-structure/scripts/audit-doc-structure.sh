#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# audit-doc-structure.sh — mechanical structure checks for the VitePress docs.
#   T1  frontmatter with title: on every page (quadrant: listed where absent)
#   T2  one H1 per page, no skipped heading level
#   T3  docs links and #anchors resolve (the build checks pages, not anchors)
#   T4  sidebar/nav links in docs/.vitepress/config.ts resolve
#   T5  no orphan page (neither in the sidebar/nav nor linked from any page)
#   T6  no bare <placeholder> in prose (Vue parses it as an unclosed element)
#
# The checks need a Markdown-aware walk, so they live in doc_structure.py
# beside this script. Pass --full to chain `npm run docs:build` (VitePress'
# dead-link gate, the CI `docs` job) after the audit; it needs `npm ci` first.
# Exit code 1 on any [FAIL].

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "${REPO_ROOT}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

FULL=0
if [[ "${1:-}" == "--full" ]]; then
  FULL=1
fi

if ! command -v python3 > /dev/null 2>&1; then
  echo "[INFO] python3 not on PATH — T1–T6 skipped"
  exit 0
fi

rc=0
python3 "${SCRIPT_DIR}/doc_structure.py" || rc=$?

if [[ "${FULL}" -eq 1 ]]; then
  echo
  echo "=== Authoritative gate — npm run docs:build ==="
  if [[ ! -d node_modules/vitepress ]]; then
    echo "[INFO] node_modules/vitepress missing — run: npm ci"
  elif npm run --silent docs:build > /dev/null 2>&1; then
    echo "[PASS] npm run docs:build: clean"
  else
    echo "[FAIL] npm run docs:build failed — rerun it without --silent for the dead-link report"
    rc=1
  fi
fi

echo
echo "=== Summary ==="
if [[ "${rc}" -eq 0 ]]; then
  echo "[PASS] no doc-structure findings"
else
  echo "[FAIL] doc-structure findings above"
fi
exit "${rc}"
