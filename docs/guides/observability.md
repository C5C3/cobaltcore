---
title: Observability & Diagnostics
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Observability & Diagnostics

How to read what the Keystone operator is doing — without tailing controller logs.

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (Extended)](../quick-start-extended.md)** devstack. Stand it up first:

```bash
kind create cluster --name forge --config hack/kind-config.yaml
make deploy-infra
```

Follow that tutorial through to its final **Verify the deployment** step, so a
Keystone CR named `keystone` is `Ready` in the `openstack` namespace. Every
resource name in the examples below is one that devstack produces.
:::

The operator surfaces its state through three complementary channels:

| Channel | Purpose | Primary audience |
|---------|---------|------------------|
| **Print columns** | One-line health summary for every Keystone CR | Humans, `kubectl get` |
| **Status conditions** | Structured, programmatic state | Automation, CI, alerts |
| **Events** | Timestamped audit trail of transitions | Humans investigating incidents |

---

## Print columns

`kubectl get keystones` exposes a compact summary via four printer columns:

```bash
kubectl get keystones -A
```

```
NAMESPACE   NAME       READY   ENDPOINT                                                 RELEASE   AGE
openstack   keystone   True    http://keystone.openstack.svc.cluster.local:5000/v3      2025.2    12m
```

| Column | Source | Meaning |
|--------|--------|---------|
| `READY` | `.status.conditions[?(@.type=='Ready')].status` | Aggregate health |
| `ENDPOINT` | `.status.endpoint` | In-cluster Keystone API URL (`…:5000/v3`) |
| `RELEASE` | `.status.installedRelease` | OpenStack release currently deployed |
| `AGE` | `.metadata.creationTimestamp` | CR age |

---

## Status conditions

`.status.conditions[]` follows the standard Kubernetes pattern (`type`, `status`, `reason`, `message`, `lastTransitionTime`, `observedGeneration`). Fourteen sub-conditions feed into the aggregate `Ready`. All are always reported; the ones tied to an optional spec field carry a "not required" / "disabled" reason when that field is unset rather than disappearing:

| Condition | Means |
|-----------|-------|
| `SecretsReady` | Referenced database and admin Secrets are available |
| `FernetKeysReady` | Fernet key Secret and rotation CronJob exist |
| `CredentialKeysReady` | Credential key Secret and rotation CronJob exist (always managed; `spec.credentialKeys` only tunes the schedule and max active keys) |
| `DatabaseReady` | `db_sync` Job completed successfully (and schema check passed) |
| `DatabaseTLSReady` | Database TLS client certificate issued, or `NotRequired` — see [Enable Keystone Database TLS](./enable-keystone-database-tls.md) |
| `PolicyValidReady` | `spec.policyOverrides` validated against `oslo.policy` |
| `DeploymentReady` | API Deployment has available replicas |
| `KeystoneAPIReady` | Keystone API is responding to `/v3` health probes |
| `HPAReady` | HorizontalPodAutoscaler created (if `spec.autoscaling` is set) |
| `NetworkPolicyReady` | NetworkPolicy created (if `spec.networkPolicy` is set) |
| `HTTPRouteReady` | Gateway API HTTPRoute reconciled, or not required when `spec.gateway` is unset |
| `BootstrapReady` | Bootstrap Job completed (admin user, region, endpoints) |
| `TrustFlushReady` | Trust-flush CronJob created — defaults to hourly |
| `PasswordRotationReady` | Scheduled admin-password rotation reconciled, or `RotationDisabled` when `spec.passwordRotation` is unset — see [Schedule Keystone Admin Password Rotation](./keystone-admin-password-scheduled-rotation.md) |
| `Ready` | All of the above are `True` |

Read them as a tree:

```bash
kubectl get keystone keystone -n openstack \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\t"}{.message}{"\n"}{end}' \
  | column -t -s $'\t'
```

Or wait for a specific one:

```bash
kubectl wait keystone/keystone -n openstack \
  --for=condition=DatabaseReady --timeout=5m
```

::: tip Diagnosing a stuck CR
The first `status=False` condition from the top is usually the bottleneck:

- `SecretsReady=False` → check that `keystone-db` and `keystone-admin` Secrets exist in the same namespace
- `DatabaseReady=False` → look at Events for `DBSyncFailed` or `SchemaDriftDetected`
- `DeploymentReady=False` → `kubectl describe deploy keystone` — usually image pull or probe failures
:::

---

## Upgrade status fields

