# Pattern: phase1-failure label + spec.concurrent:false for global-state failure tests

**Component**: tests/e2e/keystone/<failure-test>/
**Category**: testing
**Applies-When**: Adding a new E2E failure-injection test that mutates global cluster infrastructure (OpenBao seal state, OpenBao policies, ExternalSecrets Deployment) and would race with parallel-pool happy-path tests

## Description

Each failure/recovery test that mutates infrastructure shared across the openstack namespace (OpenBao replicas, ESO Deployment, OpenBao policies) carries the Chainsaw test label phase1-failure: 'true' for selective local runs and sets spec.concurrent: false to opt out of the chainsaw parallel:4 pool. Tests that mutate ONLY their own namespace's CR (e.g., db-expand-failure) stay in the parallel pool and do NOT set concurrent: false. The label enables 'chainsaw test --selector phase1-failure=true' and the spec.concurrent toggle prevents cross-test races. Per-step cleanup blocks ensure cluster invariants are restored after the test ends regardless of pass/fail.

## Examples

### `tests/e2e/keystone/openbao-sealed/chainsaw-test.yaml:39-52`

```
metadata:
  name: keystone-openbao-sealed
  labels:
    phase1-failure: "true"
spec:
  namespace: openstack
  concurrent: false
  timeouts:
    assert: 5m
```

### `tests/e2e/keystone/eso-down/chainsaw-test.yaml:65-84`

```
metadata:
  name: keystone-eso-down
  labels:
    phase1-failure: "true"
spec:
  namespace: openstack
  concurrent: false
  timeouts:
    assert: 5m
```

