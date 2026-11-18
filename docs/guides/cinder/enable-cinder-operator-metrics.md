---
title: Enable the Cinder Operator Metrics Endpoint
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

<!-- operator namespace is `cinder-system`; workload (Cinder, CinderBackend, CinderBackupBackend) stays in `openstack`. -->

# How-to: Enable the Cinder Operator Metrics Endpoint

This guide walks an operator through turning on the Prometheus ServiceMonitor
shipped with the `cinder-operator` Helm chart, importing the reference Grafana
dashboard, and verifying that scrape targets transition to `Up`.

The cinder-operator emits the shared sub-reconciler instrumentation under the
`cinder_operator` prefix, plus six per-CR collectors covering the three
Job-driven paths:

| Metric | Type | Labels |
| --- | --- | --- |
| `cinder_operator_reconcile_duration_seconds` | histogram | `sub_reconciler` |
| `cinder_operator_reconcile_errors_total` | counter | `sub_reconciler`, `condition_type` |
| `cinder_operator_db_sync_total` | counter | `cinder`, `namespace`, `result` |
| `cinder_operator_db_sync_duration_seconds` | histogram | `cinder`, `namespace` |
| `cinder_operator_db_purge_total` | counter | `cinder`, `namespace`, `result` |
| `cinder_operator_db_purge_duration_seconds` | histogram | `cinder`, `namespace` |
| `cinder_operator_service_remove_total` | counter | `cinder`, `namespace`, `result` |
| `cinder_operator_service_remove_duration_seconds` | histogram | `cinder`, `namespace` |

`sub_reconciler` carries seventeen values: `Secrets`, `DBConnectionSecret`,
`TransportURLSecret`, `Config`, `Backends`, `BackupBackend`, `Database`,
`Scheduler`, `VolumeServices`, `BackupService`, `Deployment`, `DBPurge`,
`HTTPRoute`, `HealthCheck`, `HPA`, `VPA` and `NetworkPolicy`. All seventeen
come from the `Cinder` pipeline. The operator also reconciles `CinderBackend`
and `CinderBackupBackend`; those controllers emit no sub-reconciler samples of
their own. Their outcomes reach the `Cinder` through the `Backends` and
`BackupBackend` steps. The three job pairs use the CR name and namespace instead
of `sub_reconciler`; they record terminal Job outcomes rather than reconcile
steps.

For the controller-side contract (which sub-reconciler drives which condition),
see [Cinder Reconciler Architecture](../../reference/cinder/cinder-reconciler.md).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_NFS=true WITH_PROMETHEUS=true make deploy-infra
```

Follow that tutorial through the block-storage block of Step 4 and the
**Create a first volume** check in Step 7, so the projected `controlplane-cinder`
child is `Ready` in `openstack` with `nfs1` and `nfsbk` attached. Every resource
name in the examples below is one that devstack produces.
:::

Run the commands below from a workstation with `kubectl` access to the cluster,
`curl` and `jq`; the direct Helm path also requires `helm`. For a non-kind
installation, install the Prometheus Operator CRDs and configure Prometheus's
`serviceMonitorSelector` to match the ServiceMonitor labels and
`serviceMonitorNamespaceSelector` to include `cinder-system`. The db-sync series
in Step 3 requires a reconciled `Cinder`. On the devstack that is
`controlplane-cinder`, projected when the ControlPlane declares the
block-storage service.

## Step 1 — Enable the ServiceMonitor

On that devstack, Step 1 is already done for you. `WITH_PROMETHEUS=true` patches
`monitoring.serviceMonitor.enabled=true` onto the cinder-operator HelmRelease at
bring-up, and `WITH_CONTROLPLANE=true` un-suspends that HelmRelease beforehand,
so the value reconciles and the ServiceMonitor is rendered. Step 1 is the path
for a devstack that is already running without `WITH_PROMETHEUS`, and for
non-kind clusters that run their own Prometheus. Without
`WITH_CONTROLPLANE=true` the release stays suspended, the patch is recorded but
inert, and the bring-up log says so.

On the tutorial devstacks the `cinder-operator` release is owned by Flux (a
`HelmRelease`), so set the chart value by patching that HelmRelease.
Flux's helm-controller reverts any out-of-band
Helm revision on its next reconcile:

```bash
kubectl patch helmrelease cinder-operator -n cinder-system --type=merge \
  -p '{"spec":{"values":{"monitoring":{"serviceMonitor":{"enabled":true}}}}}'

kubectl wait helmrelease/cinder-operator -n cinder-system \
  --for=condition=Ready --timeout=5m
```

Confirm the `ServiceMonitor` was rendered:

```bash
kubectl -n cinder-system get servicemonitor \
  -l app.kubernetes.io/name=cinder-operator
