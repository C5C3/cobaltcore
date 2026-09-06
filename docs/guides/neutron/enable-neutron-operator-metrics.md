---
title: Enable the Neutron Operator Metrics Endpoint
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

<!-- operator namespace is `neutron-system`; workload (Neutron, NeutronMetadataAgent) stays in `openstack`. -->

# How-to: Enable the Neutron Operator Metrics Endpoint

This guide walks an operator through turning on the Prometheus ServiceMonitor
shipped with the `neutron-operator` Helm chart, importing the reference Grafana
dashboard, and verifying that scrape targets transition to `Up`.

The neutron-operator emits the shared sub-reconciler instrumentation under the
`neutron_operator` prefix, plus four per-CR collectors covering the two
Job-driven sync paths:

| Metric | Type | Labels |
| --- | --- | --- |
| `neutron_operator_reconcile_duration_seconds` | histogram | `sub_reconciler` |
| `neutron_operator_reconcile_errors_total` | counter | `sub_reconciler`, `condition_type` |
| `neutron_operator_db_sync_total` | counter | `neutron`, `namespace`, `result` |
| `neutron_operator_db_sync_duration_seconds` | histogram | `neutron`, `namespace` |
| `neutron_operator_ovn_db_sync_total` | counter | `neutron`, `namespace`, `result` |
| `neutron_operator_ovn_db_sync_duration_seconds` | histogram | `neutron`, `namespace` |

One operator serves two kinds, so `sub_reconciler` carries sixteen values:
`Secrets`, `DBConnectionSecret`, `TransportURLSecret`, `Config`,
`OVNEndpoints`, `OVNClientSecret`, `Database`, `OVNDBSync`, `Deployment`,
`Workers`, `HTTPRoute`, `HealthCheck`, `HPA` and `NetworkPolicy` from the
`Neutron` pipeline, `Chassis` and `DaemonSet` from the `NeutronMetadataAgent`
one, which reuses `Secrets` and `Config`. The two sync pairs carry the CR name
and its namespace in place of a sub-reconciler: they measure Jobs, which is work
that happens outside a reconcile.

For the controller-side contract (which sub-reconciler drives which condition),
see [Neutron Reconciler Architecture](../../reference/neutron/neutron-reconciler.md).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_PROMETHEUS=true make deploy-infra
```

Follow that tutorial through to the **Create a first network** check in its
Step 6, so the neutron-operator (namespace `neutron-system`) is running with
kube-prometheus-stack scraping it.
:::

1. A running `neutron-operator` Helm release (namespace `neutron-system`).
2. The prometheus-operator CRDs (`servicemonitors.monitoring.coreos.com`)
   installed, and a Prometheus whose `serviceMonitorSelector` covers the
   operator namespace.
3. For the db-sync series in Step 3, a `Neutron` the operator has already
   reconciled. On the ControlPlane devstack that is `controlplane-neutron`, the
   child the plane projects once the network service is declared.

On that devstack, Step 1 is already done for you. `WITH_PROMETHEUS=true` patches
`monitoring.serviceMonitor.enabled=true` onto the neutron-operator HelmRelease
at bring-up, and `WITH_CONTROLPLANE=true` un-suspends that HelmRelease
beforehand, so the value reconciles and the ServiceMonitor is rendered. Step 1
is the path for a devstack that is already running without `WITH_PROMETHEUS`,
and for non-kind clusters that run their own Prometheus. Without
`WITH_CONTROLPLANE=true` the release stays suspended, the patch is recorded but
inert, and the bring-up log says so.

## Step 1 — Enable the ServiceMonitor

On the tutorial devstacks the `neutron-operator` release is owned by Flux (a
`HelmRelease`), so set the chart value by patching that HelmRelease rather than
running a raw `helm upgrade`. Flux's helm-controller reverts any out-of-band
Helm revision on its next reconcile:

```bash
kubectl patch helmrelease neutron-operator -n neutron-system --type=merge \
  -p '{"spec":{"values":{"monitoring":{"serviceMonitor":{"enabled":true}}}}}'

kubectl wait helmrelease/neutron-operator -n neutron-system \
  --for=condition=Ready --timeout=5m
```

Confirm the `ServiceMonitor` was rendered:

```bash
kubectl -n neutron-system get servicemonitor \
  -l app.kubernetes.io/name=neutron-operator
```

The chart renders a `ServiceMonitor` scraping the operator's metrics Service on
the `metrics` port at `/metrics`, every 30 seconds by default
(`monitoring.serviceMonitor.interval`).

::: details Helm-managed installations (non-Flux)
If you installed the operator directly with Helm (not through Flux), set the
value with a rolling `helm upgrade` instead:

```bash
helm upgrade neutron-operator oci://ghcr.io/c5c3/charts/neutron-operator \
  --namespace neutron-system --reuse-values \
  --set monitoring.serviceMonitor.enabled=true
```

Do **not** run this on the tutorial devstacks: there the release is Flux-owned,
and the helm-controller reverts out-of-band revisions on its next reconcile. Use
the HelmRelease patch above instead.
:::

## Step 2 — Import the Grafana dashboard

The reference dashboard ships in-repo at
`operators/neutron/dashboards/neutron-operator.json` (uid `neutron-operator`).
Its six panels are the duration quantiles per sub-reconciler, the error rate per
condition type, db-sync duration p95 beside the failed-run rate per CR, the same
pair for the ovn-db-sync runs, both sync counters broken out per result, and the
controller-runtime end-to-end reconcile histogram filtered to the `neutron` and
`neutronmetadataagent` controllers. Import it via the Grafana UI or provision it
from a ConfigMap.

## Step 3 — Verify the target

```bash
kubectl -n <prometheus-namespace> port-forward svc/prometheus-operated 9090 &
curl -s 'http://localhost:9090/api/v1/targets' \
  | jq '.data.activeTargets[] | select(.labels.namespace == "neutron-system") | .health'
```

Expect `"up"`. Then confirm the series exist:

```bash
curl -s 'http://localhost:9090/api/v1/query?query=neutron_operator_reconcile_duration_seconds_count' \
  | jq '.data.result | length'
```

A non-zero result count means the operator has reconciled at least one `Neutron`
or `NeutronMetadataAgent` since the scrape began.

Both sync pairs stay empty until a Job terminates, and each fills from a
different event. The db-sync pair takes its sample when the `{name}-db-sync` Job
of a `Neutron` completes, which the ControlPlane devstack does once, while it
projects `controlplane-neutron`. The operator records each run once per Job UID
through an annotation on the CR, so an operator restart after that migration
leaves the pair empty until the next `Neutron` is created. The ovn-db-sync pair
needs a run of the `{name}-ovn-db-sync` CronJob, which exists only while
`spec.ovnDBSync` is set. Fill it on the spot by scheduling the sync and
triggering a run:
[Repair OVN Drift with db-sync](./repair-ovn-drift-with-db-sync.md) walks both,
including the `kubectl create job --from=cronjob/controlplane-neutron-ovn-db-sync`
invocation.

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
chainsaw test --test-dir tests/e2e/neutron-operator/metrics
```
