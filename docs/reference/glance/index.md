---
title: Glance Operator
quadrant: operator
---

# Glance Operator

The Glance operator deploys and manages the OpenStack Image service as a
Kubernetes-native workload. It is the third service operator built on the
shared scaffolding the Keystone operator established (`internal/common`, the
`operator-library` Helm chart, the parameterized operator image), after
Keystone and Horizon.

Glance is the first service operator that is a Keystone **API consumer**: the
API server runs with a `[keystone_authtoken]` service user and validates a
Keystone token on every request, so a plain `spec.keystoneEndpoint` URL points
the pods at the auth endpoint they reach server-side. Its other distinguishing
trait is state: images live in a shared object store, and each store is modeled
out-of-band as a separate [`GlanceBackend`](./glance-backend-crd.md) CR rather
than inline in the Glance spec, so stores can be attached and detached without
editing the Glance CR.

## Design decisions

The v1 operator resolves the onboarding decisions as follows:

- **Release-switched launch mode.** `spec.openStackRelease` governs how the API
  server launches: the eventlet `glance-api` server below `2026.1`, and uWSGI
  from `2026.1` onward. The uWSGI mode loads the stock WSGI app through the
  image-shipped `glance-wsgi-api` shim, because glance's own WSGI module ignores
  `sys.argv` and cannot be pointed at the operator's config directories
  directly. The launch mode is deliberately decoupled from the image tag so a
  digest-pinned image still resolves a schema and launch mode. See
  [Container Images](../ci-cd/container-images.md#glance).
- **Always-rendered reserved stores.** Glance registers the
  `os_glance_staging_store` and `os_glance_tasks_store` filesystem stores at
  `/var/lib/glance/staging` and `/var/lib/glance/tasks-work` on every
  deployment, even an all-object-store
  one: import staging and async task work land on local disk regardless of the
  image store. `enabled_import_methods` is rendered as `[web-download,copy-image]`
  — `glance-direct` is deliberately excluded because it stages the uploaded
  image on the API pod's local disk, and there is no staging volume shared
  across replicas, so an import begun on one pod could not be finished by
  another.
- **`isDefault` lives on the backend CR.** Exactly one attached,
  credential-ready `GlanceBackend` must be marked `isDefault`; that backend
  becomes the `[glance_store] default_backend`. The glance-side sub-reconciler
  and a sibling-uniqueness webhook both enforce the single-default invariant.
- **Keystone endpoints: two plain URL fields.** `spec.keystoneEndpoint` renders
  as `[keystone_authtoken] auth_url` (the pod-reachable, server-side URL) and
  the optional `spec.keystonePublicEndpoint` as `www_authenticate_uri` (the
  browser/client-facing address a 401 points at). The service-user password is
  never rendered into config: it is delivered as the
  `OS_KEYSTONE_AUTHTOKEN__PASSWORD` environment variable, digested into a
  pod-template annotation so a rotation rolls the pods.
- **`/healthcheck` probes.** Readiness and liveness both GET `/healthcheck`,
  served by the oslo healthcheck middleware without touching the database or
  Keystone, identical in both launch modes.
- **Plain `db sync`, no upgrade phases.** Glance's schema migrations run in a
  single `glance-manage db sync` pass, so — unlike Keystone — there is no
  expand-migrate-contract phase machine, no `spec`/`status` upgrade-phase field,
  and no separate schema-check Job.
- **No live S3 probing by the operator.** The operator never connects to an S3
  endpoint to validate a backend; it only resolves the credentials Secret and
  renders the store section. Bucket reachability is a runtime concern of the
  Glance pods.

## Owned resources

For a Glance CR named `{name}` the operator manages:

| Resource | Name | Purpose |
| --- | --- | --- |
| Deployment | `{name}` | The Glance API pods (port 9292) |
| Service | `{name}` | ClusterIP in front of the API pods on port 9292 |
| PodDisruptionBudget | `{name}` | `minAvailable: 1` (or `maxUnavailable: 1` at a single replica) |
| HorizontalPodAutoscaler | `{name}` | Only when `spec.autoscaling` is set |
| NetworkPolicy | `{name}` | Only when `spec.networkPolicy` is set |
| HTTPRoute | `{name}` | Only when `spec.gateway` is set |
| ConfigMap | `{name}-config-<hash>` | Immutable, content-addressed `glance-api.conf` / `glance-api-paste.ini` (3 historical retained) |
| Secret | `{name}-backends-<hash>` | Immutable, content-addressed `backends.conf` (the aggregated store sections; 3 historical retained) |
| Secret | `{name}-db-connection` | Derived pymysql DSN, consumed via `OS_DATABASE__CONNECTION` |
| Job | `{name}-db-sync` | `glance-manage db sync` schema migration |

## Reference pages

- [Glance CRD](./glance-crd.md) — the full `spec`/`status` contract
- [GlanceBackend CRD](./glance-backend-crd.md) — the per-store attachment CRD
- [Controller Events](./glance-events.md) — the Kubernetes events the
  controller emits
- [Reconciler Architecture](./glance-reconciler.md) — the sub-reconciler
  pipeline, conditions, and requeue semantics