```

The chart renders a `ServiceMonitor` scraping the operator's metrics Service on
the `metrics` port at `/metrics`, every 30 seconds by default
(`monitoring.serviceMonitor.interval`).

::: details Helm-managed installations (non-Flux)
If you installed the operator directly with Helm (not through Flux), set the
value with a rolling `helm upgrade` instead:

```bash
helm upgrade cinder-operator oci://ghcr.io/c5c3/charts/cinder-operator \
  --namespace cinder-system --reuse-values \
  --set monitoring.serviceMonitor.enabled=true
```

Do **not** run this on the tutorial devstacks: there the release is Flux-owned,
and the helm-controller reverts out-of-band revisions on its next reconcile. Use
the HelmRelease patch above instead.
:::

## Step 2 — Import the Grafana dashboard

On the devstack, follow the
[Grafana access instructions](../keystone/enable-keystone-operator-metrics.md#_2-import-the-reference-grafana-dashboard)
to port-forward the Service, open <http://localhost:3000>, and retrieve the
login credentials. For an existing Grafana, use its configured URL and
credentials.

The reference dashboard ships in-repo at
`operators/cinder/dashboards/cinder-operator.json` (uid `cinder-operator`). Its
six panels are the duration quantiles per sub-reconciler, the error rate per
condition type, db-sync duration p95 beside the failed-run rate per CR, the
purge runs broken out per result with their own p95, the duration-and-failure
pair for the service-remove Jobs, and the controller-runtime end-to-end
reconcile histogram filtered to the `cinder` controller. Import it via the
Grafana UI or provision it from a ConfigMap.

## Step 3: Verify the target {#step-3-—-verify-the-target}

Keep the port-forward running in a separate terminal:

```bash
kubectl -n monitoring port-forward svc/prometheus-operated 9090:9090
```

For an existing Prometheus, replace the namespace, Service name and remote port
with those of its HTTP API.
In another terminal, query the targets:

```bash
curl -s 'http://localhost:9090/api/v1/targets' \
  | jq '.data.activeTargets[] | select(.labels.namespace == "cinder-system") | .health'
```

Expect `"up"`. Then confirm the series exist:

```bash
curl -s 'http://localhost:9090/api/v1/query?query=cinder_operator_reconcile_duration_seconds_count' \
  | jq '.data.result | length'
```

A non-zero result count means the operator has reconciled at least one `Cinder`
since the scrape began.

The three job pairs stay empty until a Job terminates, and each fills from a
different event:

```bash
curl -s 'http://localhost:9090/api/v1/query?query=cinder_operator_db_sync_total' \
  | jq '.data.result[] | {cinder: .metric.cinder, result: .metric.result, value: .value[1]}'
```

The db-sync pair takes its sample when the `{name}-db-sync` Job of a `Cinder`
completes, which the ControlPlane devstack does once, while it projects
`controlplane-cinder`. Expect one sample with `cinder: "controlplane-cinder"` and
`result: "succeeded"`. The operator records each run once per Job UID through an
annotation on the CR, so an operator restart after that migration leaves the pair
empty until the next `Cinder` is created.

The db-purge pair fills on the first firing of the `{name}-db-purge` CronJob,
daily at `1 0 * * *` unless `spec.dbPurge.schedule` says otherwise. Only the Jobs
that CronJob controls are counted. To start one immediately in the devstack,
create a Job from that CronJob:

```bash
kubectl create job controlplane-cinder-db-purge-manual \
  --from=cronjob/controlplane-cinder-db-purge -n openstack
```

Kubectl copies the template's labels and sets the CronJob as the Job's
controller, so the operator counts the run once it reaches a terminal state.
That run updates `DBPurgeReady` like a scheduled run. Use a new Job name if you
repeat the command. A Job applied from a copy of the template, without that
owner reference, is not counted. The service-remove pair needs a backend detach:
[Attach an NFS Backend to Cinder](./attach-an-nfs-backend.md) walks one, and the
`{cinder}-{backend}-service-remove` Job it spawns is what fills the pair.

## Tested by

The chart's ServiceMonitor render-and-remove lifecycle is asserted on the CI e2e
kind cluster by the chainsaw suite below (install with
`monitoring.serviceMonitor.enabled=true`, assert the `ServiceMonitor` shape,
uninstall, assert removal). The `WITH_PROMETHEUS=true` kind bring-up in
Step 1 provides a live Prometheus that discovers the ServiceMonitor and
marks the target Up. This suite checks the chart lifecycle on a cluster
with only the prometheus-operator CRDs installed.

```bash
chainsaw test --test-dir tests/e2e/cinder-operator/metrics
```
