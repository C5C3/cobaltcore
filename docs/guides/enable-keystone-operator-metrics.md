---
title: Enable the Keystone Operator Metrics Endpoint
quadrant: operator
---

<!-- operator namespace is `keystone-system`; workload (Keystone CR) stays in `openstack`. -->

# How-to: Enable the Keystone Operator Metrics Endpoint

This guide walks an operator through turning on the Prometheus
ServiceMonitor shipped with the `keystone-operator` Helm chart, importing the reference Grafana dashboard, and
verifying that scrape targets transition to `Up`.

For the authoritative metric catalogue (names, labels, buckets), see
[Keystone Operator Prometheus Metrics](../reference/keystone-operator-metrics.md).
For the controller-side instrumentation contract, see
[Keystone Reconciler — Metrics Instrumentation](../reference/keystone/keystone-reconciler.md#metrics-instrumentation).

::: tip On kind
If you are running the kind Quick Start, the prometheus-operator CRDs,
Prometheus, Grafana, and the bundled `Keystone Operator` dashboard are
already wrapped behind a single opt-in flag — none of the manual steps
below are required:

```bash
WITH_PROMETHEUS=true make deploy-infra
```

`make deploy-infra` will install `kube-prometheus-stack` from the kind
overlay (`deploy/kind/prometheus/`), patch the keystone-operator
HelmRelease to flip `monitoring.serviceMonitor.enabled=true`, and wait
for the scrape targets to come up. See
[Extended Quick Start — Step 4c](../quick-start-extended.md#step-4c-grafana-ui)
for the port-forward, default credentials, and the Prometheus targets
sanity-check.

The remainder of this guide is the **canonical non-kind path** —
production overlays (`deploy/flux-system/`) deliberately omit the
stack so production clusters can wire their own Prometheus, and the
sections below cover that wiring end-to-end.
:::

---

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start](../quick-start.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_PROMETHEUS=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the keystone-operator
is running with kube-prometheus-stack scraping it. (On kind the `WITH_PROMETHEUS`
opt-in above wires the manual steps below automatically — see the tip above.)
:::

- A cluster with the `keystone-operator` Helm release installed and the
  operator Pod healthy (`Ready` condition `True`).
- **prometheus-operator CRDs installed.** The chart does *not* install
  `monitoring.coreos.com/v1` CRDs; attempting to render the
  ServiceMonitor without them results in a `no matches for kind
  "ServiceMonitor"` error from `kubectl apply`. Install via the upstream
  bundle or the `prometheus-operator-crds` chart:

  ```bash
  kubectl apply --server-side -f \
    https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/main/example/prometheus-operator-crd/monitoring.coreos.com_servicemonitors.yaml
  ```

- A running Prometheus instance whose `serviceMonitorSelector` matches
  the operator chart's release labels (the default
  `kube-prometheus-stack` selector of
  `release: <prometheus-release-name>` will **not** pick up the
  operator ServiceMonitor unless you either add that label or widen the
  selector). Check the Prometheus CR:

  ```bash
  kubectl get prometheus -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\t"}{.spec.serviceMonitorSelector}{"\n"}{end}'
  ```

- A running Grafana instance with network access to the Prometheus
  data source above.

---

## Steps

### 1. Enable the ServiceMonitor via Helm

Set `monitoring.serviceMonitor.enabled=true` (the interval defaults to
`30s`; override only if your retention budget requires a different
cadence):

```bash
helm upgrade --install keystone-operator \
  oci://ghcr.io/c5c3/charts/keystone-operator \
  --namespace keystone-system \
  --set monitoring.serviceMonitor.enabled=true \
  --set monitoring.serviceMonitor.interval=30s
```

Or equivalently in a values file:

```yaml
# values.yaml
monitoring:
  serviceMonitor:
    enabled: true
    interval: 30s
```

Confirm the ServiceMonitor object was created:

```bash
kubectl -n keystone-system get servicemonitor \
  -l app.kubernetes.io/name=keystone-operator
```

Expected:

```
NAME                AGE
keystone-operator   3s
```

### 2. Import the reference Grafana dashboard

The repository ships a reference dashboard in
[`operators/keystone/dashboards/keystone-operator.json`](https://github.com/c5c3/forge/blob/main/operators/keystone/dashboards/keystone-operator.json)
covering the four core SLIs: reconcile p95 per sub-reconciler, error
rate per condition type, rotation age per key, and `db_sync` duration
p95 with failure count.

1. In Grafana: **Dashboards → New → Import**.
2. Upload `keystone-operator.json` or paste its contents.
3. Select the Prometheus data source that scrapes the operator.
4. Click **Import**.

---

## Verification

### Scrape target is Up

Port-forward Prometheus and check the target list:

```bash
kubectl -n monitoring port-forward svc/prometheus-operated 9090:9090
```

`prometheus-operated` is the headless Service the prometheus-operator always
creates; if you run the kube-prometheus-stack the release-named
`kube-prometheus-stack-prometheus` Service works too. Open
<http://localhost:9090/targets> and filter for `keystone-operator`; the target
must report `State=UP` and `Last Scrape` within the configured interval.

Equivalent API query:

```bash
curl -s 'http://localhost:9090/api/v1/targets?state=active' \
  | jq '.data.activeTargets[] | select(.labels.job | test("keystone-operator")) | {health, lastScrape, lastError}'
```

### Sample PromQL queries

A handful of queries that should return data once the operator has run
at least one reconcile cycle:

```promql
# Every sub-reconciler ran at least once in the last 5 minutes
sum by (sub_reconciler) (
  rate(keystone_operator_reconcile_duration_seconds_count[5m])
) > 0
```

```promql
# p95 latency per sub-reconciler
histogram_quantile(
  0.95,
  sum by (sub_reconciler, le) (
    rate(keystone_operator_reconcile_duration_seconds_bucket[5m])
  )
)
```

```promql
# Any Keystone CR whose Fernet rotation is older than 7 days
keystone_operator_key_rotation_age_seconds{key_type="fernet"} > (7 * 86400)
```

### Raw `/metrics` scrape

If Prometheus is not reporting metrics, rule out the endpoint first by
scraping the operator Pod directly. Port-forward the metrics port
(default `8080`):

```bash
kubectl -n keystone-system port-forward \
  deployment/keystone-operator 8080:8080
curl -s http://localhost:8080/metrics \
  | grep -E '^# (TYPE|HELP) keystone_operator_'
```

Expected: `# TYPE` and `# HELP` lines for every metric in the
[reference catalogue](../reference/keystone-operator-metrics.md), for
example:

```
# HELP keystone_operator_reconcile_duration_seconds Wall-clock duration of a Keystone sub-reconciler invocation, in seconds.
# TYPE keystone_operator_reconcile_duration_seconds histogram
```

If the Pod serves metrics but Prometheus does not scrape them, the
mismatch is in the `serviceMonitorSelector` (see Prerequisites).

---

## Security & network considerations

The operator's metrics endpoint is bound via the `--metrics-bind-address` flag
(default `:8080`), wired in
[`internal/common/bootstrap/manager.go`](https://github.com/c5c3/forge/blob/main/internal/common/bootstrap/manager.go)
as `metricsserver.Options{BindAddress: metricsAddr}`. The chart passes the flag from
the `metrics.port` value (rendered as `--metrics-bind-address=:<metrics.port>`), so the
*port* is configurable without a code change. What is **not** configurable from the chart is the
scheme: the endpoint serves **plain HTTP with no authentication and no TLS** on every
Pod network interface. It is intended for cluster-internal scraping by a Prometheus
instance that already lives on the same trust boundary (typical kube-prometheus-stack /
namespaced Prometheus deployments satisfy this).

Operators with stricter cluster policies must take extra steps:

- **Plain-HTTP NetworkPolicy denial.** Clusters that block plain HTTP
  east-west traffic by default need an explicit ingress
  NetworkPolicy permitting the Prometheus Pod's selector to reach the
  operator on port `8080`. The operator chart ships this as
  `networkPolicy.allowMetricsFrom` — see
  [Enable the Keystone Operator NetworkPolicy](./enable-keystone-operator-networkpolicy.md).
  Without it the ServiceMonitor renders successfully but every scrape
  times out and the target reports `Down`.
- **mTLS service mesh enforcement.** When a service mesh (Istio,
  Linkerd, Cilium service mesh) requires mTLS for in-cluster traffic,
  inject a sidecar that terminates mTLS in front of the operator's
  `:8080` endpoint, OR exempt the operator Pod from strict-mTLS
  enforcement for the metrics port.
- **Required TLS / authn at the controller-runtime level.** Changing the
  *port* is a chart value (`metrics.port`), but changing the *scheme* is not:
  if the threat model demands the operator itself serve metrics over HTTPS or
  enforce a token-based AuthN, switch the `metricsserver.Options` in
  `internal/common/bootstrap/manager.go` to the controller-runtime
  `SecureServing` configuration and bind a cert/key pair (for example by
  mounting a cert-manager-issued Secret on the operator Deployment). The chart
  does not ship this configuration — it is a forge-wide bootstrap change rather
  than a per-operator override.

The metrics endpoint deliberately exposes **no credentials, secrets,
or per-tenant payloads** — only Prometheus collector samples described
in the
[reference catalogue](../reference/keystone-operator-metrics.md) — so
the default plain-HTTP exposure is appropriate for cluster-internal
scraping. The hardening guidance above applies only when external
policy demands a stricter posture.

---

## Disabling

To disable the ServiceMonitor without uninstalling the operator:

```bash
helm upgrade keystone-operator \
  oci://ghcr.io/c5c3/charts/keystone-operator \
  --namespace keystone-system \
  --reuse-values \
  --set monitoring.serviceMonitor.enabled=false
```

The operator continues to serve `/metrics` on port `8080` — only the
ServiceMonitor (and therefore the Prometheus scrape) is removed.

---

## See also

- [Keystone Operator Prometheus Metrics](../reference/keystone-operator-metrics.md) — authoritative metric catalogue.
- [Keystone Reconciler — Metrics Instrumentation](../reference/keystone/keystone-reconciler.md#metrics-instrumentation) — how sub-reconcilers are instrumented.
- [Observability & Diagnostics](../guides/observability.md) — conditions, events, and logs.
- [`operators/keystone/dashboards/keystone-operator.json`](https://github.com/c5c3/forge/blob/main/operators/keystone/dashboards/keystone-operator.json) — reference Grafana dashboard.

## Tested by

The chart's ServiceMonitor render-and-remove lifecycle is asserted on the CI
e2e kind cluster by `tests/e2e/keystone/metrics`; the end-to-end
`WITH_PROMETHEUS` scrape-and-dashboard path is asserted by
`tests/e2e/keystone/prometheus-stack`:

```bash
chainsaw test --test-dir tests/e2e/keystone/metrics
chainsaw test --test-dir tests/e2e/keystone/prometheus-stack
```
