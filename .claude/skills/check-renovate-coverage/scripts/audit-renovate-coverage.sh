#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# audit-renovate-coverage.sh — mechanical Renovate-coverage checks.
# Verifies that every version literal on disk is reachable by some
# Renovate manager (native or custom) and that each customManager is
# paired with packageRules and a regression test:
#   R1  every line in releases/*/source-refs.yaml matches the source-refs regex
#   R2  every <NAME>_VERSION="…" constant in hack/*.sh is matched by a
#       customManager managerFilePatterns entry
#   R3  every version: "…" literal in deploy/kind/base/*.yaml is matched by a
#       customManager managerFilePatterns entry
#   R4  every customManager has at least one packageRules entry pointing at
#       the same file (matched by managerFilePatterns vs matchFileNames)
#   R5  every source-refs.yaml entry is covered by a packageRule that disables
#       major bumps
#   R6  every customManager has a sibling regression test in tests/unit/renovate/
#
# Defers JSON-shape validation to `renovate-config-validator`. Exit code 1 on [FAIL].

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "${REPO_ROOT}"

FAIL_COUNT=0
fail() { echo "[FAIL] $*"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
pass() { echo "[PASS] $*"; }
info() { echo "[INFO] $*"; }
hdr()  { echo; echo "=== $* ==="; }

RENOVATE="renovate.json"
if [[ ! -f "${RENOVATE}" ]]; then
  fail "missing ${RENOVATE} at repo root"
  exit 1
fi

# Helpers that read renovate.json with jq if present; otherwise grep fallbacks.
have_jq=0
if command -v jq >/dev/null 2>&1; then
  have_jq=1
else
  info "jq not on PATH — falling back to grep-only inspection (less precise)"
fi

# ---------------------------------------------------------------------------
# R1 — releases/*/source-refs.yaml lines match the source-refs regex
# ---------------------------------------------------------------------------
hdr "R1: releases/*/source-refs.yaml entries match the source-refs customManager regex"
# The current regex (per renovate.json): ^(?<depName>[\w.-]+):\s*"(?<currentValue>\d+\.\d+\.\d+)"
# Express the positive form in extended POSIX regex.
SR_RE='^[A-Za-z0-9_.-]+:[[:space:]]*"[0-9]+\.[0-9]+\.[0-9]+"'
shopt -s nullglob
for f in releases/*/source-refs.yaml; do
  while IFS= read -r line; do
    # Skip blanks, comments, and the SPDX header.
    [[ -z "${line}" || "${line}" =~ ^# ]] && continue
    if [[ "${line}" =~ ${SR_RE} ]]; then
      pass "${f}: '${line}' matches source-refs regex"
    else
      fail "${f}: '${line}' does not match source-refs customManager regex"
    fi
  done < "${f}"
done
shopt -u nullglob

# ---------------------------------------------------------------------------
# R2 — hack/*.sh VERSION constants are matched by a managerFilePatterns entry
# ---------------------------------------------------------------------------
hdr "R2: hack/*.sh <NAME>_VERSION=… constants are claimed by a customManager"
# Collect managerFilePatterns from renovate.json.
if [[ "${have_jq}" -eq 1 ]]; then
  cm_files_raw=$(jq -r '.customManagers[].managerFilePatterns[]' "${RENOVATE}" | sort -u)
else
  cm_files_raw=$(grep -oE '"/[^"]+/"' "${RENOVATE}" | tr -d '"' | sort -u)
fi
# Normalise regex form (/path/to/file\.ext$/ or /path/.*/file\.ext$/) into a bare,
# anchored ERE we can match against absolute paths with grep -E.
cm_files=$(echo "${cm_files_raw}" \
  | sed -E 's|^/||; s|/$||; s|\$$||; s|\\\.|.|g')
info "customManager managerFilePatterns (raw): $(echo "${cm_files_raw}" | tr '\n' ',' | sed 's/,$//')"
info "customManager managerFilePatterns (normalised): $(echo "${cm_files}" | tr '\n' ',' | sed 's/,$//')"

# matches_cm returns 0 if the given path is claimed by any customManager file pattern.
matches_cm() {
  local path="$1" cmf
  while IFS= read -r cmf; do
    [[ -z "${cmf}" ]] && continue
    if echo "${path}" | grep -qE "^${cmf}$"; then
      return 0
    fi
  done <<< "${cm_files}"
  return 1
}

for f in hack/*.sh; do
  [[ -f "${f}" ]] || continue
  while IFS= read -r ln; do
    var=$(echo "${ln}" | sed -nE 's/^([A-Z_]+_VERSION)=.*/\1/p')
    [[ -z "${var}" ]] && var=$(echo "${ln}" | sed -nE 's/^([A-Z_]+_VERSION)=\$\{[A-Z_]+:-.*/\1/p')
    [[ -z "${var}" ]] && continue
    if matches_cm "${f}"; then
      pass "${f}:${var} claimed by a customManager managerFilePatterns entry"
    else
      fail "${f}:${var} not claimed by any customManager — pin will not be bumped"
    fi
  done < "${f}"
