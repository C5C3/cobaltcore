---
title: Enable the Nova Operator Metrics Endpoint
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

<!-- operator namespace is `nova-system`; workload (Nova) stays in `openstack`. -->

# How-to: Enable the Nova Operator Metrics Endpoint

This guide walks an operator through turning on the Prometheus ServiceMonitor
shipped with the `nova-operator` Helm chart, importing the reference Grafana
dashboard, and verifying that scrape targets transition to `Up`.

The nova-operator emits the shared sub-reconciler instrumentation under the
`nova_operator` prefix, plus four per-CR collectors covering the two Job-driven
paths:

| Metric | Type | Labels |
| --- | --- | --- |
| `nova_operator_reconcile_duration_seconds` | histogram | `sub_reconciler` |
| `nova_operator_reconcile_errors_total` | counter | `sub_reconciler`, `condition_type` |
| `nova_operator_db_sync_total` | counter | `nova`, `namespace`, `result` |
| `nova_operator_db_sync_duration_seconds` | histogram | `nova`, `namespace` |
| `nova_operator_db_archive_total` | counter | `nova`, `namespace`, `result` |
| `nova_operator_db_archive_duration_seconds` | histogram | `nova`, `namespace` |

`sub_reconciler` carries eighteen values: `Secrets`, `DBConnectionSecrets`,
`TransportURLSecret`, `Config`, `ComputeConfig`, `Database`, `Conductor`,
`Scheduler`, `Metadata`, `ConsoleProxy`, `Deployment`, `DBArchive`, `HTTPRoute`,
`MetadataHTTPRoute`, `ConsoleHTTPRoute`, `HealthCheck`, `HPA` and
`NetworkPolicy`. All eighteen come from the one `Nova` pipeline; the API group
has no satellite kind whose controller could add more. The two job pairs carry
the CR name and its namespace in place of a sub-reconciler: they measure Jobs,
which is work that happens outside a reconcile.

For the controller-side contract (which sub-reconciler drives which condition),
see [Nova Reconciler Architecture](../../reference/nova/nova-reconciler.md).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_PROMETHEUS=true make deploy-infra
```

Follow that tutorial through the `nova` block of Step 3 and the
**Boot a first server** catalog check in Step 6, so the projected
`controlplane-nova` child is `Ready` in `openstack`. Every resource name in the
examples below is one that devstack produces.
:::

1. A running `nova-operator` Helm release (namespace `nova-system`).
2. The prometheus-operator CRDs (`servicemonitors.monitoring.coreos.com`)
   installed, and a Prometheus whose `serviceMonitorSelector` covers the
   operator namespace.
3. For the db-sync series in Step 3, a `Nova` the operator has already
   reconciled. On the ControlPlane devstack that is `controlplane-nova`, the
   child the plane projects once the compute service is declared.

On that devstack, Step 1 is already done for you. `WITH_PROMETHEUS=true` patches
`monitoring.serviceMonitor.enabled=true` onto the nova-operator HelmRelease at
bring-up, and `WITH_CONTROLPLANE=true` un-suspends that HelmRelease beforehand,
so the value reconciles and the ServiceMonitor is rendered. Step 1 is the path
for a devstack that is already running without `WITH_PROMETHEUS`, and for
non-kind clusters that run their own Prometheus. Without
`WITH_CONTROLPLANE=true` the release stays suspended, the patch is recorded but
inert, and the bring-up log says so.

## Step 1: Enable the ServiceMonitor

On the tutorial devstacks the `nova-operator` release is owned by Flux (a
`HelmRelease`), so set the chart value by patching that HelmRelease rather than
running a raw `helm upgrade`. Flux's helm-controller reverts any out-of-band
Helm revision on its next reconcile:

```bash
kubectl patch helmrelease nova-operator -n nova-system --type=merge \
  -p '{"spec":{"values":{"monitoring":{"serviceMonitor":{"enabled":true}}}}}'

kubectl wait helmrelease/nova-operator -n nova-system \
  --for=condition=Ready --timeout=5m
