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
| `infrastructure` | [`InfrastructureSpec`](#infrastructurespec) | Yes | — | Shared backing services (database, cache) the control plane's services connect to. |
| `services` | [`ServicesSpec`](#servicesspec) | Yes | — | Per-service configuration projected into the individual service CRs. |
| `global` | [`*commonv1.PolicySpec`](../keystone/keystone-crd.md#policyspec) | No | `nil` | oslo.policy overrides applied across every service in the control plane. Per-service overrides (e.g. `services.keystone.policyOverrides`) take precedence over these global rules when both are set. |
| `korc` | [`KORCSpec`](#korcspec) | Yes | — | K-ORC integration used to bootstrap and rotate the admin application credential and any declared bootstrap resources. |

---

## InfrastructureSpec

Declares the shared backing services for the control plane. Both
fields reuse the canonical `commonv1` shapes so the ControlPlane and the
per-service CRs validate the database/cache the same way.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `database` | [`commonv1.DatabaseSpec`](../keystone/keystone-crd.md#databasespec) | Yes | — | MariaDB connection parameters shared by the control plane. Supports managed (`clusterRef`) and brownfield (`host`) modes; exactly one must be set (enforced by the validating webhook — see [Validation Rules](#validation-rules)). |
| `cache` | [`commonv1.CacheSpec`](../keystone/keystone-crd.md#cachespec) | Yes | — | Memcached configuration shared by the control plane. Supports managed (`clusterRef`) and brownfield (`servers`) modes; exactly one must be set (enforced by the validating webhook). |

In managed mode the reconciler provisions an owned `MariaDB` CR (named after
`database.clusterRef.name`) and an owned `Memcached` CR (named after
`cache.clusterRef.name`) in the ControlPlane's own namespace. The Keystone CR
the reconciler projects points at the **same** `DatabaseSpec` / `CacheSpec`
verbatim, so the aggregate and the projected service agree on the backing
services.

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
> module. Fields not present below (replica strategy, uWSGI, gateway, network
> policy, fernet key count, etc.) are governed by the Keystone operator's own
> defaults on the projected CR, not by the ControlPlane.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `replicas` | `*int32` | No | `nil` (Keystone operator default, 3) | Overrides the number of Keystone API replicas. When `nil`, the reconciler leaves `replicas` unset on the projected Keystone CR, so the Keystone operator applies its own default. Minimum: 1. |
| `image` | [`*commonv1.ImageSpec`](../keystone/keystone-crd.md#imagespec) | No | `nil` | Overrides the Keystone container image. When `nil`, the reconciler derives the image as `ghcr.io/c5c3/keystone:{spec.openStackRelease}`. When set, the whole image reference is used verbatim. |
| `policyOverrides` | [`*commonv1.PolicySpec`](../keystone/keystone-crd.md#policyspec) | No | `nil` | Per-service oslo.policy overrides for Keystone. When set, these take precedence over `spec.global` for the Keystone service. |
| `rotationInterval` | `*metav1.Duration` | No | `nil` | Overrides the Fernet / credential-key rotation interval the reconciler derives for the projected Keystone CR. When `nil`, the reconciler derives a default schedule. When set, the duration is converted to a cron expression and applied to both `fernet.rotationSchedule` and `credentialKeys.rotationSchedule` on the projected Keystone CR; an unconvertible interval surfaces `KeystoneReady=False` with reason `InvalidRotationInterval`. |

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
| `passwordSecretRef` | [`commonv1.SecretRefSpec`](../keystone/keystone-crd.md#secretrefspec) | Yes | — | References the Secret holding the admin password used to (re-)mint the application credential. The `name` is required (enforced by the validating webhook — see [Validation Rules](#validation-rules)); a missing key defaults to `"password"` in the reconciler. This Secret is also projected into the Keystone CR's `bootstrap.adminPasswordSecretRef` so Keystone and K-ORC agree on the admin password source. |
| `applicationCredential` | [`ApplicationCredentialSpec`](#applicationcredentialspec) | Yes | — | Policy for the K-ORC admin application credential (restriction, access rules, rotation mode). |
| `bootstrapResources` | [`[]BootstrapResourceSpec`](#bootstrapresourcespec) | No | `nil` | OpenStack resources K-ORC bootstraps alongside the admin credential (e.g. the projects/roles a fresh control plane needs). The element shape is intentionally minimal at L1; the reconciler interprets it. |

---

## CloudCredentialsRef

References the `clouds.yaml` Secret and the cloud entry within it that K-ORC
authenticates as.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `cloudName` | `string` | Yes | — | The entry in `clouds.yaml` K-ORC authenticates as. Also used by the reconciler as the conventional K-ORC `User` reference name (defaulting to `admin` when empty) and projected onto the catalog `Service`/`Endpoint` CRs. |
| `secretName` | `string` | No | `"k-orc-clouds-yaml"` | Name of the Secret holding the `clouds.yaml` document. Defaulted to `k-orc-clouds-yaml` by **both** the `+kubebuilder:default` marker and the defaulting webhook. |

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
| `updatePhase` | [`UpdatePhase`](#updatephase) | Current phase of a control-plane release update. Empty / `Idle` outside an update. |
| `services` | `map[string]ServiceStatus` | Per-service readiness of the projected service CRs, keyed by service name (e.g. `"keystone"`). See [ServiceStatus](#servicestatus). |
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

> **Important — no CEL `x-kubernetes-validations` on this CRD.** Unlike the
> Keystone CRD, the c5c3 ControlPlane CRD does **not** carry CEL `XValidation`
> rules for the database/cache mutual-exclusivity or for the required
> `passwordSecretRef.name`. Those three invariants are enforced **only by the
> validating webhook**. A cluster that disables or bypasses the webhook (e.g.
> envtest without the webhook wired up, or a direct etcd write) will therefore
> not reject a malformed `ControlPlane` on those three rules — only the
> pattern/enum/minimum markers below remain active in that posture. This is the
> deliberate L1 surface; the markers and webhook together are defense-in-depth
> for the fields that **can** be expressed at both layers.

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

---

## Webhooks

The `ControlPlaneWebhook` struct implements both defaulting and validating
admission webhooks for the `ControlPlane` CRD via the typed-generic
`admission.Defaulter[*ControlPlane]` and `admission.Validator[*ControlPlane]`
interfaces from controller-runtime. `CredentialRotation` and `SecretAggregate`
have no webhook at this level.

The struct carries a `Client client.Reader`, injected at startup for any future
cluster-scoped lookups; it is currently unused by `validate()` but kept to
mirror the `KeystoneWebhook` shape and avoid a signature change later.

### Registration

```go
func (w *ControlPlaneWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error
```

Registers both webhooks with the manager using
`builder.WebhookManagedBy[*ControlPlane]`. The generated webhook paths are
`/mutate-c5c3-io-v1alpha1-controlplane` (mutating) and
`/validate-c5c3-io-v1alpha1-controlplane` (validating); both use
`failurePolicy=fail`, `sideEffects=None`, and `admissionReviewVersions=v1`. The
mutating webhook fires on `create`/`update`; the validating webhook on
`create`/`update`/`delete`.

### Defaulting Webhook

```go
func (w *ControlPlaneWebhook) Default(_ context.Context, obj *ControlPlane) error
```

Fills only zero-valued fields with their documented defaults, leaving any
explicit value untouched. It is **idempotent**: applying it twice produces the
same result. Each default is also expressed as a `+kubebuilder:default` marker
on the corresponding spec field, so the two layers agree — the markers cover the
normal admission path; the webhook covers callers that bypass the CRD default.
The defaulting constants `DefaultRegion` (`"RegionOne"`) and
`DefaultCloudCredentialsSecretName` (`"k-orc-clouds-yaml"`) are the single source
of truth shared with the markers' documented values.

| Field | Condition | Default Value |
| --- | --- | --- |
| `spec.region` | `== ""` | `"RegionOne"` |
| `spec.korc.adminCredential.cloudCredentialsRef.secretName` | `== ""` | `"k-orc-clouds-yaml"` |
| `spec.korc.adminCredential.applicationCredential.restricted` | `== nil` | `true` (pointer set to `true`; an explicit `false` is preserved) |
| `spec.korc.adminCredential.applicationCredential.rotation.mode` | `== ""` | `PasswordDriven` |

### Validating Webhook

```go
func (w *ControlPlaneWebhook) ValidateCreate(_ context.Context, obj *ControlPlane) (admission.Warnings, error)
func (w *ControlPlaneWebhook) ValidateUpdate(_ context.Context, _, newObj *ControlPlane) (admission.Warnings, error)
func (w *ControlPlaneWebhook) ValidateDelete(_ context.Context, _ *ControlPlane) (admission.Warnings, error)
```

- `ValidateCreate` and `ValidateUpdate` both delegate to the internal
  `validate()` method (see [Validating-webhook rules](#validating-webhook-rules)).
  There are no create-specific or update-specific rules — `ValidateUpdate`
  validates the new object only.
- `ValidateDelete` always returns `nil, nil` — **deletion is unconditionally
  allowed**.

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

`Ready` is `True` (reason `AllReady`) **only** when all five sub-conditions are
`True` (via `conditions.AllTrue`); otherwise it is `False` (reason
`NotAllReady`). For the full flow, see the
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
| `False` | `KORCCRDNotInstalled` | The K-ORC `ApplicationCredential` CRD is not installed (no-match error); surfaced cleanly without crash-looping the operator. |
| `False` | `AdminPasswordError` | Non-missing error reading the admin password. |
| `False` | `ApplicationCredentialError` | Error create-or-updating the `ApplicationCredential` CR. |

### AdminCredentialReady

Set by `reconcileAdminCredential` (gated on `KORCReady` **and** the K-ORC
`clouds.yaml` ExternalSecret being Ready).

| Status | Reason | When |
| --- | --- | --- |
| `True` | `AdminCredentialReady` | The admin application credential is committed to the owned Secret and mirrored to OpenBao. |
| `False` | `WaitingForKORC` | `KORCReady` is not `True`; credential push deferred. |
| `False` | `WaitingForCloudsYaml` | The `k-orc-clouds-yaml` ExternalSecret in the control-plane namespace (co-located with the K-ORC CRs per C1) is not yet Ready. |
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
| `False` | `KORCCRDNotInstalled` | The K-ORC `Service`/`Endpoint` CRD is not installed (no-match error). |
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
