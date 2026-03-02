---
title: Kubernetes-Interacting Packages
quadrant: backend
---

# Kubernetes-Interacting Packages

**Module:** `internal/common`
**Feature:** CC-0005

Kubernetes-client-dependent packages for managing cluster resources from OpenStack operator reconcilers. All functions use `controller-runtime` client and set owner references for garbage collection. Builds on the pure-function packages documented in [Shared Utility Packages](internal-common-packages.md).

## config (K8s extensions)

**Package:** `internal/common/config`
**Import:** `"github.com/c5c3/forge/internal/common/config"`

Extensions to the config package that interact with the Kubernetes API.

### CreateImmutableConfigMap

```go
func CreateImmutableConfigMap(ctx context.Context, c client.Client, owner client.Object, scheme *runtime.Scheme, name, namespace string, data map[string]string) (string, error)
```

Creates an immutable ConfigMap with content-hash naming. The name is formed as `{name}-{hash8}` where `hash8` is the first 8 hex characters of the SHA-256 hash of the data (keys sorted for determinism). If a ConfigMap with that name already exists, it is returned as-is (idempotent). (CC-0005 / REQ-001, REQ-009, REQ-010)

| Parameter   | Type                | Description                              |
|-------------|---------------------|------------------------------------------|
| `ctx`       | `context.Context`   | Request context                          |
| `c`         | `client.Client`     | Kubernetes client                        |
| `owner`     | `client.Object`     | Owner for garbage collection             |
| `scheme`    | `*runtime.Scheme`   | Runtime scheme for owner ref resolution  |
| `name`      | `string`            | Base name prefix for the ConfigMap       |
| `namespace` | `string`            | Target namespace                         |
| `data`      | `map[string]string` | ConfigMap data entries                   |

**Returns:** `(string, error)` — the full ConfigMap name (with hash suffix) and any error.

**Behavior:**
- Data keys are sorted before hashing for deterministic names across reconcile loops
- ConfigMap is created with `Immutable: true` — never updated after creation
- Owner references are set via `controllerutil.SetControllerReference`
- If the named ConfigMap already exists, returns its name without modification

---

## policy (K8s extensions)

**Package:** `internal/common/policy`
**Import:** `"github.com/c5c3/forge/internal/common/policy"`

Extensions to the policy package that interact with the Kubernetes API.

### LoadPolicyFromConfigMap

```go
func LoadPolicyFromConfigMap(ctx context.Context, c client.Client, configMapRef *corev1.LocalObjectReference, namespace string) (map[string]string, error)
```

Fetches the ConfigMap referenced by `configMapRef`, reads the `"policy.yaml"` key from its Data, and parses the YAML content into a flat map of policy rules. (CC-0005 / REQ-008)

| Parameter      | Type                               | Description                          |
|----------------|------------------------------------|--------------------------------------|
| `ctx`          | `context.Context`                  | Request context                      |
| `c`            | `client.Client`                    | Kubernetes client                    |
| `configMapRef` | `*corev1.LocalObjectReference`     | Reference to the source ConfigMap    |
| `namespace`    | `string`                           | Namespace of the ConfigMap           |

**Returns:** `(map[string]string, error)` — parsed policy rules and any error.

**Errors:**
- ConfigMap not found
- `"policy.yaml"` key missing from ConfigMap data
- YAML parse failure

---

## secrets

**Package:** `internal/common/secrets`
**Import:** `"github.com/c5c3/forge/internal/common/secrets"`

Kubernetes-interacting helpers for managing Secrets, ExternalSecrets, and PushSecrets.

### Types

```go
type StoreRef struct {
    Name string
    Kind string // "ClusterSecretStore" or "SecretStore"
}

type PushSecretOpts struct {
    Name          string
    Namespace     string
    SecretName    string   // source Secret name
    SecretKeys    []string // keys to push from the source Secret
    RemoteKeyBase string   // base path in external store (e.g. "secret/data/keystone")
    StoreRef      StoreRef // reference to ClusterSecretStore
}
```

