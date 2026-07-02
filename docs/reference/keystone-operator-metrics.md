---
title: Keystone Operator Prometheus Metrics
quadrant: operator
---

# Keystone Operator Prometheus Metrics

Reference catalogue for every Prometheus collector the Keystone operator
exposes on the controller-runtime metrics endpoint. Every metric name,
label set, and histogram bucket list below is authoritative. The
sub-reconciler duration and error metrics are verified by the tests in
[`internal/common/instrumentation`](https://github.com/c5c3/forge/blob/main/internal/common/instrumentation/instrumentation_test.go);
the per-CR collectors by
[`collectors_test.go`](https://github.com/c5c3/forge/blob/main/operators/keystone/internal/metrics/collectors_test.go);
and `dashboards/dashboard_test.go` fails the build if the bundled Grafana
dashboard references a metric the operator does not register.

The sub-reconciler duration/error pair
(`keystone_operator_reconcile_duration_seconds` and
`keystone_operator_reconcile_errors_total`) is shared with the other
forge operators. It lives in the
[`internal/common/instrumentation`](https://github.com/c5c3/forge/blob/main/internal/common/instrumentation/instrumentation.go)
package and is exposed as the `metrics.SubReconciler` instance, which
registers on the controller-runtime registry lazily on first use. The
per-CR collectors (rotation age and `db_sync`) register via the
process-wide `sync.Once` initializer `globalCollectors()` in
[`operators/keystone/internal/metrics/collectors.go`](https://github.com/c5c3/forge/blob/main/operators/keystone/internal/metrics/collectors.go).
All collectors attach to the controller-runtime registry
(`sigs.k8s.io/controller-runtime/pkg/metrics`) and are served on the
operator's metrics listener at `:8080/metrics` by default; see
[How to enable the Keystone operator metrics endpoint](../guides/enable-keystone-operator-metrics.md)
for cluster-side wiring.

## Metric summary

| Metric | Type | Labels | Purpose |
| --- | --- | --- | --- |
| `keystone_operator_reconcile_duration_seconds` | Histogram | `sub_reconciler` | Latency of each sub-reconciler invocation |
| `keystone_operator_reconcile_errors_total` | Counter | `sub_reconciler`, `condition_type` | Count of sub-reconciler failures per Ready sub-condition |
| `keystone_operator_key_rotation_age_seconds` | Gauge | `keystone`, `namespace`, `key_type` | Age of the most recent successful Fernet/credential/admin-password rotation |
| `keystone_operator_db_sync_total` | Counter | `keystone`, `namespace`, `result` | Terminal `db_sync` Job outcomes |
| `keystone_operator_db_sync_duration_seconds` | Histogram | `keystone`, `namespace` | Wall-clock duration of terminated `db_sync` Jobs |

---

## `keystone_operator_reconcile_duration_seconds`

Wall-clock duration of a single sub-reconciler invocation in seconds.

| Attribute | Value |
| --- | --- |
| Type | Histogram |
| Labels | `sub_reconciler` |
| Buckets (seconds) | `0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30` |
| Call site | `instrumentSubReconciler` in `operators/keystone/internal/controller/instrumentation.go` |

**Label cardinality.** The `sub_reconciler` value is drawn from a closed
set of 16 strings — the keys of `subReconcilerConditionTypes` — so the
series count is bounded regardless of how many Keystone CRs exist. The
metric deliberately omits a `keystone`/`namespace` label to keep
cardinality fleet-independent; per-CR attribution is
available via logs.

**Example PromQL.** p95 per sub-reconciler over the last 5 minutes:

```promql
histogram_quantile(
  0.95,
  sum by (sub_reconciler, le) (
    rate(keystone_operator_reconcile_duration_seconds_bucket[5m])
  )
)
```

---

## `keystone_operator_reconcile_errors_total`

Count of sub-reconciler errors, partitioned by the offending sub-reconciler
and the `status.conditions[]` type it failed to drive to `True`.

| Attribute | Value |
| --- | --- |
| Type | Counter |
| Labels | `sub_reconciler`, `condition_type` |
| Call site | `instrumentSubReconciler` (on non-nil error return) |

**Label cardinality.** Both labels are drawn from closed sets: the 16
sub-reconciler names and the 14 Ready sub-condition types listed in
`subConditionTypes`. The `sub_reconciler` → `condition_type` mapping is
one-to-one for most entries; the exceptions (`Secrets`,
`DBConnectionSecret`, `Config` all map to `SecretsReady`) collapse into
fewer distinct (sub_reconciler, condition_type) pairs in practice.

The counter is **not pre-created**; a (sub_reconciler, condition_type)
series only appears after its first error, so a healthy fleet emits no
series for this metric.

**Example PromQL.** Error rate per sub-reconciler over the last 5 minutes:

```promql
sum by (sub_reconciler) (
  rate(keystone_operator_reconcile_errors_total[5m])
)
```

---

## `keystone_operator_key_rotation_age_seconds`

Age in seconds of the most recent successful key rotation for a given
Keystone CR and key type, measured as `time.Since(rotation-completed-at)`
at reconcile time.

| Attribute | Value |
| --- | --- |
| Type | Gauge |
| Labels | `keystone`, `namespace`, `key_type` |
| Call site | `reconcileFernetKeys` / `reconcileCredentialKeys` / `reconcilePasswordRotation` on every reconcile pass |

**`key_type` values.** `fernet`, `credential`, or `admin-password`.

**`admin-password` caveat.** For `key_type="admin-password"` the gauge
measures age since the operator committed the rotated password to its
push-source Secret — not since the password went live. The new password
becomes live only after ESO mirrors it to OpenBao, the `keystone-admin`
ExternalSecret syncs it back, and bootstrap re-runs (~1h+). The gauge
therefore under-reports time-since-live for `admin-password` relative to
`fernet` and `credential`, which go live the instant the operator updates
the keys Secret.

**Annotation source.** The gauge reads the
`forge.c5c3.io/rotation-completed-at` annotation from the **production**
keys Secret first; `applyRotationOutput` stamps that annotation on the
production Secret on every successful apply, so the timestamp is durable
across the inter-rotation steady state and the gauge value (recomputed
as `time.Since(completedAt)` on every reconcile) tracks wall-clock age
correctly even after the staging Secret has been deleted. If the
production Secret has no annotation yet — i.e. the very-first rotation
has not been applied — the lookup falls back to the staging Secret to
cover the post-CronJob-PATCH/pre-apply window.

**When the gauge is NOT set.** Both lookups skip the gauge on absent or
malformed `forge.c5c3.io/rotation-completed-at` annotations: a missing
annotation on both Secrets means rotation has never completed (alert via
PromQL `absent(...)`); a malformed annotation is a script bug surfaced
via the `RotationAnnotationInvalid` event. Validation rejections of a
staging payload are tracked via the `RotationRejected` event (see
[Keystone Controller Events](./keystone/keystone-events.md)) and indirectly via
`keystone_operator_reconcile_errors_total{condition_type="FernetKeysReady"}`.

**Series lifecycle.** When a Keystone CR is deleted, every rotation-age
series tagged with that (keystone, namespace) pair is dropped by
`DeleteForKeystone` in `reconcileDelete`, preventing orphan series
from accumulating over the CR lifetime.

**Example PromQL.** Alert if any rotation is older than 14 days:

```promql
max by (keystone, namespace, key_type) (
  keystone_operator_key_rotation_age_seconds
) > (14 * 86400)
```

---

## `keystone_operator_db_sync_total`

Count of DB-related Job terminal transitions per Keystone CR, labelled
by the terminal state.

| Attribute | Value |
| --- | --- |
| Type | Counter |
| Labels | `keystone`, `namespace`, `result` |
| Call site | `reconcileDatabase`, `reconcileExpand`, `reconcileMigrate`, `reconcileContract` on each Job's first terminal transition |

**Covered Jobs.** The counter aggregates terminal transitions from every
DB-related Job the operator runs against the Keystone CR:

- `<keystone>-db-sync` — non-upgrade schema sync (`reconcileDatabase`).
- `<keystone>-schema-check` — post-`db_sync` drift detection.
- `<keystone>-db-expand` / `-db-migrate` / `-db-contract` — the three
  phases of the expand-migrate-contract upgrade flow.

This deliberate aggregation keeps the dashboard panel and failure-rate
alerts populated during upgrades. The counter does
NOT carry a per-phase label; use the matching `DatabaseReady` condition
reasons (`DBSyncFailed`, `ExpandFailed`, `MigrateFailed`, `ContractFailed`,
`SchemaDriftDetected`) on the CR to attribute a failure to a specific
phase.

**`result` values.** `succeeded` (Job `Complete=True`) or `failed` (Job
`Failed=True`).

**Single-emit guarantee.** Each phase is guarded by an independent
transition predicate keyed on the Job UID and persisted via the
`forge.c5c3.io/last-<phase>-job-uid` annotation on the Keystone CR;
repeated reconciles observing the same terminated Job contribute at
most one increment, so polling does not inflate the counter. The
annotation patch is committed BEFORE the metric is recorded so a
transient apiserver failure cannot cause the next reconcile to
double-count the same Job UID; on patch failure the metric is deferred
to the next reconcile, an Info-level log line is emitted, and a
`Warning` event with reason `DBSyncMetricEmissionDeferred` is recorded
on the Keystone CR so persistent apiserver failures are visible at
default log verbosity and via `kubectl describe keystone`.

**Series lifecycle.** Deleted alongside the per-CR rotation-age gauge
in `DeleteForKeystone` when the Keystone CR is removed.

**Example PromQL.** DB-related failure rate (any phase) over the last hour:

```promql
sum by (keystone, namespace) (
  rate(keystone_operator_db_sync_total{result="failed"}[1h])
)
```

---

## `keystone_operator_db_sync_duration_seconds`

Duration in seconds of terminated DB-related Jobs, measured as
`condition.LastTransitionTime − Job.CreationTimestamp`.

| Attribute | Value |
| --- | --- |
| Type | Histogram |
| Labels | `keystone`, `namespace` |
| Buckets (seconds) | `1, 5, 10, 30, 60, 120, 300, 600` |
| Call site | `reconcileDatabase`, `reconcileExpand`, `reconcileMigrate`, `reconcileContract` on each Job's first terminal transition |

The histogram records one sample per terminated DB-related Job
(`db_sync`, `schema-check`, `db-expand`, `db-migrate`, `db-contract`),
regardless of `result`. Use `keystone_operator_db_sync_total{result="failed"}`
to scope alerting to failures only; use this histogram to reason about
how long a sync *took* when it did run.

**Example PromQL.** p95 `db_sync` duration over the last 24 hours:

```promql
histogram_quantile(
  0.95,
  sum by (keystone, namespace, le) (
    rate(keystone_operator_db_sync_duration_seconds_bucket[24h])
  )
)
```

---

## Enabling the ServiceMonitor

The Helm chart ships an opt-in `monitoring.coreos.com/v1` ServiceMonitor. Enable it via `values.yaml`:

```yaml
# values.yaml fragment
monitoring:
  serviceMonitor:
    enabled: true
    interval: 30s
```

The ServiceMonitor selector targets the operator Service by
`operator-library.selectorLabels` on port `metrics`, scraping the path
`/metrics` at the configured interval. Prometheus-operator CRDs
(`ServiceMonitor`) must be installed in the cluster; the chart does
**not** install them. For the end-to-end operator-side enablement flow,
see
[How to enable the Keystone operator metrics endpoint](../guides/enable-keystone-operator-metrics.md).

---

## Related

- [Keystone Reconciler Architecture — Metrics Instrumentation](./keystone/keystone-reconciler.md#metrics-instrumentation) — the `instrumentSubReconciler` contract and the rule that every new sub-reconciler must be wrapped.
- [Observability & Diagnostics](../guides/observability.md) — human-facing status, conditions, and events.
- [How to enable the Keystone operator metrics endpoint](../guides/enable-keystone-operator-metrics.md) — cluster-side Prometheus/Grafana wiring.
- [`operators/keystone/dashboards/keystone-operator.json`](../../operators/keystone/dashboards/keystone-operator.json) — reference Grafana dashboard consuming the metrics above.
