# Review Pattern: Test script timeout must exceed worst-case loop duration

**Review-Area**: testing
**Detection-Hint**: When a test script contains sequential retry/polling loops with sleep, calculate the worst-case wall-clock time (iterations × sleep per iteration, summed across all sequential phases). Confirm this total is strictly less than the framework-level script timeout with reasonable margin.
**Severity**: WARNING
**Occurrences**: 2

## What to check

For each test script that uses polling loops (for/while with sleep):
1. Identify all sequential retry phases (loops that run one after another).
2. Calculate worst-case duration per phase: max_iterations × sleep_interval.
3. Sum across all sequential phases to get total worst-case wall-clock time.
4. Compare against the framework-level timeout (e.g., Chainsaw `timeout:`, pytest timeout, Jest timeout).
5. Verify the timeout exceeds worst-case by a reasonable margin (e.g., 20-30s) for kubectl overhead and scheduling jitter.

## Why it matters

When the framework timeout is less than or equal to the worst-case loop duration, the framework kills the script mid-execution. This produces cryptic timeout errors instead of meaningful test failures, and can leave resources in an inconsistent state that pollutes subsequent tests.

## Examples from external reviews

### CC-0076 — sourcery-ai[bot]
- **Feedback**: The script in Step 4 can run up to ~180s in the worst case (60s for the first loop + 120s for the second) while the Chainsaw script timeout is set to 120s, so consider tightening the loop bounds or increasing the timeout to avoid the framework killing the script mid-recovery.
- **What was missed**: Calculate the worst-case wall-clock time of all sequential retry loops in a test script (iterations × sleep per iteration, summed across phases). Confirm this total is strictly less than the framework-level timeout with reasonable margin.
- **Fix**: Phase 1 tightened from 30 to 15 iterations (30s max), Phase 2 from 60 to 45 iterations (90s max), giving 120s worst case, and the script timeout was increased from 120s to 150s to provide 30s margin.

### CC-0075 — berendt
- **Feedback**: This PR adds two new spec fields with non-trivial behavior (default injection, override, cluster-scoped PriorityClass lookup) but includes no E2E test directory, breaking the established 1 feature = 1 E2E directory convention.
- **What was missed**: When a PR introduces a new spec field or feature, verify that the corresponding E2E test directory is created following the same structure as existing feature tests (e.g., tests/e2e/<kind>/<feature-name>/chainsaw-test.yaml).
- **Fix**: Created tests/e2e/keystone/topology-spread/ and tests/e2e/keystone/priority-class/ directories with chainsaw-test.yaml files covering default injection, custom override, and empty/unset scenarios.
