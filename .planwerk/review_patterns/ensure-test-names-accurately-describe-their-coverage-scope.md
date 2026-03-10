# Review Pattern: Ensure test names accurately describe their coverage scope

**Review-Area**: testing
**Detection-Hint**: When a test name contains universal quantifiers like 'All', 'Every', 'Runs all', or 'Full', verify that every branch or validation path in the function under test is actually exercised. Cross-reference the test setup with the production code's branches.
**Severity**: WARNING
**Occurrences**: 1

## What to check

For tests claiming comprehensive coverage (e.g., 'RunsAllValidations'), enumerate all validation branches in the function under test and confirm each one is triggered in the test input. Check that the assertion count or error substring checks match the number of distinct validation paths.

## Why it matters

Misleadingly named tests give false confidence that all paths are covered and that error accumulation (no short-circuiting) is proven. Future developers may skip writing tests for uncovered paths, assuming they are already tested.

## Examples from external reviews

### CC-0011 — greptile-apps[bot]
- **Feedback**: The test is named to imply every validation rule is exercised simultaneously, but it only mutates Replicas, RotationSchedule, Plugins, and PolicyOverrides. The two other defense-in-depth checks — cache mutual-exclusivity (REQ-009) and database mutual-exclusivity (REQ-010) — are never put into a broken state here.
- **What was missed**: For tests claiming comprehensive coverage (e.g., 'RunsAllValidations'), enumerate all validation branches in the function under test and confirm each one is triggered in the test input. Check that the assertion count or error substring checks match the number of distinct validation paths.
- **Fix**: Expanded TestValidateCreate_RunsAllValidations to include cache and database mutual-exclusivity violations with assertions for both 'cache' and 'database' error substrings, so it now exercises all validation paths.
