# CobaltCore RFCs

Requests for Comments (RFCs) describe proposed changes to CobaltCore and give contributors a place to discuss
the design before implementation. This directory records each proposal, its rationale, alternatives, and
eventual decision so future contributors can understand why the project took a particular direction.

RFCs must be submitted through a pull request for review and discussion. **An RFC may only be merged after
maintainers have approved the proposal.** Keep it `Proposed` during review, then mark it `Accepted` once
approval is recorded and before merging. Acceptance records agreement on the design; implementation is
tracked separately.

## Proposal index

| RFC | Title | Status | Date |
| --- | --- | --- | --- |
| [0001](text/0001.release-channels.md) | CobaltCore Release Management | Proposed | 2026-11-07 |

Keep this index in numerical order and update it when adding a proposal or changing its status.

## File layout and naming

Store proposals in [`text/`](text/) using this filename format:

```text
<rfc_number>.<short-title>.md
```

Use a four-digit, zero-padded number and a short, lowercase title with words separated by hyphens:

```text
text/0001.release-channels.md
```

Choose the next unused number and check open RFC pull requests for collisions. Resolve any collision
before merging. Once a proposal is merged, keep its number and filename stable so existing links remain
useful; do not reuse numbers from rejected, withdrawn, or superseded proposals.

## Writing a proposal

Copy the [RFC template](rfc.template.md) into `text/` using the filename convention above. Fill in the
metadata and sections, then remove the writing prompts.

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
2. Add the RFC under `text/`, set its status to `Proposed`, and add it to the index.
3. Open a pull request that describes the problem and links the relevant issues.
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

## Proposal status

| Status | Meaning |
| --- | --- |
| `Proposed` | Under review in an open pull request; not yet approved for merge |
| `Accepted` | Maintainers have approved the design for merge; implementation may still be pending |
| `Implemented` | The agreed design has been delivered and its success criteria met |
| `Rejected` | The proposal was considered and declined; retain the rationale |
| `Withdrawn` | The author has stopped pursuing the proposal |
| `Superseded` | A newer RFC replaces the proposal; link to its replacement |

Retain merged RFCs and closed proposal pull requests as part of the project's decision history. For a
substantial change to an accepted or implemented design, write a follow-up RFC and link both documents so
readers can trace the decision.