```

Confirm the `ServiceMonitor` was rendered:

```bash
kubectl -n nova-system get servicemonitor \
  -l app.kubernetes.io/name=nova-operator
```

The chart renders a `ServiceMonitor` scraping the operator's metrics Service on
the `metrics` port at `/metrics`, every 30 seconds by default
(`monitoring.serviceMonitor.interval`).

::: details Helm-managed installations (non-Flux)
If you installed the operator directly with Helm (not through Flux), set the
value with a rolling `helm upgrade` instead:

```bash
helm upgrade nova-operator oci://ghcr.io/c5c3/charts/nova-operator \
  --namespace nova-system --reuse-values \
  --set monitoring.serviceMonitor.enabled=true
```

Do **not** run this on the tutorial devstacks: there the release is Flux-owned,
and the helm-controller reverts out-of-band revisions on its next reconcile. Use
the HelmRelease patch above instead.
:::

## Step 2: Import the Grafana dashboard

The reference dashboard ships in-repo at
`operators/nova/dashboards/nova-operator.json` (uid `nova-operator`). Its five
panels are the duration quantiles per sub-reconciler, the error rate per
condition type, db-sync duration p95 beside the failed-run rate per CR, the
archive runs broken out per result with their own p95, and the
controller-runtime end-to-end reconcile histogram filtered to the `nova`
controller. Import it via the Grafana UI or provision it from a ConfigMap.

## Step 3: Verify the target

```bash
kubectl -n <prometheus-namespace> port-forward svc/prometheus-operated 9090 &
curl -s 'http://localhost:9090/api/v1/targets' \
  | jq '.data.activeTargets[] | select(.labels.namespace == "nova-system") | .health'
```

`<prometheus-namespace>` is the namespace your Prometheus runs in. Expect
`"up"`. Then confirm the series exist:

```bash
curl -s 'http://localhost:9090/api/v1/query?query=nova_operator_reconcile_duration_seconds_count' \
  | jq '.data.result | length'
```

A non-zero result count means the operator has reconciled at least one `Nova`
since the scrape began.

The two job pairs stay empty until a Job terminates, and each fills from a
different event:

```bash
curl -s 'http://localhost:9090/api/v1/query?query=nova_operator_db_sync_total' \
  | jq '.data.result[] | {nova: .metric.nova, result: .metric.result, value: .value[1]}'
```

The db-sync pair takes its sample when the `{name}-db-sync` Job of a `Nova`
completes, which the ControlPlane devstack does once, while it projects
`controlplane-nova`. Expect one sample with `nova: "controlplane-nova"` and
`result: "succeeded"`. The three release-upgrade Jobs, `{name}-db-expand`,
`{name}-db-migrate` and `{name}-db-contract`, feed the same pair when an upgrade
runs. The operator records each run once per Job UID through an annotation on
the CR, so an operator restart after that migration leaves the pair empty until
the next Job terminates.

The db-archive pair fills on the first run of the `controlplane-nova-db-archive`
CronJob, `@daily` unless `services.nova.dbArchive.schedule` on the ControlPlane
says otherwise:

```bash
curl -s 'http://localhost:9090/api/v1/query?query=nova_operator_db_archive_total' \
  | jq '.data.result[] | {nova: .metric.nova, result: .metric.result, value: .value[1]}'
```

Only the Jobs that CronJob controls are counted, so a Job created by hand from
the same template is ignored. A suspended archive
(`services.nova.dbArchive.suspend`) spawns no Job, and the pair then simply stops
growing.

## Tested by

The chart's ServiceMonitor render-and-remove lifecycle is asserted on the CI e2e
kind cluster by the chainsaw suite below (install with
`monitoring.serviceMonitor.enabled=true`, assert the `ServiceMonitor` shape,
uninstall, assert removal). The end-to-end scrape path (a live Prometheus that
discovers the ServiceMonitor and marks the target Up) is the
`WITH_PROMETHEUS=true` kind bring-up described under the prerequisites, not this
suite: the e2e cluster ships only the prometheus-operator CRDs, not a Prometheus
instance.

```bash
chainsaw test --test-dir tests/e2e/nova-operator/metrics
```
