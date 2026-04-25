# Review Pattern: Audit suspend/disable patches against suites that depend on the disabled component

**Review-Area**: testing
**Detection-Hint**: Grep kustomization patches for `suspend: true` or component-disabling overlays, then check whether any e2e suite running in that overlay's job assumes the component is active.
**Severity**: WARNING
**Occurrences**: 1

## What to check

When an overlay disables an operator/controller (e.g., `suspend: true` on keystone-operator in `deploy/kind/base/kustomization.yaml`), confirm no test suite scheduled for jobs using that overlay relies on CRDs or behavior that the disabled component provides.

## Why it matters

Disabling a component in shared base manifests can silently invalidate tests in unrelated jobs, and the failure mode is a green CI run rather than an obvious error.

## Examples from external reviews

### CC-0088 — berendt
- **Feedback**: `deploy/kind/base/kustomization.yaml:106` patches the keystone-operator with `suspend: true`, so the Keystone CRD is never present in `e2e-infra`.
- **What was missed**: When an overlay disables an operator/controller (e.g., `suspend: true` on keystone-operator in `deploy/kind/base/kustomization.yaml`), confirm no test suite scheduled for jobs using that overlay relies on CRDs or behavior that the disabled component provides.
- **Fix**: Moved the dependent smoke suite to a job whose overlay does install the keystone-operator, so the CRD is present and the test runs.
