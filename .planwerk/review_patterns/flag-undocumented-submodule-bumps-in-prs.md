# Review Pattern: Flag undocumented submodule bumps in PRs

**Review-Area**: architecture
**Detection-Hint**: Check the diff for submodule pointer changes (lines like `-Subproject commit <hash>` / `+Subproject commit <hash>`). If present, verify the PR description explains why the submodule was bumped and what changed in it.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Any submodule commit hash change in the diff should be explicitly documented in the PR description. The submodule diff contents are not visible in the PR diff itself, so unreviewed changes can slip through.

## Why it matters

Submodule bumps are opaque in a normal PR diff — reviewers cannot see what actually changed. Undocumented bumps introduce unreviewed scope creep and make it impossible to assess whether the change is related to the PR's purpose or introduces risk.

## Examples from external reviews

### CC-0049 — berendt
- **Feedback**: The architecture submodule is bumped from 7beebd6 to d58b67d but the PR description does not mention architecture doc changes. The submodule bump is unlisted and its contents cannot be reviewed in this diff.
- **What was missed**: Any submodule commit hash change in the diff should be explicitly documented in the PR description. The submodule diff contents are not visible in the PR diff itself, so unreviewed changes can slip through.
- **Fix**: Document the architecture submodule bump rationale in the PR description, or split it into a separate PR if unrelated.
