---
title: Kubernetes-Interacting Packages
quadrant: shared-library
---

# Kubernetes-Interacting Packages

Reference documentation for the `internal/common/` packages that interact with the
Kubernetes API server. These packages provide reconciler building blocks for
managing external operator CRDs (MariaDB, External Secrets Operator, cert-manager) and
core Kubernetes resources (Deployments, Services, Jobs, CronJobs, ConfigMaps).

All packages share these conventions:

- **Idempotent create-or-update via Server-Side Apply** — `Ensure*` functions apply the
  desired object through the generic `apply.EnsureObject` helper, which uses Server-Side
  Apply under the fixed field manager `forge-operator`. The field manager owns only the
  fields the builder sets, so server-defaulted fields the builder omits are never claimed
  or overwritten and a converged object is applied without a write. The apply is wrapped
  in `retry.RetryOnConflict`, so a benign field-manager conflict is absorbed as an internal
  retry rather than surfacing as condition noise.
- **Owner references** — `controllerutil.SetControllerReference` is called on every apply
  (not only on create) so the resource is garbage-collected when the owning CR is deleted
  and the reference is re-enforced if it drifts.
- **Readiness reporting** — Functions that create resources return `(bool, error)`, where
  `true` means the resource is ready and `false` means it exists but is not yet ready.
- **Error wrapping** — All errors include context via `fmt.Errorf` with `%w` for
  `errors.Is`/`errors.As` compatibility.

## Import Paths

| Package | Import Path |
| --- | --- |
| `apply` | `github.com/c5c3/forge/internal/common/apply` |
| `config` | `github.com/c5c3/forge/internal/common/config` |
| `database` | `github.com/c5c3/forge/internal/common/database` |
| `deployment` | `github.com/c5c3/forge/internal/common/deployment` |
| `job` | `github.com/c5c3/forge/internal/common/job` |
| `policy` | `github.com/c5c3/forge/internal/common/policy` |
| `secrets` | `github.com/c5c3/forge/internal/common/secrets` |
| `tls` | `github.com/c5c3/forge/internal/common/tls` |

## External CRD Dependencies

These packages import typed Go structs from external operator modules:

| Operator | Go Module | API Version | Types Used |
| --- | --- | --- | --- |
| mariadb-operator | `github.com/mariadb-operator/mariadb-operator` | `v1alpha1` | `Database`, `User`, `Grant` |
| External Secrets Operator | `github.com/external-secrets/external-secrets` | `v1` (ExternalSecret, ClusterSecretStore), `v1alpha1` (PushSecret) | `ExternalSecret`, `PushSecret` |
| cert-manager | `github.com/cert-manager/cert-manager` | `v1` | `Certificate` |

> **Note:** The PushSecret API is `v1alpha1` (unstable). Its schema may change in future
> ESO releases. Pin the ESO module version in `go.mod` to avoid breakage.

---

## Package: `apply`

Provides the generic Server-Side Apply create-or-update primitive that backs the
`Ensure*` family.

### EnsureObject

```go
func EnsureObject[T client.Object](
    ctx context.Context,
    c client.Client,
    scheme *runtime.Scheme,
    owner client.Object,
    obj T,
    fieldManager string,
) error
```

Creates or updates `obj` via Server-Side Apply under `fieldManager` and sets `owner` as
the controller reference.

**Behavior:**

- Sets the controller owner reference and stamps the object's GVK (objects built in-code
  carry an empty `TypeMeta`, which Server-Side Apply requires).
- Applies the object with `client.FieldOwner(fieldManager)` and `client.ForceOwnership`,
  wrapped in `retry.RetryOnConflict` so a benign field-manager conflict is retried
  internally rather than surfaced.
- The field manager owns only the fields the builder sets, so server-defaulted fields the
  builder omits are never claimed and a converged object is applied without a write.
- Decodes the server response back into `obj`, so callers may read fresh status (e.g.
  readiness) without an extra `Get`.

**Field manager:** the package exports `apply.FieldManager` (`"forge-operator"`), the
stable field-manager name shared by all `Ensure*` helpers.

---

## Package: `config`

Implements the INI configuration rendering pipeline for CobaltCore operators.
The `CreateImmutableConfigMap` function provides Kubernetes-interacting config
management.

