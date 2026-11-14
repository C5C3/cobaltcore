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
[Horizon Reconciler Architecture](../../reference/horizon/horizon-reconciler.md).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_PROMETHEUS=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the horizon-operator
(namespace `horizon-system`) is running with kube-prometheus-stack scraping it.
:::

Run the commands from a workstation with `kubectl`, `curl`, and `jq`; use
`helm` for the direct Helm path. For a non-kind installation, install the
prometheus-operator CRDs and configure Prometheus's `serviceMonitorSelector`
to match the ServiceMonitor's `app.kubernetes.io/name=horizon-operator` label
and `serviceMonitorNamespaceSelector` to include `horizon-system`. A selector
restricted to a `release` label will not match this ServiceMonitor. If the
operator chart's NetworkPolicy is enabled, allow Prometheus traffic to the
metrics port with `networkPolicy.allowMetricsFrom`; see the
[Horizon operator NetworkPolicy guide](./enable-horizon-operator-networkpolicy.md#step-1-enable-the-policy).

## Step 1: Enable the ServiceMonitor

On the ControlPlane devstack, `WITH_PROMETHEUS=true` installs Prometheus and
Grafana and sets `monitoring.serviceMonitor.enabled=true` on the
horizon-operator HelmRelease. `WITH_CONTROLPLANE=true` resumes that release,
so the ServiceMonitor is already enabled. Skip the patch below and confirm
the object. Use the patch for an existing installation without monitoring.

On the tutorial devstacks the `horizon-operator` release is owned by Flux (a
`HelmRelease`), so set the chart value by patching that HelmRelease.
Flux's helm-controller reverts any out-of-band
Helm revision on its next reconcile. Use the patch and wait only when the
release is active. If it is suspended, the patch stores the value but renders
no ServiceMonitor; resume Flux only when it is the intended owner, or set the
value on the active chart installation. Do not wait on a suspended release.

```bash
kubectl patch helmrelease horizon-operator -n horizon-system --type=merge \
  -p '{"spec":{"values":{"monitoring":{"serviceMonitor":{"enabled":true}}}}}'

kubectl wait helmrelease/horizon-operator -n horizon-system \
  --for=condition=Ready --timeout=5m
```

Confirm the `ServiceMonitor` was rendered:

```bash
kubectl -n horizon-system get servicemonitor \
  -l app.kubernetes.io/name=horizon-operator
```

The chart renders a `ServiceMonitor` scraping the operator's metrics Service
on the `metrics` port over HTTP with the shared operator-library labels.

::: details Helm-managed installations (non-Flux)
If you installed the operator directly with Helm (not through Flux), set the
value with a rolling `helm upgrade` instead. Pin the upgrade to the installed
chart version to avoid changing chart versions while enabling the
ServiceMonitor:

```bash
CHART_VERSION=$(helm list -n horizon-system --filter '^horizon-operator$' -o json \
  | jq -r '.[0].chart | sub("^horizon-operator-"; "")')
helm upgrade horizon-operator oci://ghcr.io/c5c3/charts/horizon-operator \
  --namespace horizon-system --version "$CHART_VERSION" --reuse-values \
  --set monitoring.serviceMonitor.enabled=true
```

Do **not** run this on the tutorial devstacks: there the release is
Flux-owned, and the helm-controller reverts out-of-band revisions on its next
reconcile. Use the HelmRelease patch above instead.
:::

## Step 2: Import the Grafana dashboard

On the devstack, follow the
[Grafana access instructions](../keystone/enable-keystone-operator-metrics.md#_2-import-the-reference-grafana-dashboard)
to port-forward the Service, open <http://localhost:3000>, and retrieve the
login credentials. For an existing Grafana, use its configured URL and
credentials.

The reference dashboard ships in-repo at
`operators/horizon/dashboards/horizon-operator.json` (uid
`horizon-operator`): per-sub-reconciler duration quantiles, error rate per
condition type, and the controller-runtime end-to-end reconcile histogram.
Import it via the Grafana UI or provision it from a ConfigMap.

## Step 3: Verify the target {#step-3-—-verify-the-target}

Keep the port-forward running in a separate terminal:

```bash
kubectl -n monitoring port-forward svc/prometheus-operated 9090:9090
```

For an existing Prometheus, replace `monitoring`, `prometheus-operated`, and
the remote port with its namespace, Service, and HTTP API port. Keep the local
port at `9090` so the URL below still works. After Prometheus has had a scrape
interval to discover the ServiceMonitor, query the targets from another
terminal:

```bash
curl -fsS 'http://localhost:9090/api/v1/targets?state=active' \
  | jq '.data.activeTargets[] | select(.labels.job == "horizon-operator") | {health, lastScrape, lastError}'
```

Expect `health` to be `"up"`. If no target appears, check the Prometheus
selectors and NetworkPolicy; if `health` is `"down"`, inspect `lastError`.
Then confirm the series exist:

```bash
curl -fsS -G --data-urlencode \
  'query=horizon_operator_reconcile_duration_seconds_count' \
  'http://localhost:9090/api/v1/query' \
  | jq '.data.result | length'
```

A non-zero result count means the operator has reconciled at least one
Horizon CR since the scrape began.

## Tested by

The chart's ServiceMonitor render-and-remove lifecycle is asserted on the CI
e2e kind cluster by the chainsaw suite below (install with
`monitoring.serviceMonitor.enabled=true`, assert the `ServiceMonitor` shape,
uninstall, assert removal). That cluster has only the prometheus-operator
CRDs installed.

```bash
chainsaw test --test-dir tests/e2e/horizon-operator/metrics
```

The end-to-end scrape path runs in the `e2e-prometheus` CI job, which deploys
the horizon-operator with its ServiceMonitor into `horizon-system` next to a
live kube-prometheus-stack. Its suite asserts that the ServiceMonitor exists,
that every `horizon-operator` target is up, and that Prometheus ingested a
`controller_runtime_reconcile_total` series of that job:

```bash
chainsaw test --test-dir tests/e2e/horizon/prometheus-stack
```
