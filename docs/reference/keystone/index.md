---
title: Keystone Operator
quadrant: operator
---

# Keystone Operator

The Keystone operator deploys and manages the OpenStack Identity Service as a
Kubernetes-native workload. It is the reference implementation for all CobaltCore
service operators — the patterns established here (CRD layout, sub-reconciler
chain, webhooks, finalizers, instrumentation) are replicated by the Horizon,
Glance, Placement, Barbican, Neutron, Cinder and Nova operators and by the
OVN layer underneath Neutron.

This page is a feature catalogue and entry point. Each item links to the
in-depth reference doc for that area.

## Lifecycle and Reconciliation

The figure shows the pipeline one pass runs.
[Reconciliation Flow](./keystone-reconciler.md#reconciliation-flow) lists every
entry with its condition and its requeue interval.

![The sub-reconciler pipeline of the Keystone operator. A pass runs its entries one after another and ends at the first that returns a requeue or an error: Secrets, DatabaseTLS, DBConnectionSecret, IdentityBackends, Config, a parallel group of FernetKeys, CredentialKeys and NetworkPolicy, then Database, PolicyValidation, Deployment, an unnamed prune step, a second parallel group of HTTPRoute, HealthCheck, HPA, VPA, Bootstrap and TrustFlush, and PasswordRotation. Every member of a group starts, and the group returns the shortest requeue. Each step sets one condition; DBConnectionSecret and Config report through SecretsReady, and the prune step has none of its own. DBConnectionSecret hands the hash of the database connection to Deployment, IdentityBackends hands on the name of the domains Secret and the federation projection, Config hands on the name of the ConfigMap, and Deployment sets status.endpoint, which HealthCheck waits for. The early exit and the full pass both end in updateStatus, which aggregates Ready from the sub-conditions and writes the status only when it changed.](../../diagrams/service-reconciler-pipeline.svg)

- **Sub-reconciler chain.** A pipeline of sub-reconcilers: Secrets →
  DatabaseTLS → DBConnectionSecret → IdentityBackends → Config → FernetKeys /
  CredentialKeys / NetworkPolicy → Database → PolicyValidation → Deployment →
  HTTPRoute / HealthCheck / HPA / VPA / Bootstrap / TrustFlush →
  PasswordRotation. Each reports through a typed sub-condition that aggregates
  into `Ready`. See [Reconciler Architecture](./keystone-reconciler.md).
- **Two parallel groups.** FernetKeys, CredentialKeys and NetworkPolicy run
  concurrently via `errgroup`, and so do HTTPRoute, HealthCheck, HPA, VPA,
  Bootstrap and TrustFlush, to cut tail latency on cold reconciles.
- **Two finalizers, three on a target cluster.**
  `keystone.openstack.c5c3.io/finalizer` deletes the MariaDB `Database`,
  `User` and `Grant`; the OpenBao finalizer gates deletion on ESO `PushSecret`
  cleanup so Fernet/credential key backups in OpenBao stay consistent. A
  Keystone with `spec.targetClusterRef` also carries
  `openstack.c5c3.io/remote-children`, which sweeps the children that carry
  ownership labels in place of an owner reference.
- **Watch-driven reactivity.** Field-indexed `Secret` watches and a
  `PushSecret` name-match mapper with predicate filter wake the workqueue
  only on transitions the state machine branches on, not on every ESO sync
  tick.

## CRD Surface

- **Comprehensive spec.** Image, database, cache, fernet, credentialKeys,
  passwordRotation, trustFlush, bootstrap, federation, middleware, plugins,
  policy overrides, autoscaling, networkPolicy, gateway, uwsgi, logging,
  free-form `extraConfig`, a `deployment` block grouping the pod-level knobs
  (replicas, resources, rollout `strategy`, graceful-termination timings,
  topologySpreadConstraints, priorityClassName, nodeSelector, tolerations,
  affinity), and a `jobs` block that sizes, prioritizes and places every Job
  and CronJob.
- **Status with sub-conditions.** Sixteen typed sub-conditions plus
  `installedRelease`, `targetRelease`, `upgradePhase`, and `endpoint` —
  surfaced via `kubectl get keystones` printer columns.
- **Validating + Defaulting webhooks.** CEL validation rules enforced by the
  API server (database/cache exclusivity, autoscaling targets, replica/key
  minimums, graceful-termination invariants) plus defaults injected by the
  webhook for replicas. Container resources and the graceful-termination
  fallbacks resolve when the reconciler renders the pod (see
  [Resource defaults](./keystone-crd.md#resource-defaults)).
- **Stable sub-resource naming.** All emitted resources are named after the
  CR with no `-api` suffix; cluster-internal DNS aligns with the public
  Gateway hostname.

See [CRD API Reference](./keystone-crd.md) and
[Controller Events](./keystone-events.md).

## Identity Backends (LDAP/AD Domains)

- **Attachable domain CRD.** One `KeystoneIdentityBackend` CR per LDAP/AD
  domain — connection, bind credentials, tree/attribute mapping, read-only
  mode, TLS, `extraOptions` — attached via `spec.keystoneRef`.
- **Dedicated controller + keystone-side projection.** The backend
  controller owns finalizer, domain provisioning (Manage/Adopt), and the
  per-backend `DomainReady`/`ConfigProjected`/`Ready` conditions; the
  keystone-side sub-reconciler aggregates all `DomainReady` backends into a
  content-hashed domains Secret mounted at `/etc/keystone/domains/` in the
  Deployment and every keystone-manage Job/CronJob.
- **Safety rails.** The `Default` domain is never external, domain names
  are unique per Keystone, read-only mode forces the write options off, and
  `deletionPolicy: Retain|Delete` (Retain default; adopted domains always
  retained) governs teardown.

See [KeystoneIdentityBackend CRD](./identity-backend-crd.md) and the
[LDAP Domain Backend guide](../../guides/keystone/ldap-domain-backend.md).

## Encryption Key Management

- **Fernet token keys.** Per-CR CronJob with configurable schedule and
  `maxActiveKeys`; rotation script delivered via ConfigMap.
- **Credential keys.** Same rotation model, but each rotation is automatically
  followed by a `credential_migrate` step.
- **In-place key rotation, no rollout.** The operator replaces the data of the
  key Secret and the kubelet projects it into the running pods. The pod
  template carries no hash of the key Secrets. Its one hash annotation,
  `keystone.c5c3.io/db-connection-hash`, rolls the Deployment when a Dynamic
  database credential changes.
- **OpenBao backup via ESO PushSecret.** Keys are mirrored to OpenBao for
  disaster recovery; staging Secrets are owner-referenced for cache eviction
  on rotation.
- **Watch-driven backup finalizer.** PushSecret watch with predicate filter
  eliminates per-sync workqueue churn and trims delete latency to sub-15s.

See the [Key Rotation Guide](../../guides/keystone/keystone-key-rotation.md).

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
  [Database TLS guide](../../guides/keystone/enable-keystone-database-tls.md).

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
  [enablement guide](../../guides/keystone/enable-keystone-operator-networkpolicy.md).

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
  [enablement guide](../../guides/keystone/enable-keystone-operator-metrics.md).

## Day-2 Operations

- **Bootstrap Job.** Idempotent `keystone-manage bootstrap` establishing the
  admin project/user/role, region and public endpoint.
- **Trust flush CronJob.** Optional periodic cleanup of expired trust
  delegations.
- **Admin password rotation.** Manual rotation at the OpenBao source with a
  digest-gated bootstrap re-run, plus an optional in-cluster scheduled
  rotation CronJob via `spec.passwordRotation`. See the
  [rotation](../../guides/keystone/keystone-admin-password-rotation.md) and
  [scheduled rotation](../../guides/keystone/keystone-admin-password-scheduled-rotation.md)
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
  are pruned after rollout, keeping the current revision and the three newest
  before it for fast rollback.

## Owned resources

The figure groups what the operator creates for one Keystone CR and what it
only reads. [Owned Resources](./keystone-reconciler.md#owned-resources) lists
every object with the condition it exists under and names what each workload
mounts.

![What one Keystone resource creates, in four groups inside a frame of owned objects, and what it only reads. Serving: the Deployment, the Service and the PodDisruptionBudget, and with their spec fields a HorizontalPodAutoscaler, a VerticalPodAutoscaler, a NetworkPolicy and an HTTPRoute. Config and Secrets: the immutable config ConfigMap, the Fernet and credential key Secrets with a PushSecret each, the db-connection Secret, and with their spec fields the domains Secret, the federation Secret, the database client Certificate with its Secret, and the MariaDB Database, User and Grant. One-shot Jobs: db-sync, schema-check and bootstrap, the policy validation Job, and the three Jobs of a release upgrade. CronJobs: the two key rotations with a ServiceAccount, a Role, a RoleBinding and a staging Secret each, the trust flush, and with its spec field the admin password rotation with its staging Secret, its push source Secret and its PushSecret. The Deployment mounts the ConfigMap and the key Secrets and reads the database URL from the db-connection Secret. Every Job mounts the ConfigMap, the keystone-manage CronJobs mount the ConfigMap and the key Secrets, and each rotation CronJob patches only its staging Secret. Read and not owned: the database credentials Secret, the admin password Secret, the policy ConfigMap, the MariaDB cluster, the secret store, the Gateway, the ClusterIssuer of the database CA and the identity backends. Every owned object carries an owner reference to the Keystone; on a target cluster ownership labels take its place.](../../diagrams/service-owned-resources.svg)

## Where to go next

- New to the operator? Start with the [Quick Start](../../quick-start.md).
- Running it in production? Read
  [Day 2 Operations](../../guides/day-2-operations.md),
  [Observability & Diagnostics](../../guides/observability.md), and
  [Multi-Tenant Deployment](../../guides/multi-tenant-deployment.md).
- Diving into the code? Begin with
  [Reconciler Architecture](./keystone-reconciler.md) and follow the links
  into the individual sub-reconcilers.
