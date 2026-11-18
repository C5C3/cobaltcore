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

The nova-operator emits shared sub-reconciler instrumentation for both `Nova`
and `NovaCompute` under the `nova_operator` prefix, plus four per-Nova
collectors covering the two Job-driven paths:

| Metric | Type | Labels |
| --- | --- | --- |
| `nova_operator_reconcile_duration_seconds` | histogram | `sub_reconciler` |
| `nova_operator_reconcile_errors_total` | counter | `sub_reconciler`, `condition_type` |
| `nova_operator_db_sync_total` | counter | `nova`, `namespace`, `result` |
| `nova_operator_db_sync_duration_seconds` | histogram | `nova`, `namespace` |
| `nova_operator_db_archive_total` | counter | `nova`, `namespace`, `result` |
| `nova_operator_db_archive_duration_seconds` | histogram | `nova`, `namespace` |

`sub_reconciler` carries twenty-seven values across both pipelines. `Nova`
contributes nineteen: `Secrets`, `DBConnectionSecrets`, `TransportURLSecret`,
`Config`, `ComputeConfig`, `Database`, `Conductor`, `Scheduler`, `Metadata`,
`ConsoleProxy`, `Deployment`, `DBArchive`, `HTTPRoute`, `MetadataHTTPRoute`,
`ConsoleHTTPRoute`, `HealthCheck`, `HPA`, `VPA` and `NetworkPolicy`.
`NovaCompute` contributes eight: `NovaRef`, `Nodes`, `PoolConfig`, `DaemonSet`,
`ComputeVPA`, `Aggregates`, `Services` and `Traits`. Both pipelines share one
instrumenter and the same metric vectors. For the controller-side contract
(which step drives which condition), see
[Nova Reconciler Architecture](../../reference/nova/nova-reconciler.md).

The two job pairs carry the Nova CR name and namespace instead of a
sub-reconciler label because they record Job lifecycles, not reconcile steps.

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_PROMETHEUS=true make deploy-infra
```

Follow that tutorial through the `nova` block of Step 4 and the
**Boot a first server** catalog check in Step 7, so the projected
`controlplane-nova` child is `Ready` in `openstack`. Every resource name in the
examples below is one that devstack produces.
:::

Run the commands below from a workstation with `kubectl` access to the
cluster, `curl` and `jq`; the direct Helm path also requires `helm`. For a
non-kind installation, install the prometheus-operator CRDs and configure
Prometheus's `serviceMonitorSelector` to match
`app.kubernetes.io/name: nova-operator` and
`app.kubernetes.io/instance: <release-name>`. Its
`serviceMonitorNamespaceSelector` must include `nova-system`. The db-sync
series in Step 3 requires a reconciled `Nova`. On the devstack that is
`controlplane-nova`, projected when the ControlPlane declares the compute
service.

## Step 1: Enable the ServiceMonitor

On that devstack, Step 1 is already done for you. `WITH_PROMETHEUS=true` patches
`monitoring.serviceMonitor.enabled=true` onto the nova-operator HelmRelease at
bring-up, and `WITH_CONTROLPLANE=true` un-suspends that HelmRelease beforehand,
so the value reconciles and the ServiceMonitor is rendered. Step 1 is the path
for a devstack that is already running without `WITH_PROMETHEUS`, and for
non-kind clusters that run their own Prometheus. Without
`WITH_CONTROLPLANE=true` the release stays suspended, the patch is recorded but
inert, and the bring-up log says so.

On the tutorial devstacks the `nova-operator` release is owned by Flux (a
`HelmRelease`), so set the chart value by patching that HelmRelease.
Flux's helm-controller reverts any out-of-band
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
CHART_VERSION="$(helm list -n nova-system --filter '^nova-operator$' -o json \
  | jq -r '.[0].chart // "" | sub("^nova-operator-"; "")')"
helm upgrade nova-operator oci://ghcr.io/c5c3/charts/nova-operator \
  --namespace nova-system --version "$CHART_VERSION" --reuse-values \
  --set monitoring.serviceMonitor.enabled=true
```

Do **not** run this on the tutorial devstacks: there the release is Flux-owned,
and the helm-controller reverts out-of-band revisions on its next reconcile. Use
the HelmRelease patch above instead.
:::

## Step 2: Import the Grafana dashboard