### CreateImmutableConfigMap

```go
func CreateImmutableConfigMap(
    ctx context.Context,
    c client.Client,
    scheme *runtime.Scheme,
    owner client.Object,
    baseName, namespace string,
    data map[string]string,
) (string, error)
```

Creates an immutable ConfigMap with a content-hash suffix appended to the base name.
The hash ensures that configuration changes result in new ConfigMap names, triggering
pod restarts when the ConfigMap is referenced in a Deployment's volume spec.

**Parameters:**

| Name | Type | Description |
| --- | --- | --- |
| `ctx` | `context.Context` | Request context |
| `c` | `client.Client` | Kubernetes API client |
| `scheme` | `*runtime.Scheme` | Scheme for owner reference resolution |
| `owner` | `client.Object` | Owning CR for garbage collection |
| `baseName` | `string` | Base name for the ConfigMap (hash is appended as `-<hash>`) |
| `namespace` | `string` | Namespace for the ConfigMap |
| `data` | `map[string]string` | ConfigMap data entries |

**Returns:**

| Value | Description |
| --- | --- |
| `string` | Actual ConfigMap name including the 8-character SHA256 hash suffix |
| `error` | Non-nil on owner reference or API server failure |

**Behavior:**

- Computes a deterministic SHA256 hash from sorted `data` keys and values.
- Truncates the hash to 8 hex characters and appends it as `baseName-<hash>`.
- Sets `Immutable: true` on the ConfigMap.
- Sets a controller owner reference on the ConfigMap.
- If a ConfigMap with the same name already exists (`AlreadyExists` error), returns the
  name without error (idempotent).
- Same data always produces the same hash (deterministic).
- Different data always produces a different hash.

**Example:**

```go
name, err := config.CreateImmutableConfigMap(ctx, client, scheme, owner,
    "keystone-config", "openstack",
    map[string]string{"keystone.conf": renderedINI},
)
// name == "keystone-config-a1b2c3d4"
```

### PruneImmutableConfigMaps

```go
func PruneImmutableConfigMaps(
    ctx context.Context,
    c client.Client,
    owner client.Object,
    baseName, namespace, currentName string,
    retain int,
) error
```

Deletes stale immutable ConfigMaps that were previously created by
`CreateImmutableConfigMap`, retaining the newest `retain` historical ConfigMaps
(by `CreationTimestamp`) plus the currently active one identified by `currentName`.
This prevents unbounded accumulation of immutable ConfigMaps across reconcile
cycles.

**Parameters:**

| Name | Type | Description |
| --- | --- | --- |
| `ctx` | `context.Context` | Request context |
| `c` | `client.Client` | Kubernetes API client |
| `owner` | `client.Object` | Owning CR — only ConfigMaps with a controller owner reference matching this object's UID are considered |
| `baseName` | `string` | Base name prefix for candidate ConfigMaps (matches `baseName-*`) |
| `namespace` | `string` | Namespace to list ConfigMaps in |
| `currentName` | `string` | Name of the currently active ConfigMap (never deleted, even with `retain=0`) |
| `retain` | `int` | Number of historical ConfigMaps to keep beyond the current one |

**Returns:**

| Value | Description |
| --- | --- |
| `error` | Non-nil on list or delete failure; `nil` on success or when no pruning is needed |

**Algorithm:**

1. Lists ConfigMaps matching the `forge.c5c3.io/config-base` label in the namespace.
2. Filters to ConfigMaps matching the `baseName + "-"` prefix.
3. Excludes the ConfigMap named `currentName` (the active one).
4. Excludes ConfigMaps without a controller owner reference matching `owner.GetUID()`.
5. Sorts remaining candidates by `CreationTimestamp` descending (newest first).
6. If the number of candidates is less than or equal to `retain`, returns `nil` (no-op).
7. Deletes candidates from index `retain` onwards (oldest first).
8. Logs each deletion at info level for auditability.

**Idempotency and concurrency safety:**

- Uses `client.IgnoreNotFound()` on delete operations, so a ConfigMap deleted between
  the list and delete calls does not cause an error.
- Calling the function twice with the same state produces the same result.
- Does not use optimistic locking — concurrent reconcile goroutines may both attempt
  to delete the same ConfigMap, but `IgnoreNotFound` makes this safe.

