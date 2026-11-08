# AGENTS.md

## General

- Follow existing repository conventions.
- Keep changes minimal and focused.
- Prefer simplicity over unnecessary abstractions.

## RFC Guidelines

- RFCs document architectural decisions and their rationale.
- Use `Draft → Proposed → Accepted → Implemented → Superseded` as the RFC lifecycle.
- Start new RFCs as `Draft`, then mark them `Proposed` when ready for review.
- RFCs require maintainer review and approval before merging. Record approval and set the status to
  `Accepted` before merging; mark them `Implemented` when their success criteria are met.
- Accepted RFCs are immutable except for typos and clarifications that do not change the agreed design.
  Lifecycle metadata, implementation references, and supersession links may be updated as work progresses.
- Significant changes require a new RFC that supersedes the previous one.
- Once the replacement RFC is accepted, mark the previous RFC `Superseded` and link the two documents.
- Move superseded RFCs from `text/` to `archive/`, preserving their numbers, filenames, and decision history.
  Update the README index, incoming links, and relative links affected by the move.
- Never delete historical RFCs or reuse their numbers. Keep archived RFCs in the README index.
- Follow the latest accepted, non-superseded RFC for the relevant subject when implementing changes.
  An `Implemented` RFC remains authoritative until superseded.

## Documentation

- Keep documentation concise and accurate.
- Explain why, not just what.
- Update relevant documentation alongside implementation changes.
- Keep RFC metadata, the README index, and decision references consistent.
