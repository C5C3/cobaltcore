---
name: check-skills
description: >-
  Audit the repository's own Claude Code skill suite under .claude/skills/ —
  frontmatter and trigger descriptions, SKILL.md size, scripts and references
  wired to their SKILL.md, script hygiene (SPDX header, Bash 3.2 parse,
  shellcheck, no Bash 4+/GNU-only constructs, read-only), cross-references
  between skills, quoted make targets, the catalogue in
  docs/contributing/claude-skills.md, and a staleness radar of the files each
  skill describes. Use when adding, renaming, or editing a skill or its
  script, when a skill's audit script reports findings that look like false
  positives, or periodically to find skills whose tables have rotted.
---

# Check the skill suite

This skill audits the skills themselves. Every other skill in
`.claude/skills/` describes a surface of the repository — CI matrices,
enumeration points, release wiring, known failure patterns — and those
surfaces keep moving. A skill that nobody re-reads turns into confident,
wrong guidance: an audit that passes because it still checks one service
out of eight, or reports eighty false positives until nobody trusts it.

It is repeatable — run it after any change under `.claude/skills/`, and
periodically as a staleness sweep.

## What skill health means here

| Layer | Where it lives | Source of truth |
|---|---|---|
| Discoverability | `SKILL.md` frontmatter `name` + `description` | Claude Code matches the prompt against the description alone; the body loads only after a match |
| Context cost | `SKILL.md` body, `references/*.md` | the body loads on every trigger, references only when a step points at them |
| Deterministic companion | `scripts/*.sh` (+ python helpers) | `docs/contributing/claude-skills.md` § Authoring: read-only, Bash 3.2 + BSD tools, SPDX header, `[PASS]/[FAIL]/[INFO]` protocol, exit 1 on `[FAIL]` |
| Cross-links | `[[skill-name]]` references, `make` targets, repository paths | the skill directories, the `Makefile`, the tree at HEAD |
| Catalogue | `docs/contributing/claude-skills.md` | one entry per skill directory |

## Procedure

### 1. Run the deterministic audit

```bash
bash .claude/skills/check-skills/scripts/audit-skills.sh
```

Exit code `1` means at least one `[FAIL]`. Interpret:

- **Q1** — `SKILL.md` opens with frontmatter; `name` equals the
  directory (1–64 lowercase letters, digits, hyphens); `description` is
  non-empty, at most 1,024 characters, and names its triggers
  ("Use when …"). A description without triggers is a skill Claude
  never loads on its own.
- **Q2** — `SKILL.md` has at most 500 lines. Past that, move reference
  material (tables of past cases, per-service notes, build sheets) into
  `references/*.md` and point to it from the step that needs it.
- **Q3** — every file under `scripts/` is mentioned by its `SKILL.md`,
  a sibling script, or a reference; every `.claude/skills/<x>/scripts/<y>`
  path named anywhere exists; every `references/*.md` is linked from its
  `SKILL.md` (Claude reads a reference only when told to).
