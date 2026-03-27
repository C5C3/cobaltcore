# Review Pattern: Override unsafe CI matrix defaults explicitly

**Review-Area**: testing
**Detection-Hint**: When reviewing CI workflows with strategy.matrix blocks, check whether fail-fast is explicitly set. If omitted, the GitHub Actions default (true) silently cancels sibling matrix legs on first failure.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Every strategy.matrix block in CI workflows must include an explicit fail-fast: false (or a justified fail-fast: true with a comment). Never rely on the platform default.

## Why it matters

Default fail-fast: true cancels remaining matrix legs on the first failure, hiding whether multiple operators or configurations are broken in a single run. This delays discovery of independent failures across subsequent CI cycles.

## Examples from external reviews

### CC-0018 — berendt
- **Feedback**: Every matrix job (test, test-integration, e2e-keystone, build-and-push, helm-push) omits fail-fast: false. The default fail-fast: true cancels remaining legs on the first failure, hiding whether multiple operators are broken in a single run.
- **What was missed**: Every strategy.matrix block in CI workflows must include an explicit fail-fast: false (or a justified fail-fast: true with a comment). Never rely on the platform default.
- **Fix**: Add fail-fast: false to all five strategy: blocks in the workflow file.
