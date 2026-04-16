# Review Pattern: Avoid brittle index-based container assertions in tests

**Review-Area**: testing
**Detection-Hint**: In test code, look for direct slice indexing like `Containers[0]` or `InitContainers[0]` paired with `assert.Len(..., 1)`. These patterns break as soon as a sidecar or additional container is added.
**Severity**: WARNING
**Occurrences**: 3

## What to check

Do the tests hard-code container positions and assert exact container counts? If so, they should instead find the target container by name and assert only on the containers that matter for the test.

## Why it matters

Index-based access couples tests to container ordering and count. Adding a sidecar, init container, or any future container breaks every test even though the security context requirement is unchanged. Name-based lookup isolates the assertion to what actually matters.

## Examples from external reviews

### CC-0045 — sourcery-ai[bot]
- **Feedback**: The new security context tests for Jobs/CronJobs index directly into `Containers[0]`/`InitContainers[0]` and assert an exact length of 1; to make these tests more resilient to future container additions, consider finding containers by name.
- **What was missed**: Do the tests hard-code container positions and assert exact container counts? If so, they should instead find the target container by name and assert only on the containers that matter for the test.
- **Fix**: Replace `Containers[0]` access with a helper that finds a container by name, and drop the exact-length assertion in favor of asserting the target container exists.

### CC-0074 — sourcery-ai[bot]
- **Feedback**: The current `HaveLen(3)` checks for volumes and volume mounts tightly couple this test to today's container layout, so it will fail if we legitimately add another volume or sidecar.
- **What was missed**: When a test verifies that certain items exist in a list (volumes, mounts, containers, env vars), check whether it uses strict length assertions (HaveLen(N)) that would break if unrelated items are legitimately added. Prefer presence-based assertions (HaveKey, ContainElement) over count-based ones unless the exact count is the requirement.
- **Fix**: Replaced `g.Expect(volumes).To(HaveLen(3))` with `g.Expect(volumes).NotTo(BeEmpty())` plus per-name presence checks using `HaveKey` and `HaveKeyWithValue` on a map built from the list.

### CC-0074 — berendt
- **Feedback**: Brittle Containers[0] assertion
- **What was missed**: Any test assertion that accesses a container by numeric index rather than by name. Check whether the target container could shift position due to sidecar injection, init container changes, or spec reordering.
- **Fix**: Replaced the index-based access with a name-based loop that finds the 'keystone-api' container by iterating and matching on the container name.
