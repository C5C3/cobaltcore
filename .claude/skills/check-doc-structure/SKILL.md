---
name: check-doc-structure
description: >-
  Audit documentation structure and navigation for forge docs — required
  frontmatter, heading hierarchy, section order, index/nav coverage,
  link and anchor integrity, orphan pages, and duplicate or misplaced
  topics. Use when asked to check doc structure, after adding or moving
  a doc page, or when a page becomes hard to discover from the docs nav.
---

# Check documentation structure

This skill verifies that forge documentation is **structurally sound and
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
| Page metadata | frontmatter fields, title, sidebar order, quadrant/category markers | the doc family conventions in `docs/` and `architecture/` |
| Heading hierarchy | H1/H2/H3 order, required section names, no skipped levels | the current page template for that doc type |
| Navigation | VitePress sidebar, index pages, cross-links from sibling docs | `docs/.vitepress/` and the relevant section index |
| Links and anchors | relative links, fragment anchors, code-block references | the linked page and its actual heading text |
| Coverage | orphan pages, duplicate topics, stale renamed paths | the directory tree under `docs/` and `architecture/` |
| Tutorial-family naming | when several pages walk through the same workflow at different depth/audience (e.g. a base quick start plus deeper variants), names signal the relationship and scope instead of ambiguous modifiers like "extended"; cross-references point at the specific source section instead of restating it | the full set of sibling walkthrough pages, read together, not each in isolation |

A structural finding is any page that cannot be discovered, rendered,
or read in the expected order because its scaffolding drifted.

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

Resolve every local link and fragment anchor that the page introduces or
updates. If a link target moved, update the source and the destination
path together.

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