On the devstack, follow the
[Grafana access instructions](../keystone/enable-keystone-operator-metrics.md#_2-import-the-reference-grafana-dashboard)
to port-forward the Service, open <http://localhost:3000>, and retrieve the
login credentials. For an existing Grafana, use its configured URL and
credentials.

The reference dashboard ships in-repo at
`operators/nova/dashboards/nova-operator.json` (uid `nova-operator`). Its five
panels are the duration quantiles per sub-reconciler across both pipelines, the
error rate per condition type, db-sync duration p95 beside the failed-run rate
per CR, the archive runs broken out per result with their own p95, and the
controller-runtime end-to-end reconcile histogram filtered to the `nova` and
`novacompute` controllers. Import it via the Grafana UI or provision it from a
ConfigMap.

## Step 3: Verify the target

Keep the port-forward running in a separate terminal:

```bash
kubectl -n monitoring port-forward svc/prometheus-operated 9090:9090
```

For an existing Prometheus, replace the namespace, Service name and remote port
with those of its HTTP API.
In another terminal, query the targets:

```bash
curl --fail --silent --show-error 'http://localhost:9090/api/v1/targets?state=active' \
  | jq -e '
      [.data.activeTargets[]? |
        select(.labels.namespace == "nova-system") |
        {health, lastScrape, lastError}] as $targets |
      if ($targets | length) == 0 then
        error("no Nova operator target; check Prometheus ServiceMonitor selectors")
      elif any($targets[]; .health != "up") then
        error("a Nova operator target is not up: " + ($targets | tojson))
      else $targets end'
```

The query fails if Prometheus has not selected a target or if one is down;
inspect `lastError` in the output. If no target appears, recheck both
ServiceMonitor selectors. If a target is down, confirm Prometheus can reach the
metrics port. When the Nova NetworkPolicy is enabled, add the Prometheus source
to `networkPolicy.allowMetricsFrom`; see
[Enable the Nova Operator NetworkPolicy](./enable-nova-operator-networkpolicy.md).

Then confirm the series exist:

```bash
curl --fail --silent --show-error \
  'http://localhost:9090/api/v1/query?query=nova_operator_reconcile_duration_seconds_count' \
  | jq -r '
      if .status != "success" then
        error("Prometheus query failed: " + (.error // "unknown error"))
      else (.data.result | length) end'
```

A non-zero result count means the operator has recorded at least one reconcile
observation for a `Nova` or `NovaCompute` since its process started. Prometheus
scrapes the histogram's cumulative count; restarting the operator resets it.

The two job pairs stay empty until a Job terminates, and each fills from a
different event:

```bash
curl --fail --silent --show-error \
  'http://localhost:9090/api/v1/query?query=nova_operator_db_sync_total' \
  | jq -r '
      if .status != "success" then
        error("Prometheus query failed: " + (.error // "unknown error"))
      else
        .data.result[]? |
        {nova: .metric.nova, result: .metric.result, value: .value[1]}
      end'
```

The db-sync pair takes its sample when the `{name}-db-sync` Job of a `Nova`
reaches a terminal state, which the ControlPlane devstack does once while it
projects `controlplane-nova`. A successful initial run yields a sample with
`nova: "controlplane-nova"` and `result: "succeeded"`; failed terminal runs use
`result: "failed"`. The three release-upgrade Jobs, `{name}-db-expand`,
`{name}-db-migrate` and `{name}-db-contract`, feed the same pair when an upgrade
runs. After the operator records a run, its Job-UID annotation prevents a
restart from recording that run again; the pair stays empty until another
database Job terminates.

The db-archive pair fills on the first run of the `controlplane-nova-db-archive`
CronJob, `@daily` unless `services.nova.dbArchive.schedule` on the ControlPlane
says otherwise:

```bash
curl --fail --silent --show-error \
  'http://localhost:9090/api/v1/query?query=nova_operator_db_archive_total' \
  | jq -r '
      if .status != "success" then
        error("Prometheus query failed: " + (.error // "unknown error"))
      else
        .data.result[]? |
        {nova: .metric.nova, result: .metric.result, value: .value[1]}
      end'
```

Only the Jobs that CronJob controls are counted, so a Job created by hand from
the same template is ignored. A suspended archive
(`services.nova.dbArchive.suspend`) spawns no Job, and the pair then simply stops
growing.

## Tested by

The chart's ServiceMonitor render-and-remove lifecycle is asserted on the CI e2e
kind cluster by the chainsaw suite below (install with
`monitoring.serviceMonitor.enabled=true`, assert the `ServiceMonitor` shape,
uninstall, assert removal). The `WITH_PROMETHEUS=true` kind bring-up in
Step 1 provides a live Prometheus that discovers the ServiceMonitor and
marks the target Up. This suite checks the chart lifecycle on a cluster
with only the prometheus-operator CRDs installed.

```bash
chainsaw test --test-dir tests/e2e/nova-operator/metrics
```
