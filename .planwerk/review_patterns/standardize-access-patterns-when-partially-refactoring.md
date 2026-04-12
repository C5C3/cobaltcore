# Review Pattern: Standardize access patterns when partially refactoring

**Review-Area**: architecture
**Detection-Hint**: When a PR changes how an embedded or wrapped object is accessed (e.g., r.Client.Get → r.Get), grep for the old pattern across the codebase to check whether the migration is complete or creates inconsistency.
**Severity**: WARNING
**Occurrences**: 1

## What to check

If a PR switches from a direct field access to an embedded/promoted method (or vice versa), verify that the change is applied consistently across all similar call sites, or that the partial change is intentional and documented.

## Why it matters

Mixed access patterns for the same operation make the codebase harder to reason about, grep, and refactor. Reviewers and future contributors cannot tell which form is canonical without investigating both.

## Examples from external reviews

### CC-0059 — sourcery-ai[bot]
- **Feedback**: In `reconcile_networkpolicy_test.go`, some calls were switched from `r.Client.Get` to `r.Get` while others in the codebase may still use the embedded client directly; it might be worth standardizing on a single access pattern.
- **What was missed**: If a PR switches from a direct field access to an embedded/promoted method (or vice versa), verify that the change is applied consistently across all similar call sites, or that the partial change is intentional and documented.
- **Fix**: Reverted the 4 `r.Client.Get` → `r.Get` changes back to `r.Client.Get`, restoring consistency with the remaining ~37 instances across the codebase.
