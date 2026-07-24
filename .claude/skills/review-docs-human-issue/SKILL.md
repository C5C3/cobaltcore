---
name: review-docs-human-issue
description: >-
  Review documentation end-to-end without skipping any section, then
  produce a complete issue that lists user-facing problems with clear
  evidence and severity. Use when asked to audit docs quality from a
  human reader perspective, before release, or after major doc edits.
argument-hint: Target docs scope and audience
---

# Review docs and file a user-facing issue

This skill performs a full documentation review from the perspective of a
human reader. It does not stop at technical correctness: it finds anything
that makes docs harder to use, trust, or follow.

## When to use

- You need a complete doc review and cannot skip sections.
- You need a single issue listing all problems found.
- You want findings prioritized by user impact.

## Inputs

- Scope: exact files or directories to review (for example `docs/` + `README.md`).
- Audience: who the docs are for (new user, operator maintainer, contributor).
- Personas: reader lenses to run during review. Include at least developer,
  editor, and management consultant or business analyst unless explicitly scoped.
- Goal: quality audit, release readiness, onboarding readiness, or consistency pass.

If one of these inputs is missing, infer from repository conventions and state
assumptions in the report.

### Default persona set

Use this set when personas are not supplied:

- Developer: checks technical correctness, runnable steps, prerequisites,
  and debugging paths.
- Editor: checks structure, readability, consistency of voice, grammar,
  and heading flow.
- Management consultant or business analyst: checks business framing,
  decision clarity, expected outcomes, assumptions, and trade-offs.
- Operator or SRE: checks operational safety, rollback guidance,
  observability cues, and failure handling.
- New adopter: checks onboarding clarity, jargon load, and first-run success path.

### Optional personas (use when relevant)

- Security reviewer: checks secret handling, unsafe defaults, threat-model gaps,
  and whether security caveats are visible at the moment of action.
- Platform owner: checks standardization, support boundaries, ownership model,
  and whether docs align with platform operating constraints.
- Release manager: checks upgrade/rollback readiness, version provenance,
  deprecation messaging, and readiness-gate clarity.
- Support engineer: checks diagnosability, symptom-to-cause mapping,
  and whether triage paths are fast under incident pressure.
- Technical writer: checks flow, audience fit, ambiguity, and whether each section
  explains intent before mechanics.
- Copy editor: checks grammar, punctuation, sentence economy, and consistency of
  voice and terminology.
- Information architect: checks document structure, progressive disclosure,
  section chunking, and findability of key tasks.
- UX writer: checks clarity of instructions, action labels, and whether users can
  predict outcomes from wording alone.
- Non-native English reader: checks idioms, phrasal verbs, and dense phrasing that
  increases reading difficulty.
- Executive skim reader: checks whether headings and first sentences convey status,
  risk, and decision-relevant takeaways quickly.

## Procedure

### 1. Build complete review inventory

Enumerate every in-scope document and section heading before reviewing content.
Use this as a checklist and do not mark completion until every item is read.

- Include top-level pages, nested pages, frontmatter, headings, tables,
  callouts, code blocks, and command examples.
- Track each section as `unreviewed`, `reviewed`, or `blocked`.
- If content is blocked (missing asset, broken include), create a finding for it.

### 2. Review section-by-section with multi-persona lenses

Read every section in order and evaluate it once per selected persona.

- Clarity: can a reader understand this on first pass?
- Actionability: can they execute steps exactly as written?
- Navigation: can they find prerequisite or next-step information quickly?
- Trust: are claims specific, current, and consistent with nearby docs?
- Cognitive load: is the section concise enough for real usage?
- Error recovery: does the doc help when a step fails?

Use persona-specific checks in addition to shared checks:

- Developer: command correctness, version/source provenance, and troubleshooting depth.
- Editor: sentence quality, repetition control, term consistency, and section balance.
- Management consultant or business analyst: value narrative, business impact,
  risk clarity, and decision support quality.
