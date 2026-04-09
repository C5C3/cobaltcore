# Review Pattern: Avoid brittle index-based container assertions in tests

**Review-Area**: testing
**Detection-Hint**: In test code, look for direct slice indexing like `Containers[0]` or `InitContainers[0]` paired with `assert.Len(..., 1)`. These patterns break as soon as a sidecar or additional container is added.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Do the tests hard-code container positions and assert exact container counts? If so, they should instead find the target container by name and assert only on the containers that matter for the test.

## Why it matters

Index-based access couples tests to container ordering and count. Adding a sidecar, init container, or any future container breaks every test even though the security context requirement is unchanged. Name-based lookup isolates the assertion to what actually matters.

## Examples from external reviews

### CC-0045 — sourcery-ai[bot]
- **Feedback**: The new security context tests for Jobs/CronJobs index directly into `Containers[0]`/`InitContainers[0]` and assert an exact length of 1; to make these tests more resilient to future container additions, consider finding containers by name.
- **What was missed**: Do the tests hard-code container positions and assert exact container counts? If so, they should instead find the target container by name and assert only on the containers that matter for the test.
- **Fix**: Replace `Containers[0]` access with a helper that finds a container by name, and drop the exact-length assertion in favor of asserting the target container exists.