### IsExternalSecretReady

```go
func IsExternalSecretReady(ctx context.Context, c client.Client, name, namespace string) (bool, error)
```

Checks if an ExternalSecret CR has a `Ready=True` condition in its status. Uses unstructured access since ESO types are not imported. (CC-0005 / REQ-002)

| Parameter   | Type              | Description                           |
|-------------|-------------------|---------------------------------------|
| `ctx`       | `context.Context` | Request context                       |
| `c`         | `client.Client`   | Kubernetes client                     |
| `name`      | `string`          | ExternalSecret name                   |
| `namespace` | `string`          | ExternalSecret namespace              |

**Returns:** `(bool, error)` — `true` if Ready condition is True; `false` if not found or not ready.

### IsSecretReady

```go
func IsSecretReady(ctx context.Context, c client.Client, name, namespace string) (bool, error)
```

Checks if a Kubernetes Secret exists and has at least one data key. (CC-0005 / REQ-003)

| Parameter   | Type              | Description           |
|-------------|-------------------|-----------------------|
| `ctx`       | `context.Context` | Request context       |
| `c`         | `client.Client`   | Kubernetes client     |
| `name`      | `string`          | Secret name           |
| `namespace` | `string`          | Secret namespace      |

**Returns:** `(bool, error)` — `true` if the Secret exists with `len(Data) > 0`.

### GetSecretValue

```go
func GetSecretValue(ctx context.Context, c client.Client, name, namespace, key string) (string, error)
```

Retrieves a specific key's value from a Kubernetes Secret. Returns an error if the Secret does not exist or the key is missing. (CC-0005 / REQ-003)

| Parameter   | Type              | Description                     |
|-------------|-------------------|---------------------------------|
| `ctx`       | `context.Context` | Request context                 |
| `c`         | `client.Client`   | Kubernetes client               |
| `name`      | `string`          | Secret name                     |
| `namespace` | `string`          | Secret namespace                |
| `key`       | `string`          | Data key to retrieve            |

**Returns:** `(string, error)` — the value as a string, or an error.

### EnsurePushSecret

```go
func EnsurePushSecret(ctx context.Context, c client.Client, owner client.Object, scheme *runtime.Scheme, opts PushSecretOpts) (string, error)
```

Creates or updates a PushSecret CR that pushes the given source Secret keys to an external secret store. Uses create-or-update pattern. Sets owner references for garbage collection. (CC-0005 / REQ-003, REQ-009, REQ-010)

| Parameter | Type              | Description                              |
|-----------|-------------------|------------------------------------------|
| `ctx`     | `context.Context` | Request context                          |
| `c`       | `client.Client`   | Kubernetes client                        |
| `owner`   | `client.Object`   | Owner for garbage collection             |
| `scheme`  | `*runtime.Scheme` | Runtime scheme for owner ref resolution  |
| `opts`    | `PushSecretOpts`  | PushSecret configuration                 |

**Returns:** `(string, error)` — the PushSecret name and any error.

**Behavior:**
- Remote keys are formed as `{RemoteKeyBase}/{key}` for each secret key
- References a `ClusterSecretStore` or `SecretStore` via `StoreRef`
- If PushSecret already exists, updates its spec and owner references

---

## tls

**Package:** `internal/common/tls`
**Import:** `"github.com/c5c3/forge/internal/common/tls"`

Kubernetes-interacting helpers for managing TLS certificates via cert-manager and retrieving TLS secrets.

### Types

```go
type CertificateOpts struct {
    Name       string
    Namespace  string
    SecretName string // name of the Secret cert-manager will create
    IssuerRef  IssuerRef
    DNSNames   []string
}

type IssuerRef struct {
    Name  string
    Kind  string // "ClusterIssuer" or "Issuer"
    Group string // typically "cert-manager.io"
}
```

### EnsureCertificate

