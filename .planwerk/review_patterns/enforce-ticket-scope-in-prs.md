# Review Pattern: Enforce ticket scope in PRs

**Review-Area**: architecture
**Detection-Hint**: Scan the diff for references to ticket/issue IDs different from the PR's own ticket. Any added lines mentioning a different ticket number indicate scope creep.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Every changed line should relate to the ticket the PR is filed under. Look for references to other ticket IDs (e.g., CC-XXXX) in added documentation rows, comments, or code that do not match the current PR's scope.

## Why it matters

Scope creep makes PRs harder to review, harder to revert, and risks shipping unreviewed changes for other features. It also creates confusing git history where changes land under the wrong ticket.

## Examples from external reviews

### CC-0042 — berendt
- **Feedback**: The diff adds a networkPolicy row to the KeystoneSpec table referencing CC-0039, not CC-0042. This row does not exist on main and was introduced in this PR, constituting scope creep.
- **What was missed**: Every changed line should relate to the ticket the PR is filed under. Look for references to other ticket IDs (e.g., CC-XXXX) in added documentation rows, comments, or code that do not match the current PR's scope.
- **Fix**: Removed the networkPolicy documentation row from this PR so it can be added in the correct CC-0039 branch or a dedicated doc-only commit.