done

# ---------------------------------------------------------------------------
# R3 — deploy/kind/base/*.yaml version: "…" literals are claimed
# ---------------------------------------------------------------------------
hdr "R3: deploy/kind/base/*.yaml version: \"…\" literals are claimed"
shopt -s nullglob
for f in deploy/kind/base/*.yaml; do
  if grep -qE '^\s*-?\s*version:\s*"' "${f}"; then
    if matches_cm "${f}"; then
      pass "${f}: version literal claimed by a customManager"
    else
      fail "${f}: version literal not claimed by any customManager"
    fi
  fi
done
shopt -u nullglob

# ---------------------------------------------------------------------------
# R4 — every customManager has a paired packageRules entry
# ---------------------------------------------------------------------------
hdr "R4: every customManager has a paired packageRules entry (same file pattern)"
if [[ "${have_jq}" -eq 1 ]]; then
  pr_files=$(jq -r '.packageRules[]?.matchFileNames[]?' "${RENOVATE}" | sort -u)
  info "packageRules matchFileNames: $(echo "${pr_files}" | tr '\n' ',' | sed 's/,$//')"
  while IFS= read -r cmf; do
    # Strip leading/trailing slashes and the regex anchors to match packageRules glob style.
    glob=$(echo "${cmf}" | sed -E 's|^/||; s|\$/$||; s|\\\.|.|g; s|\.\*|**|g')
    # Heuristic match: any packageRule matchFileNames entry containing the basename.
    base=$(basename "${glob}")
    if echo "${pr_files}" | grep -q "${base}"; then
      pass "customManager for ${cmf} has packageRules coverage (matched on ${base})"
    else
      fail "customManager for ${cmf} has NO packageRules entry — updates land untriaged"
    fi
  done < <(echo "${cm_files}")
else
  info "skipping R4: jq not available"
fi

# ---------------------------------------------------------------------------
# R5 — source-refs.yaml has a major-bump-disable packageRule
# ---------------------------------------------------------------------------
hdr "R5: source-refs.yaml has a major-bump-disable packageRule"
if [[ "${have_jq}" -eq 1 ]]; then
  match=$(jq -r '
    .packageRules[]?
    | select((.matchFileNames // []) | any(test("source-refs")))
    | select((.matchUpdateTypes // []) | index("major"))
    | select(.enabled == false)
    | "found"
  ' "${RENOVATE}" | head -1)
  if [[ "${match}" == "found" ]]; then
    pass "source-refs.yaml has a major-bump-disable packageRule"
  else
    fail "source-refs.yaml has no packageRule disabling major bumps — accidental release.major bumps will land"
  fi
else
  if grep -q "source-refs" "${RENOVATE}" && grep -q '"major"' "${RENOVATE}"; then
    info "source-refs + major appear in renovate.json — confirm by hand (no jq)"
  fi
fi

# ---------------------------------------------------------------------------
# R6 — every customManager has a sibling regression test
# ---------------------------------------------------------------------------
hdr "R6: each customManager has a regression test under tests/unit/renovate/"
if [[ -d tests/unit/renovate ]]; then
  test_count=$(find tests/unit/renovate -name '*_test.sh' | wc -l | tr -d ' ')
  cm_count=$(echo "${cm_files}" | grep -c '.' || true)
  info "customManagers: ${cm_count}; tests under tests/unit/renovate: ${test_count}"
  if [[ "${cm_count}" -gt "${test_count}" ]]; then
    fail "more customManagers (${cm_count}) than tests (${test_count}) — at least one is untested"
  else
    pass "each customManager appears to have a sibling test (counts match)"
  fi
else
  fail "missing tests/unit/renovate/ directory"
fi

# ---------------------------------------------------------------------------
# Inventory — Makefile and workflow tool pins (R-MEDIUM candidates)
# ---------------------------------------------------------------------------
hdr "Inventory — tool pins outside Renovate coverage (review aid)"
grep -nE '^[A-Z_]+_VERSION \?= ' Makefile 2>/dev/null | sed 's/^/[INFO] Makefile pin: /' || true
grep -rEn '^[[:space:]]*[A-Z_]+_VERSION:\s*' .github/workflows/ 2>/dev/null | sed 's/^/[INFO] workflow env pin: /' || true

# ---------------------------------------------------------------------------
hdr "Summary"
if [[ ${FAIL_COUNT} -eq 0 ]]; then
  echo "[PASS] no Renovate-coverage findings"
  exit 0
else
  echo "[FAIL] ${FAIL_COUNT} Renovate-coverage finding(s)"
  exit 1
fi
