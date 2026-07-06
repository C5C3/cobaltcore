#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Repository-wide CI gate that stops NEW unresolved reviewer markers from
# landing in the tracked source tree. A reviewer marker is the literal phrase
# "please verify" (case-insensitive), almost always in its long form
# "Reviewer: please verify ..." — a review-time question that was committed
# instead of being answered. Each one is an open decision hiding in a comment:
# it reads like documentation but actually documents doubt.
#
# The tree currently carries a known set of these markers. They are tolerated
# through a per-file baseline (scripts/review-markers-baseline.txt) so the
# gate can be always-on without first forcing a big-bang cleanup:
#
#   - a file with MORE matches than its baseline entry        -> FAIL
#   - a file with matches but NO baseline entry               -> FAIL
#   - a file with FEWER matches than its baseline entry       -> INFO only;
#     the baseline is stale and should be tightened via --update-baseline
#     so the resolved marker cannot silently come back.
#
# The intended direction is monotonic: baseline counts only ever go down.
#
# Scope is git-tracked files (git ls-files). This automatically excludes
# git-ignored build output (node_modules/, bin/, docs/.vitepress/{dist,cache}/,
# coverage *.out) and the architecture/ submodule, which git lists as a single
# gitlink entry rather than its contents.
#
# Additional excludes:
#   - .planwerk/  internal planning/review tool records; they quote the
#                 marker text verbatim when describing review findings.
#   - .claude/    Claude skill docs that cite the marker text as an example.
#   - this script and the baseline file — both name the phrase verbatim.
#   - lockfiles   go.sum / go.work.sum / package-lock.json / Chart.lock.
#
# The scan is line-based, so a marker whose phrase wraps across a comment-line
# break between the two words is not caught — acceptable, since every known
# occurrence keeps the phrase on one line.
#
# Exit codes:
#   0 — no file exceeds its baseline
#   1 — at least one new/unbaselined marker; file:line:match printed to stderr
#   2 — internal error (not inside the git work tree, or baseline missing)
#
# Usage:
#   scripts/check-no-review-markers.sh                     # gate mode
#   scripts/check-no-review-markers.sh --update-baseline   # regenerate baseline
#   make check-review-markers

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  printf 'error: %s must run inside the git work tree\n' "${BASH_SOURCE[0]}" >&2
  exit 2
fi

# The guarded phrase. Case-insensitive; a single line-internal whitespace run
# between the two words covers every committed form (including continuation
# lines that wrap the leading "Reviewer:" onto the previous line).
pattern='please[[:space:]]+verify'

# Repo-relative paths, so the self-excludes survive a rename.
self="scripts/check-no-review-markers.sh"
baseline="scripts/review-markers-baseline.txt"

update=0
if [ "${1:-}" = "--update-baseline" ]; then
  update=1
fi

# Tracked files minus the documented excludes. git ls-files emits newline-
# separated paths; the repo has no spaces in paths, so plain word-splitting via
# xargs is safe on both GNU and BSD userlands (no non-portable `grep -z`).
files="$(git ls-files \
  | grep -vE '^(\.planwerk/|\.claude/)' \
  | grep -vxF -- 'architecture' \
  | grep -vxF -- "${self}" \
  | grep -vxF -- "${baseline}" \
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

# Per-file "path:count" lines, sorted by path. Empty when there are no hits.
counts="$(printf '%s\n' "${hits}" \
  | grep -v '^$' \
  | awk -F: '{print $1}' \
  | sort \
  | uniq -c \
  | awk '{print $2 ":" $1}' \
  || true)"

if [ "${update}" -eq 1 ]; then
  {
    echo "# Baseline of tolerated reviewer markers, one 'path:count' per line."
    echo "# Maintained by scripts/check-no-review-markers.sh --update-baseline."
    echo "# Counts may only shrink: resolve a marker, then regenerate this file."
    printf '%s\n' "${counts}" | grep -v '^$' || true
  } > "${baseline}"
  total="$(printf '%s\n' "${counts}" | grep -c -v '^$' || true)"
  markers="$(printf '%s\n' "${hits}" | grep -c -v '^$' || true)"
  echo "ok: baseline regenerated — ${markers} marker(s) across ${total} file(s)"
  exit 0
fi

if [ ! -f "${baseline}" ]; then
  {
    printf 'error: baseline file %s is missing.\n' "${baseline}"
    printf 'Regenerate it with: %s --update-baseline\n' "${self}"
  } >&2
  exit 2
fi

# Compare current counts against the baseline. Emits one line per finding:
#   OVER  <path> <current> <baseline>   (violation)
#   STALE <path> <current> <baseline>   (baseline higher than reality)
verdicts="$(awk -F: '
  NR == FNR {
    if ($0 ~ /^[[:space:]]*(#|$)/) next
    base[$1] = $2 + 0
    next
  }
  {
    if ($1 == "") next
    cur[$1] = $2 + 0
  }
  END {
    for (f in cur) {
      b = (f in base) ? base[f] : 0
      if (cur[f] > b) printf "OVER %s %d %d\n", f, cur[f], b
    }
    for (f in base) {
      c = (f in cur) ? cur[f] : 0
      if (c < base[f]) printf "STALE %s %d %d\n", f, c, base[f]
    }
  }' "${baseline}" <(printf '%s\n' "${counts}") | sort -k1,1 -k2,2)"

stale="$(printf '%s\n' "${verdicts}" | grep '^STALE ' || true)"
over="$(printf '%s\n' "${verdicts}" | grep '^OVER ' || true)"

if [ -n "${stale}" ]; then
  printf '%s\n' "${stale}" | while read -r _ f c b; do
    echo "info: ${f} now has ${c} marker(s) but the baseline allows ${b} — baseline is stale, tighten it"
  done
  echo "info: regenerate with: ${self} --update-baseline"
fi

if [ -z "${over}" ]; then
  echo "ok: no file exceeds its reviewer-marker baseline"
  exit 0
fi

{
  echo "error: new unresolved reviewer marker(s) found."
  echo
  echo "A committed review question is an unanswered decision, not documentation."
  echo "Answer the question, fold the answer into the surrounding comment, and"
  echo "drop the marker phrase. The pre-existing markers are baselined in"
  echo "${baseline}; new ones may not land."
  echo
  printf '%s\n' "${over}" | while read -r _ f c b; do
    echo "-- ${f}: ${c} marker(s), baseline allows ${b}:"
    printf '%s\n' "${hits}" | grep -E "^${f}:" || true
  done
} >&2
exit 1
