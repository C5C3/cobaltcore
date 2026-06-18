#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Repository-wide CI gate that keeps the whole source tree — code, tests, CI
# definitions, scripts, the Makefile, and the published docs — free of internal
# feature/requirement identifiers. Two families are guarded:
#
#   - feature / change IDs  of the form  CC-NNNN
#   - requirement IDs       of the form  REQ-NNN
#
# These are meaningless to anyone outside the internal tracker and add noise to
# every log line and comment they touch. The scan is case-insensitive so
# lowercase forms that creep in through anchor slugs (e.g. a
# "#enabling-prometheus--grafana-cc-0100" link target) are caught alongside
# prose mentions. A non-letter — or start of line — is required immediately
# before the ID so unrelated tokens such as "ecc-256" do not trip the gate.
#
# Scope is git-tracked files (git ls-files). This automatically excludes
# git-ignored build output (node_modules/, bin/, docs/.vitepress/{dist,cache}/,
# coverage *.out) and the architecture/ submodule, which git lists as a single
# gitlink entry rather than its contents — that directory is the upstream
# C5C3/C5C3 repository and is read-only from this worktree, so any IDs there are
# fixed upstream and pulled in via a submodule bump.
#
# Additional excludes:
#   - .planwerk/  internal planning/review tool records keyed by the IDs (the
#                 JSON record filenames *are* the IDs); scrubbing would break
#                 the tool's ID-keyed lookup.
#   - .claude/    Claude skill docs that use CC-NNNN / REQ-NNN as placeholders.
#   - this script it names the deny-list patterns verbatim.
#   - lockfiles   go.sum / go.work.sum / package-lock.json / Chart.lock may
#                 carry hash fragments that incidentally match.
#
# NOT excluded by design: real external references such as GitHub issue refs
# (GH-NNN, #NNN) are not matched by the (cc|req)-NNN deny-list, so they pass
# untouched. To add a future internal-ID scheme to the gate, extend `prefixes`.
#
# Exit codes:
#   0 — no internal IDs found
#   1 — at least one occurrence; offending file:line:match printed to stderr
#   2 — internal error (not run from inside the git work tree)
#
# Usage:
#   scripts/check-no-feature-ids.sh
#   make check-feature-ids

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  printf 'error: %s must run inside the git work tree\n' "${BASH_SOURCE[0]}" >&2
  exit 2
fi

# Internal-ID prefixes. Add future schemes here (e.g. 'cc|req|seq').
prefixes='cc|req'
# Match <PREFIX>-NNNN preceded by start-of-line or a non-letter, case-insensitive.
pattern="(^|[^a-z])(${prefixes})-[0-9]+"

# This script's own repo-relative path, so the self-exclude survives a rename.
self="scripts/check-no-feature-ids.sh"

# Tracked files minus the documented excludes. git ls-files emits newline-
# separated paths; the repo has no spaces in paths, so plain word-splitting via
# xargs is safe on both GNU and BSD userlands (no non-portable `grep -z`).
files="$(git ls-files \
  | grep -vE '^(\.planwerk/|\.claude/)' \
  | grep -vxF -- 'architecture' \
  | grep -vxF -- "${self}" \
  | grep -vE '(^|/)(go\.sum|go\.work\.sum|package-lock\.json|Chart\.lock)$' \
  || true)"

if [ -z "${files}" ]; then
  echo "ok: no tracked files to scan"
  exit 0
fi

hits="$(printf '%s\n' "${files}" \
  | grep -v '^$' \
  | xargs grep -HniE --binary-files=without-match "${pattern}" 2>/dev/null \
  || true)"

if [ -z "${hits}" ]; then
  echo "ok: no internal CC-/REQ- identifiers found in tracked files"
  exit 0
fi

{
  echo "error: internal feature/requirement IDs found (CC-NNNN / REQ-NNN)."
  echo
  echo "Source, tests, CI, scripts, and docs must describe behaviour, not cite"
  echo "internal tracking IDs. Remove each ID below; if it carried meaning,"
  echo "reword the surrounding text so the explanation survives without it."
  echo "Real GitHub issue references (GH-NNN, #NNN) are allowed and not matched."
  echo
  printf '%s\n' "${hits}"
} >&2
exit 1
