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
#   R4  every customManager's managerFilePatterns match at least one tracked
#       file (a manager matching nothing is dead), and some packageRules entry
#       applies to it: its matchFileNames glob matches one of those files or
#       its matchPackageNames / matchDepNames names the manager's dependency
#   R5  every source-refs.yaml entry is covered by a packageRule that disables
#       major bumps
#   R6  every customManager is singled out by some tests/unit/renovate/*_test.sh
#       (the test names its file, its dependency, or an identifier from its
#       regex, and not only needles another manager shares)
#   R7  every <NAME>_VERSION pin that appears in BOTH the Makefile and the
#       .github/workflows/ci.yaml env block carries the same value (the
#       Makefile comment "Must be kept in sync" is enforced mechanically)
#
# R4 and R6 read renovate.json with jq and are skipped without it. Defers
# JSON-shape validation to `renovate-config-validator`. Exit code 1 on [FAIL].

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
    if grep -qE "^${cmf}$" <<<"${path}"; then
      return 0
    fi
  done <<< "${cm_files}"
  return 1
}

for f in hack/*.sh; do
  [[ -f "${f}" ]] || continue
  # Only the <NAME>_VERSION= lines reach the loop: a sed fork per line of every
  # hack script cost twenty seconds.
  while IFS= read -r ln; do
    var=$(echo "${ln}" | sed -nE 's/^([A-Z_]+_VERSION)=.*/\1/p')
    [[ -z "${var}" ]] && continue
    # Runtime-resolved values are not pins Renovate could bump: command
    # substitutions (resolved from test-refs.yaml, Chart.yaml, …) and
    # ${VAR:?} required-env passthroughs. ${VAR:-literal} fallbacks DO
    # carry a bumpable default and stay in scope.
    val="${ln#*=}"
    # shellcheck disable=SC2016 # the patterns match a literal "$(", not an expansion
    case "${val}" in
      '$('* | '"$('*) continue ;;
      *':?'*) continue ;;
    esac
    if matches_cm "${f}"; then
      pass "${f}:${var} claimed by a customManager managerFilePatterns entry"
    else
      fail "${f}:${var} not claimed by any customManager — pin will not be bumped"
    fi
  done < <(grep -E '^[A-Z_]+_VERSION=' "${f}" || true)
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
# Manager / rule model for R4, R6 and the inventory (needs jq)
# ---------------------------------------------------------------------------
# to_ere — rewrite column 3 of "<index>\t<kind>\t<pattern>" rows into a POSIX
# ERE appended as column 4, the way Renovate reads the pattern: "/…/" (or
# "/…/i") is a regex, anything else a minimatch glob. For the regex the
# slashes go and the JS shorthands \d \w \/ (?: become ERE. For the glob "**"
# spans directories only as a whole path segment ("a/**/b", "a/**"); inside a
# segment ("**.yaml") it is a "*", and "*" and "?" never cross "/". A negated
# pattern ("!…") gets "-": the audit reads positive patterns only.
to_ere() {
  awk -F '\t' -v OFS='\t' '
  function glob2ere(g,    i, n, c, out, prev, nxt, depth, j) {
    out = "^"; n = length(g); i = 1; depth = 0
    while (i <= n) {
      c = substr(g, i, 1)
      if (c == "*") {
        if (substr(g, i + 1, 1) == "*") {
          prev = (i == 1) ? "/" : substr(g, i - 1, 1)
          nxt = substr(g, i + 2, 1)
          if (prev == "/" && nxt == "/") { out = out "(.*/)?"; i += 3; continue }
          if (prev == "/" && nxt == "") { out = out ".*"; i += 2; continue }
          out = out "[^/]*"; i += 2; continue
        }
        out = out "[^/]*"; i++; continue
      }
      if (c == "?") { out = out "[^/]"; i++; continue }
      if (c == "{") { out = out "("; depth++; i++; continue }
      if (c == "}" && depth > 0) { out = out ")"; depth--; i++; continue }
      if (c == "," && depth > 0) { out = out "|"; i++; continue }
      if (c == "[") {
        j = index(substr(g, i + 1), "]")
        if (j > 0) {
          c = substr(g, i + 1, j)
          if (substr(c, 1, 1) == "!") c = "^" substr(c, 2)
          out = out "[" c; i += j + 1; continue
        }
      }
      if (index(".+()|^$\\{[", c) > 0) { out = out "\\" c; i++; continue }
      out = out c; i++
    }
    return out "$"
  }
  function re2ere(r) {
    sub(/^\//, "", r); sub(/\/i?$/, "", r)
    gsub(/\\d/, "[0-9]", r); gsub(/\\w/, "[A-Za-z0-9_]", r)
    gsub(/\\\//, "/", r); gsub(/\(\?:/, "(", r)
    return r
  }
  {
    if ($3 ~ /^!/) print $1, $2, $3, "-"
    else if ($3 ~ /^\/.+\/i?$/) print $1, $2, $3, re2ere($3)
    else print $1, $2, $3, glob2ere($3)
  }'
}

NL=$'\n'
TAB=$'\t'
CM_N=0
R_N=0
if [[ "${have_jq}" -eq 1 ]]; then
  # Tracked files are what Renovate scans; fall back to the work tree outside git.
  TRACKED=$(git ls-files 2>/dev/null || true)
  if [[ -z "${TRACKED}" ]]; then
    TRACKED=$(find . -type f -not -path './.git/*' | sed 's|^\./||')
    info "no git index — matching managerFilePatterns against every file on disk"
  fi

  # One row per manager attribute. The dependency name is depNameTemplate, or
  # the literal a matchString captures as (?<depName>ghcr\.io/…); the package
  # name is packageNameTemplate, else the dependency name (Renovate's default).
  # Templated values ({{…}}) are unknown statically and left out.
  cm_rows=$(jq -r '
    def known: select(. != null and (test("\\{\\{") | not));
    def lit_dep: [ (.matchStrings // [])[]
      | capture("\\(\\?<depName>(?<l>([A-Za-z0-9_/:-]|\\\\\\.)+)\\)")?
      | .l | gsub("\\\\\\."; ".") ] | first;
    .customManagers // [] | to_entries[] | .key as $i | .value as $m
    | ($m.depNameTemplate // ($m | lit_dep)) as $dep
    | ($m.packageNameTemplate // $dep) as $pkg
    | ( ($m.managerFilePatterns // [])[] | "\($i)\tpat\t\(.)" ),
      ( $dep | known | "\($i)\tdep\t\(.)" ),
      ( $pkg | known | "\($i)\tpkg\t\(.)" ),
      ( $m.datasourceTemplate | known | "\($i)\tds\t\(.)" ),
      ( ($m.matchStrings // [])[] | gsub("[\t\n]"; " ") | "\($i)\tmatch\t\(.)" )
  ' "${RENOVATE}" | to_ere)
  CM_N=$(jq '.customManagers // [] | length' "${RENOVATE}")

  rule_rows=$(jq -r '
    .packageRules // [] | to_entries[] | .key as $j | .value as $r
    | ( ($r.matchFileNames // [])[] | "\($j)\tfile\t\(.)" ),
      ( ($r.matchPackageNames // [])[] | "\($j)\tpkg\t\(.)" ),
      ( ($r.matchDepNames // [])[] | "\($j)\tdep\t\(.)" ),
      ( ($r.matchManagers // [])[] | "\($j)\tmgr\t\(.)" ),
      ( ($r.matchDatasources // [])[] | "\($j)\tds\t\(.)" )
  ' "${RENOVATE}" | to_ere)
  R_N=$(jq '.packageRules // [] | length' "${RENOVATE}")

  # Newline-joined per-index lists (Bash 3.2 has no associative arrays).
  # CM_PAT / R_FILE / R_PKG / R_DEP rows are "<ere>\t<pattern as written>".
  CM_PAT=(); CM_DEP=(); CM_PKG=(); CM_DS=(); CM_MATCH=(); CM_FILES=()
  R_FILE=(); R_PKG=(); R_DEP=(); R_MGR=(); R_DS=()
  while IFS="${TAB}" read -r idx kind val ere; do
    [[ -z "${idx}" ]] && continue
    case "${kind}" in
      pat)   CM_PAT[idx]="${CM_PAT[idx]:-}${ere}${TAB}${val}${NL}" ;;
      dep)   CM_DEP[idx]="${val}" ;;
      pkg)   CM_PKG[idx]="${val}" ;;
      ds)    CM_DS[idx]="${val}" ;;
      match) CM_MATCH[idx]="${CM_MATCH[idx]:-}${val}${NL}" ;;
    esac
  done <<< "${cm_rows}"
  while IFS="${TAB}" read -r idx kind val ere; do
    [[ -z "${idx}" ]] && continue
    case "${kind}" in
      file) R_FILE[idx]="${R_FILE[idx]:-}${ere}${TAB}${val}${NL}" ;;
      pkg)  R_PKG[idx]="${R_PKG[idx]:-}${ere}${TAB}${val}${NL}" ;;
      dep)  R_DEP[idx]="${R_DEP[idx]:-}${ere}${TAB}${val}${NL}" ;;
      mgr)  R_MGR[idx]="${R_MGR[idx]:-}${val}${NL}" ;;
      ds)   R_DS[idx]="${R_DS[idx]:-}${val}${NL}" ;;
    esac
  done <<< "${rule_rows}"
fi

# cm_label <i> — "customManagers[<i>] (<dependency, else first pattern>)".
cm_label() {
  local first="${CM_PAT[$1]:-}"
  first="${first%%"${NL}"*}"
  echo "customManagers[$1] (${CM_DEP[$1]:-${first#*"${TAB}"}})"
}

# name_matches <name> <rows> — succeed when <name> matches one "<ere>\t<pattern>"
# row; MATCHED holds the pattern as written.
name_matches() {
  local name="$1" line re
  MATCHED=""
  for line in $2; do
    re="${line%%"${TAB}"*}"
    [[ "${re}" == "-" ]] && continue
    if [[ "${name}" =~ ${re} ]]; then
      MATCHED="${line#*"${TAB}"}"
      return 0
    fi
  done
  return 1
}

# rule_pairs <i> <j> — succeed when packageRules[<j>] applies to the updates of
# customManagers[<i>]; PAIRED_BY says why. Renovate ANDs a rule's match*
# conditions, so a condition the manager provably fails (another file, another
# named package, a non-custom matchManagers) rules the rule out. A condition
# the audit cannot evaluate (a dependency name captured at runtime) does not.
# Pairing needs one positive hit: a matchFileNames glob on one of the
# manager's tracked files, or a name list naming its dependency.
rule_pairs() {
  local i="$1" j="$2" line re glob f hit=""
  PAIRED_BY=""
  if [[ -n "${R_MGR[j]:-}" ]]; then
    case "${NL}${R_MGR[j]}" in
      *"${NL}custom.regex${NL}"* | *"${NL}regex${NL}"*) ;;
      *) return 1 ;;
    esac
  fi
  if [[ -n "${R_DS[j]:-}" && -n "${CM_DS[i]:-}" ]]; then
    case "${NL}${R_DS[j]}" in
      *"${NL}${CM_DS[i]}${NL}"*) ;;
      *) return 1 ;;
    esac
  fi
  if [[ -n "${R_FILE[j]:-}" ]]; then
    for line in ${R_FILE[j]}; do
      re="${line%%"${TAB}"*}"
      glob="${line#*"${TAB}"}"
      [[ "${re}" == "-" ]] && continue
      for f in ${CM_FILES[i]:-}; do
        if [[ "${f}" =~ ${re} ]]; then
          hit="matchFileNames '${glob}' (${f})"
          break 2
        fi
      done
    done
    [[ -z "${hit}" ]] && return 1
  fi
  if [[ -n "${R_PKG[j]:-}" && -n "${CM_PKG[i]:-}" ]]; then
    name_matches "${CM_PKG[i]}" "${R_PKG[j]}" || return 1
    hit="${hit:+${hit} + }matchPackageNames '${MATCHED}'"
  fi
  if [[ -n "${R_DEP[j]:-}" && -n "${CM_DEP[i]:-}" ]]; then
    name_matches "${CM_DEP[i]}" "${R_DEP[j]}" || return 1
    hit="${hit:+${hit} + }matchDepNames '${MATCHED}'"
  fi
  [[ -z "${hit}" ]] && return 1
  PAIRED_BY="${hit}"
  return 0
}

# ---------------------------------------------------------------------------
# R4 — every customManager matches tracked files and is paired with a rule
# ---------------------------------------------------------------------------
hdr "R4: every customManager matches tracked files and is paired with a packageRules entry"
if [[ "${have_jq}" -eq 1 ]]; then
  info "${CM_N} customManagers, ${R_N} packageRules, $(grep -c . <<<"${TRACKED}") tracked files"
  # Globs are data here: no pathname expansion while lists are word-split.
  set -f
  OLD_IFS="${IFS}"
  IFS="${NL}"
  for ((i = 0; i < CM_N; i++)); do
    label=$(cm_label "${i}")
    files=""
    stale=""
    for line in ${CM_PAT[i]:-}; do
      re="${line%%"${TAB}"*}"
      pat="${line#*"${TAB}"}"
      [[ "${re}" == "-" ]] && continue
      rc=0
      hits=$(grep -E -- "${re}" <<<"${TRACKED}" 2>/dev/null) || rc=$?
      if [[ "${rc}" -eq 2 ]]; then
        info "${label}: pattern ${pat} is not a POSIX ERE the audit can evaluate — check it by hand"
      elif [[ -z "${hits}" ]]; then
        stale="${stale}${pat}${NL}"
      else
        files="${files}${hits}${NL}"
      fi
    done
    files=$(sort -u <<<"${files}" | grep . || true)
    CM_FILES[i]="${files}"
    if [[ -z "${files}" ]]; then
      fail "${label}: managerFilePatterns match no tracked file — dead manager"
      continue
    fi
    for pat in ${stale}; do
      fail "${label}: managerFilePatterns entry ${pat} matches no tracked file — stale pattern"
    done
    n_files=$(grep -c . <<<"${files}")
    first_rule=""
    first_why=""
    n_rules=0
    for ((j = 0; j < R_N; j++)); do
      if rule_pairs "${i}" "${j}"; then
        n_rules=$((n_rules + 1))
        if [[ -z "${first_rule}" ]]; then
          first_rule="${j}"
          first_why="${PAIRED_BY}"
        fi
      fi
    done
    if [[ -n "${first_rule}" ]]; then
      more=""
      [[ "${n_rules}" -gt 1 ]] && more=" (+$((n_rules - 1)) more rule(s))"
      pass "${label}: ${n_files} tracked file(s); paired by packageRules[${first_rule}] via ${first_why}${more}"
    else
      fail "${label}: ${n_files} tracked file(s) but no packageRules entry applies — updates land untriaged"
    fi
  done
  IFS="${OLD_IFS}"
  set +f
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
# A test selects a manager by its file pattern (select(any(.managerFilePatterns[];
# test("images/nova/Dockerfile")))), its package or dependency name
# (select(.packageNameTemplate == $pkg)), or the variable its regex anchors on
# (RENOVATE_VALIDATOR_VERSION). Those are the needles. A needle that more than
# half of the tests mention ("renovate", the renovate.json path) carries no
# signal and is dropped. A test covers a manager when the needles it mentions
# are not all needles of one other manager: "hack/install-test-deps.sh" alone
# fits four managers, "hack/install-test-deps.sh" + "KIND_VERSION" only one.
if [[ ! -d tests/unit/renovate ]]; then
  fail "missing tests/unit/renovate/ directory"
elif [[ "${have_jq}" -ne 1 ]]; then
  info "skipping R6: jq not available"
else
  TESTS=$(find tests/unit/renovate -name '*_test.sh' | sort)
  T_BODY=()
  T_NAME=()
  T_N=0
  while IFS= read -r t; do
    [[ -z "${t}" ]] && continue
    T_NAME[T_N]="${t}"
    T_BODY[T_N]=$(cat "${t}")
    T_N=$((T_N + 1))
  done <<< "${TESTS}"
  info "customManagers: ${CM_N}; tests under tests/unit/renovate: ${T_N}"

  # needles_of <i> — candidate needles, one per line: each file pattern as a
  # path (its longest literal run when it carries regex syntax), the package and
  # dependency names, and the identifiers of its matchStrings that read as
  # variable names (an underscore or a camelCase hump, six characters or more).
  needles_of() {
    local i="$1"
    {
      printf '%s' "${CM_PAT[i]:-}" | cut -f2 \
        | sed -E 's|^/||; s|/i?$||; s|^\^||; s|\$$||; s|\\\.|.|g' \
        | awk '{
            n = split($0, parts, /\.\*|\.\+|[][()|?+*^$\\]/); best = ""
            for (k = 1; k <= n; k++) if (length(parts[k]) > length(best)) best = parts[k]
            if (length(best) >= 6) print best
          }'
      [[ -n "${CM_PKG[i]:-}" ]] && echo "${CM_PKG[i]}"
      [[ -n "${CM_DEP[i]:-}" ]] && echo "${CM_DEP[i]}"
      printf '%s' "${CM_MATCH[i]:-}" \
        | sed -E 's/\(\?<[A-Za-z]+>//g' \
        | grep -oE '[A-Za-z_][A-Za-z0-9_]{5,}' \
        | grep -E '_|[a-z][A-Z]' || true
    } | grep . | sort -u || true
  }

  # Drop needles that more than half of the tests mention.
  CM_NEEDLES=()
  for ((i = 0; i < CM_N; i++)); do
    kept=""
    while IFS= read -r needle; do
      [[ -z "${needle}" ]] && continue
      seen=0
      for ((k = 0; k < T_N; k++)); do
        [[ "${T_BODY[k]}" == *"${needle}"* ]] && seen=$((seen + 1))
      done
      if [[ $((seen * 2)) -le "${T_N}" ]]; then
        kept="${kept}${needle}${NL}"
      fi
    done <<< "$(needles_of "${i}")"
    CM_NEEDLES[i]="${kept}"
  done

  set -f
  OLD_IFS="${IFS}"
  IFS="${NL}"
  for ((i = 0; i < CM_N; i++)); do
    label=$(cm_label "${i}")
    covering=""
    for ((k = 0; k < T_N; k++)); do
      found=""
      n_found=0
      for needle in ${CM_NEEDLES[i]}; do
        if [[ "${T_BODY[k]}" == *"${needle}"* ]]; then
          found="${found}${needle}${NL}"
          n_found=$((n_found + 1))
        fi
      done
      [[ "${n_found}" -eq 0 ]] && continue
      # Not covering when one other manager owns every needle found.
      shadowed=0
      for ((o = 0; o < CM_N; o++)); do
        [[ "${o}" -eq "${i}" ]] && continue
        all=1
        for needle in ${found}; do
          case "${NL}${CM_NEEDLES[o]}" in
            *"${NL}${needle}${NL}"*) ;;
            *) all=0; break ;;
          esac
        done
        if [[ "${all}" -eq 1 ]]; then
          shadowed=1
          break
        fi
      done
      [[ "${shadowed}" -eq 1 ]] && continue
      covering="${covering:+${covering}, }${T_NAME[k]##*/}"
    done
    if [[ -n "${covering}" ]]; then
      pass "${label}: covered by ${covering}"
    else
      needles=$(printf '%s' "${CM_NEEDLES[i]}" | tr '\n' ',' | sed 's/,$//')
      fail "${label}: no test under tests/unit/renovate/ singles it out (needles: ${needles:-none})"
    fi
  done
  IFS="${OLD_IFS}"
  set +f
fi

# ---------------------------------------------------------------------------
# R7 — Makefile ↔ ci.yaml tool-pin lockstep
# ---------------------------------------------------------------------------
hdr "R7: duplicated <NAME>_VERSION pins in Makefile and ci.yaml agree"
CI_YAML=".github/workflows/ci.yaml"
# Makefile pins: NAME ?= value
mk_pins=$(sed -nE 's/^([A-Z][A-Z0-9_]*_VERSION)[[:space:]]*\?=[[:space:]]*([^#[:space:]]+).*/\1=\2/p' Makefile | sort -u || true)
# Workflow pins: NAME: value (env blocks at any indentation; run-block shell
# assignments deliberately excluded — those are step-local, not shared pins).
ci_pins=$(sed -nE 's/^[[:space:]]+([A-Z][A-Z0-9_]*_VERSION):[[:space:]]*"?([^"#[:space:]]+)"?.*/\1=\2/p' "${CI_YAML}" | sort -u || true)
info "Makefile pins: $(echo "${mk_pins}" | tr '\n' ' ')"
info "ci.yaml pins: $(echo "${ci_pins}" | tr '\n' ' ')"
while IFS= read -r mk; do
  [[ -z "${mk}" ]] && continue
  name="${mk%%=*}"
  mk_val="${mk#*=}"
  ci_vals=$(echo "${ci_pins}" | sed -nE "s/^${name}=(.*)$/\1/p" | sort -u)
  if [[ -z "${ci_vals}" ]]; then
    info "${name} pinned only in Makefile (${mk_val}) — single-sourced or resolved from PATH in CI; confirm ci.yaml derives it (e.g. the ENVTEST_K8S_VERSION awk read)"
    continue
  fi
  n_vals=$(echo "${ci_vals}" | grep -c '.' || true)
  if [[ "${n_vals}" -gt 1 ]]; then
    fail "${name} has ${n_vals} distinct values inside ${CI_YAML}: $(echo "${ci_vals}" | tr '\n' ' ')"
    continue
  fi
  if [[ "${mk_val}" == "${ci_vals}" ]]; then
    pass "${name} in lockstep: Makefile ${mk_val} == ci.yaml ${ci_vals}"
  else
    fail "${name} drifted: Makefile ${mk_val} != ci.yaml ${ci_vals} — local dev and CI run different tool versions"
  fi
done <<< "${mk_pins}"
# Workflow-only pins are pins too — surface them for the coverage review.
while IFS= read -r ci; do
  [[ -z "${ci}" ]] && continue
  name="${ci%%=*}"
  if ! grep -q "^${name}=" <<<"${mk_pins}"; then
    info "${name} pinned only in ci.yaml (${ci#*=}) — no Makefile counterpart to drift against"
  fi
done <<< "${ci_pins}"

# ---------------------------------------------------------------------------
# Inventory — Makefile and workflow tool pins (MEDIUM candidates when untracked)
# ---------------------------------------------------------------------------
hdr "Inventory — Makefile and workflow tool pins (review aid)"
tool_pins=$( { grep -nE '^[A-Z][A-Z0-9_]*_VERSION[[:space:]]*\?=' Makefile 2>/dev/null | sed 's/^/Makefile:/';
  grep -rEn '^[[:space:]]*[A-Z][A-Z0-9_]*_VERSION:[[:space:]]*' .github/workflows/ 2>/dev/null; } || true)
while IFS= read -r p; do
  [[ -z "${p}" ]] && continue
  pin_file="${p%%:*}"
  rest="${p#*:}"
  pin_line="${rest%%:*}"
  pin_text=$(sed -E 's/^[[:space:]]+//' <<<"${rest#*:}")
  pin_name=$(sed -nE 's/^([A-Z][A-Z0-9_]*_VERSION).*/\1/p' <<<"${pin_text}")
  # shellcheck disable=SC2016 # a literal GitHub Actions "${{", not an expansion
  case "${pin_text}" in
    *'${{'*)
      info "${pin_file}:${pin_line} ${pin_text} — resolved at run time, not a pin"
      continue ;;
  esac
  owner=""
  for ((i = 0; i < CM_N; i++)); do
    case "${NL}${CM_FILES[i]:-}${NL}" in *"${NL}${pin_file}${NL}"*) ;; *) continue ;; esac
    case "${CM_MATCH[i]:-}" in *"${pin_name}"*) owner=$(cm_label "${i}"); break ;; esac
  done
  if [[ -n "${owner}" ]]; then
    info "${pin_file}:${pin_line} ${pin_text} — tracked by ${owner}"
  elif [[ "${have_jq}" -eq 1 ]]; then
    info "${pin_file}:${pin_line} ${pin_text} — no customManager claims it (bumped by hand)"
  else
    info "${pin_file}:${pin_line} ${pin_text}"
  fi
done <<< "${tool_pins}"

# ---------------------------------------------------------------------------
hdr "Summary"
if [[ ${FAIL_COUNT} -eq 0 ]]; then
  echo "[PASS] no Renovate-coverage findings"
  exit 0
else
  echo "[FAIL] ${FAIL_COUNT} Renovate-coverage finding(s)"
  exit 1
fi
