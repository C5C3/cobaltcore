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

The 45 chaos test suites validate operator behavior during and after fault
injection. Phase 1 covers infrastructure dependency pod kills. Phase 2 adds
operator self-recovery, CronJob workload fault tolerance, and PDB availability
guarantee scenarios. Phase 3 adds an all-pod operator kill with leader
re-election verification. Phase 4 adds network chaos scenarios (partition and
latency). The suites added since cover the other service operators and their
dependencies. Each suite deploys the resources of the service under test,
asserts a healthy baseline, injects a fault, asserts the expected degradation
(or stability), removes the fault, and asserts full recovery. Every suite
except `deletion-stuck-finalizer` and `cinder-nfs-outage` injects with a
[Chaos Mesh](https://chaos-mesh.org/) `PodChaos` or `NetworkChaos`. Those two
scale a Deployment to zero with `kubectl scale` instead. Tests use
[Chainsaw](https://kyverno.github.io/chainsaw/) to orchestrate the assertion
lifecycle.

The first panel of the figure shows the test bed and the six steps a suite
takes. The second shows why a partition rule sits on the server pods, the reason
the partition suites below give for their fixtures. The list describes every
step under its number.

![Two panels. The first shows the chaos test bed and the six steps of a suite. Chainsaw on the runner applies the service resources and asserts the baseline, applies the chaos resource, a PodChaos or a NetworkChaos, waits until the fault is injected, asserts the behaviour under the fault, deletes the chaos resource and asserts recovery. In the cluster, the chaos-controller-manager in the namespace chaos-mesh reconciles the chaos resource and calls the chaos-daemon on the node of a victim. The daemon kills the pod or a container, or sets rules in the network namespace of the pod. The victims are pods in the namespace openstack or in another namespace: shared-services, or the namespace of an operator. The second panel shows why a partition rule sits on the server pods. A client pod dials the ClusterIP of a Service, and kube-proxy rewrites the destination to the address of a server pod only in the root network namespace of the node. A rule in the client pods that matches the addresses of the server pods never sees one, so the partition does nothing. A rule in the server pods matches the source address of the client pod, which the packet still carries, and drops it.](../../diagrams/test-chaos-bed.svg)

1. **Apply the service resources.** The suite applies the resources of the
   service under test and asserts the baseline, `Ready=True` with reason
   `AllReady`.
2. **Apply the chaos resource.** A `PodChaos` or a `NetworkChaos`. It lives in
   `openstack`, or in the namespace of the operator it kills in the eight
   operator suites of the other services. Its `selector` names the victims by
   namespace and label. `chaos-controller-manager` picks them and calls the
   `chaos-daemon` on their node, which kills the pod or a container, or sets
   rules in the network namespace of the pod.
3. **Wait for the injection.** The `NetworkChaos` suites, the six Keystone
   outage suites, `ovn-southbound-outage` and `keystone-federation` run
   `kubectl wait --for=condition=AllInjected` on the chaos resource. The other
   `PodChaos` suites prove the kill on the victim, by a pod UID that is gone
   or a ready replica count that drops, or read it from the condition of
   step 4.
4. **Assert under the fault.** Either the degradation the suite expects (a
   condition turns `False`, a write fails closed) or that nothing regresses
   (`Ready=True` holds, no container restarts).
5. **Delete the chaos resource.** Chaos Mesh lifts the fault. The `duration`
   of the resource is a safety net for a run that never reaches the delete.
   `cronjob-rotation-failure` and the IdP outage of `keystone-federation` let
   a `duration` of 60 seconds end the fault.
6. **Assert recovery.** The conditions return to `True`, and where a write
   failed, a fresh write succeeds.

`deletion-stuck-finalizer` and `cinder-nfs-outage` take the same steps without a
chaos resource: step 2 scales a Deployment to zero, and step 5 scales it back.
Every suite runs in the namespace `openstack`.
[Test Suite Inventory](#test-suite-inventory) lists every suite with its
scenario, its service resource and its assertions.

## Prerequisites

All 45 test suites require the infrastructure stack to be deployed and healthy.
The suites that apply a chaos resource require Chaos Mesh as well.

::: warning Run `WITH_CHAOS_MESH=true make deploy-infra` first
Chaos Mesh is **opt-in** in the kind Quick Start — the default `make deploy-infra`
flow leaves the `chaos-mesh` namespace absent. Run
`WITH_CHAOS_MESH=true make deploy-infra` before `make e2e-chaos`, or `make e2e-chaos`
will fail its preflight check (`chaos-mesh is not installed`). See the
[Enabling Chaos Mesh tip in Quick Start (Extended)](../../quick-start-extended.md#step-3-deploy-the-infrastructure-stack)
for the rationale. The suites run against the kind stack only; Chaos Mesh on
the metal-stack lab is described in
[Lab Chaos Mesh](../infrastructure/infrastructure-manifests.md#lab-chaos-mesh).
:::

| Prerequisite | Details |
| --- | --- |
| Infrastructure stack | Deployed via `WITH_CHAOS_MESH=true make deploy-infra` (opt-in path; see [Infrastructure E2E Deployment](../infrastructure/e2e-deployment.md)) |
| Chaos Mesh | Installed in `chaos-mesh` namespace by the kind-only opt-in overlay at `deploy/kind/chaos-mesh/`; CI sets `WITH_CHAOS_MESH=true` on every matrix entry of the job |
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
| `timeouts.cleanup` | 120s | 3m | Chaos Mesh CRs need time to finalize and release faults; the happy-path value is higher because finalizer-driven Keystone deletion under `parallel: 4` can take longer |
| `execution.parallel` | 1 | 4 | Chaos tests mutate shared infrastructure pod availability; serial execution prevents cross-test interference |
| `execution.failFast` | true | true | Stop on first failure for faster feedback |
| `report.name` | `chainsaw-chaos-report` | `chainsaw-report` | Distinct JUnit report artifact |

Most suites restate the assert timeout of 5 minutes (`5m`) at the spec level.
`mariadb-network-partition` raises it to `8m`, the six suites on the Nova stack raise single
asserts to `8m`, and `deletion-stuck-finalizer` and `ovn-southbound-outage` set no suite-level timeout.

## Workload and Consistency Harness

`tests/e2e-chaos/lib/` holds a workload that sends OpenStack traffic through the
fault of a suite, and the checks that read the state of OpenStack after it.
`lib/workload.py` is the workload: one Python process that uses the standard
library only. `lib/workload.sh` is what a suite calls. It starts the workload in
a `kubectl run` pod on `ghcr.io/c5c3/tempest:2026.1` (`CHAOS_WORKLOAD_IMAGE`
overrides the image), marks the fault window on the pod clock, and judges the
records against the error budget in the suite header. The directory holds no
`chainsaw-test.yaml`, so `make e2e-chaos` does not run it as a suite.

Thirty-four suites start a workload: the fifteen pod and network suites that send
no OpenStack traffic of their own, `nova-placement-outage`, which runs it after its
partition as the proof run of the server, network and image streams, and the
eighteen dependency-fault suites of #1389, from `glance-keystone-outage` to
`neutron-metadata-agent-pod-kill`, which run it through their fault. The
`Workload` line of each suite section below names its streams and its budget.

### Streams

Each stream is one thread that runs cycles one second apart until the stop file
exists. Every HTTP call has a 15-second timeout. The streams that need a token
issue a project-scoped token for the bootstrap admin, whose password `start`
reads from the `keystone-admin` Secret, and issue a new one once when a call
answers 401.

| Stream | Options | Operations of one cycle |
| --- | --- | --- |
| `token` | `--keystone` | `issue`, `validate` |
| `http` | `--http` | `get`; any final status below 400 counts as an answer, Glance's 300 included |
| `network` | `--neutron`, `--keystone` | `create-network`, `create-port`, `delete-port`, `delete-network` |
| `server` | `--nova`, `--image-ref`, `--flavor`, `--keystone` | `create` (microversion 2.37, `"networks": "none"`), `active` (300 s), `delete`, `gone` (120 s) |
| `volume` | `--cinder`, optionally `--keystone` | `create`, `available`, `attach-create`, `attach-update`, `attach-complete`, `in-use`, `detach`, `detached`, `delete`, `gone` |
| `image` | `--glance`, `--keystone` | `create` (raw, bare), `upload` (1 MiB of zero bytes), `active`, `delete` |
| `placement` | `--placement`, `--keystone` | `create-rp` (no microversion, so a 201 with the provider in `Location`), `list-rp` (by name, the one provider it created), `delete-rp` |
| `secret` | `--barbican`, `--keystone` | `create-secret` (a `passphrase` whose payload is its name), `read-payload` (`Accept: text/plain`, the payload byte for byte), `delete-secret` |

The volume stream drives the attachments API at microversions 3.27 and 3.44,
which cinder 28.0.0 defines for it. Without `--keystone` it sends the token
`admin:admin` that the noauth Cinder of the chaos suites accepts. Barbican
writes `secret_ref` under its `host_href`, so the secret stream keeps the uuid
of the reference and its own `--barbican` root. A failed
operation abandons the rest of its cycle, and the stream then deletes what that
cycle created, newest first, with one `cleanup-<kind>` record per resource, so a
fault cannot fill a quota of the admin project. Resources are named
`chaos-wl-<id>-<stream>-<n>`, and a stream prints `READY <stream>` after its
first cycle in which every operation succeeded.

### Records

Every operation writes one JSON line to the pod log:

```json
{"t": 1791987267.893, "s": "token", "op": "issue", "ok": true, "code": 201, "ms": 42, "msg": ""}
```

`t` is the start of the operation on the pod clock. `code` is the HTTP status,
or `null` when the request got no answer at all, such as a refused connection or
a timeout, and `msg` then holds the exception. A wait that the stop file
interrupts writes no record.

Once the stop file exists, every stream finishes or abandons its current
operation. A sweep then deletes every resource named `chaos-wl-<id>-` and writes
`sweep-<kind>` records. A consistency check for each service the workload used
prints `CONSISTENCY-OK <check>` or `CONSISTENCY-FAIL <check>: <detail>`, and the
process prints `DONE` last.

`workload.py evaluate` judges the log on the runner. The fault window runs from
the `fault-start` mark to the `fault-end` mark plus the recovery time of the
budget. A failed record falls inside it when its operation ended (`t` plus `ms`)
at or after `fault-start` and began at or before the deadline, so an operation
the fault caught in flight counts against the budget. The run fails in five
cases:

- a failed record falls outside the window, which every failed sweep record
  does;
- more failed records fall inside the window than the budget allows;
- a stream has no ok record after the recovery deadline;
- a `CONSISTENCY-FAIL` line is present;
- `DONE` is missing.

A failed record that comes before the `READY` line of its stream happened while
the service was still coming up, because `start` waits for `READY`. Evaluate
reports it apart and does not judge it. A run without marks has no window, so
one failed record after `READY` fails it. Evaluate
prints a summary line per stream and up to ten offending records, and exits 0
on a pass, 1 on a violation and 2 on an input error:

```text
stream token: 63 ok, 0 failed in window (budget 0), 0 outside; observed failures=0 recovery=0.0s
```

### Budget line

A suite that starts a workload carries one budget line in its header comment,
with the measurement in the line under it:

```yaml
# Chaos budget: failures=0 recovery=0s
# Chaos budget measured: 2026-10-14, kind, failures 0/0/0, recovery 0/0/0 s
```

`failures` is the number of failed records the fault window may hold.
`recovery` is the time after `fault-end` in which a failure still counts against
that number; after it, every stream has to answer again and no failure is
allowed. Every consistency finding fails the run. `verify` refuses a header
without a budget line, with two of them, or with one that does not match
`^# Chaos budget: failures=([0-9]+) recovery=([0-9]+)s$`.

Budgets are measured on kind. Run the suite three times with
`chainsaw test --config tests/e2e-chaos/chainsaw-config.yaml tests/e2e-chaos/<suite>`,
set `failures` to twice the largest in-window count and `recovery` to the
largest observed recovery plus 30 seconds, rounded up to ten seconds, and record
the three observations in the measured line. A measured line whose environment
reads `estimate` carries an estimate of the fault window that no run has measured
yet, which is where the eighteen suites of #1389 start; three runs replace it. The
budget is zero where the fault does not touch the request path of the stream: the
ten operator pod kills, `cronjob-rotation-failure`, `openbao-pod-kill`,
`nova-placement-outage` and `neutron-metadata-agent-pod-kill`. A failure in one of
them is a defect to find, and raising the budget does not fix it.

### Consistency checks

The workload checks each service it used. A check re-reads for up to 120 seconds
before it reports a failure, so a transition still in flight can finish:

| Check | Fails on |
| --- | --- |
| `servers` | a server in `BUILD` or `ERROR`, or one named with the prefix, in `GET /servers/detail?all_tenants=1` |
| `volumes` | a volume in a transitional status such as `creating`, `attaching`, `reserved` or `deleting` (the full list is in `lib/workload.py`), or one named with the prefix |
| `images` | an image in `saving` or `queued`, or one named with the prefix, on every page of `GET /v2/images` |
| `networks` | a network or a port named with the prefix |
| `resource-providers` | a resource provider named with the prefix in `GET /resource_providers` |
| `secrets` | a secret named with the prefix on every page of `GET /v1/secrets` |

`verify` runs two more checks outside the pod when the suite asks for them:

- `--placement-audit <deployment>` runs
  `nova-manage --config-dir /etc/nova/nova.conf.d placement audit --verbose` in
  the `nova-api` container, which carries `[placement]` and both database
  connections. Exit 0 passes. Exit 3 means orphaned allocations and prints
  `CONSISTENCY-FAIL placement-audit`. Any other code fails with the code and the
  output.
- `--ovn-db-sync <neutron>` creates a Job from the `<neutron>-ovn-db-sync`
  CronJob, waits up to ten minutes for it (`CHAOS_WORKLOAD_DB_SYNC_SECONDS`),
  and fails on every log line that reports a difference between Neutron and
  the OVN Northbound database, as worded in neutron 28.0.1. In log mode the
  utility exits 0 either way, so the log is the verdict, and a log without the
  `Neutron OVN DB sync completed` line fails as well. The CronJob exists only
  when the Neutron CR sets `spec.ovnDBSync`, and
  `{syncMode: log, suspend: true}` keeps it from running on its own while
  `OVNDBSyncReady` stays `True`. A CronJob in repair mode fails the check
  before any Job runs, because repair rewrites the database it reports on.
  [Repair OVN Drift with db-sync](../../guides/neutron/repair-ovn-drift-with-db-sync.md)
  walks through the same Job by hand.

### Adopting the harness

A suite calls `../lib/workload.sh` from its own directory, with `<id>` set to
the directory name. Every call sets `timeout:`, because the chaos configuration
gives a script 30 seconds:

| Call | Where it goes | `timeout:` |
| --- | --- | --- |
| `start <id> -- <run options>` | after the baseline `Ready` assertion, with `stop <id>` in the `cleanup:` of that step | 6m |
| `mark <id> fault-start` | right before the chaos resource is applied, as a script of its own or inside the script that runs `kubectl apply` | 1m |
| `mark <id> fault-end` | right after the Chainsaw `delete` of the chaos resource | 1m |
| `verify <id>` with `--placement-audit` and `--ovn-db-sync` as needed | in a step after the last recovery or reconcile assertion | 15m, 25m in the suites on the Nova stack |
| `dump <id>` | in the `catch:` of every step from `start` to `verify` | 1m |

The pod is named `chaos-wl-<id>` and carries the label
`app.kubernetes.io/name=chaos-workload`, which no chaos selector matches. It
runs as the image's non-root user with a 256 MiB memory limit. `start` waits up
to 300 seconds for `READY` on every stream (`CHAOS_WORKLOAD_READY_SECONDS`).
`verify` waits until the pod clock passes `fault-end` plus the recovery time
plus 30 seconds, or 30 seconds without marks, creates the stop file and waits up
to 420 seconds for `DONE` (`CHAOS_WORKLOAD_DONE_SECONDS`). `dump` and `stop`
always exit 0. A local run pulls the Tempest image from GHCR unless
`kind load docker-image` put it on the node.

## CI Trigger Policy

Chaos tests run as a separate `e2e-chaos` GitHub Actions job in the CI workflow.
The job is path-filtered, and its four matrix entries gate differently. The
`pod` entry is blocking; the `network`, `ovn` and `nova` entries are not (see
below). See [CI Workflow: e2e-chaos](../ci-cd/ci-workflow.md#e2e-chaos) for full
job documentation.

**Path filter (`tests_chaos`):** Only changes under `tests/e2e-chaos/**` trigger the job.

A Go code change does not. The `pod` and `network` entries alone cost about 62
runner minutes between them, and a change to an operator is exercised by that
operator's own e2e job; the chaos suites test recovery behaviour, which is what
the `tests_chaos` filter watches. Apply the `ci:chaos` label to run them against
a pull request that changes something else.

**Trigger conditions:**

| Event | Runs when |
| --- | --- |
| Push to `main` or a `v*` tag | Never; the job runs on `pull_request` events only |
| Pull request | A suite under `tests/e2e-chaos/**` changed, or the `ci:chaos` label is applied |

**Dependencies:** The job depends on `changes`, `lint`, `shellcheck`, `test`,
`test-integration`, `verify-codegen`, `chainsaw-lint`, `build-e2e-images` and
`e2e-operator`. It only runs if `build-e2e-images` succeeded and no dependency failed or was
cancelled.

**Gating per matrix entry (<code v-pre>continue-on-error: ${{ matrix.suite == 'network' || matrix.suite == 'ovn' || matrix.suite == 'nova' }}</code>):**
The `pod` entry is **blocking**: a failure in any PodChaos suite (operator
restart, PDB, rotation) fails the build. The `network`, `ovn` and `nova` entries
stay **non-blocking**, because the `ip_set`/`sch_netem` kernel-module dependency
their NetworkChaos partitions carry remains prone to environment flakiness; their
failures are visible but do not block merges. The expression names the three
non-blocking entries instead of negating the blocking one, so a fifth entry gates
merges until it is argued out of that. The `ci:chaos` PR label runs every entry
on demand for pre-validation. `run-chaos` still works as an alias for it.

**Matrix entries and what they carry:** the `pod` entry runs the PodChaos suites
of the keystone stack, the Keystone outage suites of Glance, Placement and
Barbican, and `horizon-dashboard-pod-kill`, and deploys the horizon, glance,
placement and barbican operators beside keystone. The `network` entry runs the
NetworkChaos suites and every suite that needs the neutron, cinder or ovn
operators: the four Neutron suites (`neutron-mariadb-outage`,
`neutron-broker-outage`, `neutron-keystone-outage`, `neutron-operator-pod-kill`),
`ovn-operator-pod-kill`, the database outages of Glance, Placement, Barbican and
Cinder, and `memcached-network-partition`. It therefore deploys the
neutron-operator, the ovn-operator and the placement-operator alongside keystone,
the last for `placement-mariadb-outage`. The seven Cinder suites
(`cinder-operator-pod-kill`, `cinder-broker-outage`, `cinder-nfs-outage`,
`cinder-keystone-outage`, `cinder-mariadb-outage`, `cinder-volume-pod-kill`,
`cinder-broker-pod-kill`) run in that entry alone, so it deploys the
cinder-operator into `cinder-system` as well and sets `WITH_NFS=true` and
`WITH_MESSAGING=true` on the infrastructure bring-up: each of the seven attaches
an NFS backend the operator mounts as an inline CSI volume, five of them take a
vhost on the shared broker, and the two broker suites bring a RabbitmqCluster of
their own. Several of them run there for the Cinder stack of the entry, and their
fault class would not put them there, so they inherit a `continue-on-error` the
gate below justifies with a kernel-module dependency they do not have:
`cinder-operator-pod-kill`, `cinder-volume-pod-kill` and `cinder-broker-pod-kill`
inject a `PodChaos` kill, the class the blocking entry exists for,
`cinder-keystone-outage` injects a `pod-failure`, and `cinder-nfs-outage` writes
no chaos-mesh CR at all. Each is the only automated check on the invariant it
pins, and a regression in one goes red in an entry that reports green. Moving
them to the `pod` entry means taking the Cinder stack, the NFS export and the
broker there with them, which the blocking entry's `blacksmith-4vcpu-ubuntu-2404`
runner has not been sized for; that move is tracked as a follow-up. The same
holds for the PodChaos suites of the entry that need the neutron-operator or the
ovn-operator. A `Dump diagnostic info (network leg)` step dumps the seven operator
namespaces of the entry with `OPERATOR_ONLY=1`, so a failed operator kill there
keeps the operator logs in the job log. The `ovn` entry is the third one, on the `self-hosted`
runners as well and `continue-on-error` like the network one. It sets
`WITH_OVN_KERNEL_MODULES=true` so `hack/deploy-infra.sh` modprobes `openvswitch`
and `geneve` on the host (the chassis DaemonSets cannot start without them),
deploys the ovn-operator alone, and runs `ovn-southbound-outage`. It is separate
from the `network` entry because it builds a datapath on the node itself: its
probe pods own `/run/openvswitch` and `/run/netns`, which no other suite may hold
at the same time.

The `nova` entry is the fourth, on the `self-hosted` runners and
`continue-on-error` like the other two. It carries the heaviest stack in the job:
each of its six suites (`nova-broker-outage`, `nova-mariadb-outage`,
`nova-placement-outage`, `nova-keystone-outage`, `nova-operator-pod-kill`,
`neutron-metadata-agent-pod-kill`) brings up a Keystone, and all but
`nova-mariadb-outage` add an OVNCentral, a Neutron, a Placement, a Glance and a
fake-driver compute behind the Nova. On top of the keystone stack it deploys the
placement, ovn, neutron and nova operators, and it sets `WITH_MESSAGING=true`:
every Nova process dials the bus, five of the six suites take a vhost on the
shared broker, and `nova-broker-outage` brings a RabbitmqCluster of its own,
because its fault severs a whole broker. It also sets
`WITH_OVN_KERNEL_MODULES=true`: `neutron-metadata-agent-pod-kill` runs an
OVNChassis and a metadata agent on the node, and the chassis DaemonSets need the
`openvswitch` and `geneve` modules. The suites run in a matrix entry of their own
because their stack does not fit beside the suites of the `network` entry.

Every entry but `ovn` loads `ghcr.io/c5c3/tempest:2026.1` with the keystone
stack, the image the [workload pod](#workload-and-consistency-harness) runs on.

**Timeout:** 150 minutes for the `nova` entry, 135 for `network`, 90 for `pod` and 45 for
`ovn`, seven hours in all, to accommodate serial test execution and longer recovery
assertion windows. The last green run before the suites of #1389 (run 38626068128)
took 48, 54, 12 and 69 minutes on `pod`, `network`, `ovn` and `nova`. A
`network` suite took 80 to 370 seconds there and a suite on the Nova stack 11 to 21
minutes, which puts the `network` and `nova` entries at about 122 and 132 minutes
with the new suites.

## Test Suite Inventory

| Suite | Scenario ID | CR Name | Test Pattern | Condition Assertions |
| --- | --- | --- | --- | --- |
| [mariadb-pod-kill](#mariadb-pod-kill) | SC-CHAOS-001 | `keystone-chaos-db` | Degradation and recovery | `DatabaseReady=False` → `DatabaseReady=True`, `Ready=True` |
| [memcached-pod-kill](#memcached-pod-kill) | SC-CHAOS-002 | `keystone-chaos-mc` | No-regression | All 6 conditions remain `True` during outage |
| [openbao-pod-kill](#openbao-pod-kill) | SC-CHAOS-003 | `keystone-chaos-bao` | Degradation and recovery | `SecretsReady=False` → `SecretsReady=True`, `Ready=True` |
| [operator-pod-crash](#operator-pod-crash) | SC-CHAOS-004 | `keystone-chaos-op` | Operator self-recovery (no-regression) | At least one pre-chaos operator pod UID gone, operator Deployment `readyReplicas` back to `spec.replicas`, CR `Ready=True` before and after |
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
| [cinder-nfs-outage](#cinder-nfs-outage) | — | `cinder-nfs-chaos` | Storage outage (fail-closed create) | `Ready=True/AllReady` maintained, a create ending in `error` within 240 s (`FAILCLOSED-OK`), a lock held through the outage writing again within 180 s of the server's return (`LOCK-KEPT-OK`), a post-recovery create reaching `available` within 240 s (`RECOVERED-OK`) |
| [nova-broker-outage](#nova-broker-outage) | — | `nova-broker-chaos` | Message-bus partition (degrade and recover) | `SchedulerReady=False/WaitingForScheduler`, `ConductorReady=False/WaitingForConductor` and `Ready=False/NotAllReady` within 180 s, the API container ready at `restartCount` 0 throughout, a server create unanswered within 45 s while `GET /` answers 200 (`BLOCKED-OK`), the two conditions True again within 120 s of the lift and the stalled create reaching ACTIVE (`RECOVERED-OK`) |
| [nova-mariadb-outage](#nova-mariadb-outage) | — | `nova-db-chaos` | Database partition (fail-closed read) | `DeploymentReady`, `NovaAPIReady`, `SchedulerReady` and `ConductorReady` all `True` maintained, a Keystone token still obtainable, `GET /v2.1/servers` answering 5xx or timing out and never 4xx (`FAILCLOSED-OK`), the same list answering an empty array before and after (`LIST-OK`) |
| [nova-placement-outage](#nova-placement-outage) | — | `nova-placement-chaos` | Scheduling outage (fail-closed build) | `SchedulerReady=True/SchedulerReady`, `NovaAPIReady=True/APIHealthy` and `Ready=True/AllReady` maintained, `GET /`, a server list and a 202 on create all answered under the fault, two builds reaching ERROR within 150 s each, the first on the baseline's connection and the second on a fresh one, with faults that report an unplaced build and the scheduler log showing three new Placement connections and no reset one (`FAILCLOSED-OK`), a fresh create reaching ACTIVE after the lift (`RECOVERED-OK`) |
| [horizon-memcached-pod-kill](#horizon-memcached-pod-kill) | — | `horizon-chaos-mc` | No-regression | `Ready=True/AllReady` at baseline, after Memcached has recovered and after the PodChaos is deleted, every dashboard container at `restartCount` 0 |
| [horizon-operator-pod-kill](#horizon-operator-pod-kill) | — | `horizon-chaos-op` | Operator self-recovery | Every pre-chaos operator pod UID snapshotted, `PodChaos mode: one` replaces one operator pod, operator `readyReplicas` back at `spec.replicas`, a replica patch afterwards reaches Deployment `horizon-chaos-op` (`availableReplicas: 2`, `updatedReplicas: 2`), `Ready=True/AllReady` |
| [glance-operator-pod-kill](#glance-operator-pod-kill) | — | `glance-chaos-op` | Operator self-recovery | Every pre-chaos operator and workload pod UID snapshotted, `PodChaos mode: one` replaces one operator pod, a missing workload pod UID fails the suite, a replica patch afterwards reaches Deployment `glance-chaos-op` (`availableReplicas: 2`, `updatedReplicas: 2`), `Ready=True/AllReady` |
| [placement-operator-pod-kill](#placement-operator-pod-kill) | — | `placement-chaos-op` | Operator self-recovery | Every pre-chaos operator and workload pod UID snapshotted, `PodChaos mode: one` replaces one operator pod, a missing workload pod UID fails the suite, a replica patch afterwards reaches Deployment `placement-chaos-op` (`availableReplicas: 2`, `updatedReplicas: 2`), `Ready=True/AllReady` |
| [barbican-operator-pod-kill](#barbican-operator-pod-kill) | — | `barbican-chaos-op` | Operator self-recovery | `SecretStoresReady=True/AllStoresProjected` and `Ready=True/AllReady` at baseline, every pre-chaos operator and workload pod UID snapshotted, `PodChaos mode: one` replaces one operator pod, a missing workload pod UID fails the suite, a replica patch afterwards reaches Deployment `barbican-chaos-op` (`availableReplicas: 2`, `updatedReplicas: 2`), `Ready=True/AllReady` |
| [glance-garage-outage](#glance-garage-outage) | — | `glance-garage-chaos` | Object-store partition (fail-closed write) | `BackendsReady=True/AllBackendsProjected` at baseline, `Ready=True/AllReady` maintained, `GET /healthcheck` 200 under the partition, `PUT /v2/images/{id}/file` never 2xx under the partition (`FAILCLOSED-OK`), the image not `active` after the partition (`STAYED-FAILED-OK`), a fresh upload reaching `active` (`WRITE-OK`) |
| [barbican-openbao-outage](#barbican-openbao-outage) | — | `barbican-bao-outage` | Secret-store partition (fail-closed write) | `BarbicanAPIReady=True/APIHealthy` maintained, `GET /healthcheck` 200 before and after the failed write, a Keystone token still obtainable, `POST /v1/secrets` answering 5xx or timing out and never 2xx or 4xx (`FAILCLOSED-OK`), no secret of that name returning the payload after the partition (`STAYED-FAILED-OK`), a fresh secret read back with its payload (`WRITE-OK`) |
| [glance-keystone-outage](#glance-keystone-outage) | — | `glance-ks-outage` | Keystone outage (fail-closed token validation) | `TOKEN-ISSUED` before the `PodChaos`, then `FAILCLOSED-OK`: a token issued before the outage and never shown to Glance gets 401 on `GET /v2/images`; Keystone `DeploymentReady=False` and Glance `Ready=True/AllReady` under the fault; Keystone back at `Ready=True/AllReady` and a fresh token answered 200 (`RECOVERY-OK`) |
| [placement-keystone-outage](#placement-keystone-outage) | — | `placement-ks-outage` | Keystone outage (fail-closed token validation) | `TOKEN-ISSUED` before the `PodChaos`, then `FAILCLOSED-OK`: a token issued before the outage and never shown to Placement gets 503 on `GET /resource_providers`; Keystone `DeploymentReady=False` and Placement `Ready=True/AllReady` under the fault; Keystone back at `Ready=True/AllReady` and a fresh token answered 200 (`RECOVERY-OK`) |
| [barbican-keystone-outage](#barbican-keystone-outage) | — | `barbican-ks-outage` | Keystone outage (fail-closed token validation) | `TOKEN-ISSUED` before the `PodChaos`, then `FAILCLOSED-OK`: a token issued before the outage and never shown to Barbican gets 503 on `GET /v1/secrets`; Keystone `DeploymentReady=False` and Barbican `Ready=True/AllReady` under the fault; Keystone back at `Ready=True/AllReady` and a fresh token answered 200 (`RECOVERY-OK`) |
| [neutron-keystone-outage](#neutron-keystone-outage) | — | `neutron-ks-outage` | Keystone outage (fail-closed token validation) | `TOKEN-ISSUED` before the `PodChaos`, then `FAILCLOSED-OK`: a token issued before the outage and never shown to Neutron gets 503 on `GET /v2.0/networks`; Keystone `DeploymentReady=False` and Neutron `Ready=True/AllReady` under the fault; Keystone back at `Ready=True/AllReady` and a fresh token answered 200 (`RECOVERY-OK`) |
| [cinder-keystone-outage](#cinder-keystone-outage) | — | `cinder-ks-outage` | Keystone outage (fail-closed token validation) | `TOKEN-ISSUED` before the `PodChaos`, then `FAILCLOSED-OK`: a token issued before the outage and never shown to Cinder gets 503 on `GET /v3/{project_id}/volumes`; Keystone `DeploymentReady=False` and Cinder `Ready=True/AllReady` under the fault; Keystone back at `Ready=True/AllReady` and a fresh token answered 200 (`RECOVERY-OK`) |
| [nova-keystone-outage](#nova-keystone-outage) | — | `nova-ks-chaos` | Keystone outage (fail-closed token validation) | `TOKEN-ISSUED` before the `PodChaos`, then `FAILCLOSED-OK`: a token issued before the outage and never shown to Nova gets 503 on `GET /v2.1/servers`; Keystone `DeploymentReady=False` and Nova `Ready=True/AllReady`, `NovaAPIReady=True/APIHealthy`, `SchedulerReady=True` and `ConductorReady=True` under the fault; Keystone back at `Ready=True/AllReady` and a fresh token answered 200 (`RECOVERY-OK`); placement audit and OVN db-sync comparison after the workload |
| [glance-mariadb-outage](#glance-mariadb-outage) | — | `glance-db-chaos` | Database partition (fail-closed write) | `Ready=True/AllReady` maintained, `GET /healthcheck` 200 under the partition, `POST /v2/images` named `db-outage-failclosed` answering 5xx or timing out and never 2xx or 4xx (`FAILCLOSED-OK`), the listing by that name empty after the partition and a fresh image answering 201 (`RECOVERY-OK`) |
| [placement-mariadb-outage](#placement-mariadb-outage) | — | `placement-db-chaos` | Database partition (fail-closed write) | `Ready=True/AllReady` maintained, `GET /` 200 under the partition, `POST /resource_providers` named `db-outage-failclosed` answering 5xx or timing out and never 2xx or 4xx (`FAILCLOSED-OK`), the listing by that name empty after the partition and a fresh provider answering 201 (`RECOVERY-OK`) |
| [barbican-mariadb-outage](#barbican-mariadb-outage) | — | `barbican-db-chaos` | Database partition (fail-closed write) | `BarbicanAPIReady=True/APIHealthy` maintained, `GET /healthcheck` 200 under the partition, `POST /v1/secrets` named `db-outage-failclosed` answering 5xx or timing out and never 2xx or 4xx (`FAILCLOSED-OK`), the listing by that name empty after the partition and a fresh secret answering 201 (`RECOVERY-OK`) |
| [cinder-mariadb-outage](#cinder-mariadb-outage) | — | `cinder-db-chaos` | Database partition (fail-closed write) | `Ready=True/AllReady` maintained, `GET /healthcheck` 200 under the partition, `POST /v3/volumes` named `db-outage-failclosed` answering 5xx or timing out and never 2xx or 4xx (`FAILCLOSED-OK`), the listing by that name empty after the partition and a fresh volume reaching `available` within 240 s (`RECOVERY-OK`) |
| [neutron-operator-pod-kill](#neutron-operator-pod-kill) | — | `neutron-chaos-op` | Operator self-recovery | Every pre-chaos operator and workload pod UID snapshotted (`app.kubernetes.io/instance=neutron-chaos-op`), `PodChaos mode: one` replaces one operator pod, a missing workload pod UID fails the suite, a replica patch afterwards reaches Deployment `neutron-chaos-op` (`availableReplicas: 2`, `updatedReplicas: 2`), `Ready=True/AllReady`, workload budget zero |
| [ovn-operator-pod-kill](#ovn-operator-pod-kill) | — | `ovn-chaos-op` | Operator self-recovery | Every pre-chaos operator and workload pod UID snapshotted (`app.kubernetes.io/instance=ovn-chaos-op` and `neutron-ovn-chaos-op`), `PodChaos mode: one` replaces one operator pod, a missing workload pod UID fails the suite, a replica patch afterwards reaches Deployment `ovn-chaos-op-northd` (`availableReplicas: 2`, `updatedReplicas: 2`), `Ready=True/AllReady`, workload budget zero |
| [nova-operator-pod-kill](#nova-operator-pod-kill) | — | `nova-chaos-op` | Operator self-recovery | Every pre-chaos operator and workload pod UID snapshotted (`app.kubernetes.io/instance=nova-chaos-op`), `PodChaos mode: one` replaces one operator pod, a missing workload pod UID fails the suite, a replica patch afterwards reaches Deployment `nova-chaos-op` (`availableReplicas: 2`, `updatedReplicas: 2`), `Ready=True/AllReady`, workload budget zero |
| [cinder-volume-pod-kill](#cinder-volume-pod-kill) | — | `cinder-volume-kill` | Volume-service kill mid-attach (state left behind) | A volume of the stream seen `reserved` or `attaching` (`VOLUME-ATTACHING`) before the `PodChaos`, the volume pod UID replaced and the Deployment available again, `VolumeServicesReady=True/AllVolumeServicesReady` and `Ready=True/AllReady` within 5 minutes, `verify` with `CONSISTENCY-OK volumes` and no failed `sweep-*` record |
| [cinder-broker-pod-kill](#cinder-broker-pod-kill) | — | `cinder-broker-kill` | Broker pod kill (degrade and recover) | The broker pod UID replaced and the new server pod Ready within 300 s, `SchedulerReady=True/SchedulerReady`, `VolumeServicesReady=True/AllVolumeServicesReady` and `Ready=True/AllReady` within 5 minutes, the API container at `restartCount` 0 (`API-UP-OK`), `verify` with `CONSISTENCY-OK volumes` |
| [memcached-network-partition](#memcached-network-partition) | — | `keystone-chaos-mc-part` | No-regression (NetworkChaos) | Ready and all five sub-conditions `True` on every read for 120 s under `AllInjected`, `Ready=True/AllReady` after the partition |
| [horizon-dashboard-pod-kill](#horizon-dashboard-pod-kill) | — | `horizon-chaos-dash` | Dashboard pod kill (no-regression) | `Ready=True/AllReady` with two replicas at baseline, one of the two pre-chaos dashboard pod UIDs gone, Deployment `horizon-chaos-dash` back at `readyReplicas: 2`, `Ready=True/AllReady` after the kill |
| [neutron-metadata-agent-pod-kill](#neutron-metadata-agent-pod-kill) | — | `nova-meta-chaos-agent` | Metadata agent pod kill (data path) | `BOOT-OK`, then `METADATA-OK` with the server's uuid from a network namespace on the node, the agent pod UID replaced and no metadata answer before the new agent container started (`METADATA-DOWN-OK`), the agent pod Ready again with the CR at `Ready=True/AllReady` and `numberReady: 1`, the metadata answering with the uuid within 180 s (`METADATA-RECOVERED-OK`), workload budget zero with a passing placement audit and db-sync comparison, no netns, OVS port, server, network or node label left |

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
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml`: Keystone CR `keystone-chaos-db` with database `keystone_chaos_db` |
| 2 | Assert baseline Ready=True | `assert` (5m) | Ready=True with reason AllReady, which confirms healthy state before fault injection. `workload.sh start` then starts the workload and waits for `READY`. |
| 3 | Inject PodChaos | `apply` | Applies `01-podchaos.yaml`: PodChaos `kill-mariadb` targeting `app.kubernetes.io/name: mariadb` in `openstack`. `workload.sh mark fault-start` runs right before it. |
| 4 | Assert degradation | `assert` (5m) | DatabaseReady=False — operator detects MariaDB is unavailable |
| 5 | Delete PodChaos | `delete` | Removes PodChaos `kill-mariadb` to lift the fault. `workload.sh mark fault-end` runs right after it. |
| 6 | Assert recovery | `assert` (5m) | DatabaseReady=True and Ready=True with reason AllReady |
| 7 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-keystone-cr.yaml`, `01-podchaos.yaml`

**Workload:** the `token` stream against `keystone-chaos-db`, started in step 2 and judged in step 7 against `failures=36 recovery=50s`. Every step from 2 to 7 also dumps the workload in its catch block.

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
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml`: Keystone CR `keystone-chaos-mc` with database `keystone_chaos_mc` |
| 2 | Assert baseline Ready=True | `assert` (5m) | Ready=True with reason AllReady. `workload.sh start` then starts the workload and waits for `READY`. |
| 3 | Inject PodChaos | `apply` | Applies `01-podchaos.yaml`: PodChaos `kill-memcached` targeting `app.kubernetes.io/name: memcached` in `openstack`. `workload.sh mark fault-start` runs right before it. |
| 4 | Verify chaos effect and assert no-regression | `script` (150s) + `assert` (5m) | Reads desired replica count from Deployment `.spec.replicas`, polls Memcached Deployment `readyReplicas` to confirm chaos took effect (drop below desired replicas) and recovery completed (return to desired replicas), then asserts all 6 conditions: SecretsReady=True, FernetKeysReady=True, DatabaseReady=True, DeploymentReady=True, BootstrapReady=True, Ready=True (AllReady) |
| 5 | Delete PodChaos | `delete` | Removes PodChaos `kill-memcached` to allow recovery. `workload.sh mark fault-end` runs right after it. |
| 6 | Assert Ready=True after recovery | `assert` (5m) | Ready=True with reason AllReady |
| 7 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-keystone-cr.yaml`, `01-podchaos.yaml`

**Workload:** the `token` stream against `keystone-chaos-mc`, started in step 2 and judged in step 7 against `failures=190 recovery=150s`. Every step from 2 to 7 also dumps the workload in its catch block.

**Catch blocks:** Steps 2, 4, and 6 include catch blocks dumping Memcached pod status,
Chaos Mesh experiment status, Keystone CR status, pod logs (including `--previous`), and
namespace events.

**Design note:** Step 4 uses Deployment-level `readyReplicas` polling instead of Pod-level
`kubectl wait` with label selectors. The original `kubectl wait` approach raced with pod
deletion — when Chaos Mesh deletes a pod, the watch errors with `NotFound` because it
resolves the label selector once and watches the specific pod object; it never
re-resolves onto the replacement pod. Deployment-level polling
watches the persistent Deployment object and is resilient to pod replacements, matching
the pattern used by `operator-pod-crash` and `api-pod-kill-pdb`.

---

### openbao-pod-kill

**File:** `tests/e2e-chaos/openbao-pod-kill/chainsaw-test.yaml`

**Scenario:** SC-CHAOS-003

**Purpose:** Validates that the Keystone operator detects an OpenBao outage via ESO
ExternalSecret sync failures, transitions `SecretsReady` to `False`, and recovers when
OpenBao returns and ESO resumes syncing.

**Cross-namespace targeting:** OpenBao runs in the `shared-services` namespace. The
PodChaos CR is created in `openstack` and targets `shared-services` via
`selector.namespaces`, which the cluster-wide RBAC of Chaos Mesh allows.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml`: Keystone CR `keystone-chaos-bao` with database `keystone_chaos_bao` |
| 2 | Assert baseline | `assert` (5m) | SecretsReady=True and Ready=True with reason AllReady. `workload.sh start` then starts the workload and waits for `READY`. |
| 3 | Inject PodChaos | `apply` | Applies `01-podchaos.yaml`: PodChaos `kill-openbao` targeting `app.kubernetes.io/name: openbao` in `shared-services`. `workload.sh mark fault-start` runs right before it. |
| 4 | Assert degradation | `assert` (5m) | SecretsReady=False — ESO cannot reach OpenBao |
| 5 | Delete PodChaos | `delete` | Removes PodChaos `kill-openbao` to lift the fault. `workload.sh mark fault-end` runs right after it. |
| 6 | Re-unseal OpenBao | `script` (180s) | Runs `../unseal-openbao.sh`: the single-replica OpenBao of the kind stack restarts sealed and has no auto-unseal, so the helper unseals `openbao-0` with the keys from Secret `openbao-init-keys` in `shared-services` |
| 7 | Assert recovery | `assert` (5m) | SecretsReady=True and Ready=True with reason AllReady |
| 8 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-keystone-cr.yaml`, `01-podchaos.yaml`; step 6 runs the shared helper
`tests/e2e-chaos/unseal-openbao.sh`

**Workload:** the `token` stream against `keystone-chaos-bao`, started in step 2 and judged in step 8 against `failures=0 recovery=0s`. Every step from 2 to 8 also dumps the workload in its catch block.

**Catch blocks:** Step 2 calls `diagnostics.sh baseline`, which dumps Keystone CR status,
the logs of all pods in `openstack`, OpenBao pod status (in `shared-services`), and namespace
events. Steps 4, 6, and 7 call `diagnostics.sh chaos` with `--eso`, which dumps Keystone CR
status, OpenBao pod status (in `shared-services`), Chaos Mesh experiment status, ESO
ExternalSecret conditions (via `jsonpath='{.status.conditions}'`), operator logs (including
`--previous`), and namespace events.

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
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml`: Keystone CR `keystone-chaos-op` with database `keystone_chaos_op` |
| 2 | Assert baseline Ready=True | `assert` (5m) | Ready=True with reason AllReady, which confirms healthy state before fault injection. `workload.sh start` then starts the workload and waits for `READY`. |
| 3 | Inject chaos and verify pod replacement | `script` (270s) | Snapshots operator pod UIDs, applies `01-podchaos.yaml` (PodChaos `kill-operator`, `mode: one`, targets `app.kubernetes.io/name: keystone-operator` in `keystone-system`; the PodChaos CR itself is created in the test's `openstack` namespace), waits until at least one pre-chaos UID is gone, then waits until Deployment `readyReplicas` equals `.spec.replicas`. The script runs `workload.sh mark fault-start` right before the apply. |
| 4 | Delete PodChaos | `delete` | Removes PodChaos `kill-operator` to clean up. `workload.sh mark fault-end` runs right after it. |
| 5 | Assert Ready=True after re-reconciliation | `assert` (5m) | Ready=True with reason AllReady — no sub-condition stuck in False state |
| 6 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-keystone-cr.yaml`, `01-podchaos.yaml`

**Workload:** the `token` stream against `keystone-chaos-op`, started in step 2 and judged in step 6 against `failures=0 recovery=0s`. Every step from 2 to 6 also dumps the workload in its catch block.

**Catch blocks:** Step 2 calls `diagnostics.sh baseline`. Steps 3 and 5 call
`diagnostics.sh chaos` with
`--dep-label=app.kubernetes.io/name=keystone-operator --dep-ns=keystone-system`.

**Design note:** Step 3 uses identity-based tracking (pod UIDs) instead of a fixed sleep or a
`readyReplicas`-drop poll. With `mode: one` and `gracePeriod: 0`, the kill, reschedule and
ready cycle can complete faster than the 2-second poll observes, so a poll for a transient
drop can see `readyReplicas` at the desired count throughout. Snapshotting the UIDs before
applying the PodChaos and waiting until at least one of them is gone confirms the kill took
effect regardless of timing. The apply and the wait share one script so the snapshot and the
wait share state. SC-CHAOS-009 uses the same pattern.

---

### cronjob-rotation-failure

**File:** `tests/e2e-chaos/cronjob-rotation-failure/chainsaw-test.yaml`

**Scenario:** SC-CHAOS-005

**Purpose:** Validates that the Keystone operator maintains `FernetKeysReady=True` and
`Ready=True` when a manually triggered fernet rotation Job's pods are killed by PodChaos.
The operator's `reconcileFernetKeys()` checks Secret and CronJob existence — not individual
Job run outcomes — so a failed rotation Job should not degrade the CR status.

**Key difference from dependency kills:** This scenario targets workload pods (Job pods
created by a CronJob) instead of infrastructure dependency pods. The PodChaos CR is applied
**before** the Job is created (Step 3 before Step 4) so Chaos Mesh intercepts the Job's pods
on creation.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml`: Keystone CR `keystone-chaos-cron` with database `keystone_chaos_cron` |
| 2 | Assert baseline Ready=True | `assert` (5m) | Ready=True with reason AllReady. `workload.sh start` then starts the workload and waits for `READY`. |
| 3 | Inject PodChaos | `apply` | Applies `01-podchaos.yaml`: PodChaos `fail-cronjob` targeting `job-name: chaos-cron-test` in `openstack` with `pod-failure` action and 60s duration. `workload.sh mark fault-start` runs right before it. |
| 4 | Trigger fernet rotation Job | `script` | Runs `kubectl create job chaos-cron-test --from=cronjob/keystone-chaos-cron-fernet-rotate` |
| 5 | Assert FernetKeysReady=True and Ready=True maintained | `assert` (5m) | `FernetKeysReady=True` and `Ready=True` with reason AllReady — no condition cascade from CronJob failure |
| 6 | Delete PodChaos | `delete` | Removes PodChaos `fail-cronjob` to lift the fault. `workload.sh mark fault-end` runs right after it. |
| 7 | Assert Ready=True after cleanup | `assert` (5m) | Ready=True with reason AllReady — CronJob remains correctly configured |
| 8 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-keystone-cr.yaml`, `01-podchaos.yaml`

**Workload:** the `token` stream against `keystone-chaos-cron`, started in step 2 and judged in step 8 against `failures=0 recovery=0s`. Every step from 2 to 8 also dumps the workload in its catch block.

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
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml`: Keystone CR `keystone-chaos-net-part` with database `keystone_chaos_net_part` |
| 2 | Assert baseline Ready=True | `assert` (8m) | Ready=True with reason AllReady, which confirms healthy state before fault injection. `workload.sh start` then starts the workload and waits for `READY`. |
| 3 | Inject NetworkChaos | `apply` | Applies `01-networkchaos.yaml`: NetworkChaos `partition-mariadb` severing keystone↔MariaDB traffic at the MariaDB pods in `openstack`. `workload.sh mark fault-start` runs right before it. |
| 4 | Assert NetworkChaos injection active | `script` (60s) | `kubectl wait` until NetworkChaos `partition-mariadb` reports `AllInjected` — confirms fault is active before checking effects |
| 5 | Assert degradation | `assert` (8m) | DeploymentReady=False and Ready=False — keystone pods fail the database-aware readiness probe and are depooled. `DatabaseReady` stays True (the operator still sees the cluster CR as Ready) |
| 6 | Delete NetworkChaos | `delete` | Removes NetworkChaos `partition-mariadb` to lift the partition. `workload.sh mark fault-end` runs right after it. |
| 7 | Assert recovery | `assert` (8m) | DeploymentReady=True and Ready=True with reason AllReady |
| 8 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-keystone-cr.yaml`, `01-networkchaos.yaml`

**Workload:** the `token` stream against `keystone-chaos-net-part`, started in step 2 and judged in step 8 against `failures=12 recovery=30s`. Every step from 2 to 8 also dumps the workload in its catch block.

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
  the CR; step 6 lifts the partition explicitly. It must be longer than the assert window,
  because an equal duration self-heals at the exact deadline, re-pooling the Deployment
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

**Purpose:** Validates that a Keystone whose API pods see 10s latency (2s jitter) towards
MariaDB keeps `Ready=True`, and that the operator pods do not restart meanwhile.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml`: Keystone CR `keystone-chaos-net-lat` with database `keystone_chaos_net_lat` |
| 2 | Assert baseline Ready=True | `assert` (5m) | Ready=True with reason AllReady, which confirms healthy state before fault injection. `workload.sh start` then starts the workload and waits for `READY`. |
| 3 | Inject NetworkChaos | `apply` | Applies `01-networkchaos.yaml`: NetworkChaos `latency-mariadb` injecting 10s latency with 2s jitter on keystone→mariadb traffic. `workload.sh mark fault-start` runs right before it. |
| 4 | Assert operator tolerates latency | `script` (120s) + `assert` (5m) | Waits for NetworkChaos `AllInjected=True` via `kubectl wait`, allows one reconciliation cycle (15s), then verifies operator pod `restartCount` remains 0 for all pods by container name (`manager`); asserts Ready=True maintained |
| 5 | Delete NetworkChaos | `delete` | Removes NetworkChaos `latency-mariadb` to lift the latency. `workload.sh mark fault-end` runs right after it. |
| 6 | Assert Ready=True persists | `assert` (5m) | Ready=True with reason AllReady, which confirms no delayed degradation after latency removal |
| 7 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-keystone-cr.yaml`, `01-networkchaos.yaml`

**Workload:** the `token` stream against `keystone-chaos-net-lat`, started in step 2 and judged in step 7 against `failures=0 recovery=30s`. Every step from 2 to 7 also dumps the workload in its catch block.

**Catch blocks:** Steps 2, 4, and 6 include catch blocks using `diagnostics.sh` with
appropriate mode (`baseline`/`chaos`) and `--dep-label=app.kubernetes.io/name=mariadb`.

**Design notes:**

- Uses `NetworkChaos` with `action: delay` instead of `PodChaos`. Injects 10s latency with
  2s jitter at 100% correlation, simulating degraded network conditions without full outage.
- The test verifies no-regression (Ready=True maintained) instead of degradation-recovery,
  because the Keystone API pods should tolerate the latency towards MariaDB.
- `duration: 180s` acts as a safety net for auto-expiry.
- Step 4 uses `kubectl wait --for=condition=AllInjected` to confirm injection is active
  before checking operator stability, replacing a fixed sleep for determinism.
- Restart count is checked by container name (`manager`) instead of by array index to
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
by the operator itself) instead of an external dependency pod. It uses `replicas: 3` in
the CR to trigger PDB creation via `buildPodDisruptionBudget()`, and includes an explicit
PDB assertion step and a script-based availability check.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR (replicas: 3) | `apply` | Applies `00-keystone-cr.yaml`: Keystone CR `keystone-chaos-api` with database `keystone_chaos_api`, replicas: 3 |
| 2 | Assert baseline Ready=True | `assert` (5m) | Ready=True with reason AllReady. `workload.sh start` then starts the workload and waits for `READY`. |
| 3 | Assert PDB exists | `assert` (5m) | PDB `keystone-chaos-api` with `spec.minAvailable: 1` (apiVersion: `policy/v1`) |
| 4 | Inject PodChaos | `apply` | Applies `01-podchaos.yaml`: PodChaos `kill-keystone-api` targeting `app.kubernetes.io/name: keystone` AND `app.kubernetes.io/instance: keystone-chaos-api` in `openstack` (the PodChaos resource name retains its historical `kill-keystone-api` label as a chaos-test identifier; the chaos still targets the bare-name `keystone-chaos-api` Pods). `workload.sh mark fault-start` runs right before it. <!-- keystone-api-legacy: PodChaos fixture name retained. --> |
| 5 | Verify PDB enforcement and assert conditions | `script` (120s) + `assert` (5m) | Script polls until `readyReplicas < 3` (kill took effect), then asserts `availableReplicas >= 1`; Chainsaw asserts `DeploymentReady=True` and `Ready=True` with reason AllReady |
| 6 | Delete PodChaos | `delete` | Removes PodChaos `kill-keystone-api` to clean up. `workload.sh mark fault-end` runs right after it. <!-- keystone-api-legacy: PodChaos fixture name retained. --> |
| 7 | Assert Ready=True after recovery | `assert` (5m) | Ready=True with reason AllReady — full replica count restored |
| 8 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-keystone-cr.yaml`, `01-podchaos.yaml`

**Workload:** the `token` stream against `keystone-chaos-api`, started in step 2 and judged in step 8 against `failures=0 recovery=30s`. Every step from 2 to 8 also dumps the workload in its catch block.

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
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml`: Keystone CR `keystone-chaos-opk` with database `keystone_chaos_opk` |
| 2 | Assert baseline Ready=True | `assert` (5m) | Ready=True with reason AllReady, which confirms healthy state before chaos injection. `workload.sh start` then starts the workload and waits for `READY`. |
| 3 | Inject chaos and verify pod replacement | `script` (270s) | Snapshots operator pod UIDs, applies `01-podchaos.yaml` (PodChaos `kill-operator-all`, `mode: all`, targets `app.kubernetes.io/name: keystone-operator` in `keystone-system`), waits until none of the pre-chaos UIDs remain, then waits until Deployment `readyReplicas` equals `.spec.replicas`. The script runs `workload.sh mark fault-start` right before the apply. |
| 4 | Delete PodChaos | `delete` | Removes PodChaos `kill-operator-all` to lift the fault. `workload.sh mark fault-end` runs right after it. |
| 5 | Assert Ready=True after failover | `assert` (5m) | All 6 conditions: SecretsReady=True, FernetKeysReady=True, DatabaseReady=True, DeploymentReady=True, BootstrapReady=True, Ready=True (AllReady) |
| 6 | Patch replicas 1→2 | `patch` | Applies `02-patch-replicas.yaml`: patches `spec.deployment.replicas` to 2 |
| 7 | Assert replica patch and Ready=True | `assert` (5m) | Deployment `keystone-chaos-opk` has `replicas: 2` and `availableReplicas: 2`; Ready=True with reason AllReady |
| 8 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-keystone-cr.yaml`, `01-podchaos.yaml`, `02-patch-replicas.yaml`

**Workload:** the `token` stream against `keystone-chaos-opk`, started in step 2 and judged in step 8 against `failures=0 recovery=0s`. Every step from 2 to 8 also dumps the workload in its catch block.

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
  the new leader actively processes spec changes, beyond the operator pod running.
- Step 3 uses identity-based tracking (pod UIDs) instead of `readyReplicas`-drop polling. With
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

Like `cinder-nfs-outage`, this suite injects the fault with `kubectl scale` instead of a
Chaos Mesh CR, so it needs no Chaos Mesh installation.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml`: Keystone CR `keystone-chaos-stuck` with database `keystone_chaos_stuck` |
| 2 | Assert baseline Ready=True | `assert` (5m) | Ready=True with reason AllReady, which confirms healthy state before scaling the controller down |
| 3 | Scale mariadb-operator controller to 0 | `script` (90s) | Stashes `.spec.replicas` on the Deployment via a `chaos.cobaltcore.c5c3.io/orig-replicas` annotation, scales `deployment/mariadb-operator` in `mariadb-system` to 0, and waits for `readyReplicas=0` (only the controller — the `mariadb-operator-webhook` Deployment stays up so DELETE is still admitted) |
| 4 | Delete the Keystone CR | `delete` (2m) | The single-pass finalizer must remove the CR even with the controller down; a regression that waited for the MariaDB CRs would wedge here |
| 5 | Assert stuck-finalizer state | `error` + `script` + `assert` (5m) | Keystone CR gone; `FinalizingDatabase` and `DatabaseFinalized` events present; MariaDB `Database`/`User`/`Grant` (all `keystone-chaos-stuck`) still present with `deletionTimestamp` set |
| 6 | Scale mariadb-operator controller back up | `script` (120s) | Restores the controller to the stashed replica count, clears the annotation, and waits for the rollout to complete |
| 7 | Assert deletion completes | `error` (5m) | With the controller processing finalizers again, all three MariaDB CRs are removed |

**Fixtures:** `00-keystone-cr.yaml`

**Workload:** none. The Keystone under test is being deleted, so no service is left to send traffic to.

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
- The original replica count is stashed on the Deployment as an annotation instead of a
  temp file, because Chainsaw steps cannot pass state between each other.
- Step 5 asserts the *old* state (MariaDB CRs present + Terminating) before Step 7 asserts
  the *new* state (removed), following the assert-absence-of-old-state pattern.

---

### keystone-federation

**File:** `tests/e2e-chaos/keystone-federation/chainsaw-test.yaml`

**Scenario:** none (the architecture chaos catalog entry ships with the
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
| 3 | Kill the sidecar container | `apply` + `script` (120s) | PodChaos `container-kill`, `containerNames: [federation-proxy]`, `mode: one`; the script waits for `AllInjected=True` and deletes and re-applies the experiment up to three times when it is not injected |
| 4 | Restart observed + recovery | `script` (210s) + `assert` + `script` (120s) | `federation-proxy` `restartCount >= 1` (the kill is provably observed), `readyReplicas` back to desired, `Ready=True/AllReady`, federated bearer auth answers 201 again |
| 5 | Delete container-kill chaos | `delete` | Removes PodChaos `kill-federation-proxy` |
| 6 | IdP outage fails closed | `script` (150s) | Fetches a bearer BEFORE applying a bounded (60s) pod-failure on Keycloak inline, then asserts federated auth turns non-2xx while password auth via the proxy stays 201 |
| 7 | Outage ends, federation recovers | `delete` + `assert` + `script` (240s) | Keycloak available again; the full ROPC + federated-auth flow retried until 201 |

**Fixtures:** `00-keycloak.yaml` (single-realm Keycloak with a self-signed
https listener for the introspection endpoint), `01-keystone-cr.yaml`
(replicas 2, federation proxy image), `02-backend-cr.yaml` (explicit
endpoints, introspection with `tlsVerify: false`), `03-container-kill.yaml`

**Catch blocks:** every step but step 5, the delete, calls `../diagnostics.sh` with the
Keycloak dependency label; steps 3, 4, 6 and 7 add the keystone instance log label.

**Design note:** the IdP-outage chaos is applied inline from the step-6
script (not a fixture apply) so the probe bearer token is provably fetched
before the IdP disappears; the chaos carries a bounded `duration` so the
fixture heals itself even if the test is interrupted before the explicit
delete.

---

### ovn-southbound-outage

**File:** `tests/e2e-chaos/ovn-southbound-outage/chainsaw-test.yaml`

**Scenario:** none

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

- `pod-failure` instead of `pod-kill`: the StatefulSet replaces a killed pod within seconds.
  `pod-failure` swaps the container for a pause image, so the member stays scheduled,
  serves nothing, and leaves the member Service without an endpoint for the whole
  window.
- The `ovn-sbctl` probe of step 8 is the control for everything beside it. A ping that
  keeps answering says nothing if the database might still be up. That probe runs once
  with no retry loop and accepts a connection-level failure alone: a TLS error or an
  unclassified non-zero exit fails the suite, because neither is evidence of the outage.
- Under the fault the suite asserts the ovn-controller DaemonSet scheduled and leaves
  its readiness out. Its readiness probe is the Southbound connection itself
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

**Scenario:** none

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
- The fail-closed `POST` is attempted once, bounded at 90 seconds. A retry could create a
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

**Scenario:** none

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

**Scenario:** none

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
| 1 | Give the suite its vhost, then apply the CRs | `script` (2m) + `apply` + `assert` (5m) | `../../e2e/cinder/broker-vhost.sh create cinder-chaos-op cinder-chaos-op-messaging openstack`, then `00-cinder-cr.yaml` (`cinder-chaos-op`) and `01-cinderbackend-cr.yaml` (`chaos-op-nfs1`) reach `Ready=True/AllReady`. The step cleanup deletes the vhost. `workload.sh start` then starts the workload and waits for `READY`. |
| 2 | Inject chaos and verify one operator pod was replaced | `script` (270s) | Three phases in one script, so the snapshot and the polls share state: read `spec.replicas` off the `cinder-operator` Deployment, snapshot the operator pod UIDs in `cinder-system` and the workload pod UIDs in `openstack`, apply `02-podchaos.yaml`, wait up to 120 s for at least one pre-chaos operator UID to disappear, wait up to 120 s for `readyReplicas` to return to the desired count, then compare the workload UIDs against the snapshot. The script runs `workload.sh mark fault-start` right before the apply. |
| 3 | Delete PodChaos | `delete` | Removes `PodChaos/kill-cinder-operator` from `cinder-system`. `workload.sh mark fault-end` runs right after it. |
| 4 | Prove the recovered operator reconciles a spec change | `patch` + `assert` (5m) | `03-patch-scale.yaml` takes the API to two replicas; Deployment `cinder-chaos-op` reaches `availableReplicas: 2` and `updatedReplicas: 2`, and the CR stays `Ready=True/AllReady` |
| 5 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-cinder-cr.yaml`, `01-cinderbackend-cr.yaml`, `02-podchaos.yaml`,
`03-patch-scale.yaml`.

**Workload:** the noauth `volume` stream against `cinder-chaos-op`, started in step 1 and judged in step 5 against `failures=0 recovery=0s`. Every step from 1 to 5 also dumps the workload in its catch block.

**Catch blocks:** a shared YAML anchor calls `../diagnostics.sh chaos cinder-chaos-op`
with `--cr-kind=cinder`, `--dep-label=app.kubernetes.io/name=cinder-operator` and
`--dep-ns=cinder-system`, dumps the `CinderBackend`, and lists the CR's
Deployments, pods and pod logs.

**Design notes:**

- The snapshot covers every pre-chaos operator pod UID. Recording only
  `.items[0]` is racy: the operator runs two replicas and `PodChaos mode: one`
  picks its victim at random, so the recorded pod outlives the kill in about half
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

**Scenario:** none

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

- The suite brings its own broker, `cinder-chaos-rabbitmq`, instead of taking a
  vhost on the kind-only `shared-rabbitmq`. The fault severs a whole broker, and
  doing that to the shared one would take the rest of the matrix entry with it.
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
- The NetworkChaos `duration` is longer than the Chainsaw assert window. An equal
  duration self-heals at the deadline itself, restoring
  broker connectivity in the same instant an assertion is still reading the state
  under the fault.

---

### cinder-nfs-outage

**File:** `tests/e2e-chaos/cinder-nfs-outage/chainsaw-test.yaml`

**Scenario:** none

**Purpose:** The NFS export disappears under a running Cinder. The write path
fails closed while everything above it keeps serving: a create ends in `error`
because the kernel client gives up on the soft mount and the volume service gets
EIO, `/healthcheck` still answers 200, and the `cinder-volume` container is
neither replaced nor restarted. After the server is back, a process that held a
POSIX lock on a file of the share through the whole outage writes through it
again, a fresh create reaches `available` with its file on the share, and every
volume the suite asked for deletes.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then apply the CRs | `script` (2m) + `apply` + `assert` (5m) | `../../e2e/cinder/broker-vhost.sh create cinder-nfs-chaos cinder-nfs-chaos-messaging openstack`, then `00-cinder-cr.yaml` (`cinder-nfs-chaos`) and `01-cinderbackend-cr.yaml` (`nfs-chaos-nfs1`). Every sub-condition and `Ready=True/AllReady` are asserted, with `status.volumeServices` carrying the host identity step 4 looks the heartbeat row up by |
| 2 | Baseline | `script` (8m) + `script` (3m) | A probe pod creates a volume through to `available` within 120 s, and its file is stat'ed on the share. Then the lock holder `cinder-nfs-chaos-probe-lock` of `02-lock-holder-pod.yaml` starts and has to write its third `ok` line within 120 s (`LOCK-HELD-OK`). The step cleanup, which runs at the end of the test, deletes the holder and removes its file `/exports/volumes/chaos-lock-holder.img` from the export |
| 3 | Take the export away | `script` (4m) | Stashes the `cinder-volume` pod UID and restart count as an annotation on its own Deployment, scales `deploy/nfs-server` to 0, and waits until the Service has no endpoint address and no pod left (`OUTAGE-OK`). The step cleanup scales the server back to 1 |
| 4 | Under the outage | `script` (9m) + `assert` + `script` (2m) | A probe gets 200 from `/healthcheck`, then a create is accepted and has to reach `error` within 240 s (`FAILCLOSED-OK`); the registry row is printed as `HEARTBEAT-STATE`, which the step reports without gating on it. The CR still reports `VolumeServicesReady=True/AllVolumeServicesReady` and `Ready=True/AllReady`, and the recorded pod identity is compared against the live one |
| 5 | Bring the export back and let the deployment recover | `script` (5m) + `script` (5m) + `script` (9m) + `script` (6m) | The server is scaled back to 1 and rolled out; the lock holder's log has to grow by three `ok` lines within 180 s, with no `error` line and the pod `Running`, and the holder is deleted (`LOCK-KEPT-OK`); a fresh create reaches `available` within `SETTLE_SECONDS = 240` with its file on the share (`RECOVERED-OK`); a third probe deletes the three volumes the suite is responsible for and waits for each to answer 404 (`CLEANUP-OK`) |
| 6 | Tear the deployment down | `script` (8m) | Deletes the Cinder CR and waits out its pods, then deletes the `CinderBackend` |

**Fixtures:** `00-cinder-cr.yaml`, `01-cinderbackend-cr.yaml`,
`02-lock-holder-pod.yaml`. The suite writes no chaos-mesh CR of its own.

**Catch blocks:** a shared anchor calls
`../diagnostics.sh chaos cinder-nfs-chaos` with `--cr-kind=cinder`,
`--dep-label=app.kubernetes.io/name=nfs-server`, `--dep-ns=openstack` and
`--log-label=app.kubernetes.io/instance=cinder-nfs-chaos`, dumps the
`CinderBackend`, the NFS server's Deployment, pods and EndpointSlices, the Cinder
pods with their logs, the `cinder-nfs-chaos-probe-*` logs, and the cinder-operator
logs from `cinder-system`.

**Design notes:**

- The outage is a scale-down of `deploy/nfs-server` instead of a `NetworkChaos`
  partition. The share is mounted by the `csi-nfs-node` DaemonSet, which runs
  with `hostNetwork: true`, so the kernel NFS client sends from the node's network
  namespace and an iptables rule keyed on the `cinder-volume` pod IP never matches
  a single mount packet. Removing the endpoint the client dials works instead,
  which is what `deletion-stuck-finalizer` does to the mariadb-operator in the `pod` matrix entry.
- Recovery takes longer than the fault. The restarted server comes up in its
  NFSv4 grace period, up to 90 s in which it serves reclaims alone, and the client
  retries through it without telling the application. The post-recovery create
  therefore gets 240 s where the baseline gets 120 s.
- The lock holder checks that the restarted server lets its clients reclaim. It
  is a bare Pod that takes a POSIX lock on a file of the share, as QEMU does on
  an attached volume, and writes and fsyncs through the locked descriptor every
  2 seconds, one log line per write. The mount is `hard`, as Nova's is, so a
  write blocks while the server is away. A server that restarts without its
  NFSv4 client records refuses the reclaim, the client marks the lock lost, and
  every later write fails with EIO: the check fails with
  `FAIL: the lock holder lost its lock:` and the first `error errno=5` line.
  Neither a create nor a CR shows that failure. The check reads the cluster
  through `kubectl` alone, and
  `tests/unit/ci/cinder_nfs_outage_lock_check_test.sh` runs it against a stub.
- The pod identity is recorded as a UID and a restart count together. A restart
  count read on its own falls back to 0 on a replaced pod, which is the value a
  pod that never restarted reports.
- The suite's preamble records a product gap, and step 4 asserts it: the CR stays
  `Ready=True/AllReady` through the outage while every create fails. The mount is
  not something the operator reads, the volume service's readiness is the broker
  socket, and no mount probe and no `BackendsHealthy` condition exist, so a reader
  of the CR alone cannot see this outage. The assertion pins today's behavior
  and makes no claim about the wanted one, so a mount probe or a `BackendsHealthy` condition
  added later changes this suite with it. Until then the gap is on the
  operational side too, as a troubleshooting row in
  [Attach an NFS Backend to Cinder](../../guides/cinder/attach-an-nfs-backend.md#troubleshooting).

---

### nova-broker-outage

**File:** `tests/e2e-chaos/nova-broker-outage/chainsaw-test.yaml`

**Scenario:** none

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
| 6 | Apply the Nova CR and assert its conditions | `apply` + `assert` (8m) | `nova-broker-chaos` with the sixteen conditions the suite asserts True beside `Ready` (fifteen `*Ready` sub-conditions and `ExtraConfigHealthy`; `VPAReady` is not among them) and `Ready=True/AllReady`, the baseline the partition phase is read against |
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

- The readiness flip gets 180 seconds instead of one probe period. A partition
  drops packets without a FIN or an RST, so the socket `nova-amqp-ready` reads
  stays ESTABLISHED until the client closes it; the probe documents that blind
  spot at `images/nova/nova-amqp-ready:29-33`. What closes the socket is
  oslo.messaging's heartbeat, which gives up after `heartbeat_timeout_threshold`
  (60 s by default, with no operator override), and the probe then needs one
  more failure at its 5-second period.
- The drop rule is installed on the broker pods and targets the Nova pods. Nova
  dials a ClusterIP, and kube-proxy only DNATs it to a pod IP in the node's root
  namespace, after the packet has left the client pod, so a client-side rule
  would never match a Service-routed packet.
- The blocked create is capped at `timeout 45`. The baseline Job prints the same
  call taking well under 20 seconds on a healthy bus, so a create that answers
  inside 45 has answered and did not stall. The worker outlasts the stall
  because `spec.api.uwsgi.harakiri` is nil by default, which leaves the flag off
  the uWSGI command entirely, so nothing kills the blocked request and the cast
  goes out once the broker is back.
- The suite brings its own broker instead of taking a vhost on the kind-only
  `shared-rabbitmq`: the fault severs a whole broker, and doing that to the
  shared one would take the rest of the matrix entry with it. On a broker of its own the
  Nova uses managed messaging, which is the mode the fault is about.

---

### nova-mariadb-outage

**File:** `tests/e2e-chaos/nova-mariadb-outage/chainsaw-test.yaml`

**Scenario:** none

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
- The failing read is one attempt bounded at 90 seconds, without a retry loop. A
  retry would blur which attempt the verdict came from, and the bound keeps the
  probe inside the 600 s the NetworkChaos runs for.
- There is no Placement, no Neutron, no Glance and no compute here. A list of
  zero servers is the smallest request that reaches both nova schemas, and a
  deployment with nothing else in it leaves one possible cause for a list that
  fails.

---

### nova-placement-outage

**File:** `tests/e2e-chaos/nova-placement-outage/chainsaw-test.yaml`

**Scenario:** none

**Purpose:** Placement disappears under the scheduler. The claim has two halves:
a Placement outage fails scheduling, and it fails nothing else. `GET /` answers,
a server list answers, and a create is accepted with the 202 it answers on a
healthy cluster, because none of those paths calls Placement. Two accepted
builds then reach ERROR with a fault reporting an unplaced build, while the CR
stays `Ready=True/AllReady`: the scheduler takes its readiness from the broker
socket and never from its Placement client. `s-fail-warm` meets the Placement connection the baseline
left established and `s-fail-cold` a fresh one, the two states a real partition
meets. The operator's 60-second `[placement] timeout` bounds each attempt in
both states, and the scheduler's own log proves which connection each build
met.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost | `script` (2m) | `../../e2e/cinder/broker-vhost.sh create nova-placement-chaos nova-placement-chaos-messaging openstack`. The step cleanup deletes the vhost at the end of the test |
| 2 | Bring up Keystone | `apply` + `assert` (8m) | `keystone-nova-placement-chaos` Ready |
| 3 | Register the four services in the catalog | `script` (6m) + `assert` | `02-catalog-setup-job.yaml`, `succeeded: 1` |
| 4 | Bring up the four services the boot path depends on | `apply` + `assert` (8m) | The Neutron transport-URL Secret, `nova-placement-chaos-ovn`, `neutron-nova-placement-chaos`, `placement-nova-placement-chaos`, `glance-nova-placement-chaos` and `glance-nova-placement-chaos-s3` |
| 5 | Seed the image the servers boot from | `script` (6m) + `assert` | `09-image-seed-job.yaml`, `succeeded: 1` |
| 6 | Apply the Nova CR and assert its conditions | `apply` + `assert` (8m) | `nova-placement-chaos` with the sixteen conditions the suite asserts True beside `Ready` (fifteen `*Ready` sub-conditions and `ExtraConfigHealthy`; `VPAReady` is not among them) and `Ready=True/AllReady`. The CR runs one scheduler worker and logs urllib3's connection pool at DEBUG |
| 7 | Start the fake compute and map it into cell1 | `apply` + `assert` + `script` (3m) | `12-fake-compute.yaml` available, then `../../e2e/nova/discover-hosts.sh nova-placement-chaos` |
| 8 | Baseline | `script` (11m) | `14-baseline-job.yaml` boots a server to ACTIVE, which means the scheduler reached Placement for candidates and claimed the host it picked (`BASELINE-OK`). The scheduler log then holds a `Starting new HTTP connection` for the Placement host: the connection the warm build meets, and the proof that urllib3's DEBUG records reach the log |
| 9 | Inject NetworkChaos to partition Placement | `apply` + `script` (60s) | `13-networkchaos.yaml` (`partition-placement-nova`), waited for `AllInjected`. The step cleanup deletes it |
| 10 | Under the partition | `script` (8m) + `assert` | `15-failclosed-job.yaml` proves the serving half and the failing half in one pod: `GET /`, a server list, then two builds, `s-fail-warm` and `s-fail-cold`, each accepted with 202 and ending in ERROR inside 150 s with a fault that reports an unplaced build, read by server id (`FAILCLOSED-OK`). The scheduler log since the Job started then holds three `Starting new HTTP connection` lines for the Placement host (the warm build's retry and both attempts of the cold build) and no `Resetting dropped connection`. The CR still reads `SchedulerReady`, `NovaAPIReady` and `Ready` True |
| 11 | Lift the partition and schedule again | `delete` + `script` (13m) | `16-recovered-job.yaml` creates a fresh server that reaches ACTIVE with no pod restart and no operator action, then deletes the two ERROR servers (`RECOVERED-OK`). Then `workload.sh start` starts the workload streams. |
| 12 | Judge the workload, audit Placement, compare OVN | `script` (25m) | `workload.sh verify` judges the streams without fault marks, then runs the placement audit against `nova-placement-chaos` and one log-mode run of the `neutron-nova-placement-chaos-ovn-db-sync` CronJob |
| 13 | Tear the deployment down | `script` (10m) | Deletes the compute and the three Jobs best-effort, then the Nova CR, and waits its pods out |

**Fixtures:** `01-keystone-cr.yaml`, `02-catalog-setup-job.yaml`,
`03-messaging-secret.yaml`, `04-ovncentral-cr.yaml`, `05-neutron-cr.yaml`,
`06-placement-cr.yaml`, `07-glance-cr.yaml`, `08-glancebackend-cr.yaml`,
`09-image-seed-job.yaml`, `10-metadata-secret.yaml`, `11-nova-cr.yaml`,
`12-fake-compute.yaml`, `13-networkchaos.yaml`, `14-baseline-job.yaml`,
`15-failclosed-job.yaml`, `16-recovered-job.yaml`

**Workload:** the `token`, `network`, `server` and `image` streams against the Keystone, Neutron, Nova and Glance of the suite, started in step 11 after the partition is lifted and judged in step 12 against `failures=0 recovery=0s` as a run without fault marks. The single scheduler worker leaves no room for other builds during the partition. `05-neutron-cr.yaml` sets `spec.ovnDBSync` to `{syncMode: log, suspend: true}` for the database comparison, and the shared catch anchor dumps the workload.

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

- A partition meets the scheduler in one of two connection states. Its urllib3
  pool may hold an established keep-alive connection to Placement: a request
  written on it waits for an answer while the kernel retransmits, about 15
  minutes on Linux defaults. A fresh connection sends a SYN the partition drops.
  The operator renders `[placement] timeout = 60`, which keystoneauth applies as
  both the connect and the read timeout, so each attempt fails after 60 seconds
  in both states. openstacksdk retries a timed-out request once after 0.5
  seconds (`connect_retries=1` in `Proxy.request`, which `[placement]` cannot
  override), so a build fails after about 121 seconds. That is why the
  fail-closed Job gives each build 150 seconds and why the NetworkChaos runs
  for 600.
- The first build meets the warm connection because Placement's uWSGI router
  closes an idle keep-alive connection only after 60 seconds (`--http-timeout`),
  and the partition drops that close once it is in place. The scheduler's own
  TCP keepalive (keystoneauth sets 60 seconds idle, then four probes 15 seconds
  apart) aborts the cut-off connection about 120 seconds after the baseline's
  last Placement request. The connection is therefore warm while the partition
  lands inside 60 seconds of that request and the first build reaches the
  scheduler inside 120. urllib3 discards a connection that timed out, so the
  first build's retry and both attempts of the second build open fresh ones.
  The scheduler runs one worker: with two, the RPC server hands a build to
  whichever worker is free, and the suite could not say which state a build
  met.
- The scheduler log is the evidence. The warm build's first attempt logs no
  connection line, and every later attempt logs one `Starting new HTTP
  connection`: three in all. When either timer closed the connection first, the
  first build logs `Resetting dropped connection` and reconnects on the old
  connection object, so the count stays three and the check fails on the
  reset. The step then prints the injection time and every Placement line the
  scheduler logged, whose timestamps say which timer fired. A gap under 60
  seconds in the fail-closed Job's log rules both out.
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
  and keeps the request that timed out in the fault details. Connect and read
  timeouts both become `ConnectTimeout`, so the two builds fail the same way.
- The fault is read by server id. `openstack server show <name>` resolves a name
  through the server list, and nova's list path reads faults from the schema the
  API's `[database]` connection names (the cell), while a buried build and its
  fault live in cell0. Only `GET /servers/<id>` targets the mapped cell.

---

### horizon-memcached-pod-kill

**File:** `tests/e2e-chaos/horizon-memcached-pod-kill/chainsaw-test.yaml`

**Purpose:** A `PodChaos` kills one Memcached pod under a Horizon. Horizon keeps its
sessions in signed cookies, so Memcached only backs the Django cache. Losing it lowers
the cache hit rate. It logs no user out and changes no condition. The suite therefore
asserts recovery without a restart: `Ready` stays `True` and no dashboard container
restarts.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Horizon CR | `apply` | Applies `00-horizon-cr.yaml`: Horizon CR `horizon-chaos-mc` with one replica and cache `clusterRef` `openstack-memcached` |
| 2 | Assert baseline Ready=True | `assert` (5m) | `Ready=True` with reason `AllReady`. `workload.sh start` then starts the workload and waits for `READY`. |
| 3 | Inject PodChaos | `apply` | Applies `01-podchaos.yaml`: PodChaos `kill-memcached-horizon` (`action: pod-kill`, `mode: one`, `gracePeriod: 0`) targeting `app.kubernetes.io/name: memcached` in `openstack`. `workload.sh mark fault-start` runs right before it. |
| 4 | Verify the kill took effect and Memcached recovered | `script` (150s) + `assert` (5m) | Reads `.spec.replicas` of Deployment `openstack-memcached`, polls `readyReplicas` up to 15 times at 2 s until it drops below that count, then up to 45 times at 2 s until it equals that count again. Then asserts `Ready=True` with reason `AllReady` |
| 5 | Delete PodChaos | `delete` | Removes PodChaos `kill-memcached-horizon`. `workload.sh mark fault-end` runs right after it. |
| 6 | Assert Ready held and zero dashboard pod restarts | `assert` (5m) + `script` | `Ready=True` with reason `AllReady`. The script reads the `restartCount` of every container in the pods labelled `app.kubernetes.io/name=horizon,app.kubernetes.io/instance=horizon-chaos-mc` and fails when one of them is not 0 |
| 7 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-horizon-cr.yaml`, `01-podchaos.yaml`

**Workload:** the `http` stream against `http://horizon-chaos-mc.openstack.svc:8080/auth/login/`, started in step 2 and judged in step 7 against `failures=148 recovery=130s`. Every step from 2 to 7 also dumps the workload in its catch block.

**Catch blocks:** Step 2 calls `../diagnostics.sh baseline horizon-chaos-mc --cr-kind=horizon`.
Steps 4 and 6 call `../diagnostics.sh chaos horizon-chaos-mc --cr-kind=horizon` with
`--dep-label=app.kubernetes.io/name=memcached`. Steps 1, 3 and 5 have no catch block.

**Design notes:**

- The suite asserts no degraded condition. A degraded condition would need a
  cache-health probe, and the operator has none. Recovery without a restart is the
  contract the suite can observe.
- Step 4 polls the Deployment's `readyReplicas`. A `kubectl wait` on single Pods
  errors with `NotFound` when Chaos Mesh deletes the Pod (issue #214), and
  Deployment-level polling is not affected by a Pod replacement.
- Step 4 first requires `readyReplicas` to drop below the desired count. Without that
  check the recovery loop would pass even if the PodChaos selector matched no pod.
- The 150s script timeout covers a worst case of 120 s (15 polls and 45 polls at 2 s)
  with a 30 s margin.
- The step 4 script omits `set -euo pipefail`. Its polling loop uses the default
  `${READY:-0}`, which would fail under `set -e` when kubectl returns empty output.
- Step 6 checks the restart count of every dashboard pod.

---

### horizon-operator-pod-kill

**File:** `tests/e2e-chaos/horizon-operator-pod-kill/chainsaw-test.yaml`

**Purpose:** A `PodChaos` kills one horizon-operator pod. The operator runs two
replicas. The suite proves the kill by pod identity: it snapshots every operator pod
UID before the fault and waits until at least one of them is gone. It then proves that
the recovered operator reconciles. A replica count patched after the recovery has to
reach the Deployment of the CR.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Horizon CR and wait for baseline Ready | `apply` + `assert` (5m) | Applies `00-horizon-cr.yaml`: Horizon CR `horizon-chaos-op` with one replica, which reaches `Ready=True/AllReady`. `workload.sh start` then starts the workload and waits for `READY`. |
| 2 | Inject chaos and verify one operator pod was replaced | `script` (270s) | One script, so the snapshot and the polls share state: reads `.spec.replicas` of Deployment `horizon-operator` in `horizon-system`, snapshots the operator pod UIDs, applies `01-podchaos.yaml` (PodChaos `kill-horizon-operator`, `action: pod-kill`, `mode: one`, `gracePeriod: 0`, targeting `app.kubernetes.io/name: horizon-operator` in `horizon-system`), waits up to 120 s for at least one pre-chaos UID to disappear, then waits up to 120 s for `readyReplicas` to equal the desired count. The script runs `workload.sh mark fault-start` right before the apply. |
| 3 | Delete PodChaos | `delete` | Removes PodChaos `kill-horizon-operator` from `horizon-system`. `workload.sh mark fault-end` runs right after it. |
| 4 | Prove the recovered operator reconciles a spec change | `patch` + `assert` (5m) | Applies `02-patch-scale.yaml`: sets `spec.deployment.replicas` to 2. Deployment `horizon-chaos-op` reaches `replicas: 2`, `availableReplicas: 2` and `updatedReplicas: 2`, and the CR is `Ready=True/AllReady` |
| 5 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-horizon-cr.yaml`, `01-podchaos.yaml`, `02-patch-scale.yaml`

**Workload:** the `http` stream against `http://horizon-chaos-op.openstack.svc:8080/auth/login/`, started in step 1 and judged in step 5 against `failures=0 recovery=0s`. Every step from 1 to 5 also dumps the workload in its catch block.

**Catch blocks:** Step 1 calls `../diagnostics.sh baseline horizon-chaos-op --cr-kind=horizon`.
Steps 2 and 4 call `../diagnostics.sh chaos horizon-chaos-op --cr-kind=horizon` with
`--dep-label=app.kubernetes.io/name=horizon-operator` and `--dep-ns=horizon-system`.
Step 3 has no catch block.

**Design notes:**

- The snapshot covers every pre-chaos operator pod UID. Recording only `.items[0]` is
  racy: `PodChaos mode: one` picks its victim at random among the two replicas, so the
  recorded pod stays alive in about half the runs and the poll for its disappearance
  fails spuriously.
- "At least one pre-chaos UID is gone" proves the kill whichever pod Chaos Mesh picked.
- Step 2 reads the desired replica count from the Deployment, so the test stays valid
  if the operator replica count changes.
- The first poll skips an iteration in which kubectl returns no UIDs. An empty list
  would otherwise count as "no pre-chaos UID present".
- The 270s script timeout covers a worst case of 240 s (two phases of 60 polls at 2 s)
  with a 30 s margin.
- The script omits `set -euo pipefail`. The polling loops use `${VAR:-0}` defaults
  that would fail under `set -e` when kubectl returns empty output, and the script
  checks every loop exit condition itself.

---

### glance-operator-pod-kill

**File:** `tests/e2e-chaos/glance-operator-pod-kill/chainsaw-test.yaml`

**Purpose:** A `PodChaos` kills one glance-operator pod. The operator is a
control-plane component, so the kill must not disturb the running Glance pods. The
suite states that as pod identity: it snapshots the UIDs of the pods that carry the
instance label of the CR before the kill, and it fails if one of those UIDs is missing
after the operator has recovered. A replica count patched afterwards has to reach the
Deployment of the CR, which shows that the recovered operator reconciles spec changes.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Glance CR and default store, wait for baseline Ready | `apply` + `assert` (5m) | Applies `00-glance-cr.yaml`: Glance CR `glance-chaos-op` with database `glance_chaos_op` and one replica. Applies `01-glance-backend.yaml`: GlanceBackend `glance-chaos-op-s3` (`type: S3`, `isDefault: true`, bucket `glance-images`). The CR reaches `Ready=True/AllReady`. `workload.sh start` then starts the workload and waits for `READY`. |
| 2 | Inject chaos and verify one operator pod was replaced | `script` (270s) | One script, so the snapshots and the polls share state: reads `.spec.replicas` of Deployment `glance-operator` in `glance-system`, snapshots the operator pod UIDs and the UIDs of the pods labelled `app.kubernetes.io/instance=glance-chaos-op` in `openstack`, applies `02-podchaos.yaml` (PodChaos `kill-glance-operator`, `action: pod-kill`, `mode: one`, `gracePeriod: 0`, targeting `app.kubernetes.io/name: glance-operator` in `glance-system`), waits up to 120 s for at least one pre-chaos operator UID to disappear, waits up to 120 s for `readyReplicas` to equal the desired count, then fails if a pre-chaos workload UID is missing. The script runs `workload.sh mark fault-start` right before the apply. |
| 3 | Delete PodChaos | `delete` | Removes PodChaos `kill-glance-operator` from `glance-system`. `workload.sh mark fault-end` runs right after it. |
| 4 | Prove the recovered operator reconciles a spec change | `patch` + `assert` (5m) | Applies `03-patch-scale.yaml`: sets `spec.deployment.replicas` to 2. Deployment `glance-chaos-op` reaches `replicas: 2`, `availableReplicas: 2` and `updatedReplicas: 2`, and the CR is `Ready=True/AllReady` |
| 5 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-glance-cr.yaml`, `01-glance-backend.yaml`, `02-podchaos.yaml`,
`03-patch-scale.yaml`

**Workload:** the `http` stream against `http://glance-chaos-op.openstack.svc:9292/healthcheck`, started in step 1 and judged in step 5 against `failures=0 recovery=0s`. Every step from 1 to 5 also dumps the workload in its catch block.

**Catch blocks:** Step 1 calls `../diagnostics.sh baseline glance-chaos-op --cr-kind=glance`.
Steps 2 and 4 call `../diagnostics.sh chaos glance-chaos-op --cr-kind=glance` with
`--dep-label=app.kubernetes.io/name=glance-operator` and `--dep-ns=glance-system`.
Step 3 has no catch block.

**Design notes:**

- glance-api registers no store without a default backend and cannot go Ready, so
  step 1 applies `01-glance-backend.yaml` together with the CR. The backend points at
  the in-cluster Garage S3 endpoint and takes its credentials from the Secret
  `garage-s3-credentials`.
- The snapshot covers every pre-chaos operator pod UID. Recording only `.items[0]` is
  racy: `PodChaos mode: one` picks its victim at random among the two replicas, so the
  recorded pod stays alive in about half the runs and the poll for its disappearance
  fails spuriously.
- The workload comparison is the operational-independence half of the contract. A
  pre-chaos workload UID that is gone means the operator kill disturbed a Glance pod,
  and the step fails with both UID lists printed.
- The script sets `REPLACED=0` before the first poll. Every iteration can skip on an
  empty UID list, and an unset `REPLACED` would turn the guard after the loop into an
  "integer expression expected" error that the `if` reads as false. The script would
  then continue to the second poll without failing.
- The 270s script timeout covers a worst case of 240 s (two phases of 60 polls at 2 s)
  with a 30 s margin. The workload comparison is two kubectl reads without polling.
- The script omits `set -euo pipefail`. The polling loops use `${VAR:-0}` defaults
  that would fail under `set -e` when kubectl returns empty output, and the script
  checks every loop exit condition itself.

---

### placement-operator-pod-kill

**File:** `tests/e2e-chaos/placement-operator-pod-kill/chainsaw-test.yaml`

**Purpose:** A `PodChaos` kills one placement-operator pod. The operator is a
control-plane component, so the kill must not disturb the running Placement pods. The
suite snapshots the UIDs of the pods that carry the instance label of the CR before
the kill, and it fails if one of those UIDs is missing after the operator has
recovered. A replica count patched afterwards has to reach the Deployment of the CR,
which shows that the recovered operator reconciles spec changes.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Placement CR and wait for baseline Ready | `apply` + `assert` (5m) | Applies `00-placement-cr.yaml`: Placement CR `placement-chaos-op` with database `placement_chaos_op` and one replica, which reaches `Ready=True/AllReady`. `workload.sh start` then starts the workload and waits for `READY`. |
| 2 | Inject chaos and verify one operator pod was replaced | `script` (270s) | One script, so the snapshots and the polls share state: reads `.spec.replicas` of Deployment `placement-operator` in `placement-system`, snapshots the operator pod UIDs and the UIDs of the pods labelled `app.kubernetes.io/instance=placement-chaos-op` in `openstack`, applies `01-podchaos.yaml` (PodChaos `kill-placement-operator`, `action: pod-kill`, `mode: one`, `gracePeriod: 0`, targeting `app.kubernetes.io/name: placement-operator` in `placement-system`), waits up to 120 s for at least one pre-chaos operator UID to disappear, waits up to 120 s for `readyReplicas` to equal the desired count, then fails if a pre-chaos workload UID is missing. The script runs `workload.sh mark fault-start` right before the apply. |
| 3 | Delete PodChaos | `delete` | Removes PodChaos `kill-placement-operator` from `placement-system`. `workload.sh mark fault-end` runs right after it. |
| 4 | Prove the recovered operator reconciles a spec change | `patch` + `assert` (5m) | Applies `02-patch-scale.yaml`: sets `spec.deployment.replicas` to 2. Deployment `placement-chaos-op` reaches `replicas: 2`, `availableReplicas: 2` and `updatedReplicas: 2`, and the CR is `Ready=True/AllReady` |
| 5 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-placement-cr.yaml`, `01-podchaos.yaml`, `02-patch-scale.yaml`

**Workload:** the `http` stream against `http://placement-chaos-op.openstack.svc:8778/`, started in step 1 and judged in step 5 against `failures=0 recovery=0s`. Every step from 1 to 5 also dumps the workload in its catch block.

**Catch blocks:** Step 1 calls `../diagnostics.sh baseline placement-chaos-op` and steps 2
and 4 call `../diagnostics.sh chaos placement-chaos-op`, all three with
`--cr-kind=placement`, `--dep-label=app.kubernetes.io/name=placement-operator` and
`--dep-ns=placement-system`. Step 3 has no catch block.

**Design notes:**

- Placement has no store backend, so the CR is the only object the suite needs
  before the baseline.
- The snapshot covers every pre-chaos operator pod UID. Recording only `.items[0]` is
  racy: `PodChaos mode: one` picks its victim at random among the two replicas, so the
  recorded pod stays alive in about half the runs and the poll for its disappearance
  fails spuriously.
- The workload comparison is the operational-independence half of the contract. A
  pre-chaos workload UID that is gone means the operator kill disturbed a Placement
  pod, and the step fails with both UID lists printed.
- The script sets `REPLACED=0` before the first poll, for the reason
  `glance-operator-pod-kill` gives.
- The 270s script timeout covers a worst case of 240 s (two phases of 60 polls at 2 s)
  with a 30 s margin. The workload comparison is two kubectl reads without polling.
- The script omits `set -euo pipefail`. The polling loops use `${VAR:-0}` defaults
  that would fail under `set -e` when kubectl returns empty output, and the script
  checks every loop exit condition itself.

---

### barbican-operator-pod-kill

**File:** `tests/e2e-chaos/barbican-operator-pod-kill/chainsaw-test.yaml`

**Purpose:** A `PodChaos` kills one barbican-operator pod. The operator is a
control-plane component, so the kill must not disturb the running Barbican pods. The
suite snapshots the UIDs of the pods that carry the instance label of the CR before
the kill, and it fails if one of those UIDs is missing after the operator has
recovered. A replica count patched afterwards has to reach the Deployment of the CR,
which shows that the recovered operator reconciles spec changes.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Barbican CR and store, wait for baseline Ready | `apply` + `assert` (5m) | Applies `00-barbican-cr.yaml`: Barbican CR `barbican-chaos-op` with database `barbican_chaos_opkill` and one replica. Applies `01-barbican-secretstore.yaml`: BarbicanSecretStore `barbican-chaos-op-store` (`type: OpenBao`, `isDefault: true`, `instanceRef` `openbao-instance`). The CR reaches `SecretStoresReady=True/AllStoresProjected` and `Ready=True/AllReady`. `workload.sh start` then starts the workload and waits for `READY`. |
| 2 | Inject chaos and verify one operator pod was replaced | `script` (270s) | One script, so the snapshots and the polls share state: reads `.spec.replicas` of Deployment `barbican-operator` in `barbican-system`, snapshots the operator pod UIDs and the UIDs of the pods labelled `app.kubernetes.io/instance=barbican-chaos-op` in `openstack`, applies `02-podchaos.yaml` (PodChaos `kill-barbican-operator`, `action: pod-kill`, `mode: one`, `gracePeriod: 0`, targeting `app.kubernetes.io/name: barbican-operator` in `barbican-system`), waits up to 120 s for at least one pre-chaos operator UID to disappear, waits up to 120 s for `readyReplicas` to equal the desired count, then fails if a pre-chaos workload UID is missing. The script runs `workload.sh mark fault-start` right before the apply. |
| 3 | Delete PodChaos | `delete` | Removes PodChaos `kill-barbican-operator` from `barbican-system`. `workload.sh mark fault-end` runs right after it. |
| 4 | Prove the recovered operator reconciles a spec change | `patch` + `assert` (5m) | Applies `03-patch-scale.yaml`: sets `spec.deployment.replicas` to 2. Deployment `barbican-chaos-op` reaches `replicas: 2`, `availableReplicas: 2` and `updatedReplicas: 2`, and the CR is `Ready=True/AllReady` |
| 5 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-barbican-cr.yaml`, `01-barbican-secretstore.yaml`, `02-podchaos.yaml`,
`03-patch-scale.yaml`

**Workload:** the `http` stream against `http://barbican-chaos-op.openstack.svc:9311/healthcheck`, started in step 1 and judged in step 5 against `failures=0 recovery=0s`. Every step from 1 to 5 also dumps the workload in its catch block.

**Catch blocks:** Step 1 calls `../diagnostics.sh baseline barbican-chaos-op` and steps 2
and 4 call `../diagnostics.sh chaos barbican-chaos-op`, all three with
`--cr-kind=barbican`, `--dep-label=app.kubernetes.io/name=barbican-operator` and
`--dep-ns=barbican-system`. Step 1 also dumps the BarbicanSecretStore
`barbican-chaos-op-store` as YAML. Step 3 has no catch block.

**Design notes:**

- Barbican resolves its secret store when the process starts, so step 1 applies a
  default store together with the CR. A Barbican without a credential-ready default
  store reports `SecretStoresReady=False` with reason `NoDefaultSecretStore` and
  renders no config.
- The store runs in managed mode. `instanceRef` names the OpenBaoCluster
  `openbao-instance` in the same namespace, and the server URL, the CA Secret
  `openbao-instance-tls-ca` and the provisioner ServiceAccount follow from that name.
- The single-default rule is scoped to `spec.barbicanRef`, so this store does not
  collide with the default stores that other suites attach to their own Barbicans.
- The suite runs without a Keystone. The CRD requires `keystoneEndpoint` and the
  operator renders it into `[keystone_authtoken]`, but nothing in the suite
  authenticates: the readiness probe requests `/healthcheck`, which is routed outside
  the authtoken pipeline. `barbican-openbao-outage` is the suite that authenticates.
- The workload snapshot selects by the instance label alone, so it also holds the
  completed db-sync and schema-check Job pods. Neither Job carries a TTL, so both pods
  stay until the end of the run and only a real deletion changes their UIDs.
- The snapshot covers every pre-chaos operator pod UID, and the script sets
  `REPLACED=0` before the first poll, both for the reasons `glance-operator-pod-kill`
  gives.
- The 270s script timeout covers a worst case of 240 s (two phases of 60 polls at 2 s)
  with a 30 s margin. The workload comparison is two kubectl reads without polling.

---

### glance-garage-outage

**File:** `tests/e2e-chaos/glance-garage-outage/chainsaw-test.yaml`

**Purpose:** A `NetworkChaos` partition severs glance-api from Garage, the S3 store
behind the default image backend. The suite proves that the image write path fails
closed while the API and the CR stay up. An upload during the partition must not
answer 2xx, and the image must still be non-active once Garage is reachable again.
In the same window `GET /healthcheck` answers 200 and the CR stays `Ready=True`:
Glance readiness requests `/healthcheck`, which the oslo middleware serves without
touching S3, so the storage outage is invisible to readiness. That is the contrast
with `mariadb-network-partition`, where the readiness probe is database-aware and a
MariaDB partition turns `Ready` to `False`.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply the Glance CR and default S3 store | `apply` + `assert` (5m) | Applies `00-glance-cr.yaml`: Glance CR `glance-garage-chaos` with database `glance_garage_chaos`, one replica and `spec.apiServer.uwsgi.harakiri: 20`. Applies `01-glancebackend-cr.yaml`: GlanceBackend `glance-garage-chaos-s3` (`type: S3`, `isDefault: true`, host `http://garage.shared-services.svc.cluster.local:3900`, bucket `glance-images`). The CR reaches `BackendsReady=True/AllBackendsProjected` and `Ready=True/AllReady` |
| 2 | Baseline write | `script` (5m) | Pod `glance-garage-baseline-probe` on image `ghcr.io/c5c3/glance:2026.1` runs a python client against `http://glance-garage-chaos.openstack.svc:9292`: `POST /v2/images` creates `garage-baseline-probe`, `PUT /v2/images/{id}/file` uploads a payload to the default store, and `GET /v2/images/{id}` must report status `active`. The step reads the sentinel `WRITE-OK` from the pod log |
| 3 | Inject NetworkChaos | `apply` | Applies `02-networkchaos.yaml`: NetworkChaos `partition-garage` (`action: partition`, `mode: all`, `direction: both`, `duration: "600s"`) |
| 4 | Assert injection active | `script` (60s) | `kubectl wait networkchaos/partition-garage -n openstack --for=condition=AllInjected --timeout=60s` |
| 5 | Assert fail-closed | `script` (5m) + `assert` (5m) | Pod `glance-garage-failclosed-probe`: `GET /healthcheck` answers 200, `POST /v2/images` still creates the record `garage-outage-failclosed`, and one `PUT /v2/images/{id}/file` bounded at 90 seconds must not answer 2xx. A non-2xx status, a timeout and a connection error all pass. Sentinel `FAILCLOSED-OK`. Then asserts `Ready=True/AllReady` on the CR |
| 6 | Lift the partition | `delete` | Removes NetworkChaos `partition-garage` |
| 7 | Assert the failed write stayed failed | `script` (3m) | Pod `glance-garage-failclosed-verify`: `GET /v2/images?name=garage-outage-failclosed` must list the image, and no listed image may have status `active`. Sentinel `STAYED-FAILED-OK` |
| 8 | Assert recovery | `script` (5m) | Pod `glance-garage-recovery-probe`: creates `garage-recovery-probe`, uploads a payload, and the image must report status `active`. Sentinel `WRITE-OK` |

**Fixtures:** `00-glance-cr.yaml`, `01-glancebackend-cr.yaml`, `02-networkchaos.yaml`

**Catch blocks:** Steps 1 and 2 call `../diagnostics.sh baseline glance-garage-chaos --cr-kind=glance`.
Steps 4 to 8 call `../diagnostics.sh chaos glance-garage-chaos --cr-kind=glance` with
`--dep-label=app.kubernetes.io/name=glance-operator`, `--dep-ns=glance-system` and
`--log-label=app.kubernetes.io/instance=glance-garage-chaos`. Steps 4, 5 and 6 also dump
the NetworkChaos as YAML. Steps 5, 6 and 8 also list the Garage storage pods in
`shared-services`. Step 3 has no catch block.

**Design notes:**

- `02-networkchaos.yaml` names the Garage storage pods as `selector`
  (`garage.rajsingh.info/cluster: garage` and `garage.rajsingh.info/tier: storage` in
  `shared-services`) and the Glance pods as `target` (`app.kubernetes.io/name: glance`
  and `app.kubernetes.io/instance: glance-garage-chaos` in `openstack`), with
  `direction: both` and `duration: "600s"`.
- The rule is enforced on the Garage (server) side because glance-api connects to the
  ClusterIP Service `garage.shared-services.svc.cluster.local:3900`, never to a Garage
  pod IP. Chaos Mesh installs its drop rule in the network namespace of the `selector`
  pods and matches the `target` pods by pod IP. A rule on the glance-api side would
  match Garage pod IPs while the packet still carries the Service ClusterIP, because
  kube-proxy translates the ClusterIP to a pod IP later, in the root namespace of the
  node. That rule would never match. On the Garage side the packet is already
  translated and carries the glance-api pod IP as its source.
- The `selector` label pair is the selector the cluster-level S3 API Service uses for
  its backends, so the partition severs the traffic that the Service routes. The
  namespace of the NetworkChaos object does not limit the selection: Chaos Mesh
  selects cluster-wide.
- `duration` is a safety net in case the test does not delete the object. Step 6
  deletes it. The fixture sets 600s because the value has to exceed the 5m assert
  window: with an equal value Chaos Mesh would restore the connection while step 5 is
  still probing, and a failing `PUT` could turn into a success.
- Step 4 uses `kubectl wait` because `status.conditions` of a NetworkChaos is a fixed
  list of four items (`Selected`, `AllInjected`, `AllRecovered`, `Paused`), and the
  list merge of a Chainsaw resource assert rejects a shorter expected list.
- Creating the image record in step 5 is a database-only write, which the partition
  does not affect. It gives the probe an image id to upload to.
- Step 7 exists because a timeout in step 5 only proves that the client gave up. The
  store client of Glance could still have completed the upload through its own
  retries. A fail-closed upload leaves the record `queued`, `saving` or `killed`, and
  `active` would mean the data arrived after the probe stopped waiting.
- The Glance CR sets `spec.apiServer.uwsgi.harakiri: 20` so that step 7 can hold.
  Under uWSGI (2026.1 and later) the HTTP router closes the client connection after
  its own socket timeout (`--http-timeout`, 60 seconds by default, which the operator
  does not set), while the worker keeps retrying the S3 write. Step 5 ends on that
  disconnect, step 6 lifts the partition with the write still in flight, the next
  boto attempt connects, and the image turns `active`. The 20-second cap kills the
  worker before the router gives up on the client, so no write reaches the
  recovered state. The value has to stay below the webhook's bound of
  `terminationGracePeriodSeconds - preStopSleepSeconds` (25 at the deployment
  defaults).
- Every probe takes its verdict from the pod log. `kubectl run -i` carries only what
  the container writes after the attach is established, so a fast probe could reach
  its sentinel before the capture starts. The probe pod therefore runs without `--rm`,
  and a trap deletes it.
- Each probe step polls for the pod phase `Succeeded` or `Failed`. A failed probe ends
  in `Failed`, and waiting only for `Succeeded` would use up the step timeout and lose
  the log. The sentinel is the verdict, the phase is not.
- The suite runs without a Keystone, and three settings in `00-glance-cr.yaml`
  together open the API to requests without a token. `allow_anonymous_access=true`
  admits the request. The `unauthenticated-context` middleware replaces the anonymous
  context, which is read-only and would make every write answer 403 before any policy
  check. `policyOverrides` sets the image API rules to `@`, the always-allow value of
  oslo.policy, because the CRD forbids an empty rule value.

---

### barbican-openbao-outage

**File:** `tests/e2e-chaos/barbican-openbao-outage/chainsaw-test.yaml`

**Purpose:** A `NetworkChaos` partition severs barbican-api from OpenBao, the store
behind the default secret store. The suite proves that the secret write path fails
closed while the API stays up. A `POST /v1/secrets` during the partition must answer
5xx or time out, and no secret of that name may return the payload once OpenBao is
reachable again. In the same window `GET /healthcheck` answers 200 and
`BarbicanAPIReady` stays `True`: the healthcheck app is routed outside the authtoken
pipeline and touches neither the database nor the store, so only secret traffic fails.
Barbican has no anonymous mode and the secret path needs a project-scoped token, so
the suite brings its own Keystone and every probe takes a token as the bootstrap admin
first.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` + `assert` (5m) | Applies `00-keystone-cr.yaml`: Keystone CR `keystone-barbican-outage` with database `keystone_barbican_outage` and one replica, which reaches `Ready=True/AllReady`. The probes take their tokens from it |
| 2 | Apply the Barbican CR and its default store | `apply` + `assert` (5m) | Applies `01-barbican-cr.yaml`: Barbican CR `barbican-bao-outage` with database `barbican_bao_outage` and two replicas. Applies `02-barbican-secretstore.yaml`: BarbicanSecretStore `barbican-bao-outage-store` (`type: OpenBao`, `isDefault: true`, `instanceRef` `openbao-instance`). The CR reaches `SecretStoresReady=True/AllStoresProjected` and `Ready=True/AllReady`, and the store reaches `Ready=True/AllReady` |
| 3 | Baseline round trip | `script` (5m) | Pod `barbican-bao-baseline-probe` on image `ghcr.io/c5c3/barbican:2026.1` runs a python client: it takes a project-scoped admin token from Keystone, `POST /v1/secrets` stores `bao-outage-baseline` with a payload, and `GET /v1/secrets/{uuid}/payload` must return the same payload. The step reads the sentinel `WRITE-OK` from the pod log |
| 4 | Inject NetworkChaos | `apply` | Applies `03-networkchaos.yaml`: NetworkChaos `partition-openbao-barbican` (`action: partition`, `mode: all`, `direction: both`, `duration: "600s"`) |
| 5 | Assert injection active | `script` (60s) | `kubectl wait networkchaos/partition-openbao-barbican -n openstack --for=condition=AllInjected --timeout=60s` |
| 6 | Assert fail-closed | `script` (5m) + `assert` (5m) | Pod `barbican-bao-failclosed-probe`: `GET /healthcheck` answers 200, a Keystone token is still obtainable, and one `POST /v1/secrets` named `bao-outage-failclosed`, bounded at 90 seconds, must fail as a store outage fails. A 2xx or a 4xx fails the probe. A 5xx, a timeout and a connection error pass. A second `GET /healthcheck` answers 200. Sentinel `FAILCLOSED-OK`. Then asserts `BarbicanAPIReady=True/APIHealthy` on the CR |
| 7 | Lift the partition | `delete` | Removes NetworkChaos `partition-openbao-barbican` |
| 8 | Assert the failed write stayed failed | `script` (3m) | Pod `barbican-bao-failclosed-verify`: lists `GET /v1/secrets?name=bao-outage-failclosed` and requests the payload of every listed secret. An empty list passes, an HTTP error on the payload passes, and a payload equal to `bao-outage-failclosed-payload` fails the step. Sentinel `STAYED-FAILED-OK` |
| 9 | Assert recovery | `script` (5m) | Pod `barbican-bao-recovery-probe`: stores `bao-outage-recovery` and must read its payload back unchanged. Sentinel `WRITE-OK` |

**Fixtures:** `00-keystone-cr.yaml`, `01-barbican-cr.yaml`, `02-barbican-secretstore.yaml`,
`03-networkchaos.yaml`

**Catch blocks:** Step 1 calls `../diagnostics.sh baseline keystone-barbican-outage`. Steps
2 and 3 call `../diagnostics.sh baseline barbican-bao-outage` and steps 5 to 9 call
`../diagnostics.sh chaos barbican-bao-outage`, all with `--cr-kind=barbican`,
`--dep-label=app.kubernetes.io/name=openbao` and `--dep-ns=openstack`. The chaos calls
add `--log-label=app.kubernetes.io/instance=barbican-bao-outage`. Step 2 also dumps the
BarbicanSecretStore as YAML. Steps 5, 6 and 7 also dump the NetworkChaos as YAML. Steps
6 and 9 also list the OpenBao pods in `openstack`. Step 4 has no catch block.

**Design notes:**

- `03-networkchaos.yaml` names the OpenBao pods as `selector`
  (`app.kubernetes.io/name: openbao` and `openbao.org/cluster: openbao-instance` in
  `openstack`) and the Barbican API pods as `target` (`app.kubernetes.io/name: barbican`,
  `app.kubernetes.io/instance: barbican-bao-outage` and
  `app.kubernetes.io/component: api` in `openstack`), with `direction: both` and
  `duration: "600s"`.
- The rule is enforced on the OpenBao (server) side because barbican-api connects to
  no OpenBao pod IP. The managed store derives its server URL from the instance name,
  so the vault plugin connects to the ClusterIP Service `openbao-instance.openstack.svc`.
  Chaos Mesh installs its drop rule in the network namespace of the `selector` pods
  and matches the `target` pods by pod IP. A rule on the barbican-api side would match
  OpenBao pod IPs while the packet still carries the Service ClusterIP, because
  kube-proxy translates the ClusterIP to a pod IP later, in the root namespace of the
  node. That rule would never match. On the OpenBao side the packet is already
  translated and carries the barbican-api pod IP as its source.
- The `selector` uses the name label and the cluster label only. The pod template
  also sets `openbao.org/workload-pool` and `openbao.org/revision`, and both change
  with the workload generation. The two labels are enough: the management OpenBao
  runs in `shared-services`, and `openbao-instance` is the only OpenBaoCluster in
  `openstack`.
- The `target` is narrowed to `app.kubernetes.io/component: api`. barbican-api is the
  only workload of this Barbican that talks to the secret store. The db-sync,
  schema-check and db-clean pods carry the same name and instance labels but reach
  only MariaDB, so a partition on them would widen the fault past what the suite
  asserts.
- `duration` is a safety net in case the test does not delete the object. Step 7
  deletes it. The fixture sets 600s because the value has to exceed the 5m assert
  window: with an equal value Chaos Mesh would restore the connection while step 6 is
  still probing, and a failing write could turn into a success.
- A 4xx during the partition fails the suite. The subject is the store, and a
  regression in the auth pipeline (a wrong `serviceUser`, a misrendered
  `[keystone_authtoken]`) would turn every write into a 401 that could otherwise pass
  for proof.
- The fail-closed `POST` is attempted once. A retry could create a second secret under
  the same name and blur what step 8 reads.
- Step 6 asserts `BarbicanAPIReady` and leaves the aggregate `Ready` condition out.
  The store controller revalidates its AppRole credentials against OpenBao on its own
  schedule, so `CredentialsReady` may change inside the window.
- The Barbican CR runs two replicas. The fail-closed probe leaves one uWSGI worker
  waiting on a request that drops into the partition, and the suite asserts that
  `/healthcheck` keeps answering meanwhile. With one replica (two uWSGI processes) the
  waiting worker would be half the capacity that serves health traffic.
- Step 5 uses `kubectl wait` because `status.conditions` of a NetworkChaos is a fixed
  list of four items (`Selected`, `AllInjected`, `AllRecovered`, `Paused`), and the
  list merge of a Chainsaw resource assert rejects a shorter expected list. Asserting
  the negative conditions would be worse: Chaos Mesh writes all four as `False` before
  the selector has run, so a `Selected=False` assertion passes on an experiment that
  injected nothing.
- Step 8 exists because a timeout in step 6 only proves that the client gave up. The
  vault plugin could still have completed the write through its own retries. An empty
  list means the `POST` created no row. A row without a readable payload is the other
  accepted outcome: Barbican creates the metadata before it hands the payload to the
  plugin, so a store failure can leave the record behind.
- Every probe takes its verdict from the pod log, for the reason
  `glance-garage-outage` gives. The bootstrap admin password reaches the probe pod as
  the environment variable `OS_PASSWORD`, read from the Secret `keystone-admin`.

---

### glance-keystone-outage

**File:** `tests/e2e-chaos/glance-keystone-outage/chainsaw-test.yaml`

**Scenario:** none

**Purpose:** A `PodChaos` `pod-failure` takes the API pods of the suite's own
Keystone away while Glance keeps running. The suite pins how Glance treats a token
it cannot validate: a token issued before the outage, and never shown to Glance
until then, gets 401 on `GET /v2/images`. Glance alone passes `delay_auth_decision =
true` into its paste pipeline, so keystonemiddleware marks the token invalid when
Keystone is unreachable, and Glance's `ContextMiddleware` rejects the unconfirmed
identity. The Keystone CR reports `DeploymentReady=False` in the same window, and
the Glance CR stays `Ready=True/AllReady`, because no Glance condition watches
Keystone. Once the outage is lifted a fresh token gets 200 from the same request.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` + `assert` (5m) | `keystone-glance-ks-outage` (database `keystone_glance_ks_outage`) reaches `Ready=True/AllReady` |
| 2 | Apply Glance and its store | `apply` + `assert` (5m) + `script` (6m) | `glance-ks-outage` (database `glance_ks_outage`, `keystoneEndpoint` at the suite's Keystone, no anonymous access) and `glance-ks-outage-s3` reach `Ready=True/AllReady`. `workload.sh start` then starts the workload and waits for `READY` |
| 3 | Take Keystone away | `script` (8m) + `assert` (5m) | Pod `glance-ks-outage-failclosed-probe` on `ghcr.io/c5c3/tempest:2026.1` issues a token T1 and prints `TOKEN-ISSUED`. The script then runs `workload.sh mark fault-start`, applies `03-podchaos.yaml` and waits for `AllInjected`. The probe waits up to 180 s until Keystone has refused every connection for 6 s and sends `GET /v2/images` with T1 once: 401 prints `FAILCLOSED-OK`, a 2xx fails, and any other answer is retried every 5 s for up to 120 s. The asserts read `DeploymentReady` `False` on the Keystone CR and `Ready=True/AllReady` on the Glance CR. The step cleanup deletes the `PodChaos` |
| 4 | Lift the outage | `delete` + `script` (1m) | Removes `PodChaos/fail-keystone-glance`, then `workload.sh mark fault-end` |
| 5 | Assert recovery | `assert` (5m) + `script` (7m) | The Keystone CR is back at `Ready=True/AllReady`. Pod `glance-ks-outage-recovery-probe` issues a fresh token and repeats the request, retrying a 401 or a 503 every 5 s for up to 180 s, until a 200 prints `RECOVERY-OK` |
| 6 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-keystone-cr.yaml`, `01-glance-cr.yaml`,
`02-glancebackend-cr.yaml`, `03-podchaos.yaml`.

**Workload:** the `token` and `image` streams against `keystone-glance-ks-outage`
and `glance-ks-outage`, started in step 2 and judged in step 6 against `failures=400
recovery=150s`, an estimate no run has measured yet. The suite runs in the `pod`
matrix entry. The token stream fails from the injection until Keystone answers
again, and the `image` stream fails once Glance needs a token it has not cached.
Every step from 2 to 6 dumps the workload in its catch block.

**Catch blocks:** step 1 calls `../diagnostics.sh baseline
keystone-glance-ks-outage`. The later steps share a YAML anchor that calls
`../diagnostics.sh chaos glance-ks-outage` with `--cr-kind=glance` and
`--dep-label=app.kubernetes.io/instance=keystone-glance-ks-outage`, dumps the
Keystone CR, the pod logs of the Glance CR and of the glance-operator, and every
`PodChaos`, and dumps the workload.

**Design notes:**

- `pod-failure` swaps the keystone container for a pause image. The pod stays
  scheduled, its readiness probe on `/v3` fails, the Service loses its endpoint, and
  every connection is refused at once, which gives the probe a bounded answer. A
  `NetworkChaos` partition would leave every validation waiting out its TCP connect
  retries instead: the operators set no `http_connect_timeout` in
  `[keystone_authtoken]`, and keystonemiddleware has no default for it. The same
  refusal needs no kernel module, so the suite runs in the blocking `pod` entry.
- The probe uses a token the service has never seen. keystonemiddleware caches a
  validated token for `token_cache_time` (300 s by default) in the shared Memcached,
  so a token the workload presented before the outage can keep working through it.
- The script reads `TOKEN-ISSUED` from the log of the running probe before it
  applies the `PodChaos`, so the probe runs `python3 -u`: a buffered stdout would
  hold the line until the pod ends.
- The selector is `app.kubernetes.io/name: keystone` and
  `app.kubernetes.io/component: api` with the instance label of the suite's
  Keystone. The trust-flush and key-rotation CronJob pods carry another component,
  the Keystone Job pods carry no name label, and every other Keystone in the
  namespace carries another instance label.
- The probe takes Keystone for gone once every GET has been refused for 6 seconds.
  A timeout, a reset or a single refusal does not count: a Keystone that is slow
  under load, or still serving through its termination grace period, would validate
  the token.
- The Glance CR sets none of the anonymous-access settings `glance-garage-outage`
  needs in an entry without a Keystone, because they would let through the request
  this suite proves is refused.

---

### placement-keystone-outage

**File:** `tests/e2e-chaos/placement-keystone-outage/chainsaw-test.yaml`

**Scenario:** none

**Purpose:** A `PodChaos` `pod-failure` takes the API pods of the suite's own
Keystone away while Placement keeps running. The suite pins how Placement treats a
token it cannot validate: a token issued before the outage, and never shown to
Placement until then, gets 503 on `GET /resource_providers`. keystonemiddleware
cannot reach Keystone to validate it and answers `HTTPServiceUnavailable`. The
Keystone CR reports `DeploymentReady=False` in the same window, and the Placement CR
stays `Ready=True/AllReady`, because no Placement condition watches Keystone. Once
the outage is lifted a fresh token gets 200 from the same request.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` + `assert` (5m) | `keystone-placement-ks-outage` (database `keystone_placement_ks_outage`) reaches `Ready=True/AllReady` |
| 2 | Apply Placement | `apply` + `assert` (5m) + `script` (6m) | `placement-ks-outage` (database `placement_ks_outage`, `keystoneEndpoint` at the suite's Keystone) reaches `Ready=True/AllReady`. `workload.sh start` then starts the workload and waits for `READY` |
| 3 | Take Keystone away | `script` (8m) + `assert` (5m) | Pod `placement-ks-outage-failclosed-probe` on `ghcr.io/c5c3/tempest:2026.1` issues a token T1 and prints `TOKEN-ISSUED`. The script then runs `workload.sh mark fault-start`, applies `02-podchaos.yaml` and waits for `AllInjected`. The probe waits up to 180 s until Keystone has refused every connection for 6 s and sends `GET /resource_providers` with T1 once: 503 prints `FAILCLOSED-OK`, a 2xx fails, and any other answer is retried every 5 s for up to 120 s. The asserts read `DeploymentReady` `False` on the Keystone CR and `Ready=True/AllReady` on the Placement CR. The step cleanup deletes the `PodChaos` |
| 4 | Lift the outage | `delete` + `script` (1m) | Removes `PodChaos/fail-keystone-placement`, then `workload.sh mark fault-end` |
| 5 | Assert recovery | `assert` (5m) + `script` (7m) | The Keystone CR is back at `Ready=True/AllReady`. Pod `placement-ks-outage-recovery-probe` issues a fresh token and repeats the request, retrying a 401 or a 503 every 5 s for up to 180 s, until a 200 prints `RECOVERY-OK` |
| 6 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-keystone-cr.yaml`, `01-placement-cr.yaml`, `02-podchaos.yaml`.

**Workload:** the `token` and `placement` streams against
`keystone-placement-ks-outage` and `placement-ks-outage`, started in step 2 and
judged in step 6 against `failures=400 recovery=150s`, an estimate no run has
measured yet. The suite runs in the `pod` matrix entry. The token stream fails from
the injection until Keystone answers again, and the `placement` stream fails once
Placement needs a token it has not cached. Every step from 2 to 6 dumps the workload
in its catch block.

**Catch blocks:** step 1 calls `../diagnostics.sh baseline
keystone-placement-ks-outage`. The later steps share a YAML anchor that calls
`../diagnostics.sh chaos placement-ks-outage` with `--cr-kind=placement` and
`--dep-label=app.kubernetes.io/instance=keystone-placement-ks-outage`, dumps the
Keystone CR, the pod logs of the Placement CR and of the placement-operator, and
every `PodChaos`, and dumps the workload.

**Design notes:**

- The `PodChaos`, the token the service has never seen and the unbuffered probe
  follow `glance-keystone-outage`, which gives the reasons.

---

### barbican-keystone-outage

**File:** `tests/e2e-chaos/barbican-keystone-outage/chainsaw-test.yaml`

**Scenario:** none

**Purpose:** A `PodChaos` `pod-failure` takes the API pods of the suite's own
Keystone away while Barbican keeps running. The suite pins how Barbican treats a
token it cannot validate: a token issued before the outage, and never shown to
Barbican until then, gets 503 on `GET /v1/secrets`. keystonemiddleware cannot reach
Keystone to validate it and answers `HTTPServiceUnavailable`. The Keystone CR
reports `DeploymentReady=False` in the same window, and the Barbican CR stays
`Ready=True/AllReady`, because no Barbican condition watches Keystone. Once the
outage is lifted a fresh token gets 200 from the same request.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` + `assert` (5m) | `keystone-barbican-ks-outage` (database `keystone_barbican_ks_outage`) reaches `Ready=True/AllReady` |
| 2 | Apply Barbican and its store | `apply` + `assert` (5m) + `script` (6m) | `barbican-ks-outage` (database `barbican_ks_outage`, one replica) and the store `barbican-ks-outage-store` on `openbao-instance` reach `Ready=True/AllReady`, the CR with `SecretStoresReady=True/AllStoresProjected`. `workload.sh start` then starts the workload and waits for `READY` |
| 3 | Take Keystone away | `script` (8m) + `assert` (5m) | Pod `barbican-ks-outage-failclosed-probe` on `ghcr.io/c5c3/tempest:2026.1` issues a token T1 and prints `TOKEN-ISSUED`. The script then runs `workload.sh mark fault-start`, applies `03-podchaos.yaml` and waits for `AllInjected`. The probe waits up to 180 s until Keystone has refused every connection for 6 s and sends `GET /v1/secrets` with T1 once: 503 prints `FAILCLOSED-OK`, a 2xx fails, and any other answer is retried every 5 s for up to 120 s. The asserts read `DeploymentReady` `False` on the Keystone CR and `Ready=True/AllReady` on the Barbican CR. The step cleanup deletes the `PodChaos` |
| 4 | Lift the outage | `delete` + `script` (1m) | Removes `PodChaos/fail-keystone-barbican`, then `workload.sh mark fault-end` |
| 5 | Assert recovery | `assert` (5m) + `script` (7m) | The Keystone CR is back at `Ready=True/AllReady`. Pod `barbican-ks-outage-recovery-probe` issues a fresh token and repeats the request, retrying a 401 or a 503 every 5 s for up to 180 s, until a 200 prints `RECOVERY-OK` |
| 6 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-keystone-cr.yaml`, `01-barbican-cr.yaml`,
`02-barbican-secretstore.yaml`, `03-podchaos.yaml`.

**Workload:** the `token` and `secret` streams against `keystone-barbican-ks-outage`
and `barbican-ks-outage`, started in step 2 and judged in step 6 against
`failures=400 recovery=150s`, an estimate no run has measured yet. The suite runs in
the `pod` matrix entry. The token stream fails from the injection until Keystone
answers again, and the `secret` stream fails once Barbican needs a token it has not
cached. Every step from 2 to 6 dumps the workload in its catch block.

**Catch blocks:** step 1 calls `../diagnostics.sh baseline
keystone-barbican-ks-outage`. The later steps share a YAML anchor that calls
`../diagnostics.sh chaos barbican-ks-outage` with `--cr-kind=barbican` and
`--dep-label=app.kubernetes.io/instance=keystone-barbican-ks-outage`, dumps the
Keystone CR, the pod logs of the Barbican CR and of the barbican-operator, and every
`PodChaos`, and dumps the workload.

**Design notes:**

- The Barbican CR runs one replica, where `barbican-openbao-outage` runs two to keep
  `/healthcheck` served beside a worker that waits on its partition. A request whose
  token cannot be validated is answered at once.
- The `PodChaos`, the token the service has never seen and the unbuffered probe
  follow `glance-keystone-outage`, which gives the reasons.

---

### neutron-keystone-outage

**File:** `tests/e2e-chaos/neutron-keystone-outage/chainsaw-test.yaml`

**Scenario:** none

**Purpose:** A `PodChaos` `pod-failure` takes the API pods of the suite's own
Keystone away while Neutron keeps running. The suite pins how Neutron treats a token
it cannot validate: a token issued before the outage, and never shown to Neutron
until then, gets 503 on `GET /v2.0/networks`. keystonemiddleware cannot reach
Keystone to validate it and answers `HTTPServiceUnavailable`. The Keystone CR
reports `DeploymentReady=False` in the same window, and the Neutron CR stays
`Ready=True/AllReady`, because no Neutron condition watches Keystone. Once the
outage is lifted a fresh token gets 200 from the same request.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` + `assert` (5m) | `keystone-neutron-ks-outage` (database `keystone_neutron_ks_outage`) reaches `Ready=True/AllReady` |
| 2 | Apply the OVN control plane and Neutron | `apply` + `assert` (5m) + `script` (6m) | The broker Secret `neutron-ks-outage-messaging`, `neutron-ks-outage-ovn` and `neutron-ks-outage` (database `neutron_ks_outage`) reach `Ready=True/AllReady`. `workload.sh start` then starts the workload and waits for `READY` |
| 3 | Take Keystone away | `script` (8m) + `assert` (5m) | Pod `neutron-ks-outage-failclosed-probe` on `ghcr.io/c5c3/tempest:2026.1` issues a token T1 and prints `TOKEN-ISSUED`. The script then runs `workload.sh mark fault-start`, applies `04-podchaos.yaml` and waits for `AllInjected`. The probe waits up to 180 s until Keystone has refused every connection for 6 s and sends `GET /v2.0/networks` with T1 once: 503 prints `FAILCLOSED-OK`, a 2xx fails, and any other answer is retried every 5 s for up to 120 s. The asserts read `DeploymentReady` `False` on the Keystone CR and `Ready=True/AllReady` on the Neutron CR. The step cleanup deletes the `PodChaos` |
| 4 | Lift the outage | `delete` + `script` (1m) | Removes `PodChaos/fail-keystone-neutron`, then `workload.sh mark fault-end` |
| 5 | Assert recovery | `assert` (5m) + `script` (7m) | The Keystone CR is back at `Ready=True/AllReady`. Pod `neutron-ks-outage-recovery-probe` issues a fresh token and repeats the request, retrying a 401 or a 503 every 5 s for up to 180 s, until a 200 prints `RECOVERY-OK` |
| 6 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-keystone-cr.yaml`, `01-messaging-secret.yaml`,
`02-ovncentral-cr.yaml`, `03-neutron-cr.yaml`, `04-podchaos.yaml`.

**Workload:** the `token` and `network` streams against `keystone-neutron-ks-outage`
and `neutron-ks-outage`, started in step 2 and judged in step 6 against
`failures=400 recovery=150s`, an estimate no run has measured yet. The suite runs in
the `network` matrix entry. The token stream fails from the injection until Keystone
answers again, and the `network` stream fails once Neutron needs a token it has not
cached. Every step from 2 to 6 dumps the workload in its catch block.

**Catch blocks:** step 1 calls `../diagnostics.sh baseline
keystone-neutron-ks-outage`. The later steps share a YAML anchor that calls
`../diagnostics.sh chaos neutron-ks-outage` with `--cr-kind=neutron` and
`--dep-label=app.kubernetes.io/instance=keystone-neutron-ks-outage`, dumps the
Keystone CR, the pod logs of the Neutron CR and of the neutron-operator, and every
`PodChaos`, and dumps the workload.

**Design notes:**

- The stack is the one of `neutron-mariadb-outage` under new names: a broker Secret
  whose host never resolves, a single-member OVNCentral and one API replica.
- The `PodChaos`, the token the service has never seen and the unbuffered probe
  follow `glance-keystone-outage`, which gives the reasons.

---

### cinder-keystone-outage

**File:** `tests/e2e-chaos/cinder-keystone-outage/chainsaw-test.yaml`

**Scenario:** none

**Purpose:** A `PodChaos` `pod-failure` takes the API pods of the suite's own
Keystone away while Cinder keeps running. The suite pins how Cinder treats a token
it cannot validate: a token issued before the outage, and never shown to Cinder
until then, gets 503 on `GET /v3/{project_id}/volumes`. keystonemiddleware cannot
reach Keystone to validate it and answers `HTTPServiceUnavailable`. The probe reads
the project id off the body of T1. The Keystone CR reports `DeploymentReady=False`
in the same window, and the Cinder CR stays `Ready=True/AllReady`, because no Cinder
condition watches Keystone. Once the outage is lifted a fresh token gets 200 from
the same request.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` + `assert` (5m) | `keystone-cinder-ks-outage` (database `keystone_cinder_ks_outage`) reaches `Ready=True/AllReady` |
| 2 | Give the suite its vhost, apply Cinder | `script` (2m) + `apply` + `assert` (5m) + `script` (6m) | `../../e2e/cinder/broker-vhost.sh create cinder-ks-outage cinder-ks-outage-messaging openstack`, then `cinder-ks-outage` (database `cinder_ks_outage`, `keystoneEndpoint` and `serviceUser` set) and the NFS backend `ks-outage-nfs1` reach `Ready=True/AllReady`. The step cleanup deletes the vhost. `workload.sh start` then starts the workload and waits for `READY` |
| 3 | Take Keystone away | `script` (8m) + `assert` (5m) | Pod `cinder-ks-outage-failclosed-probe` on `ghcr.io/c5c3/tempest:2026.1` issues a token T1 and prints `TOKEN-ISSUED`. The script then runs `workload.sh mark fault-start`, applies `03-podchaos.yaml` and waits for `AllInjected`. The probe waits up to 180 s until Keystone has refused every connection for 6 s and sends `GET /v3/{project_id}/volumes` with T1 once: 503 prints `FAILCLOSED-OK`, a 2xx fails, and any other answer is retried every 5 s for up to 120 s. The asserts read `DeploymentReady` `False` on the Keystone CR and `Ready=True/AllReady` on the Cinder CR. The step cleanup deletes the `PodChaos` |
| 4 | Lift the outage | `delete` + `script` (1m) | Removes `PodChaos/fail-keystone-cinder`, then `workload.sh mark fault-end` |
| 5 | Assert recovery | `assert` (5m) + `script` (7m) | The Keystone CR is back at `Ready=True/AllReady`. Pod `cinder-ks-outage-recovery-probe` issues a fresh token and repeats the request, retrying a 401 or a 503 every 5 s for up to 180 s, until a 200 prints `RECOVERY-OK` |
| 6 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-keystone-cr.yaml`, `01-cinder-cr.yaml`,
`02-cinderbackend-cr.yaml`, `03-podchaos.yaml`.

**Workload:** the `token` and `volume` streams against `keystone-cinder-ks-outage`
and `cinder-ks-outage`, started in step 2 and judged in step 6 against `failures=400
recovery=150s`, an estimate no run has measured yet. The suite runs in the `network`
matrix entry. The token stream fails from the injection until Keystone answers
again, and the `volume` stream fails once Cinder needs a token it has not cached.
Every step from 2 to 6 dumps the workload in its catch block.

**Catch blocks:** step 1 calls `../diagnostics.sh baseline
keystone-cinder-ks-outage`. The later steps share a YAML anchor that calls
`../diagnostics.sh chaos cinder-ks-outage` with `--cr-kind=cinder` and
`--dep-label=app.kubernetes.io/instance=keystone-cinder-ks-outage`, dumps the
Keystone CR, the pod logs of the Cinder CR and of the cinder-operator, and every
`PodChaos`, and dumps the workload.

**Design notes:**

- This is the first chaos suite with an authenticated Cinder. `keystoneEndpoint` and
  `serviceUser` are set together, as the pairing rule on the spec requires; without
  them the API renders `auth_strategy = noauth` and validates nothing the outage
  could break. With `--keystone` the volume stream uses the `/v3/{project_id}`
  paths.
- The `PodChaos`, the token the service has never seen and the unbuffered probe
  follow `glance-keystone-outage`, which gives the reasons.

---

### nova-keystone-outage

**File:** `tests/e2e-chaos/nova-keystone-outage/chainsaw-test.yaml`

**Scenario:** none

**Purpose:** A `PodChaos` `pod-failure` takes the API pods of the suite's own
Keystone away under a full Nova deployment. A token issued before the outage, and
never shown to Nova until then, gets 503 on `GET /v2.1/servers`: keystonemiddleware
cannot reach Keystone to validate it and answers `HTTPServiceUnavailable`. The
Keystone CR reports `DeploymentReady=False` in the same window, while the Nova CR
stays `Ready=True/AllReady` with `NovaAPIReady=True/APIHealthy`,
`SchedulerReady=True` and `ConductorReady=True`. The API's probes send no token, and
the scheduler and the conductor take their readiness off the broker socket, so the
outage does not reach the conditions. After the outage a fresh token lists the
servers with 200, and the workload's verify audits Placement and compares the
Neutron and OVN databases.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost | `script` (2m) | `../../e2e/cinder/broker-vhost.sh create nova-ks-chaos nova-ks-chaos-messaging openstack`; the step cleanup deletes the vhost |
| 2 | Bring up Keystone | `apply` + `assert` (8m) | `keystone-nova-ks-chaos` reaches `Ready=True/AllReady` |
| 3 | Register the four services | `script` (6m) | Job `nova-ks-chaos-catalog-setup` writes the public and internal endpoints of compute, placement, image and network |
| 4 | Bring up the four services | `apply` + `assert` (8m each) | `nova-ks-chaos-ovn`, `neutron-nova-ks-chaos` (with `spec.ovnDBSync: {syncMode: log, suspend: true}`), `placement-nova-ks-chaos`, `glance-nova-ks-chaos` and `glance-nova-ks-chaos-s3` reach `Ready=True/AllReady` |
| 5 | Seed the image | `script` (6m) | Job `nova-ks-chaos-image-seed` uploads image `4e0a0000-0000-4000-8000-000000000008` |
| 6 | Apply the Nova CR | `apply` + `assert` (8m) | `nova-ks-chaos` reaches its conditions and `Ready=True/AllReady` |
| 7 | Start the fake compute | `apply` + `assert` (5m) + `script` (3m) | `nova-ks-chaos-fake-compute` becomes available and `../../e2e/nova/discover-hosts.sh` maps `fake-1` into cell1 |
| 8 | Seed the flavor | `script` (6m) | Job `nova-ks-chaos-flavor-seed` creates `m1.nano` and waits for the compute service to report `up` |
| 9 | Start the workload | `script` (6m) | `workload.sh start` with the `token`, `network`, `server` and `image` streams |
| 10 | Take Keystone away | `script` (8m) + `assert` (5m) | The probe of `glance-keystone-outage` against `GET /v2.1/servers` with an expected 503, around `workload.sh mark fault-start` and `14-podchaos.yaml`; the asserts read `DeploymentReady` `False` on the Keystone CR and the four conditions above on the Nova CR. The step cleanup deletes the `PodChaos` |
| 11 | Lift the outage | `delete` + `script` (1m) | Removes `PodChaos/fail-keystone-nova`, then `workload.sh mark fault-end` |
| 12 | Assert recovery | `assert` (5m) + `script` (7m) | The Keystone CR is back at `Ready=True/AllReady`, and a fresh token gets 200 from `GET /v2.1/servers` within 180 s (`RECOVERY-OK`) |
| 13 | Judge the workload | `script` (25m) | `workload.sh verify --placement-audit nova-ks-chaos --ovn-db-sync neutron-nova-ks-chaos` |
| 14 | Tear the deployment down | `script` (10m) | Deletes the fake compute and the three Jobs, then the Nova CR, and waits up to 180 s for its pods |

**Fixtures:** `01-keystone-cr.yaml` to `12-fake-compute.yaml`, the stack of
`nova-placement-outage` with every `nova-placement-chaos` renamed to
`nova-ks-chaos`, then `13-flavor-seed-job.yaml` and `14-podchaos.yaml`.

**Workload:** the `token`, `network`, `server` and `image` streams against the
suite's Keystone, Neutron, Nova and Glance, started in step 9 and judged in step 13
against `failures=500 recovery=180s`, an estimate no run has measured yet. The suite
runs in the `nova` matrix entry. The token stream fails from the injection until
Keystone answers again. The other streams fail once a service on their path needs a
token it has not cached, a build included: the scheduler's Placement call and the
compute's image download present tokens of their own.

**Catch blocks:** a shared YAML anchor calls `../diagnostics.sh chaos nova-ks-chaos`
with `--cr-kind=nova`,
`--dep-label=app.kubernetes.io/instance=keystone-nova-ks-chaos` and
`--log-label=app.kubernetes.io/instance=nova-ks-chaos`, dumps every `PodChaos`, the
Keystone, OVNCentral, Neutron, Placement, Glance and GlanceBackend CRs, the fake
compute and Job logs, the logs of the six operators and the events, and dumps the
workload.

**Design notes:**

- The `PodChaos`, the token the service has never seen and the unbuffered probe
  follow `glance-keystone-outage`, which gives the reasons. The service tokens Nova
  presents to Placement, Neutron and Glance are cached the same way.
- The stack has no baseline Job, so step 8 seeds `m1.nano` for the server stream and
  waits for the compute. The scheduler keeps its one worker and the urllib3
  connection logs of `nova-placement-outage`; here the logs show when each Nova
  workload reconnected to Keystone.

---

### glance-mariadb-outage

**File:** `tests/e2e-chaos/glance-mariadb-outage/chainsaw-test.yaml`

**Scenario:** none

**Purpose:** A `NetworkChaos` partition severs the Glance API pods from MariaDB,
enforced on the MariaDB (server) side for the reason `mariadb-network-partition`
documents. The suite pins the fail-closed contract of a partitioned database: a
write that cannot reach it must not answer 2xx, and the record it would have created
must be absent once the database is back. The readiness probe GETs `/healthcheck`,
which touches no table, and the operator's database condition reads the MariaDB CRs,
which the partition leaves alone, so `Ready=True/AllReady` holds while
database-backed requests fail.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` + `assert` (5m) | `keystone-glance-db-chaos` (database `keystone_glance_db_chaos`) reaches `Ready=True/AllReady`; the probes and the workload authenticate against it |
| 2 | Apply Glance and its store | `apply` + `assert` (5m) + `script` (6m) | `glance-db-chaos` (database `glance_db_chaos`, `harakiri: 20`) and `glance-db-chaos-s3` reach `Ready=True/AllReady`; `workload.sh start` waits for `READY` |
| 3 | Inject NetworkChaos | `script` (1m) + `apply` + `script` (2m) | `workload.sh mark fault-start`, then `03-networkchaos.yaml`: `partition-mariadb-glance` (`direction: both`, `duration: "600s"`) between the MariaDB pods and the API pods of `glance-db-chaos`, then `kubectl wait --for=condition=AllInjected`. The step cleanup deletes it |
| 4 | Assert fail-closed | `script` (5m) + `assert` (5m) | Pod `glance-db-chaos-failclosed-probe`: `GET /healthcheck` answers 200, a token is still obtainable from `keystone-glance-db-chaos`, and one `POST /v2/images` with `{"name": "db-outage-failclosed", "disk_format": "raw", "container_format": "bare"}`, bounded at 90 s, answers 5xx or times out. A 2xx or a 4xx fails the probe. A second `GET /healthcheck` answers 200 (`FAILCLOSED-OK`). The assert reads `Ready=True/AllReady` on the CR |
| 5 | Lift the partition | `delete` + `script` (1m) | Removes `NetworkChaos/partition-mariadb-glance`, then `workload.sh mark fault-end` |
| 6 | Assert recovery | `script` (6m) | Pod `glance-db-chaos-recovery-probe`: `GET /v2/images?name=db-outage-failclosed` lists nothing, and a fresh image answers 201; the probe deletes what it created (`RECOVERY-OK`) |
| 7 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-keystone-cr.yaml`, `01-glance-cr.yaml`,
`02-glancebackend-cr.yaml`, `03-networkchaos.yaml`.

**Workload:** the `token` and `image` streams against `keystone-glance-db-chaos` and
`glance-db-chaos`, started before the partition and judged at the end against
`failures=40 recovery=90s`, an estimate no run has measured yet. The suite runs in
the `network` matrix entry. The streams through the partitioned API fail about once
every 16 seconds while it lasts, because a request that reaches the database waits
until the workload's 15-second timeout. Every step from the workload start to
`verify` dumps the workload in its catch block.

**Catch blocks:** a shared YAML anchor calls `../diagnostics.sh chaos
glance-db-chaos` with `--cr-kind=glance`,
`--dep-label=app.kubernetes.io/name=mariadb` and
`--log-label=app.kubernetes.io/instance=glance-db-chaos`, dumps every NetworkChaos
and the operator logs, and dumps the workload.

**Design notes:**

- A 4xx during the partition fails the suite, for the reason
  `neutron-mariadb-outage` gives: an auth-pipeline regression would otherwise pass
  for proof. The write is attempted once, so a retry cannot store a second record
  under the same name.
- The recovery listing by name separates a write that failed from one the client
  gave up on while the worker completed it. The listing retries a 5xx for up to two
  minutes, because the SQLAlchemy pool hands out a connection that died in the
  partition once more before it reconnects.
- The CR sets `harakiri: 20` on its uWSGI server. A request that reaches the
  database waits on a dead connection for as long as TCP retransmits, and the
  workload sends one about every 16 seconds; without a bound both workers of the pod
  would wait, and the readiness probe would find no free worker. The bound also ends
  the fail-closed write before the partition is lifted, so it cannot complete
  afterwards. `glance-garage-outage` bounds its upload the same way.
- The target is narrowed to `app.kubernetes.io/component: api`. The Job pods carry
  the same name and instance labels and reach the same database, so partitioning
  them would widen the fault past what the suite asserts.
- A Glance image record without data stays `queued`, which the `images` check of the
  workload reports, so the recovery probe deletes the image it created.

---

### placement-mariadb-outage

**File:** `tests/e2e-chaos/placement-mariadb-outage/chainsaw-test.yaml`

**Scenario:** none

**Purpose:** A `NetworkChaos` partition severs the Placement API pods from MariaDB,
enforced on the MariaDB (server) side for the reason `mariadb-network-partition`
documents. The suite pins the fail-closed contract of a partitioned database: a
write that cannot reach it must not answer 2xx, and the record it would have created
must be absent once the database is back. The readiness probe GETs `/`, which
touches no table, and the operator's database condition reads the MariaDB CRs, which
the partition leaves alone, so `Ready=True/AllReady` holds while database-backed
requests fail.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` + `assert` (5m) | `keystone-placement-db-chaos` (database `keystone_placement_db_chaos`) reaches `Ready=True/AllReady`; the probes and the workload authenticate against it |
| 2 | Apply Placement | `apply` + `assert` (5m) + `script` (6m) | `placement-db-chaos` (database `placement_db_chaos`, `extraConfig` with `connection_parameters: read_timeout=10&write_timeout=10` under `placement_database`) reaches `Ready=True/AllReady`; `workload.sh start` waits for `READY` |
| 3 | Inject NetworkChaos | `script` (1m) + `apply` + `script` (2m) | `workload.sh mark fault-start`, then `02-networkchaos.yaml`: `partition-mariadb-placement` (`direction: both`, `duration: "600s"`) between the MariaDB pods and the API pods of `placement-db-chaos`, then `kubectl wait --for=condition=AllInjected`. The step cleanup deletes it |
| 4 | Assert fail-closed | `script` (5m) + `assert` (5m) | Pod `placement-db-chaos-failclosed-probe`: `GET /` answers 200, a token is still obtainable from `keystone-placement-db-chaos`, and one `POST /resource_providers` with `{"name": "db-outage-failclosed"}`, bounded at 90 s, answers 5xx or times out. A 2xx or a 4xx fails the probe. A second `GET /` answers 200 (`FAILCLOSED-OK`). The assert reads `Ready=True/AllReady` on the CR |
| 5 | Lift the partition | `delete` + `script` (1m) | Removes `NetworkChaos/partition-mariadb-placement`, then `workload.sh mark fault-end` |
| 6 | Assert recovery | `script` (6m) | Pod `placement-db-chaos-recovery-probe`: `GET /resource_providers?name=db-outage-failclosed` lists nothing, and a fresh provider answers 201; the probe deletes what it created (`RECOVERY-OK`). Placement keeps provider names unique, so the probe deletes every provider named `db-outage-recovered` before the write, and a 409 on a retried write clears the name and writes once more |
| 7 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-keystone-cr.yaml`, `01-placement-cr.yaml`, `02-networkchaos.yaml`.

**Workload:** the `token` and `placement` streams against
`keystone-placement-db-chaos` and `placement-db-chaos`, started before the partition
and judged at the end against `failures=40 recovery=90s`, an estimate no run has
measured yet. The suite runs in the `network` matrix entry. The streams through the
partitioned API fail about once every 16 seconds while it lasts, because a request
that reaches the database waits until the workload's 15-second timeout or the socket
timeouts of the CR, whichever ends first. Every step from the workload start to
`verify` dumps the workload in its catch block.

**Catch blocks:** a shared YAML anchor calls `../diagnostics.sh chaos
placement-db-chaos` with `--cr-kind=placement`,
`--dep-label=app.kubernetes.io/name=mariadb` and
`--log-label=app.kubernetes.io/instance=placement-db-chaos`, dumps every
NetworkChaos and the operator logs, and dumps the workload.

**Design notes:**

- The suite runs in the `network` matrix entry, so that entry loads and deploys the
  placement-operator as the `pod` entry does.
- The 4xx rule, the single bounded write, the listing by name and the narrowed
  target follow `glance-mariadb-outage`, which gives the reasons.
- The bound on a request that reaches the dead database is not `harakiri` but the
  socket timeouts `barbican-mariadb-outage` uses, set in
  `[placement_database] connection_parameters`. Placement reads the database while
  it loads (it syncs the traits and resource classes), so a worker `harakiri` killed
  hangs the same way a Barbican worker does, and the liveness probe restarts the
  container until the partition ends.

---

### barbican-mariadb-outage

**File:** `tests/e2e-chaos/barbican-mariadb-outage/chainsaw-test.yaml`

**Scenario:** none

**Purpose:** A `NetworkChaos` partition severs the Barbican API pods from MariaDB,
enforced on the MariaDB (server) side for the reason `mariadb-network-partition`
documents. The suite pins the fail-closed contract of a partitioned database: a
write that cannot reach it must not answer 2xx, and the record it would have created
must be absent once the database is back. The readiness probe GETs `/healthcheck`,
which touches no table, and the operator's database condition reads the MariaDB CRs,
which the partition leaves alone, so `BarbicanAPIReady=True/APIHealthy` holds while
database-backed requests fail.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` + `assert` (5m) | `keystone-barbican-db-chaos` (database `keystone_barbican_db_chaos`) reaches `Ready=True/AllReady`; the probes and the workload authenticate against it |
| 2 | Apply Barbican and its store | `apply` + `assert` (5m) + `script` (6m) | `barbican-db-chaos` (database `barbican_db_chaos`, `extraConfig` with `connection_parameters: read_timeout=10&write_timeout=10` under `database`) and `barbican-db-chaos-store` reach `Ready=True/AllReady`, the CR with `SecretStoresReady=True/AllStoresProjected`; `workload.sh start` waits for `READY` |
| 3 | Inject NetworkChaos | `script` (1m) + `apply` + `script` (2m) | `workload.sh mark fault-start`, then `03-networkchaos.yaml`: `partition-mariadb-barbican` (`direction: both`, `duration: "600s"`) between the MariaDB pods and the API pods of `barbican-db-chaos`, then `kubectl wait --for=condition=AllInjected`. The step cleanup deletes it |
| 4 | Assert fail-closed | `script` (5m) + `assert` (5m) | Pod `barbican-db-chaos-failclosed-probe`: `GET /healthcheck` answers 200, a token is still obtainable from `keystone-barbican-db-chaos`, and one `POST /v1/secrets` with `{"name": "db-outage-failclosed", "payload": "x", "payload_content_type": "text/plain"}`, bounded at 90 s, answers 5xx or times out. A 2xx or a 4xx fails the probe. A second `GET /healthcheck` answers 200 (`FAILCLOSED-OK`). The assert reads `BarbicanAPIReady=True/APIHealthy` on the CR |
| 5 | Lift the partition | `delete` + `script` (1m) | Removes `NetworkChaos/partition-mariadb-barbican`, then `workload.sh mark fault-end` |
| 6 | Assert recovery | `script` (6m) | Pod `barbican-db-chaos-recovery-probe`: `GET /v1/secrets?name=db-outage-failclosed` lists nothing, and a fresh secret answers 201; the probe deletes what it created (`RECOVERY-OK`) |
| 7 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-keystone-cr.yaml`, `01-barbican-cr.yaml`,
`02-barbican-secretstore.yaml`, `03-networkchaos.yaml`.

**Workload:** the `token` and `secret` streams against `keystone-barbican-db-chaos`
and `barbican-db-chaos`, started before the partition and judged at the end against
`failures=40 recovery=90s`, an estimate no run has measured yet. The suite runs in
the `network` matrix entry. The streams through the partitioned API fail about once
every 16 seconds while it lasts, because a request that reaches the database waits
until the workload's 15-second timeout or the socket timeouts of the CR, whichever
ends first. Every step from the workload start to `verify` dumps the workload in its
catch block.

**Catch blocks:** a shared YAML anchor calls `../diagnostics.sh chaos
barbican-db-chaos` with `--cr-kind=barbican`,
`--dep-label=app.kubernetes.io/name=mariadb` and
`--log-label=app.kubernetes.io/instance=barbican-db-chaos`, dumps every NetworkChaos
and the operator logs, and dumps the workload.

**Design notes:**

- The assert under the partition reads `BarbicanAPIReady` and leaves the aggregate
  `Ready` out, for the reason `barbican-openbao-outage` gives: the store controller
  revalidates its AppRole credentials against OpenBao on its own schedule.
- The 4xx rule, the single bounded write, the listing by name and the narrowed
  target follow `glance-mariadb-outage`, which gives the reasons.
- The bound on a request that reaches the dead database is not `harakiri` but
  pymysql's `read_timeout` and `write_timeout`, which oslo.db appends to the DSN from
  `[database] connection_parameters`. uWSGI respawns a worker `harakiri` killed, the
  worker loads the application again (`--lazy-apps`), and barbican connects to the
  database while it loads, with the operator's `max_retries = -1`. Under the
  partition that worker never comes back, the liveness probe restarts the container,
  and the new one hangs the same way until the partition ends. With the socket
  timeouts a request on a dead connection ends in a 500 within about 20 seconds and
  the worker lives on.

---

### cinder-mariadb-outage

**File:** `tests/e2e-chaos/cinder-mariadb-outage/chainsaw-test.yaml`

**Scenario:** none

**Purpose:** A `NetworkChaos` partition severs the Cinder API pods from MariaDB,
enforced on the MariaDB (server) side for the reason `mariadb-network-partition`
documents. The suite pins the fail-closed contract of a partitioned database: a
write that cannot reach it must not answer 2xx, and the record it would have created
must be absent once the database is back. The readiness probe GETs `/healthcheck`,
which touches no table, and the operator's database condition reads the MariaDB CRs,
which the partition leaves alone, so `Ready=True/AllReady` holds while
database-backed requests fail.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then apply the CRs | `script` (2m) + `apply` + `assert` (5m) + `script` (6m) | `../../e2e/cinder/broker-vhost.sh create cinder-db-chaos cinder-db-chaos-messaging openstack`, then `cinder-db-chaos` (database `cinder_db_chaos`, noauth, `extraConfig` with `connection_parameters: read_timeout=10&write_timeout=10` under `database`) and the NFS backend `db-chaos-nfs1` reach `Ready=True/AllReady`; `workload.sh start` waits for `READY`. The step cleanup deletes the vhost |
| 2 | Inject NetworkChaos | `script` (1m) + `apply` + `script` (2m) | `workload.sh mark fault-start`, then `02-networkchaos.yaml`: `partition-mariadb-cinder` (`direction: both`, `duration: "600s"`) between the MariaDB pods and the API pods of `cinder-db-chaos`, then `kubectl wait --for=condition=AllInjected`. The step cleanup deletes it |
| 3 | Assert fail-closed | `script` (5m) + `assert` (5m) | Pod `cinder-db-chaos-failclosed-probe`: `GET /healthcheck` answers 200, and one `POST /v3/volumes` with `{"volume": {"name": "db-outage-failclosed", "size": 1}}`, bounded at 90 s, answers 5xx or times out. A 2xx or a 4xx fails the probe. A second `GET /healthcheck` answers 200 (`FAILCLOSED-OK`). The assert reads `Ready=True/AllReady` on the CR |
| 4 | Lift the partition | `delete` + `script` (1m) | Removes `NetworkChaos/partition-mariadb-cinder`, then `workload.sh mark fault-end` |
| 5 | Assert recovery | `script` (8m) | Pod `cinder-db-chaos-recovery-probe`: `GET /v3/volumes/detail?all_tenants=1&name=db-outage-failclosed` lists nothing, and a fresh volume reaches `available` within 240 s; the probe deletes what it created (`RECOVERY-OK`) |
| 6 | Judge the workload | `script` (15m) | `workload.sh verify` waits out the recovery window, stops the workload and judges its records and consistency checks against the budget |

**Fixtures:** `00-cinder-cr.yaml`, `01-cinderbackend-cr.yaml`,
`02-networkchaos.yaml`.

**Workload:** the noauth `volume` stream against `cinder-db-chaos`, started before
the partition and judged at the end against `failures=40 recovery=90s`, an estimate
no run has measured yet. The suite runs in the `network` matrix entry. The streams
through the partitioned API fail about once every 16 seconds while it lasts, because
a request that reaches the database waits until the workload's 15-second timeout or
the socket timeouts of the CR, whichever ends first. Every step from the workload
start to `verify` dumps the workload in its catch block.

**Catch blocks:** a shared YAML anchor calls `../diagnostics.sh chaos
cinder-db-chaos` with `--cr-kind=cinder`,
`--dep-label=app.kubernetes.io/name=mariadb` and
`--log-label=app.kubernetes.io/instance=cinder-db-chaos`, dumps every NetworkChaos
and the operator logs, and dumps the workload.

**Design notes:**

- The Cinder runs `auth_strategy = noauth`, as `cinder-operator-pod-kill` does, and
  every request carries the token `admin:admin`, because Keystone is not the subject
  of this suite.
- The scheduler and the volume service keep their database. The target is the API
  alone, so a write fails on one request path with one verdict.
- The 4xx rule, the single bounded write, the listing by name and the narrowed
  target follow `glance-mariadb-outage`, which gives the reasons.
- The bound on a request that reaches the dead database is not `harakiri` but the
  socket timeouts `barbican-mariadb-outage` uses, set in
  `[database] connection_parameters`. Cinder reads the database while it loads (the
  RPC clients of its volume API read the minimum service versions), so a worker
  `harakiri` killed hangs the same way a Barbican worker does, and the liveness probe
  restarts the container until the partition ends.

---

### neutron-operator-pod-kill

**File:** `tests/e2e-chaos/neutron-operator-pod-kill/chainsaw-test.yaml`

**Scenario:** none

**Purpose:** A `PodChaos` kills one of the two neutron-operator pods. The operator
is a control-plane component: the API and the RPC workers it deployed keep running
while it is gone and while it comes back. The suite states that as pod identity:
every workload pod UID of `neutron-chaos-op` is snapshotted before the kill and
compared after the recovery. A replica count patched after the recovery then has to
reach the API Deployment, which proves the recovered operator reconciles.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` + `assert` (5m) | `keystone-neutron-chaos-op` reaches `Ready=True/AllReady` |
| 2 | Apply the OVN control plane and Neutron | `apply` + `assert` (5m) + `script` (6m) | The broker Secret, `neutron-chaos-op-ovn` and `neutron-chaos-op` (one API replica) reach `Ready=True/AllReady`; `workload.sh start` waits for `READY` |
| 3 | Inject chaos and verify one operator pod was replaced | `script` (270s) | Three phases in one script, so the snapshot and the polls share state: read `spec.replicas` off the `neutron-operator` Deployment, snapshot the operator pod UIDs in `neutron-system` and the pod UIDs of `app.kubernetes.io/instance=neutron-chaos-op` in `openstack`, run `workload.sh mark fault-start`, apply `04-podchaos.yaml`, wait up to 120 s for at least one pre-chaos operator UID to disappear, wait up to 120 s for `readyReplicas` to return to the desired count, then compare the workload UIDs against the snapshot. The step cleanup deletes the `PodChaos` |
| 4 | Delete PodChaos | `delete` + `script` (1m) | Removes `PodChaos/kill-neutron-operator` from `neutron-system`, then `workload.sh mark fault-end` |
| 5 | Prove the recovered operator reconciles a spec change | `patch` + `assert` (5m) | `05-patch-scale.yaml` takes `spec.deployment.replicas` to 2; Deployment `neutron-chaos-op` reaches `availableReplicas: 2` and `updatedReplicas: 2`, and the CR is at `Ready=True/AllReady` |
| 6 | Judge the workload | `script` (15m) | `workload.sh verify` against `failures=0 recovery=0s` |

**Fixtures:** `00-keystone-cr.yaml`, `01-messaging-secret.yaml`,
`02-ovncentral-cr.yaml`, `03-neutron-cr.yaml`, `04-podchaos.yaml`,
`05-patch-scale.yaml`.

**Workload:** the `token` and `network` streams against `keystone-neutron-chaos-op`
and `neutron-chaos-op`, started in step 2 and judged in step 6 against `failures=0
recovery=0s`. The suite runs in the `network` matrix entry. The operator is on no
request path of the streams, so a failed record is a defect to find. Every step from
2 to 6 dumps the workload in its catch block.

**Catch blocks:** a shared YAML anchor calls `../diagnostics.sh chaos
neutron-chaos-op` with `--cr-kind=neutron`,
`--dep-label=app.kubernetes.io/name=neutron-operator` and `--dep-ns=neutron-system`,
dumps the OVNCentral and the Neutron, lists their Deployments and pods, dumps the
Neutron pod logs, and dumps the workload.

**Design notes:**

- The snapshot covers every pre-chaos operator pod UID, for the reason
  `cinder-operator-pod-kill` gives: `PodChaos mode: one` picks its victim at random,
  so a recorded single pod outlives the kill in about half the runs.
- `set -euo pipefail` is omitted in the kill script on purpose, as in
  `cinder-operator-pod-kill`: the polling loops use `${VAR:-0}` defaults and check
  every exit condition explicitly.

---

### ovn-operator-pod-kill

**File:** `tests/e2e-chaos/ovn-operator-pod-kill/chainsaw-test.yaml`

**Scenario:** none

**Purpose:** A `PodChaos` kills one of the two ovn-operator pods. The Northbound and
Southbound members and northd of `ovn-chaos-op` keep running while it is gone, and
so does the Neutron that programs them: every network the workload creates is
written to the Northbound database through `neutron-ovn-chaos-op`, so the stream
proves the OVN control plane serves without its operator. The pod UIDs of both CRs
are snapshotted before the kill and compared after the recovery, and a northd
replica count patched afterwards has to reach the northd Deployment.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` + `assert` (5m) | `keystone-ovn-chaos-op` reaches `Ready=True/AllReady` |
| 2 | Apply the OVN control plane and Neutron | `apply` + `assert` (5m) + `script` (6m) | The broker Secret, `ovn-chaos-op` (one northd replica) and `neutron-ovn-chaos-op` reach `Ready=True/AllReady`; `workload.sh start` waits for `READY` |
| 3 | Inject chaos and verify one operator pod was replaced | `script` (270s) | Three phases in one script, so the snapshot and the polls share state: read `spec.replicas` off the `ovn-operator` Deployment, snapshot the operator pod UIDs in `ovn-system` and the pod UIDs of `app.kubernetes.io/instance=ovn-chaos-op` (nb, sb, northd) and of `app.kubernetes.io/instance=neutron-ovn-chaos-op` in `openstack`, run `workload.sh mark fault-start`, apply `04-podchaos.yaml`, wait up to 120 s for at least one pre-chaos operator UID to disappear, wait up to 120 s for `readyReplicas` to return to the desired count, then compare the workload UIDs against the snapshot. The step cleanup deletes the `PodChaos` |
| 4 | Delete PodChaos | `delete` + `script` (1m) | Removes `PodChaos/kill-ovn-operator` from `ovn-system`, then `workload.sh mark fault-end` |
| 5 | Prove the recovered operator reconciles a spec change | `patch` + `assert` (5m) | `05-patch-scale.yaml` takes `spec.northd.deployment.replicas` to 2; Deployment `ovn-chaos-op-northd` reaches `availableReplicas: 2` and `updatedReplicas: 2`, and the OVNCentral is at `Ready=True/AllReady` |
| 6 | Judge the workload | `script` (15m) | `workload.sh verify` against `failures=0 recovery=0s` |

**Fixtures:** `00-keystone-cr.yaml`, `01-messaging-secret.yaml`,
`02-ovncentral-cr.yaml`, `03-neutron-cr.yaml`, `04-podchaos.yaml`,
`05-patch-scale.yaml`.

**Workload:** the `token` and `network` streams against `keystone-ovn-chaos-op` and
`neutron-ovn-chaos-op`, started in step 2 and judged in step 6 against `failures=0
recovery=0s`. The suite runs in the `network` matrix entry, where the
neutron-operator and the ovn-operator are both deployed. Every step from 2 to 6
dumps the workload in its catch block.

**Catch blocks:** a shared YAML anchor calls `../diagnostics.sh chaos ovn-chaos-op`
with `--cr-kind=ovncentral`, `--dep-label=app.kubernetes.io/name=ovn-operator` and
`--dep-ns=ovn-system`, dumps the OVNCentral and the Neutron, lists their
Deployments, StatefulSets and pods, dumps the Neutron pod logs, and dumps the
workload.

**Design notes:**

- The snapshot covers every pre-chaos operator pod UID, for the reason
  `cinder-operator-pod-kill` gives: `PodChaos mode: one` picks its victim at random,
  so a recorded single pod outlives the kill in about half the runs.
- `set -euo pipefail` is omitted in the kill script on purpose, as in
  `cinder-operator-pod-kill`: the polling loops use `${VAR:-0}` defaults and check
  every exit condition explicitly.
- The OVNCentral backup CronJob is suspended, so no Job pod joins the instance label
  inside the window. A new pod would pass the comparison, which looks only for the
  snapshotted UIDs, and would still blur the dump.

---

### nova-operator-pod-kill

**File:** `tests/e2e-chaos/nova-operator-pod-kill/chainsaw-test.yaml`

**Scenario:** none

**Purpose:** A `PodChaos` kills one of the two nova-operator pods under a full Nova
deployment. The API, the scheduler, the conductor, the metadata API and the console
proxy keep running while it is gone, and so does the fake compute beside them. Every
pod UID with the instance label `nova-chaos-op` is snapshotted before the kill and
compared after the recovery, and an API replica count patched afterwards has to
reach the API Deployment.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 to 8 | Bring up the stack | as `nova-keystone-outage` | The vhost, `keystone-nova-chaos-op`, the catalog Job, the four services, the seed image `4e0a0000-0000-4000-8000-000000000009`, the Nova CR `nova-chaos-op` with one API replica, the fake compute mapped into cell1 and the `m1.nano` flavor |
| 9 | Start the workload | `script` (6m) | `workload.sh start` with the `token`, `network`, `server` and `image` streams |
| 10 | Inject chaos and verify one operator pod was replaced | `script` (270s) | Three phases in one script, so the snapshot and the polls share state: read `spec.replicas` off the `nova-operator` Deployment, snapshot the operator pod UIDs in `nova-system` and the pod UIDs of `app.kubernetes.io/instance=nova-chaos-op` in `openstack`, the fake compute among them, run `workload.sh mark fault-start`, apply `14-podchaos.yaml`, wait up to 120 s for at least one pre-chaos operator UID to disappear, wait up to 120 s for `readyReplicas` to return to the desired count, then compare the workload UIDs against the snapshot. The step cleanup deletes the `PodChaos` |
| 11 | Delete PodChaos | `delete` + `script` (1m) | Removes `PodChaos/kill-nova-operator` from `nova-system`, then `workload.sh mark fault-end` |
| 12 | Prove the recovered operator reconciles a spec change | `patch` + `assert` (5m) | `15-patch-scale.yaml` takes `spec.api.deployment.replicas` to 2; the API Deployment `nova-chaos-op` reaches `availableReplicas: 2` and `updatedReplicas: 2`, and the CR is at `Ready=True/AllReady` |
| 13 | Judge the workload | `script` (25m) | `workload.sh verify --placement-audit nova-chaos-op --ovn-db-sync neutron-nova-chaos-op` against `failures=0 recovery=0s` |
| 14 | Tear the deployment down | `script` (10m) | Deletes the fake compute and the three Jobs, then the Nova CR, and waits up to 180 s for its pods |

**Fixtures:** `01-keystone-cr.yaml` to `12-fake-compute.yaml`, the stack of
`nova-placement-outage` with every `nova-placement-chaos` renamed to
`nova-chaos-op`, then `13-flavor-seed-job.yaml`, `14-podchaos.yaml` and
`15-patch-scale.yaml`.

**Workload:** the `token`, `network`, `server` and `image` streams against the
suite's Keystone, Neutron, Nova and Glance, started in step 9 and judged in step 13
against `failures=0 recovery=0s`, with the placement audit and the OVN db-sync
comparison. The suite runs in the `nova` matrix entry. The operator is on no request
path of the streams, so a failed record is a defect to find.

**Catch blocks:** the shared YAML anchor of `nova-keystone-outage`, with
`--dep-label=app.kubernetes.io/name=nova-operator` and `--dep-ns=nova-system`.

**Design notes:**

- The snapshot covers every pre-chaos operator pod UID, for the reason
  `cinder-operator-pod-kill` gives: `PodChaos mode: one` picks its victim at random,
  so a recorded single pod outlives the kill in about half the runs.
- `set -euo pipefail` is omitted in the kill script on purpose, as in
  `cinder-operator-pod-kill`: the polling loops use `${VAR:-0}` defaults and check
  every exit condition explicitly.
- The fake compute carries the instance label `nova-chaos-op`, so the snapshot
  counts it. It is outside the operator's reach as well and must not restart either.

---

### cinder-volume-pod-kill

**File:** `tests/e2e-chaos/cinder-volume-pod-kill/chainsaw-test.yaml`

**Scenario:** none

**Purpose:** A `PodChaos` kills the volume service of the one backend while the
workload's volume stream attaches a volume. A volume service lost in the middle of a
request is the fault that leaves state behind: cinder 28.0.0 `init_host` resets
`creating` and `downloading` volumes to `error` and resumes `deleting` ones, and it
leaves a `reserved` volume as it is. The verdict is the harness's. The failed cycle
and its `cleanup-volume` record fall in the fault window and are budgeted, the sweep
then deletes the attachments and the volumes the kill left behind, and the `volumes`
check requires nothing transitional to remain.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, apply the CRs | `script` (2m) + `apply` + `assert` (5m) + `script` (6m) | `../../e2e/cinder/broker-vhost.sh create cinder-volume-kill cinder-volume-kill-messaging openstack`, then `cinder-volume-kill` (database `cinder_volume_kill`, noauth) and `vol-kill-nfs1` reach `Ready=True/AllReady`; `workload.sh start` waits for `READY`. The step cleanup deletes the vhost |
| 2 | Kill the volume service while a volume attaches | `script` (8m) | Snapshot the UID of the pod with `app.kubernetes.io/component=volume-vol-kill-nfs1`, then `workload.sh mark fault-start`. `kubectl exec` into the workload pod polls `GET /v3/volumes/detail?all_tenants=1` every second for up to 240 s until a volume named `chaos-wl-cinder-volume-pod-kill-` is `reserved` or `attaching` after a listing that showed none (`VOLUME-ATTACHING <id>`, or `FAIL: no attach observed in 240s`), so the attach the kill meets started inside the fault window. Then `02-podchaos.yaml`, the old UID gone within 120 s and Deployment `cinder-volume-kill-volume-vol-kill-nfs1` available with a new pod within 300 s. The step cleanup deletes the `PodChaos` |
| 3 | Delete PodChaos | `delete` + `script` (1m) | Removes `PodChaos/kill-cinder-volume`, then `workload.sh mark fault-end` |
| 4 | The volume service is back | `assert` (5m) | `VolumeServicesReady=True/AllVolumeServicesReady` and `Ready=True/AllReady` |
| 5 | Judge the workload | `script` (15m) | `workload.sh verify`: the budget, the sweep and `CONSISTENCY-OK volumes` |

**Fixtures:** `00-cinder-cr.yaml`, `01-cinderbackend-cr.yaml`, `02-podchaos.yaml`.

**Workload:** the noauth `volume` stream against `cinder-volume-kill`, started in
step 1 and judged in step 5 against `failures=40 recovery=210s`, an estimate no run
has measured yet. The suite runs in the `network` matrix entry. The cycle the kill
meets fails, and so does every cycle whose create or attach needs the volume service
before the new pod has started. Every step from 1 to 5 dumps the workload in its
catch block.

**Catch blocks:** a shared YAML anchor calls `../diagnostics.sh chaos
cinder-volume-kill` with `--cr-kind=cinder` and
`--dep-label=app.kubernetes.io/name=cinder-operator`, dumps the `CinderBackend`, the
Deployments, pods and pod logs of the CR and every `PodChaos`, and dumps the
workload.

**Design notes:**

- The UID is read before the attach poll, so the `PodChaos` follows the sighting
  with no read in between. The window of an attach is short, so the kill meets the
  attach itself or the cycle right after it.
- A volume that Cinder cannot clear through the attachments API alone fails the
  sweep outside the window, and the suite with it. That is a finding to file against
  the operator or upstream, and raising the budget does not fix it.
- The selector carries the component `volume-vol-kill-nfs1`, so the API and the
  scheduler stay up and the fault reaches the volume service of one backend.

---

### cinder-broker-pod-kill

**File:** `tests/e2e-chaos/cinder-broker-pod-kill/chainsaw-test.yaml`

**Scenario:** none

**Purpose:** A `PodChaos` kills the one pod of the RabbitmqCluster a Cinder uses in
managed messaging mode. The StatefulSet creates the pod again and the broker comes
back on its own volume. A killed broker closes its connections, so oslo.messaging in
the scheduler and the volume service sees the loss at once and reconnects to the new
pod, where the partition of `cinder-broker-outage` leaves the socket open until the
heartbeat gives up. The suite pins that the two bus consumers recover on their own,
so `SchedulerReady` and `VolumeServicesReady` return to `True`, and that the API,
whose probes never touch the bus, does not restart.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Bring up the broker | `apply` + `script` (11m) | `RabbitmqCluster/cinder-broker-kill-rabbitmq` (`replicas: 1`) waited on `AllReplicasReady` |
| 2 | Apply the Cinder CR and its backend | `apply` + `assert` (5m) + `script` (6m) | `cinder-broker-kill` (managed messaging via `messaging.clusterRef`, database `cinder_broker_kill`, noauth) and `broker-kill-nfs1` reach `Ready=True/AllReady`; `workload.sh start` waits for `READY` |
| 3 | Kill the broker pod | `script` (8m) | Snapshot the UID of `cinder-broker-kill-rabbitmq-server-0`, `workload.sh mark fault-start`, apply `03-podchaos.yaml` (`pod-kill`, `mode: all`, `gracePeriod: 0`), wait up to 120 s for the UID to change and up to 300 s for the new pod to be Ready. The step cleanup deletes the `PodChaos` |
| 4 | Delete PodChaos | `delete` + `script` (1m) | Removes `PodChaos/kill-cinder-broker`, then `workload.sh mark fault-end` |
| 5 | The bus consumers reconnect | `assert` (5m) + `script` (2m) | `SchedulerReady=True/SchedulerReady`, `VolumeServicesReady=True/AllVolumeServicesReady` and `Ready=True/AllReady`; the API container is ready at `restartCount` 0 (`API-UP-OK`) |
| 6 | Judge the workload | `script` (15m) | `workload.sh verify` |
| 7 | Tear the broker down | `script` (8m) | Deletes the Cinder CR and waits for its pods, then deletes the RabbitmqCluster with foreground propagation and waits for its pod |

**Fixtures:** `00-rabbitmqcluster.yaml`, `01-cinder-cr.yaml`,
`02-cinderbackend-cr.yaml`, `03-podchaos.yaml`.

**Workload:** the noauth `volume` stream against `cinder-broker-kill`, started in
step 2 and judged in step 6 against `failures=20 recovery=210s`, an estimate no run
has measured yet. The suite runs in the `network` matrix entry. A create whose cast
meets the missing broker does not reach `available` within the stream's 120 seconds.
Every step from 2 to 6 dumps the workload in its catch block.

**Catch blocks:** a shared YAML anchor calls `../diagnostics.sh chaos
cinder-broker-kill` with `--cr-kind=cinder`, dumps the `CinderBackend`, the
Deployments, pods and pod logs of the CR, every `PodChaos` and the RabbitmqCluster,
describes the broker pod, and dumps the workload. Step 1 dumps the RabbitmqCluster,
the broker pod and the rabbitmq-cluster-operator logs.

**Design notes:**

- The broker is a RabbitmqCluster of its own. Killing the kind-only shared-rabbitmq
  would take every other suite of the entry down with it.
- `gracePeriod: 0` matters: the broker pod carries the operator's default
  `terminationGracePeriodSeconds` of 604800 seconds, and a graceful delete would run
  its preStop drain instead of losing the broker.
- The operator renders quorum queues, which are durable, so the queues of the
  scheduler and the volume service outlive the pod on the broker's volume.
- Step 7 tears the bus down through the RabbitmqCluster delete with foreground
  propagation, for the reasons `neutron-broker-outage` gives.

---

### memcached-network-partition

**File:** `tests/e2e-chaos/memcached-network-partition/chainsaw-test.yaml`

**Scenario:** none

**Purpose:** A `NetworkChaos` partition severs the API pods of one Keystone from the
shared Memcached, enforced on the Memcached (server) side for the reason
`mariadb-network-partition` documents. `memcached-pod-kill` kills the cache, which
closes every connection at once; under a partition every cache call waits out
oslo.cache's `socket_timeout` of 1 second until the client marks the server dead for
`memcache_dead_retry` (300 seconds). Keystone's `[cache]` sets `memcache_servers`
alone, so those defaults apply. The suite pins that the cache is a performance
dependency: Ready and every sub-condition stay `True` for 120 seconds under the
partition, while a TCP connect from the Keystone API pod to Memcached times out. It
also records what the token stream sees, either answers through the
partition once the server is marked dead or failures as after a Memcached kill.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | `keystone-chaos-mc-part` (database `keystone_chaos_mc_part`) |
| 2 | Assert baseline Ready=True | `assert` (5m) + `script` (6m) | `Ready=True/AllReady`; `workload.sh start` with the `token` stream |
| 3 | Inject NetworkChaos | `script` (1m) + `apply` + `script` (2m) | `workload.sh mark fault-start`, then `partition-memcached` (`direction: both`, `duration: "600s"`) between the `app.kubernetes.io/name: memcached` pods and the API pods of `keystone-chaos-mc-part`, then `kubectl wait --for=condition=AllInjected`. The step cleanup deletes it |
| 4 | Hold the partition for 120 s | `script` (4m) | A TCP connect from the Keystone API pod to `openstack-memcached:11211` has to time out after 3 s (`PARTITION-EFFECTIVE-OK`). Then reads the CR every 10 seconds, twelve times, and fails on the first of `SecretsReady`, `FernetKeysReady`, `DatabaseReady`, `DeploymentReady`, `BootstrapReady` and `Ready` that is not `True` |
| 5 | Lift the partition | `delete` + `script` (1m) | Removes `NetworkChaos/partition-memcached`, then `workload.sh mark fault-end` |
| 6 | Assert recovery | `assert` (5m) + `script` (3m) | `Ready=True/AllReady`, then the connect of step 4 succeeds within 20 tries 3 s apart (`PARTITION-LIFTED-OK`) |
| 7 | Judge the workload | `script` (15m) | `workload.sh verify` |

**Fixtures:** `00-keystone-cr.yaml`, `01-networkchaos.yaml`.

**Workload:** the `token` stream against `keystone-chaos-mc-part`, started in step 2
and judged in step 7 against `failures=500 recovery=180s`, an estimate that assumes
a failure in every cycle of the window. No run has measured it yet, and the measured
line will carry which of the two outcomes the stream saw. The suite runs in the
`network` matrix entry. Every step from 2 to 7 dumps the workload in its catch
block.

**Catch blocks:** step 2 calls `../diagnostics.sh baseline keystone-chaos-mc-part`.
Steps 3, 4 and 6 call `../diagnostics.sh chaos keystone-chaos-mc-part` with
`--dep-label=app.kubernetes.io/name=memcached` and
`--log-label=app.kubernetes.io/instance=keystone-chaos-mc-part`, and step 3 dumps
the NetworkChaos as YAML.

**Design notes:**

- A single Chainsaw assert passes on its first read, so step 4 reads the conditions
  every 10 seconds for the whole 120 seconds. A condition that turns `False` for one
  reconcile fails the step.
- Conditions that stay `True` read the same with no fault at all, so step 4 first
  proves the partition reached the cache path: the connect goes to the address
  Keystone's `[cache]` names, from the API pod the partition targets, and only a
  timeout counts.
- The target carries the instance label of this suite's Keystone. Every other client
  of `openstack-memcached` keeps its cache.

---

### horizon-dashboard-pod-kill

**File:** `tests/e2e-chaos/horizon-dashboard-pod-kill/chainsaw-test.yaml`

**Scenario:** none

**Purpose:** A `PodChaos` kills one of the two dashboard pods of
`horizon-chaos-dash`. The dashboard pods were never faulted directly:
`horizon-memcached-pod-kill` kills the cache and `horizon-operator-pod-kill` the
operator. With two replicas the Service keeps an endpoint through the kill, so the
login page keeps answering apart from the few requests the Service hands the dying
endpoint, and the Deployment replaces the pod with no operator action.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Horizon CR | `apply` | `horizon-chaos-dash` with `spec.deployment.replicas: 2` |
| 2 | Assert baseline Ready=True | `assert` (5m) + `assert` (5m) + `script` (6m) | `Ready=True/AllReady` and Deployment `horizon-chaos-dash` at `readyReplicas: 2`; `workload.sh start` with the `http` stream |
| 3 | Kill one dashboard pod | `script` (6m) | Snapshot both dashboard pod UIDs, `workload.sh mark fault-start`, apply `01-podchaos.yaml` (`pod-kill`, `mode: one`, `gracePeriod: 0`, labels `app.kubernetes.io/name: horizon`, `app.kubernetes.io/instance: horizon-chaos-dash`, `app.kubernetes.io/component: api`), wait up to 120 s for one UID to disappear and up to 300 s for `readyReplicas: 2`. The step cleanup deletes the `PodChaos` |
| 4 | Delete PodChaos | `delete` + `script` (1m) | Removes `PodChaos/kill-horizon-dashboard`, then `workload.sh mark fault-end` |
| 5 | Assert recovery | `assert` (5m) | `Ready=True/AllReady` |
| 6 | Judge the workload | `script` (15m) | `workload.sh verify` |

**Fixtures:** `00-horizon-cr.yaml`, `01-podchaos.yaml`.

**Workload:** the `http` stream against
`http://horizon-chaos-dash.openstack.svc:8080/auth/login/`, as the two sibling
Horizon suites run it, started in step 2 and judged in step 6 against `failures=20
recovery=60s`, an estimate no run has measured yet. The suite runs in the `pod`
matrix entry. Every step from 2 to 6 dumps the workload in its catch block.

**Catch blocks:** step 2 calls `../diagnostics.sh baseline horizon-chaos-dash
--cr-kind=horizon`; steps 3 and 5 call `../diagnostics.sh chaos horizon-chaos-dash`
with `--cr-kind=horizon` and
`--dep-label=app.kubernetes.io/instance=horizon-chaos-dash`.

**Design notes:**

- Both pre-chaos UIDs are snapshotted and one of them has to disappear, because
  `mode: one` picks its victim at random.
- No login stream POSTs credentials to the dashboard. The `http` stream reads the
  login page, which needs neither a session nor Keystone.

---

### neutron-metadata-agent-pod-kill

**File:** `tests/e2e-chaos/neutron-metadata-agent-pod-kill/chainsaw-test.yaml`

**Scenario:** none

**Purpose:** A `PodChaos` kills the pod of a `NeutronMetadataAgent` while a booted
server reads its metadata. The suite boots `meta-chaos-vm` with a port on a network
of its own, backs that port with an OVS interface in a network namespace on the node
the way a hypervisor plugs a guest, and reads the metadata from `169.254.169.254`
the way the guest would: through the agent's `ovnmeta-` namespace, its haproxy and
the Nova metadata API. The haproxy processes die with the agent's container, so the
metadata path fails while the agent is gone. The new pod of the DaemonSet provisions
the network again on its first sync, and the server reads its metadata once more.

**Steps:**

| # | Action | Type | Details |
| --- | --- | --- | --- |
| 1 | Label the node and take a vhost | `script` (1m) + `script` (2m) | Labels the first node `openstack.c5c3.io/chassis=true` and runs `../../e2e/cinder/broker-vhost.sh create nova-meta-chaos nova-meta-chaos-messaging openstack`. The step cleanups delete the vhost and remove the label from every node |
| 2 to 8 | Bring up the stack | as `nova-keystone-outage` | The stack of `nova-placement-outage` renamed to `nova-meta-chaos`, with the seed image `4e0a0000-0000-4000-8000-00000000000a`; the fake compute registers under the node's name and `../../e2e/nova/discover-hosts.sh` maps that host |
| 9 | Bring up the chassis and the agent | `apply` + `assert` (5m) | `nova-meta-chaos-chassis` reaches `Ready=True/AllReady` with `desiredNumberScheduled: 1` and `numberReady: 1`, then `nova-meta-chaos-agent` reaches `Ready=True/AllReady` with `numberReady: 1`. The probe scripts ConfigMap is applied beside them |
| 10 | Start the workload | `script` (6m) | `workload.sh start` with the `token` and `server` streams |
| 11 | Boot `meta-chaos-vm` | `script` (11m) | Job `nova-meta-chaos-boot` creates `meta-chaos-net` with `meta-chaos-subnet` (`10.99.0.0/24`) and boots `meta-chaos-vm` on it, ACTIVE within 150 s (`BOOT-OK`). The script copies the port id, MAC and address, the metadata port's address and the server id onto the Job as `chaos.c5c3.io/` annotations. The step cleanup deletes the server and the network through the API when the teardown did not run |
| 12 | Plug the port and read the metadata | `script` (8m) | The step waits for `br-int` and adds `metavif` to it with `external_ids:iface-id` of the port through the chassis pod, then probe pod `nova-meta-chaos-plug` moves it into `meta-chaos-ns` with the port's MAC and address, routes `169.254.169.254` through the metadata port, and GETs `meta_data.json` every 5 s for up to 180 s until it names the server (`METADATA-OK`). The step cleanup deletes `metavif` and `meta-chaos-ns` through the chassis and agent pods, deletes the probe pods, and fails when either is left |
| 13 | Kill the agent | `script` (8m) | Snapshot the agent pod UID and start probe pod `nova-meta-chaos-probe-down`, which arms on its first metadata answer (`PROBE-ARMED`) and then GETs every 2 s. Once armed: `workload.sh mark fault-start`, apply `17-podchaos.yaml` (`pod-kill`, `mode: all`, `gracePeriod: 0`), wait up to 120 s for the UID to go, then create `/tmp/kill-applied` in the probe pod. A failed GET counts only when it started after that marker existed, and answers before the first counted failure are the old agent's; the probe stops at the first answer after it, 30 s after it, or 60 s after arming when no GET failed. At least one GET has to fail, and an answer before the new agent container started fails the step (`METADATA-DOWN-OK`). The step cleanup deletes the `PodChaos` |
| 14 | The agent is back | `script` (6m) + `assert` (5m) | The DaemonSet pod is Ready within 300 s; the CR is at `Ready=True/AllReady` with `numberReady: 1` |
| 15 | Read the metadata again | `delete` + `script` (1m) + `script` (7m) | Removes `PodChaos/kill-metadata-agent`, `workload.sh mark fault-end`, then probe pod `nova-meta-chaos-probe-up` GETs every 5 s for up to 180 s until the metadata names the server (`METADATA-RECOVERED-OK`) |
| 16 | Judge the workload | `script` (25m) | `workload.sh verify --placement-audit nova-meta-chaos --ovn-db-sync neutron-nova-meta-chaos` against `failures=0 recovery=0s` |
| 17 | Tear the deployment down | `script` (10m) | Deletes `meta-chaos-vm` and `meta-chaos-net` through the API and fails when either is left, then the fake compute, the four Jobs and the Nova CR |

**Fixtures:** `01-keystone-cr.yaml` to `12-fake-compute.yaml`, the stack of
`nova-placement-outage` renamed to `nova-meta-chaos`, then
`13-flavor-seed-job.yaml`, `14-ovnchassis-cr.yaml`,
`15-neutronmetadataagent-cr.yaml`, `16-boot-job.yaml`, `17-podchaos.yaml` and
`18-probe-scripts.yaml`, plus the helper `probe-lib.sh` the scripts source.

**Workload:** the `token` and `server` streams against the suite's Keystone and
Nova, started in step 10 and judged in step 16 against `failures=0 recovery=0s`,
with the placement audit and the OVN db-sync comparison. The suite runs in the
`nova` matrix entry. The agent is on no boot path of the fake driver, so a failed
server cycle is a defect to find.

**Catch blocks:** the shared YAML anchor of `nova-keystone-outage`, with
`--dep-label=app.kubernetes.io/instance=nova-meta-chaos-agent`, plus the OVNChassis
and the NeutronMetadataAgent, the chassis and agent pods with the agent logs and its
network namespaces, and the logs of the three probe pods.

**Design notes:**

- The fake compute registers under the node's name. Neutron binds a port only to a
  host with a live chassis, and the chassis registers under the node name, so a
  compute named `fake-1` would leave the port of `meta-chaos-vm` unbound and the
  boot in `ERROR`. `tests/tempest/nova-2026-1/11-fake-compute.yaml` does the same.
- The plug routes `169.254.169.254` through the address of the network's
  `network:distributed` port, the classless route OVN's DHCP hands a guest. The
  agent's namespace owns `169.254.169.254` behind that port.
- The down probe ends its window at the start of the new agent's container. The
  DaemonSet replaces the pod within seconds, and the new agent provisions the
  network on its first sync, so a probe that demanded 30 seconds of failures would
  race that sync. So would a probe started after the kill, whose first GET could
  come after the new agent's sync and find no failure, which is why the probe arms
  before the `PodChaos` goes in. The claim it keeps is the one that matters: no
  answer arrives while no agent runs.
- The armed probe GETs the old agent until the kill lands, so a failed GET counts
  only when it started after the script created `/tmp/kill-applied` in the probe
  pod, which the script does once the old pod's UID is gone. A transient failure
  on the old agent followed by its answer would otherwise read as an answer while
  no agent ran.
- The probe pods run on the neutron image, which carries `python3` and `iproute2`,
  and do the namespace work; the OVN image carries no `python3`. The step plugs
  `metavif` through the chassis pod, as the step cleanup and
  `tests/e2e/neutron/metadata-agent/` do. The probe pods run with `hostNetwork`,
  privileged and as root, with `/run/netns` from the host, the shape
  `ovn-southbound-outage` uses, which is why `spec.concurrent` is `false`.
- The nova matrix entry loads the `openvswitch` and `geneve` modules on the host
  (`WITH_OVN_KERNEL_MODULES`), which the chassis DaemonSets need.

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
Apply CR → Assert Ready=True → Inject PodChaos → Poll readyReplicas < desired → Poll readyReplicas = desired
         → Assert ALL conditions=True → Delete PodChaos → Assert Ready=True
```

1. Apply Keystone CR and assert `Ready=True` (baseline)
2. Apply PodChaos to kill a non-critical dependency pod
3. Poll the dependency Deployment until `readyReplicas` drops below `spec.replicas` (confirms chaos took effect)
4. Poll until `readyReplicas` equals `spec.replicas` again (confirms recovery)
5. Assert **all 6 conditions** remain `True` — no regression
6. Delete PodChaos and assert `Ready=True` after recovery

### Operator Self-Recovery (SC-CHAOS-004)

Used when the operator's own pod is killed and the Deployment controller restarts it.
The CR conditions should remain stable because the operator crash is invisible to the
Keystone CR — the Deployment controller handles pod restart, and controller-runtime
re-registers watches and resumes reconciliation.

```text
Apply CR → Assert Ready=True → Snapshot operator pod UIDs → Inject PodChaos
         → Wait one pre-chaos UID gone → Wait readyReplicas = desired
         → Delete PodChaos → Assert Ready=True
```

1. Apply Keystone CR and assert `Ready=True` (baseline)
2. Snapshot the operator pod UIDs and apply PodChaos to kill one operator pod
3. Wait until at least one pre-chaos UID is gone (confirms kill took effect)
4. Wait until the operator Deployment `readyReplicas` equals `spec.replicas` (Deployment controller restarted it)
5. Delete PodChaos and assert `Ready=True` after re-reconciliation

### Workload Fault Tolerance (SC-CHAOS-005)

Used when a workload spawned by the operator (CronJob/Job) fails but the operator should
remain healthy because it checks resource existence and ignores Job run outcomes.

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
Apply CR → Assert Ready=True → Snapshot operator pod UIDs → Inject PodChaos (mode: all)
         → Wait all pre-chaos UIDs gone → Wait readyReplicas = desired
         → Delete PodChaos → Assert all 6 conditions=True → Patch replicas 1→2
         → Assert Deployment replicas=2 + Ready=True
```

1. Apply Keystone CR and assert `Ready=True` (baseline)
2. Snapshot the operator pod UIDs and apply PodChaos with `mode: all` to kill every operator pod
3. Wait until none of the pre-chaos UIDs remain (kill confirmed), then poll operator Deployment `readyReplicas` until it equals `spec.replicas` (recovered)
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
attempt fails continuously, which a single kill-and-restart cycle cannot show.

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
API Deployment's pods are targeted, and neither the operator pod nor the API pods of other CR instances are.

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
| `diagnostics.sh` | Steps 2, 4, 6 | Steps 2, 4, 6 | Steps 2, 4, 6, 7 | Steps 2, 3, 5 | Steps 2, 5, 7 | Steps 2, 4, 5, 7 | Steps 2, 4, 6 | Steps 2, 5, 7 | Steps 2, 3, 5, 7 |
| Target pod status | Steps 4, 6 | Steps 4, 6 | Steps 2, 4, 6, 7 | Steps 3, 5 | Steps 5, 7 | Steps 2, 4, 5, 7 | Steps 2, 4, 6 | Steps 5, 7 | Steps 3, 5, 7 |
| Chaos Mesh experiment status | Steps 4, 6 | Steps 4, 6 | Steps 4, 6, 7 | Steps 3, 5 | Steps 5, 7 | Steps 4, 5, 7 | Steps 4, 6 | Steps 5, 7 | Steps 3, 5, 7 |
| NetworkChaos CR status | — | — | — | — | — | Step 4 | — | — | — |
| Operator logs (`--previous`) | Steps 4, 6 | — | Steps 4, 6, 7 | Steps 3, 5 | — | Steps 4, 5, 7 | Steps 4, 6 | — | Steps 3, 5, 7 |
| Target pod logs (`--previous`) | — | Steps 4, 6 | — | — | Steps 5, 7 | — | — | Steps 5, 7 | — |
| ESO ExternalSecret conditions | — | — | Steps 4, 6, 7 | — | — | — | — | — | — |
| All pod logs | Step 2 | Step 2 | Step 2 | Step 2 | Step 2 | Step 2 | Step 2 | Step 2 | Step 2 |
| Namespace events | Steps 2, 4, 6 | Steps 2, 4, 6 | Steps 2, 4, 6, 7 | Steps 2, 3, 5 | Steps 2, 5, 7 | Steps 2, 4, 5, 7 | Steps 2, 4, 6 | Steps 2, 5, 7 | Steps 2, 3, 5, 7 |
| CronJob/Job status | — | — | — | — | Steps 4, 5 | — | — | — | — |
| Job pod logs | — | — | — | — | Step 5 | — | — | — | — |
| PDB describe | — | — | — | — | — | — | — | Step 3 | — |

All nine scenarios call `diagnostics.sh` in their catch blocks (it collects CR status, pod
status, logs, and events). SC-CHAOS-003 passes `--eso` in Steps 4, 6, and 7. SC-CHAOS-006
also dumps the NetworkChaos CR status in Step 4 to confirm fault injection state.
SC-CHAOS-005 also collects CronJob/Job-specific diagnostics in Steps 4 and 5, and
SC-CHAOS-008 describes the PDB in Step 3.

The thirty-four suites that start a workload also call `../lib/workload.sh dump <id>`
in the catch block of every step from `start` to `verify`. It prints the phase
of the workload pod, its two fault marks and the last 200 lines of its log.

## File Layout

```text
tests/e2e-chaos/
├── chainsaw-config.yaml              Chaos-specific Chainsaw configuration
├── diagnostics.sh                    Shared diagnostic collection script
├── README.md                         Quick-start documentation
├── unseal-openbao.sh                 Re-unseals openbao-0 after a chaos pod-kill
├── api-pod-kill-pdb/                 SC-CHAOS-008: PDB availability guarantee
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-api, replicas: 3)
│   ├── 01-podchaos.yaml              PodChaos targeting keystone API pods (multi-label)
│   └── chainsaw-test.yaml            Test: PDB minAvailable=1, availability maintained
├── barbican-keystone-outage/         Keystone outage under the Barbican API
│   ├── 00-keystone-cr.yaml           Keystone keystone-barbican-ks-outage, the one the outage takes
│   ├── 01-barbican-cr.yaml           Barbican barbican-ks-outage
│   ├── 02-barbican-secretstore.yaml  BarbicanSecretStore barbican-ks-outage-store
│   ├── 03-podchaos.yaml              PodChaos pod-failure on the Keystone API pods
│   └── chainsaw-test.yaml            Test: an unvalidated token gets 503, then 200 after the outage
├── barbican-mariadb-outage/          Database partition under the Barbican API
│   ├── 00-keystone-cr.yaml           Keystone keystone-barbican-db-chaos
│   ├── 01-barbican-cr.yaml           Barbican barbican-db-chaos with database socket timeouts
│   ├── 02-barbican-secretstore.yaml  BarbicanSecretStore barbican-db-chaos-store
│   ├── 03-networkchaos.yaml          NetworkChaos severing barbican-api↔MariaDB traffic (server-side)
│   └── chainsaw-test.yaml            Test: the write fails closed, the API stays up
├── barbican-openbao-outage/          Secret-store partition under the Barbican API
│   ├── 00-keystone-cr.yaml           Keystone keystone-barbican-outage
│   ├── 01-barbican-cr.yaml           Barbican CR barbican-bao-outage, two replicas
│   ├── 02-barbican-secretstore.yaml  Default OpenBao store barbican-bao-outage-store
│   ├── 03-networkchaos.yaml          NetworkChaos severing barbican-api↔OpenBao traffic (server-side)
│   └── chainsaw-test.yaml            Test: BarbicanAPIReady maintained, the write fails closed
├── barbican-operator-pod-kill/       barbican-operator pod kill
│   ├── 00-barbican-cr.yaml           Barbican CR fixture (barbican-chaos-op)
│   ├── 01-barbican-secretstore.yaml  Default OpenBao store barbican-chaos-op-store
│   ├── 02-podchaos.yaml              PodChaos targeting barbican-operator in barbican-system
│   ├── 03-patch-scale.yaml           Patch replicas 1→2 after the operator pod kill
│   └── chainsaw-test.yaml            Test: one operator pod replaced, workload UIDs kept, replica patch
├── cinder-broker-outage/             Message-bus partition under a Cinder
│   ├── 00-rabbitmqcluster.yaml       The broker of this suite (cinder-chaos-rabbitmq)
│   ├── 01-cinder-cr.yaml             Cinder CR cinder-broker-chaos
│   ├── 02-cinderbackend-cr.yaml      NFS backend broker-chaos-nfs1
│   ├── 03-networkchaos.yaml          NetworkChaos severing cinder↔broker traffic (server-side)
│   └── chainsaw-test.yaml            Test: scheduler and volume service NotReady → recovery
├── cinder-broker-pod-kill/           Broker pod kill under a Cinder
│   ├── 00-rabbitmqcluster.yaml       RabbitmqCluster cinder-broker-kill-rabbitmq
│   ├── 01-cinder-cr.yaml             Cinder cinder-broker-kill, managed messaging, noauth
│   ├── 02-cinderbackend-cr.yaml      NFS backend broker-kill-nfs1
│   ├── 03-podchaos.yaml              PodChaos pod-kill of the broker pod
│   └── chainsaw-test.yaml            Test: the bus consumers reconnect, the API never restarts
├── cinder-keystone-outage/           Keystone outage under an authenticated Cinder
│   ├── 00-keystone-cr.yaml           Keystone keystone-cinder-ks-outage
│   ├── 01-cinder-cr.yaml             Cinder cinder-ks-outage with keystoneEndpoint and serviceUser
│   ├── 02-cinderbackend-cr.yaml      NFS backend ks-outage-nfs1
│   ├── 03-podchaos.yaml              PodChaos pod-failure on the Keystone API pods
│   └── chainsaw-test.yaml            Test: an unvalidated token gets 503, then 200 after the outage
├── cinder-mariadb-outage/            Database partition under the Cinder API
│   ├── 00-cinder-cr.yaml             Cinder cinder-db-chaos, noauth, database socket timeouts
│   ├── 01-cinderbackend-cr.yaml      NFS backend db-chaos-nfs1
│   ├── 02-networkchaos.yaml          NetworkChaos severing cinder-api↔MariaDB traffic (server-side)
│   └── chainsaw-test.yaml            Test: the write fails closed, the API stays up
├── cinder-nfs-outage/                NFS export outage under a Cinder
│   ├── 00-cinder-cr.yaml             Cinder CR cinder-nfs-chaos
│   ├── 01-cinderbackend-cr.yaml      NFS backend nfs-chaos-nfs1
│   ├── 02-lock-holder-pod.yaml       Lock holder Pod cinder-nfs-chaos-probe-lock
│   └── chainsaw-test.yaml            Test: Ready maintained, the create fails closed
├── cinder-operator-pod-kill/         cinder-operator pod kill
│   ├── 00-cinder-cr.yaml             Cinder CR cinder-chaos-op
│   ├── 01-cinderbackend-cr.yaml      NFS backend chaos-op-nfs1
│   ├── 02-podchaos.yaml              PodChaos targeting cinder-operator in cinder-system
│   ├── 03-patch-scale.yaml           Patch API replicas to 2 after the operator pod kill
│   └── chainsaw-test.yaml            Test: one operator pod replaced, workload UIDs kept, replica patch
├── cinder-volume-pod-kill/           Volume-service kill in the middle of an attach
│   ├── 00-cinder-cr.yaml             Cinder cinder-volume-kill, noauth
│   ├── 01-cinderbackend-cr.yaml      NFS backend vol-kill-nfs1
│   ├── 02-podchaos.yaml              PodChaos pod-kill of the volume service of vol-kill-nfs1
│   └── chainsaw-test.yaml            Test: killed mid-attach, the sweep leaves nothing transitional
├── cronjob-rotation-failure/         SC-CHAOS-005: CronJob fault tolerance
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-cron)
│   ├── 01-podchaos.yaml              PodChaos pod-failure targeting job pods
│   └── chainsaw-test.yaml            Test: FernetKeysReady=True maintained
├── deletion-stuck-finalizer/         SC-CHAOS-010: Deletion with mariadb-operator down
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-stuck)
│   └── chainsaw-test.yaml            Test: scale mariadb-operator to 0, delete CR, assert stuck → recovery
├── glance-garage-outage/             Garage S3 partition under the Glance API
│   ├── 00-glance-cr.yaml             Glance CR glance-garage-chaos, API open without a token
│   ├── 01-glancebackend-cr.yaml      Default S3 store glance-garage-chaos-s3
│   ├── 02-networkchaos.yaml          NetworkChaos severing glance-api↔Garage traffic (server-side)
│   └── chainsaw-test.yaml            Test: Ready maintained, the upload fails closed
├── glance-keystone-outage/           Keystone outage under the Glance API
│   ├── 00-keystone-cr.yaml           Keystone keystone-glance-ks-outage, the one the outage takes
│   ├── 01-glance-cr.yaml             Glance glance-ks-outage, no anonymous access
│   ├── 02-glancebackend-cr.yaml      S3 backend glance-ks-outage-s3
│   ├── 03-podchaos.yaml              PodChaos pod-failure on the Keystone API pods
│   └── chainsaw-test.yaml            Test: an unvalidated token gets 401, then 200 after the outage
├── glance-mariadb-outage/            Database partition under the Glance API
│   ├── 00-keystone-cr.yaml           Keystone keystone-glance-db-chaos
│   ├── 01-glance-cr.yaml             Glance glance-db-chaos with harakiri: 20
│   ├── 02-glancebackend-cr.yaml      S3 backend glance-db-chaos-s3
│   ├── 03-networkchaos.yaml          NetworkChaos severing glance-api↔MariaDB traffic (server-side)
│   └── chainsaw-test.yaml            Test: the write fails closed, the API stays up
├── glance-operator-pod-kill/         glance-operator pod kill
│   ├── 00-glance-cr.yaml             Glance CR fixture (glance-chaos-op)
│   ├── 01-glance-backend.yaml        Default S3 store glance-chaos-op-s3
│   ├── 02-podchaos.yaml              PodChaos targeting glance-operator in glance-system
│   ├── 03-patch-scale.yaml           Patch replicas 1→2 after the operator pod kill
│   └── chainsaw-test.yaml            Test: one operator pod replaced, workload UIDs kept, replica patch
├── horizon-dashboard-pod-kill/       Dashboard pod kill under a Horizon
│   ├── 00-horizon-cr.yaml            Horizon CR fixture (horizon-chaos-dash, replicas: 2)
│   ├── 01-podchaos.yaml              PodChaos killing one of the two dashboard pods
│   └── chainsaw-test.yaml            Test: one pod replaced, readyReplicas 2, Ready=True
├── horizon-memcached-pod-kill/       Memcached pod kill under a Horizon
│   ├── 00-horizon-cr.yaml            Horizon CR fixture (horizon-chaos-mc)
│   ├── 01-podchaos.yaml              PodChaos targeting memcached in openstack
│   └── chainsaw-test.yaml            Test: Ready=True maintained, no dashboard restart
├── horizon-operator-pod-kill/        horizon-operator pod kill
│   ├── 00-horizon-cr.yaml            Horizon CR fixture (horizon-chaos-op)
│   ├── 01-podchaos.yaml              PodChaos targeting horizon-operator in horizon-system
│   ├── 02-patch-scale.yaml           Patch replicas 1→2 after the operator pod kill
│   └── chainsaw-test.yaml            Test: one operator pod replaced, replica patch
├── keystone-federation/              Federation sidecar kill and IdP outage
│   ├── 00-keycloak.yaml              In-suite Keycloak (keycloak-chaos) importing one realm
│   ├── 01-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-fed, replicas: 2)
│   ├── 02-backend-cr.yaml            OIDC KeystoneIdentityBackend keycloak-chaos
│   ├── 03-container-kill.yaml        PodChaos container-kill on federation-proxy in one pod
│   └── chainsaw-test.yaml            Test: sidecar restart → recovery, IdP outage fails closed
├── lib/                              Workload and consistency harness, no suite
│   ├── workload.py                   The workload streams (run) and the verdict (evaluate)
│   └── workload.sh                   start, mark, verify, dump and stop, called by the suites
├── mariadb-network-latency/          SC-CHAOS-007: MariaDB network latency
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-net-lat)
│   ├── 01-networkchaos.yaml          NetworkChaos injecting 10s latency on keystone→mariadb
│   └── chainsaw-test.yaml            Test: Ready=True maintained, no crash-loop
├── mariadb-network-partition/        SC-CHAOS-006: MariaDB network partition
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-net-part)
│   ├── 01-networkchaos.yaml          NetworkChaos severing keystone↔mariadb traffic (server-side)
│   └── chainsaw-test.yaml            Test: DeploymentReady=False → recovery
├── mariadb-pod-kill/                 SC-CHAOS-001: MariaDB pod kill
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-db)
│   ├── 01-podchaos.yaml              PodChaos targeting mariadb in openstack
│   └── chainsaw-test.yaml            Test: DatabaseReady=False → recovery
├── memcached-network-partition/      Memcached partition under a Keystone
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-mc-part)
│   ├── 01-networkchaos.yaml          NetworkChaos severing keystone↔Memcached traffic (server-side)
│   └── chainsaw-test.yaml            Test: Ready and every sub-condition True for 120 s
├── memcached-pod-kill/               SC-CHAOS-002: Memcached pod kill
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-mc)
│   ├── 01-podchaos.yaml              PodChaos targeting memcached in openstack
│   └── chainsaw-test.yaml            Test: Ready=True maintained (no regression)
├── neutron-broker-outage/            Message-bus partition under the Neutron API
│   ├── 00-keystone-cr.yaml           Keystone keystone-neutron-broker
│   ├── 01-rabbitmqcluster.yaml       The broker of this suite (neutron-chaos-rabbitmq)
│   ├── 02-ovncentral-cr.yaml         OVNCentral neutron-broker-ovn
│   ├── 03-neutron-cr.yaml            Neutron CR neutron-broker-chaos
│   ├── 04-networkchaos.yaml          NetworkChaos severing neutron↔broker traffic (server-side)
│   └── chainsaw-test.yaml            Test: Ready maintained, no broker connection at any point
├── neutron-keystone-outage/          Keystone outage under the Neutron API
│   ├── 00-keystone-cr.yaml           Keystone keystone-neutron-ks-outage
│   ├── 01-messaging-secret.yaml      Transport URL that never resolves
│   ├── 02-ovncentral-cr.yaml         OVNCentral neutron-ks-outage-ovn
│   ├── 03-neutron-cr.yaml            Neutron neutron-ks-outage
│   ├── 04-podchaos.yaml              PodChaos pod-failure on the Keystone API pods
│   └── chainsaw-test.yaml            Test: an unvalidated token gets 503, then 200 after the outage
├── neutron-mariadb-outage/           Database partition under the Neutron API
│   ├── 00-keystone-cr.yaml           Keystone keystone-neutron-outage
│   ├── 01-messaging-secret.yaml      Transport URL Secret neutron-db-chaos-messaging
│   ├── 02-ovncentral-cr.yaml         OVNCentral neutron-db-chaos-ovn
│   ├── 03-neutron-cr.yaml            Neutron CR neutron-db-chaos
│   ├── 04-networkchaos.yaml          NetworkChaos severing neutron-api↔MariaDB traffic (server-side)
│   └── chainsaw-test.yaml            Test: two conditions maintained, the write fails closed
├── neutron-metadata-agent-pod-kill/  Metadata agent pod kill under a booted server
│   ├── 01-… to 12-*.yaml             The nova-placement-outage stack as nova-meta-chaos, host = node
│   ├── 13-flavor-seed-job.yaml       The flavor m1.nano, and the compute up
│   ├── 14-ovnchassis-cr.yaml         OVNChassis nova-meta-chaos-chassis
│   ├── 15-neutronmetadataagent-cr.yaml NeutronMetadataAgent nova-meta-chaos-agent
│   ├── 16-boot-job.yaml              meta-chaos-vm with a port on meta-chaos-net (BOOT-OK)
│   ├── 17-podchaos.yaml              PodChaos pod-kill of the agent pod
│   ├── 18-probe-scripts.yaml         ConfigMap of the scripts the probe pods run
│   ├── chainsaw-test.yaml            Test: metadata down with the agent, back with the new pod
│   └── probe-lib.sh                  Helpers the scripts source: probe pods, OpenStack cleanup
├── neutron-operator-pod-kill/        neutron-operator pod kill
│   ├── 00-keystone-cr.yaml           Keystone keystone-neutron-chaos-op
│   ├── 01-messaging-secret.yaml      Transport URL that never resolves
│   ├── 02-ovncentral-cr.yaml         OVNCentral neutron-chaos-op-ovn
│   ├── 03-neutron-cr.yaml            Neutron neutron-chaos-op (one API replica)
│   ├── 04-podchaos.yaml              PodChaos targeting neutron-operator in neutron-system
│   ├── 05-patch-scale.yaml           Patch replicas 1→2 after the operator pod kill
│   └── chainsaw-test.yaml            Test: one operator pod replaced, workload UIDs kept, replica patch
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
├── nova-keystone-outage/             Keystone outage under a Nova deployment
│   ├── 01-… to 12-*.yaml             The nova-placement-outage stack as nova-ks-chaos
│   ├── 13-flavor-seed-job.yaml       The flavor m1.nano, and the compute up
│   ├── 14-podchaos.yaml              PodChaos pod-failure on the Keystone API pods
│   └── chainsaw-test.yaml            Test: an unvalidated token gets 503, Nova stays Ready
├── nova-mariadb-outage/              Database partition under the Nova API
│   ├── 00-keystone-cr.yaml           Keystone keystone-nova-db-chaos
│   ├── 01-catalog-setup-job.yaml     The compute catalog row
│   ├── 02-metadata-secret.yaml       The metadata shared secret
│   ├── 03-nova-cr.yaml               Nova CR nova-db-chaos
│   ├── 04-networkchaos.yaml          NetworkChaos severing nova-api↔MariaDB traffic (server-side)
│   ├── 05-list-job.yaml              The server list before and after the fault (LIST-OK)
│   ├── 06-failclosed-job.yaml        GET / serves while the read fails (FAILCLOSED-OK)
│   └── chainsaw-test.yaml            Test: four conditions maintained, the read fails closed
├── nova-operator-pod-kill/           nova-operator pod kill
│   ├── 01-… to 12-*.yaml             The nova-placement-outage stack as nova-chaos-op
│   ├── 13-flavor-seed-job.yaml       The flavor m1.nano, and the compute up
│   ├── 14-podchaos.yaml              PodChaos targeting nova-operator in nova-system
│   ├── 15-patch-scale.yaml           Patch API replicas 1→2 after the operator pod kill
│   └── chainsaw-test.yaml            Test: one operator pod replaced, workload UIDs kept, replica patch
├── nova-placement-outage/            Placement partition under the scheduler
│   ├── 01-keystone-cr.yaml           Keystone keystone-nova-placement-chaos
│   ├── 02-catalog-setup-job.yaml     Compute, placement, image and network catalog rows
│   ├── 03-messaging-secret.yaml      Transport URL for the Neutron beside the Nova
│   ├── 04-ovncentral-cr.yaml         OVNCentral nova-placement-chaos-ovn
│   ├── 05-neutron-cr.yaml            Neutron neutron-nova-placement-chaos
│   ├── 06-placement-cr.yaml          Placement placement-nova-placement-chaos
│   ├── 07-glance-cr.yaml             Glance glance-nova-placement-chaos
│   ├── 08-glancebackend-cr.yaml      S3 backend glance-nova-placement-chaos-s3
│   ├── 09-image-seed-job.yaml        The image the servers boot from
│   ├── 10-metadata-secret.yaml       The metadata shared secret
│   ├── 11-nova-cr.yaml               Nova CR nova-placement-chaos on a shared-broker vhost
│   ├── 12-fake-compute.yaml          nova-compute on the fake driver, host fake-1
│   ├── 13-networkchaos.yaml          NetworkChaos severing scheduler↔Placement traffic (server-side)
│   ├── 14-baseline-job.yaml          A server boots through a reachable Placement (BASELINE-OK)
│   ├── 15-failclosed-job.yaml        The build ends in ERROR, nothing else fails (FAILCLOSED-OK)
│   ├── 16-recovered-job.yaml         The scheduler places servers again (RECOVERED-OK)
│   └── chainsaw-test.yaml            Test: Ready maintained, the build fails closed
├── openbao-pod-kill/                 SC-CHAOS-003: OpenBao pod kill
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-bao)
│   ├── 01-podchaos.yaml              PodChaos targeting openbao in shared-services
│   └── chainsaw-test.yaml            Test: SecretsReady=False → recovery
├── operator-pod-crash/               SC-CHAOS-004: Operator self-recovery
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-op)
│   ├── 01-podchaos.yaml              PodChaos targeting keystone-operator in keystone-system
│   └── chainsaw-test.yaml            Test: Ready=True maintained after operator restart
├── operator-pod-kill/                SC-CHAOS-009: Operator pod kill all + failover
│   ├── 00-keystone-cr.yaml           Keystone CR fixture (keystone-chaos-opk)
│   ├── 01-podchaos.yaml              PodChaos targeting keystone-operator (mode: all)
│   ├── 02-patch-replicas.yaml        Patch replicas 1→2 for post-failover reconciliation
│   └── chainsaw-test.yaml            Test: Leader re-election, conditions maintained, replica patch
├── ovn-operator-pod-kill/            ovn-operator pod kill
│   ├── 00-keystone-cr.yaml           Keystone keystone-ovn-chaos-op
│   ├── 01-messaging-secret.yaml      Transport URL that never resolves
│   ├── 02-ovncentral-cr.yaml         OVNCentral ovn-chaos-op (one northd replica)
│   ├── 03-neutron-cr.yaml            Neutron neutron-ovn-chaos-op
│   ├── 04-podchaos.yaml              PodChaos targeting ovn-operator in ovn-system
│   ├── 05-patch-scale.yaml           Patch northd replicas 1→2 after the operator pod kill
│   └── chainsaw-test.yaml            Test: one operator pod replaced, workload UIDs kept, replica patch
├── ovn-southbound-outage/            Southbound database outage under a chassis
│   ├── 01-ovncentral-cr.yaml         OVNCentral ovn-sb-chaos
│   ├── 02-ovnchassis-cr.yaml         OVNChassis ovn-sb-chaos-chassis
│   ├── 03-podchaos.yaml              PodChaos pod-failure on every Southbound member
│   ├── 04-probe-scripts.yaml         ConfigMap of the scripts the probe Pods run
│   └── chainsaw-test.yaml            Test: ping answers under the fault, chassis recovers
├── placement-keystone-outage/        Keystone outage under the Placement API
│   ├── 00-keystone-cr.yaml           Keystone keystone-placement-ks-outage
│   ├── 01-placement-cr.yaml          Placement placement-ks-outage
│   ├── 02-podchaos.yaml              PodChaos pod-failure on the Keystone API pods
│   └── chainsaw-test.yaml            Test: an unvalidated token gets 503, then 200 after the outage
├── placement-mariadb-outage/         Database partition under the Placement API
│   ├── 00-keystone-cr.yaml           Keystone keystone-placement-db-chaos
│   ├── 01-placement-cr.yaml          Placement placement-db-chaos with database socket timeouts
│   ├── 02-networkchaos.yaml          NetworkChaos severing placement-api↔MariaDB traffic (server-side)
│   └── chainsaw-test.yaml            Test: the write fails closed, the API stays up
└── placement-operator-pod-kill/      placement-operator pod kill
    ├── 00-placement-cr.yaml          Placement CR fixture (placement-chaos-op)
    ├── 01-podchaos.yaml              PodChaos targeting placement-operator in placement-system
    ├── 02-patch-scale.yaml           Patch replicas 1→2 after the operator pod kill
    └── chainsaw-test.yaml            Test: one operator pod replaced, workload UIDs kept, replica patch
```

## Adding New Scenarios

1. Create a new directory: `tests/e2e-chaos/<target>-<fault-type>/`
2. Add `00-keystone-cr.yaml` with a unique CR name and database name
3. Add `01-<chaos-type>.yaml` with the appropriate Chaos Mesh CRD
4. Add `chainsaw-test.yaml` following the degradation/recovery or no-regression pattern
5. Include catch blocks with diagnostic output on every assert step
6. To send OpenStack traffic through the fault, call `../lib/workload.sh` as
   [Adopting the harness](#adopting-the-harness) describes, put the measured
   `# Chaos budget:` line in the header, and add `dump` to the catch blocks
7. All files must include the `SPDX-License-Identifier: Apache-2.0` header

## Related Resources
- [Keystone E2E Test Suites](./keystone-e2e-tests.md): Happy-path E2E tests
- [Keystone Reconciler Architecture](../keystone/keystone-reconciler.md): Sub-reconciler contracts and condition semantics
- [Infrastructure E2E Deployment](../infrastructure/e2e-deployment.md): Infrastructure stack deployment
- `tests/e2e-chaos/chainsaw-config.yaml`: Chaos-specific Chainsaw configuration
- `tests/e2e-chaos/README.md`: Quick-start guide
