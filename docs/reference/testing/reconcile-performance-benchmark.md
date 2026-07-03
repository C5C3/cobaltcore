---
title: Reconcile Performance Benchmark
quadrant: operator
---

# Reconcile Performance Benchmark

`hack/perf-reconcile-benchmark.sh` (run via `make perf-benchmark`) is the
regression gate for the Keystone reconcile-loop performance work. It applies a
batch of Keystone CRs to a running kind stack, waits for them to become Ready,
samples the operator's Prometheus metrics over a settle window, and reports
p50/p95/p99 reconcile latency for two histograms:

- `keystone_operator_reconcile_duration_seconds` — per sub-reconciler.
- `controller_runtime_reconcile_time_seconds{controller="keystone"}` — the
  built-in end-to-end histogram covering orchestration and the status update.

The percentiles are computed with the same `sum by (le)` aggregation and linear
interpolation Prometheus uses, so the numbers line up with the
[reconcile-duration SLOs](../keystone-operator-metrics.md#reconcile-duration-slos)
and the bundled Grafana dashboard.

## Prerequisites

- A running kind stack with the operator deployed: `make deploy-infra`. The
  benchmark expects the managed MariaDB (`openstack-db`) and memcached
  (`openstack-memcached`) clusters and the `keystone-db` / `keystone-admin`
  Secrets the standard stack provides.
- `kubectl`, `curl`, `python3`, and `awk` on `PATH`.

## Usage

```bash
# Default: benchmark 1, 5, and 25 CRs.
make perf-benchmark

# Smaller sweep on a constrained cluster.
make perf-benchmark CR_COUNTS="1 5"

# Fail if the steady-state end-to-end p95 exceeds 2s (the SLO as a gate).
make perf-benchmark GATE_P95_SECONDS=2
```

The script is idempotent: each count generates uniquely named CRs
(`keystone-perf-<i>`) with unique database names (`keystone_perf_<i>`), waits for
Ready, samples, then deletes the CRs and waits for finalizer cleanup before the
next count.

### Environment variables

| Variable | Default | Purpose |
| --- | --- | --- |
| `CR_COUNTS` | `1 5 25` | Space-separated CR counts to benchmark |
| `NAMESPACE` | `openstack` | Namespace for the Keystone CRs |
| `OPERATOR_NAMESPACE` | `keystone-system` | Namespace of the operator Deployment |
| `OPERATOR_DEPLOY` | `keystone-operator` | Operator Deployment name |
| `METRICS_PORT` | `8080` | Operator metrics container port |
| `SETTLE_SECONDS` | `60` | Steady-state sampling window |
| `READY_TIMEOUT` | `600s` | `kubectl wait` timeout for readiness |
| `GATE_P95_SECONDS` | _(unset)_ | Exit non-zero when the end-to-end p95 exceeds this |

## Reading the report

For each count the script prints a block like:

```text
=== Reconcile latency for 5 CR(s) (settle 60s) ===
  end-to-end (controller_runtime_reconcile_time_seconds): p50=0.02s p95=0.18s p99=0.42s
  sub-reconciler (keystone_operator_reconcile_duration_seconds): p50=0.01s p95=0.09s p99=0.21s
```

Compare the end-to-end p95 against the steady-state SLO (p95 ≤ 2s) and the
per-sub-reconciler p95 against the ≤ 300ms target. A `nan` means no reconciles
were sampled in the window — increase `SETTLE_SECONDS` or check that the CRs are
actually reconciling.

## Why this is not a CI job

A 25-CR run spawns 25 Deployments plus their db-sync, schema-check, and bootstrap
Jobs and MariaDB Database/User/Grant objects, which exceeds the capacity of the
CI kind runners. The benchmark is therefore a local / dedicated-cluster tool. A
CI smoke variant (for example `CR_COUNTS="1 3"` behind a label) would be a
separate change; open an issue for it per the project's feature-triage
convention.
