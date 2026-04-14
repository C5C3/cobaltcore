# Review Pattern: Check sibling files for same anti-pattern when PR improves a pattern

**Review-Area**: testing
**Detection-Hint**: When a PR replaces a hardcoded value with a dynamic read or fixes a known anti-pattern, search for sibling/related files that still use the old approach.
**Severity**: WARNING
**Occurrences**: 1

## What to check

When a PR introduces a better pattern (e.g., reading replica count from spec instead of hardcoding, or properly sizing timeout budgets), grep sibling test files or related configs for the same anti-pattern the PR is fixing.

## Why it matters

Leaving sibling files with the old anti-pattern creates inconsistency and deferred breakage. Hardcoded values drift from the source of truth, and undersized timeouts cause flaky tests. Catching these during the PR that establishes the correct pattern is the cheapest time to flag a follow-up.

## Examples from external reviews

### CC-0076 — berendt
- **Feedback**: The memcached test correctly reads the desired replica count from .spec.replicas, but sibling tests operator-pod-crash and api-pod-kill-pdb still hardcode their replica counts (2 and 3 respectively). This PR improves on the existing pattern but the sibling tests remain inconsistent.
- **What was missed**: When a PR introduces a better pattern (e.g., reading replica count from spec instead of hardcoding, or properly sizing timeout budgets), grep sibling test files or related configs for the same anti-pattern the PR is fixing.
- **Fix**: No code change in this PR; follow-up PRs recommended to backport the dynamic replica count approach and adjust timeout budgets in sibling tests.