**Filtering rules:**

| ConfigMap State | Included in Candidates? |
| --- | --- |
| Name matches `baseName-*` prefix, owned by `owner` | Yes |
| Name equals `currentName` | No (always excluded) |
| Name does not match `baseName-*` prefix | No |
| No owner reference | No |
| Owner reference UID does not match `owner` | No |

**Edge cases:**

| Scenario | Result |
| --- | --- |
| No historical ConfigMaps exist | No-op, returns `nil` |
| Fewer historical ConfigMaps than `retain` | No-op, returns `nil` |
| `retain=0` | All historical ConfigMaps deleted, only `currentName` survives |
| ConfigMap deleted between list and delete | `NotFound` silently ignored |
| Overlapping prefix (e.g., `test-config-` vs `test-config-extra-`) | Strict `baseName + "-"` prefix prevents false matches |
| Pre-existing ConfigMaps without `forge.c5c3.io/config-base` label | Not pruned — invisible to server-side selector. Bounded in number and GC'd on CR deletion via owner reference. |

**Example:**

```go
// After creating a new ConfigMap, prune old ones keeping 3 historical:
err := config.PruneImmutableConfigMaps(ctx, client, keystoneCR,
    "keystone-config", "openstack", "keystone-config-a1b2c3d4", 3,
)
// With 5 historical ConfigMaps, the 2 oldest are deleted, 3 newest + current remain.
```

---

## Package: `database`

Manages MariaDB database resources for CobaltCore operators. Uses typed structs from
`github.com/mariadb-operator/mariadb-operator/api/v1alpha1`.

### EnsureDatabase

```go
func EnsureDatabase(
    ctx context.Context,
    c client.Client,
    scheme *runtime.Scheme,
    owner client.Object,
    db *mariadbv1alpha1.Database,
) (bool, error)
```

Creates or updates a MariaDB Database CR.

**Returns:** `(true, nil)` when the Database has a `Ready` condition with status `True`;
`(false, nil)` when it exists but is not yet ready; `(false, error)` on failure.

**Behavior:**

- Applies the desired Database via `apply.EnsureObject` (Server-Side Apply); the field
  manager owns only the fields the builder sets, so the server default on
  `maxUserConnections` is preserved and a converged Database is not rewritten.
- Readiness is determined by the package-internal `isDatabaseReady` helper on the
  server-fresh apply response.

### EnsureDatabaseUser

```go
func EnsureDatabaseUser(
    ctx context.Context,
    c client.Client,
    scheme *runtime.Scheme,
    owner client.Object,
    user *mariadbv1alpha1.User,
    grant *mariadbv1alpha1.Grant,
) (bool, error)
```

Creates or updates a MariaDB User CR and a Grant CR in a single call.

**Returns:** `(true, nil)` when both User and Grant have `Ready` conditions with status
`True`; `(false, nil)` when either is not yet ready; `(false, error)` on failure.

**Behavior:**

- Processes User first, then Grant. If User creation/update fails, Grant is not attempted.
- Both resources receive controller owner references.
- Readiness of the User and Grant is determined by the package-internal
  `isUserReady` and `isGrantReady` helpers.

---

## Package: `deployment`

Manages Kubernetes Deployments and Services for CobaltCore operators.

### EnsureDeployment

```go
func EnsureDeployment(
    ctx context.Context,
    c client.Client,
    scheme *runtime.Scheme,
    owner client.Object,
    deploy *appsv1.Deployment,
) (bool, error)
```

Creates or updates a Deployment.

**Returns:** `(true, nil)` when all replicas are available; `(false, nil)` when the
Deployment exists but is not yet ready; `(false, error)` on failure.

**Behavior:**

- Applies the desired Deployment via `apply.EnsureObject` (Server-Side Apply); a converged
  Deployment is not rewritten on every reconcile.
- Retains a pre-apply `Get` only to delete-and-recreate on an immutable-selector change and
  to preserve the HPA-owned replica count when `spec.deployment.replicas` is left nil.
- Readiness is determined by `IsDeploymentReady` on the server-fresh apply response.

### EnsureService

```go
func EnsureService(
    ctx context.Context,
    c client.Client,
    scheme *runtime.Scheme,
    owner client.Object,
    svc *corev1.Service,
) error
```