```go
func EnsureCertificate(ctx context.Context, c client.Client, owner client.Object, scheme *runtime.Scheme, opts CertificateOpts) (string, error)
```

Creates or updates a cert-manager Certificate CR with the given options. Uses create-or-update pattern. Sets owner references for garbage collection. (CC-0005 / REQ-007, REQ-009, REQ-010)

| Parameter | Type                | Description                              |
|-----------|---------------------|------------------------------------------|
| `ctx`     | `context.Context`   | Request context                          |
| `c`       | `client.Client`     | Kubernetes client                        |
| `owner`   | `client.Object`     | Owner for garbage collection             |
| `scheme`  | `*runtime.Scheme`   | Runtime scheme for owner ref resolution  |
| `opts`    | `CertificateOpts`   | Certificate configuration                |

**Returns:** `(string, error)` — the Certificate name and any error.

**Behavior:**
- Uses unstructured client (cert-manager types not imported)
- DNSNames are optional; omitted from spec if empty
- If Certificate already exists, updates spec and owner references

### GetTLSSecret

```go
func GetTLSSecret(ctx context.Context, c client.Client, name, namespace string) (cert []byte, key []byte, err error)
```

Retrieves the TLS certificate and private key from a Kubernetes Secret created by cert-manager. (CC-0005 / REQ-007)

| Parameter   | Type              | Description                    |
|-------------|-------------------|--------------------------------|
| `ctx`       | `context.Context` | Request context                |
| `c`         | `client.Client`   | Kubernetes client              |
| `name`      | `string`          | Secret name                    |
| `namespace` | `string`          | Secret namespace               |

**Returns:** `([]byte, []byte, error)` — `tls.crt` data, `tls.key` data, and any error.

**Errors:** Returns an error if the Secret is missing or if either `tls.crt` or `tls.key` keys are absent.

---

## database

**Package:** `internal/common/database`
**Import:** `"github.com/c5c3/forge/internal/common/database"`

Functions for managing MariaDB database resources (Database, User, Grant CRs) and database schema migration Jobs via the Kubernetes API. Uses unstructured client for MariaDB operator CRDs (`k8s.mariadb.com/v1alpha1`).

### Types

```go
type DatabaseOpts struct {
    Name         string
    Namespace    string
    MariaDBRef   string // name of the MariaDB CR
    DatabaseName string // the actual database name
}

type DatabaseUserOpts struct {
    Name               string
    Namespace          string
    MariaDBRef         string
    Username           string
    PasswordSecretName string
    PasswordSecretKey  string
    DatabaseName       string
    Privileges         []string
}

type DBSyncJobOpts struct {
    Name      string
    Namespace string
    Image     string // "repository:tag" format
    Command   []string
    Env       []corev1.EnvVar
}
```

### EnsureDatabase

```go
func EnsureDatabase(ctx context.Context, c client.Client, owner client.Object, scheme *runtime.Scheme, opts DatabaseOpts) (string, error)
```

Creates or updates a MariaDB Database CR (`k8s.mariadb.com/v1alpha1/Database`). Uses create-or-update pattern with unstructured client. Sets owner references for garbage collection. (CC-0005 / REQ-004, REQ-009, REQ-010)

| Parameter | Type              | Description                              |
|-----------|-------------------|------------------------------------------|
| `ctx`     | `context.Context` | Request context                          |
| `c`       | `client.Client`   | Kubernetes client                        |
| `owner`   | `client.Object`   | Owner for garbage collection             |
| `scheme`  | `*runtime.Scheme` | Runtime scheme for owner ref resolution  |
| `opts`    | `DatabaseOpts`    | Database CR configuration                |

**Returns:** `(string, error)` — the Database CR name and any error.

### EnsureDatabaseUser

```go
func EnsureDatabaseUser(ctx context.Context, c client.Client, owner client.Object, scheme *runtime.Scheme, opts DatabaseUserOpts) (string, error)
```

Creates or updates a MariaDB User CR and a Grant CR. The Grant CR name is derived as `{name}-grant`. Both CRs get owner references. (CC-0005 / REQ-004, REQ-009)

