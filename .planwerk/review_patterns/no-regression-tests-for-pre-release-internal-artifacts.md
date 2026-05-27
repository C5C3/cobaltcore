# Review Pattern: No regression tests for pre-release internal artifacts

**Review-Area**: testing
**Detection-Hint**: Look for newly added test files that pin internal implementation details (namespace names, string literals, file contents) when the project has no stable release and no external contract to protect.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Flag tests that exist solely to detect regressions in internal definitions the project itself controls, especially when there is no stable release. Code review — not automated regression tests — should catch such changes.

## Why it matters

Pre-stable projects pay a maintenance tax for tests that lock down internal naming/structure they fully control. These tests don't catch real bugs; they create friction on legitimate refactors and duplicate what PR review already covers.

## Examples from external reviews

### CC-0105 — gndrmnn
- **Feedback**: We are not testing code using string literals. And we are not testing for regressions in this PR, as 1) we control the namespace defintions and 2) there has never been a stable release of this project yet
- **What was missed**: Flag tests that exist solely to detect regressions in internal definitions the project itself controls, especially when there is no stable release. Code review — not automated regression tests — should catch such changes.
- **Fix**: Removed multiple newly added shell-based regression tests under tests/unit/ and tests/integration/ that asserted on namespace strings, kustomize output, and CI script contents.
