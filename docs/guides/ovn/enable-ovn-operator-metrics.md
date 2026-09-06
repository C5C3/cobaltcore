---
title: Enable the OVN Operator Metrics Endpoint
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

<!-- operator namespace is `ovn-system`; workload (OVNCentral, OVNChassis) stays in `openstack`. -->

# How-to: Enable the OVN Operator Metrics Endpoint

This guide walks an operator through turning on the Prometheus ServiceMonitor
shipped with the `ovn-operator` Helm chart, importing the reference Grafana
dashboard, and verifying that scrape targets transition to `Up`.

The ovn-operator emits the shared sub-reconciler instrumentation under the
`ovn_operator` prefix, plus two per-CR collectors covering the database
snapshots:

| Metric | Type | Labels |
| --- | --- | --- |
| `ovn_operator_reconcile_duration_seconds` | histogram | `sub_reconciler` |
| `ovn_operator_reconcile_errors_total` | counter | `sub_reconciler`, `condition_type` |
| `ovn_operator_backup_total` | counter | `ovncentral`, `namespace`, `result` |
| `ovn_operator_backup_duration_seconds` | histogram | `ovncentral`, `namespace` |

One operator serves two kinds, so `sub_reconciler` carries twelve values: `TLS`,
`Northbound`, `Southbound`, `Endpoints`, `Northd`, `Relay` and `Backup` from the
`OVNCentral` pipeline, `Central`, `Nodes`, `OVS`, `Controller` and
`Maintenance` from the `OVNChassis` one. The backup pair carries the CR name and
its namespace: it measures the Jobs the CronJob spawns, which is work that
happens outside a reconcile.

For the controller-side contract (which sub-reconciler drives which condition),
see [OVN Reconciler Architecture](../../reference/ovn/ovn-reconciler.md).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_PROMETHEUS=true make deploy-infra
```

Follow that tutorial through to the **Create a first network** check in its
Step 6, so the ovn-operator (namespace `ovn-system`) is running with
kube-prometheus-stack scraping it.
:::

1. A running `ovn-operator` Helm release (namespace `ovn-system`).
2. The prometheus-operator CRDs (`servicemonitors.monitoring.coreos.com`)
   installed, and a Prometheus whose `serviceMonitorSelector` covers the
   operator namespace.
3. For the backup series in Step 3, an `OVNCentral` the operator has already
   reconciled. On the ControlPlane devstack that is `controlplane-ovn`, the
   central the quick start applies before the `ControlPlane` CR.

On that devstack, Step 1 is already done for you. `WITH_PROMETHEUS=true` patches
`monitoring.serviceMonitor.enabled=true` onto the ovn-operator HelmRelease at
bring-up, and `WITH_CONTROLPLANE=true` un-suspends that HelmRelease beforehand,
so the value reconciles and the ServiceMonitor is rendered. Step 1 is the path
for a devstack that is already running without `WITH_PROMETHEUS`, and for
non-kind clusters that run their own Prometheus. Without
`WITH_CONTROLPLANE=true` the release stays suspended, the patch is recorded but
inert, and the bring-up log says so.

## Step 1 — Enable the ServiceMonitor

On the tutorial devstacks the `ovn-operator` release is owned by Flux (a
`HelmRelease`), so set the chart value by patching that HelmRelease rather than
running a raw `helm upgrade`. Flux's helm-controller reverts any out-of-band
Helm revision on its next reconcile:

```bash
kubectl patch helmrelease ovn-operator -n ovn-system --type=merge \
  -p '{"spec":{"values":{"monitoring":{"serviceMonitor":{"enabled":true}}}}}'

kubectl wait helmrelease/ovn-operator -n ovn-system \
  --for=condition=Ready --timeout=5m
```

Confirm the `ServiceMonitor` was rendered:

```bash
kubectl -n ovn-system get servicemonitor \
  -l app.kubernetes.io/name=ovn-operator
```

The chart renders a `ServiceMonitor` scraping the operator's metrics Service on
the `metrics` port at `/metrics`, every 30 seconds by default
(`monitoring.serviceMonitor.interval`).

::: details Helm-managed installations (non-Flux)
If you installed the operator directly with Helm (not through Flux), set the
value with a rolling `helm upgrade` instead:

```bash
helm upgrade ovn-operator oci://ghcr.io/c5c3/charts/ovn-operator \
  --namespace ovn-system --reuse-values \
  --set monitoring.serviceMonitor.enabled=true
```

Do **not** run this on the tutorial devstacks: there the release is Flux-owned,
and the helm-controller reverts out-of-band revisions on its next reconcile. Use
the HelmRelease patch above instead.
:::

## Step 2 — Import the Grafana dashboard

The reference dashboard ships in-repo at
`operators/ovn/dashboards/ovn-operator.json` (uid `ovn-operator`). Its six
panels are the duration quantiles per sub-reconciler, the error rate per
condition type, backup duration p95 beside the failed-run rate per CR, backup
runs per result, reconcile errors per sub-reconciler, and the controller-runtime
end-to-end reconcile histogram filtered to the `ovncentral` and `ovnchassis`
controllers. Import it via the Grafana UI or provision it from a ConfigMap.

## Step 3 — Verify the target

```bash
kubectl -n <prometheus-namespace> port-forward svc/prometheus-operated 9090 &
curl -s 'http://localhost:9090/api/v1/targets' \
  | jq '.data.activeTargets[] | select(.labels.namespace == "ovn-system") | .health'
```

Expect `"up"`. Then confirm the series exist:

```bash
curl -s 'http://localhost:9090/api/v1/query?query=ovn_operator_reconcile_duration_seconds_count' \
  | jq '.data.result | length'
```

A non-zero result count means the operator has reconciled at least one
`OVNCentral` or `OVNChassis` since the scrape began. The backup pair stays empty
until a backup Job terminates on `controlplane-ovn`, and its CronJob fires at
02:00 UTC, so a devstack brought up during the day shows the reconcile series
first. Trigger a run by hand to fill the pair on the spot:
[Restore an OVN Database Snapshot](./restore-an-ovn-database-snapshot.md) shows
the `kubectl create job --from=cronjob/controlplane-ovn-backup` invocation.

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
chainsaw test --test-dir tests/e2e/ovn-operator/metrics
```