| Parameter | Type               | Description                              |
|-----------|--------------------|------------------------------------------|
| `ctx`     | `context.Context`  | Request context                          |
| `c`       | `client.Client`    | Kubernetes client                        |
| `owner`   | `client.Object`    | Owner for garbage collection             |
| `scheme`  | `*runtime.Scheme`  | Runtime scheme for owner ref resolution  |
| `opts`    | `DatabaseUserOpts` | User and Grant CR configuration          |

**Returns:** `(string, error)` — the User CR name and any error.

**Behavior:**
- Creates/updates User CR with password secret reference
- Creates/updates Grant CR with specified privileges on `{DatabaseName}.*`

### RunDBSyncJob

```go
func RunDBSyncJob(ctx context.Context, c client.Client, owner client.Object, scheme *runtime.Scheme, opts DBSyncJobOpts) (string, error)
```

Creates a `batch/v1` Job for database schema migration. Uses create-or-skip pattern — if the Job already exists, returns its name without error (idempotent). Sets owner references for garbage collection. (CC-0005 / REQ-004, REQ-009, REQ-010)

| Parameter | Type              | Description                              |
|-----------|-------------------|------------------------------------------|
| `ctx`     | `context.Context` | Request context                          |
| `c`       | `client.Client`   | Kubernetes client                        |
| `owner`   | `client.Object`   | Owner for garbage collection             |
| `scheme`  | `*runtime.Scheme` | Runtime scheme for owner ref resolution  |
| `opts`    | `DBSyncJobOpts`   | Job configuration                        |

**Returns:** `(string, error)` — the Job name and any error.

**Behavior:**
- Container named `"db-sync"` with `RestartPolicy: OnFailure`
- Existing Job (AlreadyExists) is a no-op — not updated

---

## deployment

**Package:** `internal/common/deployment`
**Import:** `"github.com/c5c3/forge/internal/common/deployment"`

Helpers for managing Kubernetes Deployments and Services, including readiness checks and server-side apply operations.

### IsDeploymentReady

```go
func IsDeploymentReady(deployment *appsv1.Deployment) bool
```

Pure function that returns `true` when the Deployment has an Available condition with status True and ready replicas >= desired replicas. No API calls. (CC-0005 / REQ-005)

| Parameter    | Type                   | Description                  |
|--------------|------------------------|------------------------------|
| `deployment` | `*appsv1.Deployment`   | Deployment to check          |

**Returns:** `bool` — `true` only when both conditions are met. Returns `false` for nil input or nil `Spec.Replicas`.

### EnsureDeployment

```go
func EnsureDeployment(ctx context.Context, c client.Client, owner client.Object, scheme *k8sruntime.Scheme, deployment *appsv1.Deployment) error
```

Applies a Deployment via server-side apply with field manager `"cobaltcore-operator"`. Sets owner references for garbage collection. (CC-0005 / REQ-005, REQ-009, REQ-010)

| Parameter    | Type                   | Description                              |
|--------------|------------------------|------------------------------------------|
| `ctx`        | `context.Context`      | Request context                          |
| `c`          | `client.Client`        | Kubernetes client                        |
| `owner`      | `client.Object`        | Owner for garbage collection             |
| `scheme`     | `*k8sruntime.Scheme`   | Runtime scheme for owner ref resolution  |
| `deployment` | `*appsv1.Deployment`   | Deployment to apply                      |

**Returns:** `error` — any apply error.

### EnsureService

```go
func EnsureService(ctx context.Context, c client.Client, owner client.Object, scheme *k8sruntime.Scheme, service *corev1.Service) error
```

Applies a Service via server-side apply with field manager `"cobaltcore-operator"`. Sets owner references for garbage collection. (CC-0005 / REQ-005, REQ-009, REQ-010)

