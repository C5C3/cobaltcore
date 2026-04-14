# Review Pattern: Resolve linter warnings on new code before approving

**Review-Area**: validation
**Detection-Hint**: Run the linter (or check CI linter output) on changed lines. Any new warning on code introduced in the PR is a blocker, especially deprecation warnings.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Verify that newly added or modified code does not introduce linter warnings. If the linter flags a symbol as deprecated, the code must not use it.

## Why it matters

Deprecated APIs will eventually be removed, creating future breakage. When the linter already surfaces the problem, letting it pass review means the team is ignoring automated tooling that exists precisely to prevent these issues.

## Examples from external reviews

### CC-0071 — gndrmnn
- **Feedback**: I don't see a reason why the check for `r.Requeue` was added here. The linter even complains that `r.Requeue` is deprecated. I suggest removing this again.
- **What was missed**: Verify that newly added or modified code does not introduce linter warnings. If the linter flags a symbol as deprecated, the code must not use it.
- **Fix**: Removed the `r.Requeue` check from `shortestRequeue`, updated the doc comment, and deleted the test `TestShortestRequeue_RequeueTrue` that covered the now-removed branch.
