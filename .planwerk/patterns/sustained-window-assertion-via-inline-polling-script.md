# Pattern: Sustained-window assertion via inline polling script

**Component**: tests/e2e/keystone/<test>/chainsaw-test.yaml
**Category**: testing
**Applies-When**: Asserting that a Kubernetes condition holds CONTINUOUSLY for a window (not just at one point in time) — e.g., proving a controller does not flicker Ready=True between False readings

## Description

When a test must prove a state holds continuously over a window (REQ-style 'sustained for >=60s'), a pure chainsaw assert is insufficient because chainsaw stops polling on the first match. Combine: (1) a chainsaw assert as the convergence gate (waits for the first matching state), then (2) an inline polling script that takes N evenly-spaced samples over T seconds and fails on the first sample that does NOT match the expected state. Sample interval should land inside or just after the controller's reconcile cycle so a flicker of any duration has high probability of being captured. The script also enforces that at least the required wall-clock time has elapsed (guards against clock skew or short sleep).

## Examples

### `tests/e2e/keystone/eso-down/chainsaw-test.yaml:232-274`

```
  - name: sustained-ready-false
    try:
    - script:
        timeout: 90s
        content: |
          set -euo pipefail
          SAMPLES=6
          INTERVAL=11
          for i in $(seq 1 "${SAMPLES}"); do
            ready_status=$(kubectl get keystone "${CR_NAME}" -n "${NS}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
            if [[ "${ready_status}" != "False" ]]; then
              echo "FAIL: aggregate Ready flipped to '${ready_status}' at sample ${i}"
              exit 1
            fi
            if [[ "${i}" -lt "${SAMPLES}" ]]; then sleep "${INTERVAL}"; fi
          done
```

