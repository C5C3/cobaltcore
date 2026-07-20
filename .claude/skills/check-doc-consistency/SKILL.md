---
name: check-doc-consistency
description: >-
  Audit documentation consistency across forge docs — terminology,
  naming, release/version references, repeated examples, cross-page
  claims, and alignment with code or configuration when docs describe
  implementation details. Use when asked to check for contradictions,
  after changing shared terminology, or when one doc seems to disagree
  with another.
---

# Check documentation consistency

This skill verifies that forge documentation **agrees with itself and
with the source of truth it describes**. It is the cross-document and
cross-layer check: the same concept should keep the same name, version,
shape, and meaning everywhere it appears.

It is repeatable — run it after edits that touch shared terms, repeated
examples, release references, or any doc section that states facts about
code, config, or deployment behavior.

## What consistency means here

Consistency is about preventing the docs from contradicting themselves or
the implementation. This is broader than structure and sharper than
style.

| Area | What to check | Source of truth |
|---|---|---|
| Terminology | same concept uses the same name and capitalization everywhere | the established repo vocabulary |
| Version and release references | release tags, versions, and defaults agree across pages | `releases/`, `deploy/`, `hack/`, and other live references |
| Repeated examples | duplicated command snippets and YAML blocks do not drift | the canonical workflow or config |
| Cross-page claims | overview, guide, and reference pages tell the same story | the underlying code or process |
| Code-grounded statements | docs that describe behavior match the implementation | the relevant code, config, or generated artifact |
| Environment/tooling claims | stated CLI/tool version requirements (or the absence of one) match what scripts actually enforce or require | `Makefile`, `hack/`, `scripts/`, CI workflow tool-version pins |
| Terms defined once, used elsewhere | a term, daemon, or API named on one page is only meaningfully explained on a *different* page, with no link between them | the page the term actually appears on, checked against its own content |

A consistency finding is any place where two docs, or a doc and the code,
make different claims about the same thing.

## Depth modes

Use depth as a mode, not a separate skill:

- **quick** — compare one page against its nearest sibling or source.
- **standard** — compare one doc family, such as all pages in a topic.
- **deep** — compare the broader docs corpus against the current repo
  state for release, terminology, and implementation drift.

## Procedure

Work through these steps in order and report findings at the end.

### 1. Identify the canonical claim

For each fact in the docs, decide where the truth lives:

- code
- generated config or schema
- release metadata
- another doc page that acts as the reference

If the source is unclear, the doc is already at risk of drift.

### 2. Compare the repeated surfaces

Check the places where the same fact is likely repeated:

- overview pages versus detailed guides
- user docs versus reference docs
- examples versus prose descriptions
- repo docs versus deployment or release files

### 3. Check for contradictions and stale copies

Look for:

- renamed terms that only changed in one place
- version numbers that no longer match current defaults
- copied examples that kept old values
- pages that still describe removed behavior
- claims about code paths that no longer exist
- a tool/version requirement stated (or silently assumed) that doesn't
  match what the scripts the reader is about to run actually need
- a term or reference a page uses that is only explained on a sibling
  page — the writer knew the definition existed somewhere, but the
  reader on this page doesn't have it

### 4. Report

Produce findings as a flat list, most severe first, one line each:

`[SEVERITY] CONS-<n> — <file A>:<line> vs <file B or code path>:<line> —
<the mismatch> — Fix: <one-line resolution, or "needs research" if the
correct value isn't established yet>`

Group by severity:

- **HIGH** — a doc contradicts the implementation or a sibling doc on a
  load-bearing fact such as a version, default, required step, or
  supported behavior.
- **MEDIUM** — terminology drift, stale copied examples, a doc that
  partially matches reality but omits a conflicting detail, or a tool
  requirement that's wrong rather than merely unstated.
- **LOW** — minor wording differences, old but harmless examples, or
  duplicated phrasing that could mislead later edits.

End with a verdict for the doc family or topic.

## Notes

- This skill is read-only; hand findings to [[fix-docs]] to apply them.
- Pair this with [[check-doc-structure]] for page-level navigation and
  with [[check-doc-expressions]] for readability and the
  `STYLE_GUIDE.md` rhetorical-device budget — style drift is out of
  scope here even when it happens to repeat across pages; that's a
  consistency-shaped symptom of a style problem, not a truth mismatch.
- If the disagreement is with code, config, or generated output, cite the
  code-side source of truth directly rather than treating the doc as the
  canonical side.
- If the correct value isn't derivable from anything in the repo (e.g.
  "why is this the default" has no comment or commit explaining it),
  report it as **needs research** rather than guessing — that is itself
  a finding worth surfacing.
