---
title: Keystone Operator
quadrant: operator
---

# Keystone Operator

The Keystone operator deploys and manages the OpenStack Identity Service as a
Kubernetes-native workload. It is the reference implementation for all CobaltCore
service operators — the patterns established here (CRD layout, sub-reconciler
chain, webhooks, finalizers, instrumentation) will be replicated for Nova,
Neutron, Glance, and other OpenStack service operators.

This page is a feature catalogue and entry point. Each item links to the
in-depth reference doc for that area.

## Lifecycle and Reconciliation

- **Sub-reconciler chain.** A focused pipeline of sub-reconcilers — Secrets →
  DatabaseTLS → DBConnectionSecret → Config → FernetKeys / CredentialKeys /
  NetworkPolicy → Database → PolicyValidation → Deployment → HTTPRoute →
  HealthCheck → HPA → Bootstrap → TrustFlush → PasswordRotation — each
  emitting a typed sub-condition that aggregates into `Ready`. See
  [Reconciler Architecture](./keystone-reconciler.md).
- **Parallel execution group.** FernetKeys, CredentialKeys and NetworkPolicy
  run concurrently via `errgroup` to cut tail latency on cold reconciles.
- **Two finalizers.** The standard cleanup finalizer cascades owned resources;
  the OpenBao finalizer gates deletion on ESO `PushSecret` cleanup so
  Fernet/credential key backups in OpenBao stay consistent.
- **Watch-driven reactivity.** Field-indexed `Secret` watches and a
  `PushSecret` name-match mapper with predicate filter wake the workqueue
  only on transitions the state machine branches on, not on every ESO sync
  tick.

## CRD Surface

- **Comprehensive spec.** Replicas, image, database, cache, fernet,
  credentialKeys, trustFlush, bootstrap, federation, middleware, plugins,
  policy overrides, autoscaling, networkPolicy, gateway, resources, uwsgi,
  graceful-termination knobs, topologySpreadConstraints, priorityClassName,
  rollout `strategy`, and free-form `extraConfig`.
- **Status with sub-conditions.** Fourteen typed sub-conditions plus
  `installedRelease`, `targetRelease`, `upgradePhase`, and `endpoint` —
  surfaced via `kubectl get keystones` printer columns.
- **Validating + Defaulting webhooks.** CEL validation rules enforced by the
  API server (database/cache exclusivity, autoscaling targets, replica/key
  minimums, graceful-termination invariants) plus defaults injected by the
  webhook for replicas, resources, and graceful-termination knobs.
- **Stable sub-resource naming.** All emitted resources are named after the
  CR with no `-api` suffix; cluster-internal DNS aligns with the public
  Gateway hostname.

See [CRD API Reference](./keystone-crd.md) and
[Controller Events](./keystone-events.md).

## Encryption Key Management

- **Fernet token keys.** Per-CR CronJob with configurable schedule and
  `maxActiveKeys`; rotation script delivered via ConfigMap.
- **Credential keys.** Same rotation model, but each rotation is automatically
  followed by a `credential_migrate` step.
- **Automatic rolling restart on rotation.** The pod template carries
  `keystone.c5c3.io/fernet-keys-hash` and `credential-keys-hash` annotations,
  so any key change triggers a Deployment rollout.
- **OpenBao backup via ESO PushSecret.** Keys are mirrored to OpenBao for
  disaster recovery; staging Secrets are owner-referenced for cache eviction
  on rotation.
- **Watch-driven backup finalizer.** PushSecret watch with predicate filter
  eliminates per-sync workqueue churn and trims delete latency to sub-15s.

See the [Key Rotation Guide](../../guides/keystone-key-rotation.md).

## Database Lifecycle

- **Managed mode via mariadb-operator.** Operator emits
  `Database`/`User`/`Grant` CRs and waits for the upstream MariaDB cluster
  to report health before running `db_sync`.
- **Schema drift detection.** A read-only schema-check Job runs after
  `db_sync` and fails the reconcile if the database schema deviates from
  the expected Alembic head. See
  [Schema Drift Detection](./keystone-schema-drift-detection.md).
