---
title: GlanceBackend CRD API Reference
quadrant: operator
---

# GlanceBackend CRD API Reference

Reference documentation for the GlanceBackend Custom Resource Definition. One
CR attaches to a [Glance](./glance-crd.md) CR via `spec.glanceRef` and describes
one image store. Phase 1 ships the S3-compatible object store (`type: S3`); the
`File` store is deliberately never supported, because a shared object store is
required so every Glance replica reads and writes the same images.

The attachment is inverted: the backend points at the Glance, not the other way
round, so stores are added and removed without editing the Glance CR. A
dedicated controller owns the backend lifecycle — credential resolution and the
per-backend conditions — while the glance-side sub-reconciler aggregates all
attached, credential-ready backends into the rendered store config, promoting
the one marked `isDefault` to the default store. For that controller topology
see [Glance Reconciler Architecture](./glance-reconciler.md).

The CRD is generated from
`operators/glance/api/v1alpha1/glancebackend_types.go`; the webhook lives in
`glancebackend_webhook.go` and the controllers in
`operators/glance/internal/controller/glancebackend_controller.go` and
`reconcile_backends.go`.

## API Group and Version

| Field | Value |
| --- | --- |
| Group | `glance.openstack.c5c3.io` |
| Version | `v1alpha1` |
| Kind | `GlanceBackend` |
| List Kind | `GlanceBackendList` |
| Scope | Namespaced |

**Printer columns:** `kubectl get glancebackends` shows Ready
(`.status.conditions[?(@.type=='Ready')].status`), Type (`.spec.type`), Default
(`.spec.isDefault`), Glance (`.spec.glanceRef.name`), and Age.

## Example

```yaml
apiVersion: glance.openstack.c5c3.io/v1alpha1
kind: GlanceBackend
metadata:
  name: garage-a
  namespace: openstack
spec:
  glanceRef:
    name: glance-multistore
  type: S3
  isDefault: true
  s3:
    host: http://garage.openstack.svc.cluster.local:3900
    bucket: glance-images
    region: garage
    credentialsSecretRef:
      name: garage-s3-credentials
status:
  conditions:
  - type: CredentialsReady
    status: "True"
    reason: CredentialsAvailable
  - type: ConfigProjected
    status: "True"
    reason: ConfigProjected
  - type: Ready
    status: "True"
    reason: AllReady
```

The `metadata.name` (`garage-a`) becomes the Glance store identifier: it is the
`enabled_backends` entry `garage-a:s3` and the `[garage-a]` store section in the
rendered config.

## Spec

### GlanceBackendSpec

