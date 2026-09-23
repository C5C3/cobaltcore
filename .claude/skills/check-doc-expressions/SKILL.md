---
name: check-doc-expressions
description: >-
  Audit documentation prose quality for the CobaltCore docs — sentence clarity,
  active voice, terminology, ambiguity, tone, jargon control, adherence
  to STYLE_GUIDE.md's rhetorical-device budget (em-dashes, italics,
  antithesis, callouts, aphorisms, filler words, counted per page by a
  script), and the readability of command examples and code-adjacent
  explanations. Use when asked to improve writing quality, after drafting
  or editing prose, when a page reads correctly but not clearly, or to
  find the pages furthest over the style budget.
---

# Check documentation expressions

This skill verifies that the CobaltCore documentation **reads cleanly and says
exactly what it means**. It focuses on prose quality rather than page
shape: sentences should be direct, terminology should be consistent,
examples should be readable, and important claims should not be buried
in vague language.

It is repeatable — run it any time text changes materially, especially in
user-facing guides, tutorials, and reference prose.

## What expressions means here

Expression quality is the writing layer that sits on top of structure.
The content can be technically correct and still be hard to use if the
prose is muddy or inconsistent.

| Area | What to check | Source of truth |
|---|---|---|
| Sentence clarity | short, direct sentences; one idea per sentence; no dangling references | the intended reader outcome |
| Voice and tone | active voice, concrete verbs, minimal hedging; no marketing or aspirational phrasing ("the shortest path to…", "seamlessly") in operational docs | `STYLE_GUIDE.md` and the doc family style used elsewhere in the repo |
| Rhetorical-device budget | em-dashes and italic emphasis stay within their per-1,000-word limits, antithesis ("not X, but Y"), aphoristic one-liner closers, and `:::` callouts within their per-page limits; filler vocabulary, quality self-labels, and meta-signposting are cut | `STYLE_GUIDE.md`'s budget table and Do/Don't list |
| Terminology | one term per concept, no accidental synonyms | established usage across `docs/`, anchored on the CRD field, condition, and resource names the `docs/reference/` pages define |
| Command examples | commands are complete, ordered, and copyable | the real workflow the docs describe |
| Explanatory text | definitions appear before specialized terms are used | the implementation or process being documented |
| Unexplained references | named tools, daemons, helper processes, or APIs (e.g. a background service invoked indirectly, an internal API) are explained or linked on first use **on the page they appear on** — a definition living only in a different page does not count | the reader landing on this specific page, not the whole corpus |
| Prerequisite accuracy | stated tool/version requirements match what the workflow actually needs (minimum versions, why a tool is required, what it's required *for*) | the scripts/Makefile/CI steps the doc walks the reader through |
| Operational realism | the page covers what to do when a step fails, times out, or needs a retry — not only the happy path; environment assumptions (network, resources) that can make a step fail are stated as prerequisites | known failure reports and support history for that workflow |
| Depth parity | every step/section of one walkthrough gets a comparable level of explanation — no step is a bare command while its siblings get paragraphs of context | the other steps on the same page |
| Readability | long paragraphs, repetition, and filler are removed | the page's purpose and audience |

A writing finding is any place where the prose forces the reader to infer
meaning that the document should have stated directly.

## Depth modes

Use the same skill at multiple depths rather than splitting it into more
skills:

- **quick** — one page, line-level wording cleanup.
- **standard** — one document set, such as a guide and its reference
  neighbors.
- **deep** — repo-wide terminology and phrase consistency.

## Procedure

Work through these steps in order and report findings at the end.

### 1. Identify the audience

Decide whether the page is for a new contributor, an operator, a release
owner, or a reader looking up a fact. The same sentence can be clear for
one audience and opaque for another.

### 2. Read for meaning, not grammar alone

Check whether each paragraph does the following:

- states the point up front
- uses the same term for the same concept
- avoids unexplained acronyms, jargon, or named tools/daemons/APIs that
  this page never defines (checking the page itself, not whether some
  other page defines it)
- makes examples match the surrounding text
- keeps warnings and exceptions easy to spot
- gives each step of a multi-step walkthrough comparable depth — flag a
  step that is a bare command next to siblings with full explanations

### 3. Check the style-guide budget

`STYLE_GUIDE.md` sets checkable limits: at most 2 em-dashes and 4
italic spans per ~1,000 words, and per page at most 1 antithesis
("not X, but Y" / "rather than"), 1 aphoristic one-liner close, and
1–2 `:::` callout boxes. Filler vocabulary and quality self-labels are
retired outright. Count first, then judge:

```bash
bash .claude/skills/check-doc-expressions/scripts/count-style-budget.sh [<page.md|dir>...]
bash .claude/skills/check-doc-expressions/scripts/count-style-budget.sh --page <page.md>
```

Without arguments it reads every `docs/**/*.md` outside
`docs/.vitepress/` plus `README.md`. It strips frontmatter, fenced code,
HTML comments and tags, inline code, link and image URLs, code imports,
container markers, and dash-only table cells, then prints one line per
page: `[OVER]` or `[PASS]`, the word count, each device's count (em and
ital also per 1,000 words), and a `!` on every device over its
allowance. A top-10 summary by excess follows (`--top N` changes it).
`--page` lists every hit as `file:line: device: snippet`, the form
[[fix-docs]] edits from. The exit code is 0 unless `--strict` is given
and a page is over budget. The helper `count_style_budget.py` beside
the script does the parsing; without `python3` the count is skipped.

