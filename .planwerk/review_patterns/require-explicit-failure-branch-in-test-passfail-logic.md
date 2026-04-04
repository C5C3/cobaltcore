# Review Pattern: Require explicit failure branch in test pass/fail logic

**Review-Area**: testing
**Detection-Hint**: Look for if-blocks that increment PASS but have no corresponding else/elif that increments FAIL. Search for patterns like `if <condition>; then ... PASS=$((PASS + 1)) ... fi` without an else clause.
**Severity**: BLOCKING
**Occurrences**: 3

## What to check

Every conditional block that increments a PASS counter must also have an else/elif branch that increments FAIL. There must be no code path through a test function that exits without incrementing either counter.

## Why it matters

A test that silently returns without incrementing PASS or FAIL gives a false sense of security. If a yq query returns empty results or a parse error occurs, the test appears to succeed (no FAIL) while actually validating nothing. For security-critical checks like PR-skip guards, this means a misconfiguration could go completely undetected.

## Examples from external reviews

### CC-0029 — greptile-apps[bot]
- **Feedback**: `test_sbom_steps_pr_skip_guard` has a silent failure mode: if the yq query returns empty results (e.g., no steps found, or a parse error), the function increments neither PASS nor FAIL and returns silently.
- **What was missed**: Every conditional block that increments a PASS counter must also have an else/elif branch that increments FAIL. There must be no code path through a test function that exits without incrementing either counter.
- **Fix**: Added else branches to three conditional blocks so that empty yq results or failed guard checks explicitly increment FAIL with a descriptive message instead of silently returning.

### CC-0039 — berendt
- **Feedback**: The project has Chainsaw E2E tests for every existing feature under tests/e2e/keystone/ (autoscaling, basic-deployment, brownfield-database, credential-rotation, deletion-cleanup, fernet-rotation, image-upgrade, invalid-cr, middleware-config, missing-secret, policy-overrid...
- **What was missed**: Verify the PR includes a Chainsaw (or equivalent) E2E test suite that covers at minimum the create, update, and delete lifecycle of the new resource. The test directory name should match the feature name for discoverability.
- **Fix**: Created tests/e2e/keystone/network-policy/ with four files (chainsaw-test.yaml, 00-keystone-cr.yaml, 01-patch-update-ingress.yaml, 02-patch-disable-networkpolicy.yaml) covering create, update, and delete flows.
