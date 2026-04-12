# Review Pattern: Use existing utility functions instead of reimplementing them

**Review-Area**: architecture
**Detection-Hint**: Look for small custom helper functions (e.g., boolPtr, findCondition) and check whether an equivalent already exists in standard libraries, well-known packages (k8s.io/utils/ptr, apimachinery meta), or the project's own shared packages (internal/conditions).
**Severity**: BLOCKING
**Occurrences**: 2

## What to check

Any newly introduced helper or utility function that wraps a trivial operation — pointer conversion, list searching, condition lookup — should be cross-referenced against existing project utilities and common upstream packages.

## Why it matters

Reinvented helpers create inconsistency, increase maintenance burden, and signal that the author is unaware of existing tooling. They also bypass tested, canonical implementations.

## Examples from external reviews

### CC-0013 — gndrmnn
- **Feedback**: Thats what the ``k8s.io/utils/ptr`` package is for. Use that instead of reinventing functions
- **What was missed**: Any newly introduced helper or utility function that wraps a trivial operation — pointer conversion, list searching, condition lookup — should be cross-referenced against existing project utilities and common upstream packages.
- **Fix**: Replaced custom boolPtr function with ptr.To from k8s.io/utils/ptr; replaced custom findCondition helper with meta.FindStatusCondition from apimachinery across 6 test files.

### CC-0064 — berendt
- **Feedback**: buildSchemaCheckJob reimplements the identical volume/volumeMount/securityContext/restartPolicy structure independently instead of delegating to the shared buildDBJob helper used by buildDBSyncJob, buildExpandJob, buildMigrateJob, and buildContractJob. When buildDBJob is updated (e.g., when CC-0042 adds resource limits), buildSchemaCheckJob will silently diverge.
- **What was missed**: Does the new function reimplement volume mounts, security contexts, restart policies, or other pod spec boilerplate that an existing helper already assembles? Could the existing helper be extended with optional parameters instead?
- **Fix**: Refactored buildSchemaCheckJob to delegate to the shared buildDBJob helper with custom overrides for backoffLimit and TTL, eliminating ~100 lines of duplicated Job construction.
