# Review Pattern: Catch reimplementations of existing library helpers

**Review-Area**: architecture
**Detection-Hint**: When a small helper function or inline snippet duplicates functionality already provided by an imported/available library (e.g. ptr.To, conditions.AllTrue), flag it and point to the existing utility.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Look for thin wrapper functions and hand-rolled snippets that replicate standard helpers already in use elsewhere in the codebase or its dependencies; suggest inlining the call to the existing utility.

## Why it matters

Reimplementing existing helpers increases surface area to maintain and test, diverges from established codebase idioms, and risks subtle behavioral differences from the canonical implementation.

## Examples from external reviews

### CC-0110 — gndrmnn
- **Feedback**: `aggregateReady` can be removed by in-lining `conditions.AllTrue` ... Why reimplement this? Replace with `ptr.To`
- **What was missed**: Look for thin wrapper functions and hand-rolled snippets that replicate standard helpers already in use elsewhere in the codebase or its dependencies; suggest inlining the call to the existing utility.
- **Fix**: Removed the aggregateReady wrapper in favor of conditions.AllTrue and replaced the hand-rolled pointer helper with ptr.To.