- Operator or SRE: runbook usefulness, rollback safety, and monitoring hints.
- New adopter: plain-language clarity and minimum context needed to succeed.

Apply these deeper checks per persona:

- Developer:
  - Can every command be copied and run in sequence without hidden state?
  - Are prerequisites introduced before first use (tools, env vars, permissions)?
  - Are expected outputs specific enough to distinguish success from partial failure?
  - Are fallback and debug paths attached to the step that commonly fails?
- Editor:
  - Does each section open with intent, then action, then expected result?
  - Are similar steps written with similar depth and tense?
  - Is terminology stable across quick start variants and reference links?
  - Are headings scannable and non-redundant in a long page?
- Management consultant or business analyst:
  - Is the user value clear before procedural detail?
  - Are trade-offs explicit (time, cost, risk, complexity, portability)?
  - Are decision points visible with criteria, not just options?
  - Is there a clear "what good looks like" outcome at each major milestone?
- Operator or SRE:
  - Are blast radius, rollback path, and safe re-run semantics documented?
  - Are health checks and timeout expectations actionable under pressure?
  - Are operational ownership boundaries clear (who owns what component)?
  - Are logs, conditions, and metrics references sufficient for first response?
- New adopter:
  - Can a first-time reader complete the flow without repo-internal context?
  - Is jargon either avoided or defined at first mention?
  - Are platform-specific assumptions explicit (macOS/Linux, rootful/rootless)?
  - Is the path from first success to next action obvious?

Use these readability-focused checks when optional writing personas are selected:

- Technical writer:
  - Does each section answer "why this step exists" before "how to do it"?
  - Are transitions explicit between prerequisite, action, and verification?
  - Are warnings near the exact step where mistakes happen?
- Copy editor:
  - Are long sentences split where comprehension drops?
  - Are filler words, repeated qualifiers, and hedge phrases minimized?
  - Is capitalization, hyphenation, and term spelling consistent?
- Information architect:
  - Is critical path separate from optional depth and side quests?
  - Are anchors, cross-links, and section labels predictable and scannable?
  - Can readers recover quickly if they enter mid-page?
- UX writer:
  - Do command intros set expectation and success criteria clearly?
  - Are labels and callouts action-oriented rather than abstract?
  - Does each step avoid hidden assumptions and implied context?
- Non-native English reader:
  - Are idioms replaced with literal phrasing?
  - Are dense noun clusters broken into shorter clauses?
  - Are acronyms expanded at first mention when not universally known?
- Executive skim reader:
  - Do section openings surface impact, risk, and time cost?
  - Can a reader extract decision points from headings alone?
  - Is there a concise milestone summary at major transitions?

Escalate severity when multiple personas fail on the same section, especially
when New adopter + Operator or Developer are both impacted.

### 3. Capture findings immediately with evidence

For each problem, record:

- Severity: `HIGH`, `MEDIUM`, or `LOW`.
- Confidence: `verified`, `likely`, or `uncertain`.
- Persona: which reader lens found the issue.
- Location: file path plus section heading.
- User impact: what fails or confuses the reader.
- Evidence: quote or summarize the problematic text precisely.
- Fix direction: the smallest safe improvement.

Quote-or-demote rule:

- If you cannot quote the triggering text, set confidence to
  `uncertain` and do not classify the finding as `HIGH`.
- If a claim depends on behavior outside docs, cite the source file and
  line (script, config, or code) or mark it as an open question.

Persona evidence prompts:

- Developer: include exact failing command or missing prerequisite.
- Editor: include sentence/section pair showing structure drift.
- Management consultant or business analyst: include missing decision criterion or business outcome.
- Operator or SRE: include missing rollback/check/observability cue.
- New adopter: include exact jargon or context gap that blocks first-run comprehension.
- Technical writer: include where intent is missing or order harms comprehension.
- Copy editor: include sentence-level readability defect and a simpler rewrite direction.
- Information architect: include structure/navigation mismatch and affected user path.
- UX writer: include unclear instruction wording and resulting user misinterpretation.
- Non-native English reader: include idiom or dense phrase and a literal alternative.
- Executive skim reader: include missing decision signal in headings or summary lines.

