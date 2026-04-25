# Review Pattern: Explain intentional gaps in sequential identifiers

**Review-Area**: documentation
**Detection-Hint**: When reviewing test suites or numbered specs (REQ-001, REQ-002, ...), check that any missing numbers in the sequence are explicitly justified in a comment or doc near the gap.
**Severity**: WARNING
**Occurrences**: 1

## What to check

If a sequence of identifiers (REQ-NNN, step numbers, file prefixes like 08-...) skips a value, verify there is a visible note explaining why so future readers don't assume an oversight.

## Why it matters

Unexplained gaps in numbered sequences make readers wonder if a step was lost, leading to wasted investigation time and risk of someone re-adding a deliberately removed case.

## Examples from external reviews

### CC-0094 — sourcery-ai[bot]
- **Feedback**: The REQ numbering around replicas (REQ-007 vs REQ-008) is a bit confusing now that the `08-replicas-zero` case is intentionally dropped; consider either renumbering the remaining REQs or adding a short explicit note in the chainsaw test comments to explain the gap so future readers don't wonder about the missing step.
- **What was missed**: If a sequence of identifiers (REQ-NNN, step numbers, file prefixes like 08-...) skips a value, verify there is a visible note explaining why so future readers don't assume an oversight.
- **Fix**: Added a fenced `Numbering gap (CC-0094 review-1)` block in chainsaw-test.yaml between REQ-006 and REQ-008 explaining that REQ-007 / 08-replicas-zero.yaml is intentionally absent because the mutating webhook coerces replicas:0 → 3 before the Minimum=1 marker fires.
