# Review Pattern: Reuse exported constants instead of hardcoded duplicates

**Review-Area**: architecture
**Detection-Hint**: When a PR introduces an exported constant as 'single source of truth', grep the codebase for the literal value it replaces and flag any remaining hardcoded occurrences in tests, fixtures, and docs.
**Severity**: WARNING
**Occurrences**: 1

## What to check

After a new constant (e.g. DefaultTrustFlushSchedule) is introduced, verify that tests and documentation reference the constant rather than duplicating its literal value, so future changes to the default propagate consistently.

## Why it matters

Hardcoded duplicates of a 'single source of truth' constant cause silent drift: changing the default in one place leaves stale literals in tests/docs that may pass while documenting/asserting the wrong value.

## Examples from external reviews

### CC-0096 — sourcery-ai[bot]
- **Feedback**: You've introduced DefaultTrustFlushSchedule as the single source of truth, but a lot of tests and docs still hardcode "0 * * * *"; consider reusing the constant (where possible in Go code) to reduce the risk of drift if the default is ever changed.
- **What was missed**: After a new constant (e.g. DefaultTrustFlushSchedule) is introduced, verify that tests and documentation reference the constant rather than duplicating its literal value, so future changes to the default propagate consistently.
- **Fix**: Replace literal "0 * * * *" occurrences in Go test files with the DefaultTrustFlushSchedule constant; update docs to reference the constant name or note that the literal is sourced from it.
