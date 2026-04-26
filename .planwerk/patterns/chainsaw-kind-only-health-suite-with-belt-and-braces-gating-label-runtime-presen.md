# Pattern: Chainsaw kind-only health suite with belt-and-braces gating (label + runtime presence guard)

**Component**: tests/e2e/infrastructure/<addon>-health/
**Category**: testing
**Applies-When**: Adding an E2E health-check suite for a kind-only addon that may be absent on a default cluster

## Description

Each kind-only addon health suite is a single-file Chainsaw Test with two independent gating mechanisms: (1) `metadata.labels.overlay: kind` so non-kind invocations can filter out via `chainsaw test --selector 'overlay!=kind'`, and (2) a runtime `kubectl get ns <addon>` presence guard at the top of a single `script:` step that exits 0 with a SKIP log when the namespace is absent. The script consolidates all assertions (kubectl wait, kubectl rollout status, etc.) into one step because chainsaw has no step-level skip — declarative asserts in sibling steps would time out and fail even after a guard exited 0. The catch block individually scopes each describe so a missing resource does not short-circuit the dump. This pattern was established by flux-web-health (CC-0086) and is now used by chaos-mesh-health (CC-0097).

## Examples

### `tests/e2e/infrastructure/chaos-mesh-health/chainsaw-test.yaml:55-122`

```
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: chaos-mesh-health
  labels:
    overlay: kind
spec:
  timeouts:
    assert: 5m
    exec: 5m
  steps:
  - try:
    - script:
        timeout: 5m
        shell: bash
        content: |
          set -euo pipefail
          if ! kubectl get ns chaos-mesh >/dev/null 2>&1; then
            echo "SKIP: chaos-mesh namespace not present."
            exit 0
          fi
          kubectl wait --for=condition=Available deployment/chaos-controller-manager -n chaos-mesh --timeout=2m
          kubectl rollout status daemonset/chaos-daemon -n chaos-mesh --timeout=2m
```

### `tests/e2e/infrastructure/flux-web-health/chainsaw-test.yaml (sibling)`

```
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: flux-web-health
  labels:
    overlay: kind
# Same belt-and-braces shape: overlay=kind label + runtime presence guard inside the single script step.
```

