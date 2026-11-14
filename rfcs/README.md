# CobaltCore RFCs

Requests for Comments (RFCs) describe proposed changes to CobaltCore and give contributors a place to discuss
the design before implementation. This directory records each proposal, its rationale, alternatives, and
eventual decision so future contributors can understand why the project took a particular direction.

RFCs must be submitted through a pull request for review and discussion. **An RFC may only be merged after
maintainers have approved the proposal.** Start with `Draft`, use `Proposed` when ready for review, and mark
it `Accepted` once approval is recorded and before merging. Acceptance records agreement on the design;
implementation is tracked separately.

## Proposal index

| RFC | Title | Status | Date |
| --- | --- | --- | --- |
| [0001](text/0001.release-channels.md) | CobaltCore Release Management | Proposed | 2026-11-07 |

Keep this index in numerical order and update it when adding a proposal, changing its status, or moving it
to the archive. Retain entries for superseded RFCs and point their links to the archived files.

## File layout and naming

Store active proposals in [`text/`](text/) and superseded RFCs in [`archive/`](archive/). Use this filename
format in both directories:

```text
<rfc_number>.<short-title>.md
```

Use a four-digit, zero-padded number and a short, lowercase title with words separated by hyphens:

```text
text/0001.release-channels.md
```

Choose the next unused number across `text/`, `archive/`, and open RFC pull requests. Resolve any collision
before merging. Once a proposal is merged, keep its number and filename stable, including when moving it
to the archive. Update links affected by that move; do not reuse numbers from rejected, withdrawn, or
superseded proposals.

## Writing a proposal

Copy the [RFC template](rfc.template.md) into `text/` using the filename convention above. Start with
`status: Draft`, fill in the metadata and sections, then remove the writing prompts. Set the status to
`Proposed` when the RFC is ready for review.

Start the document with YAML front matter, followed by its title. Use the existing release management RFC
as an example of the metadata format:

```markdown
---
status: Proposed
date: "2026-11-07"
related:
  - title: "#1617 — Publish Cobalt Core releases"
    url: https://github.com/C5C3/cobaltcore/issues/1617
---

# RFC: CobaltCore Release Management
```

Set `date` to the proposal's creation date in `YYYY-MM-DD` format. Link relevant issues, pull requests, or
other RFCs under `related`; use `related: []` when there are none.

A proposal should explain:

| Section | What to cover |
| --- | --- |
| Summary | The proposed change and its intended outcome |
| Motivation | The problem, who it affects, and why it needs attention |
| Proposed design | How the change works, with examples where useful |
| Compatibility and rollout | Effects on existing users, migration steps, and deployment considerations |
| Alternatives considered | Other approaches and the reasons for choosing this one |
| Open questions | Decisions that still need input |
| Success criteria | Observable results that show the proposal has been implemented |
| References | Supporting issues, documentation, and related proposals |

Adapt the sections to the proposal. Number headings consistently: `## 1. Summary`, `### 3.1 Component
versions`, and `#### 3.1.1 Example`. Leave the document title unnumbered.

Follow the repository's [writing style guide](../STYLE_GUIDE.md). Distinguish current behavior from proposed
behavior, and label illustrative examples or unverified assumptions clearly.

## Discussion and decisions

1. Check the proposal index and related issues for existing discussion.
2. Add the RFC under `text/`, set its status to `Draft`, and add it to the index.
3. Open a draft pull request that describes the problem and links the relevant issues. When ready for
   review, set the RFC and index status to `Proposed` and mark the pull request ready for review.
4. Discuss the design in the pull request. Update the RFC as feedback resolves questions or changes the
   proposal, keeping the document understandable without reading the full discussion.
5. Address review feedback and obtain maintainer approval of the proposal before merging.
6. Record the approval and link the decision discussion in the RFC. Set its status and index entry to
   `Accepted`, then merge the approved RFC pull request.
7. Track implementation in linked issues and pull requests. Mark the RFC `Implemented` once its success
   criteria are met.

Review should consider operational impact, compatibility, implementation complexity, and how the proposed
behavior will be validated. Record unresolved questions explicitly so they can be addressed before adoption.

If a proposal is rejected or withdrawn, record the outcome and rationale in the pull request and close it
without merging. Preserve the discussion for future reference.

## RFC lifecycle

The usual progression is:

```text
Draft → Proposed → Accepted → Implemented → Superseded
```

An accepted RFC can also be superseded before implementation if maintainers approve a replacement.

| Status | Meaning |
| --- | --- |
| `Draft` | Being prepared; not yet ready for formal review |
| `Proposed` | Under review in an open pull request; not yet approved for merge |
| `Accepted` | Maintainers have approved the design for merge; implementation may still be pending |
| `Implemented` | The agreed design has been delivered and its success criteria met |
| `Superseded` | An accepted replacement RFC takes precedence; the previous RFC is retained in `archive/` |
| `Rejected` | The proposal was considered and declined; retain the rationale |
| `Withdrawn` | The author has stopped pursuing the proposal |

`Rejected` and `Withdrawn` are alternative outcomes for proposals that do not proceed to acceptance.
Keep their discussion in the closed pull request without merging the proposal.

## Preserving decisions and archiving RFCs

Once accepted, an RFC's agreed design is immutable. Typos and clarifications may be corrected, and lifecycle
metadata, implementation references, and supersession links may be updated. Significant design changes
require a new RFC and another review and approval cycle.

When a replacement RFC is accepted:

1. Mark the previous RFC `Superseded` and link to the accepted replacement.
2. Link back to the previous RFC from its replacement so readers can trace the decision.
3. Move the superseded RFC from `text/` to `archive/`, keeping its number, filename, and historical content.
4. Update its status and path in the proposal index, along with incoming links and any relative links
   affected by the move.

The [`archive/`](archive/) directory contains a `.gitkeep` file so it is present even before the first RFC
is superseded. Never delete historical RFCs; the archive preserves their decisions and rationale.

When implementing changes, follow the latest accepted, non-superseded RFC for the relevant subject. An
`Implemented` RFC remains authoritative until an accepted replacement supersedes it.
