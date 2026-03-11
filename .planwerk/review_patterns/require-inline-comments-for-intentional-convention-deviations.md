# Review Pattern: Require inline comments for intentional convention deviations

**Review-Area**: documentation
**Detection-Hint**: When reviewing a file that follows a documented convention or template, diff the new code against the convention's specified pattern. Any structural deviation — even a justified one — without an accompanying comment is a finding.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Check whether code that intentionally diverges from a documented repository convention or template includes an inline comment explaining the rationale for the deviation.

## Why it matters

Without an explanation, a future reviewer or contributor will assume the deviation is accidental and 'fix' it back to the standard pattern, potentially reintroducing the problem the deviation was designed to prevent.

## Examples from external reviews

### CC-0033 — greptile-apps[bot]
- **Feedback**: This workflow uses a static `group: pages` with `cancel-in-progress: false` ... since it consciously diverges from the documented convention, a short inline comment explaining why would make the intent clear and prevent a future reviewer from 'fixing' it back to the standard pattern.
- **What was missed**: Check whether code that intentionally diverges from a documented repository convention or template includes an inline comment explaining the rationale for the deviation.
- **Fix**: Added an inline comment to the concurrency block explaining that the static group intentionally deviates from the `${{ github.ref }}-${{ github.workflow }}` pattern to prevent concurrent Pages deployments.
