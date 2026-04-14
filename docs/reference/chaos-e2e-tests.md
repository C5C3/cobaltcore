---
title: Chaos E2E Test Suites
quadrant: operator
feature: CC-0047, CC-0048, CC-0054
---

# Chaos E2E Test Suites

Reference documentation for the chaos E2E test suites (CC-0047, CC-0048). These tests
verify that OpenStack operators correctly detect infrastructure dependency failures via
status conditions and recover autonomously when dependencies return. Phase 2 (CC-0048)
extends the suite with operator resilience and workload chaos scenarios.

For happy-path E2E tests, see [Keystone E2E Test Suites](./keystone-e2e-tests.md).

## Overview

The 6 chaos test suites validate operator behavior during and after fault injection.
Phase 1 (CC-0047) covers infrastructure dependency pod kills. Phase 2 (CC-0048) adds
operator self-recovery, CronJob workload fault tolerance, and PDB availability guarantee
scenarios. Each suite deploys a Keystone CR, asserts a healthy baseline, injects a
[Chaos Mesh](https://chaos-mesh.org/) `PodChaos` fault, asserts the expected degradation
(or stability), removes the fault, and asserts full recovery. Tests use
[Chainsaw](https://kyverno.github.io/chainsaw/) to orchestrate the assertion lifecycle.

```text
┌──────────────────────────────────────────────────────────────────────────────┐
│  Chainsaw Chaos E2E Runner (parallel: 1)                                     │
│                                                                              │
│  Phase 1 (CC-0047): Dependency Pod Kill                                      │
│  ┌─────────────────────┐  ┌─────────────────────┐  ┌─────────────────────┐  │
│  │ mariadb-pod-kill     │  │ memcached-pod-kill   │  │ openbao-pod-kill   │  │
│  │ SC-CHAOS-001         │  │ SC-CHAOS-002         │  │ SC-CHAOS-003       │  │
│  │ (keystone-chaos-db)  │  │ (keystone-chaos-mc)  │  │ (keystone-chaos-   │  │
│  │                      │  │                      │  │  bao)              │  │
│  │ Pattern: degradation │  │ Pattern: no-         │  │ Pattern:           │  │
│  │ and recovery         │  │ regression           │  │ degradation and    │  │
│  │                      │  │                      │  │ recovery           │  │
│  └─────────────────────┘  └─────────────────────┘  └─────────────────────┘  │
│                                                                              │
│  Phase 2 (CC-0048): Operator Resilience and Workload Chaos                   │
│  ┌─────────────────────┐  ┌─────────────────────┐  ┌─────────────────────┐  │
│  │ operator-pod-crash   │  │ cronjob-rotation-   │  │ api-pod-kill-pdb   │  │
│  │ SC-CHAOS-004         │  │ failure             │  │ SC-CHAOS-008       │  │
│  │ (keystone-chaos-op)  │  │ SC-CHAOS-005        │  │ (keystone-chaos-   │  │
│  │                      │  │ (keystone-chaos-    │  │  api)              │  │
│  │ Pattern: operator    │  │  cron)              │  │                    │  │
│  │ self-recovery        │  │                     │  │ Pattern: PDB       │  │
│  │ (no-regression)      │  │ Pattern: workload   │  │ availability       │  │
│  │                      │  │ fault tolerance     │  │ guarantee          │  │
│  └─────────────────────┘  └─────────────────────┘  └─────────────────────┘  │
│                                                                              │
│  All tests run in: namespace openstack                                       │
│  Fault injection: Chaos Mesh PodChaos CRDs                                   │
│  Infrastructure: MariaDB, Memcached, ESO, OpenBao, Chaos Mesh (pre-deployed) │
└──────────────────────────────────────────────────────────────────────────────┘
```

## Prerequisites

All 6 test suites require the infrastructure stack and Chaos Mesh to be deployed and
healthy.

| Prerequisite | Details |
| --- | --- |
| Infrastructure stack | Deployed via `make deploy-infra` (see [Infrastructure E2E Deployment](./infrastructure/e2e-deployment.md)) |
| Chaos Mesh | Installed in `chaos-mesh` namespace (via FluxCD HelmRelease or `chaos-mesh/chaos-mesh-action`) |
| Keystone operator | Deployed to the cluster with CRDs installed |
| ESO ExternalSecrets | `keystone-admin`, `keystone-db` synced in `openstack` namespace |
| MariaDB instance | `openstack-db` MariaDB CR Ready in `openstack` namespace |
| Memcached instance | `openstack-memcached` Memcached CR Ready in `openstack` namespace |
| OpenBao instance | Running in `openbao-system` namespace |

## Running the Tests

```bash
# Run all chaos E2E tests
make e2e-chaos

# Run with chainsaw directly (equivalent)
chainsaw test --config tests/e2e-chaos/chainsaw-config.yaml tests/e2e-chaos/

# Run a specific scenario
chainsaw test --config tests/e2e-chaos/chainsaw-config.yaml tests/e2e-chaos/mariadb-pod-kill/
```

## Chainsaw Configuration

Chaos tests use a separate configuration at `tests/e2e-chaos/chainsaw-config.yaml` with
settings tuned for fault injection scenarios:

| Setting | Chaos | Happy-Path | Rationale |
| --- | --- | --- | --- |
| `timeouts.assert` | 300s | 120s | Recovery requires multiple reconciliation cycles and pod restart time |
| `timeouts.cleanup` | 120s | 60s | Chaos Mesh CRs may take longer to finalize and release faults |
| `execution.parallel` | 1 | 4 | Chaos tests mutate shared infrastructure pod availability; serial execution prevents cross-test interference |
| `execution.failFast` | true | true | Stop on first failure for faster feedback |
| `report.name` | `chainsaw-chaos-report` | `chainsaw-report` | Distinct JUnit report artifact |

Individual test suites override the assert timeout to 5 minutes (`5m`) at the spec level.

## CI Trigger Policy

Chaos tests run as a separate `e2e-chaos` GitHub Actions job in the CI workflow (CC-0054).
The job is path-filtered and non-blocking while stability is being proven. See
[CI Workflow — e2e-chaos](./ci-workflow.md#e2e-chaos) for full job documentation.

**Path filter (`e2e_chaos`):** Changes to `tests/e2e-chaos/**`, `hack/**`, `deploy/**`,
`.github/workflows/ci.yaml`, or `.github/actions/**` trigger the job. Additionally, any Go code change — whether in
a specific operator (e.g., `operators/keystone/**/*.go`) or in shared code
(`internal/common/**/*.go` via `go_common`) — triggers the job, since chaos tests validate
operator resilience against the current codebase. On `v*` tag pushes, the job is forced
active regardless of which files were touched.

**Trigger conditions:**

| Event | Runs when |
| --- | --- |
| Push to `main` | Path filter matches or Go code changed (always on `v*` tags) |
| Pull request | Path filter matches or Go code changed |

**Dependencies:** The job depends on all gate jobs (`lint`, `shellcheck`, `test`,
`test-integration`, `verify-codegen`) and `e2e-operator`. It only runs if no dependency
failed or was cancelled. A skipped `e2e-operator` does not block the job.

**Non-blocking (`continue-on-error: true`):** Chaos test failures are visible in the CI
status but do not block merges or the publish pipeline (CC-0054 REQ-004). This is
intentional while chaos test stability is being proven in CI. The setting will be revisited
after 2–4 weeks of successful CI runs to consider making the job blocking.

**Timeout:** 60 minutes (vs 45 minutes for `e2e-operator`) to accommodate serial test
execution and longer recovery assertion windows.

## Test Suite Inventory

| Suite | Scenario ID | CR Name | Test Pattern | Condition Assertions | Requirements |
| --- | --- | --- | --- | --- | --- |
| [mariadb-pod-kill](#mariadb-pod-kill) | SC-CHAOS-001 | `keystone-chaos-db` | Degradation and recovery | `DatabaseReady=False` → `DatabaseReady=True`, `Ready=True` | REQ-003, REQ-004, REQ-008, REQ-009 |
| [memcached-pod-kill](#memcached-pod-kill) | SC-CHAOS-002 | `keystone-chaos-mc` | No-regression | All 6 conditions remain `True` during outage | REQ-005, REQ-008, REQ-009 |
| [openbao-pod-kill](#openbao-pod-kill) | SC-CHAOS-003 | `keystone-chaos-bao` | Degradation and recovery | `SecretsReady=False` → `SecretsReady=True`, `Ready=True` | REQ-006, REQ-007, REQ-008, REQ-009 |
| [operator-pod-crash](#operator-pod-crash) | SC-CHAOS-004 | `keystone-chaos-op` | Operator self-recovery (no-regression) | Operator pod `Ready=false` → `Ready=true`, CR `Ready=True` maintained | REQ-001, REQ-005, REQ-006, REQ-008 (CC-0048) |
| [cronjob-rotation-failure](#cronjob-rotation-failure) | SC-CHAOS-005 | `keystone-chaos-cron` | Workload fault tolerance | `FernetKeysReady=True` maintained, `Ready=True` maintained | REQ-002, REQ-006, REQ-008 (CC-0048) |
| [api-pod-kill-pdb](#api-pod-kill-pdb) | SC-CHAOS-008 | `keystone-chaos-api` | PDB availability guarantee | PDB `minAvailable: 1`, `DeploymentReady=True` maintained, `Ready=True` maintained | REQ-003, REQ-004, REQ-006, REQ-008 (CC-0048) |

---

## Test Suite Details

### mariadb-pod-kill

**File:** `tests/e2e-chaos/mariadb-pod-kill/chainsaw-test.yaml`

**Scenario:** SC-CHAOS-001

**Purpose:** Validates that the Keystone operator detects a MariaDB outage, transitions
`DatabaseReady` to `False`, and recovers autonomously when the StatefulSet restarts the
killed pod.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-chaos-db` with database `keystone_chaos_db` |
| 2 | Assert baseline Ready=True | `assert` (5m) | Ready=True with reason AllReady — confirms healthy state before fault injection |
| 3 | Inject PodChaos | `apply` | Applies `01-podchaos.yaml` — PodChaos `kill-mariadb` targeting `app.kubernetes.io/name: mariadb` in `openstack` |
| 4 | Assert degradation | `assert` (5m) | DatabaseReady=False — operator detects MariaDB is unavailable |
| 5 | Delete PodChaos | `delete` | Removes PodChaos `kill-mariadb` to lift the fault |
| 6 | Assert recovery | `assert` (5m) | DatabaseReady=True and Ready=True with reason AllReady |

**Fixtures:** `00-keystone-cr.yaml`, `01-podchaos.yaml`

**Catch blocks:** Steps 2, 4, and 6 include catch blocks dumping Keystone CR status,
MariaDB pod status, Chaos Mesh experiment status, operator logs (including `--previous`
for crash loop detection), and namespace events.

---

### memcached-pod-kill

**File:** `tests/e2e-chaos/memcached-pod-kill/chainsaw-test.yaml`

**Scenario:** SC-CHAOS-002

**Purpose:** Validates that the Keystone operator maintains `Ready=True` when a Memcached
pod is killed. Cache failures are treated as performance degradation only — no sub-condition
should regress.

**Key difference from MariaDB:** Memcached failure should **not** set `Ready=False`. The
test asserts that all 6 conditions remain `True` while Memcached is down.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-chaos-mc` with database `keystone_chaos_mc` |
| 2 | Assert baseline Ready=True | `assert` (5m) | Ready=True with reason AllReady |
| 3 | Inject PodChaos | `apply` | Applies `01-podchaos.yaml` — PodChaos `kill-memcached` targeting `app.kubernetes.io/name: memcached` in `openstack` |
| 4 | Wait for pod recovery and assert no-regression | `wait` (2m) + `assert` (5m) | Waits for Memcached pod Ready=false (chaos took effect), then Ready=true (recovery complete), then asserts all 6 conditions: SecretsReady=True, FernetKeysReady=True, DatabaseReady=True, DeploymentReady=True, BootstrapReady=True, Ready=True (AllReady) |
| 5 | Delete PodChaos | `delete` | Removes PodChaos `kill-memcached` to allow recovery |
| 6 | Assert Ready=True after recovery | `assert` (5m) | Ready=True with reason AllReady |

**Fixtures:** `00-keystone-cr.yaml`, `01-podchaos.yaml`

**Catch blocks:** Steps 2, 4, and 6 include catch blocks dumping Memcached pod status,
Chaos Mesh experiment status, Keystone CR status, pod logs (including `--previous`), and
namespace events.

**Design note:** Step 4 uses condition-based waits (Ready=false then Ready=true) instead
of a fixed sleep. This confirms chaos actually took effect before asserting operator
conditions, and is both faster and more reliable than a fixed delay.

---

### openbao-pod-kill

**File:** `tests/e2e-chaos/openbao-pod-kill/chainsaw-test.yaml`

**Scenario:** SC-CHAOS-003

**Purpose:** Validates that the Keystone operator detects an OpenBao outage via ESO
ExternalSecret sync failures, transitions `SecretsReady` to `False`, and recovers when
OpenBao returns and ESO resumes syncing.

**Cross-namespace targeting:** OpenBao runs in the `openbao-system` namespace, not
`openstack`. The PodChaos CR is created in `openstack` but targets `openbao-system` via
`selector.namespaces`. Chaos Mesh has cluster-wide RBAC enabling this.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-chaos-bao` with database `keystone_chaos_bao` |
| 2 | Assert baseline | `assert` (5m) | SecretsReady=True and Ready=True with reason AllReady |
| 3 | Inject PodChaos | `apply` | Applies `01-podchaos.yaml` — PodChaos `kill-openbao` targeting `app.kubernetes.io/name: openbao` in `openbao-system` |
| 4 | Assert degradation | `assert` (5m) | SecretsReady=False — ESO cannot reach OpenBao |
| 5 | Delete PodChaos | `delete` | Removes PodChaos `kill-openbao` to lift the fault |
| 6 | Assert recovery | `assert` (5m) | SecretsReady=True and Ready=True with reason AllReady |

**Fixtures:** `00-keystone-cr.yaml`, `01-podchaos.yaml`

**Catch blocks:** Steps 2, 4, and 6 include catch blocks dumping Keystone CR status,
OpenBao pod status (in `openbao-system`), Chaos Mesh experiment status, ESO ExternalSecret
conditions (via `jsonpath='{.status.conditions}'`), operator logs (including `--previous`),
and namespace events.

---

### operator-pod-crash

**File:** `tests/e2e-chaos/operator-pod-crash/chainsaw-test.yaml`

**Scenario:** SC-CHAOS-004 (CC-0048)

**Purpose:** Validates that the Keystone operator self-recovers after its own pod is killed
mid-reconcile. The Deployment controller restarts the operator pod, controller-runtime
re-registers watches, and the reconcile loop re-runs all sub-reconcilers idempotently.
The Keystone CR should maintain `Ready=True` throughout because the operator crash is
invisible to the CR's status conditions.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-chaos-op` with database `keystone_chaos_op` |
| 2 | Assert baseline Ready=True | `assert` (5m) | Ready=True with reason AllReady — confirms healthy state before fault injection |
| 3 | Inject PodChaos | `apply` | Applies `01-podchaos.yaml` — PodChaos `kill-operator` targeting `app.kubernetes.io/name: keystone-operator` in `openstack` |
| 4 | Wait for operator pod crash and recovery | `wait` (2m + 2m) | Condition-based waits: operator pod `Ready=false` (kill took effect), then `Ready=true` (Deployment controller restarted pod) |
| 5 | Delete PodChaos | `delete` | Removes PodChaos `kill-operator` to clean up |
| 6 | Assert Ready=True after re-reconciliation | `assert` (5m) | Ready=True with reason AllReady — no sub-condition stuck in False state |

**Fixtures:** `00-keystone-cr.yaml`, `01-podchaos.yaml`

**Catch blocks:** Steps 2 and 6 include catch blocks calling `diagnostics.sh` with
appropriate mode (`baseline`/`chaos`) and `--dep-label=app.kubernetes.io/name=keystone-operator`.
Step 4 includes a catch block with chaos diagnostics for the operator pod.

**Design note:** Step 4 uses condition-based waits on the operator pod (`Ready=false` then
`Ready=true`) instead of a fixed sleep. This confirms the kill actually took effect before
proceeding, and is the same pattern used in SC-CHAOS-002. A theoretical race exists where
the kill-and-restart completes faster than Chainsaw's poll interval — see the inline comment
for mitigation guidance if CI flakiness occurs.

---

### cronjob-rotation-failure

**File:** `tests/e2e-chaos/cronjob-rotation-failure/chainsaw-test.yaml`

**Scenario:** SC-CHAOS-005 (CC-0048)

**Purpose:** Validates that the Keystone operator maintains `FernetKeysReady=True` and
`Ready=True` when a manually triggered fernet rotation Job's pods are killed by PodChaos.
The operator's `reconcileFernetKeys()` checks Secret and CronJob existence — not individual
Job run outcomes — so a failed rotation Job should not degrade the CR status.

**Key difference from dependency kills:** This scenario targets workload pods (Job pods
created by a CronJob) rather than infrastructure dependency pods. The PodChaos CR is applied
**before** the Job is created (Step 3 before Step 4) so Chaos Mesh intercepts the Job's pods
on creation.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-chaos-cron` with database `keystone_chaos_cron` |
| 2 | Assert baseline Ready=True | `assert` (5m) | Ready=True with reason AllReady |
| 3 | Inject PodChaos | `apply` | Applies `01-podchaos.yaml` — PodChaos `fail-cronjob` targeting `job-name: chaos-cron-test` in `openstack` with `pod-failure` action and 60s duration |
| 4 | Trigger fernet rotation Job | `script` | Runs `kubectl create job chaos-cron-test --from=cronjob/keystone-chaos-cron-fernet-rotate` |
| 5 | Assert FernetKeysReady=True and Ready=True maintained | `assert` (5m) | `FernetKeysReady=True` and `Ready=True` with reason AllReady — no condition cascade from CronJob failure |
| 6 | Delete PodChaos | `delete` | Removes PodChaos `fail-cronjob` to lift the fault |
| 7 | Assert Ready=True after cleanup | `assert` (5m) | Ready=True with reason AllReady — CronJob remains correctly configured |

**Fixtures:** `00-keystone-cr.yaml`, `01-podchaos.yaml`

**Catch blocks:** Step 2 calls `diagnostics.sh baseline`. Step 4 catches with CronJob status.
Step 5 catches with Job status, job pod logs (`job-name=chaos-cron-test`), and
`diagnostics.sh chaos`. Step 7 catches with `diagnostics.sh chaos`.

**Design note:** The PodChaos uses `pod-failure` action (not `pod-kill`) with `mode: all`
and 60s duration. This injects sustained failures into all pods matching `job-name=chaos-cron-test`,
simulating a scenario where every rotation attempt fails for the full duration. The `mode: all`
ensures every pod spawned by the targeted Job is affected.

---

### api-pod-kill-pdb

**File:** `tests/e2e-chaos/api-pod-kill-pdb/chainsaw-test.yaml`

**Scenario:** SC-CHAOS-008 (CC-0048)

**Purpose:** Validates that the Keystone operator creates a PodDisruptionBudget with
`minAvailable: 1` for Keystone API pods when `replicas > 1`, and that at least one API
pod remains available during a pod kill. The Keystone CR should maintain
`DeploymentReady=True` and `Ready=True` throughout the disruption.

**Key difference from other pod kills:** This scenario kills a Keystone API pod (managed
by the operator itself) rather than an external dependency pod. It uses `replicas: 3` in
the CR to trigger PDB creation via `buildPodDisruptionBudget()`, and includes an explicit
PDB assertion step and a script-based availability check.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR (replicas: 3) | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-chaos-api` with database `keystone_chaos_api`, replicas: 3 |
| 2 | Assert baseline Ready=True | `assert` (5m) | Ready=True with reason AllReady |
| 3 | Assert PDB exists | `assert` (5m) | PDB `keystone-chaos-api-api` with `spec.minAvailable: 1` (apiVersion: `policy/v1`) |
| 4 | Inject PodChaos | `apply` | Applies `01-podchaos.yaml` — PodChaos `kill-keystone-api` targeting `app.kubernetes.io/name: keystone` AND `app.kubernetes.io/instance: keystone-chaos-api` in `openstack` |
| 5 | Verify PDB enforcement and assert conditions | `script` (120s) + `assert` (5m) | Script polls until `readyReplicas < 3` (kill took effect), then asserts `availableReplicas >= 1`; Chainsaw asserts `DeploymentReady=True` and `Ready=True` with reason AllReady |
| 6 | Delete PodChaos | `delete` | Removes PodChaos `kill-keystone-api` to clean up |
| 7 | Assert Ready=True after recovery | `assert` (5m) | Ready=True with reason AllReady — full replica count restored |

**Fixtures:** `00-keystone-cr.yaml`, `01-podchaos.yaml`

**Catch blocks:** Step 2 calls `diagnostics.sh baseline`. Step 3 catches with PDB describe.
Steps 5 and 7 catch with `diagnostics.sh chaos` using
`--dep-label=app.kubernetes.io/name=keystone,app.kubernetes.io/instance=keystone-chaos-api`.

**Design note:** Step 5 uses a script step to poll Deployment status because Chainsaw's
`wait` with a label selector waits for ALL matching pods, which does not work when the goal
is to verify that NOT ALL pods are down. The script polls `readyReplicas < 3` (confirming
the kill took effect) then asserts `availableReplicas >= 1`. The PDB name follows the
naming convention `apiResourceName(keystone)` = `{cr-name}-api`, so for CR `keystone-chaos-api`,
the PDB is `keystone-chaos-api-api`.

---

## Test Patterns

### Degradation and Recovery (SC-CHAOS-001, SC-CHAOS-003)

Used when the killed dependency is critical and the operator must detect the outage via a
sub-condition transition.

```text
Apply CR → Assert Ready=True → Inject PodChaos → Assert SubCondition=False
         → Delete PodChaos → Assert SubCondition=True + Ready=True
```

1. Apply Keystone CR and assert `Ready=True` (baseline)
2. Apply PodChaos to kill a critical dependency pod
3. Assert the corresponding sub-condition transitions to `False`
4. Delete PodChaos to lift the fault (pod restarts via StatefulSet/Deployment controller)
5. Assert full recovery: sub-condition returns to `True`, `Ready=True`

### No-Regression (SC-CHAOS-002)

Used when the killed dependency is non-critical and the operator must maintain `Ready=True`
despite the outage.

```text
Apply CR → Assert Ready=True → Inject PodChaos → Wait Pod Ready=false → Wait Pod Ready=true
         → Assert ALL conditions=True → Delete PodChaos → Assert Ready=True
```

1. Apply Keystone CR and assert `Ready=True` (baseline)
2. Apply PodChaos to kill a non-critical dependency pod
3. Wait for pod to become NotReady (confirms chaos took effect)
4. Wait for pod to return to Ready (confirms recovery)
5. Assert **all 6 conditions** remain `True` — no regression
6. Delete PodChaos and assert `Ready=True` after recovery

### Operator Self-Recovery (SC-CHAOS-004)

Used when the operator's own pod is killed and the Deployment controller restarts it.
The CR conditions should remain stable because the operator crash is invisible to the
Keystone CR — the Deployment controller handles pod restart, and controller-runtime
re-registers watches and resumes reconciliation.

```text
Apply CR → Assert Ready=True → Inject PodChaos → Wait Operator Pod Ready=false
         → Wait Operator Pod Ready=true → Delete PodChaos → Assert Ready=True
```

1. Apply Keystone CR and assert `Ready=True` (baseline)
2. Apply PodChaos to kill the operator pod
3. Wait for operator pod `Ready=false` (confirms kill took effect)
4. Wait for operator pod `Ready=true` (Deployment controller restarted it)
5. Delete PodChaos and assert `Ready=True` after re-reconciliation

### Workload Fault Tolerance (SC-CHAOS-005)

Used when a workload spawned by the operator (CronJob/Job) fails but the operator should
remain healthy because it checks resource existence rather than Job run outcomes.

```text
Apply CR → Assert Ready=True → Inject PodChaos (pod-failure) → Trigger Job
         → Assert conditions maintained → Delete PodChaos → Assert Ready=True
```

1. Apply Keystone CR and assert `Ready=True` (baseline)
2. Apply PodChaos with `pod-failure` action **before** creating the Job
3. Create a manual Job from the CronJob (triggers fault injection on Job pods)
4. Assert `FernetKeysReady=True` and `Ready=True` — no condition cascade
5. Delete PodChaos and assert `Ready=True` after cleanup

### PDB Availability Guarantee (SC-CHAOS-008)

Used when the operator creates a PodDisruptionBudget and the test verifies that minimum
availability is maintained during a pod kill. Requires `replicas > 1` to trigger PDB
creation.

```text
Apply CR (replicas: 3) → Assert Ready=True → Assert PDB minAvailable=1
         → Inject PodChaos → Verify availableReplicas >= 1
         → Assert DeploymentReady=True + Ready=True → Delete PodChaos → Assert Ready=True
```

1. Apply Keystone CR with `replicas: 3` and assert `Ready=True` (baseline)
2. Assert PDB exists with `minAvailable: 1`
3. Apply PodChaos to kill one API pod (`mode: one`)
4. Poll until `readyReplicas < 3` (kill took effect), assert `availableReplicas >= 1`
5. Assert `DeploymentReady=True` and `Ready=True` — no condition regression
6. Delete PodChaos and assert `Ready=True` after full replica count restored

## PodChaos CRD Pattern

Phase 1 scenarios (SC-CHAOS-001 through SC-CHAOS-003) and SC-CHAOS-004/SC-CHAOS-008 use
the `pod-kill` action. SC-CHAOS-005 uses `pod-failure` for sustained fault injection.

### Standard pod-kill pattern

```yaml
apiVersion: chaos-mesh.org/v1alpha1
kind: PodChaos
metadata:
  name: kill-<target>
  namespace: openstack
spec:
  action: pod-kill
  mode: one
  selector:
    namespaces:
    - <target-namespace>         # openstack or openbao-system
    labelSelectors:
      app.kubernetes.io/name: <target>  # mariadb, memcached, openbao
  gracePeriod: 0
```

| Field | Value | Rationale |
| --- | --- | --- |
| `action` | `pod-kill` | One-shot kill — no `duration` needed |
| `mode` | `one` | Kills exactly one matching pod |
| `gracePeriod` | `0` | Immediate kill (no graceful shutdown) |
| `namespace` | `openstack` | CR lives in `openstack` even for cross-namespace targeting |
| `selector.namespaces` | varies | `openstack` for MariaDB/Memcached, `openbao-system` for OpenBao |

### pod-failure action (SC-CHAOS-005)

SC-CHAOS-005 uses `pod-failure` instead of `pod-kill` to inject sustained failures into
Job pods for a configurable `duration`. This simulates a scenario where every rotation
attempt fails continuously rather than a single kill-and-restart cycle.

```yaml
spec:
  action: pod-failure
  mode: all
  duration: "60s"
  selector:
    labelSelectors:
      job-name: chaos-cron-test     # Kubernetes auto-label on Job pods
```

| Field | Value | Rationale |
| --- | --- | --- |
| `action` | `pod-failure` | Sustained failure for the full duration (not one-shot kill) |
| `mode` | `all` | Every pod spawned by the targeted Job is affected |
| `duration` | `60s` | Failure window long enough to span at least one reconciliation cycle |
| `selector.labelSelectors` | `job-name: chaos-cron-test` | Targets pods created by the manual Job (Kubernetes auto-assigns this label) |

### Multi-label selector (SC-CHAOS-008)

SC-CHAOS-008 uses two label selectors to target only the Keystone API pods belonging to a
specific CR instance, avoiding interference with other Keystone deployments in the namespace.

```yaml
spec:
  action: pod-kill
  mode: one
  selector:
    labelSelectors:
      app.kubernetes.io/name: keystone               # service type
      app.kubernetes.io/instance: keystone-chaos-api  # CR instance
```

Both labels must match for a pod to be selected. This ensures only the `keystone-chaos-api`
API Deployment's pods are targeted, not the operator pod or API pods from other CR instances.

### Fault cleanup

The test explicitly deletes the PodChaos CR before asserting recovery. This ensures the
fault is lifted before the recovery assertion window begins.

## Keystone CR Fixtures

Each scenario uses a unique CR name and database name to enable parallel execution:

| Scenario | CR Name | Database | Replicas |
| --- | --- | --- | --- |
| SC-CHAOS-001 | `keystone-chaos-db` | `keystone_chaos_db` | 1 |
| SC-CHAOS-002 | `keystone-chaos-mc` | `keystone_chaos_mc` | 1 |
| SC-CHAOS-003 | `keystone-chaos-bao` | `keystone_chaos_bao` | 1 |
| SC-CHAOS-004 | `keystone-chaos-op` | `keystone_chaos_op` | 1 |
| SC-CHAOS-005 | `keystone-chaos-cron` | `keystone_chaos_cron` | 1 |
| SC-CHAOS-008 | `keystone-chaos-api` | `keystone_chaos_api` | **3** |

All fixtures share the same base spec: `clusterRef` for database and memcached, fernet
rotation `"0 0 * * 0"` with `maxActiveKeys: 3`, bootstrap `adminUser: admin`. Most use
`replicas: 1`. SC-CHAOS-008 uses `replicas: 3` to trigger PDB creation via
`buildPodDisruptionBudget()` which requires `replicas > 1`.

## Catch Block Diagnostics

Every assert step includes a `catch:` block that collects diagnostic information when the
assertion fails. The information collected varies by scenario:

| Diagnostic | MariaDB (001) | Memcached (002) | OpenBao (003) | Operator (004) | CronJob (005) | API PDB (008) |
| --- | --- | --- | --- | --- | --- | --- |
| `diagnostics.sh` | Steps 2, 4, 6 | Steps 2, 4, 6 | Steps 2, 4, 6 | Steps 2, 4, 6 | Steps 2, 5, 7 | Steps 2, 5, 7 |
| Target pod status | Steps 4, 6 | Steps 4, 6 | Steps 2, 4, 6 | — | — | — |
| Chaos Mesh experiment status | Steps 4, 6 | Steps 4, 6 | Steps 4, 6 | — | — | — |
| Operator logs (`--previous`) | Steps 4, 6 | — | Steps 4, 6 | — | — | — |
| Target pod logs (`--previous`) | — | Steps 4, 6 | — | — | — | — |
| ESO ExternalSecret conditions | — | — | Steps 4, 6 | — | — | — |
| All pod logs | Step 2 | Step 2 | Step 2 | — | — | — |
| Namespace events | Steps 2, 4, 6 | Steps 2, 4, 6 | Steps 2, 4, 6 | — | — | — |
| CronJob/Job status | — | — | — | — | Steps 4, 5 | — |
| Job pod logs | — | — | — | — | Step 5 | — |
| PDB describe | — | — | — | — | — | Step 3 |

Phase 2 scenarios (004, 005, 008) use `diagnostics.sh` exclusively for catch diagnostics
(which internally collects CR status, pod status, logs, and events). Phase 1 scenarios
(001, 002, 003) use inline `kubectl` commands in catch blocks. SC-CHAOS-005 additionally
collects CronJob/Job-specific diagnostics in Steps 4 and 5.

## File Layout

```text
tests/e2e-chaos/
├── chainsaw-config.yaml              Chaos-specific Chainsaw configuration (CC-0047)
├── diagnostics.sh                    Shared diagnostic collection script
├── README.md                         Quick-start documentation
├── mariadb-pod-kill/                 SC-CHAOS-001: MariaDB pod kill
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-db)
│   ├── 01-podchaos.yaml              PodChaos targeting mariadb in openstack
│   └── chainsaw-test.yaml            Test: DatabaseReady=False → recovery
├── memcached-pod-kill/               SC-CHAOS-002: Memcached pod kill
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-mc)
│   ├── 01-podchaos.yaml              PodChaos targeting memcached in openstack
│   └── chainsaw-test.yaml            Test: Ready=True maintained (no regression)
├── openbao-pod-kill/                 SC-CHAOS-003: OpenBao pod kill
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-bao)
│   ├── 01-podchaos.yaml              PodChaos targeting openbao in openbao-system
│   └── chainsaw-test.yaml            Test: SecretsReady=False → recovery
├── operator-pod-crash/               SC-CHAOS-004: Operator self-recovery (CC-0048)
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-op)
│   ├── 01-podchaos.yaml              PodChaos targeting keystone-operator in openstack
│   └── chainsaw-test.yaml            Test: Ready=True maintained after operator restart
├── cronjob-rotation-failure/         SC-CHAOS-005: CronJob fault tolerance (CC-0048)
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-cron)
│   ├── 01-podchaos.yaml              PodChaos pod-failure targeting job pods
│   └── chainsaw-test.yaml            Test: FernetKeysReady=True maintained
└── api-pod-kill-pdb/                 SC-CHAOS-008: PDB availability guarantee (CC-0048)
    ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-api, replicas: 3)
    ├── 01-podchaos.yaml              PodChaos targeting keystone API pods (multi-label)
    └── chainsaw-test.yaml            Test: PDB minAvailable=1, availability maintained
```

## Adding New Scenarios

1. Create a new directory: `tests/e2e-chaos/<target>-<fault-type>/`
2. Add `00-keystone-cr.yaml` with a unique CR name and database name
3. Add `01-<chaos-type>.yaml` with the appropriate Chaos Mesh CRD
4. Add `chainsaw-test.yaml` following the degradation/recovery or no-regression pattern
5. Include catch blocks with diagnostic output on every assert step
6. All files must include the `SPDX-License-Identifier: Apache-2.0` header

## Related Resources
- [Keystone E2E Test Suites](./keystone-e2e-tests.md) — Happy-path E2E tests (CC-0016)
- [Keystone Reconciler Architecture](./keystone-reconciler.md) — Sub-reconciler contracts and condition semantics (CC-0013, CC-0015)
- [Infrastructure E2E Deployment](./infrastructure/e2e-deployment.md) — Infrastructure stack deployment (CC-0010)
- `tests/e2e-chaos/chainsaw-config.yaml` — Chaos-specific Chainsaw configuration
- `tests/e2e-chaos/README.md` — Quick-start guide
