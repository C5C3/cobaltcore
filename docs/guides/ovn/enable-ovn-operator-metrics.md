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

One operator serves two kinds, so `sub_reconciler` carries sixteen values:
`TLS`, `Northbound`, `Southbound`, `Endpoints`, `Northd`, `Relay`, `Backup` and
`VPA` from the `OVNCentral` pipeline, `Central`, `ClientSecret`, `Nodes`, `OVS`,
`Controller`, `ChassisVPA`, `Maintenance` and `Fabric` from the `OVNChassis`
one. The backup metrics are labeled by CR name and namespace. After observing a
terminal backup Job, the operator records its result (`succeeded` or `failed`)
and duration; the Job itself runs outside a reconcile.

For the controller-side contract (which sub-reconciler drives which condition),
see [OVN Reconciler Architecture](../../reference/ovn/ovn-reconciler.md).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_PROMETHEUS=true make deploy-infra
```

Follow that tutorial through to the **Create a first network** check in its
Step 7, so the ovn-operator (namespace `ovn-system`) is running with
kube-prometheus-stack scraping it.
:::

For a non-kind installation, install the prometheus-operator CRDs and configure
Prometheus's `serviceMonitorSelector` to match the ServiceMonitor labels and
`serviceMonitorNamespaceSelector` to include `ovn-system`. If the OVN operator
NetworkPolicy is enabled, also allow Prometheus to reach the metrics port with
`networkPolicy.allowMetricsFrom`; see
[Enable the OVN Operator NetworkPolicy](./enable-ovn-operator-networkpolicy.md).
The backup series in Step 3 requires a reconciled `OVNCentral`. On the
devstack that is `controlplane-ovn`, applied before the `ControlPlane` CR.

## Step 1: Enable the ServiceMonitor

On that devstack, Step 1 is already done for you. `WITH_PROMETHEUS=true` patches
`monitoring.serviceMonitor.enabled=true` onto the ovn-operator HelmRelease at
bring-up, and `WITH_CONTROLPLANE=true` un-suspends that HelmRelease beforehand,
so the value reconciles and the ServiceMonitor is rendered. Step 1 is the path
for a devstack that is already running without `WITH_PROMETHEUS`, and for
non-kind clusters that run their own Prometheus. Without
`WITH_CONTROLPLANE=true` the release stays suspended, the patch is recorded but
inert, and the bring-up log says so.

On the tutorial devstacks the `ovn-operator` release is owned by Flux (a
`HelmRelease`), so set the chart value by patching that HelmRelease.
Flux's helm-controller reverts any out-of-band
Helm revision on its next reconcile:

```bash
set -euo pipefail
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
set -euo pipefail
CHART_VERSION="$(helm list -n ovn-system --filter '^ovn-operator$' -o json \
  | jq -r '.[0].chart // "" | sub("^ovn-operator-"; "")')"
if [ -z "$CHART_VERSION" ]; then
  echo "ERROR: could not find the ovn-operator Helm release in ovn-system." >&2
  exit 1
fi
helm upgrade ovn-operator oci://ghcr.io/c5c3/charts/ovn-operator \
  --namespace ovn-system --version "$CHART_VERSION" --reuse-values \
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
`operators/ovn/dashboards/ovn-operator.json` (uid `ovn-operator`). Its six
panels are the duration quantiles per sub-reconciler, the error rate per
condition type, backup duration p95 beside the failed-run rate per CR, backup
runs per result, reconcile errors per sub-reconciler, and the controller-runtime
end-to-end reconcile histogram filtered to the `ovncentral` and `ovnchassis`
controllers. Import it via the Grafana UI or provision it from a ConfigMap.

## Step 3: Verify the target {#step-3-—-verify-the-target}

Keep the port-forward running in a separate terminal:

```bash
kubectl -n monitoring port-forward svc/prometheus-operated 9090:9090
```

For an existing Prometheus, replace `monitoring` with its namespace and
`prometheus-operated` with its Prometheus Service name.
In another terminal, query the targets:

```bash
set -euo pipefail
curl --fail --silent --show-error \
  'http://localhost:9090/api/v1/targets' \
  | jq -e '.data.activeTargets[] | select(.labels.namespace == "ovn-system") | .health'
```

Expect `"up"`. Then confirm the series exist:

```bash
set -euo pipefail
curl --fail --silent --show-error \
  'http://localhost:9090/api/v1/query?query=ovn_operator_reconcile_duration_seconds_count' \
  | jq -e '.data.result | length'
```

A non-zero result count means the operator has reconciled at least one
`OVNCentral` or `OVNChassis` since the scrape began. The backup pair stays empty
until a backup Job reaches a terminal state for `controlplane-ovn`. Its default
CronJob schedule is `0 2 * * *` (daily at 02:00), so a devstack brought up
during the day shows the reconcile series first. Before triggering a manual run,
check `kubectl get jobs -n openstack` and wait for any active backup to finish:
a manually created Job does not honor the CronJob's `Forbid` policy and could
write the shared volume concurrently. See the
[Restore an OVN Database Snapshot](./restore-an-ovn-database-snapshot.md)
guide for the invocation.

## Tested by

The chart's ServiceMonitor render-and-remove lifecycle is asserted on the CI e2e
kind cluster by the chainsaw suite below (install with
`monitoring.serviceMonitor.enabled=true`, assert the `ServiceMonitor` shape,
uninstall, assert removal). The `WITH_PROMETHEUS=true` kind bring-up in
Step 1 provides a live Prometheus that discovers the ServiceMonitor and
marks the target Up. This suite checks the chart lifecycle on a cluster
with only the prometheus-operator CRDs installed.

```bash
chainsaw test --test-dir tests/e2e/ovn-operator/metrics
```
