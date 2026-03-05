---
title: Kubernetes-Interacting Packages
quadrant: internal-common
feature: CC-0005
---

# Kubernetes-Interacting Packages

Reference documentation for the packages introduced by CC-0005 that interact with the
Kubernetes API server and external operator CRDs. These packages provide reconciler
building blocks for managing Secrets, Databases, Deployments, Jobs, TLS certificates,
immutable ConfigMaps, and policy ConfigMaps.

All packages share a common design:

- **Idempotent create-or-update** via `controllerutil.CreateOrUpdate`
- **Owner references** via `controllerutil.SetControllerReference` for garbage collection
- **Typed CRD imports** for compile-time safety with external operators
- **Consistent error wrapping** via `fmt.Errorf` with `%w` for error chain inspection

## Package Locations

| Package | Path | External CRD Dependency |
| --- | --- | --- |
| `secrets` | `internal/common/secrets/` | external-secrets-operator (v1beta1, v1alpha1) |
| `database` | `internal/common/database/` | mariadb-operator (v1alpha1) |
| `deployment` | `internal/common/deployment/` | None (core Kubernetes) |
| `job` | `internal/common/job/` | None (core Kubernetes) |
| `tls` | `internal/common/tls/` | cert-manager (v1) |
| `config` | `internal/common/config/` | None (core Kubernetes) |
| `policy` | `internal/common/policy/` | None (core Kubernetes) |

## secrets

Helpers for managing Kubernetes Secrets and External Secrets Operator resources
(ExternalSecrets, PushSecrets). Used by reconcilers to implement the `SecretsReady`
condition.

### WaitForExternalSecret

```go
func WaitForExternalSecret(ctx context.Context, c client.Client, namespace, name string) (bool, error)
```

Checks whether an ESO `ExternalSecret` has synced by inspecting its `Ready` condition.

| Return | Condition |
| --- | --- |
| `(true, nil)` | ExternalSecret exists and has `Ready=True` condition |
| `(false, nil)` | ExternalSecret does not exist or exists but is not yet ready |
| `(false, error)` | Unexpected API server error |

**API dependency:** `github.com/external-secrets/external-secrets/apis/externalsecrets/v1beta1`

### IsSecretReady

```go
func IsSecretReady(ctx context.Context, c client.Client, namespace, name string) (bool, error)
```

Verifies that a Kubernetes Secret exists.

| Return | Condition |
| --- | --- |
| `(true, nil)` | Secret exists |
| `(false, nil)` | Secret is not found |
| `(false, error)` | Unexpected API server error |

### GetSecretValue

```go
func GetSecretValue(ctx context.Context, c client.Client, namespace, name, key string) (string, error)
```

Reads a specific key from a Kubernetes Secret and returns its decoded string value.

| Return | Condition |
| --- | --- |
| `(value, nil)` | Key exists in `Secret.Data` |
| `("", error)` | Secret not found or key not present |

**Error detail:** The returned error includes the key name and Secret namespace/name for
diagnostics.

### EnsurePushSecret

```go
func EnsurePushSecret(ctx context.Context, c client.Client, owner client.Object, desired *esov1alpha1.PushSecret) error
```

Creates or updates an ESO PushSecret CR. Sets a controller owner reference on the
PushSecret so it is garbage-collected when the owner is deleted. Uses
`controllerutil.CreateOrUpdate` for idempotent operation. Only the `Spec` field is
mutated during updates; `Name` and `Namespace` are taken from the `desired` parameter.

**API dependency:** `github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1`

**API stability note:** PushSecret uses the `v1alpha1` API, which is subject to breaking
changes in future ESO releases.

## database

Helpers for managing MariaDB Operator Database, User, and Grant resources, as well as
running database synchronisation Jobs. Used by reconcilers to implement the
`DatabaseReady` condition.

### EnsureDatabase

```go
func EnsureDatabase(ctx context.Context, c client.Client, owner client.Object, desired *mariadbv1alpha1.Database) (bool, error)
```

Creates or updates a MariaDB Operator Database CR. Sets a controller owner reference.
After create/update, re-fetches the Database to inspect its current status.

| Return | Condition |
| --- | --- |
| `(true, nil)` | Database has `Ready=True` condition |
| `(false, nil)` | Database exists but is not yet ready |
| `(false, error)` | API server error during create/update or status fetch |

**Readiness check:** Uses `meta.IsStatusConditionTrue(db.Status.Conditions, "Ready")`.

**API dependency:** `github.com/mariadb-operator/mariadb-operator/api/v1alpha1`

### EnsureDatabaseUser

```go
func EnsureDatabaseUser(ctx context.Context, c client.Client, owner client.Object, desiredUser *mariadbv1alpha1.User, desiredGrant *mariadbv1alpha1.Grant) (bool, error)
```

