---
title: Enable the Glance Operator Metrics Endpoint
quadrant: operator
---

<!-- operator namespace is `glance-system`; workload (Glance CR) stays in `openstack`. -->

# How-to: Enable the Glance Operator Metrics Endpoint

This guide walks an operator through turning on the Prometheus
ServiceMonitor shipped with the `glance-operator` Helm chart, importing the
reference Grafana dashboard, and verifying that scrape targets transition to
`Up`.

The glance-operator emits the shared sub-reconciler instrumentation under
the `glance_operator` prefix, plus per-CR collectors covering the schema
migration and the recurring database purge:

| Metric | Type | Labels |
| --- | --- | --- |
| `glance_operator_reconcile_duration_seconds` | histogram | `sub_reconciler` |
| `glance_operator_reconcile_errors_total` | counter | `sub_reconciler`, `condition_type` |
| `glance_operator_db_sync_total` | counter | `glance`, `namespace`, `result` |
| `glance_operator_db_sync_duration_seconds` | histogram | `glance`, `namespace` |
| `glance_operator_db_purge_total` | counter | `glance`, `namespace`, `result` |
| `glance_operator_db_purge_duration_seconds` | histogram | `glance`, `namespace` |

For the controller-side contract (which sub-reconciler drives which
condition), see
[Glance Reconciler Architecture](../../reference/glance/glance-reconciler.md).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_PROMETHEUS=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the glance-operator
(namespace `glance-system`) is running with kube-prometheus-stack scraping it.
:::

Run the commands from a workstation with `kubectl`, `curl`, and `jq`; use
`helm` for the direct Helm path. For a non-kind installation, install the
prometheus-operator CRDs and configure Prometheus's `serviceMonitorSelector`
to match the ServiceMonitor labels and `serviceMonitorNamespaceSelector` to
include `glance-system`. The chart labels the ServiceMonitor with
`app.kubernetes.io/name=glance-operator`; a selector restricted to a `release`
label will not match it.

## Step 1 — Enable the ServiceMonitor

On the ControlPlane devstack, `WITH_PROMETHEUS=true` installs Prometheus and
Grafana and sets `monitoring.serviceMonitor.enabled=true` on the
glance-operator HelmRelease. `WITH_CONTROLPLANE=true` resumes that release,
so the ServiceMonitor is already enabled. Skip the patch below and confirm
the object. Use the patch for an existing installation without monitoring.
If the HelmRelease is suspended, the value is stored but no ServiceMonitor
is rendered until it is resumed or the chart is installed separately with
that value.

On the tutorial devstacks the `glance-operator` release is owned by Flux (a
`HelmRelease`), so set the chart value by patching that HelmRelease.
Flux's helm-controller reverts any out-of-band
Helm revision on its next reconcile:

```bash
kubectl patch helmrelease glance-operator -n glance-system --type=merge \
  -p '{"spec":{"values":{"monitoring":{"serviceMonitor":{"enabled":true}}}}}'

kubectl wait helmrelease/glance-operator -n glance-system \
  --for=condition=Ready --timeout=5m
```

Confirm the `ServiceMonitor` was rendered:

```bash
kubectl -n glance-system get servicemonitor \
  -l app.kubernetes.io/name=glance-operator
```

The chart renders a `ServiceMonitor` scraping the operator's metrics Service
on the `metrics` port over HTTP with the shared operator-library labels. The
scrape interval is 30 seconds by default (`monitoring.serviceMonitor.interval`).

::: details Helm-managed installations (non-Flux)
If you installed the operator directly with Helm (not through Flux), set the
value with a rolling `helm upgrade` instead:

```bash
helm upgrade glance-operator oci://ghcr.io/c5c3/charts/glance-operator \
  --namespace glance-system --reuse-values \
  --set monitoring.serviceMonitor.enabled=true
```

Do **not** run this on the tutorial devstacks: there the release is
Flux-owned, and the helm-controller reverts out-of-band revisions on its next
reconcile. Use the HelmRelease patch above instead.
:::

## Step 2 — Import the Grafana dashboard

On the devstack, follow the
[Grafana access instructions](../keystone/enable-keystone-operator-metrics.md#_2-import-the-reference-grafana-dashboard)
to port-forward the Service, open <http://localhost:3000>, and retrieve the
login credentials. For an existing Grafana, use its configured URL and
credentials.

The reference dashboard ships in-repo at
`operators/glance/dashboards/glance-operator.json` (uid
`glance-operator`). Its panels show per-sub-reconciler duration quantiles,
error rate per condition type, db-sync duration and failure rate, and the
controller-runtime end-to-end reconcile histogram. The dashboard does not
chart the per-CR db-purge metrics listed above. Import it via the Grafana UI
or provision it from a ConfigMap.

## Step 3: Verify the target {#step-3-—-verify-the-target}

Keep the port-forward running in a separate terminal:

```bash
kubectl -n monitoring port-forward svc/prometheus-operated 9090:9090
```

For an existing Prometheus, replace `monitoring` with its namespace and
`prometheus-operated` and the remote port with the Service and port that expose
its HTTP API. Keep the local port at `9090` so the URL below still works.
After the ServiceMonitor has had a scrape interval to be discovered, query the
targets from another terminal:

```bash
curl -fsS 'http://localhost:9090/api/v1/targets?state=active' \
  | jq '.data.activeTargets[] | select(.labels.namespace == "glance-system") | {health, lastScrape, lastError}'
```

Expect `health` to be `"up"`. If no target appears, check that Prometheus
selects the ServiceMonitor; if `health` is `"down"`, inspect `lastError`.
Then confirm the series exist:

```bash
curl -s 'http://localhost:9090/api/v1/query?query=glance_operator_reconcile_duration_seconds_count' \
  | jq '.data.result | length'
```

A non-zero result count means the operator has reconciled at least one
Glance CR since the scrape began.

## Tested by

The Chainsaw suite below asserts the chart's ServiceMonitor render-and-remove
lifecycle on the CI e2e kind cluster. It checks the resource shape, labels,
selector, endpoint, and removal, but does not run Prometheus. The
`WITH_PROMETHEUS=true` devstack in Step 1 provides a live Prometheus for the
scrape-target check in Step 3.

```bash
chainsaw test --test-dir tests/e2e/glance-operator/metrics
```
