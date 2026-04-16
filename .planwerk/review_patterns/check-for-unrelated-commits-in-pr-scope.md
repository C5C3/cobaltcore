# Review Pattern: Check for unrelated commits in PR scope

**Review-Area**: architecture
**Detection-Hint**: Before reviewing code changes, scan the commit log for the branch. Look for commit messages referencing a different ticket/issue ID than the PR's stated scope.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Every commit in the PR should relate to the stated feature or fix. Commits with different ticket prefixes (e.g., CC-0075 on a CC-0048 branch) indicate scope creep or accidental inclusion from a wrong base branch.

## Why it matters

Unrelated commits pollute git history, make reverts harder, and can introduce unreviewed changes that bypass the PR's review scope.

## Examples from external reviews

### CC-0074 — berendt
- **Feedback**: Commit b223879 (plan for CC-0075: topologySpreadConstraints and PriorityClass support) belongs to a different feature and pollutes this branch's commit history.
- **What was missed**: Every commit in the PR should relate to the stated feature or fix. Commits with different ticket prefixes (e.g., CC-0075 on a CC-0048 branch) indicate scope creep or accidental inclusion from a wrong base branch.
- **Fix**: Recommended removing the commit via interactive rebase or cherry-picking relevant commits onto a clean branch.
