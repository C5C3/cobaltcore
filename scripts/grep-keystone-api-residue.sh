#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# CI-runnable residue scanner for the operator sub-resource rename that dropped
# the legacy `-api` suffix from operator-managed sub-resources. Greps the source
# tree for both the literal substring `keystone-api` and its templated/placeholder
# forms (`{name}-api`, `<name>-api`, `<cr-name>-api`, `{cr-name}-api`) and fails
# the build if any occurrence is not explicitly annotated as a permitted legacy
# reference.
#
# Two patterns are scanned:
#   1. Literal:    `keystone-api`
#   2. Templated:  `{name}-api`, `<name>-api`, `<cr-name>-api`, `{cr-name}-api`
#
# Both patterns share the same marker convention: a line is considered
# permitted iff it contains the marker `keystone-api-legacy` (in any comment
# style appropriate to the file format, e.g. `// keystone-api-legacy`,
# `# keystone-api-legacy`, `<!-- keystone-api-legacy -->`). The marker documents
# that the author intentionally preserved the legacy name (typically because it
# explains the rename to readers, or because it is a historical fixture
# identifier that tests pin against).
#
# Why scan templated forms separately: literal residue typically comes from
# code/configuration that names the resource directly, while templated residue
# comes from documentation tables and prose ("`{name}-api`"). A literal-only
# scan misses stale doc tables that contradict the current naming convention;
# this scanner catches both.
#
# Architecture docs (`architecture/`) are intentionally excluded from the scan:
# that directory is a git submodule pointing at the upstream C5C3 repository
# and is read-only from this worktree. Stale references inside `architecture/`
# must be fixed upstream and pulled in via a submodule SHA bump.
#
# This script itself is excluded from BOTH scans because its description
# mentions the literal and templated forms verbatim and would otherwise
# self-trip if `scripts/` is ever added to the search roots.
#
# Search-root completeness: every visible top-level directory in the repo
# must be either listed in SEARCH_ROOTS or in EXCLUDED_ROOTS so a newly added
# top-level directory cannot silently bypass the residue gate. If a freshly
# created directory is neither scanned nor explicitly excluded, the script
# exits with code 2 and instructs the author to declare its intent.
#
# Exit codes:
#   0 — no unmarked occurrences found (literal or templated)
#   1 — at least one unmarked occurrence; details printed to stderr
#   2 — internal error (e.g. a search root is missing)
#
# Usage:
#   scripts/grep-keystone-api-residue.sh        # scan default roots
#   bash scripts/grep-keystone-api-residue.sh   # ditto, no exec bit needed

set -euo pipefail

# Resolve the repository root from the script's location so the script works
# regardless of the caller's working directory.
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"

# Search roots, minus the `architecture/` submodule (see header comment for
# rationale).
SEARCH_ROOTS=(
  "operators"
  "tests"
  "docs"
  "hack"
  "deploy"
)

# Top-level directories that are intentionally NOT scanned. Each entry must be
# accompanied by a one-line rationale so reviewers can challenge the exclusion
# instead of treating it as opaque. Adding a new top-level directory without
# also adding it to SEARCH_ROOTS or here causes the script to exit 2.
EXCLUDED_ROOTS=(
  "architecture"  # git submodule; residue must be fixed upstream
  "internal"      # shared Go libs; verified clean of `keystone-api`
  "scripts"       # contains this script, which references the patterns by design
  "releases"      # generated install manifests; sourced from operators/ which is scanned
  "images"        # binary container assets, not text-greppable
  "LICENSES"      # SPDX license texts; out of scope
)

# Verify each search root exists. A missing root would silently make the scan
# vacuously pass, which would defeat the purpose of the CI gate.
missing_roots=()
for root in "${SEARCH_ROOTS[@]}"; do
  if [ ! -d "${REPO_ROOT}/${root}" ]; then
    missing_roots+=("${root}")
  fi
done
if [ "${#missing_roots[@]}" -gt 0 ]; then
  printf 'error: search roots missing under %s:\n' "${REPO_ROOT}" >&2
  printf '  - %s\n' "${missing_roots[@]}" >&2
  exit 2
fi

