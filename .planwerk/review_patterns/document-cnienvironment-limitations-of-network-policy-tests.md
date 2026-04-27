# Review Pattern: Document CNI/environment limitations of network policy tests

**Review-Area**: testing
**Detection-Hint**: When an e2e test asserts NetworkPolicy behavior, check whether the CI cluster's CNI actually enforces NetworkPolicy and whether the test file documents that limitation.
**Severity**: WARNING
**Occurrences**: 1

## What to check

For NetworkPolicy-related e2e tests, verify the test file explicitly documents whether the default CI CNI (e.g., kindnet) enforces policies, and calls out any DNAT/Service-VIP or leader-election edge cases that affect the assertions.

## Why it matters

A test that appears to pass on a non-enforcing CNI creates false confidence that the policy works; readers and maintainers need an explicit scope caveat to understand what the test actually proves.

## Examples from external reviews

### CC-0090 — berendt
- **Feedback**: E2E test cannot exercise NetworkPolicy enforcement on kindnet CI cluster
- **What was missed**: For NetworkPolicy-related e2e tests, verify the test file explicitly documents whether the default CI CNI (e.g., kindnet) enforces policies, and calls out any DNAT/Service-VIP or leader-election edge cases that affect the assertions.
- **Fix**: Added a prominent 'SCOPE CAVEAT' comment block at the top of the chainsaw-test.yaml documenting kindnet non-enforcement, Service-VIP DNAT issues on Calico/Cilium, and leader-election ambiguity, with a follow-up noted for a Calico-enabled kind CI job.