Three schema-level CEL rules hold even when the webhook is down: `glanceRef` and
`type` are immutable (UPDATE transition rules), and the `type`/`s3` union rule
`(self.type == 'S3') == has(self.s3)` enforces exactly one backend block
matching `spec.type`.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `glanceRef` | [`GlanceRefSpec`](#glancerefspec) | Yes | — | Names the Glance CR in the same namespace. **Immutable** (CEL transition rule): re-pointing a backend would strand the old deployment's store and race the new projection; delete and recreate instead. The Glance need not exist at admission time (GitOps ordering) — a dangling reference surfaces as `Ready=False`. |
| `type` | `GlanceBackendType` | Yes | — | Store driver; Phase 1 supports `S3` only (enum). **Immutable** (CEL transition rule). |
| `s3` | [`*S3BackendSpec`](#s3backendspec) | When `type: S3` | — | The S3-compatible object store. Required exactly when `type` is `S3` (union rule). |
| `isDefault` | `bool` | No | `false` | Marks this backend the Glance default store (`[glance_store] default_backend`). **Mutable.** Exactly one attached, credential-ready backend must be the default; the glance-side sub-reconciler and a sibling-uniqueness webhook both enforce the single-default invariant. |
| `extraOptions` | `map[string]string` | No | — | Free-form `[<name>]` store-section options not covered by the typed fields, keyed by bare option name. `MaxProperties=32`, and a CEL rule bounds each key at 256 and each value at 1024 characters. See the [denylist](#extraoptions-denylist). |

### GlanceRefSpec

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | `string` | Yes | The referenced Glance CR's name (`MinLength=1`). |

### S3BackendSpec

Optional fields are only rendered into the store section when set, so upstream
Glance defaults apply for everything left unset.

| Field | Type | Required | Default | Rendered `[<name>]` option | Description |
| --- | --- | --- | --- | --- | --- |
| `host` | `string` | Yes | — | `s3_store_host` | S3 endpoint URL; `MinLength=1`, pattern `^https?://` |
| `bucket` | `string` | Yes | — | `s3_store_bucket` | The bucket images are stored in; `MinLength=1` |
| `credentialsSecretRef` | [`SecretNameRefSpec`](#secretnamerefspec) | Yes | — | `s3_store_access_key` / `s3_store_secret_key` | The Secret holding the S3 credentials under the **fixed data keys** `access-key-id` and `secret-access-key` (see below) |
| `bucketURLFormat` | `string` | No | `path` | `s3_store_bucket_url_format` | `path` (`https://host/bucket`) or `virtual` (`https://bucket.host`) — enum |
| `region` | `string` | No | — | `s3_store_region_name` | The S3 region the bucket lives in |
| `createBucketOnPut` | `bool` | No | `false` | `s3_store_create_bucket_on_put` | Create the bucket on first write; only rendered when true |
| `largeObjectSize` | `*int32` (Minimum=1) | No | — | `s3_store_large_object_size` | Multipart-upload threshold in MiB; only rendered when set |
| `largeObjectChunkSize` | `*int32` (Minimum=1) | No | — | `s3_store_large_object_chunk_size` | Multipart chunk size in MiB; only rendered when set |

**Credentials data-key contract.** The Secret referenced by
`credentialsSecretRef` MUST carry exactly two data keys, pinned by contract (not
selectable per CR) and matching the repo's Garage S3 seeding:

| Data key | Rendered option |
| --- | --- |
| `access-key-id` | `s3_store_access_key` |
| `secret-access-key` | `s3_store_secret_key` |

### SecretNameRefSpec

Unlike `commonv1.SecretRefSpec`, this reference carries no `key` field — the
data keys are fixed by contract, so there is nothing to select.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | `string` | Yes | The referenced Secret's name (`MinLength=1`). |

## extraOptions Denylist

The validating webhook rejects `extraOptions` keys the projection owns, so the
escape hatch cannot silently contradict the typed spec. Every key must first
match `^[A-Za-z0-9_]+$` (letters, digits, underscore); the pattern runs before
the denylist so an embedded newline or a denylist-evading trailing space cannot
slip through. Values carrying a newline or carriage-return are rejected as an
INI-injection guard.

| Group | Rejected keys |
| --- | --- |
| Rendered from typed fields | `s3_store_host`, `s3_store_bucket`, `s3_store_access_key`, `s3_store_secret_key`, `s3_store_bucket_url_format`, `s3_store_region_name`, `s3_store_create_bucket_on_put`, `s3_store_large_object_size`, `s3_store_large_object_chunk_size` |
| Operator-owned | `store_description` |

Separately, a backend's `metadata.name` becomes its store identifier and
`[<name>]` config section, so the webhook rejects a name that collides with a
reserved Glance section: the exact names `default` (case-insensitively),
`database`, `keystone_authtoken`, `glance_store`, `paste_deploy`, `oslo_policy`,
`os_glance_staging_store`, `os_glance_tasks_store`, and any name using the
reserved `os_glance_` / `os-glance-` prefix (Glance owns that namespace for its
staging and tasks stores).

## Default-Backend Semantics

Exactly one attached, credential-ready backend must be marked `isDefault`. Two
gates enforce this:

- **Admission (sibling uniqueness).** When a backend under validation sets
  `isDefault`, the webhook lists its namespace siblings (an uncached read),
  filters to the same `spec.glanceRef.name`, skips self and `Terminating`
  siblings, and rejects if another default already exists.
- **Reconcile (exactly-one rule).** The glance-side sub-reconciler counts the
  credential-ready defaults. Zero or more than one is an invalid projection:
  it sets `BackendsReady=False / NoDefaultBackend`, re-renders **nothing**, and
  the config step retains the last-good artefacts the live Deployment mounts.

Because the default store is rendered as `[glance_store] default_backend` in
`glance-api.conf` (the config ConfigMap) and **not** into `backends.conf`, the
content of the backends Secret is invariant to an `isDefault` flip: flipping the
default re-renders the config ConfigMap, not the content-hashed backends Secret.

### Conditions

The dedicated `GlanceBackendReconciler` is the **single writer** of this status.
The glance-side sub-reconciler only reads `CredentialsReady` (it gates config
projection) and writes the aggregated `BackendsReady` condition onto the Glance
CR instead.

| Type | Owner | Status | Reason | Meaning |
| --- | --- | --- | --- | --- |
| `CredentialsReady` | GlanceBackend | True | `CredentialsAvailable` | The credentials Secret exists and carries `access-key-id` / `secret-access-key`. |
| `CredentialsReady` | GlanceBackend | False | `WaitingForCredentials` | The Secret is absent or missing a contract key (polled as a backstop). |
| `ConfigProjected` | GlanceBackend | True | `ConfigProjected` | The parent Glance Deployment mounts a `backends.conf` carrying this backend's `[<name>]` store section. |
| `ConfigProjected` | GlanceBackend | False | `WaitingForProjection` | The projection has not landed in the Deployment yet. |
| `Ready` | GlanceBackend | True | `AllReady` | Both sub-conditions are True. |
| `Ready` | GlanceBackend | False | `NotAllReady` | At least one sub-condition is not True. |
| `BackendsReady` | Glance | True | `AllBackendsProjected` | Every attached backend is credential-ready and projected, with a valid default. |
| `BackendsReady` | Glance | False | `WaitingForBackends` | A valid default exists but at least one backend is pending (not yet credential-ready, or skipped for a per-backend fault); the ready subset is still projected. |
| `BackendsReady` | Glance | False | `NoDefaultBackend` | Zero or more than one credential-ready backend is marked `isDefault`; last-good config is retained. |

**Per-backend fault isolation.** When a backend's credentials Secret is
missing/unreadable, or a rendered value carries a control character, the
glance-side step **skips that backend**, emits a `GlanceBackendSkipped` Warning
event on the Glance CR, and keeps projecting the healthy siblings — a single bad
backend never fails the whole aggregation. The `GlanceBackend` controller itself
emits **no events**: its `CredentialsReady` / `ConfigProjected` / `Ready`
conditions carry the entire per-backend contract.

## Immutability and Validation Summary

Schema-layer rules (CEL / kubebuilder markers, enforced even when the webhook is
down): the `glanceRef` and `type` transition rules, the `type`/`s3` union, the
`S3` type enum, the `host` URL pattern and `MinLength` guards, the
`bucketURLFormat` enum, the `largeObjectSize`/`largeObjectChunkSize` minimums,
and the `extraOptions` `MaxProperties` / key-and-value-length bounds.

Webhook rules (defense-in-depth plus the rules CEL cannot express): the union
re-check, the reserved-store-name collision guard on `metadata.name`, the
`extraOptions` key allowlist and denylist, the INI-injection guard on
`extraOptions` values, and the single-default sibling-uniqueness check. The
`bucketURLFormat` default (`path`) is materialized by the defaulting webhook for
callers that bypass it.

## Chainsaw E2E Tests

The end-to-end multi-store flow — a two-backend Glance against an in-suite
Garage S3, credential resolution, and store projection — lives in
`tests/e2e/glance/s3-multistore`. The default-store switch (flipping `isDefault`
between siblings and re-rendering `default_backend`) lives in
`tests/e2e/glance/default-backend-switch`. The detach path (deleting a backend
and de-projecting its store) lives in `tests/e2e/glance/deletion-cleanup`. The
rejection corpus lives in `tests/e2e/glance/invalid-glancebackend-cr`.

## Retained Artefacts

The rendered store sections live in a content-hashed
`<glance>-backends-<hash>` Secret owned by the parent Glance CR — up to 3
historical copies are retained for fast rollback, and all are garbage-collected
with the Glance CR. Its single data key is `backends.conf`, an INI document with
one `[<name>]` store section per attached, credential-ready backend, mounted
into the API pods at `/etc/glance/backends.conf.d/` as a second oslo.config
`--config-dir` root.

Each backend's store identifier is its CR name: the section header is `[<name>]`
and the `[DEFAULT] enabled_backends` list carries the entry `<name>:s3`.
