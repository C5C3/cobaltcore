# Pattern: Chainsaw E2E test suite structure with JMESPath condition assertions

**Component**: tests/e2e/*/
**Category**: testing
**Applies-When**: Adding a new Chainsaw E2E test suite for an operator or infrastructure component

## Description

Each Chainsaw E2E test file follows a fixed structure: (1) SPDX Apache-2.0 header, (2) descriptive comment block listing what the test validates and referencing requirement IDs with feature ID (e.g., REQ-001, REQ-012 (CC-0016)), (3) apiVersion chainsaw.kyverno.io/v1alpha1 kind Test, (4) spec with namespace (for namespaced tests) and timeouts.assert: 5m (overriding the global 120s for full reconciliation cycles), (5) numbered steps with '# ── Step N: Description ──' comment headers, each containing a try block. Condition assertions use JMESPath filter syntax (conditions[?type == 'X']) with string status values ('True'/'False') and reason strings. Numeric comparisons use CEL backtick-quoted literals (e.g., (availableReplicas > `0`)). Error assertions verify resource non-existence via the error step. Script steps use 'set -euo pipefail' for multi-command scripts and include explicit timeout overrides for long-running operations. Each test suite uses a unique CR name (e.g., keystone-basic, keystone-scale) to enable parallel execution. Fixture files use 00-/01-/02- numeric prefix ordering.

## Examples

### `tests/e2e/keystone/basic-deployment/chainsaw-test.yaml:15-55`

```
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: keystone-basic-deployment
spec:
  namespace: openstack
  timeouts:
    assert: 5m
  steps:
  # ── Step 1: Apply Keystone CR ──────────────────────────────────────────
  - try:
    - apply:
        file: 00-keystone-cr.yaml
  # ── Step 2: Assert all sub-conditions and Ready ────────────────────────
  - try:
    - assert:
        resource:
          apiVersion: keystone.openstack.c5c3.io/v1alpha1
          kind: Keystone
          metadata:
            name: keystone-basic
            namespace: openstack
          status:
            (conditions[?type == 'SecretsReady']):
            - status: "True"
              reason: SecretsAvailable
            (conditions[?type == 'Ready']):
            - status: "True"
              reason: AllReady
```

### `tests/e2e/infrastructure/infra-stack-health/chainsaw-test.yaml:17-37`

```
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: infra-stack-health
spec:
  timeouts:
    assert: 5m
  steps:
  # ── Step 1: Operator Deployments ────────────────────────────────────────
  - try:
    - assert:
        resource:
          apiVersion: apps/v1
          kind: Deployment
          metadata:
            name: cert-manager
            namespace: cert-manager
          status:
            (availableReplicas > `0`): true
```

