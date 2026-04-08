# Review Pattern: Validate stated acceptance criteria are met

**Review-Area**: architecture
**Detection-Hint**: Read the ticket or PR description for any quantitative targets (line counts, coverage thresholds, performance budgets). Measure the actual result against the stated goal before approving.
**Severity**: WARNING
**Occurrences**: 1

## What to check

If the PR or linked ticket states a measurable target (e.g. 'reduce ci.yaml to < 500 lines'), verify the final artifact meets that target. If it does not, the PR should document why and what remains.

## Why it matters

Approving a PR that misses its own acceptance criteria creates silent scope drift. The remaining work is easily forgotten since the ticket appears done, leaving the original goal unfinished.

## Examples from external reviews

### CC-0050 — berendt
- **Feedback**: The acceptance criteria states the CI workflow file should be reduced to < 500 lines (orchestration only). The file went from 884 to 680 lines (23% reduction), missing the target.
- **What was missed**: If the PR or linked ticket states a measurable target (e.g. 'reduce ci.yaml to < 500 lines'), verify the final artifact meets that target. If it does not, the PR should document why and what remains.
- **Fix**: Extracted the 30-line 'Resolve effective changes' block into hack/ci-resolve-changes.sh, reducing ci.yaml from 680 to 635 lines. Gap to 500-line target acknowledged as follow-up work.
