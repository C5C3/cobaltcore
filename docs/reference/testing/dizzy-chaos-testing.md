---
title: dizzy Chaos Testing
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# dizzy Chaos Testing

[dizzy](https://github.com/B42Labs/dizzy) is a scenario-driven load and
consistency tester for OpenStack control planes. Its `chaos` verb runs a
randomized create/mutate/delete churn soak through the OpenStack APIs against a
running ControlPlane. `make dizzy-keystone` and `make dizzy-glance` drive it
against the quick-start stack and export per-operation metrics into the dizzy
VictoriaMetrics for the Grafana dashboards. On the metal-stack lab, the
[in-cluster soak](#in-cluster-soak) runs `dizzy mix chaos` as a Job inside the
cluster for hours and reports on it with the platform's view.

For the overlay that installs the VictoriaMetrics + Grafana backend, see
[Infrastructure Manifests](../infrastructure/infrastructure-manifests.md#dizzy-load-chaos-stack-kind-only-opt-in)
for kind and [Lab dizzy stack](../infrastructure/infrastructure-manifests.md#lab-dizzy-stack)
for the metal-stack lab.

## What the chaos verb does

A chaos run loops for a fixed duration, creating, mutating, and deleting
resources at random through the service APIs. It removes the resources it
created when the run ends, including on an interrupt. After cleanup it runs a
leak check to confirm nothing it created outlived the run. Throughout, it
exports per-operation metrics over OTLP at a 15-second interval.

The 5-minute default duration comes from the scenario's `chaos:` block. Override
it with `DIZZY_ARGS="--duration 30m"`.

## Prerequisites

- The dizzy stack installed on the cluster (`WITH_DIZZY=true make deploy-infra`;
  on the metal-stack lab `EXTERNAL_CLUSTER=true WITH_DIZZY=true` beside the
  lab's other flags). The runners refuse to start when the `dizzy` namespace
  is absent.
- On the metal-stack lab, two port-forwards from the workstation: the
  Gateway's on local port 8443 and one to VictoriaMetrics on local port 8428
  (see [Running a soak](#running-a-soak)).
- A Ready quick-start ControlPlane. The runners read the admin password from the
  `controlplane-keystone-admin-credentials` Secret in the `openstack` namespace
  and generate `_output/dizzy/clouds.yaml` (mode 600, cloud key `devstack-c5c3`)
  from it.
- A Go toolchain on PATH. `hack/dizzy.sh` installs `bin/dizzy` with `go install`
  at the pinned version, skipping the install when `go version -m bin/dizzy`
  already reports that version.

The keystone soak's small profile needs admin credentials; it creates one domain
and two roles. The glance soak uploads synthetic images of up to 40 MiB and
needs nothing beyond a member role.

## Running a soak

```bash
# Churn Keystone for the scenario's default 5 minutes.
make dizzy-keystone

# Churn Glance instead.
make dizzy-glance

# Longer run, fixed seed, keep the created resources for inspection.
DIZZY_ARGS="--duration 30m --seed 42 --no-cleanup" make dizzy-keystone
```

Each target runs three separate preflights before handing off to
`hack/dizzy.sh chaos <service>`: kubectl reachability, the `dizzy` namespace, and
the ControlPlane admin Secret. Each failure prints its own message so the three
causes stay distinguishable.

On the metal-stack lab nothing outside the cluster reaches the stack. Open the
two port-forwards, each in a terminal of its own, then run the soak with
`EXTERNAL_CLUSTER=true`:

```bash
kubectl -n envoy-gateway-system port-forward \
  "$(kubectl -n envoy-gateway-system get svc -l gateway.envoyproxy.io/owning-gateway-name=openstack-gw -o name)" 8443:443
kubectl -n dizzy port-forward svc/dizzy-victoria-metrics-server 8428:8428
EXTERNAL_CLUSTER=true make dizzy-keystone
```

`hack/dizzy.sh` then takes the Keystone URL
`https://keystone.127-0-0-1.nip.io:8443/v3` of the first port-forward, exports
to `localhost:8428` through the second, and calls no `docker`. Neither
script opens a port-forward; restart one that has ended.

The admin password goes only to a Keystone that holds one of the Gateway's
keys. `hack/dizzy.sh` collects the Gateway's certificates from the Secrets
`openstack/*-nip-io-tls` with `jq`, as
[Step 7](../../quick-start-metal-stack.md#cp-verify) of the Quick Start
(metal-stack) does, into `_output/dizzy/gateway-ca.pem`, and names that file as
`cacert` in `clouds.yaml`. dizzy then verifies Keystone and the endpoints of
its catalog against them. Without such a certificate the script exits 1 and
writes no `clouds.yaml`. The kind mode writes `verify: false`.

## In-cluster soak

The in-cluster soak runs `dizzy mix chaos` as a Job inside the metal-stack lab
cluster ([#1274](https://github.com/c5c3/cobaltcore/issues/1274)). `mix` runs a
CI, a Gardener and a Legacy persona side by side, so one run churns Nova,
Neutron, Cinder, Glance, Placement and Keystone together on the lab's
hypervisors. The Job runs for six hours by default, or until it is stopped, in
the OpenStack project `dizzy-soak`. Beside dizzy it samples the platform: the
restarts, OOM kills, CPU and memory of every container of the platform's
namespaces, and the conditions of the ControlPlane and the service CRs. Every
run ends with a report directory on the claim `dizzy-soak-reports` and a PASS
or FAIL verdict. The workstation starts, watches, stops and fetches; it can go
away while the soak runs.

The overlay is `deploy/lab/metal-stack/dizzy-soak/`; see
[Lab dizzy soak](../infrastructure/infrastructure-manifests.md#lab-dizzy-soak)
for its objects and posture.

### Soak prerequisites

- The dizzy stack on the lab, deployed with `WITH_DIZZY=true` beside
  `EXTERNAL_CLUSTER=true`, which declares the namespace `dizzy` and runs the
  VictoriaMetrics the run exports to.
- A Ready ControlPlane in `openstack`, as Part 1 of the
  [Quick Start (metal-stack)](../../quick-start-metal-stack.md) leaves it, and
  the Secret `k-orc-clouds-yaml` that K-ORC uses there.
- The hypervisor fixtures and the hypervisors of Part 2, so the image
  `cirros-kvm` and the flavor `hvo-smoke-test` exist and servers boot. `start`
  checks that the K-ORC objects `image/hvo-cirros-kvm` and
  `flavor/hvo-smoke-test-flavor` are Available unless `DIZZY_SCENARIO` names
  another scenario.
- `kubectl` and `jq` on the workstation, with the kubeconfig context on the
  lab cluster. No port-forward is needed: the Job reaches Keystone and the
  service APIs through their Services in the cluster.

### Soak commands

```bash
# Start a six-hour soak; returns once the soak pod runs.
make dizzy-soak-start

# Start a soak that runs until it is stopped.
DIZZY_SOAK_DURATION=0 make dizzy-soak-start

# The soak's state, pod, start time and the runner's last ten log lines.
make dizzy-soak-status

# Stop it: dizzy removes its resources, the runner writes the report, and the
# report is fetched. Exits 0 when the verdict is PASS.
make dizzy-soak-stop

# Fetch the newest run's report directory to _output/dizzy/soak/<run>/, or a
# named run's.
make dizzy-soak-report
hack/dizzy-soak.sh report 20261012T081500Z
```

`start` runs nine preflights before it changes anything, each with a message
of its own: the cluster answers, the namespace `dizzy` exists, the overlay
exists, every ControlPlane in `DIZZY_CP_NAMESPACE` is Ready, the Secret
`k-orc-clouds-yaml` exists there, the fixtures are Available, the scenario
file exists, the settings are well formed, and no soak runs. Then it deletes a
finished Job `dizzy-soak` and a pod `dizzy-soak-reader` that a report left
behind, and creates the Secret `dizzy-soak-user-password` when it is absent. It applies the overlay and waits up to 300 seconds for
K-ORC to make the soak's Keystone objects Available. It writes the Secret
`dizzy-soak-clouds` and the ConfigMaps `dizzy-soak-runner`,
`dizzy-soak-scenario` and `dizzy-soak-config`, creates the Job with the dizzy
image of the pin in `hack/dizzy.sh`, and waits up to 300 seconds for its pod
to run.

`status` prints `soak: none`, `running`, `complete` or `failed` and exits 1
only for `none`. `stop` sends TERM to the runner, waits up to 600 seconds for
the Job to finish, fetches the report, and exits 0 when the Job is Complete,
which it is on PASS. `report` copies from the soak pod while it runs; such a
copy is a snapshot without `report.md` and `verdict.json`. After the run it
starts the pod `dizzy-soak-reader`, which mounts the claim read-only, and
deletes it when the copy is done.

When a wait runs out, the command exits 1 and changes nothing more. If K-ORC
does not make the identity Available, `start` prints the state of the five
objects with `kubectl get -f` and creates no Job; the K-ORC controller's log
in `orc-system` names the cause, and `start` can run again once it is fixed.
If the pod does not run within 300 seconds, `kubectl -n dizzy describe pod -l
batch.kubernetes.io/job-name=dizzy-soak` shows why, often an image pull or a
claim that does not bind. The Job stays, and `start` counts it as a running
soak; `make dizzy-soak-stop` deletes a Job whose pod never ran and exits 1,
so `start` can run again once the cause is fixed. If the Job has not
finished 600 seconds after `stop`, dizzy is still removing resources:
`make dizzy-soak-status` shows its last log lines, and
`make dizzy-soak-report` fetches the report once the Job has finished.

The pod log carries dizzy's output and, at the end, `report.md`:
`kubectl -n dizzy logs -f job/dizzy-soak -c runner`. Grafana shows the run
under the service `mix`, as in [Watching the run](#watching-the-run).

### Soak settings

| Variable | Default | Effect |
| --- | --- | --- |
| `DIZZY_SOAK_SERVICE` | `mix` | The dizzy service the runner calls, `dizzy <service> chaos`. A single-service churn such as `nova` needs a `DIZZY_SCENARIO` of that service. |
| `DIZZY_SOAK_DURATION` | `6h` | The run time, a duration of `h`, `m` and `s` parts such as `90m` or `1h30m`; `0` runs until the soak is stopped. dizzy gets it as `--duration`, which takes precedence over the scenario's `chaos.duration`. |
| `DIZZY_SCENARIO` | `<overlay>/dizzy-soak/scenario.yaml` | The scenario file `start` ships as the ConfigMap `dizzy-soak-scenario`. |
| `DIZZY_ARGS` | empty | Extra dizzy flags, space-separated, such as `--set resources.servers=10`. Flags with embedded quoted spaces are not supported. |
| `DIZZY_VERSION` | the pin of `hack/dizzy.sh` | The tag of the image `ghcr.io/b42labs/dizzy` the Job copies the binary from. |
| `DIZZY_AUTH_URL` | `http://<DIZZY_CP_NAME>-keystone.<DIZZY_CP_NAMESPACE>.svc:5000/v3` | The Keystone URL of the soak's `clouds.yaml`. |
| `DIZZY_CP_NAME` | `controlplane` | The ControlPlane whose Keystone Service the default URL names. |
| `DIZZY_CP_NAMESPACE` | `openstack` | The namespace of the ControlPlanes, the Secret `k-orc-clouds-yaml`, the fixtures and the soak's identity. |
| `EXTERNAL_OVERLAY` | `deploy/lab/metal-stack` | The overlay root; `start` applies its `dizzy-soak/`. |
| `DIZZY_SOAK_NAMESPACES` | empty | The platform namespaces to sample, space-separated. Empty samples every namespace except `kube-system`, `kube-public`, `kube-node-lease`, `default` and `dizzy`. |
| `DIZZY_SOAK_SAMPLE_INTERVAL` | `60` | Seconds between two platform samples. |
| `DIZZY_SOAK_MAX_ERROR_RATE` | empty | The maximum share of failed operations in percent, from 0 to 100. Empty runs no `error-rate` check. |
| `DIZZY_SOAK_MAX_P95_SECONDS` | empty | The maximum overall p95 latency in seconds. Empty runs no `p95` check. |

`start` writes the runner's settings and the resolved `DIZZY_VERSION` into the
ConfigMap `dizzy-soak-config`, which the Job reads once at its start.

The default scenario is a copy of dizzy's `scenarios/mix/small.yaml` with the
lab's image `cirros-kvm` and flavor `hvo-smoke-test`, resize and cold
migration of the Legacy persona turned off, and `chaos.duration: 6h`. Its six
servers divide into 3 CI, 2 Gardener and 1 Legacy server, which fit the
default quotas of the project `dizzy-soak`. The Legacy persona stops and
starts its server, live-migrates it, and detaches and re-attaches its volumes
and ports. Nova reaches the other host of a resize or a cold migration over
ssh, and the compute image has no ssh client.

### Report directory

Each run writes `/reports/<run>/` on the claim, `<run>` being its UTC start
such as `20261012T081500Z`. `report` copies it to `_output/dizzy/soak/<run>/`.

| File | Content |
| --- | --- |
| `report.md` | The verdict and its checks, the run's parameters and times, dizzy's table report, the leak-check line, the restarts and OOM kills, the pods created during the run per namespace (owner other than a Job), the 15 containers with the largest memory growth and the 15 with the highest mean CPU, the conditions, and the line count of `platform/errors.log`. The runner prints it to the pod log as well. |
| `verdict.json` | `{"verdict": "PASS" or "FAIL", "checks": [{"name", "pass", "detail"}]}`. |
| `meta.json` | The run name, start and end time, service, duration, dizzy version and arguments, platform namespaces, sample interval, both thresholds, and dizzy's exit code. |
| `scenario.yaml` | The scenario of the run. |
| `dizzy.log` | dizzy's output. |
| `run-<id>.json` | dizzy's run record, which dizzy rewrites once a minute while it runs. |
| `dizzy-report.json`, `.html`, `.txt` | `dizzy <service> report` of the record in the formats `json`, `html` and `table`; absent unless dizzy wrote one record. |
| `platform/containers.tsv` | One line per container of the platform's pods and sample: timestamp, namespace, pod, pod UID, pod creation time, owner kind, container, `restartCount`, last terminated reason and its `finishedAt`. |
| `platform/usage.tsv` | One line per container and sample of the metrics API: timestamp, namespace, pod, container, CPU in millicores and memory in bytes. |
| `platform/pod-samples.log` | The time of every pod sample that succeeded. |
| `platform/errors.log` | One line per failed read, with kubectl's message. |
| `platform/conditions-start.json`, `conditions-end.json` | The `status.conditions` of every ControlPlane and service CR at the start and the end. A kind whose CRD is absent is skipped. |
| `platform/restarts.tsv` | Per pod UID and container: the restarts during the run (the last `restartCount` minus the first sample's, or minus 0 for a pod created during the run) and the OOM kills (a last termination `OOMKilled` with `finishedAt` inside the run). |
| `platform/usage-summary.tsv` | Per container: the sample count, CPU mean and maximum, the memory mean of its first and of its last tenth of samples, the maximum, and the growth between the two means in MiB and percent. |
| `platform/conditions.tsv` | Per object: kind, namespace, name, `Ready` at the start and at the end, and every condition whose `lastTransitionTime` lies inside the run. |

The runner reads the conditions of `controlplanes.c5c3.io` and of
`keystones`, `glances`, `placements`, `barbicans`, `horizons`, `neutrons`,
`neutronmetadataagents`, `novas`, `novacomputes`, `cinders`, `ovncentrals` and
`ovnchassis`, each in its `<service>.openstack.c5c3.io` group.

### Verdict checks

The verdict is PASS when every check that ran passed. The first four checks
always run, the last two only when their setting is set.

| Check | Fails when |
| --- | --- |
| `dizzy-exit` | dizzy exits with a code other than 0, or writes no run record or more than one. |
| `leak-check` | The last leak-check line of `dizzy.log` is not the clean one, `leak check: no run-tagged resources remain` (`glance`: `leak check: no run-tagged images remain`; `keystone`: `leak check: no run-named resources remain`). dizzy exits 0 even when its leak check finds resources, so the check reads the log. Its detail is dizzy's leak-check line, or `leak check line not found`. |
| `controlplane-ready` | No ControlPlane was read at the end, the read failed, or one is not `Ready=True`. |
| `no-restarts` | No pod sample succeeded, or a container of the platform restarted or was OOM-killed during the run. |
| `error-rate` | `.metrics.overall.failed / .metrics.overall.attempted` of `dizzy-report.json`, in percent, exceeds `DIZZY_SOAK_MAX_ERROR_RATE`, or no operation was attempted. |
| `p95` | `.metrics.overall.latency.p95` of `dizzy-report.json` exceeds `DIZZY_SOAK_MAX_P95_SECONDS`. |

The runner exits 0 on PASS and 1 on FAIL, so the Job ends Complete or Failed
by verdict. A completed duration, a `stop`, a deleted Job and a pod eviction
take the same path: the runner sends TERM to dizzy once and waits while dizzy
removes its resources and prints its leak check. Then it writes the report.

### Limits

- One soak at a time, in one project the three personas share, without
  dizzy's background lanes. dizzy logs
  `lanes share a project; each quota pre-check saw only its own plan` and
  goes on.
- No resize and no cold migration in the default scenario, since the lab's
  compute image has no ssh client.
- No default thresholds for the error rate or the latency, and no quotas for
  the project. A larger scenario, such as one run with
  `--set resources.servers=20`, needs Nova's quotas raised by hand.
- Nothing restarts a soak: `backoffLimit: 0` ends it when its pod is lost.
  After a TERM the pod has 540 seconds to clean up and report; a teardown of
  dizzy that takes longer is cut short and leaves resources behind.
- The report reads the Kubernetes API directly. It holds no events and no node
  conditions, and the platform's samples go to no VictoriaMetrics series.
- Nothing in the soak is specific to the lab but the overlay's location and
  the scenario's names; only the lab run is proven.
- K-ORC applies the user's password only when the name of its Secret changes.
  To reset it, delete the K-ORC User `dizzy-soak` and the Secret
  `dizzy-soak-user-password` in `openstack` together; the next `start`
  creates both again.

### Freeing the claim

Nothing prunes old runs. Fetch the runs to keep, then delete the finished Job
and the claim; the next `start` applies the claim again, empty:

```bash
make dizzy-soak-report
kubectl -n dizzy delete job dizzy-soak --ignore-not-found
kubectl -n dizzy delete pvc dizzy-soak-reports
```

Where the default class has the reclaim policy `Delete`, the volume and every
report on it go with the claim.

### Reclaiming resources after a killed runner

A runner killed without a TERM, by a node loss or at the end of its grace
period, leaves the run's servers, volumes, ports and networks in the project
`dizzy-soak`. dizzy rewrites its run record in the report directory once a
minute, so the record names what the run created up to a minute before the
kill. A one-off pod of the dizzy image, with the Secret `dizzy-soak-clouds`
and the claim mounted, deletes the resources by the record with
`dizzy mix cleanup --run run-<id>.json`:

```bash
run=20261012T081500Z          # the run, as make dizzy-soak-report prints it
record=run-0123456789ab.json  # its record, as _output/dizzy/soak/<run>/ shows it
version="$(sed -n 's/^DIZZY_VERSION="${DIZZY_VERSION:-\(v[0-9.]*\)}"$/\1/p' hack/dizzy.sh)"
kubectl create -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: dizzy-soak-cleanup
  namespace: dizzy
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 65534
    runAsGroup: 65534
  containers:
    - name: cleanup
      image: ghcr.io/b42labs/dizzy:${version}
      args: ["mix", "cleanup", "--os-cloud", "dizzy-soak", "--run", "/reports/${run}/${record}"]
      env:
        - name: OS_CLIENT_CONFIG_FILE
          value: /etc/openstack/clouds.yaml
      volumeMounts:
        - {name: clouds, mountPath: /etc/openstack, readOnly: true}
        - {name: reports, mountPath: /reports, readOnly: true}
  volumes:
    - name: clouds
      secret: {secretName: dizzy-soak-clouds}
    - name: reports
      persistentVolumeClaim: {claimName: dizzy-soak-reports, readOnly: true}
EOF
kubectl -n dizzy wait pod/dizzy-soak-cleanup --for=jsonpath='{.status.phase}'=Succeeded --timeout=900s
kubectl -n dizzy logs dizzy-soak-cleanup
kubectl -n dizzy delete pod dizzy-soak-cleanup
```

The log ends with dizzy's leak check. The claim is `ReadWriteOnce`, so the pod
runs only while no other pod on another node mounts it.

### Lab soak run

The run of 2026-10-07 (`20261007T180919Z`, 18:09Z to 00:12Z) ran the default
settings on `newforge`, the lab's shoot of three workers with 16 vCPUs and
31 GiB each, from `main` right after #1304 merged, with dizzy v0.5.0, the
10 s liveness timeouts of #1296 and the 1Gi broker of #1298. Verdict PASS:
`dizzy-exit` pass (exit 0, `run-dd8c475c.json` written), `leak-check` pass
(no run-tagged resource left), `controlplane-ready` pass, `no-restarts` pass
(151 containers, 0 restarts, 0 OOM kills). dizzy ran 9912 operations with
0 failures (error rate 0 %): 3885 server, 3263 volume, 1178 network, 797
port, 788 subnet and 1 server group; p50 145 ms, p95 6.1 s, p99 7.6 s, with
the port and server p95 the live-migration waits of the Legacy persona.
`nova.migrations` holds 193 live migrations of the run, all `completed`.
Keystone's 3600 s token lifetime showed as exactly three 401 on `nova-api`
at every full hour after the start, one per persona, each followed by a
re-authentication, so B42Labs/dizzy#85 held over six boundaries. The three
earlier runs, two of them FAIL, are recorded in #1304. Findings, one per
line:

- The third hypervisor hosted nothing for the whole run: a stale
  `failed_builds: 1` and Nova's default `BuildFailureWeigher` kept it out of
  every placement, so the lab ran as a two-node cloud (#1310).
- `nova-scheduler` grew from 128 to 191 MiB of anonymous memory at a constant
  rate and had not levelled off at six hours (#1311).
- `nova-compute` ran at 90 to 98 % of a 512Mi limit the operator never set;
  the openbao tenant LimitRange sizes it, and the page cache of the instance
  directory inflates the reading (#1312).
- os-brick's ScaleIO connector logs an ERROR on every volume attach, 34 per
  hour, for a tool the image does not ship (#1313).
- No hypervisor is in a host aggregate, so the zone `eqx-mu4` the pool
  reports is empty in Nova (#1314).
- Not filed: the metadata agents grew by 70 to 80 MiB within their 2Gi
  limit, oscillating, and a reader pod started while the previous soak pod
  still held the claim got a `Multi-Attach` event until that pod was gone.

## Variables

Every input of `make dizzy-keystone` and `make dizzy-glance` is an optional
environment override; the in-cluster soak has [settings](#soak-settings) of its
own. The dizzy version pin lives only in `hack/dizzy.sh`.

| Variable | Effect |
| --- | --- |
| `DIZZY_SCENARIO` | Path to an alternate scenario file (default the cached `scenarios/<service>/small.yaml`). |
| `DIZZY_ARGS` | Extra dizzy flags, space-separated (for example `--duration 30m`, `--no-cleanup`, `--seed 42`). Flags with embedded quoted spaces are not supported. |
| `DIZZY_VERSION` | Pin override for the dizzy version. |
| `DIZZY_AUTH_URL` | Keystone auth URL override, skipping the host-port probe. |
| `DIZZY_SECRET` | Name of the ControlPlane admin Secret (default `controlplane-keystone-admin-credentials`). |
| `DIZZY_CP_NAMESPACE` | Namespace of the admin Secret (default `openstack`). |
| `KIND_CLUSTER` | Cluster name for the `docker port` probes (default `cobaltcore`). Note that `make deploy-infra` itself keys off `CLUSTER_NAME`. Not read under `EXTERNAL_CLUSTER=true`. |
| `EXTERNAL_CLUSTER` | `true` runs against the metal-stack lab: the Keystone URL of the Gateway port-forward on local port 8443, verified against the Gateway's certificates, no `docker port` probe, and a hint to open the VictoriaMetrics port-forward when `localhost:8428` does not answer. Any other value keeps the kind mode. `make deploy-infra` and `make teardown-infra` read the same variable. |

## Watching the run

Grafana serves the dizzy dashboards at `https://dizzy.127-0-0-1.nip.io`; append
`:<KIND_HOST_PORT>` when that override is set to something other than 443. On
the metal-stack lab it answers at `https://dizzy.127-0-0-1.nip.io:8443` through
the Gateway port-forward. Access is anonymous Viewer, so no login is needed.
Three dashboards ship with the overlay:

| Dashboard | UID |
| --- | --- |
| dizzy Overview (the anonymous home dashboard) | `dizzy-overview` |
| dizzy API Operations | `dizzy-api-operations` |
| dizzy Time to Ready | `dizzy-time-to-ready` |

## Metric families

Each family carries `cloud`, `scenario`, and `service` labels.

| Metric | Meaning |
| --- | --- |
| `dizzy_operation_duration_seconds` | Duration of a single API operation. |
| `dizzy_resource_time_to_ready_seconds` | Time for a created resource to reach a ready state. |
| `dizzy_iteration_duration_seconds` | Duration of one churn iteration. |
| `dizzy_iteration_operations_total` | Operations performed per iteration. |
| `dizzy_iterations_total` | Iterations completed. |

`dizzy_operation_errors_total` shows up only after an operation has failed, so a
clean run never emits it. List what actually landed after a run:

```bash
curl -fsS http://localhost:8428/api/v1/label/__name__/values
```

## Troubleshooting

The tooling prints two warnings during a run.

**No 30428 port mapping.** When the kind cluster has no host mapping for port
30428, it predates the mapping in `hack/kind-config.yaml` and OTLP ingest cannot
reach VictoriaMetrics. Only the kind mode probes the mapping. Recreate the
cluster to fix it:

```bash
make teardown-infra && WITH_DIZZY=true make deploy-infra
```

**VictoriaMetrics unreachable.** When nothing answers on `localhost:8428`, the
warning notes that metrics are exported into the void. The soak still runs and
exits by dizzy's own result, because dizzy degrades export failures to warnings.
Under `EXTERNAL_CLUSTER=true` a third line names the port-forward that is
missing:

```text
         Open the port-forward first: kubectl -n dizzy port-forward svc/dizzy-victoria-metrics-server 8428:8428
```
