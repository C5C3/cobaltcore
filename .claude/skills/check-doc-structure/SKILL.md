---
name: check-doc-structure
description: >-
  Audit documentation structure and navigation for the CobaltCore docs — required
  frontmatter, heading hierarchy, section order, index/nav coverage,
  link and anchor integrity, orphan pages, and duplicate or misplaced
  topics. Use when asked to check doc structure, after adding or moving
  a doc page, or when a page becomes hard to discover from the docs nav.
---

# Check documentation structure

This skill verifies that the CobaltCore documentation is **structurally sound and
navigable**: pages have the expected frontmatter, headings appear in the
right order, links resolve, anchors exist, and docs show up where readers
expect them in the site nav and index pages.

It is repeatable — run it any time a page is added, renamed, moved, or
split, and especially before publishing a docs-heavy change.

## What structure means here

Structure is the page-level and site-level scaffolding that makes docs
usable. The exact expectations depend on the doc family, but the same
classes of drift appear everywhere:

| Layer | What to check | Source of truth |
|---|---|---|
| Page metadata | frontmatter fields (`title` everywhere, `quadrant` on the reference/guide families), sidebar order | the doc family conventions under `docs/` |
| Heading hierarchy | H1/H2/H3 order, required section names, no skipped levels | the current page template for that doc type |
| Navigation | VitePress sidebar, index pages, cross-links from sibling docs | `docs/.vitepress/` and the relevant section index |
| Links and anchors | relative links, fragment anchors, code-block references | the linked page and its actual heading text |
| Coverage | orphan pages, duplicate topics, stale renamed paths | the directory tree under `docs/` (including `docs/architecture/` and `docs/future/`) |
| Tutorial-family naming | when several pages walk through the same workflow at different depth/audience (e.g. a base quick start plus deeper variants), names signal the relationship and scope instead of ambiguous modifiers like "extended"; cross-references point at the specific source section instead of restating it | the full set of sibling walkthrough pages, read together, not each in isolation |

A structural finding is any page that cannot be discovered, rendered,
or read in the expected order because its scaffolding drifted.

Two gates already exist and this skill defers to them: `npm run
docs:build` (the CI `docs` job) fails on a link to a page that does not
exist, and the shell tests under `tests/unit/docs/` (run by `make
test-shell`) pin specific cross-links and quick-start coverage. Neither
checks fragment anchors, sidebar entries, heading hierarchy, or
reachability; the audit script below does.

## Depth modes

This skill should be usable at different depths without becoming a new
skill for each one:

- **quick** — one page plus its direct links and parent index.
- **standard** — one doc family, such as a guide or reference section.
- **deep** — repository-wide navigation and path consistency across all
  doc roots.

Use the same criteria at each depth; only the scope changes.

## Procedure

Work through these steps in order and report findings at the end.

### 0. Run the deterministic audit

```bash
bash .claude/skills/check-doc-structure/scripts/audit-doc-structure.sh          # T1–T6
bash .claude/skills/check-doc-structure/scripts/audit-doc-structure.sh --full   # + npm run docs:build
```

- **T1** — every page has a frontmatter block with `title:`; pages
  without `quadrant:` are listed for a family-level judgement (the
  architecture, contributing, quick-start, and landing pages carry none
  today).
- **T2** — exactly one H1 per page and no skipped heading level.
- **T3** — every docs link resolves to a page and every `#fragment`
  resolves to an anchor on the target page. Anchors are computed with
  VitePress' own slugify, which differs from GitHub's: runs of
  separators collapse to one hyphen (`Owner-ref / GC model` →
  `#owner-ref-gc-model`, not `#owner-ref--gc-model`), a slug starting
  with a digit gains an underscore (`6. Recover…` → `#_6-recover…`),
  punctuation outside the separator set survives (an em dash stays in
  the slug), and `.` is a separator (`extra-packages.yaml` →
  `#extra-packages-yaml`). A heading can pin its id with `{#custom-id}`.
- **T4** — every sidebar/nav `link:` in `docs/.vitepress/config.ts`
  resolves to a page (the build does not check these).
- **T5** — no orphan page: every page is in the sidebar/nav or linked
  from another page. Pages reachable only through links are listed as
  `[INFO]`.
