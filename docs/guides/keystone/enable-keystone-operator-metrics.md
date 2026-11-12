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
[Keystone Operator Prometheus Metrics](../../reference/keystone-operator-metrics.md).
For the controller-side instrumentation contract, see
[Keystone Reconciler: Metrics Instrumentation](../../reference/keystone/keystone-reconciler.md#metrics-instrumentation).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start](../../quick-start.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_PROMETHEUS=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the keystone-operator
and a Keystone CR are running.
:::

For a non-kind installation, use your own Prometheus and Grafana instances.
The production overlays (`deploy/bases/operators/`) omit kube-prometheus-stack.
Step 1 describes the CRD and selector requirements for that setup.

---

## Steps

### 1. Enable the ServiceMonitor via Helm

On the Quick Start devstack, `WITH_PROMETHEUS=true` installs Prometheus,
Grafana, and the bundled `Keystone Operator` dashboard, and sets
`monitoring.serviceMonitor.enabled=true` on the keystone-operator
HelmRelease. Once that release reconciles, the ServiceMonitor is already
enabled; skip the enable commands and confirm the object below.
If the HelmRelease is suspended, the value is stored but no ServiceMonitor
is rendered until the release is resumed or the chart is installed separately
with that value.

::: details Connecting an existing Prometheus (non-kind)
The operator chart does not install the prometheus-operator CRDs.
Install them before enabling the ServiceMonitor, using your cluster's
prometheus-operator CRD bundle or the `prometheus-operator-crds` chart.
For the ServiceMonitor CRD alone:

```bash
kubectl apply --server-side -f \
  https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/main/example/prometheus-operator-crd/monitoring.coreos.com_servicemonitors.yaml
```

Without that CRD, enabling the ServiceMonitor fails with
`no matches for kind "ServiceMonitor"`.

The Prometheus CR's `serviceMonitorNamespaceSelector` must include
`keystone-system`, and its `serviceMonitorSelector` must match the
ServiceMonitor's labels. A selector of `release: <prometheus-release-name>`
does not match the operator chart's labels. Adjust the selector or manage
an additional matching label on the ServiceMonitor. Check both selectors:

```bash
kubectl get prometheus -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\t"}{.spec.serviceMonitorSelector}{"\t"}{.spec.serviceMonitorNamespaceSelector}{"\n"}{end}'
```

Grafana needs a Prometheus data source with network access to that instance.
:::

The Quick Start manages the operator through a Flux `HelmRelease`.
If monitoring is not enabled, patch that resource; Flux's helm-controller
reverts out-of-band Helm changes on its next reconcile:

```bash
kubectl patch helmrelease keystone-operator -n keystone-system --type=merge \
  -p '{"spec":{"values":{"monitoring":{"serviceMonitor":{"enabled":true}}}}}'

kubectl wait helmrelease/keystone-operator -n keystone-system \
  --for=condition=Ready --timeout=5m
```

The scrape interval defaults to `30s`. To change it, set
`spec.values.monitoring.serviceMonitor.interval` on the HelmRelease.

::: details Helm-managed installations (non-Flux)
If you installed the operator directly with Helm, enable monitoring while
preserving the release's other values:

```bash
helm upgrade keystone-operator \
  oci://ghcr.io/c5c3/charts/keystone-operator \
  --namespace keystone-system --reuse-values \
  --set monitoring.serviceMonitor.enabled=true
```

Or merge this into your existing values file:

```yaml
# values.yaml
monitoring:
  serviceMonitor:
    enabled: true
    interval: 30s
```

Do not run a raw `helm upgrade` against an actively Flux-managed release.
Use the HelmRelease patch above instead.
:::

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

On the Quick Start devstack, open Grafana by keeping this command running
in a separate terminal:

```bash
kubectl -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80
```

Open <http://localhost:3000>. Retrieve the login credentials from the
chart-managed Secret in another terminal:

```bash
kubectl -n monitoring get secret kube-prometheus-stack-grafana \
  -o jsonpath='{.data.admin-user}' | base64 -d; echo
kubectl -n monitoring get secret kube-prometheus-stack-grafana \
  -o jsonpath='{.data.admin-password}' | base64 -d; echo
```

Use those credentials only for the local devstack. For a non-kind Grafana,
use its configured URL and credentials. See
[Quick Start (Extended): Grafana UI](../../quick-start-extended.md#step-4c-grafana-ui)
for the local stack's access and target checks.

On the devstack, select **Dashboards → Keystone Operator**: the dashboard is
already provisioned. If you use your own Grafana, import it as follows.

The repository ships a reference dashboard in
[`operators/keystone/dashboards/keystone-operator.json`](https://github.com/c5c3/cobaltcore/blob/main/operators/keystone/dashboards/keystone-operator.json)
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
```

Keep the port-forward running and scrape from another terminal:

```bash
curl -s http://localhost:8080/metrics \
  | grep -E '^# (TYPE|HELP) keystone_operator_'
```

Expected: `# TYPE` and `# HELP` lines for every metric in the
[reference catalogue](../../reference/keystone-operator-metrics.md), for
example:

```
# HELP keystone_operator_reconcile_duration_seconds Wall-clock duration of a Keystone sub-reconciler invocation, in seconds.
# TYPE keystone_operator_reconcile_duration_seconds histogram
```

If the Pod serves metrics but Prometheus does not scrape them, the
mismatch is likely in the `serviceMonitorSelector` (see Step 1).
Also check `serviceMonitorNamespaceSelector`, the ServiceMonitor's Service
selector, and network access from Prometheus to the metrics port.

---

## Security & network considerations

The operator's metrics endpoint is bound via the `--metrics-bind-address` flag
(default `:8080`), wired in
[`internal/common/bootstrap/manager.go`](https://github.com/c5c3/cobaltcore/blob/main/internal/common/bootstrap/manager.go)
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
  `networkPolicy.allowMetricsFrom`; see
  [Enable the Keystone Operator NetworkPolicy](./enable-keystone-operator-networkpolicy.md).
  Without it the ServiceMonitor renders successfully but every scrape
  times out and the target reports `Down`.
- **mTLS service mesh enforcement.** When a service mesh (Istio,
  Linkerd, Cilium service mesh) requires mTLS for in-cluster traffic,
  inject a sidecar that terminates mTLS in front of the operator's
  `:8080` endpoint, OR exempt the operator Pod from strict-mTLS
  enforcement for the metrics port.
- **Required TLS / authn at the controller-runtime level.** Changing the
  port is a chart value (`metrics.port`), but changing the scheme is not:
  if the threat model demands the operator itself serve metrics over HTTPS or
  enforce a token-based AuthN, switch the `metricsserver.Options` in
  `internal/common/bootstrap/manager.go` to the controller-runtime
  `SecureServing` configuration and bind a cert/key pair (for example by
  mounting a cert-manager-issued Secret on the operator Deployment). The chart
  does not ship this configuration. Implement it by changing the shared
  controller-runtime bootstrap; no per-operator chart override exists.

The metrics endpoint exposes **no credentials, secrets,
or per-tenant payloads**. It serves Prometheus collector samples described
in the
[reference catalogue](../../reference/keystone-operator-metrics.md), so
the default plain-HTTP exposure is appropriate for cluster-internal
scraping. The hardening guidance above applies only when external
policy demands a stricter posture.

---

## Disabling

For the Flux-managed devstack, disable the ServiceMonitor without uninstalling
the operator by patching its HelmRelease:

```bash
kubectl patch helmrelease keystone-operator -n keystone-system --type=merge \
  -p '{"spec":{"values":{"monitoring":{"serviceMonitor":{"enabled":false}}}}}'

kubectl wait helmrelease/keystone-operator -n keystone-system \
  --for=condition=Ready --timeout=5m
```

For a directly Helm-managed installation instead:

```bash
helm upgrade keystone-operator \
  oci://ghcr.io/c5c3/charts/keystone-operator \
  --namespace keystone-system \
  --reuse-values \
  --set monitoring.serviceMonitor.enabled=false
```

The operator continues to serve `/metrics` on port `8080`. Only the
ServiceMonitor (and therefore the Prometheus scrape) is removed.

---

## See also

- [Keystone Operator Prometheus Metrics](../../reference/keystone-operator-metrics.md): authoritative metric catalogue.
- [Keystone Reconciler: Metrics Instrumentation](../../reference/keystone/keystone-reconciler.md#metrics-instrumentation): how sub-reconcilers are instrumented.
- [Observability & Diagnostics](../observability.md): conditions, events, and logs.
- [`operators/keystone/dashboards/keystone-operator.json`](https://github.com/c5c3/cobaltcore/blob/main/operators/keystone/dashboards/keystone-operator.json): reference Grafana dashboard.

## Tested by

The chart's ServiceMonitor render-and-remove lifecycle is asserted on the CI
e2e kind cluster by `tests/e2e/keystone/metrics`; the end-to-end
`WITH_PROMETHEUS` scrape-and-dashboard path is asserted by
`tests/e2e/keystone/prometheus-stack`:

```bash
chainsaw test --test-dir tests/e2e/keystone/metrics
chainsaw test --test-dir tests/e2e/keystone/prometheus-stack
```
