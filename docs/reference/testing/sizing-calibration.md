---
title: Sizing Calibration
quadrant: operator
---

# Sizing Calibration

A CobaltCore CR that names no resources gets its CPU and memory from
render-time defaults, and a ControlPlane on the Minimal
[sizing profile](../c5c3/controlplane-crd.md) gets fixed figures for its
service pods and backing services. This page describes how those figures are
measured with a VerticalPodAutoscaler (VPA) recommender in CI, how a script
derives them from the measurement, and which run the figures in the code come
from. The [lab measurement](#lab-measurement) reads the same recommendations
on the metal-stack lab under servers on KVM hypervisors and derives no figure.

## What is measured

A labelled CI run installs the VPA recommender on the kind cluster of three
jobs and creates one VPA per Deployment, StatefulSet and DaemonSet in the
`openstack` namespace. Every VPA has `updateMode: "Off"`, so the recommender
watches the pods and recommends requests without changing any pod. The run
needs neither the VPA updater nor the admission controller, and the
[recommender overlay](../infrastructure/infrastructure-manifests.md#vpa-recommender-kind-only-opt-in)
disables both. It lowers the recommender's per-pod floors from 25 millicores
and 250 MiB to 1 millicore and 1 MiB, so a small container reports its own
use and not its share of the floor.

The recommender accepts a VPA only when its target is the topmost
well-known or scalable controller of its pods. The `MariaDB` CRD serves a
scale subresource without a pod selector, so a VPA on the database
StatefulSet is rejected as `ConfigUnsupported`, and a VPA on the `MariaDB`
finds no pods. `WITH_VPA=true` therefore removes the scale subresource from
the `MariaDB` CRD of the kind cluster, which makes the StatefulSet the
topmost controller. Nothing in the kind stack scales a `MariaDB` through it.

A measured workload must not opt into `verticalAutoscaling`. The operator's
VPA would be a second VPA on the same pods, which upstream leaves undefined,
and a VPA in any other mode than `"Off"` changes the requests the measurement
derives its figures from. None of the three jobs' ControlPlanes sets the
block, and the [built-in profiles](../c5c3/controlplane-crd.md) set it
nowhere.

The three jobs cover the process counts the profiles and the defaults run:

| Job | Workloads | What it contributes |
| --- | --- | --- |
| `e2e-controlplane` | The Minimal ControlPlane of the full-chain suite, its MariaDB, Memcached and dedicated OpenBao, and the shared kind broker | Service rows at one process, the Minimal CPU request, the backing-service figures, the budget projection |
| `e2e-controlplane-sso` | The federated Minimal ControlPlane | The `federation-proxy` sidecar |
| `tempest` (18 legs) | Every Tempest-covered service at its default process count, and at four processes where the fixture raises it, under Tempest API load | Service rows at two and four processes, the render-time CPU request |

The derivation produces these figures:

| Key | Constant |
| --- | --- |
| `memoryBase` | `memoryBase` in `internal/common/types/resources.go` |
| `defaultMemoryPerProcess` | `defaultMemoryPerProcess` in `internal/common/types/resources.go` |
| `glanceMemoryPerProcess` | `glanceMemoryPerProcess` in `operators/glance/internal/controller/reconcile_deployment.go` |
| `defaultCPURequest` | `defaultCPURequest` in `internal/common/types/workload.go` |
| `minimalServiceCPURequest` | `minimalServiceCPURequest` in `operators/c5c3/api/v1alpha1/sizing_profiles.go` |
| `minimalDatabase`, `minimalCache`, `minimalMessaging`, `minimalSecretStore` | the `memoryBound` figures of the Minimal database, cache, messaging and secret store in `sizing_profiles.go` |
| `sidecarCPURequest`, `sidecarMemory` | `sidecarCPURequest` and `sidecarMemory` in `internal/common/types/resources.go` |

Some figures are not measured. Job and CronJob pods live for seconds and the
recommender samples once a minute, so the Job memory stays pinned at `368Mi`
and the Job CPU follows `defaultCPURequest`. No measured run sets threads
above one, so `memoryPerExtraThread` stays `32Mi`. The OVN Raft memory floor
of `256Mi` stays, because the Raft working set grows with the logical port
count, which a CI data set does not represent. The Glance `cache-maintenance`
sidecar runs only in the Glance `e2e-operator` leg, which the label does not
switch on, so the shared sidecar figure only rises. Neither the memory nor the
CPU request of the Neutron metadata agent is taken from this run: none of its
legs makes the agent provision a network, so its rows show an idle agent. Each
figure has a measurement of its own (see
[Metadata agent memory](#metadata-agent-memory) and
[Metadata agent CPU](#metadata-agent-cpu)).
The operators' manager pods, the OVN chassis and NovaCompute DaemonSets and the
platform stack keep their own figures. The chassis, NovaCompute and metadata
agent readings under real servers come from the
[lab measurement](#lab-measurement), which sets them against those figures.

## Running a measurement

The `ci:measure-sizing` label runs the measurement on a pull request. Neither
`ci:full` nor a tag push implies it. [CI Workflow](../ci-cd/ci-workflow.md#sizing-measurement)
lists the steps it adds to each job.

1. Add the label to the pull request. A push to the pull request while the
   run is in flight cancels it.
2. Wait until `e2e-controlplane`, `e2e-controlplane-sso` and the eighteen
   `tempest` legs have finished. Each uploads a `sizing-*` artifact, also when
   a suite fails. A leg whose `Collect the sizing measurement` step failed
   recorded no recommendation; run it again:

   ```bash
   gh run rerun <run-id> --failed
   ```

3. Download the artifacts:

   ```bash
   gh run download <run-id> --pattern 'sizing-*' --dir _output/sizing-run
   ```

4. Copy the `TOTAL` row of the
   [node budget table (Link 6z)](controlplane-e2e-tests.md#node-budget-link-6z)
   from the log of the same run's `e2e-controlplane` job:

   ```bash
   gh run view <run-id> --job <job-id> --log | grep -E '[[:space:]]TOTAL[[:space:]]'
   ```

5. Derive the figures, with the TOTAL row's CPU in millicores and memory in
   MiB:

   ```bash
   python3 hack/derive-sizing-figures.py --budget-total <cpu>m,<memory>Mi _output/sizing-run
   ```

The script prints one `key=value` line per figure, the projected node budget,
and a Markdown section with every row it used. It exits 1 when the input is
incomplete or the Minimal figures miss the node budget, and 2 on a usage or
parse error.

Each artifact holds the snapshots of `hack/ci-vpa-recommendations.sh`, its
`watch.log`, and the report it wrote. `recommendations.tsv` opens with a
comment line naming the run, the attempt, the commit, the job, the Tempest leg
and the recommender image, and has one row per workload container:

| Column | Content |
| --- | --- |
| `namespace`, `kind`, `workload`, `replicas` | The workload; `replicas` is `-` for a DaemonSet |
| `owner_kind`, `owner_name` | The workload's controller owner, such as `Keystone` or `MariaDB` |
| `app`, `component` | The pod template's `app.kubernetes.io/name` and `app.kubernetes.io/component` labels |
| `container` | The container the recommendation is for |
| `processes`, `threads` | The values after `--processes` and `--threads` (or `--n-threads=`) in the container's command and args, `-` when absent |
| `cpu_target_m`, `memory_target_mi` | The recommender's target, in millicores and MiB, rounded up |
| `cpu_upper_m`, `memory_upper_mi` | The recommender's upper bound |
| `cpu_request_m`, `memory_request_mi`, `memory_limit_mi` | What the container requested when the snapshot was taken, `-` when unset |
| `snapshot` | The time of the snapshot |

## Derivation rules

`hack/derive-sizing-figures.py` applies these rules in order. Rounding up to a
step `s` means rounding up to the next multiple of `s`, and a job is the
`job=` value of a file's comment line.

1. Classification. A row is a formula row when its owner is a `Keystone`,
   `Horizon`, `Placement`, `Barbican`, `Neutron`, `Cinder`, `Nova` or
   `NeutronMetadataAgent`, or an `OVNCentral` with the component `northd` or
   `sb-relay`. A row owned by a `Glance` is a Glance row. The containers
   `federation-proxy` and `cache-maintenance` are sidecar rows, whatever their
   owner. A row owned by a `MariaDB`, `Memcached`, `RabbitmqCluster` or
   `OpenBaoCluster` is a backing row when it comes from `e2e-controlplane`;
   the Tempest legs' database and broker are ignored. Every other row is
   printed as `ignored` and used for nothing, except that rule 5 reads the
   OVN `nb` and `sb` rows. Two kinds of formula row have a fixed memory
   figure. A `Cinder` row with the component `backup` gets `backupMemory`
   (`2Gi`), because a backup's footprint follows its chunk size. A
   `NeutronMetadataAgent` row with the component `metadata-agent` gets
   `metadataAgentMemory`, because the agent's footprint follows the networks
   on its node (see [Metadata agent memory](#metadata-agent-memory)). The CPU
   of such a row counts in rules 5 and 6 like any formula row's; its memory
   counts in neither rule 3 nor rule 9.
2. Process and thread counts. A formula or Glance row takes its process count
   `p` and thread count `t` from its `processes` and `threads` columns. The
   Nova scheduler and conductor and an eventlet Glance take their worker
   count from configuration instead: one process in the two ControlPlane jobs,
   where Minimal sets one worker, and two in `tempest`, the operators' default.
   Every other row counts one process of one thread. Its memory target is
   normalized to `T = target − p × (t − 1) × 32Mi`.
3. Shared memory formula. For every process count, `M(p)` is the largest `T`
   of the formula rows without a fixed memory figure. `defaultMemoryPerProcess` is the steepest slope
   between two measured process counts, at least `32Mi`, rounded up to
   `16Mi`. `memoryBase` is the largest `M(p) − p × defaultMemoryPerProcess`,
   at least 0, rounded up to `16Mi`. Together they give
   `memoryBase + p × defaultMemoryPerProcess ≥ M(p)` at every measured
   process count. Rows at fewer than two process counts stop the derivation.
4. Glance per-process figure. `glanceMemoryPerProcess` is the largest
   `(T − memoryBase) / p` of the Glance rows, at least `32Mi`, rounded up to
   `16Mi`.
5. Render-time CPU request. From the formula and Glance rows of the `tempest`
   legs and their OVN `nb` and `sb` rows, the largest CPU target per
   `owner_kind/component/container` key counts once. `defaultCPURequest` is
   the median of those values, the upper of the two middle values for an
   even count, rounded up to `10m` and at least `10m`. That is what a VPA
   would request for a service under API load, and it is also the CPU half of
   the OVN Raft request floor.
6. Minimal service CPU. The same median over the formula and Glance rows of
   the two ControlPlane jobs, without the `NeutronMetadataAgent` and
   `OVNCentral` rows the ControlPlane does not size, rounded up to `5m` and
   at least `10m`, is `minimalServiceCPURequest`.
7. Minimal backing services. Per workload of a backing kind the main
   container is the one with the largest memory target; a sidecar beside it
   keeps its own figures. The CPU figure is the largest CPU target of the main
   containers, rounded up to `5m` and at least `10m`. The memory figure is
   their largest memory target rounded up to `16Mi`, and never below a figure
   the service's own configuration depends on:

   | Backing service | Memory floor | Why |
   | --- | --- | --- |
   | Database (`MariaDB`) | `1Gi` | Its working set grows with the connection count, which one CI run does not drive to the ceiling |
   | Cache (`Memcached`) | `96Mi` | The operator's floor of `maxMemoryMB` 64 plus `32Mi` |
   | Messaging (`RabbitmqCluster`) | `1Gi` | The Cluster Operator alarms the broker at 0.6 of the limit minus a fifth, so its use reflects the limit it ran under. Its idle footprint is about 236 MiB: `512Mi` (alarm at 245.8 MiB) alarmed without a backlog ([#1298](https://github.com/C5C3/cobaltcore/issues/1298)), and `1Gi` (491.5 MiB) keeps the footprint below half the alarm threshold. In the full chain this is the shared kind broker |
   | Secret store (`OpenBaoCluster`) | `64Mi` | |

8. Sidecar. `sidecarCPURequest` is the largest `federation-proxy` CPU target
   rounded up to `5m` and at least `25m`; `sidecarMemory` is its largest
   memory target rounded up to `16Mi` and at least `256Mi`. Neither figure
   falls, because the Glance cache sidecar shares them and no measured run
   carries it.
9. Budget projection. Starting from the Link 6z total, the script adds the
   change in requests, times the pod count, of the `e2e-controlplane`
   containers the Minimal figures reach: the service rows of rule 6 take
   `minimalServiceCPURequest`, and their memory follows the refitted formula
   when the formula rendered it (the limit equals the request and the memory
   is not fixed). The main
   containers of the database, cache and secret store take their rule 7
   figures. Every other container keeps its request, including the shared
   kind broker and the compute leg Link 6z leaves out. While the CPU exceeds
   4000m the script lowers `minimalServiceCPURequest` by `5m`; at `10m` still
   over, it exits 1. Memory above 16384Mi exits 1 as well, since a memory
   request equals its limit and never goes below a measured target. Either
   miss means the 4 vCPU / 16 GiB target is not reachable with measured
   figures, and the figures stay as they are until that is decided.

## Recorded run

The figures in the code come from this run:

| Item | Value |
| --- | --- |
| Run | [36339033005](https://github.com/C5C3/cobaltcore/actions/runs/36339033005), attempt 1, on 2026-09-27 |
| Measured commit | `4ebf79968dc3b9dbec072199f72ee6858f4a864e`, the merge of pull request #1123 with `main` |
| Recommender | `registry.k8s.io/autoscaling/vpa-recommender:1.8.0` |
| Link 6z total | `3480m` CPU and `14754Mi` of memory |
| Derivation | `python3 hack/derive-sizing-figures.py --budget-total 3480m,14754Mi _output/sizing-run` |
| Projection | `projected: 2885m 15474Mi of 4000m 16384Mi` |

The two keystone Tempest legs of the run failed on three RBAC tests
([#1127](https://github.com/C5C3/cobaltcore/issues/1127)); the suite ran to its
end, so their recommendations are complete. An earlier run of the same pull
request, 36307184647, recorded no `MariaDB` row, because the MariaDB CRD still
served its scale subresource, and is not used.

## Derivation output

The derivation printed this Markdown for the recorded run. It predates the
`1Gi` broker floor ([#1298](https://github.com/C5C3/cobaltcore/issues/1298)):
on the same inputs the derivation now prints `815m,1Gi` for `minimalMessaging`
and `815m, 1Gi` for the `RabbitmqCluster` row.

### Derived figures

| Constant | Value |
| --- | --- |
| `memoryBase` | 16Mi |
| `defaultMemoryPerProcess` | 352Mi |
| `glanceMemoryPerProcess` | 1Gi |
| `defaultCPURequest` | 70m |
| `minimalServiceCPURequest` | 15m |
| `minimalDatabase` | 65m,1Gi |
| `minimalCache` | 15m,96Mi |
| `minimalMessaging` | 815m,512Mi |
| `minimalSecretStore` | 35m,64Mi |
| `sidecarCPURequest` | 25m |
| `sidecarMemory` | 256Mi |

Input: 196 rows from 14 job runs. T is the memory target less 32Mi per extra thread of each process.

### Memory formula

| p | M(p), the largest T (MiB) | 16Mi + p × 352Mi |
| --- | --- | --- |
| 1 | 363 | 368 |
| 2 | 363 | 720 |
| 4 | 1052 | 1424 |

The steepest slope between two process counts is 344.5Mi, which rounds up to 352Mi.

These containers have a fixed memory figure and stay out of the fit: `Cinder/backup/backup`, `NeutronMetadataAgent/metadata-agent/metadata-agent`.

### Glance

| Job | Leg | Workload | p | t | T (MiB) | (T − memoryBase) / p |
| --- | --- | --- | --- | --- | --- | --- |
| e2e-controlplane | - | controlplane-keystone-glance | 1 | 1 | 284 | 268 |
| tempest | cinder-2025.2 | glance-cinder-tempest-2025-2 | 2 | 1 | 1484 | 734 |
| tempest | cinder-2026.1 | glance-cinder-tempest-2026-1 | 2 | 1 | 2063 | 1023.5 |
| tempest | glance-2025.2 | glance-tempest-2025-2 | 2 | 1 | 156 | 70 |
| tempest | nova-2025.2 | glance-nova-tempest-2025-2 | 2 | 1 | 423 | 203.5 |
| tempest | nova-2026.1 | glance-nova-tempest-2026-1 | 2 | 1 | 363 | 173.5 |

### Render-time CPU request

| Key (owner_kind/component/container) | Largest CPU target (m) |
| --- | --- |
| `Barbican/api/barbican-api` | 11 |
| `Cinder/api/cinder-api` | 182 |
| `Cinder/backup/backup` | 1101 |
| `Cinder/scheduler/scheduler` | 78 |
| `Cinder/volume-nfs1/volume-nfs1` | 296 |
| `Glance/api/glance-api` | 627 |
| `Keystone/api/keystone` | 1938 |
| `Keystone/api/trust-flush` | 0 |
| `Neutron/api/neutron-api` | 1836 |
| `Neutron/ovn-maintenance-worker/ovn-maintenance-worker` | 35 |
| `Neutron/periodic-workers/periodic-workers` | 23 |
| `NeutronMetadataAgent/metadata-agent/metadata-agent` | 23 |
| `Nova/api/nova-api` | 247 |
| `Nova/conductor/conductor` | 203 |
| `Nova/metadata/nova-metadata` | 11 |
| `Nova/novncproxy/novncproxy` | 11 |
| `Nova/scheduler/scheduler` | 63 |
| `OVNCentral/nb/ovsdb` | 49 |
| `OVNCentral/northd/northd` | 35 |
| `OVNCentral/sb/ovsdb` | 63 |
| `Placement/api/placement-api` | 49 |

Median of 21 keys: 63m, so `defaultCPURequest` is 70m.

### Minimal service CPU request

| Key (owner_kind/component/container) | Largest CPU target (m) |
| --- | --- |
| `Barbican/api/barbican-api` | 11 |
| `Cinder/api/cinder-api` | 11 |
| `Cinder/backup/backup` | 716 |
| `Cinder/scheduler/scheduler` | 35 |
| `Cinder/volume-nfs1/volume-nfs1` | 49 |
| `Glance/api/glance-api` | 23 |
| `Horizon/api/horizon` | 271 |
| `Keystone/api/keystone` | 296 |
| `Neutron/api/neutron-api` | 11 |
| `Neutron/ovn-maintenance-worker/ovn-maintenance-worker` | 11 |
| `Neutron/periodic-workers/periodic-workers` | 11 |
| `Nova/api/nova-api` | 11 |
| `Nova/conductor/conductor` | 864 |
| `Nova/metadata/nova-metadata` | 11 |
| `Nova/novncproxy/novncproxy` | 11 |
| `Nova/scheduler/scheduler` | 35 |
| `Placement/api/placement-api` | 11 |

Median of 17 keys: 11m, which rounds up to 15m.

### Minimal backing services

| Kind | Workload | Container | CPU target (m) | Memory target (MiB) | Figure |
| --- | --- | --- | --- | --- | --- |
| MariaDB | openstack-db | mariadb | 63 | 489 | 65m, 1Gi |
| Memcached | openstack-memcached | memcached | 11 | 23 | 15m, 96Mi |
| RabbitmqCluster | shared-rabbitmq-server | rabbitmq | 813 | 175 | 815m, 512Mi |
| OpenBaoCluster | controlplane-keystone-barbican-bao | openbao | 35 | 61 | 35m, 64Mi |
| OpenBaoCluster | openbao-instance | openbao | 35 | 48 | 35m, 64Mi |

### Sidecar

| Job | Leg | Workload | CPU target (m) | Memory target (MiB) |
| --- | --- | --- | --- | --- |
| e2e-controlplane-sso | - | controlplane-sso-keystone | 11 | 75 |

### Budget projection

The Link 6z total of the measured e2e-controlplane node was 3480m CPU and 14754Mi of memory. These e2e-controlplane containers take the Minimal figures:

| Workload | Container | Pods | CPU request (m) | Memory request (MiB) |
| --- | --- | --- | --- | --- |
| controlplane-keystone-barbican | barbican-api | 1 | 50 → 15 | 368 → 368 |
| controlplane-keystone-barbican-bao | openbao | 1 | - → 35 | - → 64 |
| controlplane-keystone-cinder | cinder-api | 1 | 50 → 15 | 368 → 368 |
| controlplane-keystone-cinder-backup | backup | 1 | 50 → 15 | 2048 → 2048 |
| controlplane-keystone-cinder-scheduler | scheduler | 1 | 50 → 15 | 368 → 368 |
| controlplane-keystone-cinder-volume-nfs1 | volume-nfs1 | 1 | 50 → 15 | 368 → 368 |
| controlplane-keystone-glance | glance-api | 1 | 50 → 15 | 624 → 1040 |
| controlplane-keystone-horizon | horizon | 1 | 50 → 15 | 512 → 720 |
| controlplane-keystone-keystone | keystone | 1 | 50 → 15 | 368 → 368 |
| controlplane-keystone-neutron | neutron-api | 1 | 50 → 15 | 368 → 368 |
| controlplane-keystone-neutron-ovn-maintenance-worker | ovn-maintenance-worker | 1 | 50 → 15 | 368 → 368 |
| controlplane-keystone-neutron-periodic-workers | periodic-workers | 1 | 50 → 15 | 368 → 368 |
| controlplane-keystone-nova | nova-api | 1 | 50 → 15 | 368 → 368 |
| controlplane-keystone-nova-conductor | conductor | 1 | 50 → 15 | 368 → 368 |
| controlplane-keystone-nova-metadata | nova-metadata | 1 | 50 → 15 | 368 → 368 |
| controlplane-keystone-nova-novncproxy | novncproxy | 1 | 50 → 15 | 368 → 368 |
| controlplane-keystone-nova-scheduler | scheduler | 1 | 50 → 15 | 368 → 368 |
| controlplane-keystone-placement | placement-api | 1 | 50 → 15 | 368 → 368 |
| openbao-instance | openbao | 1 | - → 35 | - → 64 |
| openstack-db | mariadb | 1 | 100 → 65 | 1024 → 1024 |
| openstack-memcached | memcached | 1 | 50 → 15 | 128 → 96 |

| minimalServiceCPURequest | Projected CPU | Projected memory |
| --- | --- | --- |
| 15m | 2885m | 15474Mi |

The node budget is 4000m CPU and 16384Mi of memory.
The shared kind broker's memory request rose from 512Mi to 1Gi after this
measurement ([#1298](https://github.com/C5C3/cobaltcore/issues/1298)), so the
measured total and the projection are 512Mi low until the next measurement.

### Input rows

| job | leg | namespace | owner_kind | component | workload | container | class | p | t | cpu_target_m | memory_target_mi | T |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| e2e-controlplane | - | openstack | - | - | controlplane-keystone-fake-compute | nova-compute | ignored | - | - | 11 | 175 | - |
| e2e-controlplane | - | openstack | - | - | nfs-server | nfs-server | ignored | - | - | 11 | 75 | - |
| e2e-controlplane | - | openstack | Barbican | api | controlplane-keystone-barbican | barbican-api | formula | 1 | 1 | 11 | 138 | 138 |
| e2e-controlplane | - | openstack | Cinder | api | controlplane-keystone-cinder | cinder-api | formula | 1 | 1 | 11 | 175 | 175 |
| e2e-controlplane | - | openstack | Cinder | backup | controlplane-keystone-cinder-backup | backup | formula, fixed memory | 1 | 1 | 716 | 260 | 260 |
| e2e-controlplane | - | openstack | Cinder | scheduler | controlplane-keystone-cinder-scheduler | scheduler | formula | 1 | 1 | 35 | 156 | 156 |
| e2e-controlplane | - | openstack | Cinder | volume-nfs1 | controlplane-keystone-cinder-volume-nfs1 | volume-nfs1 | formula | 1 | 1 | 49 | 309 | 309 |
| e2e-controlplane | - | openstack | Glance | api | controlplane-keystone-glance | glance-api | glance | 1 | 1 | 23 | 284 | 284 |
| e2e-controlplane | - | openstack | Horizon | api | controlplane-keystone-horizon | horizon | formula | 2 | 1 | 11 | 260 | 260 |
| e2e-controlplane | - | openstack | Keystone | api | controlplane-keystone-keystone | keystone | formula | 1 | 1 | 126 | 156 | 156 |
| e2e-controlplane | - | openstack | MariaDB | - | openstack-db | mariadb | backing | - | - | 63 | 489 | - |
| e2e-controlplane | - | openstack | Memcached | - | openstack-memcached | memcached | backing | - | - | 11 | 23 | - |
| e2e-controlplane | - | openstack | Neutron | api | controlplane-keystone-neutron | neutron-api | formula | 1 | 1 | 11 | 237 | 237 |
| e2e-controlplane | - | openstack | Neutron | ovn-maintenance-worker | controlplane-keystone-neutron-ovn-maintenance-worker | ovn-maintenance-worker | formula | 1 | 1 | 11 | 309 | 309 |
| e2e-controlplane | - | openstack | Neutron | periodic-workers | controlplane-keystone-neutron-periodic-workers | periodic-workers | formula | 1 | 1 | 11 | 335 | 335 |
| e2e-controlplane | - | openstack | NeutronMetadataAgent | metadata-agent | controlplane-keystone-agent-metadata-agent | metadata-agent | formula, fixed memory | 1 | 1 | 11 | 175 | 175 |
| e2e-controlplane | - | openstack | Nova | api | controlplane-keystone-nova | nova-api | formula | 1 | 1 | 11 | 215 | 215 |
| e2e-controlplane | - | openstack | Nova | conductor | controlplane-keystone-nova-conductor | conductor | formula | 1 | 1 | 864 | 284 | 284 |
| e2e-controlplane | - | openstack | Nova | metadata | controlplane-keystone-nova-metadata | nova-metadata | formula | 1 | 1 | 11 | 156 | 156 |
| e2e-controlplane | - | openstack | Nova | novncproxy | controlplane-keystone-nova-novncproxy | novncproxy | formula | 1 | 1 | 11 | 138 | 138 |
| e2e-controlplane | - | openstack | Nova | scheduler | controlplane-keystone-nova-scheduler | scheduler | formula | 1 | 1 | 35 | 156 | 156 |
| e2e-controlplane | - | openstack | OVNCentral | nb | controlplane-keystone-ovn-nb | ovsdb | ignored | - | - | 23 | 11 | - |
| e2e-controlplane | - | openstack | OVNCentral | northd | controlplane-keystone-ovn-northd | northd | formula | 1 | 1 | 23 | 11 | 11 |
| e2e-controlplane | - | openstack | OVNCentral | sb | controlplane-keystone-ovn-sb | ovsdb | ignored | - | - | 23 | 23 | - |
| e2e-controlplane | - | openstack | OVNChassis | ovn-controller | controlplane-keystone-chassis-ovn-controller | ovn-controller | ignored | - | - | 23 | 11 | - |
| e2e-controlplane | - | openstack | OVNChassis | ovs | controlplane-keystone-chassis-ovs | ovs-vswitchd | ignored | - | - | 23 | 11 | - |
| e2e-controlplane | - | openstack | OVNChassis | ovs | controlplane-keystone-chassis-ovs | ovsdb-server | ignored | - | - | 23 | 11 | - |
| e2e-controlplane | - | openstack | OpenBaoCluster | - | controlplane-keystone-barbican-bao | openbao | backing | - | - | 35 | 61 | - |
| e2e-controlplane | - | openstack | OpenBaoCluster | - | openbao-instance | openbao | backing | - | - | 35 | 48 | - |
| e2e-controlplane | - | openstack | Placement | api | controlplane-keystone-placement | placement-api | formula | 1 | 1 | 11 | 105 | 105 |
| e2e-controlplane | - | openstack | RabbitmqCluster | rabbitmq | shared-rabbitmq-server | rabbitmq | backing | - | - | 813 | 175 | - |
| e2e-controlplane-sso | - | openstack | - | - | keycloak | keycloak | ignored | - | - | 1737 | 684 | - |
| e2e-controlplane-sso | - | openstack | - | - | openldap | openldap | ignored | - | - | 11 | 777 | - |
| e2e-controlplane-sso | - | openstack | Horizon | api | controlplane-sso-horizon | horizon | formula | 2 | 1 | 271 | 260 | 260 |
| e2e-controlplane-sso | - | openstack | Keystone | api | controlplane-sso-keystone | federation-proxy | sidecar | - | - | 11 | 75 | - |
| e2e-controlplane-sso | - | openstack | Keystone | api | controlplane-sso-keystone | keystone | formula | 1 | 1 | 296 | 175 | 175 |
| e2e-controlplane-sso | - | openstack | MariaDB | - | openstack-db | mariadb | ignored | - | - | 78 | 215 | - |
| e2e-controlplane-sso | - | openstack | Memcached | - | openstack-memcached | memcached | ignored | - | - | 11 | 23 | - |
| e2e-controlplane-sso | - | openstack | OpenBaoCluster | - | openbao-instance | openbao | ignored | - | - | 35 | 48 | - |
| tempest | barbican-2025.2 | openstack | Barbican | api | barbican-tempest-2025-2 | barbican-api | formula | 4 | 1 | 11 | 489 | 489 |
| tempest | barbican-2025.2 | openstack | Keystone | api | keystone-barbican-tempest-2025-2 | keystone | formula | 2 | 1 | 126 | 284 | 284 |
| tempest | barbican-2025.2 | openstack | MariaDB | - | openstack-db | mariadb | ignored | - | - | 49 | 237 | - |
| tempest | barbican-2025.2 | openstack | MariaDB | - | openstack-db-metrics | exporter | ignored | - | - | 11 | 11 | - |
| tempest | barbican-2025.2 | openstack | Memcached | - | openstack-memcached | memcached | ignored | - | - | 11 | 23 | - |
| tempest | barbican-2025.2 | openstack | OpenBaoCluster | - | openbao-instance | openbao | ignored | - | - | 35 | 48 | - |
| tempest | barbican-2026.1 | openstack | Keystone | api | keystone-barbican-tempest-2026-1 | keystone | formula | 2 | 1 | 93 | 284 | 284 |
| tempest | barbican-2026.1 | openstack | MariaDB | - | openstack-db | mariadb | ignored | - | - | 63 | 237 | - |
| tempest | barbican-2026.1 | openstack | MariaDB | - | openstack-db-metrics | exporter | ignored | - | - | 11 | 11 | - |
| tempest | barbican-2026.1 | openstack | Memcached | - | openstack-memcached | memcached | ignored | - | - | 11 | 23 | - |
| tempest | barbican-2026.1 | openstack | OpenBaoCluster | - | openbao-instance | openbao | ignored | - | - | 35 | 48 | - |
| tempest | cinder-2025.2 | openstack | - | - | nfs-server | nfs-server | ignored | - | - | 11 | 75 | - |
| tempest | cinder-2025.2 | openstack | - | - | nova-cinder-tempest-2025-2-fake-compute | nova-compute | ignored | - | - | 23 | 175 | - |
| tempest | cinder-2025.2 | openstack | Cinder | api | cinder-tempest-2025-2 | cinder-api | formula | 4 | 1 | 182 | 684 | 684 |
| tempest | cinder-2025.2 | openstack | Cinder | backup | cinder-tempest-2025-2-backup | backup | formula, fixed memory | 1 | 1 | 977 | 335 | 335 |
| tempest | cinder-2025.2 | openstack | Cinder | scheduler | cinder-tempest-2025-2-scheduler | scheduler | formula | 1 | 1 | 78 | 156 | 156 |
| tempest | cinder-2025.2 | openstack | Cinder | volume-nfs1 | cinder-tempest-2025-2-volume-nfs1 | volume-nfs1 | formula | 1 | 1 | 182 | 363 | 363 |
| tempest | cinder-2025.2 | openstack | Glance | api | glance-cinder-tempest-2025-2 | glance-api | glance | 2 | 1 | 143 | 1484 | 1484 |
| tempest | cinder-2025.2 | openstack | Keystone | api | keystone-cinder-tempest-2025-2 | keystone | formula | 2 | 1 | 163 | 284 | 284 |
| tempest | cinder-2025.2 | openstack | MariaDB | - | openstack-db | mariadb | ignored | - | - | 182 | 600 | - |
| tempest | cinder-2025.2 | openstack | MariaDB | - | openstack-db-metrics | exporter | ignored | - | - | 11 | 11 | - |
| tempest | cinder-2025.2 | openstack | Memcached | - | openstack-memcached | memcached | ignored | - | - | 11 | 23 | - |
| tempest | cinder-2025.2 | openstack | Neutron | api | neutron-cinder-tempest-2025-2 | neutron-api | formula | 4 | 1 | 379 | 826 | 826 |
| tempest | cinder-2025.2 | openstack | Neutron | ovn-maintenance-worker | neutron-cinder-tempest-2025-2-ovn-maintenance-worker | ovn-maintenance-worker | formula | 1 | 1 | 11 | 309 | 309 |
| tempest | cinder-2025.2 | openstack | Neutron | periodic-workers | neutron-cinder-tempest-2025-2-periodic-workers | periodic-workers | formula | 1 | 1 | 11 | 335 | 335 |
| tempest | cinder-2025.2 | openstack | Nova | api | nova-cinder-tempest-2025-2 | nova-api | formula | 4 | 1 | 126 | 729 | 729 |
| tempest | cinder-2025.2 | openstack | Nova | conductor | nova-cinder-tempest-2025-2-conductor | conductor | formula | 2 | 1 | 182 | 284 | 284 |
| tempest | cinder-2025.2 | openstack | Nova | metadata | nova-cinder-tempest-2025-2-metadata | nova-metadata | formula | 2 | 1 | 11 | 309 | 309 |
| tempest | cinder-2025.2 | openstack | Nova | novncproxy | nova-cinder-tempest-2025-2-novncproxy | novncproxy | formula | 1 | 1 | 11 | 138 | 138 |
| tempest | cinder-2025.2 | openstack | Nova | scheduler | nova-cinder-tempest-2025-2-scheduler | scheduler | formula | 2 | 1 | 49 | 237 | 237 |
| tempest | cinder-2025.2 | openstack | OVNCentral | nb | ovn-cinder-tempest-2025-2-nb | ovsdb | ignored | - | - | 23 | 11 | - |
| tempest | cinder-2025.2 | openstack | OVNCentral | northd | ovn-cinder-tempest-2025-2-northd | northd | formula | 1 | 1 | 23 | 11 | 11 |
| tempest | cinder-2025.2 | openstack | OVNCentral | sb | ovn-cinder-tempest-2025-2-sb | ovsdb | ignored | - | - | 23 | 11 | - |
| tempest | cinder-2025.2 | openstack | OpenBaoCluster | - | openbao-instance | openbao | ignored | - | - | 35 | 48 | - |
| tempest | cinder-2025.2 | openstack | Placement | api | placement-cinder-tempest-2025-2 | placement-api | formula | 2 | 1 | 23 | 195 | 195 |
| tempest | cinder-2025.2 | openstack | RabbitmqCluster | rabbitmq | shared-rabbitmq-server | rabbitmq | ignored | - | - | 23 | 175 | - |
| tempest | cinder-2026.1 | openstack | - | - | nfs-server | nfs-server | ignored | - | - | 11 | 75 | - |
| tempest | cinder-2026.1 | openstack | - | - | nova-cinder-tempest-2026-1-fake-compute | nova-compute | ignored | - | - | 23 | 175 | - |
| tempest | cinder-2026.1 | openstack | Cinder | api | cinder-tempest-2026-1 | cinder-api | formula | 4 | 1 | 182 | 641 | 641 |
| tempest | cinder-2026.1 | openstack | Cinder | backup | cinder-tempest-2026-1-backup | backup | formula, fixed memory | 1 | 1 | 1101 | 309 | 309 |
| tempest | cinder-2026.1 | openstack | Cinder | scheduler | cinder-tempest-2026-1-scheduler | scheduler | formula | 1 | 1 | 63 | 156 | 156 |
| tempest | cinder-2026.1 | openstack | Cinder | volume-nfs1 | cinder-tempest-2026-1-volume-nfs1 | volume-nfs1 | formula | 1 | 1 | 296 | 335 | 335 |
| tempest | cinder-2026.1 | openstack | Glance | api | glance-cinder-tempest-2026-1 | glance-api | glance | 2 | 1 | 627 | 2063 | 2063 |
| tempest | cinder-2026.1 | openstack | Keystone | api | keystone-cinder-tempest-2026-1 | keystone | formula | 2 | 1 | 271 | 284 | 284 |
| tempest | cinder-2026.1 | openstack | Keystone | api | keystone-cinder-tempest-2026-1 | trust-flush | formula | 1 | 1 | 0 | 1 | 1 |
| tempest | cinder-2026.1 | openstack | MariaDB | - | openstack-db | mariadb | ignored | - | - | 182 | 600 | - |
| tempest | cinder-2026.1 | openstack | MariaDB | - | openstack-db-metrics | exporter | ignored | - | - | 11 | 11 | - |
| tempest | cinder-2026.1 | openstack | Memcached | - | openstack-memcached | memcached | ignored | - | - | 11 | 23 | - |
| tempest | cinder-2026.1 | openstack | Neutron | api | neutron-cinder-tempest-2026-1 | neutron-api | formula | 4 | 1 | 296 | 777 | 777 |
| tempest | cinder-2026.1 | openstack | Neutron | ovn-maintenance-worker | neutron-cinder-tempest-2026-1-ovn-maintenance-worker | ovn-maintenance-worker | formula | 1 | 1 | 11 | 309 | 309 |
| tempest | cinder-2026.1 | openstack | Neutron | periodic-workers | neutron-cinder-tempest-2026-1-periodic-workers | periodic-workers | formula | 1 | 1 | 23 | 260 | 260 |
| tempest | cinder-2026.1 | openstack | Nova | api | nova-cinder-tempest-2026-1 | nova-api | formula | 4 | 1 | 49 | 684 | 684 |
| tempest | cinder-2026.1 | openstack | Nova | conductor | nova-cinder-tempest-2026-1-conductor | conductor | formula | 2 | 1 | 93 | 284 | 284 |
| tempest | cinder-2026.1 | openstack | Nova | metadata | nova-cinder-tempest-2026-1-metadata | nova-metadata | formula | 2 | 1 | 11 | 309 | 309 |
| tempest | cinder-2026.1 | openstack | Nova | novncproxy | nova-cinder-tempest-2026-1-novncproxy | novncproxy | formula | 1 | 1 | 11 | 138 | 138 |
| tempest | cinder-2026.1 | openstack | Nova | scheduler | nova-cinder-tempest-2026-1-scheduler | scheduler | formula | 2 | 1 | 35 | 260 | 260 |
| tempest | cinder-2026.1 | openstack | OVNCentral | nb | ovn-cinder-tempest-2026-1-nb | ovsdb | ignored | - | - | 23 | 23 | - |
| tempest | cinder-2026.1 | openstack | OVNCentral | northd | ovn-cinder-tempest-2026-1-northd | northd | formula | 1 | 1 | 23 | 11 | 11 |
| tempest | cinder-2026.1 | openstack | OVNCentral | sb | ovn-cinder-tempest-2026-1-sb | ovsdb | ignored | - | - | 23 | 11 | - |
| tempest | cinder-2026.1 | openstack | OpenBaoCluster | - | openbao-instance | openbao | ignored | - | - | 35 | 61 | - |
| tempest | cinder-2026.1 | openstack | Placement | api | placement-cinder-tempest-2026-1 | placement-api | formula | 2 | 1 | 23 | 195 | 195 |
| tempest | cinder-2026.1 | openstack | RabbitmqCluster | rabbitmq | shared-rabbitmq-server | rabbitmq | ignored | - | - | 23 | 195 | - |
| tempest | glance-2025.2 | openstack | Glance | api | glance-tempest-2025-2 | glance-api | glance | 2 | 1 | 23 | 156 | 156 |
| tempest | glance-2025.2 | openstack | Keystone | api | keystone-glance-tempest-2025-2 | keystone | formula | 2 | 1 | 143 | 284 | 284 |
| tempest | glance-2025.2 | openstack | MariaDB | - | openstack-db | mariadb | ignored | - | - | 63 | 215 | - |
| tempest | glance-2025.2 | openstack | MariaDB | - | openstack-db-metrics | exporter | ignored | - | - | 11 | 11 | - |
| tempest | glance-2025.2 | openstack | Memcached | - | openstack-memcached | memcached | ignored | - | - | 11 | 23 | - |
| tempest | glance-2025.2 | openstack | OpenBaoCluster | - | openbao-instance | openbao | ignored | - | - | 35 | 48 | - |
| tempest | glance-2026.1 | openstack | Keystone | api | keystone-glance-tempest-2026-1 | keystone | formula | 2 | 1 | 126 | 284 | 284 |
| tempest | glance-2026.1 | openstack | MariaDB | - | openstack-db | mariadb | ignored | - | - | 93 | 309 | - |
| tempest | glance-2026.1 | openstack | MariaDB | - | openstack-db-metrics | exporter | ignored | - | - | 11 | 11 | - |
| tempest | glance-2026.1 | openstack | Memcached | - | openstack-memcached | memcached | ignored | - | - | 11 | 23 | - |
| tempest | glance-2026.1 | openstack | OpenBaoCluster | - | openbao-instance | openbao | ignored | - | - | 49 | 48 | - |
| tempest | keystone-2025.2 | openstack | Keystone | api | keystone-tempest-2025-2 | keystone | formula | 2 | 1 | 1938 | 335 | 335 |
| tempest | keystone-2025.2 | openstack | Keystone | api | keystone-tempest-2025-2 | trust-flush | formula | 1 | 1 | 0 | 1 | 1 |
| tempest | keystone-2025.2 | openstack | MariaDB | - | openstack-db | mariadb | ignored | - | - | 442 | 215 | - |
| tempest | keystone-2025.2 | openstack | MariaDB | - | openstack-db-metrics | exporter | ignored | - | - | 11 | 11 | - |
| tempest | keystone-2025.2 | openstack | Memcached | - | openstack-memcached | memcached | ignored | - | - | 49 | 61 | - |
| tempest | keystone-2025.2 | openstack | OpenBaoCluster | - | openbao-instance | openbao | ignored | - | - | 49 | 48 | - |
| tempest | keystone-2026.1 | openstack | Keystone | api | keystone-tempest-2026-1 | keystone | formula | 2 | 1 | 1938 | 335 | 335 |
| tempest | keystone-2026.1 | openstack | Keystone | api | keystone-tempest-2026-1 | trust-flush | formula | 1 | 1 | 0 | 1 | 1 |
| tempest | keystone-2026.1 | openstack | MariaDB | - | openstack-db | mariadb | ignored | - | - | 442 | 215 | - |
| tempest | keystone-2026.1 | openstack | MariaDB | - | openstack-db-metrics | exporter | ignored | - | - | 11 | 11 | - |
| tempest | keystone-2026.1 | openstack | Memcached | - | openstack-memcached | memcached | ignored | - | - | 49 | 61 | - |
| tempest | keystone-2026.1 | openstack | OpenBaoCluster | - | openbao-instance | openbao | ignored | - | - | 49 | 48 | - |
| tempest | neutron-2025.2 | openstack | Keystone | api | keystone-neutron-tempest-2025-2 | keystone | formula | 2 | 1 | 511 | 284 | 284 |
| tempest | neutron-2025.2 | openstack | MariaDB | - | openstack-db | mariadb | ignored | - | - | 627 | 561 | - |
| tempest | neutron-2025.2 | openstack | MariaDB | - | openstack-db-metrics | exporter | ignored | - | - | 11 | 11 | - |
| tempest | neutron-2025.2 | openstack | Memcached | - | openstack-memcached | memcached | ignored | - | - | 11 | 23 | - |
| tempest | neutron-2025.2 | openstack | Neutron | api | neutron-tempest-2025-2 | neutron-api | formula | 4 | 1 | 1836 | 991 | 991 |
| tempest | neutron-2025.2 | openstack | Neutron | ovn-maintenance-worker | neutron-tempest-2025-2-ovn-maintenance-worker | ovn-maintenance-worker | formula | 1 | 1 | 23 | 335 | 335 |
| tempest | neutron-2025.2 | openstack | Neutron | periodic-workers | neutron-tempest-2025-2-periodic-workers | periodic-workers | formula | 1 | 1 | 11 | 335 | 335 |
| tempest | neutron-2025.2 | openstack | OVNCentral | nb | ovn-neutron-tempest-2025-2-nb | ovsdb | ignored | - | - | 35 | 11 | - |
| tempest | neutron-2025.2 | openstack | OVNCentral | northd | ovn-neutron-tempest-2025-2-northd | northd | formula | 1 | 1 | 35 | 11 | 11 |
| tempest | neutron-2025.2 | openstack | OVNCentral | sb | ovn-neutron-tempest-2025-2-sb | ovsdb | ignored | - | - | 35 | 23 | - |
| tempest | neutron-2025.2 | openstack | OpenBaoCluster | - | openbao-instance | openbao | ignored | - | - | 35 | 48 | - |
| tempest | neutron-2026.1 | openstack | Keystone | api | keystone-neutron-tempest-2026-1 | keystone | formula | 2 | 1 | 671 | 284 | 284 |
| tempest | neutron-2026.1 | openstack | MariaDB | - | openstack-db | mariadb | ignored | - | - | 548 | 600 | - |
| tempest | neutron-2026.1 | openstack | MariaDB | - | openstack-db-metrics | exporter | ignored | - | - | 11 | 11 | - |
| tempest | neutron-2026.1 | openstack | Memcached | - | openstack-memcached | memcached | ignored | - | - | 23 | 23 | - |
| tempest | neutron-2026.1 | openstack | Neutron | api | neutron-tempest-2026-1 | neutron-api | formula | 4 | 1 | 1737 | 991 | 991 |
| tempest | neutron-2026.1 | openstack | Neutron | ovn-maintenance-worker | neutron-tempest-2026-1-ovn-maintenance-worker | ovn-maintenance-worker | formula | 1 | 1 | 35 | 309 | 309 |
| tempest | neutron-2026.1 | openstack | Neutron | periodic-workers | neutron-tempest-2026-1-periodic-workers | periodic-workers | formula | 1 | 1 | 11 | 260 | 260 |
| tempest | neutron-2026.1 | openstack | OVNCentral | nb | ovn-neutron-tempest-2026-1-nb | ovsdb | ignored | - | - | 49 | 11 | - |
| tempest | neutron-2026.1 | openstack | OVNCentral | northd | ovn-neutron-tempest-2026-1-northd | northd | formula | 1 | 1 | 35 | 11 | 11 |
| tempest | neutron-2026.1 | openstack | OVNCentral | sb | ovn-neutron-tempest-2026-1-sb | ovsdb | ignored | - | - | 63 | 23 | - |
| tempest | neutron-2026.1 | openstack | OpenBaoCluster | - | openbao-instance | openbao | ignored | - | - | 49 | 48 | - |
| tempest | nova-2025.2 | openstack | - | - | nova-tempest-2025-2-fake-compute | nova-compute | ignored | - | - | 78 | 175 | - |
| tempest | nova-2025.2 | openstack | Glance | api | glance-nova-tempest-2025-2 | glance-api | glance | 2 | 1 | 35 | 423 | 423 |
| tempest | nova-2025.2 | openstack | Keystone | api | keystone-nova-tempest-2025-2 | keystone | formula | 2 | 1 | 350 | 309 | 309 |
| tempest | nova-2025.2 | openstack | Keystone | api | keystone-nova-tempest-2025-2 | trust-flush | formula | 1 | 1 | 0 | 1 | 1 |
| tempest | nova-2025.2 | openstack | MariaDB | - | openstack-db | mariadb | ignored | - | - | 410 | 684 | - |
| tempest | nova-2025.2 | openstack | MariaDB | - | openstack-db-metrics | exporter | ignored | - | - | 11 | 11 | - |
| tempest | nova-2025.2 | openstack | Memcached | - | openstack-memcached | memcached | ignored | - | - | 23 | 23 | - |
| tempest | nova-2025.2 | openstack | Neutron | api | neutron-nova-tempest-2025-2 | neutron-api | formula | 4 | 1 | 716 | 1052 | 1052 |
| tempest | nova-2025.2 | openstack | Neutron | ovn-maintenance-worker | neutron-nova-tempest-2025-2-ovn-maintenance-worker | ovn-maintenance-worker | formula | 1 | 1 | 23 | 309 | 309 |
| tempest | nova-2025.2 | openstack | Neutron | periodic-workers | neutron-nova-tempest-2025-2-periodic-workers | periodic-workers | formula | 1 | 1 | 11 | 335 | 335 |
| tempest | nova-2025.2 | openstack | NeutronMetadataAgent | metadata-agent | neutron-nova-tempest-2025-2-agent-metadata-agent | metadata-agent | formula, fixed memory | 1 | 1 | 23 | 175 | 175 |
| tempest | nova-2025.2 | openstack | Nova | api | nova-tempest-2025-2 | nova-api | formula | 4 | 1 | 143 | 826 | 826 |
| tempest | nova-2025.2 | openstack | Nova | conductor | nova-tempest-2025-2-conductor | conductor | formula | 2 | 1 | 203 | 309 | 309 |
| tempest | nova-2025.2 | openstack | Nova | metadata | nova-tempest-2025-2-metadata | nova-metadata | formula | 2 | 1 | 11 | 309 | 309 |
| tempest | nova-2025.2 | openstack | Nova | novncproxy | nova-tempest-2025-2-novncproxy | novncproxy | formula | 1 | 1 | 11 | 138 | 138 |
| tempest | nova-2025.2 | openstack | Nova | scheduler | nova-tempest-2025-2-scheduler | scheduler | formula | 2 | 1 | 63 | 260 | 260 |
| tempest | nova-2025.2 | openstack | OVNCentral | nb | ovn-nova-tempest-2025-2-nb | ovsdb | ignored | - | - | 35 | 23 | - |
| tempest | nova-2025.2 | openstack | OVNCentral | northd | ovn-nova-tempest-2025-2-northd | northd | formula | 1 | 1 | 35 | 11 | 11 |
| tempest | nova-2025.2 | openstack | OVNCentral | sb | ovn-nova-tempest-2025-2-sb | ovsdb | ignored | - | - | 35 | 23 | - |
| tempest | nova-2025.2 | openstack | OVNChassis | ovn-controller | ovn-nova-tempest-2025-2-chassis-ovn-controller | ovn-controller | ignored | - | - | 23 | 11 | - |
| tempest | nova-2025.2 | openstack | OVNChassis | ovs | ovn-nova-tempest-2025-2-chassis-ovs | ovs-vswitchd | ignored | - | - | 23 | 23 | - |
| tempest | nova-2025.2 | openstack | OVNChassis | ovs | ovn-nova-tempest-2025-2-chassis-ovs | ovsdb-server | ignored | - | - | 23 | 11 | - |
| tempest | nova-2025.2 | openstack | OpenBaoCluster | - | openbao-instance | openbao | ignored | - | - | 49 | 48 | - |
| tempest | nova-2025.2 | openstack | Placement | api | placement-nova-tempest-2025-2 | placement-api | formula | 2 | 1 | 35 | 195 | 195 |
| tempest | nova-2025.2 | openstack | RabbitmqCluster | rabbitmq | shared-rabbitmq-server | rabbitmq | ignored | - | - | 35 | 195 | - |
| tempest | nova-2026.1 | openstack | - | - | nova-tempest-2026-1-fake-compute | nova-compute | ignored | - | - | 93 | 175 | - |
| tempest | nova-2026.1 | openstack | Glance | api | glance-nova-tempest-2026-1 | glance-api | glance | 2 | 1 | 11 | 363 | 363 |
| tempest | nova-2026.1 | openstack | Keystone | api | keystone-nova-tempest-2026-1 | keystone | formula | 2 | 1 | 350 | 309 | 309 |
| tempest | nova-2026.1 | openstack | Keystone | api | keystone-nova-tempest-2026-1 | trust-flush | formula | 1 | 1 | 0 | 1 | 1 |
| tempest | nova-2026.1 | openstack | MariaDB | - | openstack-db | mariadb | ignored | - | - | 323 | 729 | - |
| tempest | nova-2026.1 | openstack | MariaDB | - | openstack-db-metrics | exporter | ignored | - | - | 11 | 11 | - |
| tempest | nova-2026.1 | openstack | Memcached | - | openstack-memcached | memcached | ignored | - | - | 23 | 23 | - |
| tempest | nova-2026.1 | openstack | Neutron | api | neutron-nova-tempest-2026-1 | neutron-api | formula | 4 | 1 | 627 | 991 | 991 |
| tempest | nova-2026.1 | openstack | Neutron | ovn-maintenance-worker | neutron-nova-tempest-2026-1-ovn-maintenance-worker | ovn-maintenance-worker | formula | 1 | 1 | 23 | 309 | 309 |
| tempest | nova-2026.1 | openstack | Neutron | periodic-workers | neutron-nova-tempest-2026-1-periodic-workers | periodic-workers | formula | 1 | 1 | 11 | 260 | 260 |
| tempest | nova-2026.1 | openstack | NeutronMetadataAgent | metadata-agent | neutron-nova-tempest-2026-1-agent-metadata-agent | metadata-agent | formula, fixed memory | 1 | 1 | 11 | 156 | 156 |
| tempest | nova-2026.1 | openstack | Nova | api | nova-tempest-2026-1 | nova-api | formula | 4 | 1 | 247 | 826 | 826 |
| tempest | nova-2026.1 | openstack | Nova | conductor | nova-tempest-2026-1-conductor | conductor | formula | 2 | 1 | 203 | 309 | 309 |
| tempest | nova-2026.1 | openstack | Nova | metadata | nova-tempest-2026-1-metadata | nova-metadata | formula | 2 | 1 | 11 | 309 | 309 |
| tempest | nova-2026.1 | openstack | Nova | novncproxy | nova-tempest-2026-1-novncproxy | novncproxy | formula | 1 | 1 | 11 | 138 | 138 |
| tempest | nova-2026.1 | openstack | Nova | scheduler | nova-tempest-2026-1-scheduler | scheduler | formula | 2 | 1 | 63 | 363 | 363 |
| tempest | nova-2026.1 | openstack | OVNCentral | nb | ovn-nova-tempest-2026-1-nb | ovsdb | ignored | - | - | 35 | 11 | - |
| tempest | nova-2026.1 | openstack | OVNCentral | northd | ovn-nova-tempest-2026-1-northd | northd | formula | 1 | 1 | 23 | 11 | 11 |
| tempest | nova-2026.1 | openstack | OVNCentral | sb | ovn-nova-tempest-2026-1-sb | ovsdb | ignored | - | - | 35 | 23 | - |
| tempest | nova-2026.1 | openstack | OVNChassis | ovn-controller | ovn-nova-tempest-2026-1-chassis-ovn-controller | ovn-controller | ignored | - | - | 23 | 11 | - |
| tempest | nova-2026.1 | openstack | OVNChassis | ovs | ovn-nova-tempest-2026-1-chassis-ovs | ovs-vswitchd | ignored | - | - | 23 | 11 | - |
| tempest | nova-2026.1 | openstack | OVNChassis | ovs | ovn-nova-tempest-2026-1-chassis-ovs | ovsdb-server | ignored | - | - | 23 | 11 | - |
| tempest | nova-2026.1 | openstack | OpenBaoCluster | - | openbao-instance | openbao | ignored | - | - | 49 | 48 | - |
| tempest | nova-2026.1 | openstack | Placement | api | placement-nova-tempest-2026-1 | placement-api | formula | 2 | 1 | 49 | 195 | 195 |
| tempest | nova-2026.1 | openstack | RabbitmqCluster | rabbitmq | shared-rabbitmq-server | rabbitmq | ignored | - | - | 49 | 195 | - |

## Old and new values

| Constant | Before the run | From the run |
| --- | --- | --- |
| `memoryBase` | `224Mi` | `16Mi` |
| `defaultMemoryPerProcess` | `144Mi` | `352Mi` |
| `glanceMemoryPerProcess` | `400Mi` | `1Gi` |
| `defaultCPURequest` | `100m` | `70m` |
| `minimalServiceCPURequest` | `50m` | `15m` |
| `minimalDatabase` | `100m`, `1Gi` | `65m`, `1Gi` |
| `minimalCache` | `50m`, `128Mi` | `15m`, `96Mi` |
| `minimalMessaging` | `100m`, `512Mi` | `815m`, `512Mi` |
| `minimalSecretStore` | `50m`, `256Mi` | `35m`, `64Mi` |
| `sidecarCPURequest` | `25m` | `25m` |
| `sidecarMemory` | `256Mi` | `256Mi` |

The old figures were estimates: the memory formula from the working sets of CI
job 107440936441, `glanceMemoryPerProcess` from the Glance API with the S3 store
driver under image traffic, `defaultCPURequest` from when the default CPU limit
was dropped, and the `Minimal` figures chosen to pass the node budget gate.

The formula still renders `368Mi` at one process, so a single-process service
keeps its memory. At two processes it renders `720Mi` instead of `512Mi`, and at
four `1424Mi` instead of `800Mi`. A Glance API renders `1040Mi` at one process and
`2064Mi` at two.

## Metadata agent memory

The memory of the `NeutronMetadataAgent` containers is `metadataAgentMemory` in
`operators/neutron/internal/controller/reconcile_daemonset.go`. It is sized for
32 networks on the agent's node and comes from the `metadata-agent` suite
(`tests/e2e/neutron/metadata-agent`), which runs in the `neutron` leg of
`e2e-operator`.

### What the suite measures

The agent container holds the agent, its privsep daemons and one haproxy per
network with a port bound on the node. Steps 9 to 11 of the suite write
networks into the Northbound database with `ovn-nbctl`, bind one port of each
on the kind node, and read the container's cgroup in four phases:

| Release | Networks | How the agent gets there |
| --- | --- | --- |
| 2025.2 | 0 | No port is bound on the node |
| 2025.2 | 1 | The port of one network is bound |
| 2025.2 | 32 | The ports of 31 more networks are bound |
| 2026.1 | 32 | A restart into 2026.1, whose first sync provisions all 32 networks |

A phase polls every 5 seconds until the agent runs one haproxy per network,
and for 30 seconds more. Its reading is the largest working set of those
polls: `memory.current` less the `inactive_file` of `memory.stat`, rounded up
to MiB. The proxies are idle, because no instance asks them for metadata. The
phase prints one line:

```text
MEMORY-READING release=<release> networks=<n> working_set_mi=<MiB> peak_mi=<MiB> oom_kill=<count> haproxy=<count> limit_mi=<MiB>
```

It fails on a container restart, on an OOM kill in the cgroup and on a working
set above 90% of the container's memory limit. A default that no longer holds
32 networks therefore fails the leg.

After each phase with 32 networks the suite prints one `MEMORY-PROCESSES` line
per process name in the container, with the process count and the sum of
`VmRSS`. `VmRSS` counts a shared page once per process, so the sums can exceed
the working set.

### From the readings to the figure

`W1` is the working set of 2025.2 with one network, `W32a` that of 2025.2 with
32 networks and `W32b` that of 2026.1 with 32 networks, all in MiB.

| Figure | Rule | Meaning |
| --- | --- | --- |
| `S` | `ceil((W32a − W1) / 31)`, at least 1 | The memory one more network adds |
| `L` | `max(0, 550 − W1)` | What the lab reading with one network lies above the runner's |
| `F` | `(max(W32a, W32b) + L) × 1.25`, rounded up to a multiple of 64 and at least 768 | `metadataAgentMemory` in MiB |

The 550 is the working set of an agent with one network on a hypervisor of the
metal-stack lab (the run of
[#1142](https://github.com/C5C3/cobaltcore/issues/1142), 2026-10-01). A
hypervisor may run larger privsep daemons than a CI runner, so the lab reading
enters as an offset. The factor 1.25 is the headroom for metadata requests,
which the idle proxies of the suite do not serve. The 768 is the memory the lab
agent ran with.

`S` sizes a node beyond the default: an agent whose node binds `n` networks,
`n` above 32, needs about `F + (n − 32) × S × 1.25` MiB, rounded up to a
multiple of 64. The factor gives each further proxy the headroom `F` gives the
first 32.

## Metadata agent CPU

The CPU request of the `NeutronMetadataAgent` containers is `metadataAgentCPU`
in `operators/neutron/internal/controller/reconcile_daemonset.go`, `230m`. It
comes from a session on the metal-stack lab and from no CI job: no CI leg
boots a server through the agent, so its CI rows read an idle agent, 11 to
23 m. The [lab measurement](#lab-measurement) read a target of `126m` against
the `70m` the container requested then
([#1259](https://github.com/C5C3/cobaltcore/issues/1259)), on one pass of the
quick start with one server per node. The session below loaded one node on
purpose.

### What the session measured

The agent idles and works in bursts. When the first port of a network is bound
on its node, it creates the network's namespace and starts the network's
haproxy, and the booting server then asks it for metadata. The session read
the container's cumulative CPU time from the kubelet
(`usageCoreNanoSeconds` of `/api/v1/nodes/<node>/proxy/stats/summary`) every
10 seconds, which runs nothing in the measured container. A reading is the CPU
time between two samples at least 60 seconds apart, in millicores, and the
readings of a phase do not overlap.

| Phase | Load on the node | Length | Readings (m) | CPU time |
| --- | --- | --- | --- | --- |
| A0 | The agent runs, no server | 300 s | 1.0 to 1.2 | 0.3 s |
| A1 | One server boots on one new network | 379 s | 150, then 1.0 to 1.1 | 11.4 s |
| A2 | 31 servers boot at once, each on a new network of its own | 296 s | 198, 63, 1.3, 1.1 | 16.4 s |
| A3 | The 32 servers run on their 32 networks | 600 s | 1.1 to 1.4 | 0.7 s |
| A4 | The 32 servers are hard-rebooted within 29 seconds | 282 s | 357, 1.5, 1.3, 1.2 | 23.8 s |

Every server ran the `cirros-kvm` image on flavor `1` and was placed on the
node with `--availability-zone`. In A2 the agent logged
`Provisioning metadata for network` 31 times, the console log of each of the 31
servers showed the metadata fetch of cirros-init, and the container ran 32
haproxy processes at the end of the phase. In A4 it logged 34 provisionings.
The largest reading over 10 seconds was `342m` in A1, `222m` in A2 and `770m`
in A4. The agents of the two other nodes, with one server each, read at most
`5m` from A2 to A4.

### From the reading to the request

`C` is 1.15 times the 90th percentile of the readings of A2, A3 and A4,
rounded up to a multiple of `10m`, and at least `130m`. Those phases hold 16
readings, whose 90th percentile is the second largest, `198m`: 1.15 × 198 is
228, so `C` is `230m`. The percentile and the 15 % are what a VPA recommender
applies to a container's CPU samples, and the `130m` is the `126m` of the
earlier lab run, rounded up.

The request covers the minute in which 31 servers booted on the node, and the
single boot of A1. It does not cover the minute of A4. The agent has no CPU
limit, so a burst above the request is not throttled: on a node whose CPUs
are all busy the agent gets the share its request buys, and the burst takes
longer.

The shoot's own recommender watched the DaemonSet's three pods for 56 minutes,
from before A0 until the load had ended, and gave a CPU target of `11m`, the
lowest value it gives. The bursts fill few of its one-minute samples, so its
90th percentile is the idle agent. A burst weighs more in a shorter watch: the
earlier run's lasted 22 minutes.

### Recorded session

The session of 2026-10-05 ran on shoot `newforge` from commit `d1f114f7`, on
Kubernetes v1.35.6, with three workers of 16 CPUs each and the agent image
`ghcr.io/c5c3/neutron:2025.2`. It ran Part 1, Steps 2 to 7 and Part 2, Steps 1
to 5 of the [Quick Start (metal-stack)](../../quick-start-metal-stack.md),
loaded the node `7znw9`, and tore the stack down again. Three things were done
by hand. `make deploy-infra` left the `glance-operator` release and the `k-orc`
Kustomization suspended, and both were resumed with `kubectl patch`. The
c5c3-operator pod, which had exited when the Glance CRD arrived late, was
deleted once out of its restart backoff. The first attempt at A4 passed 32
names to `openstack server reboot`, which takes one, and issued no reboot; the
53 seconds it lasted are in no reading.

With 32 networks bound on the node, the container's working set peaked at
730 MiB, against the `2Gi` of [Metadata agent memory](#metadata-agent-memory);
it was 551 MiB before the first server. The files of the session are in the
[comment on #1259](https://github.com/C5C3/cobaltcore/issues/1259#issuecomment-6001688551).

## Lab measurement

Every row above comes from pods that never ran a virtual machine. In the three
jobs compute is Nova's fake driver: no chassis carries a server's traffic, and
no libvirt runs. The lab measurement runs `hack/ci-vpa-recommendations.sh` by
hand on the [metal-stack lab](../infrastructure/infrastructure-manifests.md#metal-stack-lab)
while Part 2 of the [Quick Start (metal-stack)](../../quick-start-metal-stack.md#hypervisors)
boots, migrates and evicts servers on KVM hypervisors. It measures the
namespaces `openstack` and `hypervisor-system` with the recommender the
Gardener shoot runs, and sets the readings of the compute workloads against
their defaults. It derives no figure: `hack/derive-sizing-figures.py` accepts
the three CI jobs only, and a reading that contradicts a default becomes an
issue of its own, as [#1173](https://github.com/C5C3/cobaltcore/issues/1173)
was for the metadata agent.

### What differs from CI

| Topic | CI on kind | The lab |
| --- | --- | --- |
| Recommender | `deploy/kind/vpa`: VPA 1.8.0, floors of 1 millicore and 1 MB per pod | The shoot's. Its image and flags run in the seed and cannot be read; `report` writes `recommender=-`. Gardener's source at `gardener/gardener@92a252c0` passes floors of 10 millicores and 10 MB |
| Sampling | `--memory-saver=false`: every pod from its start | The same source passes `--memory-saver=true`: a pod is sampled once a VPA selects it, at most 60 seconds after the workload appeared |
| Updater and admission controller | not installed | Both run. A VPA in mode `Off` changes no pod; the run checks it |
| MariaDB scale subresource | removed by `WITH_VPA=true make deploy-infra` | removed by `hack/ci-vpa-recommendations.sh prepare` |
| Compute | Nova's fake driver | KVM on every worker, with one server each |
| Load | the e2e suites and Tempest | Part 2 of the Quick Start (metal-stack), once |
| Report | `job=` names a CI job; input of the derivation | `job=lab`; `hack/derive-sizing-figures.py` refuses it, and no constant is derived from it |

The Gardener figures come from `computeRecommenderArgs` in
`pkg/component/autoscaling/vpa/recommender.go`. The margin of 15 %, the CPU
target at the 90th percentile and the one-minute interval it passes are
defaults a shoot spec can override (`pkg/apis/core/v1beta1/types_shoot.go`),
and neither the seed's Gardener version nor the shoot spec can be read from
the shoot. They are what to expect; the record
states what the run showed.

### Running the lab measurement

The measurement runs beside one pass of the Quick Start (metal-stack). It
starts on a bare shoot, in bash in the root of the clone, with `KUBECONFIG`
set as in [Part 1, Step 1](../../quick-start-metal-stack.md#cp-clone). Run
the whole session in one bash shell: the third block uses the helpers, `out`
and the watch of the first. Each block names the quick start step it follows,
and the quick start's own commands run there unchanged.

After [Part 1, Step 3](../../quick-start-metal-stack.md#cp-deploy) and before
[Step 4](../../quick-start-metal-stack.md#cp-apply), define the helpers,
remove the MariaDB scale subresource and start the watch:

```bash
out=_output/sizing-lab; mkdir -p "$out"
flat() { tr '\n' ' ' | sed 's/ *$//'; }
pod_state() { # <namespace>...: namespace, pod, UID, restarts, the admission controller's vpaUpdates annotation, requests
  local ns
  for ns in "$@"; do
    kubectl get pods -n "$ns" -o json | jq -r --arg ns "$ns" '.items[] | select(.metadata.deletionTimestamp == null)
      | [$ns, .metadata.name, .metadata.uid, ([.status.containerStatuses[]?.restartCount] | add // 0),
         (.metadata.annotations.vpaUpdates // "-"),
         ([.spec.containers[] | "\(.name)=\(.resources.requests.cpu // "-")/\(.resources.requests.memory // "-")"] | join(","))] | @tsv'
  done | sort
}
no_recommendation() { # <namespace>...: the measurement's VPAs without a recommendation, with their conditions
  local ns
  for ns in "$@"; do
    kubectl get vpa -n "$ns" -l app.kubernetes.io/managed-by=ci-vpa-recommendations -o json |
      jq -r --arg ns "$ns" '.items[] | select((.status.recommendation.containerRecommendations // []) | length == 0)
        | [$ns, .metadata.name, ([.status.conditions[]? | "\(.type)=\(.status)"] | join(","))] | @tsv'
  done
}
libvirtd_scope() { # per libvirt pod: node, then memory.current, memory.peak (bytes) and the CPU time of libvirtd's host scope
  local pod
  for pod in $(kubectl get pod -n openstack -l app.kubernetes.io/name=libvirt -o name); do
    printf '%s %s\n' "$(kubectl get -n openstack "$pod" -o jsonpath='{.spec.nodeName}' 2>&1 | flat)" \
      "$(kubectl exec -n openstack "$pod" -c libvirtd -- sh -c 'cd /sys/fs/cgroup/system.slice/cobaltcore-libvirtd.scope && cat memory.current memory.peak && grep usage_usec cpu.stat' 2>&1 | flat)"
  done
}
floors() { # <recommendations.tsv>: per workload its CPU bound, memory bound, containers and name
  awk -F'\t' 'NR > 2 { k = $1 "/" $2 "/" $3; n[k]++
      if (!(k in c) || $12 + 0 < c[k]) c[k] = $12 + 0
      if (!(k in m) || $13 + 0 < m[k]) m[k] = $13 + 0 }
    END { for (k in n) print n[k] * (c[k] + 1), n[k] * m[k], n[k], k }' "$1" | sort -n
}
floor_bounds() { # <recommendations.tsv>: the smallest bound of each resource
  floors "$1" | awk 'NR == 1 || $1 < c { c = $1 } NR == 1 || $2 < m { m = $2 }
    END { if (NR) print "floor bounds: cpu below " c "m, memory at most " m "Mi" }'
}
verdicts() { # <recommendations.tsv>: the rows of the compute workloads with a verdict per resource
  awk -F'\t' '
    function verdict(target, request) { return request == "-" ? "no default" : (target + 0 <= request + 0 ? "confirmed" : "contradicted") }
    NR > 2 && $3 ~ /^(lab-nova-compute|lab-chassis-ovn-controller|lab-chassis-ovs|lab-metadata-agent-metadata-agent|libvirt)$/ {
      printf "%s %s cpu %sm request %s: %s; memory %sMi request %s: %s\n", $3, $9, $12, $16, verdict($12, $16), $13, $17, verdict($13, $17) }' "$1"
}
over_request() { # <recommendations.tsv>: every row whose memory target exceeds its memory request
  awk -F'\t' 'NR > 2 && $17 != "-" && $13 + 0 > $17 + 0 { print $1, $3, $9, "memory target " $13 "Mi above request " $17 "Mi" }' "$1"
}

hack/ci-vpa-recommendations.sh prepare 2>&1 | tee "$out/prepare.log"
nohup hack/ci-vpa-recommendations.sh watch "$out" openstack hypervisor-system >"$out/watch.log" 2>&1 &
```

`prepare` runs once the deploy has installed the MariaDB CRD, before
anything creates a database. The watch starts before Step 4 applies the
ControlPlane, as CI starts it before its suites, so each workload gets its
VPA within 60 seconds of its creation. `nohup` and `&` keep it running after
the command that started it. `openstack` holds the ControlPlane's services
and the five compute DaemonSets, `hypervisor-system` kvm-node-agent and the
migration port reservation, two more workloads that run only on a hypervisor.

The last four helpers read `recommendations.tsv` by column, below its comment
line and its header (see [the column table](#running-a-measurement)): 3
`workload`, 9 `container`, 12 `cpu_target_m`, 13 `memory_target_mi`, 16
`cpu_request_m` and 17 `memory_request_mi`. `verdicts` picks the five
DaemonSets that the CRs in `deploy/lab/metal-stack/hypervisor/compute.yaml`
and the lab's libvirt manifest render:

| Workload | Rendered from |
| --- | --- |
| `lab-nova-compute` | the `NovaCompute` `lab`, through `novaComputeDaemonSetName` in `operators/nova/internal/controller/reconcile_novacompute_daemonset.go` |
| `lab-chassis-ovn-controller`, `lab-chassis-ovs` | the `OVNChassis` `lab-chassis`, through `chassisControllerName` and `chassisOVSName` in `operators/ovn/internal/controller/` |
| `lab-metadata-agent-metadata-agent` | the `NeutronMetadataAgent` `lab-metadata-agent`, through `agentDaemonSetName` in `operators/neutron/internal/controller/reconcile_daemonset.go` |
| `libvirt` | `deploy/lab/metal-stack/hypervisor/libvirt-daemonset.yaml` |

A VPA reads a pod's cgroup. On the lab, libvirtd runs in the host scope
`cobaltcore-libvirtd.scope` (the `systemd-run` line of
`deploy/lab/metal-stack/hypervisor/libvirt-configmap.yaml`) and QEMU in
libvirt's own `/machine` cgroups, both outside the libvirt pod's cgroup, so
the `libvirt` row covers bash and the probes. `libvirtd_scope` reads what no
VPA gives: `memory.current`, `memory.peak` and the CPU time of libvirtd's
scope, through the container's mount of the host's `/sys/fs/cgroup`.

After the checks of [Part 1, Step 7](../../quick-start-metal-stack.md#cp-verify),
record every pod:

```bash
pod_state openstack hypervisor-system >"$out/pods.before"
```

Then run Part 2. After
[Part 2, Step 10](../../quick-start-metal-stack.md#hv-backup), while the
servers still run, end the measurement:

```bash
sleep 300
libvirtd_scope | tee "$out/libvirtd-scope.log"
kill "$(cat "$out/watch.pid")"
hack/ci-vpa-recommendations.sh prepare 2>&1 | tee -a "$out/prepare.log"
hack/ci-vpa-recommendations.sh snapshot "$out" openstack hypervisor-system
GITHUB_SHA="$(git rev-parse HEAD)" GITHUB_JOB=lab \
  MEASURE_LEG="$(kubectl get configmap shoot-info -n kube-system -o jsonpath='{.data.shootName}')" \
  hack/ci-vpa-recommendations.sh report "$out" >/dev/null
pod_state openstack hypervisor-system >"$out/pods.after"
{ diff "$out/pods.before" "$out/pods.after" | grep '^<' || true; } | tee "$out/pods.changed"
awk -F'\t' '$5 != "-"' "$out/pods.after" | tee "$out/pods.rewritten"
no_recommendation openstack hypervisor-system | tee "$out/no-recommendation.log"
floors "$out/recommendations.tsv" | tee "$out/floors.log"
floor_bounds "$out/recommendations.tsv" | tee -a "$out/floors.log"
verdicts "$out/recommendations.tsv" | tee "$out/verdicts.log"
over_request "$out/recommendations.tsv" | tee "$out/over-request.log"
kubectl delete vpa -n openstack -l app.kubernetes.io/managed-by=ci-vpa-recommendations --wait --timeout=120s
kubectl delete vpa -n hypervisor-system -l app.kubernetes.io/managed-by=ci-vpa-recommendations --wait --timeout=120s
kubectl get vpa -A -l app.kubernetes.io/managed-by=ci-vpa-recommendations
```

The `sleep` gives the recommender five more samples with every server
running. The watch is stopped, and one last snapshot follows it.
`MEASURE_LEG` carries the shoot's name into the comment line of
`recommendations.tsv`, which then reads
`run=- attempt=- sha=<commit> job=lab leg=<shoot> recommender=-`.

- A line of `prepare.log` that starts with `ci-vpa-recommendations:` is a
  failed `prepare`, whose exit code the pipe into `tee` hides. The second
  `prepare` line reads `MariaDB CRD serves no scale subresource` unless a
  chart upgrade of `mariadb-operator-crds` restored the subresource during the
  run.
- `pods.changed` lists the lines of `pods.before` that are gone or differ, in
  UID, restart count or requests.
- `pods.rewritten` lists the pods the admission controller rewrote. A VPA in
  mode `Off` rewrites none.
- `no-recommendation.log` lists the measurement's VPAs that hold no
  recommendation at the end, with their conditions.
- `floors.log`, `verdicts.log` and `over-request.log` are what the rules of
  [Reading the lab report](#reading-the-lab-report) read.
- The last command prints `No resources found`.

The subresource stays removed until the next chart upgrade of
`mariadb-operator-crds` or until `make teardown-infra` deletes the CRD:
`k8s.mariadb.com` is among the `STACK_CRD_GROUPS` of `hack/teardown-infra.sh`.
The run's files stay in `_output/sizing-lab`. Git ignores `_output/`, and the
teardown does not touch it.

### Reading the lab report

The floor rule. The recommender raises a container's recommendation to its
pod's floor divided by the pod's containers. On either recommender, a
container with usage samples reads at least `11m` and `11Mi`: the lowest
histogram bucket (10 millicores, 10 MB) plus the 15 % margin. A floor
therefore shows only where its share exceeds that. For a workload with `n`
containers, the floor lies below `n × (smallest CPU target + 1)` millicores
and does not exceed `n × smallest memory target` MiB. The `+ 1` covers the
recommender's rounding to whole millicores, and the report rounds memory up.
`floors` prints these bounds per workload and `floor_bounds` the smallest of
each. When it prints a CPU bound of at most `12m` and a memory bound of at
most `11Mi`, no lab row is a floor share, and a lab row compares with the CI
row of the same container at any value. Otherwise, with `B` the bound
`floor_bounds` printed for the resource, a row of a workload with `n`
containers whose target is at most `B / n` is marked "at the floor" in the
record and gets no verdict for that resource.

The verdict rule. Per container of the five compute workloads and per
resource, `verdicts` prints `no default` when the workload renders no
request, `confirmed` when the target is at most the request, and
`contradicted` when it is above. The target is what the recommender would set
as the request: the 90th percentile of CPU and the peak of memory, each plus
15 %. The upper bound is not used, because after hours instead of days of
samples it is a multiple of the target. These are the defaults the verdicts
read:

| Workload | Default | Source |
| --- | --- | --- |
| `lab-nova-compute` | none: no request, no limit | `buildNovaComputeDaemonSet` copies `spec.resources` alone, `operators/nova/internal/controller/reconcile_novacompute_daemonset.go` |
| `lab-chassis-ovn-controller`, `lab-chassis-ovs` | none | `chassisResources`, `operators/ovn/internal/controller/reconcile_ovs.go` |
| `lab-metadata-agent-metadata-agent` | CPU request `230m`, memory `2Gi` as request and limit; the CPU request was `70m` at the recorded run | `metadataAgentCPU`, `metadataAgentMemory` and `effectiveAgentResources`, `operators/neutron/internal/controller/reconcile_daemonset.go` |
| `libvirt`, container `libvirtd` | CPU request `100m`, memory request `256Mi`, limit `512Mi`, for bash and the probes | the `libvirtd` container's `resources`, `deploy/lab/metal-stack/hypervisor/libvirt-daemonset.yaml` |

Every other row is judged on memory alone: `over_request` lists the rows
whose memory target exceeds the request. For a service row this contradicts
`memoryBase + p × defaultMemoryPerProcess` at its process count, for a
backing-service row its `Minimal` figure.

### Recorded lab run

The run of 2026-10-05 measured on shoot `newforge` from commit `d34fde24`
(`d34fde24334c2809ba2225654c206af9f217eee9`), on Kubernetes v1.35.6, with the
operators from the charts published from `main`
(`deploy/flux-system/releases/c5c3-operator.yaml`). The shoot has three
workers, whose `== cpu` line of the node probe reads
`Intel(R) Xeon(R) D-2141I CPU @ 2.20GHz`. Below, a node is named by the last
part of its name, `shoot--df33f0b4c1--newforge-group-0-85bcf-<suffix>`. At
15:27:32Z the shoot's namespaces were `default`, `firewall`,
`kube-node-lease`, `kube-public`, `kube-system` and `metallb-system` and no
other. Its platform facts were those
[Lab autoscaling](../infrastructure/infrastructure-manifests.md#lab-autoscaling)
gives: the VPA CRD admitted `Off`, `Initial`, `Recreate`, `InPlaceOrRecreate`
and `Auto`, `kube-system` held six VPAs in mode `InPlaceOrRecreate`, the
resource metrics API was available and `pods/resize` was served. Of those
VPAs, `vpn-shoot` read the target `11m` and `11500000` and the CPU lower bound
`10m`, the lowest values the recommender gives (see the floor rule). Part 2,
Step 5 booted one server on each worker, `lab-0` on `7znw9`, `lab-1` on
`hmw67` and `lab-2` on `rt6kn`, each bound to one network, `lab-net`. Step 8
moved `lab-0` to `rt6kn`, and Step 9 evicted `rt6kn`, so from 15:48Z on
`7znw9` ran `lab-0` and `lab-2`, `hmw67` ran `lab-1`, and `rt6kn` ran none.
The watch's first pass ran at 15:33:06Z, before the ControlPlane was applied,
and the last snapshot, the time every row carries, was taken at 15:55:08Z.
The comment line of `recommendations.tsv` reads
`run=- attempt=- sha=d34fde24334c2809ba2225654c206af9f217eee9 job=lab leg=newforge recommender=-`.
Part 1, Steps 1 to 7 ran, without the `git clone` of Step 1 and without the
optional checks Step 7 links, and Part 2, Steps 1 to 10 ran. The ControlPlane
was `Ready` at 15:38:28Z, at the end of Part 1, Step 6. Every block of the
quick start and of this page exited 0 on its first attempt. A script typed
the console commands of Part 2, Steps 6 to 8 through `tmux send-keys`. Nothing
was done by hand on the cluster: the kubeconfig had a current context, so no
context was selected, and the two reads outside the pages, the server list
after Step 10 and `kubectl top` after the third block, changed nothing. The
files of the run are in the
[comment on #1224](https://github.com/C5C3/cobaltcore/issues/1224#issuecomment-5999185983).

| Workload | Container | CPU target (m) | Memory target (MiB) | CPU request | Memory request | CI reading | Verdict (CPU; memory) |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `lab-chassis-ovn-controller` | `ovn-controller` | 11 | 11 | none | none | 23 m, 11 MiB | no default; no default |
| `lab-chassis-ovs` | `ovs-vswitchd` | 23 | 23 | none | none | 23 m, 11 to 23 MiB | no default; no default |
| `lab-chassis-ovs` | `ovsdb-server` | 11 | 11 | none | none | 23 m, 11 MiB | no default; no default |
| `lab-metadata-agent-metadata-agent` | `metadata-agent` | 126 | 641 | `70m` | `2Gi` | 11 to 23 m, 156 to 175 MiB | contradicted; confirmed |
| `lab-nova-compute` | `nova-compute` | 143 | 455 | none | none | 11 to 93 m, 175 MiB, on the fake driver | no default; no default |
| `libvirt` | `libvirtd` | 11 | 11 | `100m` | `256Mi` | none | confirmed; confirmed |

`lab-nova-compute` renders no request, so its targets of `143m` and `455Mi`
under KVM stand without a default; on the fake driver CI read up to `93m` and
`175Mi`. The two chassis DaemonSets render no request either, and their three
containers read 11 to 23 m and 11 to 23 MiB, at or below what CI read. The
metadata agent's CPU request of `70m` is contradicted by its target of `126m`
([#1259](https://github.com/C5C3/cobaltcore/issues/1259)), and
[Metadata agent CPU](#metadata-agent-cpu) has raised it to `230m` since. Its memory of
`2Gi` is confirmed: the target was `641Mi`, which is 557 MiB without the 15 %
margin, beside the 550 MiB of the lab reading that `L` rests on (see
[From the readings to the figure](#from-the-readings-to-the-figure)). The
libvirt pod's `100m` and `256Mi` are confirmed for bash and the probes, whose
targets are the lowest values the recommender gives.

| Workload | Containers | Smallest CPU target | Smallest memory target | CPU bound | Memory bound |
| --- | --- | --- | --- | --- | --- |
| `openstack/DaemonSet/lab-chassis-ovs` | 2 | `11m` | `11Mi` | below `24m` | at most `22Mi` |

`floors.log` ends in `floor bounds: cpu below 12m, memory at most 11Mi`. The
bounds are at most `12m` and `11Mi`, so no lab row is a floor share, and every
lab row compares with the CI row of the same container at its value.

| Node | `memory.current` | `memory.peak` | CPU time | Servers on the node at the read |
| --- | --- | --- | --- | --- |
| `hmw67` | 19 MiB | 51 MiB | 7.7 s | `lab-1` |
| `rt6kn` | 18 MiB | 48 MiB | 9.1 s | none; `lab-2` until Step 9, and `lab-0` from Step 8 to Step 9 |
| `7znw9` | 19 MiB | 47 MiB | 9.0 s | `lab-0` and `lab-2` since Step 9; `lab-0` until Step 8 |

The table gives `cobaltcore-libvirtd.scope` at the start of the third block,
in MiB rounded up and in seconds. The scope holds libvirtd alone, which
runs in the host's `system.slice`; no pod request covers it, and QEMU is
in neither figure.

The checks of the run:

- `prepare.log` holds `MariaDB CRD scale subresource removed`, printed at
  15:33:06Z before Part 1, Step 4, and `MariaDB CRD serves no scale subresource`
  from the third block. The report has the row `openstack-db`, `mariadb`.
- `pods.changed` is empty: each of the 38 pods of `pods.before`, read at
  15:39Z, kept its UID, its restart count and its requests, the pods of the
  finished Jobs among them.
- `pods.rewritten` is empty.
- `no-recommendation.log` is empty: each of the 35 VPAs of the measurement
  held a recommendation, and the deletes left none (`No resources found`).

`over-request.log` holds three rows:

```text
hypervisor-system migration-port-reservation hold memory target 11Mi above request 4Mi
openstack nfs-client-modules hold memory target 11Mi above request 8Mi
openstack nfs-server nfs-server memory target 75Mi above request 64Mi
```

The `nfs-server` row contradicts the `64Mi` request of
`deploy/kind/nfs/nfs-server.yaml`
([#1260](https://github.com/C5C3/cobaltcore/issues/1260)). That server was the
kernel's `nfsd`. NFS-Ganesha replaced it on the day of the run, and its request
of `640Mi` comes from a reading of its own (see
[Lab NFS stack](../infrastructure/infrastructure-manifests.md#lab-nfs-stack)).
The two `hold` rows
read `11Mi`, the lowest value the recommender gives (see the floor rule): their
containers used less than 10 MB, and `kubectl top` read `0Mi` for each of them
at 15:56:10Z. They contradict neither `4Mi` nor `8Mi`, and no issue is filed
for them. No service or backing-service row exceeds its memory request.

The table below is the report of the run, `recommendations.md` as the third
block wrote it:

Sizing measurement: `run=- attempt=- sha=d34fde24334c2809ba2225654c206af9f217eee9 job=lab leg=newforge recommender=-`

| namespace | kind | workload | replicas | owner_kind | owner_name | app | component | container | processes | threads | cpu_target_m | memory_target_mi | cpu_upper_m | memory_upper_mi | cpu_request_m | memory_request_mi | memory_limit_mi | snapshot |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| hypervisor-system | DaemonSet | kvm-node-agent-controller-manager | - | - | - | kvm-node-agent | - | manager | - | - | 11 | 35 | 1131 | 3558 | 10 | 64 | 128 | 2026-10-05T15:55:08Z |
| hypervisor-system | DaemonSet | migration-port-reservation | - | - | - | migration-port-reservation | - | hold | - | - | 11 | 11 | 1138 | 1136 | 1 | 4 | 16 | 2026-10-05T15:55:08Z |
| openstack | Deployment | controlplane-barbican | 1 | Barbican | controlplane-barbican | barbican | api | barbican-api | 1 | 1 | 11 | 138 | 943 | 11835 | 15 | 368 | 368 | 2026-10-05T15:55:08Z |
| openstack | StatefulSet | controlplane-barbican-bao | 1 | OpenBaoCluster | controlplane-barbican-bao | openbao | - | openbao | - | - | 23 | 61 | 2829 | 4900 | - | - | - | 2026-10-05T15:55:08Z |
| openstack | Deployment | controlplane-cinder | 1 | Cinder | controlplane-cinder | cinder | api | cinder-api | 1 | 1 | 23 | 175 | 11614 | 14179 | 15 | 368 | 368 | 2026-10-05T15:55:08Z |
| openstack | Deployment | controlplane-cinder-backup | 1 | Cinder | controlplane-cinder | cinder | backup | backup | - | - | 163 | 215 | 23823 | 17300 | 15 | 2048 | 2048 | 2026-10-05T15:55:08Z |
| openstack | Deployment | controlplane-cinder-scheduler | 1 | Cinder | controlplane-cinder | cinder | scheduler | scheduler | - | - | 23 | 156 | 11711 | 12761 | 15 | 368 | 368 | 2026-10-05T15:55:08Z |
| openstack | Deployment | controlplane-cinder-volume-nfs1 | 1 | Cinder | controlplane-cinder | cinder | volume-nfs1 | volume-nfs1 | - | - | 49 | 260 | 16488 | 21074 | 15 | 368 | 368 | 2026-10-05T15:55:08Z |
| openstack | Deployment | controlplane-glance | 1 | Glance | controlplane-glance | glance | api | glance-api | - | - | 23 | 284 | 11625 | 23040 | 15 | 1040 | 1040 | 2026-10-05T15:55:08Z |
| openstack | Deployment | controlplane-horizon | 1 | Horizon | controlplane-horizon | horizon | api | horizon | 2 | 1 | 11 | 260 | 5150 | 21211 | 15 | 720 | 720 | 2026-10-05T15:55:08Z |
| openstack | Deployment | controlplane-keystone | 1 | Keystone | controlplane-keystone | keystone | api | keystone | 1 | 1 | 126 | 156 | 11086 | 12080 | 15 | 368 | 368 | 2026-10-05T15:55:08Z |
| openstack | Deployment | controlplane-neutron | 1 | Neutron | controlplane-neutron | neutron | api | neutron-api | 1 | 1 | 93 | 260 | 28375 | 21036 | 15 | 368 | 368 | 2026-10-05T15:55:08Z |
| openstack | Deployment | controlplane-neutron-ovn-maintenance-worker | 1 | Neutron | controlplane-neutron | neutron | ovn-maintenance-worker | ovn-maintenance-worker | - | - | 11 | 363 | 933 | 30783 | 15 | 368 | 368 | 2026-10-05T15:55:08Z |
| openstack | Deployment | controlplane-neutron-periodic-workers | 1 | Neutron | controlplane-neutron | neutron | periodic-workers | periodic-workers | - | - | 11 | 335 | 949 | 28902 | 15 | 368 | 368 | 2026-10-05T15:55:08Z |
| openstack | Deployment | controlplane-nova | 1 | Nova | controlplane-nova | nova | api | nova-api | 1 | 1 | 23 | 215 | 14848 | 19581 | 15 | 368 | 368 | 2026-10-05T15:55:08Z |
| openstack | Deployment | controlplane-nova-conductor | 1 | Nova | controlplane-nova | nova | conductor | conductor | - | - | 49 | 156 | 12933 | 14092 | 15 | 368 | 368 | 2026-10-05T15:55:08Z |
| openstack | Deployment | controlplane-nova-metadata | 1 | Nova | controlplane-nova | nova | metadata | nova-metadata | 1 | 1 | 11 | 156 | 13148 | 14327 | 15 | 368 | 368 | 2026-10-05T15:55:08Z |
| openstack | Deployment | controlplane-nova-novncproxy | 1 | Nova | controlplane-nova | nova | novncproxy | novncproxy | - | - | 11 | 138 | 13121 | 12658 | 15 | 368 | 368 | 2026-10-05T15:55:08Z |
| openstack | Deployment | controlplane-nova-scheduler | 1 | Nova | controlplane-nova | nova | scheduler | scheduler | - | - | 23 | 138 | 13026 | 12566 | 15 | 368 | 368 | 2026-10-05T15:55:08Z |
| openstack | StatefulSet | controlplane-ovn-nb | 1 | OVNCentral | controlplane-ovn | ovncentral | nb | ovsdb | - | - | 11 | 11 | 805 | 804 | 70 | 256 | - | 2026-10-05T15:55:08Z |
| openstack | Deployment | controlplane-ovn-northd | 1 | OVNCentral | controlplane-ovn | ovncentral | northd | northd | - | 1 | 11 | 11 | 839 | 838 | 70 | 368 | 368 | 2026-10-05T15:55:08Z |
| openstack | StatefulSet | controlplane-ovn-sb | 1 | OVNCentral | controlplane-ovn | ovncentral | sb | ovsdb | - | - | 23 | 11 | 1695 | 809 | 70 | 256 | - | 2026-10-05T15:55:08Z |
| openstack | Deployment | controlplane-placement | 1 | Placement | controlplane-placement | placement | api | placement-api | 1 | 1 | 11 | 105 | 7526 | 8476 | 15 | 368 | 368 | 2026-10-05T15:55:08Z |
| openstack | Deployment | hypervisor-operator-controller-manager | 1 | - | - | openstack-hypervisor-operator | - | manager | - | - | 11 | 35 | 2405 | 3617 | 10 | 64 | 128 | 2026-10-05T15:55:08Z |
| openstack | DaemonSet | lab-chassis-ovn-controller | - | OVNChassis | lab-chassis | ovnchassis | ovn-controller | ovn-controller | - | - | 11 | 11 | 1224 | 1222 | - | - | - | 2026-10-05T15:55:08Z |
| openstack | DaemonSet | lab-chassis-ovs | - | OVNChassis | lab-chassis | ovnchassis | ovs | ovs-vswitchd | - | - | 23 | 23 | 2366 | 2314 | - | - | - | 2026-10-05T15:55:08Z |
| openstack | DaemonSet | lab-chassis-ovs | - | OVNChassis | lab-chassis | ovnchassis | ovs | ovsdb-server | - | - | 11 | 11 | 1131 | 1129 | - | - | - | 2026-10-05T15:55:08Z |
| openstack | DaemonSet | lab-metadata-agent-metadata-agent | - | NeutronMetadataAgent | lab-metadata-agent | neutronmetadataagent | metadata-agent | metadata-agent | - | - | 126 | 641 | 27260 | 70692 | 70 | 2048 | 2048 | 2026-10-05T15:55:08Z |
| openstack | DaemonSet | lab-nova-compute | - | NovaCompute | lab | novacompute | nova-compute | nova-compute | - | - | 143 | 455 | 25592 | 47080 | - | - | - | 2026-10-05T15:55:08Z |
| openstack | DaemonSet | libvirt | - | - | - | libvirt | - | libvirtd | - | - | 11 | 11 | 1123 | 1121 | 100 | 256 | 512 | 2026-10-05T15:55:08Z |
| openstack | DaemonSet | nfs-client-modules | - | - | - | nfs-client-modules | - | hold | - | - | 11 | 11 | 762 | 761 | 1 | 8 | 32 | 2026-10-05T15:55:08Z |
| openstack | Deployment | nfs-server | 1 | - | - | nfs-server | - | nfs-server | - | - | 11 | 75 | 775 | 5260 | 50 | 64 | 256 | 2026-10-05T15:55:08Z |
| openstack | StatefulSet | openbao-instance | 1 | OpenBaoCluster | openbao-instance | openbao | - | openbao | - | - | 23 | 61 | 1595 | 4203 | - | - | - | 2026-10-05T15:55:08Z |
| openstack | StatefulSet | openstack-db | 1 | MariaDB | openstack-db | mariadb | - | mariadb | - | - | 63 | 489 | 4656 | 36074 | 65 | 1024 | 1024 | 2026-10-05T15:55:08Z |
| openstack | Deployment | openstack-memcached | 1 | Memcached | openstack-memcached | memcached | - | memcached | - | - | 11 | 35 | 808 | 2543 | 15 | 96 | 96 | 2026-10-05T15:55:08Z |
| openstack | StatefulSet | openstack-rabbitmq-server | 1 | RabbitmqCluster | openstack-rabbitmq | openstack-rabbitmq | rabbitmq | rabbitmq | - | - | 23 | 195 | 1685 | 14240 | 815 | 512 | 512 | 2026-10-05T15:55:08Z |
