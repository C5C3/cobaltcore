# Pattern: Chaos E2E test suite structure with degradation/recovery and no-regression patterns

**Component**: tests/e2e-chaos/*/
**Category**: testing
**Applies-When**: Adding a new Chaos Mesh-based E2E test suite for pod-kill, network partition, or other fault injection scenarios

## Description

Each chaos E2E test follows a 6-step structure: (1) Apply Keystone CR, (2) Assert baseline Ready=True (pre-chaos health gate), (3) Apply PodChaos/NetworkChaos CR, (4) Assert expected degradation (sub-condition=False) OR no-regression (all conditions=True after sleep), (5) Delete Chaos CR (explicit fault removal), (6) Assert full recovery (sub-condition=True, Ready=True/AllReady). Two test patterns exist: 'degradation and recovery' for critical dependencies (MariaDB, OpenBao) where a sub-condition must transition to False then back to True, and 'no-regression' for non-critical dependencies (Memcached) where all conditions must remain True during the outage with a 30s sleep before assertion to ensure at least one reconciliation cycle. Each test has a unique CR name (keystone-chaos-{suffix}) and database name (keystone_chaos_{suffix}). Catch blocks on assert steps collect target pod status, Chaos Mesh experiment status, operator/pod logs with --previous, and namespace events.

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

### `tests/e2e-chaos/memcached-pod-kill/chainsaw-test.yaml:53-77`

```
  # ── Step 4: Wait for reconciliation and assert Ready=True maintained ───
  - try:
    - sleep:
        duration: 30s
    - assert:
        resource:
          apiVersion: keystone.openstack.c5c3.io/v1alpha1
          kind: Keystone
          metadata:
            name: keystone-chaos-mc
            namespace: openstack
          status:
            (conditions[?type == 'SecretsReady']):
            - status: "True"
            (conditions[?type == 'FernetKeysReady']):
            - status: "True"
            (conditions[?type == 'DatabaseReady']):
            - status: "True"
            (conditions[?type == 'DeploymentReady']):
            - status: "True"
            (conditions[?type == 'BootstrapReady']):
            - status: "True"
            (conditions[?type == 'Ready']):
            - status: "True"
              reason: AllReady
```