Creates or updates a Service. Does not report readiness (Services are ready immediately).

**Behavior:**

- Applies the desired Service via `apply.EnsureObject` (Server-Side Apply). The builder
  leaves server-assigned fields (`ClusterIP`, `ClusterIPs`, `IPFamilies`, `NodePort`)
  unset, so the field manager never owns them and the API server keeps the values it
  assigned — no hand-rolled preservation is needed.
- Retains a pre-apply `Get` only to fail fast when the caller explicitly sets one of those
  immutable fields to a conflicting value.

### IsDeploymentReady

```go
func IsDeploymentReady(deploy *appsv1.Deployment) bool
```

Pure function. Returns `true` when both conditions are met:

1. `deploy.Status.ReadyReplicas >= *deploy.Spec.Replicas` (defaults to 1 if
   `Spec.Replicas` is nil).
2. The Deployment has an `Available` condition with status `True`.

**Edge cases:**

| Scenario | Result |
| --- | --- |
| `Spec.Replicas` is nil | Defaults to 1 |
| No `Available` condition | `false` |
| `ReadyReplicas < desired` | `false` |
| `ReadyReplicas >= desired` and `Available=True` | `true` |

---

## Package: `job`

Manages Kubernetes Jobs and CronJobs for CobaltCore operators.

### RunJob

```go
func RunJob(
    ctx context.Context,
    c client.Client,
    scheme *runtime.Scheme,
    owner client.Object,
    job *batchv1.Job,
) (bool, error)
```

Creates a Job if it does not already exist and reports completion status.

**Returns:** `(true, nil)` when the Job has a `Complete` condition with status `True`
and its re-run key is unchanged;
`(false, nil)` when the Job exists but is still running, or was deleted and
re-created because its re-run key changed;
`(false, error)` wrapping `ErrJobFailed` when the Job has permanently failed (e.g.
exceeded backoffLimit) and its re-run key is unchanged;
`(false, error)` on unexpected API failures.

**Behavior:**

- If the Job does not exist: creates it with a controller owner reference, returns
  `(false, nil)` (newly created Jobs are never immediately complete).
- If the Job already exists: checks completion via the package-internal
  `isJobComplete` helper and permanent failure via `isJobFailed`. A completed
  **or permanently failed** Job whose stored re-run key (the
  `forge.c5c3.io/pod-spec-hash` annotation) no longer matches the desired pod
  template is deleted (background propagation) and re-created — so a Job that
  failed under a since-fixed spec (new container image, corrected ConfigMap,
  rotated password) re-runs instead of wedging. A permanently failed Job whose
  re-run key is unchanged returns an error wrapping `ErrJobFailed` to prevent
  infinite requeue loops. Jobs are never updated in place.
- Reconcilers should call `RunJob` on each reconciliation loop. The function is
  idempotent: calling it when the Job already exists and is complete returns
  `(true, nil)` without side effects.

### EnsureCronJob

```go
func EnsureCronJob(
    ctx context.Context,
    c client.Client,
    scheme *runtime.Scheme,
    owner client.Object,
    cronJob *batchv1.CronJob,
) error
```

Creates or updates a CronJob with a controller owner reference.

**Behavior:**

- Applies the desired CronJob via `apply.EnsureObject` (Server-Side Apply); a converged
  CronJob is not rewritten, and spec changes take effect on the next scheduled run.

The Job completion and permanent-failure checks (`isJobComplete` / `isJobFailed`)
are package-internal helpers consumed by `RunJob`.

---

## Package: `policy`

Provides pure functions for OpenStack oslo.policy rule rendering, merging, and validation. The `LoadPolicyFromConfigMap` function reads policy from
Kubernetes ConfigMaps.

### LoadPolicyFromConfigMap

```go
func LoadPolicyFromConfigMap(
    ctx context.Context,
    c client.Client,
    key client.ObjectKey,
) (map[string]string, error)
```

Reads a ConfigMap by namespace/name and extracts the `policy.yaml` key as a
`map[string]string` of oslo.policy rules.

**Parameters:**

