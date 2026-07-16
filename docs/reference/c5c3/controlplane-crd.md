---
title: ControlPlane CRD API Reference
quadrant: operator
---

# ControlPlane CRD API Reference

Reference documentation for the c5c3-operator ControlPlane Custom Resource
Definition. The ControlPlane CRD is the top-level aggregate that
projects an OpenStack control plane: it owns the shared infrastructure
references (database, cache), a curated set of per-service specs (today:
Keystone), and the K-ORC (OpenStack Resource Controller) integration that
bootstraps and rotates the admin application credential. The reconciler (L2)
materializes this aggregate into the individual per-service CRs — see the
[ControlPlane Reconciler reference](./controlplane-reconciler.md) for the
reconciliation flow.

The c5c3 API group also ships two companion kinds: `CredentialRotation`
(a one-shot credential-rotation request) and `SecretAggregate` (types-only at
this level; the reconciler is deferred). All three are documented here.

The API surface is intentionally **smaller** than the
[Keystone CRD](../keystone/keystone-crd.md): the ControlPlane curates a subset
of each service's knobs and derives the rest from operator policy, rather than
re-exposing every service field through the aggregate.

## API Group and Version

| Field | Value |
| --- | --- |
| Group | `c5c3.io` |
| Version | `v1alpha1` |
| Scope | Namespaced |

| Kind | List Kind |
| --- | --- |
| `ControlPlane` | `ControlPlaneList` |
| `CredentialRotation` | `CredentialRotationList` |
| `SecretAggregate` | `SecretAggregateList` |

**Import path:**

```go
import c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
```

**Scheme registration:**

The `init()` functions in `controlplane_types.go`, `credentialrotation_types.go`,
and `secretaggregate_types.go` each register their kind (and List kind) with the
shared `SchemeBuilder`. The operator entrypoint registers the group with the
manager's scheme through `internal/common/bootstrap` (which calls
`AddToScheme`), so every kind in the group is available to the manager:

```go
utilruntime.Must(c5c3v1alpha1.AddToScheme(scheme))
```

The manager runs with `LeaderElectionID` `c5c3.openstack.c5c3.io`.

---

## Resource Shape — ControlPlane

```yaml
apiVersion: c5c3.io/v1alpha1
kind: ControlPlane
metadata:
  name: controlplane
  namespace: openstack
spec:
  openStackRelease: "2025.2"
  region: RegionOne
  infrastructure:
    database:
      clusterRef:
        name: mariadb
      database: keystone
      # credentialsMode selects how the service DB credential is provisioned.
      # Managed mode defaults to Dynamic (engine-issued, short-lived credentials
      # from the OpenBao database engine); set Static to opt out (migration
      # staging / brownfield). reconcileDBCredentials projects a per-ControlPlane
      # VaultDynamicSecret generator (Dynamic) or the stage-(a) KV-backed
      # ExternalSecret (Static). Dynamic requires clusterRef (managed mode).
      # credentialsMode: Dynamic
      # In managed mode (clusterRef set) database.secretRef is
      # operator-owned — reconcileDBCredentials materialises a per-ControlPlane
      # Secret and the projected Keystone CR's secretRef is overridden to
      # {name}-keystone-db-credentials (key "password"); the value below is not
      # what Keystone consumes. A brownfield CR (database.host) must instead
      # supply its own database.secretRef Secret out-of-band.
      secretRef:
        name: keystone-db-credentials
        key: password
    cache:
      backend: dogpile.cache.pymemcache
      clusterRef:
        name: memcached
  services:
    keystone:
      replicas: 3
      rotationInterval: 168h
      gateway:
        parentRef:
          name: openstack-gw
        hostname: keystone.example.com
        path: /
      publicEndpoint: https://keystone.example.com/v3
  korc:
    adminCredential:
      cloudCredentialsRef:
        cloudName: admin
        secretName: k-orc-clouds-yaml
      passwordSecretRef:
        name: keystone-admin
        key: password
      applicationCredential:
        restricted: true
        rotation:
          mode: PasswordDriven
      bootstrapResources:
        - kind: Project
          name: admin
        - kind: Role
          name: admin
status:
  conditions:
    - type: Ready
      status: "True"
      reason: AllReady
      message: All sub-conditions are ready
      lastTransitionTime: "2026-06-02T00:00:00Z"
  observedGeneration: 4
  updatePhase: Idle
  services:
    - name: keystone
      ready: true
      release: "2025.2"
  adminApplicationCredential:
    id: 6f3c…
    restricted: true
    lastRotation: "2026-06-02T00:00:00Z"
```

### Printer Columns

`kubectl get controlplanes` displays these columns:

| Column | JSON Path | Type |
| --- | --- | --- |
| Ready | `.status.conditions[?(@.type=='Ready')].status` | string |
| Release | `.spec.openStackRelease` | string |
| Age | `.metadata.creationTimestamp` | date |

The status subresource is enabled via `+kubebuilder:subresource:status`.

---

## Resource Shape — CredentialRotation

```yaml
apiVersion: c5c3.io/v1alpha1
kind: CredentialRotation
metadata:
  name: rotate-admin
  namespace: openstack
spec:
  target: adminApplicationCredential
  bootstrap: false
  reMint: true
status:
  conditions:
    - type: Ready
      status: "True"
      reason: RotationTriggered
      lastTransitionTime: "2026-06-02T00:00:00Z"
  observedGeneration: 1
```

### Printer Columns

`kubectl get credentialrotations` displays these columns:

| Column | JSON Path | Type |
| --- | --- | --- |
| Target | `.spec.target` | string |
| Ready | `.status.conditions[?(@.type=='Ready')].status` | string |
| Age | `.metadata.creationTimestamp` | date |

---

## ControlPlaneSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `openStackRelease` | `string` | Yes | — | OpenStack release the control plane targets (e.g. `"2025.2"`). The reconciler (L2) projects this into each service CR's image tag. Must match the date-based release pattern `^\d{4}\.[12]$`, enforced by both the CRD `+kubebuilder:validation:Pattern` marker and the validating webhook. Upgrades are allowed on update, but **downgrades are rejected** (Keystone DB migrations are forward-only). Stays required in **both** keystone modes; in **External** mode it is **advisory** — no images are deployed, so the value only needs to match the external installation's release at the phase-3 managed takeover. |
| `region` | `string` | No | `"RegionOne"` | OpenStack region name applied across the control plane. Projected into the Keystone CR's `bootstrap.region`. Defaulted to `RegionOne` by **both** the `+kubebuilder:default` marker (normal admission) and the defaulting webhook (callers that bypass the CRD default). Immutable after create (the projected `bootstrap.region` is itself immutable). |
| `infrastructure` | [`*InfrastructureSpec`](#infrastructurespec) | Conditional | managed-mode defaulted | Shared backing services (database, cache) the control plane's services connect to. **Required** when `services.keystone.mode` is `Managed` (or unset, or `services.keystone` unset) — the defaulting webhook materializes a managed-mode `database`/`cache` when omitted, and the validating webhook rejects a non-External ControlPlane without it. **Forbidden** in **External** mode (an External ControlPlane provisions no backing services; phase 2 relaxes this to optional). The mode-conditional required/forbidden rule is webhook-enforced because CEL cannot span `spec.infrastructure` and `spec.services.keystone`; see [InfrastructureSpec](#infrastructurespec) and [Validation Rules](#validation-rules). |
| `services` | [`ServicesSpec`](#servicesspec) | Yes | — | Per-service configuration projected into the individual service CRs. |
| `globalPolicyOverrides` | [`*commonv1.PolicySpec`](../keystone/keystone-crd.md#policyspec) | No | `nil` | oslo.policy overrides applied across every service in the control plane. Per-service overrides (e.g. `services.keystone.policyOverrides`) take precedence over these global rules when both are set. |
| `secretStoreRef` | [`*commonv1.SecretStoreRefSpec`](#secretstorerefspec) | No | `nil` (defaults to the shared cluster store `openbao-cluster-store`) | Selects the External Secrets store the control plane routes its ExternalSecrets and backup PushSecrets through, and is **projected onto the Keystone and Horizon children** — so operators normally set the store here rather than on the individual service CRs. **Mutable:** switching stores is supported — the operator moves the fernet/credential key material in place, never re-creating it. When omitted, defaults to the shared cluster-scoped `ClusterSecretStore` named `openbao-cluster-store`, so existing deployments are unchanged; set `{kind: SecretStore, name: <store>}` to reach OpenBao as a per-tenant identity resolved in the ControlPlane's own namespace. See [SecretStoreRefSpec](#secretstorerefspec). |
| `korc` | [`KORCSpec`](#korcspec) | No | defaulted | K-ORC integration used to bootstrap and rotate the admin application credential and any declared bootstrap resources. Optional — the defaulting webhook fills `adminCredential` (cloudCredentialsRef, passwordSecretRef, applicationCredential restriction/rotation) from well-known defaults when omitted. |

### SecretStoreRefSpec

`spec.secretStoreRef` selects the External Secrets store the control plane and
its projected children route their ExternalSecrets and backup PushSecrets
through. It reuses the shared `commonv1.SecretStoreRefSpec` — a `kind`
(`ClusterSecretStore` \| `SecretStore`, defaulted to `ClusterSecretStore`) plus a
required non-empty `name`; see the canonical two-field table in the
[Keystone CRD → SecretStoreRefSpec](../keystone/keystone-crd.md#secretstorerefspec).

When omitted the field defaults to the shared cluster-scoped `ClusterSecretStore`
named `openbao-cluster-store`, so existing deployments are unchanged. Set
`{kind: SecretStore, name: <store>}` to reach OpenBao as a per-tenant identity,
always resolved in the ControlPlane's own namespace (there is no namespace
field). The field is **mutable** — switching stores is supported, and the
operator moves the fernet/credential key material in place rather than
re-creating it. Its value is **projected onto the Keystone and Horizon
children**, so operators normally set it on the ControlPlane rather than on the
individual service CRs.

---

## InfrastructureSpec

Declares the shared backing services for the control plane. Both
fields reuse the canonical `commonv1` shapes so the ControlPlane and the
per-service CRs validate the database/cache the same way.

`spec.infrastructure` (and each of its `database` / `cache` blocks)
may be **omitted entirely** on a minimal managed-mode ControlPlane. The
defaulting webhook constructs a managed-mode database (`clusterRef:
openstack-db`, `database: keystone`, `secretRef.name: keystone-db`) and a
managed-mode cache (`clusterRef: openstack-memcached`, `backend:
dogpile.cache.pymemcache`) before validation runs. The two managed `clusterRef`
names are only invented when the brownfield discriminator (`database.host` /
`cache.servers`) is unset, so the database/cache XOR rule below still passes for
a brownfield CR — the webhook never coerces an explicit brownfield endpoint into
managed mode. See the [Defaulting Webhook](#defaulting-webhook) for the exact
conditions and mechanism.

The defaulted `database.secretRef.name` (`keystone-db`) is a **managed-mode
convenience name only** — in managed mode `database.secretRef` is operator-owned
and the projected Keystone CR's `secretRef` is overridden to a per-ControlPlane
Secret, and a brownfield CR must supply its own. See the [`database` field
notes](#infrastructurespec) below.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `database` | [`commonv1.DatabaseSpec`](../keystone/keystone-crd.md#databasespec) | No | managed `clusterRef: openstack-db`, `database: keystone`, `secretRef.name: keystone-db` | MariaDB connection parameters shared by the control plane. Supports managed (`clusterRef`) and brownfield (`host`) modes; exactly one must hold **after defaulting** (enforced by the CRD CEL `XValidation` rule and the validating webhook — see [Validation Rules](#validation-rules)). Optional because the defaulting webhook materializes a managed-mode block when omitted. **`database.secretRef` ownership:** in managed mode this reference is **operator-owned** — `reconcileDBCredentials` materialises a per-ControlPlane DB-credential Secret and the reconciler overrides the projected Keystone CR's `spec.database.secretRef` to point at it, so the `keystone-db` default `secretRef.name` is only a managed-mode convenience name (it is **not** what Keystone consumes and no longer resolves to a cluster Secret). A **brownfield** ControlPlane (`database.host` set, no `clusterRef`) **MUST supply** its own `database.secretRef` Secret out-of-band — the operator projects no ExternalSecret in brownfield mode. See [managed-mode provisioning](#infrastructurespec) below. |
| `cache` | [`commonv1.CacheSpec`](../keystone/keystone-crd.md#cachespec) | No | managed `clusterRef: openstack-memcached`, `backend: dogpile.cache.pymemcache` | Memcached configuration shared by the control plane. Supports managed (`clusterRef`) and brownfield (`servers`) modes; exactly one must hold **after defaulting** (enforced by the CRD CEL `XValidation` rule and the validating webhook). Optional because the defaulting webhook materializes a managed-mode block when omitted. |

<!-- DECISION: `database`/`cache` Required flipped from Yes to No because the
     defaulting webhook now constructs a managed-mode block when the field is omitted.
     The validating webhook still enforces the clusterRef/host (resp. servers) XOR after
     defaulting, so a brownfield CR that sets host/servers is unaffected. -->

In managed mode the reconciler provisions an owned `MariaDB` CR (named after
`database.clusterRef.name`) and an owned `Memcached` CR (named after
`cache.clusterRef.name`) in the ControlPlane's own namespace. The Keystone CR
the reconciler projects points at the **same** `DatabaseSpec` / `CacheSpec`
verbatim, so the aggregate and the projected service agree on the backing
services.

The managed-mode MariaDB topology is derived from
`spec.infrastructure.database.replicas` (default `3`, minimum `1`): the default
projects a production-shaped Galera HA cluster (3 replicas, `galera.enabled`,
`100Gi` storage), while `database.replicas: 1` projects a single-instance,
non-Galera MariaDB so the fresh-create path schedules on a constrained cluster
such as a single-node kind. `cache.replicas` (also default `3`) drives the
Memcached replica count the same way. Both are only honoured in managed mode;
storage stays at `100Gi` regardless of the replica count, and a ControlPlane
that adopts a pre-existing MariaDB/Memcached leaves its topology untouched.

> **`database.secretRef` is operator-owned in managed mode.** The
> `DatabaseSpec` is projected onto the Keystone CR verbatim **except** for its
> `secretRef`. In managed mode the `reconcileDBCredentials` sub-reconciler
> create-or-updates a per-ControlPlane DB-credential `ExternalSecret` named
> `{controlplane.Name}-keystone-db-credentials` in the ControlPlane namespace
> (reading OpenBao path `openstack/keystone/{namespace}/{name}/db`), and
> `reconcileKeystone` then **overrides** the projected Keystone CR's
> `spec.database.secretRef` to `{name: "{controlplane.Name}-keystone-db-credentials",
> key: "password"}` — the operator-owned Secret. The source `cp.Spec` is left
> untouched; only the projected child's `secretRef` value is reassigned. The
> Secret that Keystone actually consumes is therefore the one this reconciler
> materialises, **not** a Secret literally named after `database.secretRef.name`.
> Consequently the `keystone-db` default for `database.secretRef.name` is
> a **managed-mode convenience name only**: the production deploy stack ships
> no `keystone-db` ExternalSecret (only the kind overlay materialises one, for
> standalone Keystone instances), and a managed ControlPlane never consumes
> it either way.
>
> A **brownfield** ControlPlane (`database.host` set, `clusterRef == nil`)
> **MUST supply** `spec.infrastructure.database.secretRef` pointing to a Secret
> it owns out-of-band. In brownfield mode `reconcileDBCredentials` is a no-op
> (it reports `DBCredentialsReady=True`, reason `BrownfieldUserSuppliedCredential`)
> and projects no ExternalSecret, so the operator never materialises the Secret
> — and the `keystone-db` default no longer resolves to a cluster Secret. See the
> [ControlPlane Reconciler reference](./controlplane-reconciler.md) for the
> `reconcileDBCredentials` flow.

---

## ServicesSpec

Declares the per-service configuration of the control plane. Today
Keystone and the Horizon dashboard are modeled; additional services are added as
fields as the operator grows.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `keystone` | [`*ServiceKeystoneSpec`](#servicekeystonespec) | No | `nil` | Configuration for the Keystone service projected by the reconciler. Optional: when unset, this ControlPlane manages no Keystone service (staged adoption, or an externally-managed Keystone) and `KeystoneReady` is reported as not-managed. Flipping it from set to `nil` **preserves** the previously-projected Keystone child by default — deleting it would cascade to the child's irreplaceable `<name>-credential-keys` Secret (and its OpenBao backup), so an accidental unset is fail-safe. Set the `c5c3.io/allow-keystone-deletion: "true"` annotation on the ControlPlane to opt in to deleting the child on unset. |
| `horizon` | [`*ServiceHorizonSpec`](#servicehorizonspec) | No | `nil` | Configuration for the Horizon dashboard projected by the reconciler. Optional: when unset, this ControlPlane manages no dashboard and `HorizonReady` is reported as not-managed (`HorizonNotManaged`), so the aggregate `Ready` is not blocked. **Forbidden in External mode** — the dashboard needs its own External-mode design. Flipping it from set to `nil` preserves the previously-projected Horizon child by default; set the `c5c3.io/allow-horizon-deletion: "true"` annotation to opt in to deleting the child on unset. |

---

## ServiceKeystoneSpec

A **curated local subset** of the knobs the ControlPlane exposes for the
Keystone service.

> **DECISION:** This struct is intentionally **not**
> an import of `keystonev1alpha1.KeystoneSpec`. The reconciler (L2)
> **projects** this struct into a Keystone CR; the database, cache, and Fernet
> rotation schedule of that Keystone CR are **derived** from the ControlPlane
> (`infrastructure.*` and operator policy) rather than set by the user here.
> Keeping a curated subset avoids leaking every Keystone knob through the
> aggregate and keeps the L1 API package free of a dependency on the keystone
> module. Fields not present below (replica strategy, uWSGI, network policy,
> fernet key count, etc.) are governed by the Keystone operator's own defaults on
> the projected CR, not by the ControlPlane.

The `mode` discriminator gives the Keystone service three states, mirroring the
managed-vs-brownfield split of the infrastructure specs at the service level:

| `services.keystone` | Meaning |
| --- | --- |
| unset (`nil`) | No Keystone at all — `KeystoneReady` is reported not-managed (see [ServicesSpec](#servicesspec)). |
| `mode: Managed` (or unset) | The reconciler deploys and owns a full Keystone workload — today's behavior, byte-identical. |
| `mode: External` | Service-less: identity is managed against a pre-existing, externally-operated Keystone at [`external.authURL`](#externalkeystonespec) and no Keystone workload is deployed. |

In **External** mode every managed-only field below (`replicas`, `image`,
`policyOverrides`, `rotationInterval`, `gateway`, `publicEndpoint`) is
**forbidden** and the typed [`external`](#externalkeystonespec) block is
**required**. These intra-struct rules are enforced by type-level CEL
`XValidation` rules (so they hold at the CRD schema layer even when the
validating webhook is bypassed) and mirrored by the validating webhook; see
[Validation Rules](#validation-rules).

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `mode` | `string` (`Managed` \| `External`) | No | `Managed` | Selects whether the Keystone service is **Managed** (the reconciler deploys and owns a full Keystone workload) or **External** (identity is managed against a pre-existing Keystone at `external.authURL` and no workload is deployed). Defaulted to `Managed` by both the `+kubebuilder:default` marker and the defaulting webhook. In External mode the [`external`](#externalkeystonespec) block is required and every managed-only field below is forbidden. |
| `external` | [`*ExternalKeystoneSpec`](#externalkeystonespec) | Conditional | `nil` | Connection parameters for an externally-operated Keystone. **Required** when `mode` is `External`, **forbidden** otherwise (CEL + webhook enforced). |
| `replicas` | `*int32` | No | `nil` (Keystone operator default, 3) | Overrides the number of Keystone API replicas. When `nil`, the reconciler leaves `replicas` unset on the projected Keystone CR, so the Keystone operator applies its own default. Minimum: 1. **Forbidden in External mode.** |
| `image` | [`*commonv1.ImageSpec`](../keystone/keystone-crd.md#imagespec) | No | `nil` | Overrides the Keystone container image. When `nil`, the reconciler derives the image as `ghcr.io/c5c3/keystone:{spec.openStackRelease}`. When set, the whole image reference is used verbatim. |
| `policyOverrides` | [`*commonv1.PolicySpec`](../keystone/keystone-crd.md#policyspec) | No | `nil` | Per-service oslo.policy overrides for Keystone. When set, these take precedence over `spec.globalPolicyOverrides` for the Keystone service. |
| `rotationInterval` | `*metav1.Duration` | No | `nil` | Overrides the Fernet / credential-key rotation interval the reconciler derives for the projected Keystone CR. When `nil`, the reconciler derives a default schedule. When set, the duration is converted to a cron expression and applied to both `fernet.rotationSchedule` and `credentialKeys.rotationSchedule` on the projected Keystone CR. An unconvertible interval (not a positive whole number of days) is **rejected at admission** by the validating webhook; if the webhook is bypassed, the reconciler surfaces `KeystoneReady=False` with reason `InvalidRotationInterval` and returns the error so the reconcile chain stops and requeues with backoff. |
| `gateway` | [`*commonv1.GatewaySpec`](#gatewayspec) | No | `nil` | Exposes the projected Keystone API externally via a Gateway API HTTPRoute. When `nil`, no HTTPRoute is projected and the Keystone API is reachable in-cluster only (its ClusterIP Service). When set, the reconciler projects it onto the Keystone CR's `spec.gateway`, so the Keystone operator attaches an HTTPRoute to the referenced Gateway. When a `gateway` is set its `hostname` must be non-empty — enforced at admission by the validating webhook (see [Validation Rules](#validation-rules)). |
| `publicEndpoint` | `string` | No | `""` | Externally routable Keystone identity endpoint URL (e.g. `https://keystone.example.com/v3`). Projected into the Keystone bootstrap (`--bootstrap-public-url`) and used for the K-ORC identity catalog Endpoint, so external clients resolve the same URL Keystone advertises. When set, it must be an HTTP(S) URL (`+kubebuilder:validation:Pattern=^https?://`), so a malformed endpoint fails at admission rather than wedging the projected Keystone CR. When empty and `gateway` is set, the reconciler derives `https://{gateway.hostname}/v3` (the default-443 form); set it explicitly when the externally reachable port differs (e.g. a kind host-port mapping like `:8443`). |
| `federationProxyImage` | [`*commonv1.ImageSpec`](../keystone/keystone-crd.md#imagespec) | No | `nil` | Overrides the `mod_auth_openidc` sidecar image projected onto the Keystone child's `spec.federation.proxyImage`. When `nil` the reconciler projects `ghcr.io/c5c3/keystone-federation-proxy:latest`. That default is a **mutable tag**: every node re-pulls it on each pod start, and a locally built sidecar cannot be exercised. Override it with a digest-carrying `ImageSpec` for the immutable pin published images are expected to carry. Inert until a federation-typed `KeystoneIdentityBackend` attaches. Forbidden in External mode (CEL + webhook). |
| `dedicatedBackingServices` | [`*KeystoneDedicatedBackingServicesSpec`](#dedicatedbackingservices) | No | `nil` (shares the ControlPlane-wide instances) | Opts the Keystone service **out** of the shared `spec.infrastructure` instances and gives it backing services of its own. Forbidden in External mode (CEL + webhook): no backing services are provisioned at all there. |
| `namespace` | [`*ServiceNamespaceSpec`](#service-namespaces) | No | `nil` (placed in the ControlPlane's namespace) | Places the Keystone service — and the backing services, secret store, and credential material that follow it — in a namespace of its own. Create-only. Forbidden in External mode (CEL + webhook): no Keystone workload is deployed, so there is nothing to place. See [Service Namespaces](#service-namespaces). |

---

## ServiceHorizonSpec

A curated local subset of the knobs the ControlPlane exposes for the Horizon
dashboard, mirroring `ServiceKeystoneSpec`. The reconciler projects it into a
Horizon CR; the cache and the Keystone endpoint of that child are derived from
the ControlPlane rather than set here.

Forbidden entirely when `services.keystone.mode` is `External` (the dashboard
needs its own External-mode design), so — unlike `ServiceKeystoneSpec` — none of
its fields carry per-field External-mode forbid-rules.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `replicas` | `*int32` | No | `nil` | Overrides the number of dashboard replicas. When `nil` the reconciler applies the Horizon operator's own default (3). Minimum 1. |
| `image` | [`*commonv1.ImageSpec`](../keystone/keystone-crd.md#imagespec) | No | `nil` | Overrides the Horizon container image. When `nil` the reconciler derives `ghcr.io/c5c3/horizon:{spec.openStackRelease}`. |
| `gateway` | [`*commonv1.GatewaySpec`](#gatewayspec) | No | `nil` | Exposes the projected dashboard externally via a Gateway API HTTPRoute. When `nil` the dashboard is reachable in-cluster only. |
| `secretKeyRef` | [`*commonv1.SecretRefSpec`](../keystone/keystone-crd.md#secretrefspec) | No | `nil` | Overrides the Secret holding the Django `SECRET_KEY` the dashboard replicas share. When `nil` the reconciler defaults to the kind-infrastructure shim Secret `horizon-secret-key`, which is pinned to the **default** ControlPlane identity — multi-ControlPlane deployments MUST set this explicitly. |
| `publicEndpoint` | `string` | No | `""` | The **browser-observed** dashboard base URL, without a trailing slash and **including a non-default port** (e.g. `https://horizon.example.com:8443`). The reconciler derives the WebSSO origin from it (`publicEndpoint + "/auth/websso/"`) and projects that onto the Keystone child's `spec.federation.trustedDashboards`. Keystone matches the origin the dashboard sends verbatim, so the value must reproduce exactly what the browser's address bar shows. When empty and `gateway` is set, the reconciler derives `https://{gateway.hostname}` (the default-443 form). Must match `^https?://`, parse with a host, and be at most 499 characters — the Keystone child's 512-character bound on `trustedDashboards[]` minus the 13 characters `/auth/websso/` appends. |
| `dedicatedBackingServices` | [`*HorizonDedicatedBackingServicesSpec`](#dedicatedbackingservices) | No | `nil` (shares the ControlPlane-wide cache) | Opts the dashboard **out** of the shared `spec.infrastructure.cache` and gives it a cache of its own. The dashboard consumes no database, so `cache` is the only class it can take dedicated. |
| `namespace` | [`*ServiceNamespaceSpec`](#service-namespaces) | No | `nil` (placed in the ControlPlane's namespace) | Places the dashboard — and the cache and secret store that follow it — in a namespace of its own. Create-only. A dashboard placed apart reads its `SECRET_KEY` from **that** namespace: the default `horizon-secret-key` shim Secret is namespace-local, so supply the key material there (and name it via `secretKeyRef`). See [Service Namespaces](#service-namespaces). |

> **`publicEndpoint` and `gateway.hostname` must name the same host.** Django
> derives the origin it sends from the request's `Host` header — i.e. from
> `gateway.hostname`, not from this field. A `publicEndpoint` whose host differs
> produces an origin Keystone will reject, so whenever a `gateway` is configured
> the validating webhook rejects the ControlPlane instead. The **port** may still
> differ, since Gateway API hostnames carry none. Behind a gateway the scheme
> must be `https`: the listener terminates TLS, and Keystone POSTs the unscoped
> WebSSO token to this origin. See the
> [End-to-End SSO guide](../../guides/end-to-end-sso.md).

---

## DedicatedBackingServices

By default every service a ControlPlane manages connects to the **shared**
instances declared in [`spec.infrastructure`](#infrastructurespec): one database
cluster, one cache. Isolation between services is logical only — each service
gets its own logical database and its own credentials on the shared MariaDB, and
shares the Memcached instance.

`services.<svc>.dedicatedBackingServices` is the **opt-in** that gives a single
service backing services of its own instead. It is declared per service and per
backing-service **class**:

```yaml
spec:
  services:
    keystone:
      dedicatedBackingServices:
        database:                       # Keystone gets its own database cluster
          clusterRef:
            name: prod-keystone-db
          credentialsMode: Static
          database: keystone
          secretRef:
            name: keystone-db
          replicas: 3
          storageSize: 200Gi
        cache:                          # …and its own cache
          clusterRef:
            name: prod-keystone-cache
          backend: dogpile.cache.pymemcache
          replicas: 3
    horizon:
      dedicatedBackingServices:
        cache:                          # the dashboard gets a cache of its own
          clusterRef:
            name: prod-horizon-cache
          backend: dogpile.cache.pymemcache
          replicas: 1
```

**Omitting the block is the default and keeps today's behavior**: the service
shares the ControlPlane-wide instances. A class left unset inside a declared
block is shared too — the Keystone service above could take a dedicated database
and keep sharing the cache.

### Fields

`KeystoneDedicatedBackingServicesSpec`:

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `database` | [`*commonv1.DatabaseSpec`](../keystone/keystone-crd.md#databasespec) | No | `nil` (shares `spec.infrastructure.database`) | Gives Keystone its own database cluster. In managed mode `clusterRef.name` defaults to `{controlplane}-keystone-db`. |
| `cache` | [`*commonv1.CacheSpec`](../keystone/keystone-crd.md#cachespec) | No | `nil` (shares `spec.infrastructure.cache`) | Gives Keystone its own cache. In managed mode `clusterRef.name` defaults to `{controlplane}-keystone-cache`. |

`HorizonDedicatedBackingServicesSpec`:

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `cache` | [`*commonv1.CacheSpec`](../keystone/keystone-crd.md#cachespec) | No | `nil` (shares `spec.infrastructure.cache`) | Gives the dashboard its own cache. In managed mode `clusterRef.name` defaults to `{controlplane}-horizon-cache`. |

Declaring the block with **no class set** is rejected — it would request nothing.
Omit it entirely to share.

### Lifecycle

A dedicated instance is not a second-class one. It reuses the same `commonv1`
shapes as the shared block, so it carries the same **managed-versus-brownfield**
split — managed mode (`clusterRef`) has the ControlPlane provision the instance,
brownfield mode (`database.host` / `cache.servers`) references an externally
operated endpoint and provisions nothing — and the reconciler puts it through the
same path as a shared instance:

| Guarantee | How it holds for a dedicated instance |
| --- | --- |
| Provisioning | `reconcileInfrastructure` ensures a `MariaDB` / `Memcached` child CR per managed instance a service **resolves to**, shared and dedicated alike, sized from **that instance's** `replicas` / `storageSize`. Opting out is a genuine opt-out: a shared instance every service has left has no consumer, so it is not provisioned. Keystone is the only database consumer, so giving it a dedicated database means the shared cluster is never created — it would otherwise be an orphan (3 Galera replicas, 100Gi by default) that nothing talks to and readiness still waits for. |
| Ownership and teardown | The child carries a controller owner reference to the ControlPlane with `blockOwnerDeletion`, so it is garbage-collected with the ControlPlane. A pre-existing CR under the same name is **adopted read-only** and never GC-claimed. |
| Readiness gating | `InfrastructureReady` is `True` only once **every** managed instance is Ready. A service whose dedicated database is still converging holds the condition `False`, so its projection is deferred — it waits for the database it actually talks to, not just for the shared cluster. |
| Credentials | The service child's `spec.database` is projected from the dedicated spec, so credential provisioning and rotation follow the instance the service connects to (see [Credential modes](#credential-modes) below). |
| Network policy | The service operators derive their database/cache egress rules from the projected `spec.database` / `spec.cache`, so they follow the dedicated instance automatically. |

### Credential modes

A dedicated **managed** database uses `credentialsMode: Static`. The defaulting
webhook materializes it, and an explicit `Dynamic` is **rejected at admission**:
the OpenBao database engine carries exactly one connection and one role **per
namespace** (`deploy/openbao/bootstrap/setup-database-tenant.sh`), bootstrapped
against the *shared* cluster, so no engine role exists that could issue
credentials for a dedicated instance — an admitted `Dynamic` dedicated database
would wedge on an ExternalSecret that can never sync.

`Static` is the same contract the shared block's own Static opt-out carries: the
operator projects a KV-backed ExternalSecret reading
`openstack/keystone/{namespace}/{name}/db`, and the credential is **seeded and
rotated at the OpenBao source** (ESO refreshes it within the hour). A brownfield
dedicated database keeps the user-supplied `secretRef` Secret, exactly as a
brownfield shared one does.

> **Seed the KV path before you expect `Ready`.** It is seeded by **neither the
> operator nor the bootstrap** — the per-ControlPlane static seed was retired when
> managed mode moved to engine-issued credentials. A dedicated managed database
> therefore reaches `Ready` only once you have seeded
> `kv-v2/openstack/keystone/{namespace}/{name}/db` (`username`, `password`)
> yourself; see
> [Migrate the Keystone DB to dynamic credentials](../../guides/migrate-keystone-db-to-dynamic-credentials.md)
> for the exact `bao kv put`. Until then `DBCredentialsReady` stays `False` with
> reason `WaitingForDBCredentialSecret` and a message naming the path.

### Name collisions

Managed `clusterRef` names must be **unique per backing-service class** across
the shared block and every dedicated instance. Two instances sharing a name would
resolve to a single child CR that both projections then fight over — silently
voiding the isolation the opt-in exists for — so the validating webhook rejects
the duplicate. The derived defaults (`{controlplane}-keystone-db`,
`{controlplane}-keystone-cache`, `{controlplane}-horizon-cache`) never collide
with each other or with the shared defaults (`openstack-db`,
`openstack-memcached`).

### Immutability

Both the per-service block and each class within it are **frozen on a live
ControlPlane**: a service cannot be moved between shared and dedicated backing
services in either direction, because the flip would re-point the consuming
child's (immutable) database fields at a different instance while the
previously-provisioned one keeps running with the data still on it. The
create-only leaves of a declared instance (`clusterRef.name`, `database`,
`replicas`, `storageSize`, and the managed-vs-brownfield mode) are frozen the same
way the shared block's are; a cache's `replicas` stays mutable.

The freeze is **webhook-only** — deliberately carrying no CEL transition rule —
so a later transition feature (with or without data migration) can relax it to a
gated migration. An immutable CEL marker never could.

### Adding a backing-service class

Database and cache exist today. A new class (Valkey, RabbitMQ) is added as **one
more optional pointer field** on the per-service block, reusing its own canonical
`commonv1` shape. The shared-by-default / dedicated-on-request contract and the
per-service opt-in surface are unchanged by that addition — which is why the
classes are individual fields rather than one opaque block. A service's block
only ever surfaces the classes that service actually consumes, which is why
Horizon has a `cache` and no `database`.

---

## ExternalKeystoneSpec

Declares how the control plane reaches a pre-existing, externally-operated
Keystone in **External** mode. Present only under `services.keystone.external`
and required when `services.keystone.mode` is `External`. It mirrors the
brownfield infrastructure shape at the identity level: the endpoint and,
optionally, a private-CA bundle are supplied here, and the reconciler manages
identity against that endpoint rather than deploying a Keystone workload.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `authURL` | `string` | Yes | — | Identity endpoint of the external Keystone (e.g. `https://keystone.example.com/v3`). Must match the HTTP(S) URL pattern `^https?://[^\s/]+` — an HTTP(S) shape with a **non-empty host**, so a hostless endpoint is rejected at admission — and be at most **2048** characters. Both are enforced by the CRD `+kubebuilder:validation:Pattern` / `MaxLength` markers and mirrored by the validating webhook with a full `net/url` parse. The cap bounds the one unbounded input the reconciler interpolates into `status.conditions[].message`: the pattern is end-unanchored, so without it a multi-kilobyte path could push the assembled message past the apiserver's 32768-byte cap and fail the **whole** `status.conditions` write. Neither gate is an SSRF control — admission cannot resolve where the host points, so egress restrictions remain the operator's responsibility. |
| `endpointType` | `string` (`public` \| `internal` \| `admin`) | No | `public` | Which Keystone catalog interface to authenticate against. Defaulted to `public` by both the `+kubebuilder:default` marker and the defaulting webhook. Rendered as the clouds.yaml `endpoint_type` key of **both** generated credentials Secrets. Named `endpointType` (not `interface`) because K-ORC drops gophercloud's `Interface` field and only honours `endpoint_type` (the authoritative note lives on `buildAppCredCloudsYAML` in the reconciler's `korc_cloudsyaml.go`). The selected interface must exist in the external catalog for `spec.region` — otherwise the control plane fails loud with `KORCReady=False/CatalogEndpointMismatch`. |
| `caBundleSecretRef` | [`*commonv1.SecretRefSpec`](../keystone/keystone-crd.md#secretrefspec) | No | `nil` | References a Secret carrying a private CA bundle the client trusts when verifying the external endpoint. The bundle is projected verbatim as the inline `cacert` key into **both** generated K-ORC credentials Secrets — K-ORC reads that key natively from the same Secret that carries `clouds.yaml`, so no mount and no upstream change are needed. `key` defaults to `ca.crt`; this default is **webhook-only** because the shared `SecretRefSpec` carries no c5c3-specific marker (the same discipline as `passwordSecretRef.key`). When the ref is set its `name` must be non-empty (CRD `MinLength` marker + webhook). A missing Secret, a missing key, or a present-but-empty key defers the mint with `KORCReady=False/WaitingForCABundle` — the last shape is the normal transient of a two-step "create the Secret, then populate it" flow. |
| `catalog` | [`*ExternalCatalogSpec`](#externalcatalogspec) | No | `nil` | Tunes how the control plane stewards the external Keystone's service catalog. Omitting it selects the conservative default: the existing identity service and all three of its endpoint interfaces are **imported** as unmanaged K-ORC CRs, and **zero** catalog entries are created. |

> **`spec.region` must match the external catalog.** `region_name` in both
> generated `clouds.yaml` documents comes from `spec.region` (defaulted
> `RegionOne`). Against an external catalog that publishes a different region,
> gophercloud fails **loud** with *"No suitable endpoint could be found in the
> service catalog"*, which the reconciler classifies onto
> `KORCReady=False/CatalogEndpointMismatch` and annotates with the effective
> `spec.region` and `endpointType`. There is no silent fallback: the operator
> cannot repair an external catalog.

> **Rotating the CA bundle is not instantaneous.** Changing (or removing)
> `caBundleSecretRef` converges both credentials Secrets on the next reconcile —
> the CA Secret is watched, so the ControlPlane wakes immediately. K-ORC's
> provider-client cache, however, keys on the parsed cloud struct only; `cacert`
> is **not** part of the cache key. The new trust store therefore takes effect
> only once the cached client expires (TTL = token lifetime / 2, ≈30 min at
> Keystone defaults). Nothing in this operator can shorten that window.

> **TLS and egress prerequisites.** An IP-based `authURL` needs an IP SAN in the
> external Keystone's server certificate; hostnames resolve through the cluster
> DNS upstream forwarder. Nothing restricts `orc-system` egress today, but a
> cluster with restrictive egress NetworkPolicies must explicitly allow K-ORC to
> reach the external endpoint and port — see the
> [reconciler reference](./controlplane-reconciler.md#external-keystone-mode-and-the-chain).

---

## ExternalCatalogSpec

Tunes External-mode catalog stewardship. Present only under
`services.keystone.external.catalog`, and entirely optional: its zero value is
the conservative default.

In **External** mode the service catalog belongs to the pre-existing
installation, so the control plane is **import-first**. It never registers the
identity service, because Keystone enforces no uniqueness on service names and a
managed registration against a populated catalog would silently duplicate rows.
Instead it imports the existing identity service and each of its endpoint
interfaces as unmanaged K-ORC CRs, which resolve read-only and write nothing.
Creating catalog entries survives only as the explicit `managedEntries` opt-in
below.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `identityServiceName` | `string` | No | `""` | Disambiguates the identity `Service` import when the external catalog carries more than one `identity`-type service. When empty the import filters on type alone. Minimum length 1, maximum 255, and — like [`managedEntries[].name`](#externalcatalogentryspec) — no comma, mirroring K-ORC's `OpenStackName` pattern `^[^,]+$` which the value is cast to on the import filter. |
| `managedEntries` | [`[]ExternalCatalogEntrySpec`](#externalcatalogentryspec) | No | `nil` | The explicit opt-in for creating genuinely new catalog entries. Absent by default, so External mode creates **zero** catalog entries. A `listType=map` list keyed on `type`, so the API server rejects duplicate entry types. At most 32 entries (`maxItems`), because every entry amplifies into managed K-ORC CRs and therefore into writes against the external Keystone. |

> **All three interfaces are imported; only one is required.** The `public`,
> `internal` **and** `admin` endpoints of the identity service are imported, not
> only the one `endpointType` selects. Catalog rows are listable through the
> identity API whether or not the endpoint they advertise is reachable from this
> cluster, so full visibility costs nothing. Entries for unreachable interfaces
> are informational.
>
> Only the interface `endpointType` selects **gates** `CatalogReady`. The control
> plane already authenticates through that interface, so a catalog that does not
> publish it is not the catalog K-ORC was pointed at, and its import stalling is
> the silent-empty hazard the detector exists to surface. An external
> installation is free not to publish the other two — kolla-ansible stopped
> registering the identity `admin` endpoint after Zed, and a devstack
> bootstrapped with only a public URL publishes neither of the others. Those
> imports simply stay `resolved: false` in
> [`status.catalog.imports`](#catalogimportstatus). Gating readiness on an
> interface the installation never published would hold the aggregate `Ready` at
> `False` forever for the two most common brownfield deployment tools.

> **Disambiguation is by name only.** There is no import-by-id. K-ORC's
> `ServiceImport.id` carries a `Format:=uuid` marker (the RFC 4122 dashed form)
> while Keystone mints service ids as dashless `uuid4().hex`, so an id-based
> import is rejected by K-ORC's own CRD schema and cannot be offered. A catalog
> holding two identically **named** identity services therefore cannot be
> disambiguated from the spec at all: the control plane fails loud with
> `CatalogReady=False/CatalogFailed` and the external catalog must be repaired.

> **Multi-region catalogs.** K-ORC's `EndpointFilter` carries no region field, so
> an identity service publishing one `public` endpoint per region makes the
> endpoint import match several rows. K-ORC reports that as a terminal error and
> the control plane relays it — loud, never silent — but no spec field can select
> among them today.

---

## ExternalCatalogEntrySpec

One genuinely new catalog entry the control plane creates and owns in the
external Keystone. Projected as one managed K-ORC `Service` named
`{controlplane.Name}-catalog-{type}` plus one managed `Endpoint` per declared
interface. Removing an entry from `managedEntries` deletes exactly those
resources and nothing else.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `type` | `string` | Yes | — | The OpenStack service type (e.g. `image`, `compute`). Keys the `listType=map` list. Must be a lowercase DNS-1123 label (`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, maximum 63) because it is embedded verbatim in the child CR names. `identity` is **forbidden** (CEL rule + webhook): that entry is owned by the imports. The webhook additionally rejects a type whose composed child CR name — `{controlplane.Name}-catalog-{type}-{interface}` — would exceed the apiserver's 253-byte `metadata.name` limit, which no CRD marker can express. |
| `name` | `string` | No | `""` | Overrides the catalog service name. When empty K-ORC names the service after the child CR. Minimum length 1, maximum 255, and no comma — the pattern mirrors K-ORC's own `OpenStackName` (`^[^,]+$`), which the name is cast to on the child `Service` CR, so a name admitted here can never be rejected downstream. The validating webhook mirrors the pattern. |
| `endpoints` | [`[]ExternalCatalogEndpointSpec`](#externalcatalogendpointspec) | No | `nil` | The endpoint rows registered for this entry, at most one per interface (`listType=map` keyed on `interface`). An entry with no endpoints registers the service row alone. |

---

## ExternalCatalogEndpointSpec

One endpoint row of a managed catalog entry.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `interface` | `string` (`public` \| `internal` \| `admin`) | Yes | — | The catalog interface this endpoint is published under. Keys the `listType=map` list. |
| `url` | `string` | Yes | — | The endpoint URL registered in the catalog. Must match `^https?://[^\s/]+` and be at most 1024 bytes — the cap mirrors K-ORC's own `EndpointResourceSpec.url`, so a URL admitted here can never be rejected downstream. The validating webhook mirrors both the shape and the cap with a full `net/url` parse. |

---

## GatewaySpec

The shared `commonv1.GatewaySpec` (`internal/common/types`), the **single source
of truth** for the Gateway API HTTPRoute knobs reused by both the ControlPlane
and the Keystone CRD — see the [Keystone CRD →
GatewaySpec](../keystone/keystone-crd.md#gatewayspec) for the same shape on the
projected child. The reconciler (L2) maps it onto the projected Keystone CR's
`spec.gateway`. As with the other `commonv1` shapes, reusing this type still
keeps the L1 API package free of a dependency on the keystone module (the
formerly hand-curated local copy was consolidated into `commonv1`; the L1
package imports only `commonv1`, never the keystone module).

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `parentRef` | [`GatewayParentRefSpec`](#gatewayparentrefspec) | Yes | — | The pre-existing Gateway the HTTPRoute attaches to. The Gateway (and GatewayClass) are platform-team infrastructure managed outside this CR. |
| `hostname` | `string` | Yes | — | Externally reachable host (SNI / Host header) the HTTPRoute matches, e.g. `keystone.example.com`. Minimum length 1. |
| `path` | `string` | No | `"/"` (Keystone operator default) | URL path prefix matched by the HTTPRoute. |
| `annotations` | `map[string]string` | No | `nil` | Passed through to the generated HTTPRoute metadata verbatim (rate limits, timeouts, CORS) without extending the CRD. |

---

## GatewayParentRefSpec

References a pre-existing Gateway that the projected Keystone's HTTPRoute
attaches to. The shared `commonv1.GatewayParentRefSpec` (`internal/common/types`),
nested under [`commonv1.GatewaySpec`](#gatewayspec) and reused by both the
ControlPlane and the Keystone CRD.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` | Yes | — | Gateway resource name. Minimum length 1. |
| `namespace` | `string` | No | `""` | Namespace of the referenced Gateway. When empty, the projected Keystone CR's namespace is assumed. |
| `sectionName` | `string` | No | `""` | Targets a specific listener on the Gateway (e.g. `https`) when it defines multiple listeners. When empty, the HTTPRoute attaches to all compatible listeners. |

---

## KORCSpec

Configures the K-ORC (OpenStack Resource Controller) integration of the control
plane. It declares how the admin application credential is
bootstrapped and rotated and which bootstrap resources are reconciled.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `adminCredential` | [`AdminCredentialSpec`](#admincredentialspec) | Yes | — | The admin OpenStack credential K-ORC uses to reconcile resources, plus the application-credential rotation policy. |
| `serviceAccounts` | [`[]ServiceAccountSpec`](#serviceaccountspec) | No | `nil` | Composite OpenStack service accounts (nova, glance, …) the control plane manages: one entry = one K-ORC `User` + `Project` with an operator-generated, OpenBao-backed, rotatable password. Mode-independent (managed and external Keystone). `listType=map` keyed by `name`; max 32 entries. |

---

## AdminCredentialSpec

Declares the admin OpenStack credential and the application-credential rotation
policy for the control plane.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `cloudCredentialsRef` | [`CloudCredentialsRef`](#cloudcredentialsref) | Yes | — | References the `clouds.yaml` Secret and cloud entry K-ORC authenticates as. |
| `passwordSecretRef` | [`commonv1.SecretRefSpec`](../keystone/keystone-crd.md#secretrefspec) | No | name `"keystone-admin"`, key `"password"` | References the Secret holding the admin password used to (re-)mint the application credential. The defaulting webhook materializes a missing `name` to `keystone-admin` and a missing `key` to `password`, so the block may be omitted on a minimal CR. The validating webhook still enforces `passwordSecretRef.name` non-empty as defense-in-depth (see [Validation Rules](#validation-rules)), but the defaulting webhook always satisfies it before validation runs, so a user may leave it unset. The reconciler's existing `"password"` key fallback also remains. **Mode-dependent use:** the `keystone-admin` default is the **brownfield / spec-level default**. In **brownfield mode** (`database.clusterRef == nil`) this field is used verbatim — the user supplies the admin-password Secret out-of-band and the operator projects no ExternalSecret, so this reference is projected onto the Keystone CR's `bootstrap.adminPasswordSecretRef` so Keystone and K-ORC agree on the admin password source. In **managed mode** (`database.clusterRef` set) the operator instead projects a per-ControlPlane admin ExternalSecret named `{controlplane.Name}-keystone-admin-credentials` (materialising the admin password from OpenBao) and **overrides** the projected Keystone CR's `bootstrap.adminPasswordSecretRef` to point at that operator-owned per-CP Secret's `password` key — the cp-level `passwordSecretRef` is **not** used as the child's ref in managed mode. See the [managed-mode admin-password provisioning](#admincredentialspec) note below. |
| `userName` | `string` | No | `"admin"` | OpenStack admin user name the control plane authenticates as. Defaulted to `admin` by both the `+kubebuilder:default` marker and the defaulting webhook. Valid in **both** keystone modes. Rendered as the `clouds.yaml` `username` **and** used as the K-ORC admin `User` import filter the application credential's `UserRef` resolves to — see the same-user constraint below. |
| `projectName` | `string` | No | `"admin"` | OpenStack admin project name, rendered as the `clouds.yaml` `project_name`. Defaulted to `admin` by both the marker and the defaulting webhook. Valid in both modes. |
| `domainName` | `string` | No | `"Default"` | OpenStack admin domain name. Defaulted to `Default` by both the marker and the defaulting webhook. Valid in both modes. It is the K-ORC admin `Domain` import filter. **Phase-1 nuance:** the single `domainName` feeds **both** `user_domain_name` and `project_domain_name` in the generated `clouds.yaml`, so the admin user and project must live in the **same** domain; a later `userDomainName`/`projectDomainName` split is a compatible extension. |

> **Same-user constraint (hard, enforced by Keystone).** Keystone's default
> policy allows creating an application credential only for the **token's own
> user** — even an admin token is refused (HTTP 403,
> `identity:create_application_credential`) when it targets another user. The
> `clouds.yaml` `username` and the imported admin `User` the credential's
> `UserRef` points at must therefore name the same OpenStack user. Both derive
> from this single `userName` field, and a unit test
> (`TestPasswordCloudsYAMLIdentityMatchesUserImportFilter`) pins the agreement.

> **Identity edits are not re-resolved.** Changing `userName`, `projectName` or
> `domainName` on a live ControlPlane updates the K-ORC import filters in place,
> but K-ORC imports resolve **once**: the already-resolved OpenStack id is not
> looked up again. The mismatch surfaces as `KORCReady=False/CredentialDrift`
> rather than silently repointing the credential. The Kubernetes CR names of the
> imports (`{controlplane.Name}-user-admin`, `{controlplane.Name}-domain-default`)
> are stable handles and deliberately do **not** track the identity.
| `applicationCredential` | [`ApplicationCredentialSpec`](#applicationcredentialspec) | Yes | — | Policy for the K-ORC admin application credential (restriction, access rules, rotation mode). |
| `bootstrapResources` | [`[]BootstrapResourceSpec`](#bootstrapresourcespec) | No | `nil` | OpenStack resources K-ORC bootstraps alongside the admin credential (e.g. the projects/roles a fresh control plane needs). The element shape is intentionally minimal at L1; the reconciler interprets it. |

> **`passwordSecretRef` is operator-owned in managed mode.** The
> `keystone-admin` default is the **brownfield / spec-level default** only. In
> **managed mode** (`database.clusterRef` set) the operator projects a
> per-ControlPlane admin `ExternalSecret` named
> `{controlplane.Name}-keystone-admin-credentials` in the ControlPlane namespace
> that materialises the admin password from OpenBao path
> `bootstrap/{namespace}/{controlplane.Name}-keystone/admin` (canonical:
> `bootstrap/openstack/controlplane-keystone/admin`, property `password`), and
> **overrides** the projected Keystone CR's `bootstrap.adminPasswordSecretRef` to
> point at that operator-owned per-CP Secret's `password` key. The cp-level
> `passwordSecretRef` is therefore **not** used as the child's ref in managed
> mode — the source `cp.Spec` is left untouched; only the projected child's ref
> is reassigned.
>
> In **brownfield mode** (`database.clusterRef == nil`, a Host-based DB) the
> operator projects **no** admin ExternalSecret: the user supplies the
> admin-password Secret out-of-band and the cp-level `passwordSecretRef` (default
> `keystone-admin`) is projected onto the Keystone CR's
> `bootstrap.adminPasswordSecretRef` verbatim. See the
> [ControlPlane Reconciler reference](./controlplane-reconciler.md) for the
> admin-credential flow.

---

## CloudCredentialsRef

References the `clouds.yaml` Secret and the cloud entry within it that K-ORC
authenticates as.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `cloudName` | `string` | No | `"admin"` | The entry in `clouds.yaml` K-ORC authenticates as. Also used by the reconciler as the conventional K-ORC `User` reference name and projected onto the catalog `Service`/`Endpoint` CRs. Defaulted to `admin` by **both** the `+kubebuilder:default` marker (normal admission) and the defaulting webhook (callers that bypass the CRD default). |
| `secretName` | `string` | No | `"k-orc-clouds-yaml"` | Name of the Secret holding the `clouds.yaml` document. Defaulted to `k-orc-clouds-yaml` by **both** the `+kubebuilder:default` marker and the defaulting webhook. The Secret is namespace-local to the ControlPlane's child namespace; because the operator enforces one ControlPlane per namespace, the shared default name does not collide across control planes. The operator (`reconcileKORC` → `ensureKORCCloudsYAMLExternalSecret`) **creates and owns** a per-ControlPlane ExternalSecret of this name in the child namespace that materialises the Secret, reading the per-CR OpenBao path — so the shared default name is safe and needs no per-CR manifest. |

---

## ApplicationCredentialSpec

Declares the K-ORC admin application-credential policy.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `restricted` | `*bool` | No | `true` | Controls whether the application credential is restricted (least-privilege, unable to create further application credentials). Defaulted to `true` by **both** the `+kubebuilder:default` marker and the defaulting webhook. The pointer distinguishes "unset" (→ default `true`) from an explicit `false`, which is preserved. See the [restricted → unrestricted inversion](#restricted--unrestricted-inversion) note. |
| `accessRules` | [`[]AccessRule`](#accessrule) | No | `nil` | Optionally narrows the application credential to a specific set of service/method/path rules. When empty, the credential is not constrained by access rules. |
| `rotation` | [`RotationSpec`](#rotationspec) | Yes | — | How the application credential is rotated. |

### restricted → unrestricted inversion

The ControlPlane spec exposes a **`restricted`** flag (the safe, least-privilege
posture). K-ORC's `ApplicationCredentialResourceSpec` exposes the inverse field,
**`unrestricted`**. The reconciler performs the inversion when projecting the
K-ORC `ApplicationCredential` CR:

```
restricted=true  → K-ORC spec.resource.unrestricted=false
restricted=false → K-ORC spec.resource.unrestricted=true
```

The same inversion is applied in reverse when reflecting the K-ORC-reported
state back into `status.adminApplicationCredential.restricted`.

---

## AccessRule

Narrows an application credential to a specific service endpoint and method,
mirroring the Keystone application-credential access-rule shape.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `service` | `string` | Yes | — | OpenStack service type the rule applies to (e.g. `"compute"`). The reconciler uses this verbatim as the name of the referenced K-ORC `Service` CR (`serviceRef`). |
| `method` | `string` | No | — | HTTP method the rule allows (e.g. `"GET"`, `"POST"`). Projected onto the K-ORC typed `HTTPMethod` enum; constrained by `+kubebuilder:validation:Enum=CONNECT;DELETE;GET;HEAD;OPTIONS;PATCH;POST;PUT;TRACE` (mirrors that enum). Optional — omitted from the projected rule when empty. |
| `path` | `string` | No | — | Request path the rule allows (e.g. `"/v2.1/servers"`). When set it must be an absolute path (`+kubebuilder:validation:Pattern=^/`). Optional — omitted from the projected rule when empty. |

---

## RotationSpec

Declares the rotation policy for the admin application credential.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `mode` | [`RotationMode`](#rotationmode) | No | `PasswordDriven` | Selects the rotation strategy. Defaulted to `PasswordDriven` by **both** the `+kubebuilder:default` marker and the defaulting webhook. |

### RotationMode

`RotationMode` is a string enum
(`+kubebuilder:validation:Enum=PasswordDriven;Scheduled;Manual`).

| Value | Status | Meaning |
| --- | --- | --- |
| `PasswordDriven` | Active (default) | Re-mints the application credential whenever the underlying admin password changes. The reconciler compares the SHA-256 of the admin password against an annotation stamped on the K-ORC `ApplicationCredential` CR; a mismatch drives a re-mint. |
| `Scheduled` | **Reserved** | Rotates the application credential on a schedule. Surfaced in the enum now so the CRD schema is stable, but the scheduled-rotation logic is deferred to a later level. |
| `Manual` | **Reserved** | Rotates only when a [`CredentialRotation`](#credentialrotationspec) CR requests it. The `CredentialRotation` flow is the mechanism; the `Manual` mode value itself is reserved at this level. |

---

## BootstrapResourceSpec

Declares an OpenStack resource K-ORC bootstraps with the control plane.
The shape is intentionally minimal at L1 — the reconciler interprets
the kind/name and applies it.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `kind` | `string` | Yes | — | The K-ORC resource kind to bootstrap. Constrained to the kinds the control plane bootstraps today by `+kubebuilder:validation:Enum=Project;Role`; widen the enum when the reconciler learns to interpret additional kinds. |
| `name` | `string` | Yes | — | Name of the bootstrapped resource. |

> **RESERVED.** No controller reads `bootstrapResources` today. For service
> users of other OpenStack services, declare a composite
> [`serviceAccounts`](#serviceaccountspec) entry instead — it owns the full
> user + project + password lifecycle.

---

## ServiceAccountSpec

Declares one composite OpenStack service account: a managed K-ORC `User` with an
operator-generated, OpenBao-backed, rotatable password, its project (referenced
or created), and the roles bound to it. Projected by
[`reconcileServiceAccounts`](./controlplane-reconciler.md#reconcileserviceaccounts).

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` | Yes | — | Keys the `listType=map` list and is embedded in every child CR and Secret name (DNS-1123 label, `[a-z0-9]([-a-z0-9]*[a-z0-9])?`, ≤ 63). Not the OpenStack user name — that is `userName`. |
| `userName` | `string` | No | `name` | The OpenStack user name managed in Keystone. Defaults to `name` via the defaulting webhook. Pattern `^[^,]+$`, ≤ 255 (mirrors K-ORC's `OpenStackName`). |
| `domainName` | `string` | No | admin domain | The OpenStack domain the user and project live in. Empty resolves to `spec.korc.adminCredential.domainName`. |
| `adopt` | `bool` | No | `false` | Explicit consent that a pre-existing Keystone user of this name may be taken over. Fail-loudly by default: a declared user that already exists surfaces `ServiceAccountsReady=False/ServiceAccountCollision` and is never touched. `adopt: true` opts into a **password takeover** AND into operator ownership — an adopted user is a managed `User`, so it is **deleted from Keystone at teardown**, exactly like one the operator created. |
| `project` | [`ServiceAccountProjectSpec`](#serviceaccountprojectspec) | Yes | — | The project the service user is associated with, referenced (default) or created. |
| `roles` | `[]string` | No | `nil` | OpenStack role names assigned to the user on the project. Each role projects one **unmanaged** K-ORC `Role` import (referenced by name, never created or deleted — Keystone roles are global) plus one **managed** `RoleAssignment` binding it to the user on the project (one per user × project × role). Their readiness folds into the per-account `ServiceAccountsReady` gate; removing a role prunes both child CRs; at teardown the managed assignment is deleted from Keystone while the `Role` import is released untouched. Item pattern `^[^,]+$`, ≤ 255, max 32. |
| `rotation` | [`*ServiceAccountRotationSpec`](#serviceaccountrotationspec) | No | mode `Manual` | Per-account password-rotation policy. |

### ServiceAccountProjectSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` | Yes | — | The OpenStack project name. Pattern `^[^,]+$`, ≤ 255 (mirrors K-ORC's `KeystoneName`). |
| `create` | `bool` | No | `false` | `false` **references** a pre-existing project via an unmanaged import (the operator never creates or deletes it); `true` **creates and owns** a managed `Project`, gated by the same fail-loudly collision probe as the user. |

### ServiceAccountRotationSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `mode` | `ServiceAccountRotationMode` | No | `Manual` | `Manual` rotates only on a [`CredentialRotation`](#credentialrotationspec) request. `Scheduled` is **reserved** (surfaced in the enum now, deferred non-silently via a `ScheduledRotationDeferred` event). Deliberately not the admin `RotationMode`: there is no external password source, so `PasswordDriven` does not apply. |

**Consumption contract.** Each account's credentials are materialized into a
Secret named `{controlplane.Name}-service-account-{name}-credentials` (keys
`password` and a ready-to-use `clouds.yaml`), mirrored from the per-CR OpenBao
path `openstack/keystone/{namespace}/{controlplane.Name}/service-accounts/{name}`.
`status.serviceAccounts[].secretName` names it. See the
[reconciler reference](./controlplane-reconciler.md#reconcileserviceaccounts).

---

## ControlPlaneStatus

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | Latest available observations of the control-plane state. Each condition carries an `observedGeneration`. See [Status Conditions](#status-conditions). |
| `observedGeneration` | `int64` | The `.metadata.generation` the controller last reconciled, so a stale status is distinguishable from a current one. |
| `updatePhase` | [`UpdatePhase`](#updatephase) | Current phase of a control-plane release update. Written on every status update; fixed at `Idle` in the current implementation because the release-update state machine is reserved (the other `UpdatePhase` values are not yet set). |
| `services` | `[]ServiceStatus` | Per-service readiness of the projected service CRs. A `listType=map` list keyed by `name`, so per-service entries merge under server-side apply and can grow per-service conditions cleanly. Written on every status update with a `keystone` entry whose `ready` mirrors the `KeystoneReady` condition and whose `release` is `spec.openStackRelease` — omitted entirely when `spec.services.keystone` is unset (no Keystone is managed). See [ServiceStatus](#servicestatus). |
| `catalog` | [`*CatalogStatus`](#catalogstatus) | Observed state of the External-mode catalog imports. Nil in Managed mode, where the control plane creates the catalog entries rather than importing them. See [CatalogStatus](#catalogstatus). |
| `serviceAccounts` | `[]ServiceAccountStatus` | Observed state of the declared service accounts, keyed by `name` (`listType=map`). The discoverability half of the consumption contract: `secretName` names the materialized Secret each account's password is read from. See [ServiceAccountStatus](#serviceaccountstatus). |

> **`updatePhase` vs the Keystone CRD's `upgradePhase`.** These field names are
> intentionally distinct: `ControlPlane.status.updatePhase` is the control-plane
> release-update machine (`Idle` / `Updating` / …/ `RollingBack`), while
> `Keystone.status.upgradePhase` is the live per-service database expand-migrate-
> contract machine (`Expanding` / `Migrating` / `RollingUpdate` / `Contracting`).
> They carry different enum vocabularies for different concerns and are not the
> same field under two names.
| `adminApplicationCredential` | [`*AdminApplicationCredentialStatus`](#adminapplicationcredentialstatus) | Observed state of the K-ORC admin application credential. |

### ServiceStatus

Reports the observed readiness of a single projected service CR.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | `string` | Yes | Service name (e.g. `keystone`); keys the `listType=map` `services` list. |
| `ready` | `bool` | Yes | Whether the projected service CR is Ready. |
| `release` | `string` | No | The OpenStack release the service currently reports installed. |

### AdminApplicationCredentialStatus

Reports the observed state of the K-ORC admin application credential.

> **Multi-instance — per-ControlPlane OpenBao path.** The minted admin
> application credential is mirrored to OpenBao at the **per-ControlPlane** path
> `openstack/keystone/{namespace}/{name}/admin/app-credential` (for the default
> deployment identity `openstack/controlplane`, this is
> `openstack/keystone/openstack/controlplane/admin/app-credential`), replacing the
> earlier flat path shared across control planes. Because the validating webhook
> permits exactly one ControlPlane per namespace, these per-CR paths are disjoint
> across namespaces by construction. See the
> [ControlPlane Reconciler reference](./controlplane-reconciler.md) for the full
> OpenBao layout and the migration from the legacy flat path.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `id` | `string` | No | The OpenStack application-credential ID currently in use, populated by K-ORC once the credential is minted. |
| `restricted` | `bool` | No | Whether the active credential is restricted. Computed as the inverse of the K-ORC-reported `unrestricted` (falling back to the desired value while K-ORC status is empty). |
| `lastRotation` | `*metav1.Time` | No | Timestamp of the last successful rotation. (Re-)stamped to "now" whenever the recorded credential `id` changes (initial mint or re-mint); preserved once the `id` is stable. |

### CatalogStatus

Reports how the External-mode identity catalog imports resolved. Nil in Managed
mode. It is the operator-visible answer to *"did the ControlPlane find the
catalog it was pointed at?"* — the aggregate [`CatalogReady`](#catalogready)
condition says whether they all resolved, this list says which ones did.

The list is rebuilt on every pass **before** any failure return, so an unresolved
import is reported as `resolved: false` rather than omitted.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `imports` | [`[]CatalogImportStatus`](#catalogimportstatus) | No | The unmanaged K-ORC CRs importing the external identity service and its endpoint interfaces. A `listType=map` list keyed by `name`. |

### CatalogImportStatus

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | `string` | Yes | The K-ORC CR name; keys the `listType=map` `imports` list. |
| `kind` | `string` (`Service` \| `Endpoint`) | Yes | The imported K-ORC kind. |
| `interface` | `string` (`public` \| `internal` \| `admin`) | No | The catalog interface of an imported `Endpoint`; empty for the `Service` import. |
| `resolved` | `bool` | Yes | Whether K-ORC matched this import against a live catalog entry (its `Available` condition is `True` for the CR's current generation). |
| `id` | `string` | No | The OpenStack id K-ORC resolved the import to. Empty while the import is unresolved. |

### ServiceAccountStatus

Reports the observed state of one declared service account.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | `string` | Yes | The service account name; keys the `listType=map` list. |
| `ready` | `bool` | Yes | Whether the user, project, and materialized password Secret are all converged for the current generation. |
| `userID` | `string` | No | The OpenStack user id K-ORC resolved (or created). |
| `projectID` | `string` | No | The OpenStack project id K-ORC resolved (or created). |
| `passwordGeneration` | `int64` | No | The monotonically increasing generation of the password currently applied. Increments on every rotation. |
| `lastPasswordRotation` | `*metav1.Time` | No | Timestamp of the last successful password rotation. |
| `secretName` | `string` | No | The materialized Secret carrying the account's credentials (`password` + `clouds.yaml`) — the documented, stable handle consumers read from. |

### UpdatePhase

`UpdatePhase` is a string enum
(`+kubebuilder:validation:Enum=Idle;Updating;UpdatingServices;Verifying;RollingBack`).

> **DECISION:** the enum surfaces the future phases alongside the
> active ones so the CRD schema is stable across levels and does not need a
> breaking change when the update state machine is implemented. The reserved
> values below are never set by the current reconciler; they are documented so
> consumers (dashboards, `kubectl`) see the full vocabulary.

| Value | Status | Meaning |
| --- | --- | --- |
| `Idle` | Active | No update is in progress. |
| `Updating` | Active | A release update has started. |
| `UpdatingServices` | **Reserved — not yet implemented** | Per-service CRs are being updated. |
| `Verifying` | **Reserved — not yet implemented** | The control plane is verifying an update. |
| `RollingBack` | **Reserved — not yet implemented** | A failed update is being rolled back. |

---

## CredentialRotationSpec

Defines the desired state of a `CredentialRotation` — a one-shot request to
rotate a control-plane credential. The reconciler re-mints the target
credential and reports progress via status conditions.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `target` | [`RotationTarget`](#rotationtarget) | Yes | — | Which credential to rotate. |
| `serviceAccount` | `string` | Conditional | — | Names the declared service account (`spec.korc.serviceAccounts[].name`) whose password is rotated. **Required** exactly when `target` is `serviceAccountPassword`, **forbidden** otherwise (two CEL rules; there is no CredentialRotation webhook, so CEL is the only gate). DNS-1123 label, ≤ 63. |
| `bootstrap` | `bool` | No | `false` | When `true`, requests an initial **mint** of the credential rather than a rotation of an existing one. Idempotent: if the credential already exists it is a no-op. |
| `reMint` | `bool` | No | `false` | When `true`, forces the reconciler to discard the current credential and mint a fresh one even if the existing credential is still valid. The nudge is **one-shot per spec generation** (latched on `status.lastTriggeredGeneration`), so a `reMint: true` left in the spec does not re-rotate on every resync. |
| `intervalDays` | `*int32` | No | `nil` | **Deferred** — accepted by the schema but ignored by the L1 reconciler. Rotation cadence in days for scheduled rotation. Minimum: 1. |
| `preRotationDays` | `*int32` | No | `nil` | **Deferred** — accepted but ignored. Days before expiry a replacement credential is minted (the overlap window). Minimum: 0. |
| `gracePeriodDays` | `*int32` | No | `nil` | **Deferred** — accepted but ignored. Days the superseded credential remains valid after a rotation before it is revoked. Minimum: 0. |

> **DECISION:** the scheduled-rotation fields (`intervalDays`,
> `preRotationDays`, `gracePeriodDays`) surface in the CRD schema now so the
> contract is stable, but the L1 reconciler **ignores** them — scheduled
> rotation (and the two-credential pre-rotation/grace overlap) is implemented in
> a later level. They are kept here, rather than introduced via a future
> breaking schema change, so dashboards and GitOps manifests can be written
> against the final shape.

### RotationTarget

`RotationTarget` is a string enum
(`+kubebuilder:validation:Enum=adminApplicationCredential;serviceAccountPassword`).

| Value | Meaning |
| --- | --- |
| `adminApplicationCredential` | Rotates the K-ORC admin application credential. |
| `serviceAccountPassword` | Rotates the password of the declared service account named by `spec.serviceAccount`. On demand only (no auto-detect: there is no external password source); fires on an explicit `reMint`, latched to the spec generation. |

## CredentialRotationStatus

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | Latest available observations of the rotation state. Upserted via the shared conditions helper. |
| `observedGeneration` | `int64` | The `.metadata.generation` the controller last reconciled. |
| `lastTriggeredGeneration` | `int64` | The most recent `.metadata.generation` for which an explicit `reMint` nudge was performed. Latches `reMint` to a single spec generation so a `reMint: true` left in the spec fires once per edit, not on every resync or restart. |

---

## SecretAggregate

`SecretAggregate` aggregates the Secrets produced by a control plane into a
single materialized Secret.

> **DECISION:** this is **types-only** at this level — there is **no
> controller**. The reconciler is **deferred to a later level**, and the operator RBAC
> for this kind is **read-only** (`get`/`list`/`watch`) until that reconciler
> lands, so the operator can observe `SecretAggregate` CRs without being granted
> write access to a kind it does not yet manage. The `Spec`/`Status` below are
> intentionally minimal placeholders; a later level will flesh them out.

### SecretAggregateSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `targetSecretName` | `string` | No | `""` | Name of the materialized aggregate Secret the (deferred) reconciler will produce. |

### SecretAggregateStatus

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | Latest available observations of the aggregate state. Upserted via the shared conditions helper. |

`SecretAggregate` has no printer columns and no defaulting/validating webhook at
this level.

---

## Shared Types (from `internal/common/types`)

The `ControlPlane` reuses the canonical `commonv1` shapes imported from
`github.com/c5c3/forge/internal/common/types`. These are shared across all
CobaltCore operator CRDs and are documented in full in the Keystone reference;
this section links rather than re-documents them to keep a single source of
truth.

| Type | Used by | Reference |
| --- | --- | --- |
| `ImageSpec` | `services.keystone.image` | [Keystone CRD → ImageSpec](../keystone/keystone-crd.md#imagespec) |
| `DatabaseSpec` | `infrastructure.database` | [Keystone CRD → DatabaseSpec](../keystone/keystone-crd.md#databasespec) |
| `CacheSpec` | `infrastructure.cache` | [Keystone CRD → CacheSpec](../keystone/keystone-crd.md#cachespec) |
| `SecretRefSpec` | `korc.adminCredential.passwordSecretRef` | [Keystone CRD → SecretRefSpec](../keystone/keystone-crd.md#secretrefspec) |
| `SecretStoreRefSpec` | `secretStoreRef` (projected onto the Keystone and Horizon children) | [Keystone CRD → SecretStoreRefSpec](../keystone/keystone-crd.md#secretstorerefspec) |
| `PolicySpec` | `globalPolicyOverrides`, `services.keystone.policyOverrides` | [Keystone CRD → PolicySpec](../keystone/keystone-crd.md#policyspec) |

> **Note on `DatabaseSpec.tls` / `CacheSpec`:** the `commonv1` shapes carry the
> full Keystone field set, including the optional `database.tls` block.
> Those fields are part of the reused struct and are validated by the API server,
> but the ControlPlane reconciler projects the `DatabaseSpec`/`CacheSpec` onto the
> Keystone CR verbatim — TLS behavior is therefore governed by the
> [Keystone DatabaseTLSSpec](../keystone/keystone-crd.md#databasetlsspec) on the
> projected child, not re-implemented in the aggregate.

---

## Validation Rules

The c5c3 ControlPlane uses a **two-layer** validation strategy, mirroring the
Keystone discipline:

1. **CRD schema markers** (`+kubebuilder:validation:*`) enforced by the API
   server before webhooks run — patterns, enums, and minimums.
2. **The validating webhook** (`validate()`), which re-checks the schema-level
   rules **and** adds the cross-field invariants that cannot be expressed as
   simple field markers.

> **CEL `x-kubernetes-validations` on this CRD.** The ControlPlane CRD carries
> CEL `XValidation` rules inherited from the shared `commonv1` types: the
> database clusterRef/host and cache clusterRef/servers mutual-exclusivity (from
> `DatabaseSpec`/`CacheSpec`, applied to `spec.infrastructure.database` and
> `spec.infrastructure.cache`), and the policy-rule name/value constraints (from
> `PolicySpec`, applied to `spec.globalPolicyOverrides` and `spec.services.keystone.policyOverrides`).
> The required `passwordSecretRef.name` remains enforced **only by the validating
> webhook**: a cluster that disables or bypasses the webhook (e.g. envtest without
> the webhook wired up, or a direct etcd write) will not reject a `ControlPlane`
> on that one rule, only on the CEL rules and the pattern/enum/minimum markers
> below. The markers and webhook together are defense-in-depth for the fields
> that **can** be expressed at both layers.

### CRD schema markers (API-server enforced)

| Field | Rule |
| --- | --- |
| `spec.openStackRelease` | Pattern `^\d{4}\.[12]$` |
| `spec.services.keystone.publicEndpoint` | Pattern `^https?://`; MaxLength 512 (the Horizon child's bound on `websso.keystoneURL`, which this value is projected onto) |
| `spec.services.horizon.publicEndpoint` | Pattern `^https?://`; MaxLength 499 (the Keystone child's 512-character bound on `trustedDashboards[]` minus `/auth/websso/`) |
| `spec.services.keystone.mode` | Enum: `Managed`, `External`; schema default `Managed` |
| `spec.services.keystone.external.authURL` | Pattern `^https?://[^\s/]+`; MaxLength 2048 (bounds the one unbounded input interpolated into `status.conditions[].message`) |
| `spec.services.keystone.external.endpointType` | Enum: `public`, `internal`, `admin`; schema default `public` |
| `spec.services.keystone.external.caBundleSecretRef.name` | MinLength 1 (shared `SecretRefSpec` marker) |
| `spec.korc.adminCredential.applicationCredential.accessRules[].method` | Enum: `CONNECT`, `DELETE`, `GET`, `HEAD`, `OPTIONS`, `PATCH`, `POST`, `PUT`, `TRACE` |
| `spec.korc.adminCredential.applicationCredential.accessRules[].path` | Pattern `^/` |
| `spec.korc.adminCredential.bootstrapResources[].kind` | Enum: `Project`, `Role` |
| `spec.korc.adminCredential.applicationCredential.rotation.mode` | Enum: `PasswordDriven`, `Scheduled`, `Manual` |
| `spec.korc.serviceAccounts[].name` | Pattern `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`; MinLength 1; MaxLength 63 |
| `spec.korc.serviceAccounts[].userName`, `.domainName`, `.project.name` | Pattern `^[^,]+$`; MinLength 1; MaxLength 255 |
| `spec.korc.serviceAccounts[].roles[]` | Pattern `^[^,]+$`; item MinLength 1; item MaxLength 255; MaxItems 32 |
| `spec.korc.serviceAccounts[].rotation.mode` | Enum: `Manual`, `Scheduled`; schema default `Manual` |
| `spec.korc.serviceAccounts` | listType=map keyed by `name`; MaxItems 32 |
| `spec.services.keystone.replicas` | Minimum: 1 |
| `spec.infrastructure.database.replicas` | Minimum: 1, schema default `3`. The webhook additionally rejects exactly `2` (Galera quorum — see below). |
| `spec.infrastructure.cache.replicas` | Minimum: 1, schema default `3` |
| `CredentialRotation spec.target` | Enum: `adminApplicationCredential`, `serviceAccountPassword` |
| `CredentialRotation spec.serviceAccount` | Pattern `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`; MaxLength 63 |
| `CredentialRotation` (CEL) | `target == 'serviceAccountPassword'` ⇒ `has(self.serviceAccount)` → "serviceAccount is required when target is serviceAccountPassword" |
| `CredentialRotation` (CEL) | `has(self.serviceAccount)` ⇒ `target == 'serviceAccountPassword'` → "serviceAccount may only be set when target is serviceAccountPassword" |
| `CredentialRotation spec.intervalDays` | Minimum: 1 |
| `CredentialRotation spec.preRotationDays` | Minimum: 0 |
| `CredentialRotation spec.gracePeriodDays` | Minimum: 0 |
| `status.updatePhase` | Enum: `Idle`, `Updating`, `UpdatingServices`, `Verifying`, `RollingBack` |
| `spec.infrastructure.database` (CEL) | `has(self.clusterRef) != has(self.host)` → "exactly one of clusterRef or host must be set" |
| `spec.infrastructure.cache` (CEL) | `has(self.clusterRef) != (has(self.servers) && size(self.servers) > 0)` → "exactly one of clusterRef or servers must be set" |
| `spec.globalPolicyOverrides`, `spec.services.keystone.policyOverrides` (CEL) | `!has(self.rules) \|\| self.rules.all(k, size(k) > 0)` → "policy rule name must not be empty" |
| `spec.globalPolicyOverrides`, `spec.services.keystone.policyOverrides` (CEL) | `!has(self.rules) \|\| self.rules.all(k, size(self.rules[k]) > 0)` → "policy rule value must not be empty" |
| `spec.services.keystone` (CEL) | `mode == 'External'` ⇒ `has(self.external)` → "external is required when services.keystone.mode is External" |
| `spec.services.keystone` (CEL) | `has(self.external)` ⇒ `mode == 'External'` → "external may only be set when services.keystone.mode is External" |
| `spec.services.keystone` (CEL) | `mode == 'External'` ⇒ each managed-only field (`replicas`, `image`, `policyOverrides`, `rotationInterval`, `gateway`, `publicEndpoint`, `federationProxyImage`) absent → "services.keystone.\<field\> is forbidden when services.keystone.mode is External" (one rule per field) |

### Validating-webhook rules

The `validate()` method accumulates all errors in a `field.ErrorList` and
returns a single `apierrors.NewInvalid` error keyed on
`GroupKind{Group: "c5c3.io", Kind: "ControlPlane"}`. It does **not**
short-circuit on the first error.

| Rule | Field Path | Error Type | Condition |
| --- | --- | --- | --- |
| Release pattern | `spec.openStackRelease` | `field.Invalid` | Value does not match `^\d{4}\.[12]$`. Defense-in-depth alongside the CRD `+kubebuilder:validation:Pattern` marker. |
| Database mutual exclusivity | `spec.infrastructure.database` | `field.Invalid` | Both `clusterRef` and `host` set, or neither (`(clusterRef != nil) == (host != "")`). Defense-in-depth alongside the CEL `XValidation` rule on `commonv1.DatabaseSpec`. |
| Cache mutual exclusivity | `spec.infrastructure.cache` | `field.Invalid` | Both `clusterRef` and `servers` set, or neither (`(clusterRef != nil) == (len(servers) > 0)`). Defense-in-depth alongside the CEL `XValidation` rule on `commonv1.CacheSpec`. |
| Database replicas quorum | `spec.infrastructure.database.replicas` | `field.Invalid` | Value is exactly `2`. The managed-mode projection turns any `replicas > 1` into a Galera cluster, and a two-node Galera cluster cannot hold a majority — a single pod disruption then loses quorum. Replicas must be 1 (standalone) or >=3. The CRD marker enforces only `Minimum=1` (the shared `commonv1.DatabaseSpec` must not carry a c5c3-specific CEL rule the keystone operator, which ignores `replicas`, would inherit), so this check is **webhook-only**; a zero value (defaulting bypassed) is left to the reconciler's floor. |
| Admin password Secret required | `spec.korc.adminCredential.passwordSecretRef.name` | `field.Required` | `name` is empty — without it the reconciler cannot (re-)mint the admin application credential. **Webhook-only**. |
| Gateway hostname required | `spec.services.keystone.gateway.hostname` | `field.Required` | A `gateway` is configured but its `hostname` is empty. Mirrors the `+kubebuilder:validation:MinLength=1` marker on `commonv1.GatewaySpec.Hostname`; without it the reconciler derives an empty `https:///v3` public endpoint. |
| Empty policy rule name | `spec.globalPolicyOverrides.rules[<key>]`, `spec.services.keystone.policyOverrides.rules[<key>]` | `field.Required` | A rule name (map key) is the empty string. Enforced via the shared `policy.ValidatePolicyRules`, mirrored by the CEL rule on `commonv1.PolicySpec`. |
| Empty policy rule value | `spec.globalPolicyOverrides.rules[<key>]`, `spec.services.keystone.policyOverrides.rules[<key>]` | `field.Required` | A rule value is the empty string. Enforced via the shared `policy.ValidatePolicyRules`, mirrored by the CEL rule on `commonv1.PolicySpec`. |
| External block required | `spec.services.keystone.external` | `field.Required` | `mode: External` but `external` unset. Defense-in-depth mirror of the CEL rule. |
| External authURL required/URL | `spec.services.keystone.external.authURL` | `field.Required` / `field.Invalid` | In External mode, `authURL` empty (Required), or not matching `^https?://[^\s/]+` / failing a full `net/url` parse / exceeding 2048 characters (Invalid). Mirrors the CRD required/pattern/maxLength markers. |
| External caBundle name required | `spec.services.keystone.external.caBundleSecretRef.name` | `field.Required` | `caBundleSecretRef` set with an empty `name`. Mirrors the shared `SecretRefSpec` MinLength marker. |
| Managed-only field forbidden in External mode | `spec.services.keystone.{replicas,image,policyOverrides,rotationInterval,gateway,publicEndpoint,federationProxyImage}` | `field.Forbidden` | The field is set while `mode: External`. Defense-in-depth mirror of the per-field CEL rules. |
| Federation proxy image resolvable | `spec.services.keystone.federationProxyImage` | `field.Required` / `field.Invalid` | Empty `repository`, or neither/both of `tag` and `digest`. Surfaces on the ControlPlane the operator edits rather than as an opaque `KeystoneProjectionRejected` condition on the child. |
| Dashboard public endpoint is a URL | `spec.services.horizon.publicEndpoint` | `field.Invalid` | Not an absolute HTTP(S) URL with a host. Keystone matches the derived WebSSO origin verbatim, so an unusable endpoint could never match any dashboard. |
| Dashboard public endpoint is a bare origin | `spec.services.horizon.publicEndpoint` | `field.Invalid` | Carries a path, query, or fragment (a single trailing `/` is trimmed and allowed). The `^https?://` pattern anchors only the prefix, so `https://horizon.example.com?utm=1` is schema-legal and would render the trusted origin `https://horizon.example.com?utm=1/auth/websso/` — accepted by Keystone, matched by nothing. **Webhook-only.** |
| Dashboard public endpoint agrees with the gateway | `spec.services.horizon.publicEndpoint` | `field.Invalid` | With `services.horizon.gateway` set: the scheme is not `https` (the listener terminates TLS, and Keystone POSTs the unscoped WebSSO token to this origin), or its host differs from `gateway.hostname` (Django derives the origin it sends from the request `Host` header). The port may differ. **Cross-field, webhook-only.** |
| Gateway hostname is a usable DNS name | `spec.services.{keystone,horizon}.gateway.hostname` | `field.Invalid` | A wildcard, an embedded port, a path, a scheme, a control character, or over 253 characters. Each shape either breaks the browser-facing origins derived from the hostname or overruns the children's own `MaxLength` markers on those origins. |
| External block forbidden in non-External mode | `spec.services.keystone.external` | `field.Forbidden` | `external` set while `mode` is not `External`. Defense-in-depth mirror of the CEL rule. |
| Infrastructure forbidden in External mode | `spec.infrastructure` | `field.Forbidden` | `spec.infrastructure` set while `mode: External`. **Cross-field, webhook-only** — CEL cannot span `spec.infrastructure` and `spec.services.keystone` (phase 2 relaxes this to optional). |
| Horizon forbidden in External mode | `spec.services.horizon` | `field.Forbidden` | `services.horizon` set while `mode: External` (P2 — Horizon needs its own External-mode design). **Cross-field, webhook-only.** |
| Infrastructure required in non-External mode | `spec.infrastructure` | `field.Required` | `spec.infrastructure` unset while the keystone mode is not `External` (Managed, unset mode, or `services.keystone` unset). Preserves today's contract now the Go field is an optional pointer. **Webhook-only.** |
| Service-account shape | `spec.korc.serviceAccounts[]` | `field.Invalid` / `field.Required` / `field.Duplicate` / `field.TooMany` | Per-entry defense-in-depth mirrors of the CRD markers (name shape, `project.name` required, comma guards, child-CR name-length bound). |
| Service-account identity uniqueness | `spec.korc.serviceAccounts[].userName` | `field.Duplicate` | Two entries resolve to the same effective `(userName, domainName)` — they would project two managed `User`s onto one Keystone user. **Cross-item, webhook-only.** |
| Service-account admin collision | `spec.korc.serviceAccounts[].userName` | `field.Invalid` | An entry's effective identity equals the admin identity (`adminCredential.userName`/`domainName`) — a managed `User` would take over the admin user and rotate its password. **Cross-field, webhook-only.** |
| Service-account managed-project uniqueness | `spec.korc.serviceAccounts[].project.name` | `field.Duplicate` | Two `project.create: true` entries name the same project in one domain — each managed `Project` would adopt the other's row. **Cross-item, webhook-only.** |

### Update-only immutability rules

On **update** the validating webhook additionally rejects changes to the
create-only fields below, accumulating them into the same `field.ErrorList` as
the spec checks above. These fields are **webhook-only** (the affected leaves
live in the shared `commonv1.DatabaseSpec`/`CacheSpec`, which the keystone
operator reuses and which must not carry c5c3-specific CEL immutability markers).
Flipping the database/cache mode or renaming a managed `clusterRef` would leave
the previously-projected MariaDB/Memcached child (and its per-ControlPlane
credential) orphaned and owned until the ControlPlane is deleted; renaming
`cloudCredentialsRef.secretName` would leak the previously-projected K-ORC
clouds.yaml ExternalSecret. The database **name** and the **region** are also
immutable: both are projected verbatim into the Keystone child's now-immutable
`spec.database.database` / `spec.bootstrap.region`, so a rename here would make
the next reconcile attempt an update the Keystone CEL rule rejects, wedging the
loop — rejecting it at the ControlPlane layer surfaces a clean error instead.

The webhook additionally rejects an **openStackRelease downgrade**: a monotonic
upgrade check parses the `YYYY.N` release into `(year, minor)` and rejects a
lower tuple while allowing upgrades and same-release no-ops. Keystone DB
migrations are forward-only, so a downgrade would project an older image against
an already-migrated schema — an unrecoverable state.

The webhook also **gates the keystone-mode transition**. Both directions are
rejected with **distinct messages** — and deliberately as *rejections*, not
immutable markers, because `External → Managed` must become a *gated* takeover
in phase 3 (an immutable marker could never be relaxed to a gated transition):

- `Managed → External` is rejected outright — adopting an existing installation
  must be a fresh External-mode ControlPlane, not an in-place flip of a live
  one.
- `External → Managed` (or away from External by removing the keystone service)
  is rejected with a message naming the **reserved phase-3 takeover**.

An **infrastructure presence flip** (adding or removing the whole
`spec.infrastructure` block on update while the mode is unchanged) is rejected
independently as defense-in-depth for webhook-bypassed states.

| Rule | Field Path | Condition |
| --- | --- | --- |
| Managed → External rejected | `spec.services.keystone.mode` | Old not External, new External — "cannot be changed to External" |
| External → Managed rejected | `spec.services.keystone.mode` | Old External, new not External — names the reserved phase-3 takeover |
| Infrastructure presence immutable | `spec.infrastructure` | The block was added or removed (presence flip) while the mode is unchanged |
| Database mode immutable | `spec.infrastructure.database` | `clusterRef` nil-ness changed (managed ↔ brownfield) |
| Database clusterRef.name immutable | `spec.infrastructure.database.clusterRef.name` | Both managed, but the name changed |
| Database name immutable | `spec.infrastructure.database.database` | The database name changed |
| Database replicas immutable | `spec.infrastructure.database.replicas` | The value changed. `replicas` is projected into the managed MariaDB child's replica count and derived Galera topology, so a live edit would drive a destructive Update on the owned cluster (toggling Galera off or scaling a running Galera cluster down); the topology can only be changed safely by recreating the control plane. |
| Cache mode immutable | `spec.infrastructure.cache` | `clusterRef` nil-ness changed (managed ↔ brownfield) |
| Cache clusterRef.name immutable | `spec.infrastructure.cache.clusterRef.name` | Both managed, but the name changed |
| Cloud secretName immutable | `spec.korc.adminCredential.cloudCredentialsRef.secretName` | The value changed |
| Region immutable | `spec.region` | The region changed |
| Service-account identity/project immutable | `spec.korc.serviceAccounts[].{userName,domainName,project.name,project.create}` | For an entry matched by `name` across old/new, one of these changed — an in-place repoint would rename or re-own live Keystone resources; remove and re-add the entry instead. `adopt` stays mutable (flipping it to `true` is the documented collision remediation). |
| Release downgrade rejected | `spec.openStackRelease` | New release `(year, minor)` is lower than the old (upgrades and same-release updates allowed) |

---

## Webhooks

The `ControlPlaneWebhook` struct implements both defaulting and validating
admission webhooks for the `ControlPlane` CRD via the typed-generic
`admission.Defaulter[*ControlPlane]` and `admission.Validator[*ControlPlane]`
interfaces from controller-runtime. `CredentialRotation` and `SecretAggregate`
have no webhook at this level.

The struct carries a `Client client.Reader`, injected at startup with the
manager's **uncached API reader** (`mgr.GetAPIReader()`). `ValidateCreate` uses
it to enforce one ControlPlane per namespace; reading the API server directly
ensures concurrent or cache-sync-window CREATEs cannot both pass the check
against an empty informer cache. The spec-level `validate()` rules do not touch
the client.

### Registration

```go
func (w *ControlPlaneWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error
```

Registers both webhooks with the manager using
`builder.WebhookManagedBy[*ControlPlane]`. The generated webhook paths are
`/mutate-c5c3-io-v1alpha1-controlplane` (mutating) and
`/validate-c5c3-io-v1alpha1-controlplane` (validating); both use
`failurePolicy=fail`, `sideEffects=None`, and `admissionReviewVersions=v1`.
Both webhooks fire on `create`/`update` only. `delete` is deliberately **not**
registered: the webhook is served in-process by the operator, so with
`failurePolicy=fail` a `delete` rule would let a down operator block CR — and
thereby namespace — deletion.

### Defaulting Webhook

```go
func (w *ControlPlaneWebhook) Default(_ context.Context, obj *ControlPlane) error
```

Fills only zero-valued fields with their documented defaults, leaving any
explicit value untouched. It is **idempotent**: applying it twice produces the
same result. The defaults split across **two layers** that do **not** uniformly
overlap:

- **Dual-layer defaults** — also expressed as a `+kubebuilder:default` marker on
  the corresponding spec field, so the marker covers the normal admission path
  and the webhook covers callers that bypass the CRD default. These are `region`,
  `cloudCredentialsRef.secretName`, `applicationCredential.restricted`,
  `applicationCredential.rotation.mode`, `cloudCredentialsRef.cloudName`,
  `services.keystone.mode` (→ `Managed`), `services.keystone.external.endpointType`
  (→ `public`, when the external block is present), and the admin identity fields
  `adminCredential.userName` / `.projectName` (→ `admin`) / `.domainName`
  (→ `Default`).

**Mode-aware infrastructure defaulting.** The webhook defaults
`services.keystone.mode` to `Managed` first (when a keystone block is present
with an empty mode), then branches on the mode:

- In **External** mode it **does not invent** any managed database/cache
  `clusterRef` — `spec.infrastructure` is left unset (the validating webhook
  forbids it in External mode) — and only defaults the external block's own
  `endpointType` (→ `public`) and `caBundleSecretRef.key` (→ `ca.crt`, when the
  ref is set).
- In **Managed** mode (or when the keystone service is unset) it materializes
  and defaults the shared backing services exactly as before, so a minimal
  managed CR still round-trips unchanged.
- **Webhook-only defaults** — materialized by the webhook with **no**
  `+kubebuilder:default` marker. These are the shared-`commonv1`-leaf defaults
  (`database.database`, `database.secretRef.name`, `cache.backend`,
  `passwordSecretRef.name`, `passwordSecretRef.key`) and the two managed
  `clusterRef` names (`openstack-db`, `openstack-memcached`). They carry no
  marker because the `commonv1` `DatabaseSpec` / `CacheSpec` / `SecretRefSpec`
  types are reused by the Keystone CRD, and a c5c3-specific `+kubebuilder:default`
  on those shared types would leak into Keystone's CRD. The two managed
  `clusterRef` names are **brownfield-guarded**: they are only invented when the
  brownfield discriminator (`database.host` / `cache.servers`) is unset, so the
  database/cache XOR validation still passes for a brownfield CR.

The defaulting constants in `controlplane_webhook.go` (e.g. `DefaultRegion`
`"RegionOne"`, `DefaultDatabaseName` `"keystone"`, `DefaultCacheBackend`
`"dogpile.cache.pymemcache"`) are the single source of truth shared with the
markers' documented values where a marker also exists.

| Field | Condition | Default Value | Mechanism |
| --- | --- | --- | --- |
| `spec.region` | `== ""` | `"RegionOne"` | Marker + webhook |
| `spec.korc.adminCredential.cloudCredentialsRef.secretName` | `== ""` | `"k-orc-clouds-yaml"` | Marker + webhook |
| `spec.korc.adminCredential.applicationCredential.restricted` | `== nil` | `true` (pointer set to `true`; an explicit `false` is preserved) | Marker + webhook |
| `spec.korc.adminCredential.applicationCredential.rotation.mode` | `== ""` | `PasswordDriven` | Marker + webhook |
| `spec.korc.adminCredential.cloudCredentialsRef.cloudName` | `== ""` | `"admin"` | Marker + webhook |
| `spec.infrastructure.database.database` | `== ""` | `"keystone"` | Webhook-only |
| `spec.infrastructure.database.secretRef.name` | `== ""` | `"keystone-db"` [†](#secretref-default-note) | Webhook-only |
| `spec.infrastructure.database.clusterRef.name` | `host == ""` (managed mode) | `"openstack-db"` | Webhook-only, brownfield-guarded |
| `spec.infrastructure.cache.backend` | `== ""` | `"dogpile.cache.pymemcache"` | Webhook-only |
| `spec.infrastructure.cache.clusterRef.name` | `len(servers) == 0` (managed mode) | `"openstack-memcached"` | Webhook-only, brownfield-guarded |
| `spec.korc.adminCredential.passwordSecretRef.name` | `== ""` | `"keystone-admin"` | Webhook-only |
| `spec.korc.adminCredential.passwordSecretRef.key` | `== ""` | `"password"` | Webhook-only |
| `spec.services.keystone.mode` | `== ""` (keystone block present) | `Managed` | Marker + webhook |
| `spec.services.keystone.external.endpointType` | `== ""` (External mode, external present) | `public` | Marker + webhook |
| `spec.services.keystone.external.caBundleSecretRef.key` | `== ""` (ref present) | `"ca.crt"` | Webhook-only |
| `spec.korc.adminCredential.userName` | `== ""` | `"admin"` | Marker + webhook |
| `spec.korc.adminCredential.projectName` | `== ""` | `"admin"` | Marker + webhook |
| `spec.korc.adminCredential.domainName` | `== ""` | `"Default"` | Marker + webhook |
| `spec.korc.serviceAccounts[].userName` | `== ""` | the entry's `name` | Webhook-only |

<a id="secretref-default-note"></a>
> **† `database.secretRef.name` default — managed-mode convenience name only.**
> The webhook still defaults `secretRef.name` to the `keystone-db` value
> (unchanged), but that default is no longer the Secret Keystone consumes. In
> **managed mode** `database.secretRef` is **operator-owned**: `reconcileDBCredentials`
> materialises a per-ControlPlane `ExternalSecret` and `reconcileKeystone`
> overrides the projected Keystone CR's `spec.database.secretRef` to the
> operator-owned Secret `{controlplane.Name}-keystone-db-credentials` (key
> `"password"`) — see [managed-mode provisioning](#infrastructurespec). In
> production the bare name `keystone-db` does not resolve to any cluster
> Secret (only the kind overlay ships a `keystone-db` ExternalSecret, pinned
> to the default identity's path for standalone Keystone instances); a
> managed ControlPlane never consumes it either way.
> A **brownfield** ControlPlane (`database.host` set,
> `clusterRef == nil`) **MUST supply** its own `database.secretRef` Secret
> out-of-band; the operator projects no ExternalSecret in brownfield mode.

> **Operational contract.** The webhook only materializes the Secret
> *names and references* — it never invents credential material. In **managed
> mode** (`database.clusterRef` set) the operator itself projects the admin
> password: it creates a per-ControlPlane admin `ExternalSecret`
> (`{controlplane.Name}-keystone-admin-credentials`) materialising the password
> from OpenBao and **overrides** the projected Keystone CR's
> `bootstrap.adminPasswordSecretRef` onto it, so the cp-level `passwordSecretRef`
> (default `keystone-admin`) is **not** the Secret the child consumes — see the
> [`passwordSecretRef` managed-mode note](#admincredentialspec) above. In
> **brownfield mode** (`database.clusterRef == nil`) the operator projects no
> admin ExternalSecret, so a ControlPlane that omits `spec.korc` (or
> `spec.infrastructure.database.secretRef`) relies on the cluster operator having
> **pre-seeded** the referenced Secrets in the ControlPlane's namespace before the
> credential sub-reconcilers can advance: the admin password Secret
> (`keystone-admin`, key `password`) and the K-ORC `clouds.yaml`
> ExternalSecret/Secret (`k-orc-clouds-yaml`). The infrastructure layer seeds
> these (see the [quick-start](../../quick-start-controlplane.md)). A missing
> admin password Secret degrades to `KORCReady=False` / `WaitingForAdminPassword`,
> and a not-yet-synced `clouds.yaml` to `AdminCredentialReady=False` /
> `WaitingForCloudsYaml` — never a silent authentication. A `clouds.yaml` that is
> synced but stale (a re-mint revoked the old credential while ESO has not yet
> re-materialised the Secret) degrades to `AdminCredentialReady=False` /
> `WaitingForCloudsYamlSync`: `reconcileAdminCredential` semantically compares the
> materialised Secret (by parsed application-credential id+secret) against the
> freshly assembled credential and forces an ESO re-sync, so the gate never passes
> against a revoked credential.
> `TestIntegration_MinimalManagedToReady` encodes this contract by pre-creating
> those Secrets at the defaulted names.

### Validating Webhook

```go
func (w *ControlPlaneWebhook) ValidateCreate(_ context.Context, obj *ControlPlane) (admission.Warnings, error)
func (w *ControlPlaneWebhook) ValidateUpdate(_ context.Context, _, newObj *ControlPlane) (admission.Warnings, error)
func (w *ControlPlaneWebhook) ValidateDelete(_ context.Context, _ *ControlPlane) (admission.Warnings, error)
```

- `ValidateCreate` and `ValidateUpdate` both delegate to the internal
  `validate()` method (see [Validating-webhook rules](#validating-webhook-rules)).
  `ValidateCreate` additionally enforces the one-ControlPlane-per-namespace
  contract: it lists existing ControlPlanes in the new object's namespace
  through the uncached API reader and rejects the CREATE with a `Forbidden`
  error naming the incumbent when one already exists. The check runs only on
  CREATE so an existing CR stays mutable; `ValidateUpdate` validates the new
  object only.
- `ValidateDelete` always returns `nil, nil`. It exists only to satisfy the
  `admission.Validator` interface and is **never invoked** — the validating
  webhook does not register the `delete` verb, so **deletion is unconditionally
  allowed** even while the operator is down.

---

## Status Conditions

The ControlPlane status is driven by ten sub-reconcilers, each owning one
condition type, plus an aggregate `Ready` condition. The condition-type
constants in `controlplane_controller.go` (`subConditionTypes`) are the single
source of truth; call sites reference the constants rather than inline literals.

The sub-reconcilers run in dependency order; a stage that has not converged
requeues and stops the chain, so later conditions are never computed against a
half-built earlier stage. Four stages additionally gate **explicitly** on an
earlier condition being `True` (`reconcileKeystone` on `InfrastructureReady`,
`reconcileHorizon` on `KeystoneReady`, `reconcileAdminCredential` on `KORCReady`,
`reconcileCatalog` and `reconcileServiceAccounts` on `AdminCredentialReady`):

```
InfrastructureReady → ESOTenantStoreReady → DBCredentialsReady → AdminPasswordReady
  → KeystoneReady → HorizonReady → KORCReady → AdminCredentialReady
  → CatalogReady → ServiceAccountsReady
```

`ESOTenantStoreReady` runs ahead of every store-consuming stage because it
provisions the per-tenant `SecretStore` they route their ExternalSecrets and
PushSecrets through. It is **mode-independent**: an External-mode ControlPlane
provisions the same tenant store (it just never seeds a bootstrap path through
it), so both `ESOTenantStoreReady` and `HorizonReady` appear on an External-mode
CR's status — the latter as `HorizonNotManaged`, since the dashboard is forbidden
in External mode.

`ServiceAccountsReady` gates explicitly on `AdminCredentialReady` (like
`CatalogReady`): the admin credential must be minted before K-ORC can project
the service-account User/Project.

`Ready` is `True` (reason `AllReady`) **only** when all sub-conditions are
`True` (via `conditions.AllTrue`); otherwise it is `False` (reason
`NotAllReady`). One exception bypasses the aggregation: when a namespace holds
more than one ControlPlane (possible only for CRs that predate the
one-per-namespace webhook guard or bypassed it), every CR except the oldest is
parked with `Ready=False` reason `DuplicateControlPlane` naming the incumbent,
and none of its sub-reconcilers run. For the full flow, see the
[ControlPlane Reconciler reference](./controlplane-reconciler.md).

### InfrastructureReady

Set by `reconcileInfrastructure`.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `InfrastructureReady` | All managed backing services are ensured and report Ready (or the control plane uses only brownfield infra, so there is nothing to provision). |
| `False` | `WaitingForDatabase` | Managed MariaDB is ensured but not yet Ready. |
| `False` | `WaitingForCache` | Managed Memcached is ensured but not yet Ready. |
| `False` | `MariaDBError` | Error create-or-updating the MariaDB child. |
| `False` | `MemcachedError` | Error create-or-updating the Memcached child. |
| `True` | `ExternallyManaged` | `services.keystone.mode` is `External`: identity is managed against `services.keystone.external.authURL`, so no MariaDB/Memcached is provisioned. |
| `False` | `InfrastructureNotConfigured` | `spec.infrastructure` is unset on a **non**-External ControlPlane. The validating webhook requires the block outside External mode, so this only fires for a webhook-bypassed CR; it fails closed rather than dereferencing the nil block. |

### ESOTenantStoreReady

Set by `reconcileESOTenantStore`. It provisions the per-tenant `SecretStore` (plus
its ServiceAccount and mTLS certificate) that every store-consuming stage routes
its ExternalSecrets and PushSecrets through, which is why it runs ahead of them.
It is **mode-independent** — an External-mode ControlPlane provisions the same
store.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `ESOTenantStoreReady` | The operator-provisioned per-tenant `SecretStore` is Ready. |
| `True` | `StoreRefOverridden` | An explicit `spec.secretStoreRef` opts out of the operator-provisioned store: the ControlPlane owns the referenced store's lifecycle, so nothing is provisioned. The selected store's readiness is still gated by each store-consuming sub-reconciler. |
| `False` | `SecretStoreNotReady` | The per-tenant `SecretStore` is not Ready yet; waiting on cert issuance and the OpenBao backend. |
| `False` | `ProvisioningError` | Error ensuring the per-tenant secret-store objects (SecretStore, ServiceAccount, certificate). |

### DBCredentialsReady

Set by `reconcileDBCredentials`. In managed mode (`database.clusterRef` set) it
create-or-updates the per-ControlPlane DB-credential `ExternalSecret`
(`{name}-keystone-db-credentials`, reading OpenBao path
`openstack/keystone/{namespace}/{name}/db`, hourly refresh) and mirrors its
Ready status. The OpenBao-backed `ClusterSecretStore` is checked first so an
ESO/OpenBao outage surfaces promptly instead of hiding behind the
ExternalSecret's stale per-object Ready cache.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `DBCredentialsReady` | The DB-credential ExternalSecret is Ready; the materialised Secret exists. |
| `True` | `BrownfieldUserSuppliedCredential` | Brownfield database (`clusterRef` unset): the user supplies the DB-credential Secret out-of-band, so no ExternalSecret is projected and the chain proceeds immediately. |
| `True` | `ExternallyManaged` | `services.keystone.mode` is `External`: the ControlPlane manages no database at all, so nothing is projected and neither OpenBao nor the `ClusterSecretStore` is consulted. |
| `False` | `SecretStoreNotReady` | The store selected by `spec.secretStoreRef` (a ClusterSecretStore or a namespaced SecretStore) is not Ready; the secret backend is unreachable. |
| `False` | `GeneratorError` | Error ensuring the dynamic DB-credential objects (the MariaDB `database` engine-backed generator that issues short-lived credentials). |
| `False` | `ExternalSecretError` | Error ensuring or checking the DB-credential ExternalSecret. |
| `False` | `WaitingForDBCredentialSecret` | The ExternalSecret is ensured but ESO has not yet synced it to Ready. In `Static` mode (the explicit opt-out, and every dedicated managed database) the message names the OpenBao KV path the credential has to be **seeded at out-of-band** — nothing seeds it, so until it exists this is where the ControlPlane stops. |

### AdminPasswordReady

Set by `reconcileAdminPassword`. It runs **before** the Keystone stage because
the keystone-operator's `SecretsReady` gate needs the admin-password
ExternalSecret to exist before the projected Keystone child references it. In
managed mode it create-or-updates the per-ControlPlane admin-password
`ExternalSecret` (`{name}-keystone-admin-credentials`, reading OpenBao path
`bootstrap/{namespace}/{name}-keystone/admin` — keystone-**name**-scoped so it
matches the seeder and the keystone-operator rotation PushSecret) and mirrors
its Ready status.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `AdminPasswordReady` | The admin-password ExternalSecret is Ready; the materialised Secret exists. |
| `True` | `BrownfieldUserSuppliedCredential` | Brownfield database (`clusterRef` unset): the user supplies the admin-password Secret out-of-band, so no ExternalSecret is projected and the chain proceeds immediately. |
| `True` | `ExternallyManaged` | `services.keystone.mode` is `External`: the admin password is read from the user-supplied `korc.adminCredential.passwordSecretRef` Secret; no ExternalSecret is projected and no OpenBao bootstrap path is seeded. Updating that Secret is what drives a hash-driven re-mint of the admin application credential. |
| `False` | `SecretStoreNotReady` | The store selected by `spec.secretStoreRef` (a ClusterSecretStore or a namespaced SecretStore) is not Ready; the secret backend is unreachable. |
| `False` | `ExternalSecretError` | Error ensuring or checking the admin-password ExternalSecret. |
| `False` | `WaitingForAdminPasswordSecret` | The ExternalSecret is ensured but ESO has not yet synced it to Ready. |

### KeystoneReady

Set by `reconcileKeystone` (gated on `InfrastructureReady`).

| Status | Reason | When |
| --- | --- | --- |
| `True` | `KeystoneReady` | The projected Keystone CR reports Ready. |
| `True` | `KeystoneNotManaged` | `services.keystone` is unset: this ControlPlane manages no identity plane. |
| `True` | `ExternallyManaged` | `services.keystone.mode` is `External`: identity is managed against `services.keystone.external.authURL`; no Keystone child is projected and none is deleted. |
| `False` | `WaitingForInfrastructure` | `InfrastructureReady` is not `True`; Keystone projection deferred. |
| `False` | `WaitingForKeystone` | The Keystone CR is ensured but not yet Ready. |
| `False` | `InvalidRotationInterval` | `services.keystone.rotationInterval` could not be converted to a cron schedule. |
| `False` | `KeystoneProjectionRejected` | The Keystone API server rejected the projected spec (HTTP 422) — almost always a now-immutable db/bootstrap field that diverged from the frozen Keystone child. Reconcile the ControlPlane spec back to the child's values, or recreate the child, to recover. Distinct from `KeystoneError` so the wedge is diagnosable from the condition. |
| `False` | `KeystoneError` | Error create-or-updating the Keystone CR. |

### HorizonReady

Set by `reconcileHorizon` (gated on `KeystoneReady` — the dashboard authenticates
against the Keystone child). The dashboard is **forbidden in External mode**, so
an External-mode ControlPlane always reports `HorizonNotManaged`.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `HorizonReady` | The projected Horizon CR reports Ready. |
| `True` | `HorizonNotManaged` | `services.horizon` is unset (or the ControlPlane is in External mode): no dashboard is managed, so the aggregate `Ready` is not blocked. Any previously-projected Horizon child is **preserved** unless the `c5c3.io/allow-horizon-deletion: "true"` annotation opts in to its deletion. |
| `False` | `WaitingForKeystone` | `KeystoneReady` is not `True`; Horizon projection deferred. |
| `False` | `WaitingForHorizon` | The Horizon CR is ensured but not yet Ready. |
| `False` | `IdentityBackendsUnavailable` | Listing the Keystone child's identity backends for the Horizon projection failed. The chain stops rather than projecting an empty `websso` block, which would silently remove a working SSO button from the login page. |
| `False` | `HorizonProjectionRejected` | The Horizon API server rejected the projected spec (HTTP 422) — the projection violates a CRD/webhook rule. Reconcile the ControlPlane spec to a valid projection to recover. |
| `False` | `HorizonError` | Error create-or-updating the Horizon CR. |

### KORCReady

Set by `reconcileKORC`.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `ApplicationCredentialMinted` | The K-ORC admin `ApplicationCredential` is minted and reports `Available=True`. |
| `False` | `WaitingForAdminPassword` | The admin password Secret/key is not yet available; minting deferred. |
| `False` | `WaitingForCABundle` | **External mode only.** The Secret referenced by `external.caBundleSecretRef` does not exist yet, or exists with a missing/empty key; the mint is deferred rather than attempted against an endpoint whose certificate cannot be verified. The present-but-empty shape is the normal transient of a two-step "create the Secret, then populate it" flow. |
| `False` | `CABundleError` | **External mode only.** Non-missing error reading the external CA bundle. |
| `False` | `ApplicationCredentialFailed` | The `ApplicationCredential` reports a terminal K-ORC error (`GetTerminalError` — an unrecoverable/invalid-config reason such as K-ORC being unable to authenticate); the message folds in any stuck admin Domain/User import. |
| `False` | `WaitingForApplicationCredential` | The `ApplicationCredential` CR is ensured but not yet `Available`; the message folds in any stuck admin Domain/User import. |
| `False` | `ReMinting` | The admin application credential is being re-minted (delete + recreate) after an admin-password change or a `CredentialRotation` request; awaiting K-ORC's revoke of the previous credential. The old credential is **already revoked** at the Keystone level — see the [consumption contract](#admincredentialspec). |
| `False` | `ReMintStalled` | The `ApplicationCredential` has been `Terminating` longer than `remintStallTimeout`; K-ORC may be unable to reach Keystone to revoke the previous credential. Escalated from `ReMinting` so a stuck finalizer is alertable instead of looping silently. |
| `False` | `AdminPasswordError` | Non-missing error reading the admin password. |
| `False` | `PasswordCloudError` | Error ensuring the operator-owned password-based `clouds.yaml` Secret the mint authenticates with. |
| `False` | `AdminImportError` | Error create-or-updating the K-ORC admin `User`/`Domain` imports. |
| `False` | `SecretError` | Error ensuring the application-credential Secret, or regenerating its `value` for a re-mint. |
| `False` | `SeedCloudsYamlError` | Error seeding the bootstrap `clouds.yaml`. |
| `False` | `PushSecretError` | Error ensuring the admin app-credential `PushSecret`, or forcing its CA-bundle re-push. |
| `False` | `ExternalSecretError` | Error ensuring the K-ORC `clouds.yaml` `ExternalSecret`. |
| `False` | `ApplicationCredentialError` | Error create-or-updating (or deleting, for a re-mint) the `ApplicationCredential` CR. |
| `False` | `FinalizingORC` | The ControlPlane is being deleted and the operator is waiting for the owned K-ORC CRs and PushSecrets to finish their teardown (K-ORC's finalizer revokes the credential against Keystone) before releasing the `c5c3.io/orc-teardown` finalizer. Set by `reconcileDelete`; see the [reconciler reference](./controlplane-reconciler.md). |
| `False` | `AuthenticationFailed` | **External mode only.** The external Keystone rejected the admin credential (HTTP 401) — typically the password was rotated out-of-band and `passwordSecretRef` is stale. K-ORC's message is relayed verbatim. |
| `False` | `EndpointUnreachable` | **External mode only.** `services.keystone.external.authURL` could not be dialled (DNS failure, connection refused, timeout). |
| `False` | `TLSVerificationFailed` | **External mode only.** The external endpoint's certificate did not verify; supply the private CA via `external.caBundleSecretRef`. |
| `False` | `CatalogEndpointMismatch` | **External mode only.** Authentication succeeded but the requested interface/region is absent from the external service catalog — a wrong `external.endpointType` or `spec.region`. |
| `False` | `CredentialDrift` | **External mode only.** An application-credential create against a stale resolve-once import id yielded a Keystone 403 (`identity:create_application_credential`). Drift is surfaced, never remediated. |
| `False` | `ImportStalled` | **External mode only.** An admin Domain/User import has been waiting to be "created externally" for longer than `externalImportStallGrace` (2m). In External mode the import target already exists, so this is a misconfiguration. |

### AdminCredentialReady

Set by `reconcileAdminCredential` (gated on `KORCReady`, the OpenBao-backed
`ClusterSecretStore` being Ready, the K-ORC `clouds.yaml` ExternalSecret being
Ready, the admin app-credential `PushSecret` having actually synced to OpenBao,
**and** the materialised `clouds.yaml` Secret semantically matching (parsed
application-credential id+secret) the freshly assembled credential).

| Status | Reason | When |
| --- | --- | --- |
| `True` | `AdminCredentialReady` | The admin application credential is committed to the owned Secret, mirrored to OpenBao, and the materialised `clouds.yaml` Secret matches the assembled credential. |
| `False` | `WaitingForKORC` | `KORCReady` is not `True`; credential push deferred. |
| `False` | `CredentialDrift` | **External mode only.** `KORCReady` reports drift in the external installation (`AuthenticationFailed` or `CredentialDrift`). The operator never remediates the external Keystone; update the `passwordSecretRef` Secret to drive a re-mint. |
| `False` | `SecretStoreNotReady` | The store selected by `spec.secretStoreRef` (a ClusterSecretStore or a namespaced SecretStore) is not Ready; the secret backend is unreachable. |
| `False` | `WaitingForCloudsYaml` | The operator-created per-ControlPlane `k-orc-clouds-yaml` ExternalSecret in the control-plane namespace (co-located with the K-ORC CRs per C1; created and owned by `reconcileKORC`) is not yet Ready. |
| `False` | `WaitingForCredentialID` | K-ORC has not yet reported the minted application credential's id; the assembly is deferred until it does. |
| `False` | `WaitingForAppCredentialSecret` | The operator-owned app-credential Secret does not exist yet, or carries no credential key — the mint is not complete. |
| `False` | `WaitingForPushSecret` | The admin app-credential `PushSecret` has not synced the assembled `clouds.yaml` to OpenBao yet. `AdminCredentialReady` is gated on the PushSecret's `Ready` condition — not merely on the CR existing — so a backend permission failure (e.g. the ESO role missing the push policy) cannot yield a false-positive Ready while OpenBao still serves the password-based bootstrap `clouds.yaml`. |
| `False` | `WaitingForCloudsYamlSync` | The materialised `k-orc-clouds-yaml` Secret is absent or still holds a stale credential (a re-mint revoked the old one but ESO has not re-synced yet). `reconcileAdminCredential` stamps the `external-secrets.io/force-sync` annotation to force an immediate re-sync and compares the materialised Secret semantically (parsed application-credential id+secret) against the assembled credential before reporting Ready, so the condition never reads `True` against a revoked credential. |
| `False` | `CloudsYamlSyncStuck` | The materialised `k-orc-clouds-yaml` Secret has failed to match the assembled credential for longer than `cloudsYamlSyncStuckTimeout` (measured from the credential's `LastRotation`); the ESO ExternalSecret or OpenBao backend may be unable to sync. Escalated from `WaitingForCloudsYamlSync` so a never-converging sync is alertable and distinguishable from a transient miss. |
| `False` | `CloudsYamlError` | Error checking the `clouds.yaml` ExternalSecret, forcing its re-sync, or reading the materialised Secret. |
| `False` | `SecretError` | Error ensuring the operator-owned application-credential Secret. |
| `False` | `PushSecretError` | Error ensuring the OpenBao PushSecret, stamping its content-hash re-push annotation, or reading it back for the sync gate. |

### CatalogReady

Set by `reconcileCatalog` (gated on `AdminCredentialReady`, **and** on every
catalog child reporting `Available` for its current generation —
`korcAvailableUpToDate`, which refuses a stale `Available` condition whose
`ObservedGeneration` lags the object, so an endpoint/region edit cannot flip
`CatalogReady` True before K-ORC re-reconciles).

What "every catalog child" means depends on the Keystone mode. In **Managed**
mode the control plane owns the catalog and registers the identity `Service` and
its public `Endpoint`. In **External** mode it is import-first: the identity
`Service` and the `Endpoint` of the interface `endpointType` selects are the
gating unmanaged imports (the other two interfaces are imported for visibility
only — see [ExternalCatalogSpec](#externalcatalogspec)), plus one managed
`Service`/`Endpoint` set per declared [`managedEntries`](#externalcatalogspec)
entry.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `CatalogRegistered` | **Managed mode only.** Every managed catalog entry is registered as K-ORC CRs **and** reports `Available`. The catalog is a per-service table whose only entry today is the identity (Keystone) `Service` and its public `Endpoint`; the message counts the registered entries, so a future second service is one more entry rather than a reworded condition. |
| `True` | `CatalogImported` | **External mode only.** The external identity `Service` and the endpoint interface `endpointType` selects resolved as unmanaged imports, and every declared managed entry is `Available`. The message reports how many of the three endpoint interfaces resolved. Deliberately distinct from `CatalogRegistered`: nothing was registered, and conflating the two would make "did this ControlPlane write to my catalog?" unanswerable from status. |
| `False` | `WaitingForAdminCredential` | `AdminCredentialReady` is not `True`; catalog reconciliation deferred. |
| `False` | `WaitingForCatalog` | A catalog child is reconciled but not yet `Available` for the current generation (a stale `Available` condition whose `ObservedGeneration` lags the object does not count). In External mode this names the gating import or declared entry that has not resolved. |
| `False` | `CatalogFailed` | A catalog child reports a terminal K-ORC error (`GetTerminalError`). In External mode this is where the **>1-match** half of the ambiguity contract lands: K-ORC refuses to guess and stops retrying, and the message relays it verbatim plus a hint at `external.catalog.identityServiceName` (or, for an endpoint import, at the region limitation no spec field can fix). Terminal errors are surfaced for **every** import, gating or not — with one exception: a >1-match on a **non-gating** interface has no remediation and nothing depends on it, so it is tolerated exactly like a non-gating `ImportStalled` and reported as `resolved: false`. |
| `False` | `ImportStalled` | **External mode only.** A **gating** catalog import has been waiting to be "created externally" for longer than `externalImportStallGrace` (2m). This is the **0-match** half of the ambiguity contract: a gating import's target pre-exists by definition, so the wait never ends on its own. The message names `external.endpointType` and `spec.region` as the likely causes, and for an endpoint import the third possibility — the external catalog publishes no such interface. A non-gating interface import stalls on the same marker without failing the condition. |
| `False` | `AuthenticationFailed` \| `EndpointUnreachable` \| `TLSVerificationFailed` \| `CatalogEndpointMismatch` \| `CredentialDrift` | **External mode only.** An unresolved import carries a K-ORC message identifying one of these failure classes; it is relayed verbatim (see [`KORCReady`](#korcready) for each class). `CatalogEndpointMismatch` additionally names the effective `endpointType` and `spec.region`. |
| `False` | `ServiceError` | **Managed mode only.** Error create-or-updating the identity `Service` CR. |
| `False` | `EndpointError` | **Managed mode only.** Error create-or-updating the identity `Endpoint` CR. |
| `False` | `ImportError` | **External mode only.** Kubernetes-level error create-or-updating one of the unmanaged import CRs. |
| `False` | `CatalogEntryError` | **External mode only.** Kubernetes-level error create-or-updating (or garbage-collecting) an opt-in managed catalog entry. |

### ServiceAccountsReady

Set by `reconcileServiceAccounts` (gated on `AdminCredentialReady` and on the
OpenBao-backed `ClusterSecretStore`). It projects each
[`serviceAccounts`](#serviceaccountspec) entry onto a managed K-ORC `User` and
`Project`, generates the password, round-trips it through OpenBao, and gates each
account's readiness on the materialized Secret carrying the current-generation
password. Mode-independent: the same rules apply against a managed and an
external Keystone.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `NoServiceAccountsDeclared` | `spec.korc.serviceAccounts` is empty. Still `True` so the condition schema is identical whether or not accounts are declared. |
| `True` | `ServiceAccountsProvisioned` | Every declared account's `User`, `Project`, and materialized password Secret are converged for the current generation. |
| `False` | `WaitingForAdminCredential` | `AdminCredentialReady` is not `True`; projection deferred. |
| `False` | `SecretStoreNotReady` | The store selected by `spec.secretStoreRef` (a ClusterSecretStore or a namespaced SecretStore) is not Ready. |
| `False` | `ProbingForCollision` | A fail-loudly collision probe (see [adopt semantics](#serviceaccountspec)) has not yet resolved either way. |
| `False` | `ServiceAccountCollision` | A declared user (or a `project.create: true` project) already exists in Keystone and `adopt`/`project.create: false` was not set — the operator fails loud rather than take over an account it did not create. The message names the account and both remediations. |
| `False` | `WaitingForServiceAccounts` | The `User`/`Project`/password round-trip is converging, or an undeclared child is still being removed. |
| `False` | `ServiceAccountsFailed` | A service-account child reports a terminal K-ORC error. |
| `False` | `ServiceAccountError` | Kubernetes-level error reconciling (or pruning) a service-account child. |
| `False` | `AuthenticationFailed` \| `EndpointUnreachable` \| `TLSVerificationFailed` \| `CatalogEndpointMismatch` \| `CredentialDrift` | **External mode only.** A pending child carries a K-ORC message identifying one of these failure classes (see [`KORCReady`](#korcready)). |

### Ready (aggregate)

Set by `setReadyCondition`.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `AllReady` | All eight sub-conditions above are `True`. |
| `False` | `NotAllReady` | One or more sub-conditions are not `True`. |

---

## Service Namespaces

By default every service a ControlPlane projects lands in the **ControlPlane's
own namespace**: namespace and ControlPlane are the same boundary, so no
network-policy, RBAC, or quota line can be drawn between the services of one
control plane. A `namespace` assignment on `services.keystone` or
`services.horizon` makes the target namespace a **per-service choice** — a
service can be placed in a namespace of its own, and the backing services, secret
store, and credential material that belong to it follow it there. A service
without an assignment stays in the ControlPlane's namespace exactly as before.

### ServiceNamespaceSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` | Yes | — | The namespace the service is placed in. Must be an RFC-1123 label (`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, ≤ 63 chars) and must **differ** from the ControlPlane's own namespace — omit the whole block to keep the service there. |
| `lifecycle` | `string` (`Managed` \| `External`) | No | `Managed` | Who owns the namespace's lifecycle. Defaulted to `Managed` by both the `+kubebuilder:default` marker and the defaulting webhook. |

### Lifecycles

The two lifecycles are deliberately asymmetric — they differ on who owns the
namespace, both when the ControlPlane is created and when it is deleted:

| Lifecycle | On reconcile | On ControlPlane deletion |
| --- | --- | --- |
| `Managed` | The operator **creates** the namespace and stamps it with the ownership labels plus `app.kubernetes.io/managed-by: c5c3-operator`. A namespace that already exists **without** those labels is never adopted — the operator fails loud with `NamespacesReady=False/NamespaceNotOwned` rather than taking over a namespace it did not create. | The operator **deletes** the namespace (only if it carries the ownership labels), which cascades everything left in it. |
| `External` | The operator only **verifies** the namespace exists; it never creates, labels, or mutates it. A missing one parks on `NamespacesReady=False/NamespaceNotFound` and requeues. | The namespace **survives**. The residue the ControlPlane placed in it — backing services, credential material, tenant store — is swept by name, but the namespace itself is left standing. |

Use `External` for namespaces whose quotas, RBAC, and policies are provisioned
out-of-band, and `Managed` for a namespace the operator should own end to end.

### Backing services follow the service

Each namespace that hosts at least one service of the ControlPlane gets its **own
set of backing-service instances** (database, cache) materialized from the shared
`spec.infrastructure` block. Services co-located in one namespace share that
namespace's instances (with the same per-service logical databases, users, and
cache isolation used within a single namespace); services placed apart each get
their own. Within a namespace a service can still opt into dedicated instances
via [`dedicatedBackingServices`](#dedicatedbackingservices) — that opt-in simply
follows its service into the assigned namespace.

### Ownership and garbage collection

Kubernetes garbage collection only cascades **within** a namespace, so a
controller owner reference cannot cross one — the API server rejects it. A child
the ControlPlane places in a service namespace therefore carries no owner
reference; it is stamped with two **ownership labels** instead —
`c5c3.io/controlplane-name` and `c5c3.io/controlplane-namespace`, which together
name the owning ControlPlane — and the [ORC-teardown
finalizer](./controlplane-reconciler.md#owner-ref--gc-model) deletes it
explicitly, because nothing else collects it. The finalizer deletes
the service children first and waits for them (their own operators run a
sequenced ESO cleanup through the tenant store in the same namespace), then takes
the namespace down per its lifecycle.

### Secret distribution

An ESO `SecretStore` and the Secrets it materializes are namespace-local, so a
store in the ControlPlane's namespace cannot deliver anything into a service
namespace. Every namespace the ControlPlane occupies therefore gets its own
per-tenant `openbao-tenant-store` (its own ServiceAccount, client certificate,
and store object), and `ESOTenantStoreReady` gates on **all** of them. This needs
no OpenBao-side change: the `eso-tenant` role binds the ServiceAccount name in any
namespace and its templated policy scopes every path to the caller's own
namespace.

The credential material follows the Keystone service, and its OpenBao paths are
re-keyed on the Keystone service namespace:

- the admin-password seed path is
  `bootstrap/<keystone-namespace>/<controlplane>-keystone/admin` — the same path
  the keystone-operator's rotation `PushSecret` writes to (both follow the
  Keystone child), and the path
  [`write-bootstrap-secrets.sh`](../infrastructure/infrastructure-manifests.md)
  seeds (its `KORC_CONTROLPLANES` entries accept an optional
  `<namespace>/<controlplane>/<keystone-namespace>` third segment);
- the database-engine role is `keystone-<keystone-namespace>`, which
  `setup-database-tenant.sh` provisions by resolving the service namespace from
  the live ControlPlane spec.

A **brownfield** or **External** admin-password Secret is the user's, supplied
against the ControlPlane they wrote, so it stays in the ControlPlane's own
namespace where they put it.

### Cross-namespace service discovery

A service placed in one namespace still reaches the identity service in another
through the **namespace-qualified Service DNS**:
`http://<controlplane>-keystone.<keystone-namespace>.svc:5000/v3`. ClusterIP
Service DNS resolves across namespaces unchanged, so K-ORC's `clouds.yaml`
`auth_url` and the dashboard's `spec.keystoneEndpoint` reach a Keystone placed
apart with no extra wiring. What does **not** come for free is reachability —
namespaces are where NetworkPolicy is attached, so a default-deny namespace must
explicitly allow this flow (see below).

### Network policies

The operator creates **no NetworkPolicies** — it never has, in either the
single-namespace or the split-namespace case. Splitting a control plane across
namespaces is precisely what makes drawing them possible, so a platform operator
who wants a default-deny posture writes them per namespace. The cross-namespace
flows one ControlPlane needs are:

| From | To | Port | Purpose |
| --- | --- | --- | --- |
| the Horizon namespace | the Keystone namespace | `5000` | the dashboard authenticates every login against Keystone |
| the ControlPlane's namespace (K-ORC) | the Keystone namespace | `5000` | K-ORC reconciles the identity catalog and the admin credential |
| each service's namespace | its own database | `3306` | the service's DB connection |
| each service's namespace | its own cache | `11211` | the service's cache connection |
| a gateway namespace | the exposed service's namespace | the service port | external ingress via Gateway API |

### Uniqueness and immutability

A namespace is the **tenant key** the whole secret stack is scoped by (the
OpenBao KV paths, the `keystone-<namespace>` database-engine role, the templated
`eso-tenant` policy), so it belongs to **at most one** ControlPlane. Admission
rejects an assignment that names a namespace another ControlPlane already occupies
(its own or one of its service namespaces), and vice versa — the same rule
[`validateUniqueInNamespace`](#validation-rules) already enforces for a
ControlPlane's own namespace, one level out. An `External`-lifecycle namespace
that also hosts unrelated third-party workloads shares that namespace's OpenBao
path scope by design, so pick a dedicated namespace when that scope matters.

The assignment is **create-only**: the block's presence, its `name`, and its
`lifecycle` are all frozen after creation (webhook-only, no CEL transition rule,
so a later gated migration can relax it). Moving a live service across namespaces
would leave its backing services, its secret store, and every OpenBao path scoped
to the old namespace behind with no migration path — remove and recreate the
ControlPlane to change it. Two services co-located in one namespace must also
agree on its lifecycle: they share that namespace's backing services and tenant
store, so one must not have the teardown delete what the other declared
untouchable.

> **Chart mode:** the Helm chart's namespace-scoped RBAC mode
> (`rbac.namespaceScoped: true`) does **not** support dedicated service
> namespaces — the operator needs cluster-scoped `namespaces` (`create`,
> `delete`) and cross-namespace child access, which only the default ClusterRole
> mode grants.

---

## Child Namespace

> **DECISION:** the **ControlPlane-scoped** children the reconciler projects —
> the ones that belong to the ControlPlane as a whole rather than to one service:
> the K-ORC `ApplicationCredential` / `Service` / `Endpoint` / `User` / `Domain`
> CRs, the `clouds.yaml` Secret, the OpenBao `PushSecret`, and the
> service-account material — are created in the **ControlPlane's own namespace**
> (`childNamespace = cp.Namespace`), owned by it through a controller owner
> reference so the GC cascade reaps them.

A **service** and the things that follow it — its `MariaDB`, `Memcached`, and
`Keystone`/`Horizon` CRs, its tenant store, its credential material — are placed
in `cp.KeystoneNamespace()` / `cp.HorizonNamespace()`, the service's own
namespace when [`services.<svc>.namespace`](#service-namespaces) assigns one and
the ControlPlane's namespace otherwise. Only the latter can carry an owner
reference; a child in a different namespace carries the ownership labels instead
(see [Ownership and garbage collection](#ownership-and-garbage-collection)).

The rationale is garbage collection: `controllerutil.SetControllerReference`
rejects cross-namespace owner references because Kubernetes GC only cascades
within a single namespace. A child in `openstack` owned by a ControlPlane in
`default` would fail admission and, even if forced, would never be GC'd.
Co-locating a same-namespace child with its owner keeps the owner reference valid
and the GC cascade intact; a cross-namespace child is instead cleaned up by the
finalizer. In production a ControlPlane without any namespace assignments is
deployed into the `openstack` control-plane namespace, so its projected children
land in `openstack` exactly as expected — the namespace is **derived from the
owner (or the assignment)** rather than assumed. Projected child names are
deterministic and derived from the ControlPlane name (e.g. `{name}-keystone`,
`{name}-admin-app-credential`, `{name}-identity-service`,
`{name}-identity-endpoint`, `{name}-service-account-{account}-role-{slug}` (an
unmanaged `Role` import) and `{name}-service-account-{account}-assign-{slug}` (a
managed `RoleAssignment`)) so a single namespace can host the children of
multiple ControlPlanes without clashing.

---

## Related References

- [ControlPlane Reconciler](./controlplane-reconciler.md) — the reconciliation
  flow, sub-reconciler ordering, and gating semantics.
- [Keystone CRD](../keystone/keystone-crd.md) — the shared `commonv1` types and
  the reference operator the c5c3 aggregate projects into.
- [Keystone Reconciler](../keystone/keystone-reconciler.md) — the per-service
  reconciler that owns the projected Keystone CR.
- [Kubernetes Packages](../backend/kubernetes-packages.md) — operator image and
  Helm-chart packaging.
- [Infrastructure Manifests](../infrastructure/infrastructure-manifests.md) —
  the GitOps stack that deploys the c5c3-operator, K-ORC, and the backing
  services.