# Detect newly added top-level directories that are neither scanned nor
# explicitly excluded. Hidden directories (e.g. `.git`, `.github`, `.planwerk`)
# are skipped: dotfiles carry their own conventions and including them would
# muddle CI infra (`.github/`) and review history (`.planwerk/`) into the
# residue gate. Anything visible must be classified.
declared_roots=$(printf '%s\n' "${SEARCH_ROOTS[@]}" "${EXCLUDED_ROOTS[@]}" | LC_ALL=C sort -u)
unknown_roots=()
for entry in "${REPO_ROOT}"/*/; do
  [ -d "${entry}" ] || continue
  name="$(basename "${entry}")"
  if ! printf '%s\n' "${declared_roots}" | grep -Fxq -- "${name}"; then
    unknown_roots+=("${name}")
  fi
done
if [ "${#unknown_roots[@]}" -gt 0 ]; then
  {
    printf 'error: top-level directories present but neither scanned nor excluded:\n'
    printf '  - %s\n' "${unknown_roots[@]}"
    printf 'Add each to SEARCH_ROOTS (to include in the residue scan) or to\n'
    printf 'EXCLUDED_ROOTS (with a one-line rationale) in this script. This gate\n'
    printf 'exists so a newly created top-level directory cannot silently bypass\n'
    printf 'the keystone-api residue check.\n'
  } >&2
  exit 2
fi

cd "${REPO_ROOT}"

# Run two scans (literal and templated) and union the unmarked hits. Each scan
# uses `grep -rnE` to enable extended-regex alternation; `|| true` suppresses
# grep's exit-1 when there are no matches (we drive the exit code from our own
# filters below). `--exclude` skips this script itself for both scans because
# its description names the literal and templated forms verbatim and would
# otherwise self-trip if `scripts/` is ever added to SEARCH_ROOTS.
literal_pattern='keystone-api'
templated_pattern='\{name\}-api|<name>-api|<cr-name>-api|\{cr-name\}-api'

literal_hits="$(grep -rnE --binary-files=without-match \
  --exclude='grep-keystone-api-residue.sh' \
  "${literal_pattern}" "${SEARCH_ROOTS[@]}" || true)"
templated_hits="$(grep -rnE --binary-files=without-match \
  --exclude='grep-keystone-api-residue.sh' \
  "${templated_pattern}" "${SEARCH_ROOTS[@]}" || true)"

# Combined hits — used for the "no occurrences at all" early exit and for the
# total-count summary in the "everything annotated" success path.
all_hits="$(printf '%s\n%s\n' "${literal_hits}" "${templated_hits}" \
  | sed '/^$/d' || true)"

if [ -z "${all_hits}" ]; then
  echo "ok: no literal or templated 'keystone-api' / '{name}-api' occurrences found in: ${SEARCH_ROOTS[*]}"
  exit 0
fi

# Filter out lines that carry the explicit `keystone-api-legacy` annotation.
# Anything that survives is unmarked residue and must be addressed (either by
# removing the substring or by adding the marker if the reference is intentional).
unmarked_hits="$(printf '%s\n' "${all_hits}" | grep -v 'keystone-api-legacy' || true)"

if [ -z "${unmarked_hits}" ]; then
  marked_count="$(printf '%s\n' "${all_hits}" | wc -l | tr -d '[:space:]')"
  echo "ok: all ${marked_count} 'keystone-api' / '{name}-api' occurrences are annotated with 'keystone-api-legacy'"
  exit 0
fi

unmarked_count="$(printf '%s\n' "${unmarked_hits}" | wc -l | tr -d '[:space:]')"
{
  printf 'error: %s unmarked occurrence(s) of literal "keystone-api" or templated\n' "${unmarked_count}"
  printf '"{name}-api" / "<name>-api" / "<cr-name>-api" / "{cr-name}-api" found:\n\n'
  printf '%s\n\n' "${unmarked_hits}"
  printf 'Each occurrence must either be removed (preferred — the operator now\n'
  printf 'emits sub-resources at the bare CR name; docs should describe the\n'
  printf 'current behavior, not the legacy form) or, if the legacy reference is\n'
  printf 'intentional (e.g. historical context in a comment, a pinned fixture\n'
  printf 'identifier in a chaos-test, or the upgrade-flow runbook documenting\n'
  printf 'the rename itself), annotated on the same line with the marker\n'
  printf '`keystone-api-legacy` so this scanner recognises it.\n'
} >&2
exit 1