- **T6** — no bare `<placeholder>` in prose. VitePress compiles every
  page as a Vue template, so `<name>` outside backticks or a fence is an
  element that never closes and fails the whole `docs:build` with
  `Element is missing end tag`. Wrap placeholders in code.

Every `[FAIL]` is a HIGH or MEDIUM finding per step 6; the script cannot
judge the steps below.

### 1. Identify the doc family

Decide whether the target is a guide, reference page, architecture
chapter, README, or generated artifact. The required sections and nav
expectations come from that family, not from a single universal template.

### 2. Check the page skeleton

Verify the page has the expected metadata and outline:

- frontmatter exists and is valid
- title matches the page purpose
- required headings appear in the expected order
- no empty or duplicated sections remain after edits
- code blocks and admonitions are not breaking the flow

### 3. Check discoverability

Confirm the page is reachable from the places readers actually use:

- sidebar or nav config
- section index pages
- sibling cross-links
- any landing page that lists the topic

### 4. Check links and anchors

Step 0's T3/T4 resolve every local link and anchor mechanically. By
hand, check that each link lands where the sentence promises (a link
that resolves to the wrong section is a finding T3 cannot see). If a
link target moved, update the source and the destination path
together.

### 5. Check tutorial/guide families as a set

When two or more pages cover the same workflow at different depth or for
a different audience (a quick start plus a deeper variant, a base guide
plus a role-specific one), read them together, not just individually:

- do the names tell a reader which one to open first, and what the
  others add — a bare modifier like "extended" doesn't say what it
  extends or why
- does the deeper page belong in this section at all, or does its real
  audience (e.g. developers) mean it should live and be named elsewhere
- when one page mentions a topic the sibling covers in depth, does it
  link to the sibling's specific section instead of re-explaining or
  silently assuming the reader already read it

### 6. Report

Produce findings as a flat list, most severe first, one line each:

`[SEVERITY] STRUCT-<n> — <source page>:<line> [+ nav/index page] —
<problem> — Fix: <one-line resolution, or "needs judgment" if it
involves a rename/move/reorg a human should confirm>`

Group by severity:

- **HIGH** — a page cannot be found through the documented nav; a link
  target is missing; the page skeleton is broken enough that the doc
  family no longer renders as intended.
- **MEDIUM** — a required section is missing or out of order; an index or
  sidebar entry is stale; a renamed page still has old inbound links; a
  tutorial family's naming or scope actively misleads about what each
  page covers.
- **LOW** — a cosmetic heading-level mismatch, duplicate wording, or a
  stale anchor that still resolves after redirects.

End with a per-doc-family verdict.

### 7. Verification guardrails

Before finalizing findings:

- Quote the exact structural defect trigger (missing heading, stale nav
  entry, broken anchor, unresolved link).
- Cite concrete `file:line` locations for source and target sides when
  applicable.
- If a target cannot be resolved from the checkout, mark the finding as
  **blocked** or **needs judgment** rather than guessing.
- Re-check every **HIGH** finding against current files before output.

A zero-finding run is valid. If navigation and structure are sound,
report a clean result.

### 8. Suppressions (do not report)

- Cosmetic heading-style preferences when hierarchy and navigation are
  still correct.
- Pure prose complaints with no structural impact (route to
  [[check-doc-expressions]]).
- "Could be organized differently" suggestions without a concrete
  discoverability or integrity failure.

## Notes

- This skill is read-only; hand findings to [[fix-docs]] to apply them.
  Renames and reorganizations from step 5 are judgment calls — flag them
  for confirmation rather than treating them as mechanical fixes.
- Pair this with [[check-doc-expressions]] for prose quality and with
  [[check-doc-consistency]] for cross-document truth alignment.
- `STYLE_GUIDE.md`'s "Keep" list (frontmatter, tables/diagrams/code
  blocks, cross-links) overlaps this skill's scope — if a prose-style
  pass under [[check-doc-expressions]] or [[fix-docs]] touched one of
  those, re-run this skill on the page.
- If a generated doc or a site template is involved, verify the generator
  or template separately before patching the rendered page.
- `docs/.vitepress/dist/` and `cache/` are build output; never audit or
  edit them.
