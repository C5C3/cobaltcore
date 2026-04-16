# Review Pattern: Detect no-op retry/wait loops

**Review-Area**: testing
**Detection-Hint**: When a loop polls a value and compares it to a reference, check whether the reference was obtained from the same source. If both reads hit the same API/object with no mutation in between, the comparison is tautologically true on the first iteration.
**Severity**: WARNING
**Occurrences**: 1

## What to check

For every wait/retry loop in tests: (1) identify where the 'expected' value was captured, (2) identify where the 'current' value is read inside the loop, (3) confirm something external actually changes between reads. If both values come from the same API object and nothing mutates it, the loop is a no-op.

## Why it matters

A no-op wait loop gives false confidence that timing-sensitive behavior was verified. The test passes instantly but covers nothing, hiding real flakiness that will surface in CI on slow clusters.

## Examples from external reviews

### CC-0074 — berendt
- **Feedback**: The loop re-reads the Secret API object and compares it to AFTER_HASH, which was already read from the same API. Since nothing else mutates the Secret between the two reads, CURRENT_HASH will always equal AFTER_HASH on the first iteration, making the loop a no-op.
- **What was missed**: For every wait/retry loop in tests: (1) identify where the 'expected' value was captured, (2) identify where the 'current' value is read inside the loop, (3) confirm something external actually changes between reads. If both values come from the same API object and nothing mutates it, the loop is a no-op.
- **Fix**: Removed the no-op wait loops entirely from both fernet-rotation and credential-rotation E2E tests, replacing them with comments explaining why no wait is needed.