Then judge each hit:

- **em, ital** are exact counts. Keep the one em-dash a paragraph's real
  pivot needs and italics on a term's first definition; cut the rest.
- **anti** lists candidates: "rather than", "not X, but Y", "not just
  X but Y", "doesn't just", and "X, not Y". Keep the one antithesis the
  page earns and rewrite the others as plain statements. A contrast in
  parentheses ("(not the image)") is not counted; read for it.
- **aph** lists candidates: the short declarative sentence that closes
  a paragraph of two or more sentences. Most are plain statements;
  report only real slogans ("Idempotency is the whole point."). A slogan
  that a `:::` box then repeats is not counted; read for it.
- **call**: a guide's `::: info Devstack` container is required by
  `docs/contributing/guide-conventions.md`, and the operator-owned
  `::: warning` of a ControlPlane guide comes with the
  [[prepare-new-guide]] skeleton (its V4 check wants one beside any edit
  of a projected child). Both count toward the budget but are not
  findings. `::: details` and `::: v-pre` are not counted.
- **filler** (`load-bearing`, `by construction`, `structural rather than
  aspirational`, `first-class`, `precisely`, `exactly`, `deliberately`):
  `precisely` and `exactly` are cut outright rather than replaced;
  "exactly" before a number ("exactly one of clusterRef or host") states
  a quantity and is not counted.
- **label** ("robust", "clean", "battle-tested", "seamless"): a
  self-label with nothing concrete backing it. The fix replaces the
  label with the fact it was standing in for (a linked test, a
  condition name, a command); it doesn't just delete the sentence.
  "clean up" and compounds such as `db-clean` are not counted.

Two items stay manual:

- A sentence with three or more subordinate clauses that can't be read
  aloud in one breath — the fix splits it at a colon or semicolon.
- A sentence that only previews the next one ("The section below
  covers…") — the fix deletes it; the heading already orients the
  reader.

This is a mechanical pass, separate from the meaning-level read in
step 2 — do both.

### 4. Inspect code-adjacent text

For commands, YAML, shell snippets, and API examples:

- ensure the example is complete enough to run or adapt
- check that flags, paths, and release names are not stale
- verify the prose around the example explains what changes and why
- for any stated tool/version prerequisite, check it against what the
  workflow actually requires (e.g. a command uses a flag or operator
  that only exists from some minimum version) — a requirement that is
  present but wrong is worse than one that is simply missing
- check whether the page tells the reader what to do if the step fails,
  times out, or needs a retry, and whether failure-prone environment
  assumptions (network access, resource limits) are stated up front as
  prerequisites rather than discovered by trial and error

### 5. Report

Produce findings as a flat list, most severe first, one line each:

`[SEVERITY] EXPR-<n> — <file>:<line> — <problem, quoting the offending
text> — Fix: <one-line suggested rewording, or "needs judgment" if the
right wording depends on a decision the audit can't make>`

Group by severity:

- **HIGH** — text says the opposite of what the intended behavior is;
  an example is misleading enough to cause a bad operation; a stated
  prerequisite is wrong (not just unclear) and follows it fails.
- **MEDIUM** — ambiguous wording, unexplained jargon/reference, repeated
  terms for one concept, missing failure/retry guidance, or prose that
  obscures the actual workflow; a style-guide budget blown so far past
  the limit (e.g. the machine-written claim → contrast → em-dash → tidy
  one-liner pattern repeating every paragraph) that it degrades
  readability rather than just drifting from house style.
- **LOW** — awkward phrasing, overly long sentences, depth-parity
  drift, or a step 3 style-guide budget item over its limit without
  otherwise changing the meaning.

End with a short verdict for the page or doc set.

### 6. Verification guardrails

Before finalizing findings:

- Quote the exact sentence, paragraph fragment, or command/context pair
  that triggered the finding.
- Cite concrete `file:line` locations.
- If a readability claim is subjective and cannot be grounded in a
  specific text defect, either rewrite it as a concrete defect or drop
  it.
- Re-check every **HIGH** finding to ensure the cited text still exists
  in the current checkout.

A zero-finding run is valid. If prose is clear for the selected audience
and no comprehension-impacting defects are found, report a clean result.

### 7. Suppressions (do not report)

- Pure preference nits that do not change comprehension or actionability.
- Repetition-only comments when the repeated text is intentional for
  safety-critical visibility.
- General statements like "too long" without a concrete place where
  meaning is lost.

## Notes

- `scripts/count-style-budget.sh` is a triage aid, not a gate: the
  heuristic devices (anti, aph) report candidates, and most reference
  pages are over budget today (the large ControlPlane and Keystone
  reference pages carry several times their em-dash allowance). In a
  quick or standard run, report the pages the change touched plus the
  top of the summary rather than every `[OVER]` line; a deep run can
  walk the summary.
- This skill is read-only; do not rewrite the page until the wording
  issue has been localized. Hand findings to [[fix-docs]] to apply them.
  Step 3 findings map to STYLE_GUIDE.md's own Do/Don't pairs, so
  fix-docs can usually treat them as mechanical.
- Pair this with [[check-doc-structure]] so the page is both readable
  and well organized, and with [[check-doc-consistency]] so the prose
  matches the rest of the docs corpus.
- If the issue is a factual mismatch rather than wording, hand it off to
  [[check-doc-consistency]].
