---
title: Placement Operator
quadrant: operator
---

# Placement Operator

The Placement operator deploys and manages the OpenStack Placement service as a
Kubernetes-native workload. It is the fourth service operator built on the
shared scaffolding the Keystone operator established (`internal/common`, the
`operator-library` Helm chart, the parameterized operator image), after
Keystone, Horizon, and Glance.

Placement is the second Keystone API consumer after Glance: the API server runs
with a `[keystone_authtoken]` service user and validates a Keystone token on
every request, so a plain `spec.keystoneEndpoint` URL points the pods at the
auth endpoint they reach server-side. Its profile is the smallest of the
database-backed services so far. An API, a MariaDB schema, and a service user
are the whole surface: no stores, no message bus, no satellite CRD.

## Design decisions

The v1 operator resolves the onboarding decisions as follows:

- **One uWSGI launch mode for both releases.** Placement never shipped an
  eventlet server, so it carries no release switch of the kind Glance has and no
  worker count outside uWSGI. The command loads the application through
  `--wsgi-file /var/lib/openstack/bin/placement-api`, the WSGI entry file the
  operator's Placement image writes, and the config location travels in the
  `OS_PLACEMENT_CONFIG_DIR=/etc/placement` environment variable. No `--pyargv`
  is emitted: the entry calls `init_application()` with no arguments and
  placement's `_get_config_files` never reads `sys.argv`, so a value passed
  there would reach nothing. See
  [Container Images](../ci-cd/container-images.md#placement).
- **Probes target `GET /`.** Readiness and liveness both GET `/`, the
  version-discovery document, which placement answers without a token and
  without opening the database. The oslo healthcheck middleware Glance probes at
  `/healthcheck` has nowhere to be wired here: placement composes its middleware
  stack in code and reads no paste pipeline, so the operator renders no
  `api-paste.ini` and the CRD carries no `middleware` field.
- **No upgrade phase machine.** The CR has no `upgradePhase` status field. A
  release bump runs the one `{name}-db-sync` Job, which chains
  `placement-manage db sync`, `placement-manage db online_data_migrations`, and
  `placement-status upgrade check`. The upgrade check exits 1 for warnings,
  which the Job tolerates, and 2 for errors, which fails it. Upgrade paths stay
  sequential and a downgrade is refused before any Job runs.
- **No recurring maintenance task.** `placement-manage` carries four
  subcommands (`db sync`, `db version`, `db stamp`, `db online_data_migrations`)
  and none of them purges, archives, or cleans up. The placement schema does not
  soft-delete either, so no backlog of dead rows accumulates to reclaim. The
  operator projects no maintenance CronJob, and the validating webhook carries
  no `metadata.name` length bound. Glance needs one because Kubernetes caps a
  CronJob name at 52 characters, which its `db-purge` suffix spends down to 43
  for the CR name; every placement-owned object fits inside the 63 characters
  Kubernetes already allows a CR name.
- **Service-user and DSN delivery.** `[keystone_authtoken]` renders without the
  password. The service-user password arrives as the
  `OS_KEYSTONE_AUTHTOKEN__PASSWORD` environment variable and the assembled DSN
  as `OS_PLACEMENT_DATABASE__CONNECTION`, both sourced from Secrets, so no
  credential material enters the namespace-readable ConfigMap. Each value is
  digested into a pod-template annotation, so a rotation at the source rolls the
  pods.
- **`[placement_database]`, not `[database]`.** Placement reads `connection`,
  `max_retries`, and `connection_recycle_time` from its own
  `[placement_database]` section, and `auth_strategy` from `[api]`, where
  upstream moved the option away from the deprecated `[DEFAULT]` spelling. Both
  spellings of `auth_strategy` are registered as operator-owned and refused in
  `spec.extraConfig`, because `extraConfig` has the last word in the merge and
  `noauth2` would put the API on the no-auth middleware.

## Owned resources

For a Placement CR named `{name}` the operator manages:

| Resource | Name | Purpose |
| --- | --- | --- |
| Deployment | `{name}` | The Placement API pods (port 8778) |
| Service | `{name}` | ClusterIP in front of the API pods on port 8778 |
| PodDisruptionBudget | `{name}` | `minAvailable: 1` (or `maxUnavailable: 1` at a single replica) |
| HorizontalPodAutoscaler | `{name}` | Only when `spec.autoscaling` is set |
| NetworkPolicy | `{name}` | Only when `spec.networkPolicy` is set |
| HTTPRoute | `{name}` | Only when `spec.gateway` is set |
| ConfigMap | `{name}-config-<hash>` | Immutable, content-addressed `placement.conf` (plus `policy.yaml` / `logging.conf` when applicable; 3 historical retained) |
| Secret | `{name}-db-connection` | Derived pymysql DSN, consumed via `OS_PLACEMENT_DATABASE__CONNECTION` |
| Job | `{name}-db-sync` | Schema migration, online data migrations, and the upgrade check |
| MariaDB `Database` / `User` / `Grant` | `{name}` | Managed mode only (`spec.database.clusterRef`); a brownfield database is left alone |

## Reference pages

- [Placement CRD](./placement-crd.md) — the full `spec`/`status` contract
- [Controller Events](./placement-events.md) — the Kubernetes events the
  controller emits
- [Reconciler Architecture](./placement-reconciler.md) — the sub-reconciler
  pipeline, conditions, and requeue semantics
