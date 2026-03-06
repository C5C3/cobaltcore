# Review Pattern: Ensure tests cover all instances of duplicated logic

**Review-Area**: testing
**Detection-Hint**: When reviewing tests for a workflow or module, check whether all instances of repeated patterns are exercised. If tests validate behavior for one copy of duplicated logic, look for untested copies.
**Severity**: WARNING
**Occurrences**: 1

## What to check

When duplicated logic exists (even if justified), verify that the test suite validates every instance — not just the first occurrence. Check that divergence between copies would be caught by at least one test.

## Why it matters

A test suite that only covers one instance of duplicated logic creates a false sense of safety. Changes to one copy are validated while the other copy can silently drift, introducing bugs that pass CI undetected.

## Examples from external reviews

### CC-0007 — greptile-apps[bot]
- **Feedback**: The test suite (`verify_build_images_workflow.sh`) checks the tag schema only in the `build-service-images` step (via `test_tag_schema_composite`), so this divergence would not be caught by automated checks either.
- **What was missed**: When duplicated logic exists (even if justified), verify that the test suite validates every instance — not just the first occurrence. Check that divergence between copies would be caught by at least one test.
- **Fix**: Added three new test cases to verify_build_images_workflow.sh to enforce the null guards in both yq call sites and verify the presence of the sync comment.