| Name | Type | Description |
| --- | --- | --- |
| `ctx` | `context.Context` | Request context |
| `c` | `client.Client` | Kubernetes API client |
| `key` | `client.ObjectKey` | Namespace and name of the ConfigMap |

**Returns:**

| Value | Description |
| --- | --- |
| `map[string]string` | Parsed policy rules (action → rule expression) |
| `error` | Non-nil when ConfigMap is missing, key is absent, or YAML is invalid |

**Error conditions:**

| Condition | Error |
| --- | --- |
| ConfigMap does not exist | Wrapped API server error (compatible with `apierrors.IsNotFound`) |
| `policy.yaml` key absent | `ConfigMap <key> does not contain key "policy.yaml"` |
| Invalid YAML content | `parsing policy.yaml from ConfigMap <key>: <parse error>` |

**Example:**

```go
rules, err := policy.LoadPolicyFromConfigMap(ctx, client,
    types.NamespacedName{Namespace: "openstack", Name: "keystone-policy"},
)
// rules == map[string]string{"identity:get_user": "role:admin", ...}
```

---

## Package: `secrets`

Manages External Secrets Operator resources and Kubernetes Secrets for CobaltCore
operators. Uses typed structs from `github.com/external-secrets/external-secrets`.

### WaitForExternalSecret

```go
func WaitForExternalSecret(
    ctx context.Context,
    c client.Client,
    key client.ObjectKey,
) (exists, ready bool, err error)
```

Checks whether the ExternalSecret identified by `key` exists and has a `Ready`
condition with status `True` (the ESO `ExternalSecretReady` condition type).

**Returns (tri-state):**

- `(false, false, nil)` — the ExternalSecret was not found.
- `(true, false, nil)` — it exists but has not synced yet (no `Ready=True`).
- `(true, true, nil)` — it exists and is Ready.
- `(false, false, err)` — an unexpected client failure.

**Behavior:**

- This is a point-in-time check, not a blocking wait. Reconcilers should call it on each
  reconciliation loop and requeue while `ready` is `false`.
- The separate `exists` return lets callers surface a clearer status —
  "ExternalSecret not found yet" versus "waiting for ESO to sync" — instead of
  collapsing both into a single not-ready signal.
- Uses the ESO `ExternalSecretReady` condition type constant, not a raw string.

### IsSecretReady

```go
func IsSecretReady(
    ctx context.Context,
    c client.Client,
    key client.ObjectKey,
    expectedKeys ...string,
) (bool, error)
```

Checks whether a Kubernetes Secret exists at the given key and, when
`expectedKeys` are provided, verifies that all specified keys are present in
the Secret's `.Data` field.

**Returns:** `(true, nil)` if the Secret exists and contains all expected keys;
`(false, nil)` if not found or missing expected keys;
`(false, error)` on unexpected API failures.

**Behavior:**

- A `NotFound` error is treated as a normal condition (`(false, nil)`), not a failure.
- When no `expectedKeys` are provided, only checks for Secret existence.
- When `expectedKeys` are provided, returns `(false, nil)` if any key is absent
  from `Secret.Data`.

### GetSecretValue

```go
func GetSecretValue(
    ctx context.Context,
    c client.Client,
    key client.ObjectKey,
    dataKey string,
) (string, error)
```

Retrieves and decodes the value of a specific data key from a Secret.

**Parameters:**

| Name | Type | Description |
| --- | --- | --- |
| `key` | `client.ObjectKey` | Namespace and name of the Secret |
| `dataKey` | `string` | Key within `Secret.Data` to retrieve |

**Returns:** The decoded string value, or an error if the Secret or key is not found.

**Error conditions:**

| Condition | Error |
| --- | --- |
| Secret does not exist | Wrapped API server error |
| Key not in `Secret.Data` | Wraps `ErrKeyNotFound`: `key not found in Secret: key "<dataKey>" in Secret <namespace>/<name>` — test with `errors.Is(err, secrets.ErrKeyNotFound)` |

### EnsurePushSecret

```go
func EnsurePushSecret(
    ctx context.Context,
    c client.Client,
    scheme *runtime.Scheme,
    owner client.Object,
    ps *esov1alpha1.PushSecret,
) error
```

Creates or updates a PushSecret CR with a controller owner reference.

**Behavior:**

