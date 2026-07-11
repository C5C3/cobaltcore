---
title: Enable the Horizon Operator Metrics Endpoint
quadrant: operator
---

<!-- operator namespace is `horizon-system`; workload (Horizon CR) stays in `openstack`. -->

# How-to: Enable the Horizon Operator Metrics Endpoint

This guide walks an operator through turning on the Prometheus
ServiceMonitor shipped with the `horizon-operator` Helm chart, importing the
reference Grafana dashboard, and verifying that scrape targets transition to
`Up`.

The horizon-operator emits the shared sub-reconciler instrumentation under
the `horizon_operator` prefix:

| Metric | Type | Labels |
| --- | --- | --- |
| `horizon_operator_reconcile_duration_seconds` | histogram | `sub_reconciler` |
| `horizon_operator_reconcile_errors_total` | counter | `sub_reconciler`, `condition_type` |

For the controller-side contract (which sub-reconciler drives which
condition), see
[Horizon Reconciler Architecture](../reference/horizon/horizon-reconciler.md).

::: tip On kind
If you are running the kind ControlPlane Quick Start, the prometheus-operator
CRDs, Prometheus, and Grafana are already wrapped behind opt-in flags:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_PROMETHEUS=true make deploy-infra
```

The rest of this guide is the canonical path for non-kind clusters that run
their own Prometheus.
:::

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_PROMETHEUS=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the horizon-operator
(namespace `horizon-system`) is running with kube-prometheus-stack scraping it.
:::

1. A running `horizon-operator` Helm release (namespace `horizon-system`).
2. The prometheus-operator CRDs (`servicemonitors.monitoring.coreos.com`)
   installed, and a Prometheus whose `serviceMonitorSelector` covers the
   operator namespace.

## Step 1 — Enable the ServiceMonitor

```bash
helm upgrade horizon-operator oci://ghcr.io/c5c3/charts/horizon-operator \
  --namespace horizon-system --reuse-values \
  --set monitoring.serviceMonitor.enabled=true
```

The chart renders a `ServiceMonitor` scraping the operator's metrics Service
on the `https`-less metrics port with the shared operator-library labels.

## Step 2 — Import the Grafana dashboard

The reference dashboard ships in-repo at
`operators/horizon/dashboards/horizon-operator.json` (uid
`horizon-operator`): per-sub-reconciler duration quantiles, error rate per
condition type, and the controller-runtime end-to-end reconcile histogram.
Import it via the Grafana UI or provision it from a ConfigMap.

## Step 3 — Verify the target

```bash
kubectl -n <prometheus-namespace> port-forward svc/prometheus-operated 9090 &
curl -s 'http://localhost:9090/api/v1/targets' \
  | jq '.data.activeTargets[] | select(.labels.namespace == "horizon-system") | .health'
```

Expect `"up"`. Then confirm the series exist:

```bash
curl -s 'http://localhost:9090/api/v1/query?query=horizon_operator_reconcile_duration_seconds_count' \
  | jq '.data.result | length'
```

A non-zero result count means the operator has reconciled at least one
Horizon CR since the scrape began.
