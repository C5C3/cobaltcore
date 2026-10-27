---
title: Performance Baselines
quadrant: operator
---

# Performance Baselines

Two measurements track the latency of the keystone-operator stack. The
reconcile benchmark (`hack/perf-reconcile-benchmark.sh`) reports how long the
operator takes to reconcile a batch of Keystone CRs. The token measurement
(`hack/perf-token-baseline.sh`) reports how long Keystone takes to issue and
validate a token. `hack/perf-baseline.py` turns both into JSON and judges them
against the baseline committed in `tests/perf/baseline.json`. The nightly
`Performance` workflow runs the whole chain and fails when a latency is more
than 20 % above its baseline.

## Prerequisites

- A running kind stack with the keystone operator deployed: `make deploy-infra`.
  Both measurements expect the managed MariaDB (`openstack-db`) and memcached
  (`openstack-memcached`) clusters and the `keystone-db` / `keystone-admin`
  Secrets the standard stack provides.
- `kubectl`, `curl`, `python3`, and `awk` on `PATH`.
- For the token measurement, a node that can run the Tempest image
  (`ghcr.io/c5c3/tempest:2026.1` unless `CHAOS_WORKLOAD_IMAGE` names another).

## Reconcile benchmark

`hack/perf-reconcile-benchmark.sh` (run via `make perf-benchmark`) is the
regression gate for the Keystone reconcile-loop performance work. It applies a
batch of Keystone CRs, waits for them to become Ready, samples the operator's
Prometheus metrics over a settle window, and reports p50/p95/p99 reconcile
latency for two histograms:

- `keystone_operator_reconcile_duration_seconds`, per sub-reconciler.
- `controller_runtime_reconcile_time_seconds{controller="keystone"}`, the
  built-in end-to-end histogram covering orchestration and the status update.

