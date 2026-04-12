---
title: Keystone E2E Test Suites
quadrant: operator
feature: CC-0016
---

# Keystone E2E Test Suites

Reference documentation for the Keystone Chainsaw E2E test suites (CC-0016). These
tests validate the KeystoneReconciler's end-to-end behavior in a real Kubernetes cluster
with all infrastructure dependencies deployed (MariaDB, Memcached, ESO, OpenBao).

For CRD validation E2E tests (`invalid-cr`), see
[Keystone CRD API Reference](./keystone-crd.md#chainsaw-e2e-tests). For the reconciler
architecture and sub-reconciler contracts, see
[Keystone Reconciler Architecture](./keystone-reconciler.md). For infrastructure
deployment automation, see
[Infrastructure E2E Deployment](./infrastructure/e2e-deployment.md).

## Overview

The test suites cover the full reconciler lifecycle — from initial deployment through
scaling, key rotation, image upgrades, cross-release upgrades, and deletion cleanup. Each
suite is independent and creates its own Keystone CR with a unique name in the `openstack`
namespace, enabling parallel execution.

```text
┌─────────────────────────────────────────────────────────────────────────────┐
│  Chainsaw E2E Runner (parallel: 4)                                         │
│                                                                             │
│  ┌───────────────────┐  ┌───────────────────┐  ┌───────────────────────┐   │
│  │ autoscaling        │  │ basic-deployment  │  │ basic-deployment-     │   │
│  │                    │  │                   │  │  2026-1               │   │
│  └───────────────────┘  └───────────────────┘  └───────────────────────┘   │
│  ┌───────────────────┐  ┌───────────────────┐  ┌───────────────────────┐   │
│  │ brownfield-        │  │ credential-       │  │ deletion-cleanup      │   │
│  │  database          │  │  rotation         │  │                       │   │
│  └───────────────────┘  └───────────────────┘  └───────────────────────┘   │
│  ┌───────────────────┐  ┌───────────────────┐  ┌───────────────────────┐   │
│  │ fernet-rotation    │  │ image-upgrade     │  │ invalid-cr            │   │
│  │                    │  │                   │  │                       │   │
│  └───────────────────┘  └───────────────────┘  └───────────────────────┘   │
│  ┌───────────────────┐  ┌───────────────────┐  ┌───────────────────────┐   │
│  │ middleware-config  │  │ missing-secret    │  │ namespace-scoped-     │   │
│  │                    │  │                   │  │  rbac                 │   │
│  └───────────────────┘  └───────────────────┘  └───────────────────────┘   │
│  ┌───────────────────┐  ┌───────────────────┐  ┌───────────────────────┐   │
│  │ network-policy     │  │ policy-overrides  │  │ release-upgrade       │   │
│  │                    │  │                   │  │                       │   │
│  └───────────────────┘  └───────────────────┘  └───────────────────────┘   │
│  ┌───────────────────┐  ┌───────────────────┐  ┌───────────────────────┐   │
│  │ resources          │  │ scale             │  │ trust-flush           │   │
│  │                    │  │                   │  │                       │   │
│  └───────────────────┘  └───────────────────┘  └───────────────────────┘   │
│  ┌───────────────────┐  ┌───────────────────┐                              │
│  │ upgrade-flow       │  │ uwsgi             │                              │
│  │                    │  │                   │                              │
│  └───────────────────┘  └───────────────────┘                              │
│                                                                             │
│  All tests run in: namespace openstack                                      │
│  Infrastructure: MariaDB, Memcached, ESO, OpenBao (pre-deployed)           │
└─────────────────────────────────────────────────────────────────────────────┘
```

## Prerequisites

All test suites require the infrastructure stack to be deployed and healthy. The
`infra-stack-health` test (`tests/e2e/infrastructure/`) verifies this precondition.

| Prerequisite | Details |
| --- | --- |
| Infrastructure stack | Deployed via `make deploy-infra` (see [Infrastructure E2E Deployment](./infrastructure/e2e-deployment.md)) |
| Keystone operator | Deployed to the cluster with CRDs installed |
| ESO ExternalSecrets | `keystone-admin`, `keystone-db` synced in `openstack` namespace |
| MariaDB instance | `openstack-db` MariaDB CR Ready in `openstack` namespace |
| Memcached instance | `openstack-memcached` Memcached CR Ready in `openstack` namespace |

## Running the Tests

```bash
# Run all E2E tests (infrastructure + Keystone)
make e2e

# Run only Keystone E2E tests
chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e/keystone/

# Run a specific test suite
chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e/keystone/basic-deployment/
```

## Chainsaw Configuration

All tests use the shared configuration at `tests/e2e/chainsaw-config.yaml`:

| Setting | Value | Purpose |
| --- | --- | --- |
| `timeouts.apply` | 30s | Resource application timeout |
| `timeouts.assert` | 120s | Default assertion timeout (overridden per-step in most suites) |
| `timeouts.cleanup` | 60s | Post-test resource cleanup |
| `timeouts.delete` | 30s | Resource deletion timeout |
| `timeouts.error` | 30s | Error assertion timeout |
| `timeouts.exec` | 30s | Script execution timeout |
| `execution.parallel` | 4 | Maximum concurrent test suites |
| `execution.failFast` | true | Stop on first failure |
| `report.format` | JUNIT-TEST | JUnit XML output for CI |
| `report.path` | `_output/reports` | Report directory |

Individual test suites override the assert timeout to 5 minutes (`5m`) to accommodate
the full reconciliation cycle (Secret sync, database provisioning, db_sync Job,
Deployment rollout, bootstrap Job).

## Test Suite Inventory

| Suite | CR Name | Reconciler Behavior Validated | Requirements |
| --- | --- | --- | --- |
| [basic-deployment](#basic-deployment) | `keystone-basic` | Full happy-path reconciliation, all conditions, owned resources, API accessibility | REQ-001, REQ-002, REQ-003, REQ-012, REQ-013 |
| [missing-secret](#missing-secret) | `keystone-missing-secret` | SecretsReady requeue on missing ESO Secrets, recovery on creation | REQ-004, REQ-012, REQ-013 |
| [fernet-rotation](#fernet-rotation) | `keystone-fernet` | CronJob schedule, manual rotation trigger, Secret data change, Deployment annotation update | REQ-005, REQ-012, REQ-013 |
| [scale](#scale) | `keystone-scale` | Replica scaling up (3→5) and down (5→2) | REQ-006, REQ-012, REQ-013 |
| [deletion-cleanup](#deletion-cleanup) | `keystone-cleanup` | Owner reference cascading deletion of all owned resources | REQ-007, REQ-012, REQ-013 |
| [policy-overrides](#policy-overrides) | `keystone-policy` | oslo.policy integration via ConfigMap reference | REQ-008, REQ-012, REQ-013 |
| [middleware-config](#middleware-config) | `keystone-middleware` | WSGI middleware pipeline customization in api-paste.ini | REQ-009, REQ-012, REQ-013 |
| [brownfield-database](#brownfield-database) | `keystone-brownfield` | Explicit database host (no MariaDB CRs created) | REQ-010, REQ-012, REQ-013 |
| [image-upgrade](#image-upgrade) | `keystone-upgrade` | Rolling image update without losing Ready status | REQ-011, REQ-012, REQ-013 |
| [release-upgrade](#release-upgrade) | `keystone-release-upgrade` | Cross-release upgrade from 2025.2 to 2026.1 via expand-migrate-contract, API accessibility before/after | REQ-001–REQ-009 (CC-0060) |

---

## Test Suite Details

### basic-deployment

**File:** `tests/e2e/keystone/basic-deployment/chainsaw-test.yaml`

**Purpose:** Validates the full happy-path reconciliation cycle in managed mode. Deploys
a Keystone CR with `clusterRef` and verifies all 5 sub-conditions progress to True,
the aggregate Ready condition reaches True with reason `AllReady`, all owned resources
exist, a ConfigMap with the expected prefix exists, and the Keystone API at `/v3` is
accessible.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-basic` in managed mode |
| 2 | Assert all sub-conditions and Ready | `assert` (5m) | Verifies SecretsReady=True (SecretsAvailable), DatabaseReady=True (DatabaseSynced), FernetKeysReady=True (FernetKeysAvailable), DeploymentReady=True, BootstrapReady=True (BootstrapComplete), Ready=True (AllReady). Condition order follows the `subConditionTypes` display order. |
| 3 | Assert Deployment and Service | `assert` (5m) | Deployment `keystone-basic-api` has `availableReplicas > 0`; Service `keystone-basic-api` has port 5000 |
| 4 | Assert Fernet resources | `assert` (5m) | CronJob `keystone-basic-fernet-rotate`, Secret `keystone-basic-fernet-keys`, ServiceAccount/Role/RoleBinding `keystone-basic-fernet-rotate`, PushSecret `keystone-basic-fernet-keys-backup` all exist |
| 5 | Assert ConfigMap exists | `script` | `kubectl get cm -n openstack -o name \| grep keystone-basic-config-` — verifies a ConfigMap with content-hash suffix exists |
| 6 | Assert API accessibility | `script` | `curl -sf http://keystone-basic-api.openstack.svc:5000/v3` — verifies the Keystone API responds |

**Fixtures:** `00-keystone-cr.yaml`

---

### missing-secret

**File:** `tests/e2e/keystone/missing-secret/chainsaw-test.yaml`

**Purpose:** Validates the reconciler's secret dependency recovery behavior. Applies a
Keystone CR referencing non-existent ExternalSecret names, verifies the operator sets
SecretsReady=False and waits, then creates the missing ExternalSecrets and verifies
recovery to Ready=True.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR with non-existent secret references | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-missing-secret` with unique secretRef names |
| 2 | Assert SecretsReady=False | `assert` (5m) | SecretsReady condition has status False with reason WaitingForDBCredentials |
| 3 | Create the missing ExternalSecrets | `apply` | Applies `01-late-secrets.yaml` — creates ExternalSecrets pointing to existing OpenBao paths |
| 4 | Assert recovery to Ready=True | `assert` (5m) | Ready condition transitions to True with reason AllReady |

**Fixtures:** `00-keystone-cr.yaml`, `01-late-secrets.yaml`

**Design note:** Recovery requires creating ExternalSecrets (not raw Secrets) because
`reconcileSecrets` calls `WaitForExternalSecret`, which checks for the ExternalSecret
resource. The ESO controller then syncs the actual Secret.

---

### fernet-rotation

**File:** `tests/e2e/keystone/fernet-rotation/chainsaw-test.yaml`

**Purpose:** Validates the CronJob-based Fernet key rotation mechanism end-to-end.
Deploys a Keystone CR, verifies the CronJob schedule matches `spec.fernet.rotationSchedule`,
triggers a manual rotation via `kubectl create job --from=cronjob`, and verifies
both the Secret data and Deployment annotation change.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-fernet` |
| 2 | Wait for Ready=True | `assert` (5m) | Ready=True with reason AllReady |
| 3 | Assert CronJob schedule matches spec | `assert` (5m) | CronJob `keystone-fernet-fernet-rotate` schedule is `"0 0 * * 0"` |
| 4 | Trigger rotation and verify changes | `script` (180s) | Records Secret hash before rotation, creates manual Job from CronJob, verifies Secret data hash changed, polls up to 120s for Deployment `keystone.c5c3.io/fernet-keys-hash` annotation change |
| 5 | Assert Ready=True maintained | `assert` (5m) | Ready=True with reason AllReady after rotation |

**Fixtures:** `00-keystone-cr.yaml`

**Rotation verification approach:** The script step uses a hash-comparison approach
(record before, compare after) rather than polling for specific values. This avoids
timing issues with controller reconciliation delays.

---

### scale

**File:** `tests/e2e/keystone/scale/chainsaw-test.yaml`

**Purpose:** Validates that patching `spec.replicas` on the Keystone CR propagates to the
underlying Deployment. Tests both scale-up (3→5) and scale-down (5→2).

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR with replicas: 3 | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-scale` with replicas: 3 |
| 2 | Assert Ready and initial replica count | `assert` (5m) | Ready=True, Deployment `keystone-scale-api` has replicas: 3 and availableReplicas >= 3 |
| 3 | Scale up to 5 replicas | `patch` | Applies `01-patch-scale-up.yaml` — patches replicas to 5 |
| 4 | Assert scale-up | `assert` (5m) | Deployment has replicas: 5 and availableReplicas >= 5 |
| 5 | Scale down to 2 replicas | `patch` | Applies `02-patch-scale-down.yaml` — patches replicas to 2 |
| 6 | Assert scale-down | `assert` (5m) | Deployment has replicas: 2 and availableReplicas == 2 (exact equality to verify scale-down completed) |

**Fixtures:** `00-keystone-cr.yaml`, `01-patch-scale-up.yaml`, `02-patch-scale-down.yaml`

---

### deletion-cleanup

**File:** `tests/e2e/keystone/deletion-cleanup/chainsaw-test.yaml`

**Purpose:** Validates that deleting a Keystone CR triggers Kubernetes garbage collection
of all owned resources via owner references. Deploys a CR, waits for Ready, deletes the
CR, then error-asserts that all owned resources return NotFound.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-cleanup` |
| 2 | Wait for Ready=True | `assert` (5m) | Ready=True with reason AllReady |
| 3 | Delete the Keystone CR | `delete` | Deletes Keystone CR `keystone-cleanup` from namespace `openstack` |
| 4 | Assert all owned resources deleted | `error` | 12 error assertions verifying NotFound for: Deployment `keystone-cleanup-api`, Service `keystone-cleanup-api`, CronJob `keystone-cleanup-fernet-rotate`, Secret `keystone-cleanup-fernet-keys`, ServiceAccount `keystone-cleanup-fernet-rotate`, Role `keystone-cleanup-fernet-rotate`, RoleBinding `keystone-cleanup-fernet-rotate`, PushSecret `keystone-cleanup-fernet-keys-backup`, Job `keystone-cleanup-db-sync`, Database `keystone-cleanup`, User `keystone-cleanup`, Grant `keystone-cleanup` |
| 5 | Assert dynamically-named ConfigMap deleted | `script` | Inverted grep verifies no ConfigMap matching `keystone-cleanup-config-*` remains after garbage collection |

**Fixtures:** `00-keystone-cr.yaml`

**Design note:** The bootstrap Job (`keystone-cleanup-bootstrap`) has
`TTLSecondsAfterFinished: 300` and may be TTL-cleaned before CR deletion. It is excluded
from the error assertions because its absence is expected in both cases.

---

### policy-overrides

**File:** `tests/e2e/keystone/policy-overrides/chainsaw-test.yaml`

**Purpose:** Validates oslo.policy integration. Applies a policy source ConfigMap and a
Keystone CR with `policyOverrides.configMapRef`, then verifies the generated ConfigMap
contains a `policy.yaml` data key with the expected rules and that `keystone.conf`
contains the `[oslo_policy]` section with `policy_file`.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply policy source ConfigMap | `apply` | Applies `00-policy-cm.yaml` — ConfigMap with policy rules (e.g., `identity:list_users`) |
| 2 | Apply Keystone CR with policyOverrides | `apply` | Applies `01-keystone-cr.yaml` — Keystone CR `keystone-policy` with `policyOverrides.configMapRef` |
| 3 | Wait for Ready=True | `assert` (5m) | Ready=True with reason AllReady |
| 4 | Verify ConfigMap contents | `script` | Gets ConfigMap matching `keystone-policy-config-*`, verifies `policy.yaml` data key contains `identity:list_users` and `keystone.conf` contains `policy_file` |

**Fixtures:** `00-policy-cm.yaml`, `01-keystone-cr.yaml`

---

### middleware-config

**File:** `tests/e2e/keystone/middleware-config/chainsaw-test.yaml`

**Purpose:** Validates WSGI middleware pipeline customization. Applies a Keystone CR with
custom `spec.middleware` entries and verifies the generated ConfigMap's `api-paste.ini`
contains the custom filter name in the pipeline definition and the filter factory entry.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR with custom middleware | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-middleware` with middleware entries (e.g., `audit` filter with `keystonemiddleware.audit` factory) |
| 2 | Wait for Ready=True | `assert` (5m) | Ready=True with reason AllReady |
| 3 | Verify api-paste.ini contents | `script` | Gets ConfigMap matching `keystone-middleware-config-*`, verifies `api-paste.ini` contains `audit` filter reference and `keystonemiddleware.audit` factory |

**Fixtures:** `00-keystone-cr.yaml`

---

### brownfield-database

**File:** `tests/e2e/keystone/brownfield-database/chainsaw-test.yaml`

**Purpose:** Validates brownfield database support — using an explicit `database.host`
without `clusterRef`. Verifies no MariaDB CRs (Database, User, Grant) are created, the
generated `keystone.conf` connection string contains the explicit host, and the
reconciliation completes to Ready=True.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR with brownfield database | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-brownfield` with `database.host` and `database.port` (no `clusterRef`) |
| 2 | Assert no MariaDB CRs created | `error` | 3 error assertions verifying NotFound for: Database `keystone-brownfield`, User `keystone-brownfield`, Grant `keystone-brownfield` |
| 3 | Wait for Ready=True | `assert` (5m) | Ready=True with reason AllReady |
| 4 | Verify keystone.conf connection | `script` | Gets ConfigMap matching `keystone-brownfield-config-*`, verifies `keystone.conf` `[database]` section contains `openstack-db.openstack.svc.cluster.local` |

**Fixtures:** `00-keystone-cr.yaml`

---

### image-upgrade

**File:** `tests/e2e/keystone/image-upgrade/chainsaw-test.yaml`

**Purpose:** Validates non-disruptive rolling image upgrades. Deploys a Keystone CR,
waits for Ready, patches `spec.image.tag` to a new value, then verifies the Deployment
container image updates and Ready=True is maintained after the rollout completes.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-upgrade` |
| 2 | Assert Ready and initial image tag | `assert` + `script` (5m) | Ready=True; script verifies Deployment `keystone-upgrade-api` container image contains `2025.2` |
| 3 | Patch image tag | `patch` | Applies `01-patch-image.yaml` — patches `spec.image.tag` to `2025.2-upgraded` |
| 4 | Assert image updated and Ready maintained | `script` (120s) + `assert` (5m) | Script polls up to 120s to verify Deployment image contains `2025.2-upgraded`; assert verifies Ready=True, availableReplicas > 0, and updatedReplicas == replicas (rollout complete) |

**Fixtures:** `00-keystone-cr.yaml`, `01-patch-image.yaml`

---

### release-upgrade

**File:** `tests/e2e/keystone/release-upgrade/chainsaw-test.yaml`

**Purpose:** Validates a cross-release upgrade from OpenStack 2025.2 to 2026.1 via the
expand-migrate-contract database migration path (keystone 28.0.0 → 29.0.0). Deploys a
Keystone CR with tag 2025.2, verifies the Keystone API at `/v3` is accessible, patches
`spec.image.tag` to 2026.1, then verifies the expand/migrate/contract Jobs are created,
the Deployment image updates to 2026.1, the rollout completes, `installedRelease` reaches
2026.1, and the Keystone API remains accessible post-upgrade.

This differs from `image-upgrade` (CC-0016), which tests same-release tag swaps
(2025.2→2025.2-upgraded) without database migration, and from `upgrade-flow` (CC-0056),
which focuses on internal state machine mechanics (skip-level rejection).

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR with tag 2025.2 | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-release-upgrade` in managed mode with tag 2025.2 |
| 2 | Assert Ready and initial image | `assert` (5m) + `script` | Ready=True (AllReady), `installedRelease`=2025.2; script verifies Deployment `keystone-release-upgrade-api` container image ends with `2025.2` |
| 3 | Verify API before upgrade | `script` (30s) | `kubectl run curl-test-release-pre` with python3 `urllib.request` — verifies GET `/v3` succeeds |
| 4 | Patch image tag to 2026.1 | `patch` | Applies `01-patch-upgrade.yaml` — patches `spec.image.tag` to `2026.1` |
| 5 | Assert upgrade completes | `assert` (5m) + `script` | `installedRelease`=2026.1, Ready=True (AllReady); scripts verify db-expand, db-migrate, db-contract Jobs exist and Deployment image ends with `2026.1`; assert verifies `updatedReplicas == replicas` and `availableReplicas > 0` |
| 6 | Verify API after upgrade | `script` (30s) | `kubectl run curl-test-release-post` with python3 `urllib.request` — verifies GET `/v3` succeeds post-upgrade |

**Fixtures:** `00-keystone-cr.yaml`, `01-patch-upgrade.yaml`

**Diagnostics:** Steps 2 and 5 include `catch` blocks that capture pod logs (including
`--previous`), Job logs, pod descriptions, Job status, and namespace events for
debugging failures.

**Design notes:**

- API accessibility is tested via `kubectl run` with python3 `urllib.request` rather than
  direct curl, because the test pods use the Keystone service image which provides python3.
  The curl test pods use unique names (`curl-test-release-pre`, `curl-test-release-post`) to
  avoid conflicts during parallel execution.
- The 5-minute assert timeout accommodates the full expand→migrate→rolling-update→contract
  cycle.
- This test complements `upgrade-flow` (CC-0056): `upgrade-flow` validates internal state
  machine behavior (skip-level rejection, phase transitions), while `release-upgrade`
  validates the user-facing lifecycle (API accessibility before/after, Deployment rollout).

---

## Assertion Patterns

The test suites use three Chainsaw assertion patterns:

### Resource Assertion (`assert`)

Declarative YAML matching against a Kubernetes resource using JMESPath filter syntax for
condition assertions. Used for condition checks, replica counts, and resource existence.
The assert timeout is set to 5 minutes at the spec level (`timeouts.assert: 5m`).

```yaml
- try:
    - assert:
        resource:
          apiVersion: keystone.openstack.c5c3.io/v1alpha1
          kind: Keystone
          metadata:
            name: keystone-basic
            namespace: openstack
          status:
            (conditions[?type == 'Ready']):
            - status: "True"
              reason: AllReady
```

### Error Assertion (`error`)

Verifies that a resource does **not** exist. Used in `deletion-cleanup` and
`brownfield-database` to assert garbage collection and absence of MariaDB CRs.

```yaml
- name: Assert Deployment not found
  try:
    - error:
        resource:
          apiVersion: apps/v1
          kind: Deployment
          metadata:
            name: keystone-cleanup-api
            namespace: openstack
```

### Script Assertion (`script`)

Shell commands for assertions that cannot be expressed declaratively — ConfigMap name
patterns (content-hash suffix), API endpoint connectivity, and hash-based rotation
verification.

```yaml
- name: Assert ConfigMap exists
  try:
    - script:
        content: |
          kubectl get cm -n openstack -o name | grep keystone-basic-config-
```

---

## Sub-Condition Progression

The Keystone reconciler sets conditions in this order during a successful reconciliation.

> **Note:** This diagram shows the _execution order_ within `Reconcile()`, which differs
> from the `subConditionTypes` display order (`SecretsReady, DatabaseReady,
> FernetKeysReady, DeploymentReady, BootstrapReady`). The display order determines how
> conditions appear in `kubectl get` and status output; the execution order below shows
> the actual reconciliation sequence.

```text
SecretsReady=True (SecretsAvailable)
    │
    ▼
FernetKeysReady=True (FernetKeysAvailable)
    │
    ▼
reconcileConfig (no condition — returns configMapName)
    │
    ▼
DatabaseReady=True (DatabaseSynced)
    │
    ▼
DeploymentReady=True (DeploymentReady)
    │
    ▼
BootstrapReady=True (BootstrapComplete)
    │
    ▼
Ready=True (AllReady) — aggregate of all 5 sub-conditions
```

The `basic-deployment` test asserts all 6 conditions (5 sub-conditions + Ready) in a
single assert step, validating the full progression.

---

## File Layout

```text
tests/e2e/keystone/
├── autoscaling/
│   ├── chainsaw-test.yaml              HPA reconciliation (CC-0038)
│   ├── 00-keystone-cr.yaml             Keystone CR with CPU autoscaling
│   ├── 01-patch-add-memory-metric.yaml Patch to add memory metric
│   └── 02-patch-disable-autoscaling.yaml Patch to disable autoscaling
├── basic-deployment/
│   ├── chainsaw-test.yaml              Happy-path reconciliation (CC-0016)
│   └── 00-keystone-cr.yaml             Keystone CR in managed mode
├── basic-deployment-2026-1/
│   ├── chainsaw-test.yaml              Happy-path reconciliation 2026.1 (CC-0051)
│   └── 00-keystone-cr.yaml             Keystone CR with 2026.1 image
├── brownfield-database/
│   ├── chainsaw-test.yaml              External database mode (CC-0016)
│   ├── 00-brownfield-db-setup.yaml     External database setup
│   └── 00-keystone-cr.yaml             Keystone CR with database.host
├── credential-rotation/
│   ├── chainsaw-test.yaml              Credential key rotation (CC-0036)
│   └── 00-keystone-cr.yaml             Keystone CR with rotation schedule
├── deletion-cleanup/
│   ├── chainsaw-test.yaml              Garbage collection (CC-0016)
│   └── 00-keystone-cr.yaml             Keystone CR for cleanup test
├── fernet-rotation/
│   ├── chainsaw-test.yaml              Fernet key rotation (CC-0016)
│   └── 00-keystone-cr.yaml             Keystone CR with rotation schedule
├── image-upgrade/
│   ├── chainsaw-test.yaml              Rolling image upgrade (CC-0016)
│   ├── 00-keystone-cr.yaml             Keystone CR with initial image tag
│   └── 01-patch-image.yaml             Patch spec.image.tag
├── invalid-cr/
│   ├── chainsaw-test.yaml              CRD webhook validation (CC-0012)
│   ├── 00-invalid-cron.yaml            Invalid cron expression CR
│   └── 01-duplicate-plugins.yaml       Duplicate plugin configSection CR
├── middleware-config/
│   ├── chainsaw-test.yaml              Middleware pipeline (CC-0016)
│   └── 00-keystone-cr.yaml             Keystone CR with custom middleware
├── missing-secret/
│   ├── chainsaw-test.yaml              Secret dependency recovery (CC-0016)
│   ├── 00-keystone-cr.yaml             Keystone CR with non-existent secretRefs
│   └── 01-late-secrets.yaml            ExternalSecrets created after CR
├── namespace-scoped-rbac/
│   ├── chainsaw-test.yaml              Namespace-scoped RBAC (CC-0043)
│   └── 00-keystone-cr.yaml             Keystone CR for RBAC test
├── network-policy/
│   ├── chainsaw-test.yaml              NetworkPolicy reconciliation (CC-0039)
│   ├── 00-keystone-cr.yaml             Keystone CR with ingress policy
│   ├── 01-patch-update-ingress.yaml    Patch ingress rule
│   └── 02-patch-disable-networkpolicy.yaml Patch to disable NetworkPolicy
├── policy-overrides/
│   ├── chainsaw-test.yaml              oslo.policy integration (CC-0016)
│   ├── 00-policy-cm.yaml               Policy source ConfigMap
│   └── 01-keystone-cr.yaml             Keystone CR with policyOverrides
├── release-upgrade/
│   ├── chainsaw-test.yaml              Cross-release upgrade 2025.2→2026.1 (CC-0060)
│   ├── 00-keystone-cr.yaml             Keystone CR with initial tag 2025.2
│   └── 01-patch-upgrade.yaml           Patch spec.image.tag to 2026.1
├── resources/
│   ├── chainsaw-test.yaml              Resource defaults and propagation (CC-0042)
│   ├── 00-keystone-cr.yaml             Keystone CR without explicit resources
│   └── 01-patch-custom-resources.yaml  Patch with custom resource limits
├── scale/
│   ├── chainsaw-test.yaml              Replica scaling and PDB (CC-0016, CC-0037)
│   ├── 00-keystone-cr.yaml             Keystone CR with replicas: 3
│   ├── 01-patch-scale-up.yaml          Patch replicas to 5
│   ├── 02-patch-scale-down.yaml        Patch replicas to 2
│   └── 03-patch-scale-to-one.yaml      Patch replicas to 1
├── trust-flush/
│   ├── chainsaw-test.yaml              Trust flush CronJob (CC-0057)
│   ├── 00-keystone-cr.yaml             Keystone CR with trustFlush config
│   └── 01-patch-disable-trust-flush.yaml Patch to disable trust flush
├── upgrade-flow/
│   ├── chainsaw-test.yaml              Expand-migrate-contract upgrade (CC-0056)
│   ├── 00-keystone-cr.yaml             Keystone CR with initial release
│   ├── 01-patch-upgrade.yaml           Patch for sequential upgrade
│   └── 02-patch-skip-level.yaml        Patch for skip-level upgrade
└── uwsgi/
    ├── chainsaw-test.yaml              uWSGI command propagation (CC-0040)
    ├── 00-keystone-cr.yaml             Keystone CR without explicit uWSGI
    └── 01-patch-custom-uwsgi.yaml      Patch with custom uWSGI settings
```

## Related Resources

- [Keystone CRD API Reference](./keystone-crd.md) — CRD types, webhooks, and `invalid-cr` E2E tests (CC-0011, CC-0012)
- [Keystone Reconciler Architecture](./keystone-reconciler.md) — Sub-reconciler contracts and unit tests (CC-0013, CC-0015)
- [Infrastructure E2E Deployment](./infrastructure/e2e-deployment.md) — Infrastructure stack deployment and `infra-stack-health` test (CC-0010)
- `tests/e2e/chainsaw-config.yaml` — Shared Chainsaw configuration
- `.github/workflows/ci.yaml` — CI workflow with E2E job
