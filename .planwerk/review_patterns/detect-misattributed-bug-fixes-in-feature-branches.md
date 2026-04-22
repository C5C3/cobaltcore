# Review Pattern: Detect misattributed bug fixes in feature branches

**Review-Area**: architecture
**Detection-Hint**: For each commit, check whether the modified file paths belong to the feature's functional area. Fixes to unrelated test suites or modules inside a feature PR are scope creep even when the fix itself is correct.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Whether commits labeled with the feature ticket actually modify files related to that feature, versus unrelated tests or modules that should be tracked under their own tickets.

## Why it matters

Correct fixes attributed to the wrong ticket distort project history, make it impossible to cleanly revert the feature without also reverting the unrelated fix, and mask work on other tickets.

## Examples from external reviews

### CC-0086 — berendt
- **Feedback**: Commit dc0c38d 'fix(CC-0086): make deletion-cleanup finalizer assert nil-safe' ... The fix is correct ... but it belongs to the Keystone deletion-cleanup test (CC-0016/CC-0078), not to the Flux Web UI feature.
- **What was missed**: Whether commits labeled with the feature ticket actually modify files related to that feature, versus unrelated tests or modules that should be tracked under their own tickets.
- **Fix**: Extract the finalizer nil-safe fix to its own CC-0078 PR so it can be reviewed against the correct ticket.