The percentiles are computed with the same `sum by (le)` aggregation and linear
interpolation Prometheus uses, so the numbers line up with the
[reconcile-duration SLOs](../keystone-operator-metrics.md#reconcile-duration-slos)
and the bundled Grafana dashboard.

### Usage

```bash
# Default: benchmark 1, 5, and 25 CRs.
make perf-benchmark

# Smaller sweep on a constrained cluster.
make perf-benchmark CR_COUNTS="1 5"

# Fail if the steady-state end-to-end p95 exceeds 2s (the SLO as a gate).
make perf-benchmark GATE_P95_SECONDS=2

# Also write the quantiles to a JSON file, as the nightly does.
make perf-benchmark CR_COUNTS="1 3" RESULT_JSON=_output/perf/reconcile.json
```

The script is idempotent: each count generates uniquely named CRs
(`keystone-perf-<i>`) with unique database names (`keystone_perf_<i>`), waits for
Ready, samples, then deletes the CRs and waits for finalizer cleanup before the
next count.

A 25-CR run spawns 25 Deployments plus their db-sync, schema-check, and
bootstrap Jobs and MariaDB Database/User/Grant objects, which does not fit a CI
kind runner. The nightly runs 1 and 3 CRs; the 25-CR default stays a local or
dedicated-cluster run.

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
| `DB_CLUSTER_REF` | `openstack-db` | Managed MariaDB cluster the CRs use |
| `CACHE_CLUSTER_REF` | `openstack-memcached` | Managed memcached cluster the CRs use |
| `IMAGE_REPOSITORY` | `ghcr.io/c5c3/keystone` | Keystone image repository |
| `IMAGE_TAG` | `2026.1` | Keystone image tag, recorded as `meta.release` |
| `GATE_P95_SECONDS` | _(unset)_ | Exit non-zero when the end-to-end p95 exceeds this |
| `RESULT_JSON` | _(unset)_ | Also write the quantiles to this JSON file |

### Reading the report

For each count the script prints a block like:

```text
=== Reconcile latency for 5 CR(s) (settle 60s) ===
  end-to-end (controller_runtime_reconcile_time_seconds): p50=0.02s p95=0.18s p99=0.42s
  sub-reconciler (keystone_operator_reconcile_duration_seconds): p50=0.01s p95=0.09s p99=0.21s
```

Compare the end-to-end p95 against the steady-state SLO (p95 ≤ 2s) and the
per-sub-reconciler p95 against the ≤ 300ms target. A `nan` means no reconciles
were sampled in the window: increase `SETTLE_SECONDS` or check that the CRs are
actually reconciling.

With `RESULT_JSON` set, the script also writes the six quantiles of every count
to that file, under `metrics` as `reconcile.<count>.e2e_p50_s`,
`reconcile.<count>.e2e_p95_s` and `reconcile.<count>.e2e_p99_s` for the
end-to-end histogram and the same three `sub_` keys for the sub-reconciler
histogram. A `nan` is written as `null`. The `meta` block holds the release
(`IMAGE_TAG`), the UTC time of the measurement, `cr_counts` and
`settle_seconds`. The report on stdout is the same with and without the file,
and a red `GATE_P95_SECONDS` still writes it.

## Token issue and validate

`hack/perf-token-baseline.sh` (run via `make perf-token-baseline`) measures
`POST /v3/auth/tokens` and `GET /v3/auth/tokens` against a Keystone of its own:

1. The preflight checks that `kubectl` reaches the cluster, that the operator
   Deployment exists, and that `python3` and the chaos workload harness are
   present. It exits 1 before any CR exists when one is missing.
2. The script applies the Keystone CR `keystone-perf-token` with the database
   `keystone_perf_token` and waits for it to become Ready.
3. It starts the token stream of the
   [chaos workload harness](./chaos-e2e-tests.md#workload-and-consistency-harness)
   in the pod `chaos-wl-perf-token`, against
   `http://keystone-perf-token.<namespace>.svc:5000/v3`. Each cycle of the
   stream issues a token, validates it and pauses for a second, and each
   request writes one record with its latency in `ms`. The chaos suites drive
   the token path with the same client.
4. After `DURATION_SECONDS` it reads the pod log, stops the pod, and runs
   `hack/perf-baseline.py token` on the log, which writes `RESULT_JSON`.

An `EXIT` trap stops the pod and deletes the CR with `--wait` on every exit
after the preflight.

```bash
# The nightly's measurement: 10 minutes of token traffic.
make perf-token-baseline

# A short run that still meets the sample floor.
make perf-token-baseline DURATION_SECONDS=60 MIN_SAMPLES=30
```

| Variable | Default | Purpose |
| --- | --- | --- |
| `NAMESPACE` | `openstack` | Namespace of the CR and the workload pod |
| `CR_NAME` | `keystone-perf-token` | Keystone CR name |
| `DURATION_SECONDS` | `600` | How long the token stream runs |
| `MIN_SAMPLES` | `300` | Ok records each of issue and validate needs |
| `READY_TIMEOUT` | `600s` | `kubectl wait` and delete timeout |
| `OPERATOR_NAMESPACE` | `keystone-system` | Namespace of the operator Deployment |
| `OPERATOR_DEPLOY` | `keystone-operator` | Operator Deployment name |
| `DB_CLUSTER_REF` | `openstack-db` | Managed MariaDB cluster the CR uses |
| `CACHE_CLUSTER_REF` | `openstack-memcached` | Managed memcached cluster the CR uses |
| `IMAGE_REPOSITORY` | `ghcr.io/c5c3/keystone` | Keystone image repository |
| `IMAGE_TAG` | `2026.1` | Keystone image tag, recorded as `meta.release` |
| `RESULT_JSON` | `_output/perf/token.json` | The JSON file written |
| `CHAOS_WORKLOAD_IMAGE` | `ghcr.io/c5c3/tempest:2026.1` | Workload pod image, read by the harness |

`RESULT_JSON` holds eight metrics, four per op (`issue`, `validate`):

| Key | Meaning |
| --- | --- |
| `token.<op>.p50_ms` | Nearest-rank p50 over the `ms` of the ok records |
| `token.<op>.p99_ms` | Nearest-rank p99 over the `ms` of the ok records |
| `token.<op>.samples` | Number of ok records |
| `token.<op>.failed` | Number of failed records |

The `meta` block holds the release (`IMAGE_TAG`), the UTC time of the
measurement and `token_records`, the number of token records in the log, ok
and failed.

The script exits 0 when both ops have at least `MIN_SAMPLES` ok records and no
failed record. It exits 1 when an op falls short or failed, naming the op and
its count with the JSON still written, and also when the CR never turns Ready
or the workload pod never reports `READY token` within
`CHAOS_WORKLOAD_READY_SECONDS`. A pod log without a token record exits 2.

## perf-baseline.py

`hack/perf-baseline.py` uses the Python standard library only. Its three
subcommands share the exit codes 0 (pass), 1 (a judged failure) and 2 (an input
error):

| Subcommand | Flags | Exit 1 | Exit 2 |
| --- | --- | --- | --- |
| `token` | `--log <file> --release <r> --min-samples <n> --out <json>` | An op with fewer than `--min-samples` ok records or a failed record; the JSON is still written | The log cannot be opened or holds no token record |
| `compare` | `--baseline <json> --tolerance-percent <t> --measured <json>...` | A judged key above its baseline by more than the tolerance | A baseline without `recorded.release`, a `metrics` object or numeric values, a baseline key missing from the measured files, a measured `null`, a key in two measured files, a measured file of another release than the baseline, a file that cannot be parsed |
| `write-baseline` | `--measured <json>... --release <r> --commit <sha> --run <url> --out <json>` | Never | A measured file of another release, a `null`, a key in two files, a file that cannot be parsed |

`token` skips log lines that are not JSON records with `op`, `ok` and `ms`,
such as `READY token`, and prints how many it skipped. The p-th percentile of
n values is the ⌈p·n/100⌉-th smallest (nearest rank).

`compare` merges the `metrics` of the measured files and prints one row per
key. A key that ends in `_ms` or `_s` is judged: its measured value may be at
most `baseline × (1 + tolerance / 100)`. Keys ending in `.samples` or `.failed`
are printed and never judged, and a key only the measured files carry is
printed as `new`:

```text
key                    baseline  measured  delta    verdict
reconcile.1.e2e_p95_s  0.02      0.021     +5.0 %   ok
token.issue.p50_ms     100       125       +25.0 %  REGRESSION
token.issue.samples    300       310       +3.3 %   -
compare: 1 regression(s) above 20 %
```

## The baseline file

`tests/perf/baseline.json` is the output of `write-baseline`: the merged
`metrics` of the token and the reconcile measurement of one `Performance` run,
and a `recorded` block that names where they come from.

| Field | Content |
| --- | --- |
| `recorded.release` | The OpenStack release the run measured |
| `recorded.commit` | The commit the run checked out |
| `recorded.run` | The URL of the workflow run whose artifact the file was written from |
| `recorded.date` | The UTC date the file was written |
| `metrics` | The eight `token.*` and the twelve `reconcile.{1,3}.*` keys |

The tolerance is 20 %, the figure #625 Phase 8 named. A measured file whose
`meta.release` differs from `recorded.release` makes `compare` exit 2 and name
both releases, so moving the nightly to another release forces a new baseline.
Until the file exists, the comparison exits 2 with `cannot read`.

## The Performance workflow

`.github/workflows/performance.yaml` runs at 01:30 UTC (`30 1 * * *`) and on
`workflow_dispatch`. Its one job, `baseline`, runs on a `self-hosted` runner
with a 75-minute wall and read-only `contents` and `packages` permissions:

1. It resolves the release as the oldest one keystone ships a service image
   for (`OPERATOR=keystone hack/ci-service-image-releases.sh | head -n1`).
2. It creates the kind cluster from `hack/kind-config.yaml`, logs in to GHCR,
   pulls `keystone-operator:latest`, `keystone:<release>` and
   `tempest:<release>`, and loads them into the node. Every push to `main`
   publishes `keystone-operator:latest`, so the night measures the operator
   `main` last built.
3. It deploys the infrastructure stack (`setup-e2e-infra`) and the operator at
   `:latest` into `keystone-system`, with the chart of the checked-out commit
   and one replica. The reconcile benchmark reads the metrics through a
   port-forward to the Deployment, and only the leader reconciles.
4. It runs the token measurement and then the reconcile benchmark with
   `CR_COUNTS="1 3"`, `SETTLE_SECONDS=120` and `GATE_P95_SECONDS=2`, each
   writing its JSON under `_output/perf/`.
5. It runs `perf-baseline.py compare --tolerance-percent 20` on both files
   and writes its output, inside a text fence, to `_output/perf/summary.md`
   through `tee`. The step runs under `set -o pipefail` and exits with the
   status of `compare`, so a regression still fails it. A failed measurement
   ends the job before this step.
6. It uploads `_output/perf/` as the artifact `performance-results`, kept for
   90 days, dumps diagnostics when a step failed, and deletes the cluster.

A red night fails the run. On a scheduled run the `report` job then files it
on one tracking issue, "Scheduled performance run is red", through
`hack/ci-report-red-run.sh`: it comments on the open issue of that title that
`github-actions[bot]` opened, or opens one, with `_output/perf/summary.md` of
the `performance-results` artifact as the body. A night that failed before the
comparison has no summary, and the issue points at the run log. The `report`
job runs on `ubuntu-latest` and is the only job of the workflow that holds
`issues: write`. A dispatched run never reports, and a human closes the issue
once the cause is fixed. The workflow pins
`KIND_VERSION` like `ci.yaml`, `e2e-operator-upgrade.yaml`, `flux-path.yaml`
and `hack/install-test-deps.sh`, and Renovate's `kind` group moves the five
pins in one pull request
(`tests/unit/renovate/workflow_pins_custommanager_test.sh` asserts they agree).

## Re-baselining

Write a new baseline when the nightly moves to another release (`compare` exits
2 naming both releases), after a change that alters the measured path on
purpose, or when the measurement itself changes. Diagnose a red night from its
artifact before re-baselining it away.

1. Dispatch the workflow on the branch to measure:

   ```bash
   gh workflow run performance.yaml --ref <branch>
   ```

2. Download the artifact of that run and note the commit it measured:

   ```bash
   gh run download <run-id> -n performance-results -D /tmp/perf
   gh run view <run-id> --json headSha -q .headSha
   ```

3. Write the baseline:

   ```bash
   python3 hack/perf-baseline.py write-baseline \
     --measured /tmp/perf/token.json /tmp/perf/reconcile.json \
     --release <release> --commit <head-sha> \
     --run https://github.com/C5C3/cobaltcore/actions/runs/<run-id> \
     --out tests/perf/baseline.json
   ```

4. Commit `tests/perf/baseline.json`, naming the run in the commit message.
