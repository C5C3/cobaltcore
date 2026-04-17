# Pattern: Chaos E2E test suite structure with degradation/recovery and no-regression patterns

**Component**: tests/e2e-chaos/*/
**Category**: testing
**Applies-When**: Adding a new Chaos Mesh-based E2E test suite for pod-kill, network partition, or other fault injection scenarios; Adding a chaos E2E test that needs to confirm pod disruption took effect, especially when the target pod may be deleted and replaced by its controller (ReplicaSet/Deployment) before kubectl wait can observe the transition; Adding a new NetworkChaos-based chaos E2E test that asserts degradation or stability after fault injection

## Description

Each chaos E2E test follows a 6-step structure: (1) Apply Keystone CR, (2) Assert baseline Ready=True (pre-chaos health gate), (3) Apply PodChaos/NetworkChaos CR, (4) Assert expected degradation (sub-condition=False) OR no-regression (all conditions=True after Deployment-level recovery polling), (5) Delete Chaos CR (explicit fault removal), (6) Assert full recovery (sub-condition=True, Ready=True/AllReady). Two test patterns exist: 'degradation and recovery' for critical dependencies (MariaDB, OpenBao) where a sub-condition must transition to False then back to True, and 'no-regression' for non-critical dependencies (Memcached) where all conditions must remain True during the outage — verified by polling the target Deployment's readyReplicas to confirm disruption and recovery before asserting conditions (CC-0076). Each test has a unique CR name (keystone-chaos-{suffix}) and database name (keystone_chaos_{suffix}). Catch blocks on assert steps collect target pod status, Chaos Mesh experiment status, operator/pod logs with --previous, and namespace events.

Instead of using chainsaw wait with kind: Pod and a label selector (which resolves the selector once and watches the specific Pod object — failing with NotFound when the Pod is deleted), poll the parent Deployment's status.readyReplicas via kubectl get with jsonpath. The script has two phases: Phase 1 polls until readyReplicas drops below the desired count (chaos confirmed), Phase 2 polls until readyReplicas returns to the desired count (recovery confirmed). Uses ${READY:-0} defensive defaults for empty kubectl output. Omits set -euo pipefail because the defensive defaults would fail under set -e. Each phase has a bounded for-loop with sleep 2 and an explicit exit 1 on timeout with a descriptive error message. The script step has a timeout (120-150s) as a hard upper bound.

NetworkChaos tests must verify the fault is actually injected (AllInjected=True) before asserting its effects. Unlike PodChaos pod-kill which has immediate visible impact (pod deletion), NetworkChaos CRs can be accepted by the API server without effect if the selector doesn't match target pods. Without the AllInjected gate, a mis-targeted selector causes the degradation assertion (e.g., DatabaseReady=False) to pass vacuously if the condition briefly flickers for an unrelated reason. Two implementation variants exist: (1) a dedicated Chainsaw assert step for degradation-recovery tests where the gate and effect assertion are separate steps, and (2) a kubectl wait inside a script step for no-regression tests where the gate is bundled with stability verification.

## Examples

### `tests/e2e-chaos/mariadb-pod-kill/chainsaw-test.yaml:53-64`

```
  # ── Step 4: Assert DatabaseReady=False after pod kill ──────────────────
  - try:
    - assert:
        resource:
          apiVersion: keystone.openstack.c5c3.io/v1alpha1
          kind: Keystone
          metadata:
            name: keystone-chaos-db
            namespace: openstack
          status:
            (conditions[?type == 'DatabaseReady']):
            - status: "False"
```

### `tests/e2e-chaos/operator-pod-crash/chainsaw-test.yaml:55`

```
    - script:
        timeout: 150s
        content: |
          # NOTE: set -euo pipefail is intentionally omitted. The polling loop
          # uses ${READY:-0} defensive defaults that would fail under set -e
          # when kubectl returns empty output, and the loop's exit condition
          # is checked explicitly rather than relying on exit codes.
          # Phase 1: poll until readyReplicas drops below 2 (chaos took effect).
          for i in $(seq 1 60); do
            READY=$(kubectl get deployment keystone-operator -n default -o jsonpath='{.status.readyReplicas}')
            if [ "${READY:-0}" -lt 2 ]; then
              break
            fi
            sleep 2
          done
          if [ "${READY:-0}" -ge 2 ]; then
            echo "ERROR: readyReplicas never dropped below 2 — pod kill did not take effect"
            exit 1
          fi
          echo "Pod kill confirmed: readyReplicas=${READY:-0}"
          # Phase 2: poll until readyReplicas returns to 2 (operator recovered).
          for i in $(seq 1 60); do
            READY=$(kubectl get deployment keystone-operator -n default -o jsonpath='{.status.readyReplicas}')
            if [ "${READY:-0}" -eq 2 ]; then
              echo "Operator recovered: readyReplicas=2"
              exit 0
            fi
            sleep 2
          done
          echo "ERROR: operator did not recover — readyReplicas=${READY:-0}"
          exit 1
```

### `tests/e2e-chaos/memcached-pod-kill/chainsaw-test.yaml:52`

```
    - script:
        timeout: 120s
        content: |
          # NOTE: set -euo pipefail is intentionally omitted. The polling loop
          # uses ${READY:-0} defensive defaults that would fail under set -e
          # when kubectl returns empty output, and the loop's exit condition
          # is checked explicitly rather than relying on exit codes.
          # Phase 1: poll until readyReplicas drops below 3 (chaos took effect).
          for i in $(seq 1 30); do
            READY=$(kubectl get deployment openstack-memcached -n $NAMESPACE -o jsonpath='{.status.readyReplicas}')
            if [ "${READY:-0}" -lt 3 ]; then
              break
            fi
            sleep 2
          done
          if [ "${READY:-0}" -ge 3 ]; then
            echo "ERROR: readyReplicas never dropped below 3 — pod kill did not take effect"
            exit 1
          fi
          echo "Pod kill confirmed: readyReplicas=${READY:-0}"
          # Phase 2: poll until readyReplicas returns to 3 (memcached recovered).
          for i in $(seq 1 60); do
            READY=$(kubectl get deployment openstack-memcached -n $NAMESPACE -o jsonpath='{.status.readyReplicas}')
            if [ "${READY:-0}" -eq 3 ]; then
              echo "Memcached recovered: readyReplicas=3"
              exit 0
            fi
            sleep 2
          done
          echo "ERROR: memcached did not recover — readyReplicas=${READY:-0}"
          exit 1
```

### `tests/e2e-chaos/api-pod-kill-pdb/chainsaw-test.yaml:72`

```
    - script:
        timeout: 120s
        content: |
          # NOTE: set -euo pipefail is intentionally omitted. The polling loop
          # uses ${READY:-0} defensive defaults that would fail under set -e
          # when kubectl returns empty output, and the loop's exit condition
          # is checked explicitly rather than relying on exit codes.
          # Poll until readyReplicas < 3 (confirms pod kill took effect)
          for i in $(seq 1 60); do
            READY=$(kubectl get deployment keystone-chaos-api-api -n $NAMESPACE -o jsonpath='{.status.readyReplicas}')
            if [ "${READY:-0}" -lt 3 ]; then
              break
            fi
            sleep 2
          done
          # Fail explicitly if the kill never took effect
          if [ "${READY:-0}" -ge 3 ]; then
            echo "ERROR: readyReplicas never dropped below 3 — pod kill did not take effect"
            exit 1
          fi
```
### `tests/e2e-chaos/mariadb-network-partition/chainsaw-test.yaml:48-59`

```
  - try:
    - assert:
        resource:
          apiVersion: chaos-mesh.org/v1alpha1
          kind: NetworkChaos
          metadata:
            name: partition-mariadb
            namespace: openstack
          status:
            conditions:
            - type: AllInjected
              status: "True"
```

### `tests/e2e-chaos/mariadb-network-latency/chainsaw-test.yaml:58-61`

```
          echo "Waiting for NetworkChaos latency-mariadb injection..."
          kubectl wait networkchaos/latency-mariadb -n openstack \
            --for=condition=AllInjected --timeout=60s
          echo "NetworkChaos latency injection confirmed active"
```



