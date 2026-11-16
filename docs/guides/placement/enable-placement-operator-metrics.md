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

The placement-operator emits shared sub-reconciler metrics under the
`placement_operator` prefix, plus per-CR metrics for the database sync Job:

| Metric | Type | Labels |
| --- | --- | --- |
| `placement_operator_reconcile_duration_seconds` | histogram | `sub_reconciler` |
| `placement_operator_reconcile_errors_total` | counter | `sub_reconciler`, `condition_type` |
| `placement_operator_db_sync_total` | counter | `placement`, `namespace`, `result` |
| `placement_operator_db_sync_duration_seconds` | histogram | `placement`, `namespace` |

The `sub_reconciler` label values are `Secrets`, `DBConnectionSecret`,
`Config`, `Database`, `Deployment`, `HTTPRoute`, `HealthCheck`, `HPA`, `VPA`
and `NetworkPolicy`. The db-sync `result` is `succeeded` or `failed`.

There is no purge pair here. Placement keeps no expiring rows of its own, so
the operator runs no recurring maintenance job to instrument.

For the controller-side contract (which sub-reconciler drives which
condition), see
[Placement Reconciler Architecture](../../reference/placement/placement-reconciler.md).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_PROMETHEUS=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the placement-operator
(namespace `placement-system`) is running with kube-prometheus-stack scraping it.
:::

For a non-kind installation, install the Prometheus Operator custom resource
definitions (CRDs). Configure Prometheus's `serviceMonitorSelector` to match
`app.kubernetes.io/name: placement-operator` and
`app.kubernetes.io/instance: placement-operator` (or your Helm release name),
and set `serviceMonitorNamespaceSelector` to include `placement-system`. If
the operator NetworkPolicy is enabled, also allow the Prometheus source with
`networkPolicy.allowMetricsFrom`; see
[Enable the Placement Operator NetworkPolicy](./enable-placement-operator-networkpolicy.md).
The db-sync series in Step 3 requires a reconciled `Placement` CR. On the
devstack that is `controlplane-placement`, projected when the ControlPlane
declares `spec.services.placement`.

## Step 1: Enable the ServiceMonitor

On the ControlPlane devstack, `WITH_PROMETHEUS=true` installs Prometheus and
Grafana and sets `monitoring.serviceMonitor.enabled=true` on the
placement-operator HelmRelease. `WITH_CONTROLPLANE=true` resumes that release,
so the ServiceMonitor is already enabled. Skip the patch below and confirm
the object.

On other Flux-managed installations without monitoring, set the chart value
by patching the `HelmRelease`.
Flux's helm-controller reverts any out-of-band
Helm revision on its next reconcile:

```bash
set -euo pipefail
kubectl patch helmrelease placement-operator -n placement-system --type=merge \
  -p '{"spec":{"values":{"monitoring":{"serviceMonitor":{"enabled":true}}}}}'

SUSPENDED="$(kubectl get helmrelease placement-operator -n placement-system \
  -o jsonpath='{.spec.suspend}')"
if [ "$SUSPENDED" = "true" ]; then
  echo "ERROR: HelmRelease is suspended; resume it through its deployment owner before waiting." >&2
  exit 1
fi

kubectl wait helmrelease/placement-operator -n placement-system \
  --for=condition=Ready --timeout=5m
```

If the HelmRelease is suspended, the patch is stored but no ServiceMonitor is
rendered. The command exits before waiting; resume the release through its
deployment owner, then rerun this step.

Confirm the `ServiceMonitor` was rendered:

```bash
kubectl -n placement-system get servicemonitor \
  -l app.kubernetes.io/name=placement-operator
```

The chart renders a `ServiceMonitor` scraping the operator's metrics Service
on the `metrics` port at `/metrics`, every 30 seconds by default
(`monitoring.serviceMonitor.interval`).

::: details Helm-managed installations (non-Flux)
If you installed the operator directly with Helm (not through Flux), set the
value with a rolling `helm upgrade` instead:

```bash
set -euo pipefail
CHART_VERSION="$(helm list -n placement-system --filter '^placement-operator$' -o json \
  | jq -r '.[0].chart // "" | sub("^placement-operator-"; "")')"
if [ -z "$CHART_VERSION" ]; then
  echo "ERROR: could not determine the installed placement-operator chart version." >&2
  exit 1
fi
helm upgrade placement-operator oci://ghcr.io/c5c3/charts/placement-operator \
  --namespace placement-system --version "$CHART_VERSION" --reuse-values \
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
`operators/placement/dashboards/placement-operator.json` (uid
`placement-operator`). Its panels show per-sub-reconciler duration quantiles,
error rate per condition type, db-sync p95 and failure rate per Placement CR,
and the controller-runtime end-to-end reconcile histogram. Import it via the
Grafana UI or provision it from a ConfigMap.

## Step 3: Verify the target {#step-3-—-verify-the-target}

Keep the port-forward running in a separate terminal:

```bash
kubectl -n monitoring port-forward svc/prometheus-operated 9090:9090
```

For an existing Prometheus, replace the namespace and Service name, and adjust
the port if needed. Keep the port-forward running in one terminal. In another,
query the targets:

```bash
set -euo pipefail
curl --fail --silent --show-error \
  'http://localhost:9090/api/v1/targets?state=active' \
  | jq -e '
      [.data.activeTargets[]? |
        select(.labels.namespace == "placement-system") |
        {health, lastScrape, lastError}] as $targets |
      if ($targets | length) == 0 then
        error("no Placement operator target; check Prometheus ServiceMonitor selectors")
      elif any($targets[]; .health != "up") then
        error("a Placement operator target is not up: " + ($targets | tojson))
      else $targets end'
```

The query fails if Prometheus has not selected a target or if one is down;
inspect `lastError` in the output. If no target appears, recheck both
ServiceMonitor selectors. If a target is down, confirm Prometheus can reach the
metrics port and that the NetworkPolicy allows its source. Then confirm the
reconcile series exists:

```bash
set -euo pipefail
curl --fail --silent --show-error \
  'http://localhost:9090/api/v1/query?query=placement_operator_reconcile_duration_seconds_count' \
  | jq -r '
      if .status != "success" then
        error("Prometheus query failed: " + (.error // "unknown error"))
      elif (.data.result | length) == 0 then
        error("no sub-reconciler observations have been recorded yet")
      else (.data.result | length) end'
```

A non-zero result count means the operator has recorded at least one
sub-reconciler observation. The db-sync pair stays empty until a db-sync Job
terminates and the controller observes it. A new `Placement` CR therefore
shows reconcile metrics first, then db-sync metrics after its Job completes.

## Tested by

The chart's ServiceMonitor render-and-remove lifecycle is asserted on the CI
e2e kind cluster by the chainsaw suite below (install with
`monitoring.serviceMonitor.enabled=true`, assert the `ServiceMonitor` shape,
uninstall, assert removal). The `WITH_PROMETHEUS=true` kind bring-up in
Step 1 provides a live Prometheus that discovers the ServiceMonitor and
marks the target Up. This suite checks the chart lifecycle on a cluster
with only the prometheus-operator CRDs installed.

```bash
chainsaw test --test-dir tests/e2e/placement-operator/metrics
```
