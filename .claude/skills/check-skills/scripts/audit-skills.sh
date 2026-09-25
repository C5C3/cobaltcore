#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# audit-skills.sh — mechanical checks over the repository's own Claude Code
# skill suite under .claude/skills/. Verifies:
#   Q1  every skill directory has a SKILL.md that opens with YAML frontmatter,
#       whose name: equals the directory name (lowercase, digits, hyphens,
#       at most 64 characters) and whose description: is non-empty, at most
#       1,024 characters, and names its triggers ("Use when …")
#   Q2  SKILL.md stays small enough to load on every trigger (> 500 lines is
#       [FAIL]; move reference material into references/*.md)
#   Q3  every file under scripts/ is mentioned in its SKILL.md, every
#       .claude/skills/<skill>/scripts/<file> path any skill or the catalogue
#       names exists, and every references/*.md is linked from its SKILL.md
#   Q4  every script carries the SPDX header pair, parses (bash -n on the
#       macOS /bin/bash 3.2 when present, ast.parse for python), lints clean
#       under `shellcheck -S warning` (when installed), and avoids the Bash
#       4+/GNU-only constructs the suite bans and variables piped into grep -q
#   Q5  no script performs a repository, GitHub, or cluster write in command
#       position (sed -i, git add/commit/push/checkout/reset/stash/rebase,
#       gh issue/pr create/edit/comment/merge, kubectl apply/delete/patch);
#       a grep FOR such a string is fine
#   Q6  every [[skill-name]] cross-reference names an existing skill
#   Q7  every `make <target>` quoted in code in a skill exists in the Makefile
#   Q8  docs/contributing/claude-skills.md links every skill's SKILL.md and no
#       skill that does not exist
#   Q9  backticked repository paths a SKILL.md or reference names exist
#       (review aid — [INFO] only, examples and placeholders are expected)
#   Q10 staleness radar: commits that touched the files a skill names since the
#       skill itself last changed (review aid — [INFO] only)
#
# A line ending in `# audit-skills: allow` is exempt from Q4's construct scan
# and from Q5 (for a pattern definition, or a write into a mktemp dir).
#
# Read-only. Exit code 1 on any [FAIL].

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "${REPO_ROOT}"

