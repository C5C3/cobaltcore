#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# CI-runnable gate that keeps the published documentation under `docs/` free of
# internal feature/requirement identifiers. The user-facing docs describe
# *behaviour*; internal tracking IDs of the form `CC-NNNN` (feature / change
# IDs) and `REQ-NNN` (requirement IDs) are meaningless to site readers and must
# never appear in the documentation source.
#
# The scan is case-insensitive so lowercase forms that creep in through anchor
# slugs (e.g. a `#enabling-prometheus--grafana-cc-0100` link target) are caught
# alongside prose mentions. A non-letter — or start of line — is required
# immediately before the ID so unrelated tokens such as `ecc-256` do not trip
# the gate.
#
# Generated VitePress build output (`docs/.vitepress/dist`, `.../cache`) is
# excluded: it is git-ignored and regenerates from the source this gate guards.
# This script lives under `scripts/`, not `docs/`, so it never scans itself
# even though it names the patterns verbatim.
#
# Exit codes:
#   0 — no internal IDs found in docs/
#   1 — at least one occurrence; offending file:line:match printed to stderr
#   2 — internal error (docs/ directory missing)
#
# Usage:
#   scripts/check-docs-no-feature-ids.sh
#   make check-docs-ids

set -euo pipefail

# Resolve the repository root from the script's location so the script works
# regardless of the caller's working directory.
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"

DOCS_DIR="${REPO_ROOT}/docs"
if [ ! -d "${DOCS_DIR}" ]; then
  printf 'error: docs directory not found at %s\n' "${DOCS_DIR}" >&2
  exit 2
fi

# Match CC-NNNN / REQ-NNN preceded by start-of-line or a non-letter, so the gate
# catches `(CC-0106)`, `pre-CC-0106`, and `#...-cc-0100` but not `ecc-256`. With
# `-i`, the bracket class `[^a-z]` also excludes uppercase letters.
pattern='(^|[^a-z])(cc|req)-[0-9]+'

# `|| true` suppresses grep's exit-1 on "no matches" — the exit code is driven
# explicitly below. `--binary-files=without-match` skips any binary assets.
hits="$(grep -rniE --binary-files=without-match \
  --exclude-dir='dist' \
  --exclude-dir='cache' \
  "${pattern}" "${DOCS_DIR}" || true)"

if [ -z "${hits}" ]; then
  echo "ok: no internal CC-/REQ- identifiers found in docs/"
  exit 0
fi

{
  echo "error: internal feature/requirement IDs found in docs/ (CC-NNNN / REQ-NNN)."
  echo
  echo "User-facing documentation must describe behaviour, not cite internal"
  echo "tracking IDs. Remove each ID below; if it carried meaning, reword the"
  echo "surrounding text so the explanation survives without the identifier."
  echo
  # Strip the absolute REPO_ROOT prefix so paths are repo-relative and stable.
  printf '%s\n' "${hits}" | sed "s#^${REPO_ROOT}/##"
} >&2
exit 1
