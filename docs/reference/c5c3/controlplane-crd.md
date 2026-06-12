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
    keystone:
      ready: true
      release: "2025.2"
  adminApplicationCredential:
    id: 6f3c…
    restricted: true
    lastRotation: "2026-06-02T00:00:00Z"
  catalogReady: true
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
| `openStackRelease` | `string` | Yes | — | OpenStack release the control plane targets (e.g. `"2025.2"`). The reconciler (L2) projects this into each service CR's image tag. Must match the date-based release pattern `^\d{4}\.\d$`, enforced by both the CRD `+kubebuilder:validation:Pattern` marker and the validating webhook. |
| `region` | `string` | No | `"RegionOne"` | OpenStack region name applied across the control plane. Projected into the Keystone CR's `bootstrap.region`. Defaulted to `RegionOne` by **both** the `+kubebuilder:default` marker (normal admission) and the defaulting webhook (callers that bypass the CRD default). |
| `infrastructure` | [`InfrastructureSpec`](#infrastructurespec) | No | managed-mode defaulted | Shared backing services (database, cache) the control plane's services connect to. Optional — the defaulting webhook materializes a managed-mode `database`/`cache` when omitted; see [InfrastructureSpec](#infrastructurespec). |
| `services` | [`ServicesSpec`](#servicesspec) | Yes | — | Per-service configuration projected into the individual service CRs. |
| `global` | [`*commonv1.PolicySpec`](../keystone/keystone-crd.md#policyspec) | No | `nil` | oslo.policy overrides applied across every service in the control plane. Per-service overrides (e.g. `services.keystone.policyOverrides`) take precedence over these global rules when both are set. |
| `korc` | [`KORCSpec`](#korcspec) | No | defaulted | K-ORC integration used to bootstrap and rotate the admin application credential and any declared bootstrap resources. Optional — the defaulting webhook fills `adminCredential` (cloudCredentialsRef, passwordSecretRef, applicationCredential restriction/rotation) from well-known defaults when omitted. |

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
| `database` | [`commonv1.DatabaseSpec`](../keystone/keystone-crd.md#databasespec) | No | managed `clusterRef: openstack-db`, `database: keystone`, `secretRef.name: keystone-db` | MariaDB connection parameters shared by the control plane. Supports managed (`clusterRef`) and brownfield (`host`) modes; exactly one must hold **after defaulting** (enforced by the validating webhook — see [Validation Rules](#validation-rules)). Optional because the defaulting webhook materializes a managed-mode block when omitted. **`database.secretRef` ownership:** in managed mode this reference is **operator-owned** — `reconcileDBCredentials` materialises a per-ControlPlane DB-credential Secret and the reconciler overrides the projected Keystone CR's `spec.database.secretRef` to point at it, so the `keystone-db` default `secretRef.name` is only a managed-mode convenience name (it is **not** what Keystone consumes and no longer resolves to a cluster Secret). A **brownfield** ControlPlane (`database.host` set, no `clusterRef`) **MUST supply** its own `database.secretRef` Secret out-of-band — the operator projects no ExternalSecret in brownfield mode. See [managed-mode provisioning](#infrastructurespec) below. |
| `cache` | [`commonv1.CacheSpec`](../keystone/keystone-crd.md#cachespec) | No | managed `clusterRef: openstack-memcached`, `backend: dogpile.cache.pymemcache` | Memcached configuration shared by the control plane. Supports managed (`clusterRef`) and brownfield (`servers`) modes; exactly one must hold **after defaulting** (enforced by the validating webhook). Optional because the defaulting webhook materializes a managed-mode block when omitted. |

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
only Keystone is modeled; additional services are added as fields as the
operator grows.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `keystone` | [`ServiceKeystoneSpec`](#servicekeystonespec) | Yes | — | Configuration for the Keystone service projected by the reconciler. |

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

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `replicas` | `*int32` | No | `nil` (Keystone operator default, 3) | Overrides the number of Keystone API replicas. When `nil`, the reconciler leaves `replicas` unset on the projected Keystone CR, so the Keystone operator applies its own default. Minimum: 1. |
| `image` | [`*commonv1.ImageSpec`](../keystone/keystone-crd.md#imagespec) | No | `nil` | Overrides the Keystone container image. When `nil`, the reconciler derives the image as `ghcr.io/c5c3/keystone:{spec.openStackRelease}`. When set, the whole image reference is used verbatim. |
| `policyOverrides` | [`*commonv1.PolicySpec`](../keystone/keystone-crd.md#policyspec) | No | `nil` | Per-service oslo.policy overrides for Keystone. When set, these take precedence over `spec.global` for the Keystone service. |
| `rotationInterval` | `*metav1.Duration` | No | `nil` | Overrides the Fernet / credential-key rotation interval the reconciler derives for the projected Keystone CR. When `nil`, the reconciler derives a default schedule. When set, the duration is converted to a cron expression and applied to both `fernet.rotationSchedule` and `credentialKeys.rotationSchedule` on the projected Keystone CR. An unconvertible interval (not a positive whole number of days) is **rejected at admission** by the validating webhook; if the webhook is bypassed, the reconciler surfaces `KeystoneReady=False` with reason `InvalidRotationInterval` and returns the error so the reconcile chain stops and requeues with backoff. |
| `gateway` | [`*commonv1.GatewaySpec`](#gatewayspec) | No | `nil` | Exposes the projected Keystone API externally via a Gateway API HTTPRoute. When `nil`, no HTTPRoute is projected and the Keystone API is reachable in-cluster only (its ClusterIP Service). When set, the reconciler projects it onto the Keystone CR's `spec.gateway`, so the Keystone operator attaches an HTTPRoute to the referenced Gateway. When a `gateway` is set its `hostname` must be non-empty — enforced at admission by the validating webhook (see [Validation Rules](#validation-rules)). |
| `publicEndpoint` | `string` | No | `""` | Externally routable Keystone identity endpoint URL (e.g. `https://keystone.example.com/v3`). Projected into the Keystone bootstrap (`--bootstrap-public-url`) and used for the K-ORC identity catalog Endpoint, so external clients resolve the same URL Keystone advertises. When empty and `gateway` is set, the reconciler derives `https://{gateway.hostname}/v3` (the default-443 form); set it explicitly when the externally reachable port differs (e.g. a kind host-port mapping like `:8443`). |

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

---

## AdminCredentialSpec

Declares the admin OpenStack credential and the application-credential rotation
policy for the control plane.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `cloudCredentialsRef` | [`CloudCredentialsRef`](#cloudcredentialsref) | Yes | — | References the `clouds.yaml` Secret and cloud entry K-ORC authenticates as. |
| `passwordSecretRef` | [`commonv1.SecretRefSpec`](../keystone/keystone-crd.md#secretrefspec) | No | name `"keystone-admin"`, key `"password"` | References the Secret holding the admin password used to (re-)mint the application credential. The defaulting webhook materializes a missing `name` to `keystone-admin` and a missing `key` to `password`, so the block may be omitted on a minimal CR. The validating webhook still enforces `passwordSecretRef.name` non-empty as defense-in-depth (see [Validation Rules](#validation-rules)), but the defaulting webhook always satisfies it before validation runs, so a user may leave it unset. The reconciler's existing `"password"` key fallback also remains. **Mode-dependent use:** the `keystone-admin` default is the **brownfield / spec-level default**. In **brownfield mode** (`database.clusterRef == nil`) this field is used verbatim — the user supplies the admin-password Secret out-of-band and the operator projects no ExternalSecret, so this reference is projected onto the Keystone CR's `bootstrap.adminPasswordSecretRef` so Keystone and K-ORC agree on the admin password source. In **managed mode** (`database.clusterRef` set) the operator instead projects a per-ControlPlane admin ExternalSecret named `{controlplane.Name}-keystone-admin-credentials` (materialising the admin password from OpenBao) and **overrides** the projected Keystone CR's `bootstrap.adminPasswordSecretRef` to point at that operator-owned per-CP Secret's `password` key — the cp-level `passwordSecretRef` is **not** used as the child's ref in managed mode. See the [managed-mode admin-password provisioning](#admincredentialspec) note below. |
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
| `method` | `string` | Yes | — | HTTP method the rule allows (e.g. `"GET"`, `"POST"`). Projected onto the K-ORC typed `HTTPMethod` enum. |
| `path` | `string` | Yes | — | Request path the rule allows (e.g. `"/v2.1/servers"`). |

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
| `kind` | `string` | Yes | — | The K-ORC resource kind to bootstrap (e.g. `"Project"`, `"Role"`). |
| `name` | `string` | Yes | — | Name of the bootstrapped resource. |

---

## ControlPlaneStatus

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | Latest available observations of the control-plane state. Each condition carries an `observedGeneration`. See [Status Conditions](#status-conditions). |
| `observedGeneration` | `int64` | The `.metadata.generation` the controller last reconciled, so a stale status is distinguishable from a current one. |
| `updatePhase` | [`UpdatePhase`](#updatephase) | Current phase of a control-plane release update. Written on every status update; fixed at `Idle` in the current implementation because the release-update state machine is reserved (the other `UpdatePhase` values are not yet set). |
| `services` | `map[string]ServiceStatus` | Per-service readiness of the projected service CRs, keyed by service name. Written on every status update with a `"keystone"` entry whose `ready` mirrors the `KeystoneReady` condition and whose `release` is `spec.openStackRelease`. See [ServiceStatus](#servicestatus). |
| `adminApplicationCredential` | [`*AdminApplicationCredentialStatus`](#adminapplicationcredentialstatus) | Observed state of the K-ORC admin application credential. |
| `catalogReady` | `bool` | Whether the OpenStack service catalog has been observed as fully populated for the control plane. Flipped `true` by the catalog sub-reconciler once the identity `Service` and `Endpoint` are registered. |

### ServiceStatus

Reports the observed readiness of a single projected service CR.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
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
| `bootstrap` | `bool` | No | `false` | When `true`, requests an initial **mint** of the credential rather than a rotation of an existing one. Idempotent: if the credential already exists it is a no-op. |
| `reMint` | `bool` | No | `false` | When `true`, forces the reconciler to discard the current credential and mint a fresh one even if the existing credential is still valid. |
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
(`+kubebuilder:validation:Enum=adminApplicationCredential`).

| Value | Meaning |
| --- | --- |
| `adminApplicationCredential` | Rotates the K-ORC admin application credential. The only target supported at this level. |

## CredentialRotationStatus

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | Latest available observations of the rotation state. Upserted via the shared conditions helper. |
| `observedGeneration` | `int64` | The `.metadata.generation` the controller last reconciled. |

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
| `PolicySpec` | `global`, `services.keystone.policyOverrides` | [Keystone CRD → PolicySpec](../keystone/keystone-crd.md#policyspec) |

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
> CEL `XValidation` rules for the policy-rule name/value constraints inherited
> from the shared `commonv1.PolicySpec` type — they apply wherever a `PolicySpec`
> is used (`spec.global` and `spec.services.keystone.policyOverrides`). The
> database/cache mutual-exclusivity and the required `passwordSecretRef.name`
> remain enforced **only by the validating webhook**: a cluster that disables or
> bypasses the webhook (e.g. envtest without the webhook wired up, or a direct
> etcd write) will not reject a malformed `ControlPlane` on those three rules,
> only on the policy-rule CEL and the pattern/enum/minimum markers below. The
> markers and webhook together are defense-in-depth for the fields that **can**
> be expressed at both layers.

### CRD schema markers (API-server enforced)

| Field | Rule |
| --- | --- |
| `spec.openStackRelease` | Pattern `^\d{4}\.\d$` |
| `spec.korc.adminCredential.applicationCredential.rotation.mode` | Enum: `PasswordDriven`, `Scheduled`, `Manual` |
| `spec.services.keystone.replicas` | Minimum: 1 |
| `CredentialRotation spec.target` | Enum: `adminApplicationCredential` |
| `CredentialRotation spec.intervalDays` | Minimum: 1 |
| `CredentialRotation spec.preRotationDays` | Minimum: 0 |
| `CredentialRotation spec.gracePeriodDays` | Minimum: 0 |
| `status.updatePhase` | Enum: `Idle`, `Updating`, `UpdatingServices`, `Verifying`, `RollingBack` |
| `spec.global`, `spec.services.keystone.policyOverrides` (CEL) | `!has(self.rules) \|\| self.rules.all(k, size(k) > 0)` → "policy rule name must not be empty" |
| `spec.global`, `spec.services.keystone.policyOverrides` (CEL) | `!has(self.rules) \|\| self.rules.all(k, size(self.rules[k]) > 0)` → "policy rule value must not be empty" |

### Validating-webhook rules

The `validate()` method accumulates all errors in a `field.ErrorList` and
returns a single `apierrors.NewInvalid` error keyed on
`GroupKind{Group: "c5c3.io", Kind: "ControlPlane"}`. It does **not**
short-circuit on the first error.

| Rule | Field Path | Error Type | Condition |
| --- | --- | --- | --- |
| Release pattern | `spec.openStackRelease` | `field.Invalid` | Value does not match `^\d{4}\.\d$`. Defense-in-depth alongside the CRD `+kubebuilder:validation:Pattern` marker. |
| Database mutual exclusivity | `spec.infrastructure.database` | `field.Invalid` | Both `clusterRef` and `host` set, or neither (`(clusterRef != nil) == (host != "")`). **Webhook-only** — there is no CEL rule for this on the c5c3 CRD. |
| Cache mutual exclusivity | `spec.infrastructure.cache` | `field.Invalid` | Both `clusterRef` and `servers` set, or neither (`(clusterRef != nil) == (len(servers) > 0)`). **Webhook-only**. |
| Admin password Secret required | `spec.korc.adminCredential.passwordSecretRef.name` | `field.Required` | `name` is empty — without it the reconciler cannot (re-)mint the admin application credential. **Webhook-only**. |
| Gateway hostname required | `spec.services.keystone.gateway.hostname` | `field.Required` | A `gateway` is configured but its `hostname` is empty. Mirrors the `+kubebuilder:validation:MinLength=1` marker on `commonv1.GatewaySpec.Hostname`; without it the reconciler derives an empty `https:///v3` public endpoint. |

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
clouds.yaml ExternalSecret.

| Rule | Field Path | Condition |
| --- | --- | --- |
| Database mode immutable | `spec.infrastructure.database` | `clusterRef` nil-ness changed (managed ↔ brownfield) |
| Database clusterRef.name immutable | `spec.infrastructure.database.clusterRef.name` | Both managed, but the name changed |
| Cache mode immutable | `spec.infrastructure.cache` | `clusterRef` nil-ness changed (managed ↔ brownfield) |
| Cache clusterRef.name immutable | `spec.infrastructure.cache.clusterRef.name` | Both managed, but the name changed |
| Cloud secretName immutable | `spec.korc.adminCredential.cloudCredentialsRef.secretName` | The value changed |

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
  `applicationCredential.rotation.mode`, and
  `cloudCredentialsRef.cloudName`.
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
> `WaitingForCloudsYaml` — never a silent authentication.
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

The ControlPlane status is driven by five sub-reconcilers, each owning one
condition type, plus an aggregate `Ready` condition. The condition-type
constants in `controlplane_controller.go` are the single source of truth; call
sites reference the constants rather than inline literals.

The sub-reconcilers run in dependency order, each **gated** on the previous
one's condition being `True`:

```
InfrastructureReady → KeystoneReady → KORCReady → AdminCredentialReady → CatalogReady
```

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

### KeystoneReady

Set by `reconcileKeystone` (gated on `InfrastructureReady`).

| Status | Reason | When |
| --- | --- | --- |
| `True` | `KeystoneReady` | The projected Keystone CR reports Ready. |
| `False` | `WaitingForInfrastructure` | `InfrastructureReady` is not `True`; Keystone projection deferred. |
| `False` | `WaitingForKeystone` | The Keystone CR is ensured but not yet Ready. |
| `False` | `InvalidRotationInterval` | `services.keystone.rotationInterval` could not be converted to a cron schedule. |
| `False` | `KeystoneError` | Error create-or-updating the Keystone CR. |

### KORCReady

Set by `reconcileKORC`.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `ApplicationCredentialMinted` | The K-ORC admin `ApplicationCredential` is minted and reports `Available=True`. |
| `False` | `WaitingForAdminPassword` | The admin password Secret/key is not yet available; minting deferred. |
| `False` | `WaitingForApplicationCredential` | The `ApplicationCredential` CR is ensured but not yet `Available`. |
| `False` | `AdminPasswordError` | Non-missing error reading the admin password. |
| `False` | `ApplicationCredentialError` | Error create-or-updating the `ApplicationCredential` CR. |

### AdminCredentialReady

Set by `reconcileAdminCredential` (gated on `KORCReady`, the OpenBao-backed
`ClusterSecretStore` being Ready, **and** the K-ORC `clouds.yaml` ExternalSecret
being Ready).

| Status | Reason | When |
| --- | --- | --- |
| `True` | `AdminCredentialReady` | The admin application credential is committed to the owned Secret and mirrored to OpenBao. |
| `False` | `WaitingForKORC` | `KORCReady` is not `True`; credential push deferred. |
| `False` | `SecretStoreNotReady` | The OpenBao-backed `ClusterSecretStore` is not Ready; the secret backend is unreachable. |
| `False` | `WaitingForCloudsYaml` | The operator-created per-ControlPlane `k-orc-clouds-yaml` ExternalSecret in the control-plane namespace (co-located with the K-ORC CRs per C1; created and owned by `reconcileKORC`) is not yet Ready. |
| `False` | `CloudsYamlError` | Error checking the `clouds.yaml` ExternalSecret. |
| `False` | `SecretError` | Error ensuring the operator-owned application-credential Secret. |
| `False` | `PushSecretError` | Error ensuring the OpenBao PushSecret. |

### CatalogReady

Set by `reconcileCatalog` (gated on `AdminCredentialReady`).
Also flips `status.catalogReady` to `true`.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `CatalogRegistered` | The Keystone identity `Service` and public `Endpoint` are registered as K-ORC CRs. |
| `False` | `WaitingForAdminCredential` | `AdminCredentialReady` is not `True`; catalog registration deferred. |
| `False` | `ServiceError` | Error create-or-updating the identity `Service` CR. |
| `False` | `EndpointError` | Error create-or-updating the identity `Endpoint` CR. |

### Ready (aggregate)

Set by `setReadyCondition`.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `AllReady` | All five sub-conditions above are `True`. |
| `False` | `NotAllReady` | One or more sub-conditions are not `True`. |

---

## Child Namespace

> **DECISION:** every child the reconciler projects — the `MariaDB`,
> `Memcached`, and `Keystone` CRs, the K-ORC `ApplicationCredential` /
> `Service` / `Endpoint` CRs, the owned Secret, and the OpenBao `PushSecret` —
> is created in the **ControlPlane's own namespace** (`childNamespace =
> cp.Namespace`), **not** a hardcoded `"openstack"` literal.

The rationale is garbage collection: `controllerutil.SetControllerReference`
rejects cross-namespace owner references because Kubernetes GC only cascades
within a single namespace. A child in `openstack` owned by a ControlPlane in
`default` would fail admission and, even if forced, would never be GC'd.
Co-locating the children with their owner keeps the owner reference valid and the
GC cascade intact. In production the ControlPlane is deployed into the
`openstack` control-plane namespace, so the projected children land in
`openstack` exactly as expected — the namespace is now **derived from the
owner** rather than assumed. Projected child names are deterministic and derived
from the ControlPlane name (e.g. `{name}-keystone`,
`{name}-admin-app-credential`, `{name}-identity-service`,
`{name}-identity-endpoint`) so a single namespace can host the children of
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
