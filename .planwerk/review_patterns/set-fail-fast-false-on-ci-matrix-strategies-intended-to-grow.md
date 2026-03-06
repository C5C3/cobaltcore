# Review Pattern: Set fail-fast false on CI matrix strategies intended to grow

**Review-Area**: architecture
**Detection-Hint**: Look for GitHub Actions `strategy.matrix` blocks that omit `fail-fast: false`. Cross-reference with documentation or comments indicating the matrix will be expanded with additional entries.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Any matrix job that is designed to run independent builds/tests for multiple services or configurations should explicitly set `fail-fast: false`. Check both build and test matrix jobs.

## Why it matters

The default `fail-fast: true` cancels remaining matrix legs on the first failure, hiding whether multiple services are broken. This forces sequential fix-and-rerun cycles instead of surfacing all failures in a single run.

## Examples from external reviews

### CC-0007 — greptile-apps[bot]
- **Feedback**: `fail-fast: true` (default) will silence failures when matrix is extended. [...] a failure in one service/release combination will immediately cancel all remaining matrix legs. This makes it impossible to detect whether multiple services are broken in a single run.
- **What was missed**: Any matrix job that is designed to run independent builds/tests for multiple services or configurations should explicitly set `fail-fast: false`. Check both build and test matrix jobs.
- **Fix**: Added `fail-fast: false` to both the `build-service-images` and `smoke-test` matrix strategy blocks.