Creates or updates a MariaDB Operator User CR **and** its associated Grant CR in a single
call. Sets controller owner references on both resources. After create/update, re-fetches
both to inspect their status.

| Return | Condition |
| --- | --- |
| `(true, nil)` | Both User and Grant have `Ready=True` condition |
| `(false, nil)` | Either User or Grant is not yet ready |
| `(false, error)` | API server error during create/update or status fetch |

**Ordering:** The User is created first, then the Grant. Both must be ready for the
function to return `true`.

### RunDBSyncJob

```go
func RunDBSyncJob(ctx context.Context, c client.Client, owner client.Object, desired *batchv1.Job) (bool, error)
```

Creates or updates a Kubernetes Job intended for database schema synchronisation
(migrations, seed data). Sets a controller owner reference. After create/update,
re-fetches the Job to check completion status.

| Return | Condition |
| --- | --- |
| `(true, nil)` | Job has `Status.Succeeded >= 1` |
| `(false, nil)` | Job exists but has not yet succeeded |
| `(false, error)` | API server error during create/update or status fetch |

## deployment

Helpers for managing Kubernetes Deployments and Services. Used by reconcilers to implement
the `DeploymentReady` condition.

### EnsureDeployment

```go
func EnsureDeployment(ctx context.Context, c client.Client, owner client.Object, spec *appsv1.Deployment) (bool, error)
```

Creates or updates a Deployment using `controllerutil.CreateOrUpdate`. Sets a controller
owner reference. The mutate function copies `Labels` and `Spec` from the desired
Deployment.

| Return | Condition |
| --- | --- |
| `(true, nil)` | Deployment is ready (see `IsDeploymentReady`) |
| `(false, nil)` | Deployment exists but is not yet ready |
| `(false, error)` | API server error |

### EnsureService

```go
func EnsureService(ctx context.Context, c client.Client, owner client.Object, spec *corev1.Service) error
```

Creates or updates a ClusterIP Service. Sets a controller owner reference. The mutate
function copies `Labels`, `Spec.Selector`, `Spec.Ports`, and `Spec.Type` from the desired
Service.

**ClusterIP preservation:** The `Spec.ClusterIP` and `Spec.ClusterIPs` fields are
intentionally not overwritten during updates because they are assigned by the API server
and must be preserved.

### IsDeploymentReady

```go
func IsDeploymentReady(deployment *appsv1.Deployment) bool
```

Pure function (no API calls) that checks whether all desired replicas of a Deployment are
available, ready, and updated. Returns `true` when all three conditions are met:

- `Status.AvailableReplicas >= desired`
- `Status.ReadyReplicas >= desired`
- `Status.UpdatedReplicas >= desired`

**Nil replicas handling:** If `Spec.Replicas` is `nil`, the desired count defaults to `1`,
matching the Kubernetes API server default.

## job

Helpers for managing Kubernetes Jobs and CronJobs. Used by reconcilers to implement
`db_sync` and Fernet rotation phases.

### RunJob

```go
func RunJob(ctx context.Context, c client.Client, owner client.Object, desired *batchv1.Job) (bool, error)
```

Creates or updates a Kubernetes Job and checks whether it has completed. The `desired`
parameter must be a fully constructed `*batchv1.Job` including `ObjectMeta` (Name,
Namespace) and `Spec`. Sets a controller owner reference.

| Return | Condition |
| --- | --- |
| `(true, nil)` | Job has `Status.Succeeded >= 1` |
| `(false, nil)` | Job exists but has not yet succeeded |
| `(false, error)` | API server error during create/update or status fetch |

### EnsureCronJob

```go
func EnsureCronJob(ctx context.Context, c client.Client, owner client.Object, desired *batchv1.CronJob) error
```

Creates or updates a Kubernetes CronJob. The `desired` parameter must be a fully
constructed `*batchv1.CronJob` including `ObjectMeta` and `Spec`. Sets a controller owner
reference.

### IsJobComplete

```go
func IsJobComplete(job *batchv1.Job) bool
```

Pure function (no API calls) that returns `true` if the given Job has completed
successfully, indicated by `Status.Succeeded >= 1`. Returns `false` for `nil` input.

## tls

Helpers for managing TLS certificates via cert-manager. Used by reconcilers to provision
TLS certificates for service endpoints.

### EnsureCertificate

```go
func EnsureCertificate(ctx context.Context, c client.Client, owner client.Object, desired *certmanagerv1.Certificate) error
```

Creates or updates a cert-manager Certificate CR. Sets a controller owner reference. The
`desired` parameter must be a fully constructed `*certmanagerv1.Certificate` including
`ObjectMeta` (Name, Namespace) and `Spec`.

**API dependency:** `github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1`

### GetTLSSecret

```go
func GetTLSSecret(ctx context.Context, c client.Client, namespace, name string) (*corev1.Secret, error)
```

