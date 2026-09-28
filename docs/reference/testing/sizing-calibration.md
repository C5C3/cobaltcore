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
from.

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
| `tempest` (12 legs) | Every Tempest-covered service at its default process count, and at four processes where the fixture raises it, under Tempest API load | Service rows at two and four processes, the render-time CPU request |

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
switch on, so the shared sidecar figure only rises. The operators' manager
pods, the OVN chassis and NovaCompute DaemonSets and the platform stack keep
their own figures.

## Running a measurement

The `ci:measure-sizing` label runs the measurement on a pull request. Neither
`ci:full` nor a tag push implies it. [CI Workflow](../ci-cd/ci-workflow.md#sizing-measurement)
lists the steps it adds to each job.

1. Add the label to the pull request. A push to the pull request while the
   run is in flight cancels it.
2. Wait until `e2e-controlplane`, `e2e-controlplane-sso` and the twelve
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
   OVN `nb` and `sb` rows. A `Cinder` row with the component `backup` is a
   formula row with a fixed memory figure: the operator sets its memory to
   `backupMemory` (`2Gi`), because a backup's footprint follows its chunk
   size. Its CPU counts in rules 5 and 6 like
   any formula row's; its memory counts in neither rule 3 nor rule 9.
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
   | Messaging (`RabbitmqCluster`) | `512Mi` | RabbitMQ derives its memory alarm from its limit, so its use reflects the limit it ran under. In the full chain this is the shared kind broker |
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

The derivation printed this Markdown for the recorded run.

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

These containers have a fixed memory figure and stay out of the fit: `Cinder/backup/backup`.

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
| e2e-controlplane | - | openstack | NeutronMetadataAgent | metadata-agent | controlplane-keystone-agent-metadata-agent | metadata-agent | formula | 1 | 1 | 11 | 175 | 175 |
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
| tempest | nova-2025.2 | openstack | NeutronMetadataAgent | metadata-agent | neutron-nova-tempest-2025-2-agent-metadata-agent | metadata-agent | formula | 1 | 1 | 23 | 175 | 175 |
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
| tempest | nova-2026.1 | openstack | NeutronMetadataAgent | metadata-agent | neutron-nova-tempest-2026-1-agent-metadata-agent | metadata-agent | formula | 1 | 1 | 11 | 156 | 156 |
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