| Parameter | Type                 | Description                              |
|-----------|----------------------|------------------------------------------|
| `ctx`     | `context.Context`    | Request context                          |
| `c`       | `client.Client`      | Kubernetes client                        |
| `owner`   | `client.Object`      | Owner for garbage collection             |
| `scheme`  | `*k8sruntime.Scheme` | Runtime scheme for owner ref resolution  |
| `service` | `*corev1.Service`    | Service to apply                         |

**Returns:** `error` — any apply error.

---

## job

**Package:** `internal/common/job`
**Import:** `"github.com/c5c3/forge/internal/common/job"`

Helpers for managing Kubernetes Jobs and CronJobs, including completion checks and batch operation lifecycle.

### IsJobComplete

```go
func IsJobComplete(job *batchv1.Job) bool
```

Pure function that returns `true` when the Job has a Complete condition with status True. No API calls. (CC-0005 / REQ-006)

| Parameter | Type            | Description      |
|-----------|-----------------|------------------|
| `job`     | `*batchv1.Job`  | Job to check     |

**Returns:** `bool` — `true` when Complete condition has status True. Returns `false` for nil input.

### RunJob

```go
func RunJob(ctx context.Context, c client.Client, owner client.Object, scheme *k8sruntime.Scheme, job *batchv1.Job) (string, error)
```

Creates a Job using the create-or-skip pattern for idempotency. If the Job already exists, returns its name without error. Sets owner references for garbage collection. (CC-0005 / REQ-006, REQ-009, REQ-010)

| Parameter | Type                 | Description                              |
|-----------|----------------------|------------------------------------------|
| `ctx`     | `context.Context`    | Request context                          |
| `c`       | `client.Client`      | Kubernetes client                        |
| `owner`   | `client.Object`      | Owner for garbage collection             |
| `scheme`  | `*k8sruntime.Scheme` | Runtime scheme for owner ref resolution  |
| `job`     | `*batchv1.Job`       | Job to create                            |

**Returns:** `(string, error)` — the Job name and any error.

### EnsureCronJob

```go
func EnsureCronJob(ctx context.Context, c client.Client, owner client.Object, scheme *k8sruntime.Scheme, cronJob *batchv1.CronJob) (string, error)
```

Applies a CronJob via server-side apply with field manager `"cobaltcore-operator"`. Sets owner references for garbage collection. (CC-0005 / REQ-006, REQ-009, REQ-010)

| Parameter | Type                 | Description                              |
|-----------|----------------------|------------------------------------------|
| `ctx`     | `context.Context`    | Request context                          |
| `c`       | `client.Client`      | Kubernetes client                        |
| `owner`   | `client.Object`      | Owner for garbage collection             |
| `scheme`  | `*k8sruntime.Scheme` | Runtime scheme for owner ref resolution  |
| `cronJob` | `*batchv1.CronJob`   | CronJob to apply                         |

**Returns:** `(string, error)` — the CronJob name and any error.

---

## Resource Management Patterns

All K8s-interacting functions follow one of two idempotency patterns:

### Create-or-Update (Unstructured)

Used by: `EnsureDatabase`, `EnsureDatabaseUser`, `EnsureCertificate`, `EnsurePushSecret`

```
Get existing → NotFound? Create : Update spec + ownerRefs
```

### Server-Side Apply

Used by: `EnsureDeployment`, `EnsureService`, `EnsureCronJob`

```
Set TypeMeta → Convert to unstructured → Apply with field manager + force
```

### Create-or-Skip

Used by: `RunJob`, `RunDBSyncJob`, `CreateImmutableConfigMap`

```
Create → AlreadyExists? Return name (no-op) : Return name
```

All functions set owner references via `controllerutil.SetControllerReference` for automatic garbage collection when the parent resource is deleted.

## Dependencies

- **CC-0004** — pure-function packages (`config/`, `policy/`, `conditions/`, `plugins/`, `types/`)
- **CC-0005** — implements all K8s-interacting extensions documented here
- **CC-0003** — `testutil/envtest` setup, builders, assertions, simulators