- **Expand-migrate-contract upgrades.** When `spec.image.tag` advances to a
  new OpenStack release, the operator drives phased database migrations
  while keeping the API available. Sequential-only upgrade paths; patch
  revisions skip migration entirely. See
  [Upgrade Flow](./keystone-upgrade-flow.md).
- **oslo.config env-var overrides.** Database credentials and other runtime
  knobs are injected via `OS_<GROUP>__<OPTION>` env vars rather than baked
  into the rendered config, so credential rotation does not require a
  ConfigMap re-render.
- **Optional database TLS.** `spec.database.tls` enables encrypted MariaDB
  connections up to `verify-full`, with a cert-manager-issued client
  certificate in managed mode and a dedicated `DatabaseTLSReady`
  sub-condition. See the
  [Database TLS guide](../../guides/enable-keystone-database-tls.md).

## Networking and Exposure

- **Cluster-internal Service.** ClusterIP, named port, stable DNS at
  `<name>.<namespace>.svc.cluster.local:5000`.
- **Gateway API integration.** Optional `HTTPRoute` rendered from
  `spec.gateway`; presence of `gateway.networking.k8s.io/v1` is detected at
  startup via the manager's `RESTMapper` and the watch is registered only
  when the CRD is installed. `status.endpoint` reflects the Gateway hostname.
- **Per-CR NetworkPolicy.** Auto-derived egress to database, cache, ESO and
  OpenBao; configurable ingress.
- **Operator NetworkPolicy.** Chart-level, default-off, opt-in hardening of
  the operator pod itself with fail-closed render guards. See
  [Operator NetworkPolicy](./keystone-operator-networkpolicy.md) and the
  [enablement guide](../../guides/enable-keystone-operator-networkpolicy.md).

## Observability

- **Active HTTP health check** against the Keystone API endpoint drives the
  `KeystoneAPIReady` condition. Injectable HTTP client for tests.
- **Kubernetes Events** for every state transition — bootstrap, db_sync,
  upgrade phases, key generation, deployment rollout. Catalogued in
  [Controller Events](./keystone-events.md).
- **Prometheus metrics + ServiceMonitor.** Reconcile duration, per-condition
  error counts, key rotation age, db_sync outcomes and duration.
  Contract-tested against this catalogue. See
  [Operator Metrics](../keystone-operator-metrics.md) and the
  [enablement guide](../../guides/enable-keystone-operator-metrics.md).

## Day-2 Operations

- **Bootstrap Job.** Idempotent `keystone-manage bootstrap` establishing the
  admin project/user/role, region and public endpoint.
- **Trust flush CronJob.** Optional periodic cleanup of expired trust
  delegations.
- **Admin password rotation.** Manual rotation at the OpenBao source with a
  digest-gated bootstrap re-run, plus an optional in-cluster scheduled
  rotation CronJob via `spec.bootstrap.passwordRotation`. See the
  [rotation](../../guides/keystone-admin-password-rotation.md) and
  [scheduled rotation](../../guides/keystone-admin-password-scheduled-rotation.md)
  guides.
- **Policy validation.** `oslopolicy-validator` Job blocks rollouts on
  invalid policy overrides.
- **Graceful-termination knobs.** `terminationGracePeriodSeconds`,
  `preStopSleepSeconds`, and rollout `strategy` exposed on the CR with
  webhook-enforced invariants.
- **HPA lifecycle.** HPA is created when `spec.autoscaling` is set, removed
  when cleared, with CPU and/or memory targets.
- **Topology spread + PriorityClass.** Sensible defaults across zone and
  hostname; webhook validates that referenced PriorityClasses exist.
- **ConfigMap rotation pruning.** Stale `<name>-config-<hash>` ConfigMaps
  are pruned after rollout, retaining the three most recent revisions for
  fast rollback.

## Where to go next

- New to the operator? Start with the [Quick Start](../../quick-start.md).
- Running it in production? Read
  [Day 2 Operations](../../guides/day-2-operations.md),
  [Observability & Diagnostics](../../guides/observability.md), and
  [Multi-Tenant Deployment](../../guides/multi-tenant-deployment.md).
- Diving into the code? Begin with
  [Reconciler Architecture](./keystone-reconciler.md) and follow the links
  into the individual sub-reconcilers.
