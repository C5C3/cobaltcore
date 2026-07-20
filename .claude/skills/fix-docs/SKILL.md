---
name: fix-docs
description: >-
  Apply fixes for findings produced by check-doc-consistency,
  check-doc-expressions, and check-doc-structure — or run those audits
  first if no findings are supplied. Classifies each finding as
  mechanical (safe to apply directly), a judgment call (propose the
  change, confirm before applying), or needs-research (establish the
  correct fact before writing anything), then edits the docs and
  re-verifies with the originating audit. Use when asked to fix
  documentation issues, action a docs audit report, or clean up findings
  from check-doc-consistency/expressions/structure.
---

# Fix documentation findings

This skill turns a docs audit into an actual edit. The three check-doc-*
skills are deliberately read-only — this is the paired skill that closes
the loop by applying their findings, or a subset of them, to the repo.

## Input

Either:

- a findings list already produced by check-doc-consistency,
  check-doc-expressions, or check-doc-structure (the `[SEVERITY]
  <PREFIX>-<n> — <location> — <problem> — Fix: <hint>` format those
  skills emit), or
- nothing — in which case, run **all** check-doc-* skills, at
  a depth matching the requested scope, to generate findings. Default to
  all even when the request names only "the style guide" or
  "STYLE_GUIDE.md" — that file covers one narrow slice of
  check-doc-expressions. Run a subset instead of all three only when the
  user explicitly names a single check-doc-* skill or scopes the request
  to one of their concerns (e.g. "just check terminology consistency").

If given a specific severity or file scope ("just the HIGH ones", "only
the quick-start pages"), filter to that before classifying — this narrows
which findings to act on, not which check-doc-* skills produce them.

## Procedure

### 1. Classify every finding

- **Mechanical** — a stale value, dead link, wrong field/flag name,
  duplicate section, or unexplained-reference gap where the correct
  text is directly derivable from code, config, or a sibling doc. The
  finding's `Fix:` hint already states the resolution, or it's a single
  unambiguous edit. A `STYLE_GUIDE.md` budget finding from
  [[check-doc-expressions]] step 3 is also mechanical: excess em-dashes
  or italics get cut per the guide's Do example, a second antithesis on
  a page becomes a plain statement, a repeated aphorism/callout keeps
  one copy, a bare quality label ("robust", "clean") is replaced with
  the concrete fact it stood in for, retired filler vocabulary is
  reworded or (for `precisely`/`exactly`) deleted outright, and a
  meta-signposting sentence is deleted. Splitting a long sentence at
  its colon or semicolon is mechanical only if the split doesn't change
  what the sentence claims — otherwise treat it as a judgment call.
- **Judgment call** — page renames or moves, tone/marketing-language
  rewrites, tutorial-family reorganization, condensing a paragraph,
  anything the audits tagged "needs judgment." The right answer depends
  on a decision, not a lookup.
- **Needs research** — the finding's `Fix:` hint says "needs research,"
  or the correct value isn't yet established (e.g. a version pin's
  justification, a minimum tool version nobody wrote down). Investigate
  first — grep `hack/`, `Makefile`, CI workflows, commit history, or the
  relevant script — until the fact is pinned down, then treat it as
  mechanical.

### 2. Confirm before applying judgment calls

Present the proposed change (old vs. new wording, or the rename/move
plan including every inbound link and nav entry that would need to
update) and get explicit confirmation before editing. Renames ripple
through sidebar config and cross-links repo-wide — treat them as a
reversible-but-noisy action worth a quick check-in, not a silent edit.

### 3. Apply

- Apply mechanical fixes and confirmed judgment calls with direct edits.
- When a page is renamed or moved, update in the same change: every
  inbound link, the VitePress sidebar/nav config, and any section index
  that lists it. A rename that doesn't update its own inbound links just
  trades one structural finding for another.
- When condensing or rewording prose, preserve any warnings, exceptions,
  or version-specific caveats the original sentence carried — cutting
  marketing fluff should not also cut the caveat sitting next to it.
- When applying a `STYLE_GUIDE.md` fix, leave frontmatter, tables,
  diagrams, code blocks, and cross-links untouched — the guide's "Keep"
  list protects them from style sanding even when the surrounding prose
  is being cut down.

### 4. Re-verify

Re-run the check-doc-* skill(s) that produced the fixed findings, scoped
to just the touched pages ("quick" depth), to confirm the fix didn't:

- introduce a new dead link or anchor (structure)
- leave a sibling page's copy of the same fact now out of sync
  (consistency)
- swap one wording problem for another (expressions)

For any touched page, also run `STYLE_GUIDE.md`'s own pre-commit check
(em-dash/italic counts back under budget, no second antithesis, no
unbacked quality label, no sentence too long to read in one breath, no
leftover meta-signposting) — this is the same check as
[[check-doc-expressions]] step 3, so a quick-depth re-run of that skill
covers it.

### 5. Report

List what was fixed, what was skipped and why (deferred pending user
decision, or still needs research), grouped the same way the input
findings were grouped. Reference the original finding ID
(`CONS-3`, `EXPR-1`, `STRUCT-2`, …) next to its outcome.

## Notes

- Unlike its three siblings, this skill writes to the repo — treat file
  renames/moves as the one action class that always needs confirmation,
  even under an otherwise-autonomous invocation.
- Don't invent a fix for a needs-research finding — establish the fact
  first, and if it truly isn't derivable from the repo, say so instead
  of guessing at a plausible-sounding value.
- Pair with [[check-doc-consistency]], [[check-doc-expressions]], and
  [[check-doc-structure]], which generate the findings this skill
  consumes.
- For any prose rewrite, `STYLE_GUIDE.md` is the contract: its Do/Don't
  pairs are the default rewrite pattern for a given device, and its
  "Keep" list is the boundary this skill must not cross.