- **Q4** — scripts carry the SPDX pair, parse under `bash -n` and the
  macOS `/bin/bash` 3.2, lint clean under `shellcheck -S warning`, and
  avoid `mapfile`/`readarray`, associative arrays, `${x,,}`, `grep -P`,
  `sed -r`, and gawk's three-argument `match()`, and never pipe a
  variable into `grep -q` (under `pipefail` the early exit can SIGPIPE the
  writer and fail a matching pipeline; `grep -q PATTERN <<<"$var"` is the
  repo's form, enforced by `tests/unit/ci/grep_q_here_string_guard_test.sh`).
  Python helpers must parse.
- **Q5** — no script writes in command position: `sed -i`, git
  add/commit/push/checkout/reset/stash/rebase, `gh issue|pr
  create|edit|comment|merge`, `gh run rerun`, `kubectl
  apply|delete|patch`. Grepping *for* such a string is fine. A deliberate
  exception (a pattern definition, a write into a `mktemp` directory)
  ends its line with `# audit-skills: allow`.
- **Q6** — every `[[skill-name]]` cross-reference names an existing
  skill directory.
- **Q7** — every `make <target>` a skill quotes in code (fenced blocks,
  inline spans) exists in the `Makefile`.
- **Q8** — `docs/contributing/claude-skills.md` links every skill's
  `SKILL.md` and nothing that does not exist.
- **Q9** — `[INFO]` list of backticked repository paths that do not
  exist at HEAD. Examples (`releases/2026.2/`) are expected; a renamed
  file is not.
- **Q10** — `[INFO]` staleness radar: per skill, the commits that
  touched the files it names since the skill itself last changed. A high
  count is not a finding; it tells you which skill to re-read first.

### 2. Re-read the stalest skills against HEAD

Take the skills Q10 ranks highest, plus any skill whose own audit
reported findings you suspect are false positives, and check the facts
that rot fastest:

1. **Hard-coded lists.** Operator, service, release, CI job, and Tempest
   leg lists. The repo derives most of them (`releases/<latest>/source-refs.yaml`
   keys, `OPERATORS ?=` in the `Makefile`, `ALL_TEMPEST_SERVICES` in
   `hack/ci-generate-tempest-matrix.sh`, `ls operators/*/api`); a skill
   script should read them from there instead of repeating them.
2. **Counts and versions** in prose ("the three service operators",
   "chainsaw v0.2.14"). Point at the pin instead of copying it.
3. **Run the skill's own script** and triage every `[FAIL]`: real
   finding, or a heuristic that no longer fits the repo? A known-noisy
   check teaches readers to ignore the whole audit; fix the heuristic,
   or record the deviation in the script's allowlist with its reason.
4. **Gates named in the skill** still exist and still do what the skill
   says (CI job names in `.github/workflows/ci.yaml`, `make` targets,
   test scripts).

### 3. Report

Group findings by severity:

- **HIGH** — a skill whose script silently checks a subset of what its
  description promises (a surface grew and the script did not); a script
  that writes to the tree; a broken frontmatter that keeps a skill from
  loading.
- **MEDIUM** — a script with false positives the skill does not explain;
  a stale fact in a table or procedure step; a missing catalogue entry; a
  dangling `[[…]]` or `make` reference; `SKILL.md` over 500 lines.
- **LOW** — a Q9 path that is an unmarked example; wording drift between
  the catalogue entry and the frontmatter description.

One line per finding with `file:line`. End with a per-skill verdict for
every skill you re-read in step 2.

## Authoring a new skill

1. Create `.claude/skills/<name>/SKILL.md`, starting directly with the
   frontmatter (no header comment). Write the description as "<what it
   does> — <scope>. Use when <concrete triggers>." The triggers carry the
   weight: name the symptom a user would type or the CI signal they see.
2. Put the deterministic part in `scripts/` (audits: `audit-<name>.sh`),
   following the conventions Q4/Q5 check. Derive lists from the repo;
   degrade to `[INFO] … skipped` when an optional tool (python3, jq,
   network) is missing.
3. Keep `SKILL.md` to the procedure; move long reference material into
   `references/*.md` and link each file from the step that needs it.
4. Name the authoritative gate (CI job, `make` target) and say to trust
   it over the script.
5. Add the catalogue entry to `docs/contributing/claude-skills.md`.
6. Run this audit and the new skill's own script; both should exit `0`
   at HEAD, or every remaining `[FAIL]` must be a real finding.

## Notes

- This skill is read-only; the script edits nothing. Apply fixes as a
  separate, explicitly scoped task — a skill refresh is a normal commit
  (`chore(skills): …`).
- Auditing a skill means distrusting it: verify every claim you keep or
  add against HEAD, the way the skills themselves ask of their readers.
- Pair this with [[check-doc-structure]] when editing the catalogue page,
  which is part of the VitePress site.
