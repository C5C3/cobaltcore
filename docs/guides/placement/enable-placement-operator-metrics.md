---
title: Enable the Placement Operator Metrics Endpoint
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

<!-- operator namespace is `placement-system`; workload (Placement CR) stays in `openstack`. -->

# How-to: Enable the Placement Operator Metrics Endpoint

This guide walks an operator through turning on the Prometheus
ServiceMonitor shipped with the `placement-operator` Helm chart, importing the
reference Grafana dashboard, and verifying that scrape targets transition to
`Up`.

The placement-operator emits the shared sub-reconciler instrumentation under
the `placement_operator` prefix, plus per-CR collectors covering the schema
migration:

| Metric | Type | Labels |
| --- | --- | --- |
| `placement_operator_reconcile_duration_seconds` | histogram | `sub_reconciler` |
| `placement_operator_reconcile_errors_total` | counter | `sub_reconciler`, `condition_type` |
| `placement_operator_db_sync_total` | counter | `placement`, `namespace`, `result` |
| `placement_operator_db_sync_duration_seconds` | histogram | `placement`, `namespace` |

There is no purge pair here. Placement keeps no expiring rows of its own, so
the operator runs no recurring maintenance job to instrument.

For the controller-side contract (which sub-reconciler drives which
condition), see
[Placement Reconciler Architecture](../../reference/placement/placement-reconciler.md).

::: tip On kind
If you are running the kind ControlPlane Quick Start, the prometheus-operator
CRDs, Prometheus, and Grafana are already wrapped behind opt-in flags:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_PROMETHEUS=true make deploy-infra
```

`WITH_PROMETHEUS=true` also flips the placement-operator `ServiceMonitor` for
you at bring-up (`deploy-infra` patches the placement-operator HelmRelease), so
on a fresh kind devstack none of the manual steps below are required. Step 1
is the path for a devstack that is **already running** without
`WITH_PROMETHEUS`, or for non-kind clusters that run their own Prometheus.
:::

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_PROMETHEUS=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the placement-operator
(namespace `placement-system`) is running with kube-prometheus-stack scraping it.
:::

1. A running `placement-operator` Helm release (namespace `placement-system`).
2. The prometheus-operator CRDs (`servicemonitors.monitoring.coreos.com`)
   installed, and a Prometheus whose `serviceMonitorSelector` covers the
   operator namespace.
3. For the db-sync series in Step 3, a `Placement` CR the operator has already
   reconciled. On the ControlPlane devstack that is the projected
   `controlplane-placement` child, which the ControlPlane only projects when it
   declares `spec.services.placement`.

## Step 1 — Enable the ServiceMonitor

On the tutorial devstacks the `placement-operator` release is owned by Flux (a
`HelmRelease`), so set the chart value by patching that HelmRelease rather than
running a raw `helm upgrade`. Flux's helm-controller reverts any out-of-band
Helm revision on its next reconcile:

```bash
kubectl patch helmrelease placement-operator -n placement-system --type=merge \
  -p '{"spec":{"values":{"monitoring":{"serviceMonitor":{"enabled":true}}}}}'

kubectl wait helmrelease/placement-operator -n placement-system \
  --for=condition=Ready --timeout=5m
```

Confirm the `ServiceMonitor` was rendered:

```bash
kubectl -n placement-system get servicemonitor \
  -l app.kubernetes.io/name=placement-operator
```

The chart renders a `ServiceMonitor` scraping the operator's metrics Service
on the `https`-less metrics port with the shared operator-library labels.

::: details Helm-managed installations (non-Flux)
If you installed the operator directly with Helm (not through Flux), set the
value with a rolling `helm upgrade` instead:

```bash
helm upgrade placement-operator oci://ghcr.io/c5c3/charts/placement-operator \
  --namespace placement-system --reuse-values \
  --set monitoring.serviceMonitor.enabled=true
```

Do **not** run this on the tutorial devstacks: there the release is
Flux-owned, and the helm-controller reverts out-of-band revisions on its next
reconcile. Use the HelmRelease patch above instead.
:::

## Step 2 — Import the Grafana dashboard

The reference dashboard ships in-repo at
`operators/placement/dashboards/placement-operator.json` (uid
`placement-operator`): per-sub-reconciler duration quantiles, error rate per
condition type, db-sync p95 and failure rate per Placement CR, and the
controller-runtime end-to-end reconcile histogram. Import it via the Grafana UI
or provision it from a ConfigMap.

## Step 3 — Verify the target

```bash
kubectl -n <prometheus-namespace> port-forward svc/prometheus-operated 9090 &
curl -s 'http://localhost:9090/api/v1/targets' \
  | jq '.data.activeTargets[] | select(.labels.namespace == "placement-system") | .health'
```

Expect `"up"`. Then confirm the series exist:

```bash
curl -s 'http://localhost:9090/api/v1/query?query=placement_operator_reconcile_duration_seconds_count' \
  | jq '.data.result | length'
```

A non-zero result count means the operator has reconciled at least one
Placement CR since the scrape began. The db-sync pair stays empty until a
db-sync Job terminates, so a fresh CR shows the reconcile series first and the
migration series once the schema is in place.

## Tested by

The chart's ServiceMonitor render-and-remove lifecycle is asserted on the CI
e2e kind cluster by the chainsaw suite below (install with
`monitoring.serviceMonitor.enabled=true`, assert the `ServiceMonitor` shape,
uninstall, assert removal). The end-to-end scrape path (a live Prometheus
that discovers the ServiceMonitor and marks the target Up) is the
`WITH_PROMETHEUS=true` kind bring-up shown in the tip above, not this suite:
the e2e cluster ships only the prometheus-operator CRDs, not a Prometheus
instance.

```bash
chainsaw test --test-dir tests/e2e/placement-operator/metrics
```
