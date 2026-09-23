#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# count-style-budget.sh — count STYLE_GUIDE.md's rhetorical-device budget per
# documentation page, the mechanical half of check-doc-expressions step 3.
# Strips frontmatter, fenced code, HTML comments and tags, inline code, link
# and image URLs, code imports and container markers, then counts per page:
#   em      em-dashes                          <= 2 per 1,000 words
#   ital    italic spans *x* / _x_             <= 4 per 1,000 words
#   anti    antithesis candidates              <= 1 per page
#   call    ::: info/tip/warning/danger boxes  <= 2 per page
#   aph     aphoristic one-liner candidates    <= 1 per page
#   filler  retired filler vocabulary          0
#   label   quality self-labels                0
# One line per page ([OVER] or [PASS], '!' on each device over its
# allowance), then the top pages by excess. Antithesis and aphorism are
# heuristics: judge each candidate before reporting it.
#
# The parsing lives in count_style_budget.py beside this script (it needs
# regex lookarounds and paragraph structure); without python3 the count is
# skipped.
#
# Usage:
#   count-style-budget.sh [--strict] [--top N] [<page.md|dir>...]
#   count-style-budget.sh --page <page.md>    # every hit with its line number
# Default paths: every docs/**/*.md except docs/.vitepress/, plus README.md.
# A triage aid: exit 0 always, unless --strict and a page is over budget.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../../.." && pwd)"

if ! command -v python3 > /dev/null 2>&1; then
  echo "[INFO] python3 not on PATH — style-budget count skipped"
  exit 0
fi

# Relative paths resolve against the caller's directory; the default page set
# and the reported paths are anchored at the repository root.
exec python3 "${SCRIPT_DIR}/count_style_budget.py" --root "${REPO_ROOT}" "$@"