FAIL_COUNT=0
fail() { echo "[FAIL] $*"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
pass() { echo "[PASS] $*"; }
info() { echo "[INFO] $*"; }
hdr()  { echo; echo "=== $* ==="; }

SKILLS_DIR=".claude/skills"
CATALOGUE="docs/contributing/claude-skills.md"
MAX_LINES=500
MAX_DESC=1024

SKILLS=""
for d in "${SKILLS_DIR}"/*/; do
  [[ -d "${d}" ]] || continue
  SKILLS="${SKILLS} $(basename "${d}")"
done
if [[ -z "${SKILLS// /}" ]]; then
  fail "no skill directories under ${SKILLS_DIR}/"
  exit 1
fi
info "skills:${SKILLS}"

is_skill() { # is_skill <name>
  case " ${SKILLS} " in *" $1 "*) return 0 ;; esac
  return 1
}

# frontmatter <file> — the lines between the opening --- and the next ---.
frontmatter() {
  awk 'NR == 1 && $0 != "---" { exit } NR > 1 && $0 == "---" { exit } NR > 1 { print }' "$1"
}

# fm_value <frontmatter> <key> — a scalar or folded (>-, |) value, joined with
# single spaces.
fm_value() {
  printf '%s\n' "$1" | awk -v key="$2" '
    $0 ~ "^" key ":" {
      sub("^" key ":[[:space:]]*", "")
      if ($0 ~ /^[>|]-?[[:space:]]*$/) { folded = 1; next }
      gsub(/^["'\'']|["'\'']$/, "")
      print; exit
    }
    folded && /^[A-Za-z0-9_-]+:/ { exit }
    folded { sub(/^[[:space:]]+/, ""); if (out != "") out = out " "; out = out $0 }
    END { if (folded) print out }
  '
}

# code_spans <file> — fenced code lines and inline `code` spans, one per line.
code_spans() {
  awk '
    /^[[:space:]]*(```|~~~)/ { fence = !fence; next }
    fence { print; next }
    {
      line = $0
      while (match(line, /`[^`]+`/)) {
        print substr(line, RSTART + 1, RLENGTH - 2)
        line = substr(line, RSTART + RLENGTH)
      }
    }
  ' "$1"
}

# ---------------------------------------------------------------------------
# Q1 — frontmatter
# ---------------------------------------------------------------------------
hdr "Q1: SKILL.md frontmatter (name = directory, description with triggers)"
for s in ${SKILLS}; do
  f="${SKILLS_DIR}/${s}/SKILL.md"
  if [[ ! -f "${f}" ]]; then
    fail "${s}: no SKILL.md"
    continue
  fi
  if [[ "$(head -1 "${f}")" != "---" ]]; then
    fail "${f}:1: does not open with YAML frontmatter (the suite starts SKILL.md directly with ---, no header comment)"
    continue
  fi
  fm="$(frontmatter "${f}")"
  name="$(fm_value "${fm}" name)"
  desc="$(fm_value "${fm}" description)"
  ok=1
  if [[ "${name}" != "${s}" ]]; then
    fail "${f}: name '${name}' does not match the directory '${s}'"
    ok=0
  fi
  if ! grep -qE '^[a-z0-9-]{1,64}$' <<<"${name}"; then
    fail "${f}: name '${name}' must be 1–64 lowercase letters, digits, or hyphens"
    ok=0
  fi
  if [[ -z "${desc}" ]]; then
    fail "${f}: empty description — it is the only text Claude matches a prompt against"
    ok=0
  else
    len="$(printf '%s' "${desc}" | wc -m | tr -d ' ')"
    if [[ "${len}" -gt "${MAX_DESC}" ]]; then
      fail "${f}: description is ${len} characters (limit ${MAX_DESC})"
      ok=0
    fi
    if ! grep -qiE 'use (it )?when|use this when|use for' <<<"${desc}"; then
      fail "${f}: description names no trigger ('Use when …')"
      ok=0
    fi
  fi
  [[ "${ok}" -eq 1 ]] && pass "${s}: frontmatter ok (description ${len:-0} chars)"
done

# ---------------------------------------------------------------------------
# Q2 — SKILL.md size
# ---------------------------------------------------------------------------
hdr "Q2: SKILL.md at most ${MAX_LINES} lines (details belong in references/)"
for s in ${SKILLS}; do
  f="${SKILLS_DIR}/${s}/SKILL.md"
  [[ -f "${f}" ]] || continue
  n="$(wc -l < "${f}" | tr -d ' ')"
  if [[ "${n}" -gt "${MAX_LINES}" ]]; then
    fail "${f}: ${n} lines — move reference sections into ${SKILLS_DIR}/${s}/references/ and link them"
  else
    pass "${s}: ${n} lines"
  fi
done

# ---------------------------------------------------------------------------
# Q3 — scripts and references are wired to their SKILL.md
# ---------------------------------------------------------------------------
hdr "Q3: scripts/ and references/ are referenced; referenced scripts exist"
for s in ${SKILLS}; do
  f="${SKILLS_DIR}/${s}/SKILL.md"
  [[ -f "${f}" ]] || continue
  for sc in "${SKILLS_DIR}/${s}"/scripts/*; do
    [[ -f "${sc}" ]] || continue
    base="$(basename "${sc}")"
    # A helper may be reached through the audit script rather than SKILL.md.
    if grep -qF "${base}" "${f}" \
      || grep -qF "${base}" "${SKILLS_DIR}/${s}"/scripts/*.sh 2>/dev/null \
      || grep -qF "${base}" "${SKILLS_DIR}/${s}"/references/*.md 2>/dev/null; then
      :
    else
      fail "${sc}: never mentioned by ${f}, a sibling script, or a reference — dead file?"
    fi
  done
  for ref in "${SKILLS_DIR}/${s}"/references/*.md; do
    [[ -f "${ref}" ]] || continue
    if ! grep -qF "references/$(basename "${ref}")" "${f}"; then
      fail "${ref}: not linked from ${f} — Claude only reads a reference the SKILL.md points to"
    fi
  done
done
missing_refs=0
while IFS= read -r hit; do
  [[ -z "${hit}" ]] && continue
  file="${hit%%:*}"
  rest="${hit#*:}"
  line="${rest%%:*}"
  path="${rest#*:}"
  if [[ ! -f "${path}" ]]; then
    fail "${file}:${line}: names ${path}, which does not exist"
    missing_refs=$((missing_refs + 1))
  fi
done < <(grep -rnoE '\.claude/skills/[a-z0-9-]+/scripts/[A-Za-z0-9_.-]+' \
  "${SKILLS_DIR}" "${CATALOGUE}" --include='*.md' 2>/dev/null | sort -u || true)
[[ "${missing_refs}" -eq 0 ]] && pass "every named skill script exists"

# ---------------------------------------------------------------------------
# Q4 — script hygiene: SPDX, syntax, shellcheck, portability
# ---------------------------------------------------------------------------
hdr "Q4: scripts carry SPDX, parse, pass shellcheck, and stay Bash 3.2/BSD portable"
BASH32=""
if [[ -x /bin/bash ]] && /bin/bash -c '[[ ${BASH_VERSINFO[0]} -lt 4 ]]' 2>/dev/null; then
  BASH32=/bin/bash
  info "syntax-checking with ${BASH32} ($(/bin/bash -c 'echo ${BASH_VERSION}'))"
fi
HAVE_SHELLCHECK=0
if command -v shellcheck > /dev/null 2>&1; then
  HAVE_SHELLCHECK=1
else
  info "shellcheck not on PATH — the lint half of Q4 is skipped"
fi
# Constructs the suite bans (docs/contributing/claude-skills.md § Authoring):
# Bash 4+ builtins and expansions, PCRE grep, GNU-only sed/awk forms.
BANNED='(^|[^A-Za-z_])(mapfile|readarray)([^A-Za-z_]|$)|declare -A|local -A|\$\{[A-Za-z_]+(,,|\^\^)\}|grep -[A-Za-z]*P|sed -r |match\([^,]+,[^,]+,[[:space:]]*[A-Za-z_]+\)' # audit-skills: allow
PIPED_GREP_Q='(printf|echo)[^|]*"[$][{]?[A-Za-z_0-9@*]+[}]?"[^|]*[|][[:space:]]*grep[[:space:]]+(-[A-Za-z]*q|-[A-Za-z]+[[:space:]]+-q)' # audit-skills: allow
script_count=0
for sc in "${SKILLS_DIR}"/*/scripts/*; do
  [[ -f "${sc}" ]] || continue
  case "${sc}" in *.sh|*.py) ;; *) continue ;; esac
  script_count=$((script_count + 1))
  bad=0
  head_buf="$(head -6 "${sc}")"
  if ! grep -q 'SPDX-FileCopyrightText' <<<"${head_buf}" \
    || ! grep -q 'SPDX-License-Identifier' <<<"${head_buf}"; then
    fail "${sc}: missing the SPDX-FileCopyrightText / SPDX-License-Identifier pair"
    bad=1
  fi
  case "${sc}" in
    *.sh)
      if ! bash -n "${sc}" 2>/dev/null; then
        fail "${sc}: bash -n reports a syntax error"
        bad=1
      elif [[ -n "${BASH32}" ]] && ! "${BASH32}" -n "${sc}" 2>/dev/null; then
        fail "${sc}: does not parse under ${BASH32} (Bash 3.2)"
        bad=1
      fi
      if [[ "${HAVE_SHELLCHECK}" -eq 1 ]] && ! shellcheck -S warning "${sc}" > /dev/null 2>&1; then
        fail "${sc}: shellcheck -S warning reports findings (run it for details)"
        bad=1
      fi
      hits="$(grep -nE "${BANNED}" "${sc}" | grep -vE '^[0-9]+:[[:space:]]*#|# audit-skills: allow$' || true)"
      if [[ -n "${hits}" ]]; then
        fail "${sc}: Bash 4+/GNU-only construct(s): $(printf '%s' "${hits}" | cut -d: -f1 | tr '\n' ' ')"
        bad=1
      fi
      # The shape tests/unit/ci/grep_q_here_string_guard_test.sh rejects repo-wide:
      # under pipefail, grep -q exiting early can SIGPIPE the writer and fail a
      # matching pipeline. Feed grep -q from a here-string instead.
      hits="$(grep -nE "${PIPED_GREP_Q}" "${sc}" | grep -vE '^[0-9]+:[[:space:]]*#|# audit-skills: allow$' || true)"
      if [[ -n "${hits}" ]]; then
        fail "${sc}: variable piped into grep -q on line(s) $(printf '%s' "${hits}" | cut -d: -f1 | tr '\n' ' ')— use grep -q PATTERN <<<\"\$var\""
        bad=1
      fi
      ;;
    *.py)
      if command -v python3 > /dev/null 2>&1 \
        && ! python3 -c 'import ast, sys; ast.parse(open(sys.argv[1]).read())' "${sc}" 2>/dev/null; then
        fail "${sc}: python3 cannot parse it"
        bad=1
      fi
      ;;
  esac
  [[ "${bad}" -eq 0 ]] && pass "${sc}"
done
info "${script_count} script(s) checked"

# ---------------------------------------------------------------------------
# Q5 — scripts stay read-only
# ---------------------------------------------------------------------------
hdr "Q5: no script writes to the repository, GitHub, or a cluster"
# Command position: start of line, or after ; & | ( then do else.
WRITES='(^|[;&|(]|then|do|else)[[:space:]]*(sed -i|git (add|commit|push|checkout|switch|reset|stash|rebase|tag)([[:space:]]|$)|gh (issue|pr) (create|edit|comment|merge|close)|gh run rerun|kubectl (apply|delete|patch|create|replace)([[:space:]]|$))' # audit-skills: allow
writers=0
for sc in "${SKILLS_DIR}"/*/scripts/*; do
  [[ -f "${sc}" ]] || continue
  hits="$(grep -nE "${WRITES}" "${sc}" | grep -vE '^[0-9]+:[[:space:]]*#|# audit-skills: allow$' || true)"
  if [[ -n "${hits}" ]]; then
    fail "${sc}: write operation(s) on line(s) $(printf '%s' "${hits}" | cut -d: -f1 | tr '\n' ' ')— skill scripts are read-only"
    writers=$((writers + 1))
  fi
done
[[ "${writers}" -eq 0 ]] && pass "no script carries a write operation"

# ---------------------------------------------------------------------------
# Q6 — [[skill]] cross-references resolve
# ---------------------------------------------------------------------------
hdr "Q6: every [[skill-name]] cross-reference names an existing skill"
dangling=0
while IFS= read -r hit; do
  [[ -z "${hit}" ]] && continue
  file="${hit%%:*}"
  rest="${hit#*:}"
  line="${rest%%:*}"
  ref="${rest#*:}"
  ref="${ref#[[}"
  ref="${ref%]]}"
  if ! is_skill "${ref}"; then
    fail "${file}:${line}: [[${ref}]] names no skill under ${SKILLS_DIR}/"
    dangling=$((dangling + 1))
  fi
done < <(for f in "${SKILLS_DIR}"/*/SKILL.md "${SKILLS_DIR}"/*/references/*.md; do
  [[ -f "${f}" ]] || continue
  # Outside fenced blocks and inline code: `[[skill-name]]` in backticks is
  # the syntax being described, not a reference.
  awk -v file="${f}" '
    /^[[:space:]]*(```|~~~)/ { fence = !fence; next }
    fence { next }
    {
      line = $0
      gsub(/`[^`]*`/, "", line)
      while (match(line, /\[\[[a-z0-9:-]+\]\]/)) {
        print file ":" NR ":" substr(line, RSTART, RLENGTH)
        line = substr(line, RSTART + RLENGTH)
      }
    }
  ' "${f}"
done)
[[ "${dangling}" -eq 0 ]] && pass "all [[…]] cross-references resolve"

# ---------------------------------------------------------------------------
# Q7 — make targets quoted in code exist
# ---------------------------------------------------------------------------
hdr "Q7: every make target a skill quotes in code exists in the Makefile"
TARGETS="$(grep -hoE '^[A-Za-z0-9_.-]+:' Makefile | tr -d ':' | sort -u)"
unknown=0
quoted=0
for f in "${SKILLS_DIR}"/*/SKILL.md "${SKILLS_DIR}"/*/references/*.md; do
  [[ -f "${f}" ]] || continue
  while IFS= read -r t; do
    [[ -z "${t}" ]] && continue
    quoted=$((quoted + 1))
    if ! grep -qx "${t}" <<<"${TARGETS}"; then
      fail "${f}: quotes 'make ${t}', which the Makefile does not define"
      unknown=$((unknown + 1))
    fi
  done < <(code_spans "${f}" | grep -oE '(^|[^A-Za-z0-9_-])make [a-z][a-z0-9-]*' \
    | sed -E 's/.*make //' | sort -u)
done
[[ "${unknown}" -eq 0 ]] && pass "all ${quoted} quoted make target(s) exist"

# ---------------------------------------------------------------------------
# Q8 — the contributor catalogue lists every skill
# ---------------------------------------------------------------------------
hdr "Q8: ${CATALOGUE} catalogues every skill"
if [[ ! -f "${CATALOGUE}" ]]; then
  fail "${CATALOGUE} missing"
else
  gaps=0
  for s in ${SKILLS}; do
    if ! grep -qF ".claude/skills/${s}/SKILL.md" "${CATALOGUE}"; then
      fail "${CATALOGUE}: no catalogue entry links .claude/skills/${s}/SKILL.md"
      gaps=$((gaps + 1))
    fi
  done
  while IFS= read -r listed; do
    [[ -z "${listed}" ]] && continue
    if ! is_skill "${listed}"; then
      fail "${CATALOGUE}: links .claude/skills/${listed}/SKILL.md, which does not exist"
      gaps=$((gaps + 1))
    fi
  done < <(grep -oE '\.claude/skills/[a-z0-9-]+/SKILL\.md' "${CATALOGUE}" \
    | sed -E 's|\.claude/skills/([a-z0-9-]+)/SKILL\.md|\1|' | sort -u)
  [[ "${gaps}" -eq 0 ]] && pass "catalogue and skill directories agree"
fi

# ---------------------------------------------------------------------------
# Q9 — repository paths named in the skills (review aid)
# ---------------------------------------------------------------------------
hdr "Q9: backticked repository paths resolve (review aid, no pass/fail)"
unresolved=0
for f in "${SKILLS_DIR}"/*/SKILL.md "${SKILLS_DIR}"/*/references/*.md; do
  [[ -f "${f}" ]] || continue
  while IFS= read -r p; do
    [[ -z "${p}" ]] && continue
    q="${p%/}"
    case "${q}" in *'<'*|*'>'*|*'*'*|*'{'*|*'…'*|*'...'*) continue ;; esac
    if ! ls -d "${q}" > /dev/null 2>&1; then
      info "${f}: names ${p}, which does not exist at HEAD (stale, or an example?)"
      unresolved=$((unresolved + 1))
    fi
  done < <(code_spans "${f}" \
    | grep -oE '^(operators|internal|tests|hack|deploy|docs|releases|images|scripts|patches|overrides|\.github)/[^[:space:]`:]*$' \
    | sort -u)
done
info "${unresolved} unresolved path mention(s)"

# ---------------------------------------------------------------------------
# Q10 — staleness radar (review aid)
# ---------------------------------------------------------------------------
# A skill describes files; when those files keep changing and the skill does
# not, its tables and gotchas rot. For each committed skill, count the commits
# since the skill's own last commit that touched the files it names.
hdr "Q10: staleness radar — commits to named files since the skill last changed"
if git rev-parse --git-dir > /dev/null 2>&1; then
  for s in ${SKILLS}; do
    last="$(git log -1 --format=%H -- "${SKILLS_DIR}/${s}" 2>/dev/null || true)"
    if [[ -z "${last}" ]]; then
      info "${s}: not committed yet — no baseline"
      continue
    fi
    paths=""
    for f in "${SKILLS_DIR}/${s}/SKILL.md" "${SKILLS_DIR}/${s}"/references/*.md; do
      [[ -f "${f}" ]] || continue
      paths="${paths}$(code_spans "${f}" \
        | grep -oE '^(operators|internal|tests|hack|deploy|docs|releases|images|scripts|patches|overrides|\.github)/[^[:space:]`:<>*{}]*$' \
        || true)"$'\n'
    done
    set --
    while IFS= read -r p; do
      [[ -n "${p}" && -e "${p}" ]] && set -- "$@" "${p}"
    done < <(printf '%s' "${paths}" | sort -u)
    if [[ $# -eq 0 ]]; then
      info "${s}: names no existing repository path"
      continue
    fi
    n="$(git rev-list --count "${last}..HEAD" -- "$@" 2>/dev/null || echo 0)"
    info "${s}: ${n} commit(s) to its $# named path(s) since $(git log -1 --format=%cs "${last}")"
  done
else
  info "not a git checkout — Q10 skipped"
fi

# ---------------------------------------------------------------------------
hdr "Summary"
if [[ ${FAIL_COUNT} -eq 0 ]]; then
  echo "[PASS] no skill-suite findings"
  exit 0
else
  echo "[FAIL] ${FAIL_COUNT} skill-suite finding(s)"
  exit 1
fi
