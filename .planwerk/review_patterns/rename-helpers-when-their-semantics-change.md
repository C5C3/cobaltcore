# Review Pattern: Rename helpers when their semantics change

**Review-Area**: naming
**Detection-Hint**: When a function/variable's behavior is broadened or narrowed in a refactor, check whether its name still reflects what it actually returns or does.
**Severity**: WARNING
**Occurrences**: 1

## What to check

If a helper named after a specific concern (e.g. `apiResourceName`) is changed to serve a more general purpose (e.g. all sub-resources, returning the bare CR name), its name should be updated to match the new semantics, and all call sites/docs/tests that reference it by name should be updated together.

## Why it matters

Stale names create future confusion: readers infer behavior from the name and may use the helper incorrectly, or hesitate to use it where they should. The misalignment compounds as more callers are added.

## Examples from external reviews

### CC-0095 — sourcery-ai[bot]
- **Feedback**: Now that `apiResourceName` returns the bare CR name and is used for all sub-resources, consider renaming it to something neutral like `coreResourceName` or `subResourceName` to better reflect its semantics and avoid future confusion.
- **What was missed**: If a helper named after a specific concern (e.g. `apiResourceName`) is changed to serve a more general purpose (e.g. all sub-resources, returning the bare CR name), its name should be updated to match the new semantics, and all call sites/docs/tests that reference it by name should be updated together.
- **Fix**: Renamed `apiResourceName` to `subResourceName` across the helper, all five call sites, tests, and downstream docs/fixtures that referenced it by name.
