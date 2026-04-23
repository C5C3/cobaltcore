# Review Pattern: No historical references in comments

**Review-Area**: documentation
**Detection-Hint**: Scan doc/code comments for phrases like 'previously', 'used to', 'replaces', 'prior implementation', or references to review rounds/PR numbers
**Severity**: WARNING
**Occurrences**: 1

## What to check

Comments should describe current behavior only, not how the code used to work or why it was changed from a prior version

## Why it matters

Future readers have no context on prior implementations; historical notes add noise, become stale, and belong in git history or PR descriptions, not source comments

## Examples from external reviews

### CC-0087 — gndrmnn
- **Feedback**: Do not reference prior implementation details in code comments. Nobody evaluating the new code will know or care about how it was once implemented.
- **What was missed**: Comments should describe current behavior only, not how the code used to work or why it was changed from a prior version
- **Fix**: Rewrote the secretToKeystoneMapper doc comment to remove the paragraph describing how the new path 'replaces the prior unfiltered namespace-scoped List' and removed 'review #1' parenthetical references, keeping only current-behavior rationale.
