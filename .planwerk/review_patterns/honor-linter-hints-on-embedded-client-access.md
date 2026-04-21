# Review Pattern: Honor linter hints on embedded-client access

**Review-Area**: naming
**Detection-Hint**: Look for `r.Client.Method(...)` calls on reconcilers where Client is embedded; the linter typically flags these as redundant selectors.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Usages of `.Client.X` on receivers that embed the client interface directly — the intermediary selector is redundant and usually already flagged by the linter.

## Why it matters

Ignoring existing linter warnings in submitted code signals the author did not run or review lint output, and leaves inconsistent access patterns across the codebase.

## Examples from external reviews

### CC-0078 — gndrmnn
- **Feedback**: As already suggested by the linter, Remove the `Client`
- **What was missed**: Usages of `.Client.X` on receivers that embed the client interface directly — the intermediary selector is redundant and usually already flagged by the linter.
- **Fix**: Changed `r.Client.Get(...)` to `r.Get(...)` so the embedded client is used directly.
