# Review Pattern: Check cleanupPolicy is symmetric across related CRs

**Review-Area**: testing
**Detection-Hint**: Diff cleanupPolicy (and similar lifecycle fields) across all CRs in a single test directory; flag any that omit a setting present on siblings.
**Severity**: WARNING
**Occurrences**: 1

## What to check

When multiple related CRs are created in a single test (Keystone + User + Grant + Project), check that lifecycle/cleanup settings are applied consistently. Asymmetric cleanupPolicy across siblings usually indicates an oversight rather than intent.

## Why it matters

Inconsistent cleanup behavior leaves resources behind between test runs, causing flaky tests or polluting the cluster, and is hard to diagnose because failures only appear on retry.

## Examples from external reviews

### CC-0104 — berendt
- **Feedback**: W-002 (asymmetric cleanupPolicy) — added cleanupPolicy: Skip to the User and Grant CRs in tests/e2e/keystone/br...
- **What was missed**: When multiple related CRs are created in a single test (Keystone + User + Grant + Project), check that lifecycle/cleanup settings are applied consistently. Asymmetric cleanupPolicy across siblings usually indicates an oversight rather than intent.
- **Fix**: Added cleanupPolicy: Skip to the User and Grant CRs to match the sibling CRs in the same test.
