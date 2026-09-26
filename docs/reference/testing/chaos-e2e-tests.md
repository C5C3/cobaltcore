---
title: Chaos E2E Test Suites
quadrant: operator
---

# Chaos E2E Test Suites

Reference documentation for the chaos E2E test suites. These
tests verify that OpenStack operators correctly detect infrastructure dependency failures
via status conditions and recover autonomously when dependencies return. Phase 2
extends the suite with operator resilience and workload chaos scenarios. Phase 3
adds operator pod kill with leader re-election and post-failover reconciliation verification.

For happy-path E2E tests, see [Keystone E2E Test Suites](./keystone-e2e-tests.md).

## Overview

The 9 chaos test suites validate operator behavior during and after fault injection.
Phase 1 covers infrastructure dependency pod kills. Phase 2 adds
operator self-recovery, CronJob workload fault tolerance, and PDB availability guarantee
scenarios. Phase 3 adds an all-pod operator kill with leader re-election
verification. Phase 4 adds network chaos scenarios (partition and latency).
Each suite deploys a Keystone CR, asserts a healthy baseline, injects a
[Chaos Mesh](https://chaos-mesh.org/) `PodChaos` or `NetworkChaos` fault, asserts the expected degradation
(or stability), removes the fault, and asserts full recovery. Tests use
[Chainsaw](https://kyverno.github.io/chainsaw/) to orchestrate the assertion lifecycle.

```text
┌──────────────────────────────────────────────────────────────────────────────┐
│  Chainsaw Chaos E2E Runner (parallel: 1)                                     │
│                                                                              │
│  Phase 1: Dependency Pod Kill                                                │
│  ┌──────────────────────┐  ┌──────────────────────┐  ┌────────────────────┐  │
│  │ mariadb-pod-kill     │  │ memcached-pod-kill   │  │ openbao-pod-kill   │  │
│  │ SC-CHAOS-001         │  │ SC-CHAOS-002         │  │ SC-CHAOS-003       │  │
│  │ (keystone-chaos-db)  │  │ (keystone-chaos-mc)  │  │ (keystone-chaos-   │  │
│  │                      │  │                      │  │  bao)              │  │
│  │ Pattern: degradation │  │ Pattern: no-         │  │ Pattern:           │  │
│  │ and recovery         │  │ regression           │  │ degradation and    │  │
│  │                      │  │                      │  │ recovery           │  │
│  └──────────────────────┘  └──────────────────────┘  └────────────────────┘  │
│                                                                              │
│  Phase 2: Operator Resilience and Workload Chaos                             │
│  ┌──────────────────────┐  ┌──────────────────────┐  ┌────────────────────┐  │
│  │ operator-pod-crash   │  │ cronjob-rotation-    │  │ api-pod-kill-pdb   │  │
│  │ SC-CHAOS-004         │  │ failure              │  │ SC-CHAOS-008       │  │
│  │ (keystone-chaos-op)  │  │ SC-CHAOS-005         │  │ (keystone-chaos-   │  │
│  │                      │  │ (keystone-chaos-     │  │  api)              │  │
│  │ Pattern: operator    │  │  cron)               │  │                    │  │
│  │ self-recovery        │  │                      │  │ Pattern: PDB       │  │
│  │ (no-regression)      │  │ Pattern: workload    │  │ availability       │  │
│  │                      │  │ fault tolerance      │  │ guarantee          │  │
│  └──────────────────────┘  └──────────────────────┘  └────────────────────┘  │
│                                                                              │
│  Phase 3: Concurrent Conflicts and Failover                                  │
│  ┌──────────────────────┐                                                    │
│  │ operator-pod-kill    │                                                    │
│  │ SC-CHAOS-009         │                                                    │
│  │ (keystone-chaos-opk) │                                                    │
│  │                      │                                                    │
│  │ Pattern: operator    │                                                    │
│  │ pod kill (all) with  │                                                    │
│  │ failover reconcile   │                                                    │
│  └──────────────────────┘                                                    │
│                                                                              │
│  Phase 4: Network Chaos                                                      │
│  ┌──────────────────────┐  ┌──────────────────────┐                          │
│  │ mariadb-network-     │  │ mariadb-network-     │                          │
│  │ partition            │  │ latency              │                          │
│  │ SC-CHAOS-006         │  │ SC-CHAOS-007         │                          │
│  │ (keystone-chaos-     │  │ (keystone-chaos-     │                          │
│  │  net-part)           │  │  net-lat)            │                          │
│  │                      │  │                      │                          │
│  │ Pattern: degradation │  │ Pattern: latency     │                          │
│  │ and recovery         │  │ tolerance            │                          │
│  │ (NetworkChaos)       │  │ (no-regression)      │                          │
│  └──────────────────────┘  └──────────────────────┘                          │
│                                                                              │
│  All tests run in: namespace openstack                                       │
│  Fault injection: Chaos Mesh PodChaos and NetworkChaos CRDs                  │
│  Infrastructure: MariaDB, Memcached, ESO, OpenBao, Chaos Mesh (pre-deployed) │
└──────────────────────────────────────────────────────────────────────────────┘
```

## Prerequisites

All 9 test suites require the infrastructure stack and Chaos Mesh to be deployed and
healthy.

::: warning Run `WITH_CHAOS_MESH=true make deploy-infra` first
Chaos Mesh is **opt-in** in the kind Quick Start — the default `make deploy-infra`
flow leaves the `chaos-mesh` namespace absent. Run
`WITH_CHAOS_MESH=true make deploy-infra` before `make e2e-chaos`, or `make e2e-chaos`
will fail its preflight check (`chaos-mesh is not installed`). See the
[Enabling Chaos Mesh tip in Quick Start (Extended)](../../quick-start-extended.md#step-3-deploy-the-infrastructure-stack)
for the rationale.
:::

| Prerequisite | Details |
| --- | --- |
| Infrastructure stack | Deployed via `WITH_CHAOS_MESH=true make deploy-infra` (opt-in path; see [Infrastructure E2E Deployment](../infrastructure/e2e-deployment.md)) |
| Chaos Mesh | Installed in `chaos-mesh` namespace by the kind-only opt-in overlay at `deploy/kind/chaos-mesh/` (or by `chaos-mesh/chaos-mesh-action` in CI) |
| Keystone operator | Deployed to the cluster with CRDs installed |
| ESO ExternalSecrets | `keystone-admin`, `keystone-db` synced in `openstack` namespace |
| MariaDB instance | `openstack-db` MariaDB CR Ready in `openstack` namespace |
| Memcached instance | `openstack-memcached` Memcached CR Ready in `openstack` namespace |
| OpenBao instance | Running in `shared-services` namespace |

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

Chaos tests run as a separate `e2e-chaos` GitHub Actions job in the CI workflow.
The job is path-filtered; its four matrix legs gate differently. The pod leg is
blocking, the network, ovn and nova legs are not (see below). See
[CI Workflow — e2e-chaos](../ci-cd/ci-workflow.md#e2e-chaos) for full job documentation.

**Path filter (`e2e_chaos`):** Changes to `tests/e2e-chaos/**`, `hack/**`, `deploy/**`,
`.github/workflows/ci.yaml`, or `.github/actions/**` trigger the job.

A Go code change does not. The pod and network legs alone cost about 62 runner
minutes between them, and a change to an operator is exercised by that operator's own e2e leg;
the chaos suites test recovery behaviour, which is what the `tests_chaos` filter
watches. Apply the `ci:chaos` label to run them against a pull request that
changes something else. On `v*` tag pushes the job is forced active regardless
of which files were touched.

**Trigger conditions:**

| Event | Runs when |
| --- | --- |
| Push to `main` | Not scheduled; the merged pull request already ran it (always on `v*` tags) |
| Pull request | A suite under `tests/e2e-chaos/**` changed, or the `ci:chaos` label is applied |

**Dependencies:** The job depends on all gate jobs (`lint`, `shellcheck`, `test`,
`test-integration`, `verify-codegen`). It only runs if no dependency failed or was
cancelled.

**Per-leg gating (<code v-pre>continue-on-error: ${{ matrix.suite == 'network' || matrix.suite == 'ovn' || matrix.suite == 'nova' }}</code>):** The `pod`
leg is **blocking**: a failure in any PodChaos suite (operator restart, PDB, rotation)
fails the build. The `network`, `ovn` and `nova` legs stay **non-blocking**,
because the `ip_set`/`sch_netem` kernel-module dependency their NetworkChaos
partitions carry remains prone to environment flakiness; their failures are
visible but do not block merges. The three are named rather than the blocking one
negated, so a fifth leg gates merges until it is argued out of that. The
`ci:chaos` PR label runs every leg on demand for pre-validation. `run-chaos`
still works as an alias for it.

**Legs and what they carry:** the `network` leg runs the NetworkChaos suites, the two
Neutron ones (`neutron-mariadb-outage`, `neutron-broker-outage`) among them, and
therefore deploys the neutron-operator and the ovn-operator alongside keystone. The
three Cinder suites (`cinder-operator-pod-kill`, `cinder-broker-outage`,
`cinder-nfs-outage`) run on that leg alone, so it deploys the cinder-operator into
`cinder-system` as well and sets `WITH_NFS=true` and `WITH_MESSAGING=true` on the
infrastructure bring-up: each of the three attaches an NFS backend the operator
mounts as an inline CSI volume, and two of them (`cinder-operator-pod-kill`,
`cinder-nfs-outage`) take a vhost on the shared broker. Two of the three are
there for the leg's Cinder stack rather than for its fault class, so they inherit
a `continue-on-error` the gate below justifies with a kernel-module dependency
neither of them has: `cinder-operator-pod-kill` injects `PodChaos`, the class the
blocking leg exists for, and `cinder-nfs-outage` writes no chaos-mesh CR at all.
Each is the only automated check on the invariant it pins — operational
independence and the fail-closed write path — and a regression in either goes red
on a leg that reports green. Moving them onto the pod leg means taking the Cinder
stack, the NFS export and the broker there with them, which the blocking leg's
`blacksmith-4vcpu-ubuntu-2404` runner has not been sized for; it is tracked as
a follow-up rather than done here. The
`ovn` leg is the third one, on the `self-hosted` runners as well and
`continue-on-error` like the network one. It sets `WITH_OVN_KERNEL_MODULES=true` so
`hack/deploy-infra.sh` modprobes `openvswitch` and `geneve` on the host (the chassis
DaemonSets cannot start without them), deploys the ovn-operator alone, and runs
`ovn-southbound-outage`. It is
separate from the network leg because it builds a datapath on the node itself: its
probe pods own `/run/openvswitch` and `/run/netns`, which no other suite may hold at
the same time.

The `nova` leg is the fourth, on the `self-hosted` runners and
`continue-on-error` like the other two. It carries the heaviest stack in the job:
each of its three suites (`nova-broker-outage`, `nova-mariadb-outage`,
`nova-placement-outage`) brings up a Keystone, and the first and third add an
OVNCentral, a Neutron, a Placement, a Glance and a fake-driver compute behind the
Nova. On top of the keystone stack it deploys the placement, ovn, neutron and
nova operators, and it sets `WITH_MESSAGING=true`: every Nova process dials the
bus, `nova-mariadb-outage` and `nova-placement-outage` take a vhost on the shared
broker, and `nova-broker-outage` brings a RabbitmqCluster of its own, because its
fault severs a whole broker. The suites run on a leg of their own rather than on
the network one, which already spends 78 of its 90 minutes and deploys no
placement-operator.

**Timeout:** 90 minutes to accommodate serial test execution and longer recovery
assertion windows.

## Test Suite Inventory

| Suite | Scenario ID | CR Name | Test Pattern | Condition Assertions |
| --- | --- | --- | --- | --- |
| [mariadb-pod-kill](#mariadb-pod-kill) | SC-CHAOS-001 | `keystone-chaos-db` | Degradation and recovery | `DatabaseReady=False` → `DatabaseReady=True`, `Ready=True` |
| [memcached-pod-kill](#memcached-pod-kill) | SC-CHAOS-002 | `keystone-chaos-mc` | No-regression | All 6 conditions remain `True` during outage |
| [openbao-pod-kill](#openbao-pod-kill) | SC-CHAOS-003 | `keystone-chaos-bao` | Degradation and recovery | `SecretsReady=False` → `SecretsReady=True`, `Ready=True` |
| [operator-pod-crash](#operator-pod-crash) | SC-CHAOS-004 | `keystone-chaos-op` | Operator self-recovery (no-regression) | Operator pod `Ready=false` → `Ready=true`, CR `Ready=True` maintained |
| [cronjob-rotation-failure](#cronjob-rotation-failure) | SC-CHAOS-005 | `keystone-chaos-cron` | Workload fault tolerance | `FernetKeysReady=True` maintained, `Ready=True` maintained |
| [mariadb-network-partition](#mariadb-network-partition) | SC-CHAOS-006 | `keystone-chaos-net-part` | Degradation and recovery (NetworkChaos) | `DeploymentReady=False`, `Ready=False` → `DeploymentReady=True`, `Ready=True` |
| [mariadb-network-latency](#mariadb-network-latency) | SC-CHAOS-007 | `keystone-chaos-net-lat` | Latency tolerance (no-regression) | `Ready=True` maintained, operator `restartCount=0` |
| [api-pod-kill-pdb](#api-pod-kill-pdb) | SC-CHAOS-008 | `keystone-chaos-api` | PDB availability guarantee | PDB `minAvailable: 1`, `DeploymentReady=True` maintained, `Ready=True` maintained |
| [operator-pod-kill](#operator-pod-kill) | SC-CHAOS-009 | `keystone-chaos-opk` | Operator pod kill (all) with failover reconciliation | All 6 conditions `True` maintained, replica patch reconciled by new leader |
| [deletion-stuck-finalizer](#deletion-stuck-finalizer) | SC-CHAOS-010 | `keystone-chaos-stuck` | Deletion with a downed dependency operator | Keystone CR removed, `FinalizingDatabase`/`DatabaseFinalized` emitted, MariaDB CRs Terminating → removed after recovery |
| [keystone-federation](#keystone-federation) | — | `keystone-chaos-fed` | Federation sidecar container-kill + IdP outage (fail-closed) | Sidecar `restartCount` gated recovery, `Ready=True` restored, federated auth recovers; during IdP outage federated auth non-2xx while password auth stays 201 |
| [ovn-southbound-outage](#ovn-southbound-outage) | — | `ovn-sb-chaos` | Southbound database outage (datapath no-regression) | Ping across the datapath answers under the fault, chassis containers stay at `restartCount: 0`, `SouthboundReady=True` and chassis `Ready=True/AllReady` restored after it |
| [neutron-mariadb-outage](#neutron-mariadb-outage) | — | `neutron-db-chaos` | Database partition (fail-closed write) | `NeutronAPIReady=True/APIHealthy` and `DeploymentReady=True/DeploymentReady` maintained, `POST /v2.0/networks` non-2xx during the partition and 201 after it |
| [neutron-broker-outage](#neutron-broker-outage) | — | `neutron-broker-chaos` | Message-bus partition (no-regression) | `Ready=True/AllReady` maintained, `POST /v2.0/networks` answers 201 throughout, `rabbitmqctl list_connections` empty before, during and after |
| [cinder-operator-pod-kill](#cinder-operator-pod-kill) | — | `cinder-chaos-op` | Operator self-recovery | Every pre-chaos operator and workload pod UID snapshotted, `PodChaos mode: one` replaces one operator pod, a workload pod restart fails the suite, a replica patch afterwards reaches the API Deployment |
| [cinder-broker-outage](#cinder-broker-outage) | — | `cinder-broker-chaos` | Message-bus partition (degrade and recover) | `SchedulerReady=False/WaitingForScheduler` and `VolumeServicesReady=False/WaitingForVolumeServices` within 180 s, `/healthcheck` 200 and API `restartCount` 0 throughout, `POST /v3/volumes` unanswered within 20 s (`BLOCKED-OK`), the blocked create settling to `available` after the partition (`RECOVERED-OK`) |
| [cinder-nfs-outage](#cinder-nfs-outage) | — | `cinder-nfs-chaos` | Storage outage (fail-closed create) | `Ready=True/AllReady` maintained, a create ending in `error` within 240 s (`FAILCLOSED-OK`), a post-recovery create reaching `available` within 240 s (`RECOVERED-OK`) |
| [nova-broker-outage](#nova-broker-outage) | — | `nova-broker-chaos` | Message-bus partition (degrade and recover) | `SchedulerReady=False/WaitingForScheduler`, `ConductorReady=False/WaitingForConductor` and `Ready=False/NotAllReady` within 180 s, the API container ready at `restartCount` 0 throughout, a server create unanswered within 45 s while `GET /` answers 200 (`BLOCKED-OK`), the two conditions True again within 120 s of the lift and the stalled create reaching ACTIVE (`RECOVERED-OK`) |
| [nova-mariadb-outage](#nova-mariadb-outage) | — | `nova-db-chaos` | Database partition (fail-closed read) | `DeploymentReady`, `NovaAPIReady`, `SchedulerReady` and `ConductorReady` all `True` maintained, a Keystone token still obtainable, `GET /v2.1/servers` answering 5xx or timing out and never 4xx (`FAILCLOSED-OK`), the same list answering an empty array before and after (`LIST-OK`) |
| [nova-placement-outage](#nova-placement-outage) | — | `nova-placement-chaos` | Scheduling outage (fail-closed build) | `SchedulerReady=True/SchedulerReady`, `NovaAPIReady=True/APIHealthy` and `Ready=True/AllReady` maintained, `GET /`, a server list and a 202 on create all answered under the fault, the build reaching ERROR within 300 s with a fault that reports an unplaced build (`FAILCLOSED-OK`), a fresh create reaching ACTIVE after the lift (`RECOVERED-OK`) |

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
re-resolving onto the replacement pod. Deployment-level polling
watches the persistent Deployment object and is resilient to pod replacements, matching
the pattern used by `operator-pod-crash` and `api-pod-kill-pdb`.

---

### openbao-pod-kill

**File:** `tests/e2e-chaos/openbao-pod-kill/chainsaw-test.yaml`

**Scenario:** SC-CHAOS-003

**Purpose:** Validates that the Keystone operator detects an OpenBao outage via ESO
ExternalSecret sync failures, transitions `SecretsReady` to `False`, and recovers when
OpenBao returns and ESO resumes syncing.

**Cross-namespace targeting:** OpenBao runs in the `shared-services` namespace, not
`openstack`. The PodChaos CR is created in `openstack` but targets `shared-services` via
`selector.namespaces`. Chaos Mesh has cluster-wide RBAC enabling this.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-chaos-bao` with database `keystone_chaos_bao` |
| 2 | Assert baseline | `assert` (5m) | SecretsReady=True and Ready=True with reason AllReady |
| 3 | Inject PodChaos | `apply` | Applies `01-podchaos.yaml` — PodChaos `kill-openbao` targeting `app.kubernetes.io/name: openbao` in `shared-services` |
| 4 | Assert degradation | `assert` (5m) | SecretsReady=False — ESO cannot reach OpenBao |
| 5 | Delete PodChaos | `delete` | Removes PodChaos `kill-openbao` to lift the fault |
| 6 | Assert recovery | `assert` (5m) | SecretsReady=True and Ready=True with reason AllReady |

**Fixtures:** `00-keystone-cr.yaml`, `01-podchaos.yaml`

**Catch blocks:** Steps 2, 4, and 6 include catch blocks dumping Keystone CR status,
OpenBao pod status (in `shared-services`), Chaos Mesh experiment status, ESO ExternalSecret
conditions (via `jsonpath='{.status.conditions}'`), operator logs (including `--previous`),
and namespace events.

---

### operator-pod-crash

**File:** `tests/e2e-chaos/operator-pod-crash/chainsaw-test.yaml`

**Scenario:** SC-CHAOS-004

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
| 3 | Inject PodChaos | `apply` | Applies `01-podchaos.yaml` — PodChaos `kill-operator` targeting `app.kubernetes.io/name: keystone-operator` in `keystone-system` (the operator controller lives in its own Namespace; the PodChaos CR itself is still created in the test's `openstack` namespace) |
| 4 | Wait for operator pod crash and recovery | `wait` (2m + 2m) | Condition-based waits: operator pod `Ready=false` (kill took effect), then `Ready=true` (Deployment controller restarted pod) |
| 5 | Delete PodChaos | `delete` | Removes PodChaos `kill-operator` to clean up |
| 6 | Assert Ready=True after re-reconciliation | `assert` (5m) | Ready=True with reason AllReady — no sub-condition stuck in False state |

**Fixtures:** `00-keystone-cr.yaml`, `01-podchaos.yaml`

**Catch blocks:** Steps 2 and 6 include catch blocks calling `diagnostics.sh` with
appropriate mode (`baseline`/`chaos`) and `--dep-label=app.kubernetes.io/name=keystone-operator --dep-ns=keystone-system`.
Step 4 includes a catch block with chaos diagnostics for the operator pod.

**Design note:** Step 4 uses condition-based waits on the operator pod (`Ready=false` then
`Ready=true`) instead of a fixed sleep. This confirms the kill actually took effect before
proceeding, and is the same pattern used in SC-CHAOS-002. A theoretical race exists where
the kill-and-restart completes faster than Chainsaw's poll interval — see the inline comment
for mitigation guidance if CI flakiness occurs.

---

### cronjob-rotation-failure

**File:** `tests/e2e-chaos/cronjob-rotation-failure/chainsaw-test.yaml`

**Scenario:** SC-CHAOS-005

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

**Scenario:** SC-CHAOS-006

**Purpose:** Validates that a keystone↔MariaDB network partition is surfaced and
recovers autonomously. The partition is enforced on the MariaDB (server) side so it severs
the ClusterIP-routed connection keystone actually uses; the MariaDB cluster CR stays `Ready`
and the operator cannot see the fault from its own vantage point.
Detection happens at the keystone API pods instead: their database-aware readiness probe
fails while they cannot reach MariaDB, the pods are depooled, the Deployment drops below its
desired ready replicas, and the operator reports `DeploymentReady=False` / `Ready=False`.
Lifting the partition restores readiness and the CR returns to `Ready=True`.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-chaos-net-part` with database `keystone_chaos_net_part` |
| 2 | Assert baseline Ready=True | `assert` (5m) | Ready=True with reason AllReady — confirms healthy state before fault injection |
| 3 | Inject NetworkChaos | `apply` | Applies `01-networkchaos.yaml` — NetworkChaos `partition-mariadb` severing keystone↔MariaDB traffic at the MariaDB pods in `openstack` |
| 4 | Assert NetworkChaos injection active | `assert` (5m) | NetworkChaos `partition-mariadb` has `AllInjected=True` — confirms fault is active before checking effects |
| 5 | Assert degradation | `assert` (5m) | DeploymentReady=False and Ready=False — keystone pods fail the database-aware readiness probe and are depooled. `DatabaseReady` stays True (the operator still sees the cluster CR as Ready) |
| 6 | Delete NetworkChaos | `delete` | Removes NetworkChaos `partition-mariadb` to lift the partition |
| 7 | Assert recovery | `assert` (5m) | DeploymentReady=True and Ready=True with reason AllReady |

**Fixtures:** `00-keystone-cr.yaml`, `01-networkchaos.yaml`

**Catch blocks:** Steps 2, 4, 5, and 7 include catch blocks. Step 4 dumps the NetworkChaos
CR status for injection diagnosis. All catch blocks use `diagnostics.sh` with appropriate
mode (`baseline`/`chaos`) and `--dep-label=app.kubernetes.io/name=mariadb`.

**Design notes:**

- Uses `NetworkChaos` with `action: partition` instead of `PodChaos` with `action: pod-kill`,
  simulating a network failure without killing the MariaDB pod.
- The chaos is applied on the MariaDB pods (`selector`) and drops traffic to/from the keystone
  API pods (`target`, `direction: both`). It must be enforced on the server side: keystone
  connects to the ClusterIP Service `openstack-db.openstack.svc:3306`, and a client-side rule
  would match MariaDB pod IPs while the packet still carries the Service ClusterIP (kube-proxy
  DNATs ClusterIP→pod IP later, in the node root namespace), so it would never match. Dropping
  at the MariaDB side works because the packet is already DNATed and carries the keystone pod
  source IP there — the same reason NetworkPolicies match Service-routed traffic by client pod IP.
- Because the MariaDB cluster CR stays `Ready` under a keystone-only partition, detection
  cannot come from the operator's view of the cluster — it comes from the keystone API pods'
  database-aware readiness probe (a TCP connect to the configured DB endpoint, run from
  inside the pod). The probe's connect timeout is sized above the latency scenario's ~12s
  handshake and below an unbounded partition, so a reachable-but-slow database keeps the Pod
  Ready while a partitioned one depools it. The complementary full-outage case (cluster CR
  goes NotReady) is covered by `mariadb-pod-kill`.
- `duration: 600s` is a safety net for auto-expiry if the test does not explicitly delete
  the CR; step 6 lifts the partition explicitly. It must *exceed* the assert window, not
  equal it: an equal duration self-heals at the exact deadline, re-pooling the Deployment
  and flipping `DeploymentReady` back to `True` in the same instant the detect assertion
  times out. This test therefore overrides the default assert timeout to `8m`, because
  end-to-end detection (readiness-probe failures plus the operator observing the Deployment
  drop below its ready replicas) runs close to 5m and the default window left no margin.
- Step 4 verifies `AllInjected=True` before checking the degradation to prevent vacuous
  test passes when the Chaos Mesh selector doesn't match.

---

### mariadb-network-latency

**File:** `tests/e2e-chaos/mariadb-network-latency/chainsaw-test.yaml`

**Scenario:** SC-CHAOS-007

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
  before checking operator stability, replacing a fixed sleep for determinism.
- Restart count is checked by container name (`manager`) rather than by array index to
  avoid false negatives if container ordering changes.

---

### api-pod-kill-pdb

**File:** `tests/e2e-chaos/api-pod-kill-pdb/chainsaw-test.yaml`

**Scenario:** SC-CHAOS-008

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
| 4 | Inject PodChaos | `apply` | Applies `01-podchaos.yaml` — PodChaos `kill-keystone-api` targeting `app.kubernetes.io/name: keystone` AND `app.kubernetes.io/instance: keystone-chaos-api` in `openstack` (the PodChaos resource name retains its historical `kill-keystone-api` label as a chaos-test identifier; the chaos still targets the bare-name `keystone-chaos-api` Pods) <!-- keystone-api-legacy: PodChaos fixture name retained. --> |
| 5 | Verify PDB enforcement and assert conditions | `script` (120s) + `assert` (5m) | Script polls until `readyReplicas < 3` (kill took effect), then asserts `availableReplicas >= 1`; Chainsaw asserts `DeploymentReady=True` and `Ready=True` with reason AllReady |
| 6 | Delete PodChaos | `delete` | Removes PodChaos `kill-keystone-api` to clean up <!-- keystone-api-legacy: PodChaos fixture name retained. --> |
| 7 | Assert Ready=True after recovery | `assert` (5m) | Ready=True with reason AllReady — full replica count restored |

**Fixtures:** `00-keystone-cr.yaml`, `01-podchaos.yaml`

**Catch blocks:** Step 2 calls `diagnostics.sh baseline`. Step 3 catches with PDB describe.
Steps 5 and 7 catch with `diagnostics.sh chaos` using
`--dep-label=app.kubernetes.io/name=keystone,app.kubernetes.io/instance=keystone-chaos-api`.

**Design note:** Step 5 uses a script step to poll Deployment status because Chainsaw's
`wait` with a label selector waits for ALL matching pods, which does not work when the goal
is to verify that NOT ALL pods are down. The script polls `readyReplicas < 3` (confirming
the kill took effect) then asserts `availableReplicas >= 1`. The PDB name follows the
naming convention `subResourceName(keystone)` = `{cr-name}` (bare CR name),
so for CR `keystone-chaos-api`, the PDB is `keystone-chaos-api`.

---

### operator-pod-kill

**File:** `tests/e2e-chaos/operator-pod-kill/chainsaw-test.yaml`

**Scenario:** SC-CHAOS-009

**Purpose:** Validates that the Keystone operator recovers after ALL operator pods are killed
simultaneously (`mode: all`), forcing the Deployment controller to restart every pod and
trigger leader re-election. After recovery, a spec change (replica patch 1→2) verifies the
new leader can actively reconcile — proving operational capability beyond just running.

**Key difference from operator-pod-crash (SC-CHAOS-004):** SC-CHAOS-004 uses `mode: one`,
killing a single operator pod while leaving other replicas running. SC-CHAOS-009 uses
`mode: all`, killing every operator pod simultaneously. SC-CHAOS-004 does not verify
post-failover reconciliation capability; SC-CHAOS-009 patches `spec.deployment.replicas` after
recovery to confirm the new leader processes spec changes end-to-end.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-chaos-opk` with database `keystone_chaos_opk` |
| 2 | Assert baseline Ready=True | `assert` (5m) | Ready=True with reason AllReady — confirms healthy state before chaos injection |
| 3 | Inject chaos and verify pod replacement | `script` (270s) | Snapshots operator pod UIDs, applies `01-podchaos.yaml` (PodChaos `kill-operator-all`, `mode: all`, targets `app.kubernetes.io/name: keystone-operator` in `keystone-system`), waits until none of the pre-chaos UIDs remain, then waits until Deployment `readyReplicas` equals `.spec.replicas` |
| 4 | Delete PodChaos | `delete` | Removes PodChaos `kill-operator-all` to lift the fault |
| 5 | Assert Ready=True after failover | `assert` (5m) | All 6 conditions: SecretsReady=True, FernetKeysReady=True, DatabaseReady=True, DeploymentReady=True, BootstrapReady=True, Ready=True (AllReady) |
| 6 | Patch replicas 1→2 | `patch` | Applies `02-patch-replicas.yaml` — patches `spec.deployment.replicas` to 2 |
| 7 | Assert replica patch and Ready=True | `assert` (5m) | Deployment `keystone-chaos-opk` has `replicas: 2` and `availableReplicas: 2`; Ready=True with reason AllReady |

**Fixtures:** `00-keystone-cr.yaml`, `01-podchaos.yaml`, `02-patch-replicas.yaml`

**Catch blocks:** Step 2 calls `diagnostics.sh baseline`. Steps 3, 5, and 7 call
`diagnostics.sh chaos` with `--dep-label=app.kubernetes.io/name=keystone-operator --dep-ns=keystone-system`.

**Design notes:**

- The operator pod runs in the `keystone-system` Namespace (see
  [Infrastructure Manifests › Keystone Operator](../infrastructure/infrastructure-manifests.md#keystone-operator));
  the operator-managed Keystone workload remains in `openstack`. The PodChaos
  `selector.namespaces` targets `keystone-system` accordingly.

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

### deletion-stuck-finalizer

**File:** `tests/e2e-chaos/deletion-stuck-finalizer/chainsaw-test.yaml`

**Scenario:** SC-CHAOS-010

**Purpose:** Validates the documented single-pass finalizer behaviour when a Keystone CR
is deleted while the mariadb-operator controller is down. `deletion-cleanup` only covers a
healthy mariadb-operator; this suite scales the mariadb-operator controller to 0, deletes
the CR, and asserts the CR is still removed (the finalizer does not wait), the finalizing
events are emitted, and the MariaDB CRs sit in Terminating until the controller is scaled
back up and processes their finalizers.

Unlike the other suites, this one injects the fault with `kubectl scale` rather than a
Chaos Mesh CR, so it needs no Chaos Mesh installation.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-chaos-stuck` with database `keystone_chaos_stuck` |
| 2 | Assert baseline Ready=True | `assert` (5m) | Ready=True with reason AllReady — confirms healthy state before scaling the controller down |
| 3 | Scale mariadb-operator controller to 0 | `script` (90s) | Stashes `.spec.replicas` on the Deployment via a `chaos.cobaltcore.c5c3.io/orig-replicas` annotation, scales `deployment/mariadb-operator` in `mariadb-system` to 0, and waits for `readyReplicas=0` (only the controller — the `mariadb-operator-webhook` Deployment stays up so DELETE is still admitted) |
| 4 | Delete the Keystone CR | `delete` (2m) | The single-pass finalizer must remove the CR even with the controller down; a regression that waited for the MariaDB CRs would wedge here |
| 5 | Assert stuck-finalizer state | `error` + `script` + `assert` (5m) | Keystone CR gone; `FinalizingDatabase` and `DatabaseFinalized` events present; MariaDB `Database`/`User`/`Grant` (all `keystone-chaos-stuck`) still present with `deletionTimestamp` set |
| 6 | Scale mariadb-operator controller back up | `script` (120s) | Restores the controller to the stashed replica count, clears the annotation, and waits for the rollout to complete |
| 7 | Assert deletion completes | `error` (5m) | With the controller processing finalizers again, all three MariaDB CRs are removed |

**Fixtures:** `00-keystone-cr.yaml`

**Catch blocks:** Steps 1–2 call `diagnostics.sh baseline`. Steps 3, 4, 5, and 7 call
`diagnostics.sh chaos` with
`--dep-label=app.kubernetes.io/name=mariadb-operator --dep-ns=mariadb-system --log-label=app.kubernetes.io/name=keystone-operator`.
Steps 3–5 additionally restore the mariadb-operator controller from the stashed annotation
in their catch blocks so a mid-test failure never leaves the operator wedged for later
suites.

**Design notes:**

- Only the mariadb-operator *controller* Deployment is scaled to 0. The separate
  `mariadb-operator-webhook` Deployment stays up, so the apiserver still admits the DELETE
  calls the keystone finalizer issues against the MariaDB CRs.
- The original replica count is stashed on the Deployment as an annotation, not in a
  temp file, because Chainsaw steps cannot pass state between each other.
- Step 5 asserts the *old* state (MariaDB CRs present + Terminating) before Step 7 asserts
  the *new* state (removed), following the assert-absence-of-old-state pattern.

---

### keystone-federation

**File:** `tests/e2e-chaos/keystone-federation/chainsaw-test.yaml`

**Scenario:** — (the architecture chaos catalog entry ships with the
identity-backends implementation chapter)

**Purpose:** Two scenarios against a federated Keystone (2 replicas,
in-suite Keycloak IdP with an `mod_auth_openidc` sidecar in every pod).
First, the repository's first **container-kill**: Chaos Mesh kills the
`federation-proxy` container in one pod (`containerNames` on PodChaos);
kubelet restarts it in place, so recovery is gated on the named container's
`restartCount` before asserting `readyReplicas`, `Ready=True`, and a working
federated bearer flow. Second, a bounded **IdP outage** (pod-failure on
Keycloak): federated login must fail **closed** (non-2xx — the introspection
path cannot validate bearers) while password auth through the very same
proxy keeps answering 201; after the outage the federated flow recovers.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Fixture + federated Keystone | `apply` + `assert` (5m) | Keycloak fixture ready, Keystone `Ready=True`, backend `Ready=True`, sidecar rollout complete (`updatedReplicas == replicas`) |
| 2 | Baseline federated auth | `script` (120s) | ROPC bearer from Keycloak, federated auth through the sidecar answers 201 |
| 3 | Kill the sidecar container | `apply` | PodChaos `container-kill`, `containerNames: [federation-proxy]`, `mode: one` |
| 4 | Restart observed + recovery | `script` (210s) + `assert` + `script` (120s) | `federation-proxy` `restartCount >= 1` (the kill is provably observed), `readyReplicas` back to desired, `Ready=True/AllReady`, federated bearer auth answers 201 again |
| 5 | Delete container-kill chaos | `delete` | Removes PodChaos `kill-federation-proxy` |
| 6 | IdP outage fails closed | `script` (150s) | Fetches a bearer BEFORE applying a bounded (60s) pod-failure on Keycloak inline, then asserts federated auth turns non-2xx while password auth via the proxy stays 201 |
| 7 | Outage ends, federation recovers | `delete` + `assert` + `script` (240s) | Keycloak available again; the full ROPC + federated-auth flow retried until 201 |

**Fixtures:** `00-keycloak.yaml` (single-realm Keycloak with a self-signed
https listener for the introspection endpoint), `01-keystone-cr.yaml`
(replicas 2, federation proxy image), `02-backend-cr.yaml` (explicit
endpoints, introspection with `tlsVerify: false`), `03-container-kill.yaml`

**Catch blocks:** every step calls `../diagnostics.sh` with the Keycloak
dependency label and the keystone instance log label.

**Design note:** the IdP-outage chaos is applied inline from the step-6
script (not a fixture apply) so the probe bearer token is provably fetched
before the IdP disappears; the chaos carries a bounded `duration` so the
fixture heals itself even if the test is interrupted before the explicit
delete.

---

### ovn-southbound-outage

**File:** `tests/e2e-chaos/ovn-southbound-outage/chainsaw-test.yaml`

**Scenario:** —

**Purpose:** A `PodChaos` `pod-failure` fails every Southbound member of the suite's
OVNCentral while the chassis on the node keeps running. The suite pins the property
that makes an OVN dataplane survivable: a chassis forwards on the flows it has already
programmed, so losing the Southbound database costs it new logical state and not the
traffic it is carrying. The datapath is a real one, built the way
`tests/e2e-ovn-overlay/geneve-datapath/` builds its own: an internal OVS port on
`br-int` carrying `external_ids:iface-id`, moved into a network namespace that
configures the MAC and IP the logical port advertises. Both ports sit on the one
chassis, so the ping crosses `br-int` and nothing else. The Open vSwitch DaemonSet stays
ready throughout; the ovn-controller one goes unready by design while the database is
away (see the design notes).

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Label the node | `script` | Labels the first node `openstack.c5c3.io/chassis=true`; the step cleanup removes the label again |
| 2 | Control plane | `apply` + `assert` (5m) | `ovn-sb-chaos` (one member per database, one northd, backup suspended) reaches `Ready=True/AllReady` and publishes `clientSecretName: ovn-sb-chaos-client` |
| 3 | Chassis | `apply` + `assert` (5m) | `ovn-sb-chaos-chassis` reaches `Ready=True/AllReady` with `numberReady: 1` |
| 4 | Logical model | `script` (6m) | A probe pod runs `ovn-nbctl` over the client certificate and creates `chaos-sw` with `lsp-1` (`10.98.0.1`) and `lsp-2` (`10.98.0.2`); sentinel `NB-SETUP-OK`. The step cleanup deletes the switch |
| 5 | Datapath | `script` (6m) | A privileged `hostNetwork` pod on the node binds `chaos-p1` in netns `chaos-1` and `chaos-p2` in `chaos-2`. The step cleanup removes all three namespaces, their OVS ports and every probe pod |
| 6 | Baseline ping | `script` (6m) | `ip netns exec chaos-1 ping -c 3 -W 2 10.98.0.2` answers; sentinel `PING-OK` |
| 7 | Inject PodChaos | `apply` + `script` | `PodChaos/sb-outage` (`pod-failure`, `mode: all`, `duration: 900s`) on the `sb` component, then `kubectl wait --for=condition=AllInjected` |
| 8 | Assert under the fault | `script` + `assert` | `ovn-sbctl --timeout=5 list Chassis` fails on a connection error (`SB-DOWN-OK`), the same ping still answers (`PING-OK`), the OVS DaemonSet keeps `numberReady: 1`, the ovn-controller DaemonSet keeps `currentNumberScheduled: 1`, and every chassis container reports `restartCount: 0` |
| 9 | Lift the fault | `script` + `assert` (5m) | Deletes the PodChaos; `SouthboundReady=True/StatefulSetReady`, the ovn-controller DaemonSet back to `numberReady: 1`, and the OVNChassis back to `Ready=True/AllReady` |
| 10 | Assert post-fault programming | `script` | `lsp-3` is added (`NB-PORT3-OK`), `chaos-p3` is bound in netns `chaos-3`, `chaos-1` reaches `10.98.0.3` (`PING-OK`), and `find Chassis hostname=<node>` returns one row (`CHASSIS-OK`) |

**Fixtures:** `01-ovncentral-cr.yaml`, `02-ovnchassis-cr.yaml`, `03-podchaos.yaml`,
`04-probe-scripts.yaml` (the shell the probe pods run, mounted at `/probe`; its
`common.sh` key holds the client certificate arguments, the retryable-error
vocabulary and the `retry_nbctl` loop the other scripts source).

**Catch blocks:** every step from 2 on calls `../diagnostics.sh` with
`--cr-kind=ovncentral` or `--cr-kind=ovnchassis` and adds the chassis pod logs
(`--all-containers`), `kubectl get podchaos -o yaml`, the `ovs-vsctl show` of the
datapath pod, and the namespace events.

**Design notes:**

- `pod-failure`, not `pod-kill`: the StatefulSet replaces a killed pod within seconds.
  `pod-failure` swaps the container for a pause image, so the member stays scheduled,
  serves nothing, and leaves the member Service without an endpoint for the whole
  window.
- The `ovn-sbctl` probe of step 8 is the control for everything beside it. A ping that
  keeps answering says nothing if the database might still be up. That probe runs once
  with no retry loop and accepts a connection-level failure alone: a TLS error or an
  unclassified non-zero exit fails the suite, because neither is evidence of the outage.
- Under the fault the ovn-controller DaemonSet is asserted scheduled, not ready. Its
  readiness probe is the Southbound connection itself
  (`ovn-appctl connection-status | grep -q connected` in
  `operators/ovn/internal/controller/reconcile_controller.go`), and the operator means
  it that way: a chassis that cannot reach the database serves stale flows and must not
  count as a node a rollout may move on from. The suite's claim is that split. The
  chassis is marked unready and keeps forwarding, and step 9 asserts readiness returns
  once ovn-controller reconnects.
- `restartCount: 0` on every chassis container separates "kept forwarding" from
  "recovered by restarting". Neither DaemonSet declares a liveness probe, so an unready
  ovn-controller is never restarted by the kubelet; a container that had exited and come
  back would have re-programmed `br-int` from scratch.
- `lsp-3` is written only after the fault is lifted, so reaching `10.98.0.3` needs
  northd to translate the port and ovn-controller to claim and program it after
  rejoining the database it lost.
- `duration: 900s` is a safety net for an interrupted run. The two probes that need the
  fault run first, and step 9 lifts it explicitly. Like the `duration` of the two
  Neutron `NetworkChaos` fixtures, it has to outlast everything asserted under it:
  step 8 grants its probes 4m and 6m and then runs two DaemonSet asserts inside the
  300s assert window, and a fault that expires in the middle of that lets
  ovn-controller reconnect and answer the ping the suite reads as proof that the
  datapath forwards without the database.
- Probe pods take their image from the Northbound StatefulSet, so no fixture spells the
  OVN tag the operator resolved, and every probe reads its verdict from the pod log
  after a terminal phase. An attached `kubectl run -i` stream can miss a sentinel
  written before the attach is established.

---

### neutron-mariadb-outage

**File:** `tests/e2e-chaos/neutron-mariadb-outage/chainsaw-test.yaml`

**Scenario:** —

**Purpose:** A `NetworkChaos` partition severs neutron-api from MariaDB, enforced on the
MariaDB (server) side for the reason `mariadb-network-partition` documents. The suite
pins the fail-closed contract of a partitioned database: a write that cannot reach it
must not answer 2xx, and the record it would have created must be absent once the
database is back. The counterpart is pinned in the same window: all three Neutron
container probes GET the API root, which serves the version document without a token
and without touching a table, so `NeutronAPIReady` and `DeploymentReady` stay `True`
and the pod stays pooled while database-backed requests fail. That is the difference
from `mariadb-network-partition`, where keystone's database-aware readiness probe
depools the pods and `DeploymentReady` goes `False`.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` + `assert` (5m) | `keystone-neutron-outage` (database `keystone_neutron_outage`) reaches `Ready=True/AllReady`; the probes authenticate against it |
| 2 | Apply the service stack | `apply` + `assert` (5m) | Broker Secret, `neutron-db-chaos-ovn` and `neutron-db-chaos` (database `neutron_db_chaos`, one API replica, one worker); both CRs reach `Ready=True/AllReady` |
| 3 | Baseline write | `script` (5m) | A python probe on the neutron image takes a project-scoped admin token and `POST /v2.0/networks` answers 201; sentinel `WRITE-OK` |
| 4 | Inject NetworkChaos | `apply` | `partition-mariadb-neutron` (`action: partition`, `direction: both`, `duration: 600s`) drops traffic between the MariaDB pods and the API pods of this CR. The step cleanup deletes it |
| 5 | Assert injection active | `script` (60s) | `kubectl wait networkchaos/partition-mariadb-neutron --for=condition=AllInjected` |
| 6 | Assert fail-closed | `script` (5m) + `assert` | `GET /` stays 200, a token is still obtainable, and a `POST` named `db-outage-failclosed` returns 5xx or times out within 90s and never 2xx (`FAILCLOSED-OK`); `NeutronAPIReady=True/APIHealthy` and `DeploymentReady=True/DeploymentReady` |
| 7 | Lift the partition | `delete` | Removes the NetworkChaos |
| 8 | Assert recovery | `script` (5m) | `GET /v2.0/networks?name=db-outage-failclosed` returns an empty list and a fresh `POST` answers 201; sentinel `RECOVERY-OK` |

**Fixtures:** `00-keystone-cr.yaml`, `01-messaging-secret.yaml`, `02-ovncentral-cr.yaml`,
`03-neutron-cr.yaml`, `04-networkchaos.yaml`.

**Catch blocks:** every assert step calls `../diagnostics.sh` with `--cr-kind=neutron`
and `--dep-label=app.kubernetes.io/name=mariadb`, and adds the OVNCentral or Keystone CR
dump, `kubectl get networkchaos -o yaml`, and the neutron-operator logs from
`neutron-system`. `diagnostics.sh` ends every run with the namespace events.

**Design notes:**

- A 4xx during the partition fails the suite. The subject is the database write path,
  and a regression in the auth pipeline (a wrong `serviceUser`, a misrendered
  `[keystone_authtoken]`) would turn every write into a 401 that could otherwise pass
  for proof.
- The fail-closed `POST` is attempted once, bounded at 90 seconds. A retry could land a
  second network under the same name and blur what step 8 reads.
- Step 8 exists because a timeout only proves the client gave up. The request could
  still have been completed by the uWSGI worker it was parked in, and listing by name
  once the database is reachable again is what separates the two outcomes.
- The recovery probe retries a 5xx for up to two minutes. The SQLAlchemy pool holds
  connections that died inside the partition and hands them out once more before it
  reconnects. A 4xx stays fatal there.
- The target is narrowed to `app.kubernetes.io/component: api`. The worker pods and the
  db-sync Job pods carry the same name and instance labels and reach the same database,
  so partitioning them would widen the fault past what the suite asserts.

---

### neutron-broker-outage

**File:** `tests/e2e-chaos/neutron-broker-outage/chainsaw-test.yaml`

**Scenario:** —

**Purpose:** A `NetworkChaos` partition severs Neutron from its RabbitMQ cluster,
enforced on the broker side. The suite pins the finding that makes the outage a
non-event: a pure-OVN Neutron never opens a broker connection at all. The operator
renders `rpc_workers = 0` and the `noop` notification driver, so the transport URL it
builds from the RabbitmqCluster's default-user Secret is configuration nothing dials.
The API keeps answering 201 to network creations throughout, the CR stays `Ready`, and
`rabbitmqctl list_connections` on the broker is empty before, during and after the
fault.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` + `assert` (5m) | `keystone-neutron-broker` (database `keystone_neutron_broker`) reaches `Ready=True/AllReady` |
| 2 | Bring up the broker | `apply` + `script` (11m) | `RabbitmqCluster/neutron-chaos-rabbitmq` (`replicas: 1`) waited on `AllReplicasReady`, the condition the RabbitMQ Cluster Operator does set |
| 3 | Apply the service stack | `apply` + `assert` (5m) | `neutron-broker-ovn` and `neutron-broker-chaos` (managed messaging via `messaging.clusterRef`, database `neutron_broker_chaos`) reach `Ready=True/AllReady` |
| 4 | Baseline | `script` (5m) + `script` (2m) | An authenticated `POST /v2.0/networks` answers 201 (`WRITE-OK`), and `rabbitmqctl list_connections` on `neutron-chaos-rabbitmq-server-0` lists no connection (`NO-CONN-OK`) |
| 5 | Inject NetworkChaos | `apply` + `script` (60s) | `partition-rabbitmq-neutron` (`direction: both`, `duration: 600s`) between the broker pods and every pod of this Neutron, then `kubectl wait --for=condition=AllInjected`. The step cleanup deletes it |
| 6 | Assert no regression | `script` (5m) + `assert` + `script` (2m) | A `POST` still answers 201, the CR stays `Ready=True/AllReady`, and the connection list is still empty |
| 7 | Lift the partition | `delete` + `script` (2m) | Removes the NetworkChaos, waits 30 seconds for a reconnect that must not happen, and reads the connection list once more |
| 8 | Tear the broker down | `script` (8m) | Deletes the Neutron CR and waits for its pods, then deletes the RabbitmqCluster and waits for `neutron-chaos-rabbitmq-server-0` to disappear |

**Fixtures:** `00-keystone-cr.yaml`, `01-rabbitmqcluster.yaml`, `02-ovncentral-cr.yaml`,
`03-neutron-cr.yaml`, `04-networkchaos.yaml`.

**Catch blocks:** the assert steps call `../diagnostics.sh` with `--cr-kind=neutron` and
`--dep-label=app.kubernetes.io/name=neutron-chaos-rabbitmq`, and add
`kubectl get rabbitmqcluster -o yaml`, the broker pod logs, the
rabbitmq-cluster-operator logs from `rabbitmq-system`, and the NetworkChaos dump.

**Design notes:**

- The CR uses managed messaging (`messaging.clusterRef`), which is the mode the fault is
  about. The brownfield `secretRef` mode the other Neutron suites use points at a host
  that never resolves and could not tell a partition from a typo.
- The empty connection list carries the suite. A test that only showed "the partition
  changed nothing" would pass just as well against a Neutron whose transport URL pointed
  at the wrong host; the broker's own accounting is what rules that out.
- `kubectl exec` into the broker keeps working under the fault. The drop rule matches the
  neutron pod IPs, and the exec arrives from the kubelet.
- The target carries no component key, so API and worker pods are both cut off. Sparing
  the workers would leave the half of the deployment meant to consume RPC still
  connected.
- Step 8 tears the bus down through the RabbitmqCluster delete itself, inside the suite.
  The broker pod carries the RabbitMQ Cluster Operator's default
  `terminationGracePeriodSeconds` of 604800 (seven days, so its preStop drain can
  finish), and the operator's own delete path is what labels the pod `skipPreStopChecks`
  and releases the finalizer. `tests/e2e/c5c3/messaging/` documents the same teardown.
  The delete uses foreground propagation: with background propagation the operator's
  deletion path can re-create the broker under the same name
  (rabbitmq/cluster-operator#1864). The Cinder and Nova broker suites tear their bus
  down the same way.
- The RabbitmqCluster fixture sets `replicas: 1` and leaves image, resources and
  persistence at the operator's defaults, the way the ControlPlane projection
  (`ensureRabbitMQ`) leaves them.

---

### cinder-operator-pod-kill

**File:** `tests/e2e-chaos/cinder-operator-pod-kill/chainsaw-test.yaml`

**Scenario:** —

**Purpose:** A `PodChaos` kills one of the two cinder-operator pods. The operator
is a control-plane component: the API, the scheduler and the volume service it
deployed keep running while it is gone, and they keep running while it comes
back. The suite states that as pod identity. Every workload pod UID is
snapshotted before the kill and compared after the recovery, so a restart
anywhere in the deployment fails the test instead of passing as "still Ready".
The second half is the other side of the contract: a replica count patched after
the recovery has to reach the API Deployment, which nothing but a working control
loop does.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then apply the CRs | `script` (2m) + `apply` + `assert` (5m) | `../../e2e/cinder/broker-vhost.sh create cinder-chaos-op cinder-chaos-op-messaging openstack`, then `00-cinder-cr.yaml` (`cinder-chaos-op`) and `01-cinderbackend-cr.yaml` (`chaos-op-nfs1`) reach `Ready=True/AllReady`. The step cleanup deletes the vhost |
| 2 | Inject chaos and verify one operator pod was replaced | `script` (270s) | Three phases in one script, so the snapshot and the polls share state: read `spec.replicas` off the `cinder-operator` Deployment, snapshot the operator pod UIDs in `cinder-system` and the workload pod UIDs in `openstack`, apply `02-podchaos.yaml`, wait up to 120 s for at least one pre-chaos operator UID to disappear, wait up to 120 s for `readyReplicas` to return to the desired count, then compare the workload UIDs against the snapshot |
| 3 | Delete PodChaos | `delete` | Removes `PodChaos/kill-cinder-operator` from `cinder-system` |
| 4 | Prove the recovered operator reconciles a spec change | `patch` + `assert` (5m) | `03-patch-scale.yaml` takes the API to two replicas; Deployment `cinder-chaos-op` reaches `availableReplicas: 2` and `updatedReplicas: 2`, and the CR stays `Ready=True/AllReady` |

**Fixtures:** `00-cinder-cr.yaml`, `01-cinderbackend-cr.yaml`, `02-podchaos.yaml`,
`03-patch-scale.yaml`.

**Catch blocks:** a shared YAML anchor calls `../diagnostics.sh chaos cinder-chaos-op`
with `--cr-kind=cinder`, `--dep-label=app.kubernetes.io/name=cinder-operator` and
`--dep-ns=cinder-system`, dumps the `CinderBackend`, and lists the CR's
Deployments, pods and pod logs.

**Design notes:**

- The snapshot covers every pre-chaos operator pod UID. Recording only
  `.items[0]` is racy: the operator runs two replicas and `PodChaos mode: one`
  picks its victim at random, so the recorded pod survives the kill in about half
  the runs and the old-pod-gone poll fails spuriously.
  `glance-operator-pod-kill` records the same reasoning.
- The workload snapshot is the operational-independence half. The instance label
  covers all four pods the CR owns: the API, the scheduler, the volume service of
  `chaos-op-nfs1`, and the completed db-sync Job pod, which stays listed because
  the migration Job carries no `ttlSecondsAfterFinished`. A UID that changed means
  an operator kill disturbed a data-plane pod, so the step fails with both UID
  lists printed.
- `set -euo pipefail` is omitted in step 2 on purpose. The polling loops use
  `${VAR:-0}` defaults that would abort under `set -e` when kubectl returns empty
  output, and every loop exit condition is checked explicitly instead.

---

### cinder-broker-outage

**File:** `tests/e2e-chaos/cinder-broker-outage/chainsaw-test.yaml`

**Scenario:** —

**Purpose:** A `NetworkChaos` partition severs Cinder from its RabbitMQ cluster,
enforced on the broker side. The suite pins that the three processes react
differently, each the way its role demands. The scheduler and the volume service
take their readiness off the broker socket and go NotReady, so the CR reports
`SchedulerReady=False/WaitingForScheduler`,
`VolumeServicesReady=False/WaitingForVolumeServices` and `Ready=False`. The API
keeps its ready container and never restarts, because its readiness is
`/healthcheck`, which the bus does not reach into. A `POST /v3/volumes` under the
partition does not answer at all: a create casts to the scheduler, and
oslo.messaging retries that publish for as long as the broker is unreachable, so
the request hangs instead of reporting a success the deployment cannot deliver.
After the partition is lifted both processes reconnect on their own and the
blocked create settles like any other.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Bring up the broker | `apply` + `script` (11m) | `00-rabbitmqcluster.yaml` creates `cinder-chaos-rabbitmq` (`replicas: 1`), waited on `AllReplicasReady` for up to 600 s, the condition the RabbitMQ Cluster Operator does set |
| 2 | Apply the Cinder CR and its backend, assert Ready | `apply` + `assert` (5m) | `01-cinder-cr.yaml` (`cinder-broker-chaos`) and `02-cinderbackend-cr.yaml` (`broker-chaos-nfs1`) reach `Ready=True/AllReady` |
| 3 | Baseline | `script` (8m) + `script` (2m) | A probe pod creates a volume through to `available` (`WRITE-OK`), then the scheduler, the volume service and the API are each read for a ready container (`BASELINE-READY-OK`) |
| 4 | Inject NetworkChaos to partition the broker | `apply` + `script` (60s) | `03-networkchaos.yaml` creates `partition-broker-cinder` (`action: partition`, `direction: both`, `duration: 600s`), then `kubectl wait --for=condition=AllInjected`. The step cleanup deletes it |
| 5 | Under the partition | `script` (5m) + `assert` + `script` (2m) + `script` (5m) | The scheduler and volume containers are polled to `ready=false` within 180 s; the CR reports the two waiting reasons and `Ready=False`; the API container reads `ready=true` at `restartCount` 0 (`API-UP-OK`); a probe gets 200 from `/healthcheck` while `POST /v3/volumes` does not answer within `CREATE_TIMEOUT = 20` seconds (`BLOCKED-OK`) |
| 6 | Lift the partition and let the deployment recover itself | `delete` + `script` (5m) + `assert` + `script` (8m) | Both containers are ready again within 120 s, the three conditions return to `SchedulerReady`, `AllVolumeServicesReady` and `AllReady`, and a probe creates a volume that reaches `available` and then waits until every volume the suite created is `available`, the blocked one included (`RECOVERED-OK`) |
| 7 | Tear the broker down through its own operator | `script` (8m) | Deletes the Cinder CR and waits out its pods, then deletes the RabbitmqCluster and waits for `cinder-chaos-rabbitmq-server-0` to disappear |

**Fixtures:** `00-rabbitmqcluster.yaml`, `01-cinder-cr.yaml`,
`02-cinderbackend-cr.yaml`, `03-networkchaos.yaml`.

**Catch blocks:** a shared anchor calls
`../diagnostics.sh chaos cinder-broker-chaos` with `--cr-kind=cinder`,
`--dep-label=app.kubernetes.io/name=cinder-operator`, `--dep-ns=cinder-system`
and `--log-label=app.kubernetes.io/instance=cinder-broker-chaos`, dumps the
`CinderBackend`, lists the CR's pods with their logs and the
`cinder-broker-probe-*` pod logs, and adds
`kubectl get networkchaos,rabbitmqcluster -o yaml`.

**Design notes:**

- The suite brings its own broker, `cinder-chaos-rabbitmq`, rather than taking a
  vhost on the kind-only `shared-rabbitmq`. The fault severs a whole broker, and
  doing that to the shared one would take the rest of the leg with it.
- `partition-broker-cinder` selects the broker pods
  (`app.kubernetes.io/name=cinder-chaos-rabbitmq`) and targets every pod of this
  Cinder with no component key. A client-side rule would match broker pod IPs
  while the packet still carries the Service ClusterIP, because kube-proxy DNATs
  later in the node's root namespace, so it would never match Service-routed
  traffic. Sparing one of the three processes would leave that part of the claim
  untested.
- The conditions take up to 180 s to flip, and they cannot be faster.
  `cinder-amqp-ready` looks for a socket ESTABLISHED to the broker port, and a
  partition drops packets without a FIN or an RST, so the socket stays
  ESTABLISHED until the client itself closes it. What closes it is
  oslo.messaging's heartbeat, which gives up after `heartbeat_timeout_threshold`
  (60 s by default, and the operator renders no override). The probe then needs
  two failures at its 5 s period, so 180 s covers that roughly 70 s worst case
  and stays inside the 600 s the NetworkChaos runs for.
- `CREATE_TIMEOUT = 20` is the client-side cap on the blocked create: long enough
  that a slow but working API is not mistaken for a blocked one, short enough
  that the probe reports while the partition is still up. Neither a 202 nor an
  error status may come back, since both would be the deployment reporting on a
  request it cannot carry out, so a socket timeout is the only outcome that
  passes. `BLOCKED-OK` records it.
- `RECOVERED-OK` carries two claims: the post-partition create reached
  `available`, and nothing the suite asked for is stranded. The create that
  blocked left a row in `creating`, and the API worker still retrying its cast
  publishes it once the broker is reachable, so a row that stays in `creating` is
  a request the deployment accepted and then dropped.
- The NetworkChaos `duration` exceeds the Chainsaw assert window rather than
  equalling it. An equal duration self-heals at the deadline itself, restoring
  broker connectivity in the same instant an assertion is still reading the state
  under the fault.

---

### cinder-nfs-outage

**File:** `tests/e2e-chaos/cinder-nfs-outage/chainsaw-test.yaml`

**Scenario:** —

**Purpose:** The NFS export disappears under a running Cinder. The write path
fails closed while everything above it keeps serving: a create ends in `error`
because the kernel client gives up on the soft mount and the volume service gets
EIO, `/healthcheck` still answers 200, and the `cinder-volume` container is
neither replaced nor restarted. After the server is back, a fresh create reaches
`available` with its file on the share and every volume the suite asked for
deletes.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then apply the CRs | `script` (2m) + `apply` + `assert` (5m) | `../../e2e/cinder/broker-vhost.sh create cinder-nfs-chaos cinder-nfs-chaos-messaging openstack`, then `00-cinder-cr.yaml` (`cinder-nfs-chaos`) and `01-cinderbackend-cr.yaml` (`nfs-chaos-nfs1`). Every sub-condition and `Ready=True/AllReady` are asserted, with `status.volumeServices` carrying the host identity step 4 looks the heartbeat row up by |
| 2 | Baseline | `script` (8m) | A probe pod creates a volume through to `available` within 120 s, and its file is stat'ed on the share |
| 3 | Take the export away | `script` (4m) | Stashes the `cinder-volume` pod UID and restart count as an annotation on its own Deployment, scales `deploy/nfs-server` to 0, and waits until the Service has no endpoint address and no pod left (`OUTAGE-OK`). The step cleanup scales the server back to 1 |
| 4 | Under the outage | `script` (9m) + `assert` + `script` (2m) | A probe gets 200 from `/healthcheck`, then a create is accepted and has to reach `error` within 240 s (`FAILCLOSED-OK`); the registry row is printed as `HEARTBEAT-STATE`, which the step reports without gating on it. The CR still reports `VolumeServicesReady=True/AllVolumeServicesReady` and `Ready=True/AllReady`, and the recorded pod identity is compared against the live one |
| 5 | Bring the export back and let the deployment recover | `script` (5m) + `script` (9m) + `script` (6m) | The server is scaled back to 1 and rolled out; a fresh create reaches `available` within `SETTLE_SECONDS = 240` with its file on the share (`RECOVERED-OK`); a third probe deletes the three volumes the suite is responsible for and waits for each to answer 404 (`CLEANUP-OK`) |
| 6 | Tear the deployment down | `script` (8m) | Deletes the Cinder CR and waits out its pods, then deletes the `CinderBackend` |

**Fixtures:** `00-cinder-cr.yaml`, `01-cinderbackend-cr.yaml`. The suite writes no
chaos-mesh CR.

**Catch blocks:** a shared anchor calls
`../diagnostics.sh chaos cinder-nfs-chaos` with `--cr-kind=cinder`,
`--dep-label=app.kubernetes.io/name=nfs-server`, `--dep-ns=openstack` and
`--log-label=app.kubernetes.io/instance=cinder-nfs-chaos`, dumps the
`CinderBackend`, the NFS server's Deployment, pods and EndpointSlices, the Cinder
pods with their logs, the `cinder-nfs-chaos-probe-*` logs, and the cinder-operator
logs from `cinder-system`.

**Design notes:**

- The outage is a scale-down of `deploy/nfs-server`, not a `NetworkChaos`
  partition. The share is mounted by the `csi-nfs-node` DaemonSet, which runs
  with `hostNetwork: true`, so the kernel NFS client sends from the node's network
  namespace and an iptables rule keyed on the `cinder-volume` pod IP never matches
  a single mount packet. Removing the endpoint the client dials works instead,
  which is what `deletion-stuck-finalizer` does to the mariadb-operator on this
  same leg.
- Recovery takes longer than the fault. A fresh nfsd comes up in its NFSv4 grace
  period, up to 90 s in which it serves reclaims alone, and the client retries
  through it without telling the application. The post-recovery create therefore
  gets 240 s where the baseline gets 120 s.
- The pod identity is recorded as a UID and a restart count together. A restart
  count read on its own falls back to 0 on a replaced pod, which is the value a
  pod that never restarted reports.
- The suite's preamble records a product gap, and step 4 asserts it: the CR stays
  `Ready=True/AllReady` through the outage while every create fails. The mount is
  not something the operator reads, the volume service's readiness is the broker
  socket, and no mount probe and no `BackendsHealthy` condition exist, so a reader
  of the CR alone cannot see this outage. The assertion pins today's behavior
  rather than a wanted one, so a mount probe or a `BackendsHealthy` condition
  added later changes this suite with it. Until then the gap is on the
  operational side too, as a troubleshooting row in
  [Attach an NFS Backend to Cinder](../../guides/cinder/attach-an-nfs-backend.md#troubleshooting).

---

### nova-broker-outage

**File:** `tests/e2e-chaos/nova-broker-outage/chainsaw-test.yaml`

**Scenario:** —

**Purpose:** The message bus disappears under a running Nova. The scheduler and
the conductor take their readiness off the broker socket, so both go NotReady
and the CR reports `Ready=False/NotAllReady`, while the API keeps its ready
container and never restarts: both of its probes GET the version document. A
server create written during the outage does not answer at all, and its build
request is listed BUILD out of the API database. Once the partition is lifted
both processes reconnect on their own, the stalled create finishes, and a fresh
one reaches ACTIVE.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Bring up the broker | `apply` + `script` (11m) | `00-rabbitmqcluster.yaml` (`nova-chaos-rabbitmq`), polled for `AllReplicasReady=True` for up to 600 s, since the RabbitMQ Cluster Operator publishes no Ready condition |
| 2 | Bring up Keystone | `apply` + `assert` (8m) | `keystone-nova-broker-chaos` Ready, because the catalog Job, the four sibling CRs and the Nova all authenticate against it |
| 3 | Register the four services in the catalog | `script` (6m) + `assert` | `02-catalog-setup-job.yaml`, `succeeded: 1` |
| 4 | Bring up the four services the boot path depends on | `apply` + `assert` (8m) | The Neutron transport-URL Secret, `nova-broker-chaos-ovn`, `neutron-nova-broker-chaos`, `placement-nova-broker-chaos`, `glance-nova-broker-chaos` and `glance-nova-broker-chaos-s3` |
| 5 | Seed the image the servers boot from | `script` (6m) + `assert` | `09-image-seed-job.yaml`, `succeeded: 1` |
| 6 | Apply the Nova CR and assert its conditions | `apply` + `assert` (8m) | `nova-broker-chaos` with all fifteen sub-conditions True and `Ready=True/AllReady`, the baseline the partition phase is read against |
| 7 | Start the fake compute and map it into cell1 | `apply` + `assert` + `script` (3m) | `12-fake-compute.yaml` available, then `../../e2e/nova/discover-hosts.sh nova-broker-chaos` |
| 8 | Baseline | `script` (11m) + `script` (2m) | `14-baseline-job.yaml` boots a server end to end (`BASELINE-OK`) and prints how long the create call took; the scheduler, conductor and API containers are recorded ready (`BASELINE-READY-OK`) |
| 9 | Inject NetworkChaos to partition the broker | `apply` + `script` (60s) | `13-networkchaos.yaml` (`partition-rabbitmq-nova`), waited for `AllInjected`. The step cleanup deletes it |
| 10 | Under the partition | `script` (5m) + `assert` + `script` (2m) + `script` (4m) | Both bus consumers report `ready=false` within 180 s; the CR reads `SchedulerReady=False/WaitingForScheduler`, `ConductorReady=False/WaitingForConductor`, `Ready=False/NotAllReady`; the API container is still ready at `restartCount` 0 (`API-UP-OK`); `15-blocked-job.yaml` gets 200 from `GET /` and no answer from a create inside 45 s, then lists the build request as BUILD (`BLOCKED-OK`) |
| 11 | Lift the partition and let the deployment recover | `delete` + `script` (3m) + `assert` + `script` (13m) | Both consumers are ready again within 120 s, the two conditions and `Ready` read True, and `16-recovered-job.yaml` finds the stalled server ACTIVE and creates a second one (`RECOVERED-OK`) |
| 12 | Tear the broker down through its own operator | `script` (12m) | The compute and the three Jobs first, then the Nova and its pods, then the RabbitmqCluster and its broker pod |

**Fixtures:** `00-rabbitmqcluster.yaml`, `01-keystone-cr.yaml`,
`02-catalog-setup-job.yaml`, `03-messaging-secret.yaml`, `04-ovncentral-cr.yaml`,
`05-neutron-cr.yaml`, `06-placement-cr.yaml`, `07-glance-cr.yaml`,
`08-glancebackend-cr.yaml`, `09-image-seed-job.yaml`, `10-metadata-secret.yaml`,
`11-nova-cr.yaml`, `12-fake-compute.yaml`, `13-networkchaos.yaml`,
`14-baseline-job.yaml`, `15-blocked-job.yaml`, `16-recovered-job.yaml`

**Catch blocks:** a shared anchor calls
`../diagnostics.sh chaos nova-broker-chaos` with `--cr-kind=nova`,
`--dep-label=app.kubernetes.io/name=nova-chaos-rabbitmq`, `--dep-ns=openstack`
and `--log-label=app.kubernetes.io/instance=nova-broker-chaos`, dumps the
NetworkChaos and RabbitmqCluster objects, the Keystone, OVNCentral, Neutron,
Placement, Glance and GlanceBackend CRs, the fake compute and the five Job logs,
and the operator logs from `nova-system`, `keystone-system`, `ovn-system`,
`neutron-system`, `placement-system`, `glance-system` and `rabbitmq-system`.

**Design notes:**

- The readiness flip gets 180 seconds rather than one probe period. A partition
  drops packets without a FIN or an RST, so the socket `nova-amqp-ready` reads
  stays ESTABLISHED until the client closes it; the probe documents that blind
  spot at `images/nova/nova-amqp-ready:28-36`. What closes the socket is
  oslo.messaging's heartbeat, which gives up after `heartbeat_timeout_threshold`
  (60 s by default, with no operator override), and the probe then needs one
  more failure at its 5-second period.
- The drop rule is installed on the broker pods and targets the Nova pods. Nova
  dials a ClusterIP, and kube-proxy only DNATs it to a pod IP in the node's root
  namespace, after the packet has left the client pod, so a client-side rule
  would never match a Service-routed packet.
- The blocked create is capped at `timeout 45`. The baseline Job prints the same
  call taking well under 20 seconds on a healthy bus, so a create that answers
  inside 45 has answered rather than stalled. The worker survives the stall
  because `spec.api.uwsgi.harakiri` is nil by default, which leaves the flag off
  the uWSGI command entirely, so nothing kills the blocked request and the cast
  goes out once the broker is back.
- The suite brings its own broker rather than taking a vhost on the kind-only
  `shared-rabbitmq`: the fault severs a whole broker, and doing that to the
  shared one would take the rest of the leg with it. On a broker of its own the
  Nova uses managed messaging, which is the mode the fault is about.

---

### nova-mariadb-outage

**File:** `tests/e2e-chaos/nova-mariadb-outage/chainsaw-test.yaml`

**Scenario:** —

**Purpose:** The databases disappear under the Nova API. The outage fails
requests closed while the process stays healthy: `GET /` still answers 200 and a
Keystone token is still obtainable, while one `GET /v2.1/servers` ends in a 5xx
or a bounded timeout and never in a 4xx or a listing. The four workload
conditions stay True throughout, which is the contract: both API probes GET the
version document, which renders without touching a table, so the pod stays
pooled and only the database-backed requests fail.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost and bring up Keystone | `script` (2m) + `apply` + `assert` (5m) | `../../e2e/cinder/broker-vhost.sh create nova-db-chaos nova-db-chaos-messaging openstack`, then `keystone-nova-db-chaos` Ready. The step cleanup deletes the vhost at the end of the test |
| 2 | Register the compute service in the catalog | `script` (6m) + `assert` | `01-catalog-setup-job.yaml`, `succeeded: 1` |
| 3 | Apply the Nova CR and assert its conditions | `apply` + `assert` (8m) | `nova-db-chaos` with `NovaAPIReady=True/APIHealthy`, `SchedulerReady`, `ConductorReady` and `Ready=True/AllReady`, the three the partition phase reads again |
| 4 | Baseline | `script` (6m) | `05-list-job.yaml` lists zero servers through openstacksdk (the `openstack` CLI needs an image endpoint this suite's catalog does not have), which reads the API database and the cell it points at (`LIST-OK`) |
| 5 | Inject NetworkChaos to partition MariaDB traffic | `apply` + `script` (60s) | `04-networkchaos.yaml` (`partition-mariadb-nova`), waited for `AllInjected`. The step cleanup deletes it |
| 6 | Under the partition | `script` (5m) + `assert` | `06-failclosed-job.yaml` gets 200 from `GET /`, mints a token, watches `GET /v2.1/servers` fail, and gets 200 from `GET /` again afterwards (`FAILCLOSED-OK`); `DeploymentReady`, `NovaAPIReady`, `SchedulerReady` and `ConductorReady` all still True |
| 7 | Delete NetworkChaos to lift the partition | `delete` | `partition-mariadb-nova` removed |
| 8 | Recovery | `script` (6m) | The same `05-list-job.yaml` answers an empty list again within the 120 s it polls, with no pod restart and no operator action in between |
| 9 | Tear the deployment down | `script` (8m) | Deletes the three Jobs best-effort, then the Nova CR, and waits its pods out |

**Fixtures:** `00-keystone-cr.yaml`, `01-catalog-setup-job.yaml`,
`02-metadata-secret.yaml`, `03-nova-cr.yaml`, `04-networkchaos.yaml`,
`05-list-job.yaml`, `06-failclosed-job.yaml`

**Catch blocks:** a shared anchor calls
`../diagnostics.sh chaos nova-db-chaos` with `--cr-kind=nova`,
`--dep-label=app.kubernetes.io/name=mariadb`, `--dep-ns=openstack` and
`--log-label=app.kubernetes.io/instance=nova-db-chaos`, dumps the NetworkChaos
objects, the Keystone CR, the three Job logs, and the operator logs from
`nova-system` and `keystone-system`, plus the events of the `openstack`
namespace.

**Design notes:**

- The partition targets the API pods of this Nova alone. The scheduler, the
  conductor, the metadata API and the console proxy hold database connections of
  their own, and leaving them outside the fault is what lets the suite assert
  they stay Ready while the request path fails. A partition of all five would
  prove the outage failed requests closed and say nothing about what survived
  it.
- The drop rule is installed on the MariaDB pods and targets the Nova API pods,
  for the same reason the broker suite installs its rule server-side: a packet
  still carries the Service ClusterIP when it leaves the client pod, so a
  client-side rule keyed on MariaDB pod IPs would never match.
- The failing read is one attempt bounded at 90 seconds, not a retry loop. A
  retry would blur which attempt the verdict came from, and the bound keeps the
  probe inside the 600 s the NetworkChaos runs for.
- There is no Placement, no Neutron, no Glance and no compute here. A list of
  zero servers is the smallest request that reaches both nova schemas, and a
  deployment with nothing else in it leaves one possible cause for a list that
  fails.

---

### nova-placement-outage

**File:** `tests/e2e-chaos/nova-placement-outage/chainsaw-test.yaml`

**Scenario:** —

**Purpose:** Placement disappears under the scheduler. The claim has two halves:
a Placement outage fails scheduling, and it fails nothing else. `GET /` answers,
a server list answers, and a create is accepted with the 202 it answers on a
healthy cluster, because none of those paths calls Placement. The accepted build
then reaches ERROR with a fault reporting an unplaced build, while the CR stays
`Ready=True/AllReady`: the scheduler's readiness is the broker socket, not its
Placement client.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost | `script` (2m) | `../../e2e/cinder/broker-vhost.sh create nova-placement-chaos nova-placement-chaos-messaging openstack`. The step cleanup deletes the vhost at the end of the test |
| 2 | Bring up Keystone | `apply` + `assert` (8m) | `keystone-nova-placement-chaos` Ready |
| 3 | Register the four services in the catalog | `script` (6m) + `assert` | `02-catalog-setup-job.yaml`, `succeeded: 1` |
| 4 | Bring up the four services the boot path depends on | `apply` + `assert` (8m) | The Neutron transport-URL Secret, `nova-placement-chaos-ovn`, `neutron-nova-placement-chaos`, `placement-nova-placement-chaos`, `glance-nova-placement-chaos` and `glance-nova-placement-chaos-s3` |
| 5 | Seed the image the servers boot from | `script` (6m) + `assert` | `09-image-seed-job.yaml`, `succeeded: 1` |
| 6 | Apply the Nova CR and assert its conditions | `apply` + `assert` (8m) | `nova-placement-chaos` with all fifteen sub-conditions True and `Ready=True/AllReady` |
| 7 | Start the fake compute and map it into cell1 | `apply` + `assert` + `script` (3m) | `12-fake-compute.yaml` available, then `../../e2e/nova/discover-hosts.sh nova-placement-chaos` |
| 8 | Baseline | `script` (11m) | `14-baseline-job.yaml` boots a server to ACTIVE, which means the scheduler reached Placement for candidates and claimed the host it picked (`BASELINE-OK`) |
| 9 | Inject NetworkChaos to partition Placement | `apply` + `script` (60s) | `13-networkchaos.yaml` (`partition-placement-nova`), waited for `AllInjected`. The step cleanup deletes it |
| 10 | Under the partition | `script` (8m) + `assert` | `15-failclosed-job.yaml` proves the serving half and the failing half in one pod: `GET /`, a server list, a create accepted with 202, then a build ending in ERROR inside 300 s with a fault that reports an unplaced build, read by server id (`FAILCLOSED-OK`). The CR still reads `SchedulerReady`, `NovaAPIReady` and `Ready` True |
| 11 | Lift the partition and schedule again | `delete` + `script` (13m) | `16-recovered-job.yaml` creates a fresh server that reaches ACTIVE with no pod restart and no operator action (`RECOVERED-OK`) |
| 12 | Tear the deployment down | `script` (10m) | Deletes the compute and the three Jobs best-effort, then the Nova CR, and waits its pods out |

**Fixtures:** `01-keystone-cr.yaml`, `02-catalog-setup-job.yaml`,
`03-messaging-secret.yaml`, `04-ovncentral-cr.yaml`, `05-neutron-cr.yaml`,
`06-placement-cr.yaml`, `07-glance-cr.yaml`, `08-glancebackend-cr.yaml`,
`09-image-seed-job.yaml`, `10-metadata-secret.yaml`, `11-nova-cr.yaml`,
`12-fake-compute.yaml`, `13-networkchaos.yaml`, `14-baseline-job.yaml`,
`15-failclosed-job.yaml`, `16-recovered-job.yaml`

**Catch blocks:** a shared anchor calls
`../diagnostics.sh chaos nova-placement-chaos` with `--cr-kind=nova`,
`--dep-label=app.kubernetes.io/instance=placement-nova-placement-chaos`,
`--dep-ns=openstack` and
`--log-label=app.kubernetes.io/instance=nova-placement-chaos`, dumps the
NetworkChaos objects, the Keystone, OVNCentral, Neutron, Placement, Glance and
GlanceBackend CRs, the fake compute and the five Job logs, and the operator logs
from `nova-system`, `keystone-system`, `ovn-system`, `neutron-system`,
`placement-system` and `glance-system`.

**Design notes:**

- The ERROR takes its time and cannot do otherwise. The drop rule takes the SYN
  without an ICMP reject, so the scheduler's connection attempt runs out the
  kernel's own retry ladder, roughly 130 seconds of exponential backoff, before
  it sees an error at all. That is why the fail-closed Job gives the ERROR 300
  seconds and why the NetworkChaos runs for 600.
- The partition is scoped to the scheduler pods. The API never calls Placement
  and the conductor only relays to the scheduler over the bus, so cutting all
  five workloads off would leave nothing serving to read the second half of the
  claim against. The fake compute is outside the target for the same reason: it
  reports its inventory into Placement, and cutting that at the same time would
  put a second failure under one verdict.
- The rule is installed on the Placement pods and targets the scheduler pods,
  the same server-side placement the other two Nova suites use, because a
  client-side rule never matches Service-routed traffic.
- The fault is matched on `No valid host` or `allocation_candidates`. The first is
  what the conductor writes when the scheduler returns no candidate. Under a drop
  rule the scheduler's Placement request ends in a `ConnectTimeout` instead, which
  reaches the conductor as a `RemoteError`; nova records that under its class name
  and keeps the request that timed out in the fault details.
- The fault is read by server id. `openstack server show <name>` resolves a name
  through the server list, and nova's list path reads faults from the schema the
  API's `[database]` connection names (the cell), while a buried build and its
  fault live in cell0. Only `GET /servers/<id>` targets the mapped cell.

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
6. Patch `spec.deployment.replicas` from 1 to 2
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
    - <target-namespace>         # openstack or shared-services
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
| `selector.namespaces` | varies | `openstack` for MariaDB/Memcached, `shared-services` for OpenBao |

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
    - keystone-system
    labelSelectors:
      app.kubernetes.io/name: keystone-operator
  gracePeriod: 0
```

| Field | Value | Rationale |
| --- | --- | --- |
| `mode` | `all` | Kills every operator pod — unlike SC-CHAOS-004 (`mode: one`) which leaves other replicas running |
| `selector.namespaces` | `keystone-system` | Operator controller runs in `keystone-system`; the operator-managed Keystone workload stays in `openstack` |

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
├── chainsaw-config.yaml              Chaos-specific Chainsaw configuration
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
│   ├── 01-podchaos.yaml              PodChaos targeting openbao in shared-services
│   └── chainsaw-test.yaml            Test: SecretsReady=False → recovery
├── operator-pod-crash/               SC-CHAOS-004: Operator self-recovery
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-op)
│   ├── 01-podchaos.yaml              PodChaos targeting keystone-operator in keystone-system
│   └── chainsaw-test.yaml            Test: Ready=True maintained after operator restart
├── cronjob-rotation-failure/         SC-CHAOS-005: CronJob fault tolerance
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-cron)
│   ├── 01-podchaos.yaml              PodChaos pod-failure targeting job pods
│   └── chainsaw-test.yaml            Test: FernetKeysReady=True maintained
├── mariadb-network-partition/        SC-CHAOS-006: MariaDB network partition
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-net-part)
│   ├── 01-networkchaos.yaml          NetworkChaos severing keystone↔mariadb traffic (server-side)
│   └── chainsaw-test.yaml            Test: DeploymentReady=False → recovery
├── mariadb-network-latency/          SC-CHAOS-007: MariaDB network latency
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-net-lat)
│   ├── 01-networkchaos.yaml          NetworkChaos injecting 10s latency on keystone→mariadb
│   └── chainsaw-test.yaml            Test: Ready=True maintained, no crash-loop
├── api-pod-kill-pdb/                 SC-CHAOS-008: PDB availability guarantee
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-api, replicas: 3)
│   ├── 01-podchaos.yaml              PodChaos targeting keystone API pods (multi-label)
│   └── chainsaw-test.yaml            Test: PDB minAvailable=1, availability maintained
├── operator-pod-kill/                SC-CHAOS-009: Operator pod kill all + failover
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-opk)
│   ├── 01-podchaos.yaml              PodChaos targeting keystone-operator (mode: all)
│   ├── 02-patch-replicas.yaml        Patch replicas 1→2 for post-failover reconciliation
│   └── chainsaw-test.yaml            Test: Leader re-election, conditions maintained, replica patch
├── deletion-stuck-finalizer/         SC-CHAOS-010: Deletion with mariadb-operator down
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-stuck)
│   └── chainsaw-test.yaml            Test: scale mariadb-operator to 0, delete CR, assert stuck → recovery
├── nova-broker-outage/               Message-bus partition under a Nova
│   ├── 00-rabbitmqcluster.yaml       The broker of this suite (nova-chaos-rabbitmq)
│   ├── 01-keystone-cr.yaml           Keystone keystone-nova-broker-chaos
│   ├── 02-catalog-setup-job.yaml     Compute, placement, image and network catalog rows
│   ├── 03-messaging-secret.yaml      Transport URL for the Neutron beside the Nova
│   ├── 04-ovncentral-cr.yaml         OVNCentral nova-broker-chaos-ovn
│   ├── 05-neutron-cr.yaml            Neutron neutron-nova-broker-chaos
│   ├── 06-placement-cr.yaml          Placement placement-nova-broker-chaos
│   ├── 07-glance-cr.yaml             Glance glance-nova-broker-chaos
│   ├── 08-glancebackend-cr.yaml      S3 backend glance-nova-broker-chaos-s3
│   ├── 09-image-seed-job.yaml        The image the servers boot from
│   ├── 10-metadata-secret.yaml       The metadata shared secret
│   ├── 11-nova-cr.yaml               Nova CR nova-broker-chaos on managed messaging
│   ├── 12-fake-compute.yaml          nova-compute on the fake driver, host fake-1
│   ├── 13-networkchaos.yaml          NetworkChaos severing nova↔broker traffic (server-side)
│   ├── 14-baseline-job.yaml          A server boots on a healthy bus (BASELINE-OK)
│   ├── 15-blocked-job.yaml           GET / serves while a create stalls (BLOCKED-OK)
│   ├── 16-recovered-job.yaml         The stalled create finishes, a new one runs (RECOVERED-OK)
│   └── chainsaw-test.yaml            Test: both bus consumers NotReady → recovery
├── nova-mariadb-outage/              Database partition under the Nova API
│   ├── 00-keystone-cr.yaml           Keystone keystone-nova-db-chaos
│   ├── 01-catalog-setup-job.yaml     The compute catalog row
│   ├── 02-metadata-secret.yaml       The metadata shared secret
│   ├── 03-nova-cr.yaml               Nova CR nova-db-chaos
│   ├── 04-networkchaos.yaml          NetworkChaos severing nova-api↔MariaDB traffic (server-side)
│   ├── 05-list-job.yaml              The server list before and after the fault (LIST-OK)
│   ├── 06-failclosed-job.yaml        GET / serves while the read fails (FAILCLOSED-OK)
│   └── chainsaw-test.yaml            Test: four conditions maintained, the read fails closed
└── nova-placement-outage/            Placement partition under the scheduler
    ├── 01-keystone-cr.yaml           Keystone keystone-nova-placement-chaos
    ├── 02-catalog-setup-job.yaml     Compute, placement, image and network catalog rows
    ├── 03-messaging-secret.yaml      Transport URL for the Neutron beside the Nova
    ├── 04-ovncentral-cr.yaml         OVNCentral nova-placement-chaos-ovn
    ├── 05-neutron-cr.yaml            Neutron neutron-nova-placement-chaos
    ├── 06-placement-cr.yaml          Placement placement-nova-placement-chaos
    ├── 07-glance-cr.yaml             Glance glance-nova-placement-chaos
    ├── 08-glancebackend-cr.yaml      S3 backend glance-nova-placement-chaos-s3
    ├── 09-image-seed-job.yaml        The image the servers boot from
    ├── 10-metadata-secret.yaml       The metadata shared secret
    ├── 11-nova-cr.yaml               Nova CR nova-placement-chaos on a shared-broker vhost
    ├── 12-fake-compute.yaml          nova-compute on the fake driver, host fake-1
    ├── 13-networkchaos.yaml          NetworkChaos severing scheduler↔Placement traffic (server-side)
    ├── 14-baseline-job.yaml          A server boots through a reachable Placement (BASELINE-OK)
    ├── 15-failclosed-job.yaml        The build ends in ERROR, nothing else fails (FAILCLOSED-OK)
    ├── 16-recovered-job.yaml         The scheduler places servers again (RECOVERED-OK)
    └── chainsaw-test.yaml            Test: Ready maintained, the build fails closed
```

## Adding New Scenarios

1. Create a new directory: `tests/e2e-chaos/<target>-<fault-type>/`
2. Add `00-keystone-cr.yaml` with a unique CR name and database name
3. Add `01-<chaos-type>.yaml` with the appropriate Chaos Mesh CRD
4. Add `chainsaw-test.yaml` following the degradation/recovery or no-regression pattern
5. Include catch blocks with diagnostic output on every assert step
6. All files must include the `SPDX-License-Identifier: Apache-2.0` header

## Related Resources
- [Keystone E2E Test Suites](./keystone-e2e-tests.md) — Happy-path E2E tests
- [Keystone Reconciler Architecture](../keystone/keystone-reconciler.md) — Sub-reconciler contracts and condition semantics
- [Infrastructure E2E Deployment](../infrastructure/e2e-deployment.md) — Infrastructure stack deployment
- `tests/e2e-chaos/chainsaw-config.yaml` — Chaos-specific Chainsaw configuration
- `tests/e2e-chaos/README.md` — Quick-start guide