- Applies the desired PushSecret via `apply.EnsureObject` (Server-Side Apply). The field
  manager owns only the fields the builder sets, so the server defaults on `updatePolicy`
  and `refreshInterval` are preserved and a converged PushSecret is not rewritten — ESO is
  not woken to re-push an unchanged credential.
- Uses the ESO `v1alpha1` PushSecret API (unstable — see [External CRD Dependencies](#external-crd-dependencies)).

### Per-CR secret store selection

These helpers back the optional per-CR `spec.secretStoreRef`: each CR routes its
ExternalSecrets and PushSecrets through the store it selects (a cluster-scoped
`ClusterSecretStore`, default `openbao-cluster-store`, or a namespaced
`SecretStore`), while the `IsClusterSecretStoreReady` /
`OpenBaoClusterStoreName` primitives above remain.

- `EffectiveStoreRef(ref *commonv1.SecretStoreRefSpec) commonv1.SecretStoreRefSpec` — resolves a nil or empty-kind ref to the shared cluster store `{ClusterSecretStore, openbao-cluster-store}`, so a CR that omits the field behaves as before.
- `IsStoreRefReady(ctx, c, ref commonv1.SecretStoreRefSpec, namespace string) (bool, error)` — reports store readiness, dispatching on kind: a cluster store is looked up by name, a namespaced store in `namespace`; an unknown kind returns an error.
- `IsSecretStoreReady(ctx, c, name, namespace string) (bool, error)` — the namespaced twin of `IsClusterSecretStoreReady`, checking a `SecretStore`'s `Ready` condition.
- `GateStoreReady(ctx, c, ref, namespace, conds *[]metav1.Condition, generation int64, conditionType string) (bool, error)` — store-ref-aware readiness gate; on a not-ready store it sets a `SecretStoreNotReady` condition whose message names the store kind and name (`"%s %q is not ready; upstream secret backend unreachable"`) and returns `(false, nil)`.
- `ESOSecretStoreRef(ref commonv1.SecretStoreRefSpec) esov1.SecretStoreRef` — builds the ESO ExternalSecret store ref from a resolved store reference.
- `PushSecretStoreRefs(ref commonv1.SecretStoreRefSpec) []esov1alpha1.PushSecretStoreRef` — builds the single-element ESO PushSecret store-ref slice from a resolved store reference.

---

## Package: `tls`

Manages TLS certificates and secrets for CobaltCore operators. Uses typed structs from
`github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1`.

### EnsureCertificate

```go
func EnsureCertificate(
    ctx context.Context,
    c client.Client,
    scheme *runtime.Scheme,
    owner client.Object,
    cert *certmanagerv1.Certificate,
) (bool, error)
```

Creates or updates a cert-manager Certificate CR.

**Returns:** `(true, nil)` when the Certificate has a `Ready` condition with status
`True`; `(false, nil)` when it exists but is not yet ready; `(false, error)` on failure.

**Behavior:**

- On create: sets controller owner reference, creates the resource, returns `(false, nil)`.
- On update: overwrites `existing.Spec` with the provided spec.
- Readiness is determined by the package-internal `isCertificateReady` helper on
  the existing resource.
- cert-manager creates a Secret with the TLS certificate once the Certificate is ready.

---

## Cross-Package Dependencies

These `internal/common` packages are independent of each other; reconcilers
compose them directly (for example a Keystone sub-reconciler calls
`job.RunJob` and `database.EnsureDatabase` side by side).

## Reconciler Integration Pattern

A typical reconciler calls these packages in its sub-reconciler phases:

```text
SecretsReady      → secrets.WaitForExternalSecret, secrets.IsSecretReady
DatabaseReady     → database.EnsureDatabase, database.EnsureDatabaseUser,
                    job.RunJob
ConfigReady       → config.CreateImmutableConfigMap
DeploymentReady   → deployment.EnsureDeployment, deployment.EnsureService
ConfigMapPruning  → config.PruneImmutableConfigMaps (after DeploymentReady)
TLSReady          → tls.EnsureCertificate
PolicyReady       → policy.LoadPolicyFromConfigMap
```

Each phase returns a readiness boolean. The reconciler advances to the next phase only
when the previous phase returns `true`. If any phase returns `false`, the reconciler
requeues and re-evaluates on the next reconciliation.