During an image-tag change, three additional status fields track progress (see [Day 2 Operations — Image upgrade](./day-2-operations.md#image-upgrade-with-upgradephase)):

| Field | Outside upgrade | During upgrade |
|-------|-----------------|----------------|
| `.status.installedRelease` | Currently deployed release | Previous release (not yet changed) |
| `.status.targetRelease` | `""` | Target release |
| `.status.upgradePhase` | `""` | `Expanding` → `Migrating` → `RollingUpdate` → `Contracting` |

Watch the upgrade live:

```bash
kubectl get keystone keystone -n openstack -w \
  -o custom-columns=NAME:.metadata.name,PHASE:.status.upgradePhase,FROM:.status.installedRelease,TO:.status.targetRelease
```

---

## Events

Every lifecycle transition emits a Kubernetes Event with a stable, PascalCase `reason`. Events are deduplicated by (object, reason, message) — repeated reconciles do not spam the event stream.

### Show everything for a CR

```bash
kubectl describe keystone keystone -n openstack
```

The bottom of the output lists the Events in reverse-chronological order. Alternatively, a timeline view:

```bash
kubectl get events -n openstack \
  --field-selector involvedObject.kind=Keystone \
  --sort-by='.lastTimestamp'
```

### Common reasons

| Reason | Type | When you see it |
|--------|------|-----------------|
| `BootstrapComplete` | Normal | Bootstrap Job finished (admin user, region, endpoints created) |
| `DatabaseSynced` | Normal | `db_sync` finished, schema matches Alembic head |
| `FernetKeysGenerated` | Normal | Fernet Secret was created or rotated |
| `UpgradeInitiated` | Normal | `spec.image.tag` change triggered an upgrade |
| `ExpandComplete` / `MigrateComplete` | Normal | Upgrade phase boundary reached |
| `UpgradeComplete` | Normal | Full expand-migrate-contract pipeline finished, `installedRelease` advanced |
| `ContractFailed` | Warning | Contract phase `db_sync --contract` returned non-zero |
| `DBSyncFailed` | Warning | `db_sync` Job returned non-zero |
| `SchemaDriftDetected` | Warning | Schema check found unexpected drift |
| `DowngradeNotSupported` | Warning | Target tag is older than `installedRelease` |
| `UpgradePathInvalid` | Warning | Target tag skips a release (non-sequential) |

The full catalogue is in [Keystone Controller Events](../reference/keystone/keystone-events.md).

---

## Keystone application logs

For the Keystone API itself (as opposed to the operator), tail the workload pods directly:

```bash
kubectl logs -n openstack -l app.kubernetes.io/name=keystone --tail=200 -f
```

Two distinct streams are interleaved on the same stdout/stderr:

- **uWSGI access lines** — emitted per HTTP request via an always-on `--log-master
  --log-format` literal, e.g.
  `GET /v3/auth/tokens => generated 1234 bytes in 12 msecs (HTTP/1.1 201)`.
  Useful for traffic-shape questions (latency, status code distribution).
- **oslo.log application records** — emitted by Keystone code paths (auth, federation,
  middleware), formatted by `oslo.log` per `spec.logging`. Useful for "why did this
  request fail" questions.

Only the oslo.log stream honours `spec.logging.format`; the uWSGI access lines use a
fixed plain-text format regardless. The oslo.log default is `text` (line format,
human-readable). Switch to JSON for direct ingest by Loki/OpenSearch:

```bash
kubectl patch keystone -n openstack keystone --type=merge \
  -p '{"spec":{"logging":{"format":"json"}}}'

# Wait for the rollout, then verify each oslo.log record is jq-parseable:
kubectl logs -n openstack -l app.kubernetes.io/name=keystone --tail=20 \
  | grep -v 'generated.*bytes in' \
  | jq -e .
```

`jq -e .` exits non-zero if any input line is not valid JSON, giving a binary
pass/fail signal for the format toggle. See
[`spec.logging` in the CRD reference](../reference/keystone/keystone-crd.md#loggingspec)
for the full field semantics, including `level`, `debug`, and `perLoggerLevels`.

---

## Controller logs (last resort)

If status and events don't explain a failure, read the operator logs directly:

```bash
kubectl logs -n keystone-system -l app.kubernetes.io/name=keystone-operator \
  --tail=200 -f
```

The operator uses structured `logr` output — every line includes the reconciled object's namespace/name and the sub-reconciler that produced the log. Filter a specific CR:

```bash
kubectl logs -n keystone-system -l app.kubernetes.io/name=keystone-operator --tail=500 \
  | grep '"Keystone":"openstack/keystone"'
```

---

## Further reading

- [Keystone Controller Events](../reference/keystone/keystone-events.md) — full event reason catalogue with example messages and alerting templates
- [Keystone Reconciler Architecture](../reference/keystone/keystone-reconciler.md) — sub-reconciler contracts and watches
- [Keystone Operator Prometheus Metrics](../reference/keystone-operator-metrics.md) — metric catalogue, labels, buckets, and sample PromQL
- [Reconcile duration SLOs](../reference/keystone-operator-metrics.md#reconcile-duration-slos) — steady-state and rotation-wait p95 targets for the reconcile loop
- [Enable the Keystone operator metrics endpoint](./enable-keystone-operator-metrics.md) — ServiceMonitor enablement and Grafana import walk-through
- [Keystone Upgrade Flow](../reference/keystone/keystone-upgrade-flow.md) — state machine that drives upgrade conditions
- [Day 2 Operations](./day-2-operations.md) — putting this observability into practice during scale, upgrade, rotation
