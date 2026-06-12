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

- **Idempotent create-or-update** — `Ensure*` functions create a resource if it does not
  exist or update its spec if it already exists. They use `Get` + `Create`/`Update` (not
  `controllerutil.CreateOrUpdate`) for explicit control.
- **Owner references** — `controllerutil.SetControllerReference` is called on create so
  the resource is garbage-collected when the owning CR is deleted.
- **Readiness reporting** — Functions that create resources return `(bool, error)`, where
  `true` means the resource is ready and `false` means it exists but is not yet ready.
- **Error wrapping** — All errors include context via `fmt.Errorf` with `%w` for
  `errors.Is`/`errors.As` compatibility.

## Import Paths

| Package | Import Path |
| --- | --- |
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
| External Secrets Operator | `github.com/external-secrets/external-secrets` | `v1beta1` (ExternalSecret), `v1alpha1` (PushSecret) | `ExternalSecret`, `PushSecret` |
| cert-manager | `github.com/cert-manager/cert-manager` | `v1` | `Certificate` |

> **Note:** The PushSecret API is `v1alpha1` (unstable). Its schema may change in future
> ESO releases. Pin the ESO module version in `go.mod` to avoid breakage.

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

- On create: sets controller owner reference, creates the resource, returns `(false, nil)`.
- On update: overwrites `existing.Spec` with the provided spec.
- Readiness is determined by the package-internal `isDatabaseReady` helper on the
  existing resource.

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

- On create: sets controller owner reference, creates the resource, returns `(false, nil)`.
- On update: overwrites `existing.Spec` with the provided spec.
- Readiness is determined by `IsDeploymentReady` on the existing resource.

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

- On create: sets controller owner reference, creates the resource.
- On update: preserves the existing `ClusterIP` and `ClusterIPs` values assigned by the
  API server before overwriting the spec. This prevents accidental ClusterIP reassignment,
  which would break existing DNS-based service discovery.

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

- On create: sets controller owner reference, creates the resource.
- On update: overwrites `existing.Spec` with the provided spec. CronJob spec updates
  take effect on the next scheduled run.

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
) (bool, error)
```

Checks whether the ExternalSecret identified by `key` has a `Ready` condition with
status `True` (the ESO `ExternalSecretReady` condition type).

**Returns:** `(true, nil)` when synced; `(false, nil)` when not yet synced;
`(false, error)` when the ExternalSecret does not exist or API call fails.

**Behavior:**

- This is a point-in-time check, not a blocking wait. Reconcilers should call it on each
  reconciliation loop and requeue if it returns `false`.
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

- On create: sets controller owner reference via `SetControllerReference`, creates the
  resource.
- On update: overwrites `existing.Spec` with the provided spec.
- Uses the ESO `v1alpha1` PushSecret API (unstable — see [External CRD Dependencies](#external-crd-dependencies)).

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