Never merge unrelated problems into one finding. One user-visible defect per
finding keeps follow-up work precise.

### 4. Apply decision rules for severity

- `HIGH`: user is likely blocked, misled, or at risk of a wrong action.
- `MEDIUM`: user can proceed but with friction, ambiguity, or backtracking.
- `LOW`: polish issue that does not block progress.

When uncertain between two levels, choose the higher level and explain why.

### 5. Run completion checks

Before producing output, confirm:

- Every in-scope section is marked `reviewed` or `blocked`.
- Every selected persona has reviewed every in-scope section.
- Every finding includes impact and evidence.
- Duplicates are consolidated only when they are truly the same defect.
- Findings are sorted by severity, then by reader journey order.
- Every `HIGH` finding was re-checked against the current checkout
  immediately before output.

### 6. Produce the issue draft

Output a ready-to-file issue with this structure:

```markdown
Title: docs: user-focused quality review findings (<scope>)

## Summary
- Reviewed scope: ...
- Audience lens: ...
- Personas used: ...
- Sections reviewed: X/Y (blocked: Z)
- Findings: HIGH A / MEDIUM B / LOW C

## Persona coverage
- Developer: reviewed X/Y sections
- Editor: reviewed X/Y sections
- Management consultant or business analyst: reviewed X/Y sections
- Operator or SRE: reviewed X/Y sections
- New adopter: reviewed X/Y sections

## Persona summary
- Developer: top systemic gap
- Editor: top systemic gap
- Management consultant or business analyst: top systemic gap
- Operator or SRE: top systemic gap
- New adopter: top systemic gap

## Findings
### HIGH
1. [Persona | file path - section heading] Problem statement
  - Confidence:
   - Impact:
   - Evidence:
   - Suggested fix:

### MEDIUM
...

### LOW
...

## Coverage checklist
- [x] page/section 1
- [x] page/section 2
- [ ] page/section blocked (reason)

## Open questions
- ...
```

## Branching guidance

- If scope is very large, split review execution into batches but keep one final
  merged issue.
- If domain facts are uncertain, log as `Open questions` instead of guessing.
- If a finding belongs to code behavior rather than docs quality, still record it
  when it causes user confusion and tag it as cross-team follow-up.
- If the same defect appears across personas, keep one finding and list all
  impacted personas in that entry.

## Release-version handling

- Do not encode or hardcode any release version in this skill text, findings
  templates, or suggested fixes.
- When docs mention a release and it looks stale or unclear, report it as a
  provenance finding: ask where the value is sourced from and point to the
  source-of-truth file or generator path.
- Prefer wording such as "current supported release" or "value from release
  source-of-truth" over embedding concrete release numbers.

## Quality bar

- No skipped sections.
- No vague findings without user impact.
- No purely stylistic nitpicks unless they affect comprehension.
- Output is issue-ready without extra cleanup.

## Suppressions (do not report)

- Pure style preferences with no clarity, trust, or actionability impact.
- "Could be shorter" feedback without a concrete point where meaning or
  execution fails.
- Suggestions that rely on unstated assumptions and provide no quoted
  evidence.
- Duplicate findings that restate the same user-visible defect.

## Empty-result branch

Zero findings is a valid outcome. If the reviewed scope is clear,
actionable, and trustworthy for the selected personas, output a clean
issue draft with:

- Findings summary set to `HIGH 0 / MEDIUM 0 / LOW 0`.
- An explicit statement that no user-facing defects were found.
- Any residual risk listed only as open questions, not as invented
  findings.

## Related customizations

- Pair with [[check-doc-consistency]] for contradiction detection.
- Pair with [[check-doc-structure]] for nav/frontmatter/link integrity.
- Pair with [[check-doc-expressions]] for prose clarity refinement.