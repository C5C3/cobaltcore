---
title: Chaos E2E Test Suites
quadrant: operator
feature: CC-0047, CC-0048, CC-0049, CC-0054, CC-0066
---

# Chaos E2E Test Suites

Reference documentation for the chaos E2E test suites (CC-0047, CC-0048, CC-0049, CC-0066). These
tests verify that OpenStack operators correctly detect infrastructure dependency failures
via status conditions and recover autonomously when dependencies return. Phase 2 (CC-0048)
extends the suite with operator resilience and workload chaos scenarios. Phase 3 (CC-0066)
adds operator pod kill with leader re-election and post-failover reconciliation verification.

For happy-path E2E tests, see [Keystone E2E Test Suites](./keystone-e2e-tests.md).

## Overview

The 9 chaos test suites validate operator behavior during and after fault injection.
Phase 1 (CC-0047) covers infrastructure dependency pod kills. Phase 2 (CC-0048) adds
operator self-recovery, CronJob workload fault tolerance, and PDB availability guarantee
scenarios. Phase 3 (CC-0066) adds an all-pod operator kill with leader re-election
verification. Phase 4 (CC-0049) adds network chaos scenarios (partition and latency).
Each suite deploys a Keystone CR, asserts a healthy baseline, injects a
[Chaos Mesh](https://chaos-mesh.org/) `PodChaos` or `NetworkChaos` fault, asserts the expected degradation
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
│  Phase 3 (CC-0066): Concurrent Conflicts and Failover                        │
│  ┌─────────────────────┐                                                     │
│  │ operator-pod-kill    │                                                     │
│  │ SC-CHAOS-009         │                                                     │
│  │ (keystone-chaos-opk) │                                                     │
│  │                      │                                                     │
│  │ Pattern: operator    │                                                     │
│  │ pod kill (all) with  │                                                     │
│  │ failover reconcile   │                                                     │
│  └─────────────────────┘                                                     │
│                                                                              │
│  Phase 4 (CC-0049): Network Chaos                                            │
│  ┌─────────────────────┐  ┌─────────────────────┐                            │
│  │ mariadb-network-     │  │ mariadb-network-     │                            │
│  │ partition            │  │ latency              │                            │
│  │ SC-CHAOS-006         │  │ SC-CHAOS-007         │                            │
│  │ (keystone-chaos-     │  │ (keystone-chaos-     │                            │
│  │  net-part)           │  │  net-lat)            │                            │
│  │                      │  │                      │                            │
│  │ Pattern: degradation │  │ Pattern: latency     │                            │
│  │ and recovery         │  │ tolerance            │                            │
│  │ (NetworkChaos)       │  │ (no-regression)      │                            │
│  └─────────────────────┘  └─────────────────────┘                            │
│                                                                              │
│  All tests run in: namespace openstack                                       │
│  Fault injection: Chaos Mesh PodChaos and NetworkChaos CRDs                  │
│  Infrastructure: MariaDB, Memcached, ESO, OpenBao, Chaos Mesh (pre-deployed) │
└──────────────────────────────────────────────────────────────────────────────┘
```

## Prerequisites

All 9 test suites require the infrastructure stack and Chaos Mesh to be deployed and
healthy.

::: warning Run `WITH_CHAOS_MESH=true make deploy-infra` first (CC-0097)
Chaos Mesh is **opt-in** in the kind Quick Start — the default `make deploy-infra`
flow leaves the `chaos-mesh` namespace absent. Run
`WITH_CHAOS_MESH=true make deploy-infra` before `make e2e-chaos`, or `make e2e-chaos`
will fail its preflight check (`chaos-mesh is not installed`). See the
[Enabling Chaos Mesh tip in Quick Start](../../quick-start.md#step-3-deploy-the-infrastructure-stack)
for the rationale.
:::

| Prerequisite | Details |
| --- | --- |
| Infrastructure stack | Deployed via `WITH_CHAOS_MESH=true make deploy-infra` (CC-0097 — opt-in path; see [Infrastructure E2E Deployment](../infrastructure/e2e-deployment.md)) |
| Chaos Mesh | Installed in `chaos-mesh` namespace by the kind-only opt-in overlay at `deploy/kind/chaos-mesh/` (or by `chaos-mesh/chaos-mesh-action` in CI) |
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
[CI Workflow — e2e-chaos](../ci-cd/ci-workflow.md#e2e-chaos) for full job documentation.

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
| Pull request | Path filter matches, Go code changed, **or `run-chaos` label present** (CC-0049 REQ-007) |

**Dependencies:** The job depends on all gate jobs (`lint`, `shellcheck`, `test`,
`test-integration`, `verify-codegen`). It only runs if no dependency failed or was
cancelled.

**Non-blocking (`continue-on-error: true`):** Chaos test failures are visible in the CI
status but do not block merges or the publish pipeline (CC-0054 REQ-004). This is
intentional while chaos test stability is being proven in CI. The setting will be revisited
after 2–4 weeks of successful CI runs to consider making the job blocking.

**Timeout:** 45 minutes to accommodate serial test execution and longer recovery
assertion windows.

## Test Suite Inventory

| Suite | Scenario ID | CR Name | Test Pattern | Condition Assertions | Requirements |
| --- | --- | --- | --- | --- | --- |
| [mariadb-pod-kill](#mariadb-pod-kill) | SC-CHAOS-001 | `keystone-chaos-db` | Degradation and recovery | `DatabaseReady=False` → `DatabaseReady=True`, `Ready=True` | REQ-003, REQ-004, REQ-008, REQ-009 |
| [memcached-pod-kill](#memcached-pod-kill) | SC-CHAOS-002 | `keystone-chaos-mc` | No-regression | All 6 conditions remain `True` during outage | REQ-005, REQ-008, REQ-009 |
| [openbao-pod-kill](#openbao-pod-kill) | SC-CHAOS-003 | `keystone-chaos-bao` | Degradation and recovery | `SecretsReady=False` → `SecretsReady=True`, `Ready=True` | REQ-006, REQ-007, REQ-008, REQ-009 |
| [operator-pod-crash](#operator-pod-crash) | SC-CHAOS-004 | `keystone-chaos-op` | Operator self-recovery (no-regression) | Operator pod `Ready=false` → `Ready=true`, CR `Ready=True` maintained | REQ-001, REQ-005, REQ-006, REQ-008 (CC-0048) |
| [cronjob-rotation-failure](#cronjob-rotation-failure) | SC-CHAOS-005 | `keystone-chaos-cron` | Workload fault tolerance | `FernetKeysReady=True` maintained, `Ready=True` maintained | REQ-002, REQ-006, REQ-008 (CC-0048) |
| [mariadb-network-partition](#mariadb-network-partition) | SC-CHAOS-006 | `keystone-chaos-net-part` | Degradation and recovery (NetworkChaos) | `DatabaseReady=False` → `DatabaseReady=True`, `Ready=True` | REQ-001, REQ-002, REQ-008 (CC-0049) |
| [mariadb-network-latency](#mariadb-network-latency) | SC-CHAOS-007 | `keystone-chaos-net-lat` | Latency tolerance (no-regression) | `Ready=True` maintained, operator `restartCount=0` | REQ-003, REQ-004, REQ-008 (CC-0049) |
| [api-pod-kill-pdb](#api-pod-kill-pdb) | SC-CHAOS-008 | `keystone-chaos-api` | PDB availability guarantee | PDB `minAvailable: 1`, `DeploymentReady=True` maintained, `Ready=True` maintained | REQ-003, REQ-004, REQ-006, REQ-008 (CC-0048) |
| [operator-pod-kill](#operator-pod-kill) | SC-CHAOS-009 | `keystone-chaos-opk` | Operator pod kill (all) with failover reconciliation | All 6 conditions `True` maintained, replica patch reconciled by new leader | REQ-005, REQ-006, REQ-007, REQ-008 (CC-0066) |

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
| 4 | Verify chaos effect and assert no-regression | `script` (150s) + `assert` (5m) | Reads desired replica count from Deployment `.spec.replicas`, polls Memcached Deployment `readyReplicas` to confirm chaos took effect (drop below desired replicas) and recovery completed (return to desired replicas), then asserts all 6 conditions: SecretsReady=True, FernetKeysReady=True, DatabaseReady=True, DeploymentReady=True, BootstrapReady=True, Ready=True (AllReady) |
| 5 | Delete PodChaos | `delete` | Removes PodChaos `kill-memcached` to allow recovery |
| 6 | Assert Ready=True after recovery | `assert` (5m) | Ready=True with reason AllReady |

**Fixtures:** `00-keystone-cr.yaml`, `01-podchaos.yaml`

**Catch blocks:** Steps 2, 4, and 6 include catch blocks dumping Memcached pod status,
Chaos Mesh experiment status, Keystone CR status, pod logs (including `--previous`), and
namespace events.

**Design note:** Step 4 uses Deployment-level `readyReplicas` polling instead of Pod-level
`kubectl wait` with label selectors. The original `kubectl wait` approach raced with pod
deletion — when Chaos Mesh deletes a pod, the watch errors with `NotFound` because it
resolves the label selector once and watches the specific pod object rather than
re-resolving onto the replacement pod (CC-0076, issue #214). Deployment-level polling
watches the persistent Deployment object and is resilient to pod replacements, matching
the pattern used by `operator-pod-crash` and `api-pod-kill-pdb`.

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

### mariadb-network-partition

**File:** `tests/e2e-chaos/mariadb-network-partition/chainsaw-test.yaml`

**Scenario:** SC-CHAOS-006 (CC-0049)

**Purpose:** Validates that the Keystone operator detects a MariaDB network partition
(unidirectional traffic block from keystone to mariadb), transitions `DatabaseReady` to
`False`, and recovers autonomously when the partition is lifted by deleting the
NetworkChaos CR.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-chaos-net-part` with database `keystone_chaos_net_part` |
| 2 | Assert baseline Ready=True | `assert` (5m) | Ready=True with reason AllReady — confirms healthy state before fault injection |
| 3 | Inject NetworkChaos | `apply` | Applies `01-networkchaos.yaml` — NetworkChaos `partition-mariadb` partitioning keystone→mariadb traffic in `openstack` |
| 4 | Assert NetworkChaos injection active | `assert` (5m) | NetworkChaos `partition-mariadb` has `AllInjected=True` — confirms fault is active before checking effects |
| 5 | Assert degradation | `assert` (5m) | DatabaseReady=False — operator detects MariaDB is unreachable via network partition |
| 6 | Delete NetworkChaos | `delete` | Removes NetworkChaos `partition-mariadb` to lift the partition |
| 7 | Assert recovery | `assert` (5m) | DatabaseReady=True and Ready=True with reason AllReady |

**Fixtures:** `00-keystone-cr.yaml`, `01-networkchaos.yaml`

**Catch blocks:** Steps 2, 4, 5, and 7 include catch blocks. Step 4 dumps the NetworkChaos
CR status for injection diagnosis. All catch blocks use `diagnostics.sh` with appropriate
mode (`baseline`/`chaos`) and `--dep-label=app.kubernetes.io/name=mariadb`.

**Design notes:**

- Uses `NetworkChaos` with `action: partition` instead of `PodChaos` with `action: pod-kill`.
  The partition is unidirectional (keystone→mariadb) via `direction: to`, simulating a
  network failure without killing the MariaDB pod.
- `duration: 300s` acts as a safety net for auto-expiry if the test does not explicitly
  delete the CR. The duration matches the Chainsaw assert timeout (5m/300s) to ensure the
  partition persists through the full assertion window.
- Step 4 verifies `AllInjected=True` before checking `DatabaseReady=False` to prevent
  vacuous test passes when the Chaos Mesh selector doesn't match.

---

### mariadb-network-latency

**File:** `tests/e2e-chaos/mariadb-network-latency/chainsaw-test.yaml`

**Scenario:** SC-CHAOS-007 (CC-0049)

**Purpose:** Validates that the Keystone operator tolerates slow MariaDB responses (10s
latency, 2s jitter) without crash-looping or losing Ready status, confirming adequate
timeout configuration in the operator's database client.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-chaos-net-lat` with database `keystone_chaos_net_lat` |
| 2 | Assert baseline Ready=True | `assert` (5m) | Ready=True with reason AllReady — confirms healthy state before fault injection |
| 3 | Inject NetworkChaos | `apply` | Applies `01-networkchaos.yaml` — NetworkChaos `latency-mariadb` injecting 10s latency with 2s jitter on keystone→mariadb traffic |
| 4 | Assert operator tolerates latency | `script` (120s) + `assert` (5m) | Waits for NetworkChaos `AllInjected=True` via `kubectl wait`, allows one reconciliation cycle (15s), then verifies operator pod `restartCount` remains 0 for all pods by container name (`manager`); asserts Ready=True maintained |
| 5 | Delete NetworkChaos | `delete` | Removes NetworkChaos `latency-mariadb` to lift the latency |
| 6 | Assert Ready=True persists | `assert` (5m) | Ready=True with reason AllReady — confirms no delayed degradation after latency removal |

**Fixtures:** `00-keystone-cr.yaml`, `01-networkchaos.yaml`

**Catch blocks:** Steps 2, 4, and 6 include catch blocks using `diagnostics.sh` with
appropriate mode (`baseline`/`chaos`) and `--dep-label=app.kubernetes.io/name=mariadb`.

**Design notes:**

- Uses `NetworkChaos` with `action: delay` instead of `PodChaos`. Injects 10s latency with
  2s jitter at 100% correlation, simulating degraded network conditions without full outage.
- The test verifies no-regression (Ready=True maintained) rather than degradation-recovery,
  because latency should be tolerated by the operator's database client timeouts.
- `duration: 180s` acts as a safety net for auto-expiry.
- Step 4 uses `kubectl wait --for=condition=AllInjected` to confirm injection is active
  before checking operator stability, replacing a fixed sleep for determinism (CC-0049).
- Restart count is checked by container name (`manager`) rather than by array index to
  avoid false negatives if container ordering changes.

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
| 3 | Assert PDB exists | `assert` (5m) | PDB `keystone-chaos-api` with `spec.minAvailable: 1` (apiVersion: `policy/v1`) |
| 4 | Inject PodChaos | `apply` | Applies `01-podchaos.yaml` — PodChaos `kill-keystone-api` targeting `app.kubernetes.io/name: keystone` AND `app.kubernetes.io/instance: keystone-chaos-api` in `openstack` (the PodChaos resource name retains its historical `kill-keystone-api` label as a chaos-test identifier; the chaos still targets the bare-name `keystone-chaos-api` Pods, since CC-0095) | <!-- CC-0095 legacy: chaos-test fixture identifier intentionally retained -->
| 5 | Verify PDB enforcement and assert conditions | `script` (120s) + `assert` (5m) | Script polls until `readyReplicas < 3` (kill took effect), then asserts `availableReplicas >= 1`; Chainsaw asserts `DeploymentReady=True` and `Ready=True` with reason AllReady |
| 6 | Delete PodChaos | `delete` | Removes PodChaos `kill-keystone-api` to clean up | <!-- CC-0095 legacy: chaos-test fixture identifier intentionally retained -->
| 7 | Assert Ready=True after recovery | `assert` (5m) | Ready=True with reason AllReady — full replica count restored |

**Fixtures:** `00-keystone-cr.yaml`, `01-podchaos.yaml`

**Catch blocks:** Step 2 calls `diagnostics.sh baseline`. Step 3 catches with PDB describe.
Steps 5 and 7 catch with `diagnostics.sh chaos` using
`--dep-label=app.kubernetes.io/name=keystone,app.kubernetes.io/instance=keystone-chaos-api`.

**Design note:** Step 5 uses a script step to poll Deployment status because Chainsaw's
`wait` with a label selector waits for ALL matching pods, which does not work when the goal
is to verify that NOT ALL pods are down. The script polls `readyReplicas < 3` (confirming
the kill took effect) then asserts `availableReplicas >= 1`. The PDB name follows the
naming convention `subResourceName(keystone)` = `{cr-name}` (bare CR name since CC-0095),
so for CR `keystone-chaos-api`, the PDB is `keystone-chaos-api`.

---

### operator-pod-kill

**File:** `tests/e2e-chaos/operator-pod-kill/chainsaw-test.yaml`

**Scenario:** SC-CHAOS-009 (CC-0066)

**Purpose:** Validates that the Keystone operator recovers after ALL operator pods are killed
simultaneously (`mode: all`), forcing the Deployment controller to restart every pod and
trigger leader re-election. After recovery, a spec change (replica patch 1→2) verifies the
new leader can actively reconcile — proving operational capability beyond just running.

**Key difference from operator-pod-crash (SC-CHAOS-004):** SC-CHAOS-004 uses `mode: one`,
killing a single operator pod while leaving other replicas running. SC-CHAOS-009 uses
`mode: all`, killing every operator pod simultaneously. SC-CHAOS-004 does not verify
post-failover reconciliation capability; SC-CHAOS-009 patches `spec.replicas` after
recovery to confirm the new leader processes spec changes end-to-end.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-chaos-opk` with database `keystone_chaos_opk` |
| 2 | Assert baseline Ready=True | `assert` (5m) | Ready=True with reason AllReady — confirms healthy state before chaos injection |
| 3 | Inject chaos and verify pod replacement | `script` (270s) | Snapshots operator pod UIDs, applies `01-podchaos.yaml` (PodChaos `kill-operator-all`, `mode: all`, targets `app.kubernetes.io/name: keystone-operator` in `default`), waits until none of the pre-chaos UIDs remain, then waits until Deployment `readyReplicas` equals `.spec.replicas` |
| 4 | Delete PodChaos | `delete` | Removes PodChaos `kill-operator-all` to lift the fault |
| 5 | Assert Ready=True after failover | `assert` (5m) | All 6 conditions: SecretsReady=True, FernetKeysReady=True, DatabaseReady=True, DeploymentReady=True, BootstrapReady=True, Ready=True (AllReady) |
| 6 | Patch replicas 1→2 | `patch` | Applies `02-patch-replicas.yaml` — patches `spec.replicas` to 2 |
| 7 | Assert replica patch and Ready=True | `assert` (5m) | Deployment `keystone-chaos-opk` has `replicas: 2` and `availableReplicas: 2`; Ready=True with reason AllReady |

**Fixtures:** `00-keystone-cr.yaml`, `01-podchaos.yaml`, `02-patch-replicas.yaml`

**Catch blocks:** Step 2 calls `diagnostics.sh baseline`. Steps 3, 5, and 7 call
`diagnostics.sh chaos` with `--dep-label=app.kubernetes.io/name=keystone-operator --dep-ns=default`.

**Design notes:**

- The operator pod runs in the `default` namespace (not `openstack`) because
  `hack/ci-deploy-operator.sh` runs `helm install` without `--namespace`. The PodChaos
  `selector.namespaces` targets `default` accordingly.
- Step 5 asserts all 6 individual conditions (not just the aggregate Ready) to verify
  that no sub-condition was stuck in a stale state after the operator restart and leader
  re-election.
- The replica patch in Step 6 is the critical differentiator from SC-CHAOS-004: it proves
  the new leader actively processes spec changes, not just that the operator pod is running.
- Step 3 uses identity-based tracking (pod UIDs), not `readyReplicas`-drop polling. With
  `mode: all` and `gracePeriod: 0`, the kill+reschedule+ready cycle can complete faster
  than the first poll observes, so a previous implementation saw `readyReplicas=2`
  throughout and reported "kill did not take effect". Snapshotting UIDs before applying
  PodChaos and waiting for each of them to disappear is race-free — it proves replacement
  happened regardless of timing. The apply and wait share one script because chainsaw
  steps cannot pass state between each other.

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

### Operator Pod Kill All with Failover Reconciliation (SC-CHAOS-009)

Used when ALL operator pods are killed simultaneously (`mode: all`), forcing the Deployment
controller to restart all pods and trigger leader re-election. After recovery, a spec change
(replica patch) verifies the new leader can actively reconcile — proving operational
capability beyond just running.

```text
Apply CR → Assert Ready=True → Inject PodChaos (mode: all) → Wait readyReplicas 0→2
         → Delete PodChaos → Assert all 6 conditions=True → Patch replicas 1→2
         → Assert Deployment replicas=2 + Ready=True
```

1. Apply Keystone CR and assert `Ready=True` (baseline)
2. Apply PodChaos with `mode: all` to kill every operator pod
3. Poll operator Deployment `readyReplicas`: wait for drop to 0 (kill confirmed), then return to 2 (recovered)
4. Delete PodChaos to lift the fault
5. Assert all 6 conditions remain `True` — operator restart is invisible to CR status
6. Patch `spec.replicas` from 1 to 2
7. Assert Deployment has `replicas: 2` and `availableReplicas: 2`, and `Ready=True`

## PodChaos CRD Pattern

Phase 1 scenarios (SC-CHAOS-001 through SC-CHAOS-003), SC-CHAOS-004/SC-CHAOS-008, and
SC-CHAOS-009 use the `pod-kill` action. SC-CHAOS-005 uses `pod-failure` for sustained
fault injection. SC-CHAOS-009 uses `mode: all` (unlike all other `pod-kill` scenarios
which use `mode: one`) to kill every operator pod simultaneously.

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

### mode: all pod-kill (SC-CHAOS-009)

SC-CHAOS-009 uses `mode: all` instead of `mode: one` to kill every matching operator pod
simultaneously. This forces the Deployment controller to restart all pods (not just one)
and triggers a full leader re-election cycle.

```yaml
spec:
  action: pod-kill
  mode: all
  selector:
    namespaces:
    - default
    labelSelectors:
      app.kubernetes.io/name: keystone-operator
  gracePeriod: 0
```

| Field | Value | Rationale |
| --- | --- | --- |
| `mode` | `all` | Kills every operator pod — unlike SC-CHAOS-004 (`mode: one`) which leaves other replicas running |
| `selector.namespaces` | `default` | Operator runs in `default` because `hack/ci-deploy-operator.sh` runs `helm install` without `--namespace` |

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
| SC-CHAOS-006 | `keystone-chaos-net-part` | `keystone_chaos_net_part` | 1 |
| SC-CHAOS-007 | `keystone-chaos-net-lat` | `keystone_chaos_net_lat` | 1 |
| SC-CHAOS-008 | `keystone-chaos-api` | `keystone_chaos_api` | **3** |
| SC-CHAOS-009 | `keystone-chaos-opk` | `keystone_chaos_opk` | 1 |

All fixtures share the same base spec: `clusterRef` for database and memcached, fernet
rotation `"0 0 * * 0"` with `maxActiveKeys: 3`, bootstrap `adminUser: admin`. Most use
`replicas: 1`. SC-CHAOS-008 uses `replicas: 3` to trigger PDB creation via
`buildPodDisruptionBudget()` which requires `replicas > 1`.

## Catch Block Diagnostics

Every assert step includes a `catch:` block that collects diagnostic information when the
assertion fails. The information collected varies by scenario:

| Diagnostic | MariaDB (001) | Memcached (002) | OpenBao (003) | Operator (004) | CronJob (005) | Net Partition (006) | Net Latency (007) | API PDB (008) | Pod Kill (009) |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `diagnostics.sh` | Steps 2, 4, 6 | Steps 2, 4, 6 | Steps 2, 4, 6 | Steps 2, 4, 6 | Steps 2, 5, 7 | Steps 2, 4, 5, 7 | Steps 2, 4, 6 | Steps 2, 5, 7 | Steps 2, 4, 6, 8 |
| Target pod status | Steps 4, 6 | Steps 4, 6 | Steps 2, 4, 6 | — | — | — | — | — | — |
| Chaos Mesh experiment status | Steps 4, 6 | Steps 4, 6 | Steps 4, 6 | — | — | — | — | — | — |
| NetworkChaos CR status | — | — | — | — | — | Step 4 | — | — | — |
| Operator logs (`--previous`) | Steps 4, 6 | — | Steps 4, 6 | — | — | — | — | — | — |
| Target pod logs (`--previous`) | — | Steps 4, 6 | — | — | — | — | — | — | — |
| ESO ExternalSecret conditions | — | — | Steps 4, 6 | — | — | — | — | — | — |
| All pod logs | Step 2 | Step 2 | Step 2 | — | — | — | — | — | — |
| Namespace events | Steps 2, 4, 6 | Steps 2, 4, 6 | Steps 2, 4, 6 | — | — | — | — | — | — |
| CronJob/Job status | — | — | — | — | Steps 4, 5 | — | — | — | — |
| Job pod logs | — | — | — | — | Step 5 | — | — | — | — |
| PDB describe | — | — | — | — | — | — | — | Step 3 | — |

Phase 2 scenarios (004, 005, 008) and Phase 3 (009) use `diagnostics.sh` exclusively for catch diagnostics
(which internally collects CR status, pod status, logs, and events). Phase 1 scenarios
(001, 002, 003) use inline `kubectl` commands in catch blocks. Phase 4 network chaos
scenarios (006, 007) use `diagnostics.sh` for all catch blocks; SC-CHAOS-006 additionally
dumps the NetworkChaos CR status in Step 4 to confirm fault injection state.
SC-CHAOS-005 additionally collects CronJob/Job-specific diagnostics in Steps 4 and 5.

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
├── mariadb-network-partition/        SC-CHAOS-006: MariaDB network partition (CC-0049)
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-net-part)
│   ├── 01-networkchaos.yaml          NetworkChaos partitioning keystone→mariadb traffic
│   └── chainsaw-test.yaml            Test: DatabaseReady=False → recovery
├── mariadb-network-latency/          SC-CHAOS-007: MariaDB network latency (CC-0049)
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-net-lat)
│   ├── 01-networkchaos.yaml          NetworkChaos injecting 10s latency on keystone→mariadb
│   └── chainsaw-test.yaml            Test: Ready=True maintained, no crash-loop
├── api-pod-kill-pdb/                 SC-CHAOS-008: PDB availability guarantee (CC-0048)
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-api, replicas: 3)
│   ├── 01-podchaos.yaml              PodChaos targeting keystone API pods (multi-label)
│   └── chainsaw-test.yaml            Test: PDB minAvailable=1, availability maintained
└── operator-pod-kill/                SC-CHAOS-009: Operator pod kill all + failover (CC-0066)
    ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-opk)
    ├── 01-podchaos.yaml              PodChaos targeting keystone-operator (mode: all)
    ├── 02-patch-replicas.yaml        Patch replicas 1→2 for post-failover reconciliation
    └── chainsaw-test.yaml            Test: Leader re-election, conditions maintained, replica patch
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
- [Keystone Reconciler Architecture](../keystone/keystone-reconciler.md) — Sub-reconciler contracts and condition semantics (CC-0013, CC-0015)
- [Infrastructure E2E Deployment](../infrastructure/e2e-deployment.md) — Infrastructure stack deployment (CC-0010)
- `tests/e2e-chaos/chainsaw-config.yaml` — Chaos-specific Chainsaw configuration
- `tests/e2e-chaos/README.md` — Quick-start guide
