---
title: Enable the Barbican Operator Metrics Endpoint
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

<!-- operator namespace is `barbican-system`; workload (Barbican CR) stays in `openstack`. -->

# How-to: Enable the Barbican Operator Metrics Endpoint

This guide walks an operator through turning on the Prometheus ServiceMonitor
shipped with the `barbican-operator` Helm chart, importing the reference Grafana
dashboard, and verifying that scrape targets transition to `Up`.

The barbican-operator emits the shared sub-reconciler instrumentation under the
`barbican_operator` prefix, plus per-CR collectors covering the schema migration,
the recurring clean-up, and the secret store's AppRole rotation:

| Metric | Type | Labels |
| --- | --- | --- |
| `barbican_operator_reconcile_duration_seconds` | histogram | `sub_reconciler` |
| `barbican_operator_reconcile_errors_total` | counter | `sub_reconciler`, `condition_type` |
| `barbican_operator_db_sync_total` | counter | `barbican`, `namespace`, `result` |
| `barbican_operator_db_sync_duration_seconds` | histogram | `barbican`, `namespace` |
| `barbican_operator_db_clean_total` | counter | `barbican`, `namespace`, `result` |
| `barbican_operator_db_clean_duration_seconds` | histogram | `barbican`, `namespace` |
| `barbican_operator_secretstore_remints_total` | counter | `store`, `namespace`, `trigger` |

The clean-up pair has no counterpart on the other service operators. Barbican
never hard-deletes: deleting a secret, container, or order flips its row to
deleted, so every Barbican gets a `{name}-db-clean` CronJob and the counter and
histogram track its terminal runs.

`barbican_operator_secretstore_remints_total` is keyed on the
`BarbicanSecretStore` CR rather than on its parent Barbican, and its `trigger`
label carries `proactive` when the operator refreshed the AppRole secret ID
before its TTL lapsed and `reactive` when the credential had already been
rejected. The split is what tells a rotation schedule that runs on time from one
that keeps arriving late.

For the controller-side contract (which sub-reconciler drives which condition),
see
[Barbican Reconciler Architecture](../../reference/barbican/barbican-reconciler.md).

::: tip On kind
If you are running the kind ControlPlane Quick Start, the prometheus-operator
CRDs, Prometheus, and Grafana are already wrapped behind opt-in flags:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_PROMETHEUS=true make deploy-infra
```

`WITH_PROMETHEUS=true` also flips the barbican-operator `ServiceMonitor` for you
at bring-up (`deploy-infra` patches the barbican-operator HelmRelease), so on a
fresh kind devstack none of the manual steps below are required. Step 1 is the
path for a devstack that is **already running** without `WITH_PROMETHEUS`, or for
non-kind clusters that run their own Prometheus.
:::

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_PROMETHEUS=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the barbican-operator
(namespace `barbican-system`) is running with kube-prometheus-stack scraping it.
:::

1. A running `barbican-operator` Helm release (namespace `barbican-system`).
2. The prometheus-operator CRDs (`servicemonitors.monitoring.coreos.com`)
   installed, and a Prometheus whose `serviceMonitorSelector` covers the
   operator namespace.
3. For the per-CR series in Step 3, a `Barbican` CR the operator has already
   reconciled. On the ControlPlane devstack that is the projected
   `controlplane-barbican` child, which the ControlPlane only projects when it
   declares `spec.services.barbican`. See
   [Run Barbican on a Dedicated OpenBao](./barbican-dedicated-openbao.md).

## Step 1 — Enable the ServiceMonitor

On the tutorial devstacks the `barbican-operator` release is owned by Flux (a
`HelmRelease`), so set the chart value by patching that HelmRelease rather than
running a raw `helm upgrade`. Flux's helm-controller reverts any out-of-band
Helm revision on its next reconcile:

```bash
kubectl patch helmrelease barbican-operator -n barbican-system --type=merge \
  -p '{"spec":{"values":{"monitoring":{"serviceMonitor":{"enabled":true}}}}}'

kubectl wait helmrelease/barbican-operator -n barbican-system \
  --for=condition=Ready --timeout=5m
```

Confirm the `ServiceMonitor` was rendered:

```bash
kubectl -n barbican-system get servicemonitor \
  -l app.kubernetes.io/name=barbican-operator
```

The chart renders a `ServiceMonitor` scraping the operator's metrics Service on
the `https`-less metrics port with the shared operator-library labels, at the
`monitoring.serviceMonitor.interval` the values file defaults to 30s.

::: details Helm-managed installations (non-Flux)
If you installed the operator directly with Helm (not through Flux), set the
value with a rolling `helm upgrade` instead:

```bash
helm upgrade barbican-operator oci://ghcr.io/c5c3/charts/barbican-operator \
  --namespace barbican-system --reuse-values \
  --set monitoring.serviceMonitor.enabled=true
```

Do **not** run this on the tutorial devstacks: there the release is Flux-owned,
and the helm-controller reverts out-of-band revisions on its next reconcile. Use
the HelmRelease patch above instead.
:::

## Step 2 — Import the Grafana dashboard

The reference dashboard ships in-repo at
`operators/barbican/dashboards/barbican-operator.json` (uid `barbican-operator`):
per-sub-reconciler duration quantiles, error rate per condition type, db-sync and
db-clean p95 with their failure rates, secret-store AppRole re-mints per trigger,
and the controller-runtime end-to-end reconcile histogram. Import it via the
Grafana UI or provision it from a ConfigMap.

## Step 3 — Verify the target

```bash
kubectl -n <prometheus-namespace> port-forward svc/prometheus-operated 9090 &
curl -s 'http://localhost:9090/api/v1/targets' \
  | jq '.data.activeTargets[] | select(.labels.namespace == "barbican-system") | .health'
```

Expect `"up"`. Then confirm the series exist:

```bash
curl -s 'http://localhost:9090/api/v1/query?query=barbican_operator_reconcile_duration_seconds_count' \
  | jq '.data.result | length'
```

A non-zero result count means the operator has reconciled at least one Barbican
CR since the scrape began. The per-CR series fill in later and on their own
schedules: db-sync publishes when the migration Job terminates, db-clean when the
CronJob's first run does, and the re-mint counter only once a managed store
actually rotates its secret ID. A freshly created CR therefore shows the
reconcile series first and the rest as its jobs complete.

## Tested by

The chart's ServiceMonitor render-and-remove lifecycle is asserted on the CI e2e
kind cluster by the chainsaw suite below (install with
`monitoring.serviceMonitor.enabled=true`, assert the `ServiceMonitor` shape,
uninstall, assert removal). The end-to-end scrape path (a live Prometheus that
discovers the ServiceMonitor and marks the target Up) is the
`WITH_PROMETHEUS=true` kind bring-up shown in the tip above, not this suite: the
e2e cluster ships only the prometheus-operator CRDs, not a Prometheus instance.

```bash
chainsaw test --test-dir tests/e2e/barbican-operator/metrics
```
