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
  image store. Both are `emptyDir`s bounded by a default `sizeLimit` of 10Gi
  (`spec.staging.sizeLimit`), so an oversized import gets the glance-api pod
  evicted rather than running until the node's disk is full; see
  [StagingSpec](./glance-crd.md#stagingspec) for what that bound does and does
  not guarantee.
  `enabled_import_methods` is rendered as `[web-download,copy-image]`
  — `glance-direct` is deliberately excluded because it stages the uploaded
  image on the API pod's local disk, and there is no staging volume shared
  across replicas, so an import begun on one pod could not be finished by
  another. Every deployment also renders an `[import_filtering_opts]` group, so
  a `web-download` URI is filtered before glance fetches it: HTTPS on port 443,
  plus a literal host denylist covering loopback, the link-local metadata
  address, and the in-cluster API server. See
  [ImportFilteringSpec](./glance-crd.md#importfilteringspec).
- **Image cache: sqlite driver, bounded `emptyDir`, sidecar pruner.**
  `spec.imageCache` turns on a per-replica cache of the image data the API has
  served. The driver is pinned to `sqlite` even though upstream deprecates it in
  favour of `centralized_db`: that default keys cache state in the Glance
  database per `worker_self_reference_url`, so with Deployment pods whose names
  change on every replacement it strands `node_reference` and `cached_images`
  rows there and turns every cache hit into a database write. `sqlite` keeps the
  metadata inside the cache directory, where it shares the volume's lifecycle.
  The directory is a bounded `emptyDir` and not a PVC, so the operator assumes
  no StorageClass, reuses the bounded-scratch pattern the staging volumes
  already establish, and avoids a bound that would be fiction on kind, whose
  local-path provisioner enforces no capacity. Pruning runs in a
  `cache-maintenance` sidecar because a CronJob cannot reach a pod-local volume.
  See [ImageCacheSpec](./glance-crd.md#imagecachespec).
- **Import plugins: an opt-in block per plugin, rendered in a fixed order.**
  `spec.importPlugins` selects the image-import plugins, and the presence of a
  sub-block is the switch for its plugin. The rendered order
  (`image_decompression`, `image_conversion`, `inject_image_metadata`) is the
  operator's and not an input, which is what satisfies upstream's one ordering
  requirement: decompression has to precede conversion, or conversion would
  rewrite the archive instead of the disk image inside it.
  `image_import_plugins` is rendered on every deployment, `[]` while the block
  is unset, so a missing key never leaves Glance on its own default. The plugins
  are stages of the interoperable import flow, so a
  `PUT /v2/images/{id}/file` upload bypasses them and `copy-image` skips them,
  which leaves `web-download` imports. The Glance image ships `qemu-img` and
  `lhafile`, the two the conversion and decompression plugins shell out to and
  import at run time. See
  [ImportPluginsSpec](./glance-crd.md#importpluginsspec).
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
- **Expand-migrate-contract upgrades.** When `spec.openStackRelease` advances
  to a new OpenStack release (with the image in lockstep), the operator drives
  phased database migrations while the API keeps serving. Sequential-only
  upgrade paths; a fresh install or a same-release image bump stays on the
  single-pass `glance-manage db sync`. See
  [Upgrade Flow](./glance-upgrade-flow.md).
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
| CronJob | `{name}-db-purge` | Scheduled purge of soft-deleted task rows (image rows opt-in) |

## Reference pages

- [Glance CRD](./glance-crd.md) — the full `spec`/`status` contract
- [GlanceBackend CRD](./glance-backend-crd.md) — the per-store attachment CRD
- [Controller Events](./glance-events.md) — the Kubernetes events the
  controller emits
- [Reconciler Architecture](./glance-reconciler.md) — the sub-reconciler
  pipeline, conditions, and requeue semantics
- [Upgrade Flow](./glance-upgrade-flow.md) — the expand-migrate-contract
  release-upgrade machine
