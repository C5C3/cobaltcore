---
title: Chaos E2E Test Suites
quadrant: operator
feature: CC-0047, CC-0054
---

# Chaos E2E Test Suites

Reference documentation for the chaos E2E test suites (CC-0047). These tests verify that
OpenStack operators correctly detect infrastructure dependency failures via status
conditions and recover autonomously when dependencies return.

For happy-path E2E tests, see [Keystone E2E Test Suites](./keystone-e2e-tests.md).

## Overview

The 3 Phase 1 test suites validate operator behavior during and after infrastructure
pod failures. Each suite deploys a Keystone CR, asserts a healthy baseline, injects a
[Chaos Mesh](https://chaos-mesh.org/) `PodChaos` fault, asserts the expected degradation
(or stability), removes the fault, and asserts full recovery. Tests use
[Chainsaw](https://kyverno.github.io/chainsaw/) to orchestrate the assertion lifecycle.

```text
┌─────────────────────────────────────────────────────────────────────────────┐
│  Chainsaw Chaos E2E Runner (parallel: 1)                                    │
│                                                                             │
│  ┌─────────────────────┐  ┌─────────────────────┐  ┌────────────────────┐  │
│  │ mariadb-pod-kill     │  │ memcached-pod-kill   │  │ openbao-pod-kill  │  │
│  │ SC-CHAOS-001         │  │ SC-CHAOS-002         │  │ SC-CHAOS-003      │  │
│  │ (keystone-chaos-db)  │  │ (keystone-chaos-mc)  │  │ (keystone-chaos-  │  │
│  │                      │  │                      │  │  bao)             │  │
│  │ Pattern: degradation │  │ Pattern: no-         │  │ Pattern:          │  │
│  │ and recovery         │  │ regression           │  │ degradation and   │  │
│  │                      │  │                      │  │ recovery          │  │
│  └─────────────────────┘  └─────────────────────┘  └────────────────────┘  │
│                                                                             │
│  All tests run in: namespace openstack                                      │
│  Fault injection: Chaos Mesh PodChaos CRDs                                  │
│  Infrastructure: MariaDB, Memcached, ESO, OpenBao, Chaos Mesh (pre-deployed)│
└─────────────────────────────────────────────────────────────────────────────┘
```

## Prerequisites

All 3 test suites require the infrastructure stack and Chaos Mesh to be deployed and
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

## PodChaos CRD Pattern

All scenarios use the `pod-kill` action with `mode: one`:

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

The test explicitly deletes the PodChaos CR (Step 5) before asserting recovery (Step 6).
This ensures the fault is lifted before the recovery assertion window begins.

## Keystone CR Fixtures

Each scenario uses a unique CR name and database name to enable parallel execution:

| Scenario | CR Name | Database |
| --- | --- | --- |
| SC-CHAOS-001 | `keystone-chaos-db` | `keystone_chaos_db` |
| SC-CHAOS-002 | `keystone-chaos-mc` | `keystone_chaos_mc` |
| SC-CHAOS-003 | `keystone-chaos-bao` | `keystone_chaos_bao` |

All fixtures share the same spec: `replicas: 1`, `clusterRef` for database and memcached,
fernet rotation `"0 0 * * 0"` with `maxActiveKeys: 3`, bootstrap `adminUser: admin`.

## Catch Block Diagnostics

Every assert step includes a `catch:` block that collects diagnostic information when the
assertion fails. The information collected varies by scenario:

| Diagnostic | MariaDB | Memcached | OpenBao |
| --- | --- | --- | --- |
| Keystone CR status | Steps 2, 4, 6 | Steps 2, 4, 6 | Steps 2, 4, 6 |
| Target pod status | Steps 4, 6 | Steps 4, 6 | Steps 2, 4, 6 |
| Chaos Mesh experiment status | Steps 4, 6 | Steps 4, 6 | Steps 4, 6 |
| Operator logs (`--previous`) | Steps 4, 6 | — | Steps 4, 6 |
| Target pod logs (`--previous`) | — | Steps 4, 6 | — |
| ESO ExternalSecret conditions | — | — | Steps 4, 6 |
| All pod logs | Step 2 | Step 2 | Step 2 |
| Namespace events | Steps 2, 4, 6 | Steps 2, 4, 6 | Steps 2, 4, 6 |

## File Layout

```text
tests/e2e-chaos/
├── chainsaw-config.yaml              Chaos-specific Chainsaw configuration (CC-0047)
├── README.md                         Quick-start documentation
├── mariadb-pod-kill/                 SC-CHAOS-001: MariaDB pod kill
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-db)
│   ├── 01-podchaos.yaml              PodChaos targeting mariadb in openstack
│   └── chainsaw-test.yaml            Test: DatabaseReady=False → recovery
├── memcached-pod-kill/               SC-CHAOS-002: Memcached pod kill
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-mc)
│   ├── 01-podchaos.yaml              PodChaos targeting memcached in openstack
│   └── chainsaw-test.yaml            Test: Ready=True maintained (no regression)
└── openbao-pod-kill/                 SC-CHAOS-003: OpenBao pod kill
    ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-bao)
    ├── 01-podchaos.yaml              PodChaos targeting openbao in openbao-system
    └── chainsaw-test.yaml            Test: SecretsReady=False → recovery
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
