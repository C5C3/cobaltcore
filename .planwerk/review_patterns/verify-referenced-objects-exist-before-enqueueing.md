# Review Pattern: Verify referenced objects exist before enqueueing

**Review-Area**: validation
**Detection-Hint**: In event-handler mappers that build reconcile requests from ownerReferences or annotations, check whether the code verifies the target actually exists.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Mapper/handler functions that enqueue reconcile requests from ownerReferences or labels should do a cached client.Get on the referenced object and drop NotFound results, while letting non-NotFound errors fall through to enqueue.

## Why it matters

Stale or spurious ownerRefs would otherwise cause the controller to repeatedly reconcile non-existent CRs, wasting work and masking bugs; swallowing all errors is equally bad because transient cache blips would drop legitimate events.

## Examples from external reviews

### CC-0087 — sourcery-ai[bot]
- **Feedback**: secretToKeystoneMapper currently enqueues reconcile requests purely from ownerReferences without verifying that the referenced Keystone objects actually exist; if spurious or stale ownerRefs are a concern, you may want to guard this with an existence check or a lightweight cache lookup to avoid queuing work for non-existent CRs.
- **What was missed**: Mapper/handler functions that enqueue reconcile requests from ownerReferences or labels should do a cached client.Get on the referenced object and drop NotFound results, while letting non-NotFound errors fall through to enqueue.
- **Fix**: Added a cached client.Get that drops the owner-ref only on NotFound; other errors still enqueue so transient failures don't lose events.