Retrieves the Kubernetes Secret that cert-manager populates with TLS certificate material.
The `name` typically matches the Certificate's `spec.secretName` field.

| Return | Condition |
| --- | --- |
| `(*corev1.Secret, nil)` | Secret exists |
| `(nil, error)` | Secret not found or API server error |

## config (CC-0005 extension)

The `config` package was introduced by CC-0004 for pure INI rendering functions. CC-0005
extends it with a Kubernetes-interacting function for immutable ConfigMap management.

### CreateImmutableConfigMap

```go
func CreateImmutableConfigMap(ctx context.Context, c client.Client, owner client.Object, name string, data map[string]string) (*corev1.ConfigMap, error)
```

Creates an immutable ConfigMap with a content-hash suffix in its name. The hash is computed
from the data map using SHA-256 with sorted keys for determinism. The resulting ConfigMap
name follows the pattern `{name}-{hash[:8]}`.

**Properties:**

- `Immutable` is set to `true` on the ConfigMap
- Controller owner reference is set via `controllerutil.SetControllerReference`
- Namespace is derived from `owner.GetNamespace()`
- Same data always produces the same hash suffix (deterministic)
- Different data produces a different hash suffix, which creates a new ConfigMap name and
  triggers Deployment rolling restarts when the volume reference changes

**Use case:** Config changes trigger Deployment rolling restarts via changed volume
references. Old ConfigMaps are garbage-collected through owner references when the owning
CR is deleted.

## policy (CC-0005 extension)

The `policy` package was introduced by CC-0004 for pure oslo.policy rendering functions.
CC-0005 extends it with a Kubernetes-interacting function for reading policy overrides from
ConfigMaps.

### LoadPolicyFromConfigMap

```go
func LoadPolicyFromConfigMap(ctx context.Context, c client.Client, namespace, name string) (map[string]string, error)
```

Reads a ConfigMap by namespace/name, extracts the `policy.yaml` key from its `Data`, and
parses it as a YAML mapping of string keys to string values (oslo.policy rules).

| Return | Condition |
| --- | --- |
| `(map[string]string, nil)` | ConfigMap exists and `policy.yaml` key parses successfully |
| `(nil, error)` | ConfigMap not found |
| `(nil, error)` | `policy.yaml` key absent from ConfigMap |
| `(nil, error)` | `policy.yaml` contains invalid YAML |

## Common Patterns

### Owner References

All `Ensure*` and `Create*` functions accept an `owner client.Object` parameter and call
`controllerutil.SetControllerReference(owner, resource, scheme)`. This establishes a
Kubernetes owner reference so that the created resource is garbage-collected when the owner
is deleted. The owner must be a persisted object (it must have a UID assigned by the API
server).

### Idempotent Create-or-Update

Functions that manage resources use `controllerutil.CreateOrUpdate`, which:

1. Attempts to get the existing resource by name/namespace
2. If not found, creates it after calling the mutate function
3. If found, calls the mutate function then updates the resource

The mutate function must not modify the object's `Name` or `Namespace` — only `Spec`,
`Data`, `Labels`, and similar fields.

### Error Wrapping

All errors are wrapped with `fmt.Errorf("context: %w", err)` to preserve the original
error for inspection via `errors.Is` and `errors.As`. The context string includes the
resource type, namespace, and name for diagnostics.

## Dependencies on Prior Features

| Feature | Artifact | Used by | Purpose |
| --- | --- | --- | --- |
| CC-0001 | `go.work` | All packages | Go Workspace enabling cross-module imports |
| CC-0002 | `testutil/envtest` | Integration tests | envtest setup, namespace creation, cleanup |
| CC-0002 | `testutil/simulators` | Integration tests | Simulating external CRD readiness |
| CC-0002 | `testutil/builders` | Integration tests | Fluent test resource construction |
| CC-0004 | `config.MergeDefaults` | `config.CreateImmutableConfigMap` | Deep copy for map data |
| CC-0004 | `policy.RenderPolicyYAML` | Reconcilers using `LoadPolicyFromConfigMap` | Rendering loaded policies |
| CC-0004 | `types.PolicySpec` | `policy.MergePolicies` | Policy data structure |

## External Go Module Dependencies

| Module | Version API | Types Used |
| --- | --- | --- |
| `github.com/mariadb-operator/mariadb-operator` | `api/v1alpha1` | `Database`, `User`, `Grant`, `MariaDB` |
| `github.com/external-secrets/external-secrets` | `apis/externalsecrets/v1beta1` | `ExternalSecret` |
| `github.com/external-secrets/external-secrets` | `apis/externalsecrets/v1alpha1` | `PushSecret` |
| `github.com/cert-manager/cert-manager` | `pkg/apis/certmanager/v1` | `Certificate` |
