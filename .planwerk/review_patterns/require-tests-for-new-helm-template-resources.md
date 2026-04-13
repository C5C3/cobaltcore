# Review Pattern: Require tests for new Helm template resources

**Review-Area**: testing
**Detection-Hint**: When a PR adds a new Helm template file (e.g., a new sub-reconciler producing a Kubernetes resource like NetworkPolicy or Certificate), check that a corresponding *_test.yaml exists covering both enabled and disabled states plus key spec fields.
**Severity**: WARNING
**Occurrences**: 2

## What to check

Each new Helm template resource should have a test file covering: resource creation when enabled, resource absence when disabled, and correctness of critical spec fields (names, references, selectors).

## Why it matters

Untested Helm templates can silently render incorrect manifests. Catching missing test coverage at review time is far cheaper than debugging broken deployments.

## Examples from external reviews

### CC-0041 — berendt
- **Feedback**: W-004 was addressed by creating certificate_test.yaml with 5 test cases covering both enabled/disabled states, Issuer creation, Certificate DNS names, and issuer references.
- **What was missed**: Each new Helm template resource should have a test file covering: resource creation when enabled, resource absence when disabled, and correctness of critical spec fields (names, references, selectors).
- **Fix**: Created certificate_test.yaml with tests for enabled/disabled states, Issuer creation, Certificate DNS names, and issuer reference correctness.

### CC-0067 — berendt
- **Feedback**: This PR adds a new feature but includes no corresponding tests/e2e/keystone/healthcheck/ directory. The unit tests are thorough, but a Chainsaw test that deploys a Keystone CR to a real cluster and asserts the KeystoneAPIReady condition transitions to True after the Deployment becomes ready would verify the end-to-end wiring.
- **What was missed**: When a PR introduces a new feature (new reconcile function, new status condition), verify a matching Chainsaw E2E test directory exists that exercises the feature end-to-end on a real cluster.
- **Fix**: Created tests/e2e/keystone/healthcheck/chainsaw-test.yaml that applies a Keystone CR, waits for DeploymentReady=True, asserts KeystoneAPIReady=True with reason APIHealthy, and verifies the aggregate Ready=True condition.
