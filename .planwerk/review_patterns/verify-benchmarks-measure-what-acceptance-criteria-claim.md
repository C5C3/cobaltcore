# Review Pattern: Verify benchmarks measure what acceptance criteria claim

**Review-Area**: testing
**Detection-Hint**: When a PR states a specific performance improvement target (e.g. '30% latency reduction'), check whether the included benchmark actually exercises the conditions that produce the improvement. Fake/mock clients that return instantly cannot demonstrate gains from I/O parallelization.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Does the benchmark use a test setup where the optimization under test can actually manifest? For parallelization claims, do the sub-operations have realistic latency? Are before/after results included or referenced?

## Why it matters

A benchmark that passes but cannot demonstrate the claimed improvement gives false confidence. The acceptance criterion becomes unverifiable, and regressions will go undetected.

## Examples from external reviews

### CC-0071 — berendt
- **Feedback**: The benchmark uses a fake client where all operations complete instantly — parallelization benefits come from overlapping I/O wait, which doesn't exist with a fake client. The benchmark establishes a baseline but cannot validate the 30% claim.
- **What was missed**: Does the benchmark use a test setup where the optimization under test can actually manifest? For parallelization claims, do the sub-operations have realistic latency? Are before/after results included or referenced?
- **Fix**: Add a benchmark variant that uses envtest or injects simulated API latency to demonstrate real parallelization gains, and include before/after results in the PR description.
