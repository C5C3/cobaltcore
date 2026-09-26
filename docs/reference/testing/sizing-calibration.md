---
title: Sizing Calibration
quadrant: operator
---

# Sizing Calibration

A CobaltCore CR that names no resources gets its CPU and memory from
render-time defaults, and a ControlPlane on the Minimal
[sizing profile](../c5c3/controlplane-crd.md) gets fixed figures for its
service pods and backing services. This page describes how those figures are
measured with a VerticalPodAutoscaler (VPA) recommender in CI and how a
script derives them from the measurement.

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

## Current figures

No calibration run is recorded yet. The figures in the code are these
estimates:

| Key | Value | Origin |
| --- | --- | --- |
| `memoryBase` | `224Mi` | Working sets of CI job 107440936441 |
| `defaultMemoryPerProcess` | `144Mi` | Working sets of CI job 107440936441 |
| `glanceMemoryPerProcess` | `400Mi` | Glance API with the S3 store driver under image traffic |
| `defaultCPURequest` | `100m` | Set when the default CPU limit was dropped |
| `minimalServiceCPURequest` | `50m` | Chosen to pass the node budget gate |
| `minimalDatabase` | `100m`, `1Gi` | Chosen to pass the node budget gate |
| `minimalCache` | `50m`, `128Mi` | Chosen to pass the node budget gate |
| `minimalMessaging` | `100m`, `512Mi` | Chosen to pass the node budget gate |
| `minimalSecretStore` | `50m`, `256Mi` | Chosen to pass the node budget gate |
| `sidecarCPURequest` | `25m` | Fixed sidecar budget |
| `sidecarMemory` | `256Mi` | Fixed sidecar budget |

A recorded run replaces this section with its run ID, date, commit and
recommender image, the Markdown the derivation printed, and the old and new
value of every constant.
