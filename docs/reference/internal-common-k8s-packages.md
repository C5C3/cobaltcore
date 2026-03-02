---
title: Kubernetes-Interacting Packages
quadrant: backend
---

# Kubernetes-Interacting Packages

**Module:** `internal/common`
**Feature:** CC-0005

Functions that interact with the Kubernetes API server via `controller-runtime/pkg/client`. Used by operator reconcilers for resource management, secret handling, TLS provisioning, and database lifecycle. All "Ensure" functions are idempotent — AlreadyExists errors are treated as success.

---

## config (K8s extensions)

**Package:** `internal/common/config`
**Import:** `"github.com/c5c3/forge/internal/common/config"`

Extends the CC-0004 pure-function config package with a Kubernetes-client function for creating immutable ConfigMaps.

### CreateImmutableConfigMap

```go
func CreateImmutableConfigMap(ctx context.Context, c client.Client, name, namespace string, data map[string]string, ownerRefs ...metav1.OwnerReference) (string, error)
```

Creates a ConfigMap with `name={name}-{sha256[:8]}`, `immutable=true`, the provided data and owner references. Returns the generated name. Idempotent — AlreadyExists is not an error. (CC-0005, REQ-001)

| Parameter   | Type                      | Description                                      |
|-------------|---------------------------|--------------------------------------------------|
| `ctx`       | `context.Context`         | Context for the API call                         |
| `c`         | `client.Client`           | Kubernetes client                                |
| `name`      | `string`                  | Base name prefix for the ConfigMap               |
| `namespace` | `string`                  | Namespace for the ConfigMap                      |
| `data`      | `map[string]string`       | ConfigMap data entries                           |
| `ownerRefs` | `...metav1.OwnerReference`| Optional owner references for garbage collection |

**Returns:** `(string, error)` — the generated ConfigMap name (`{name}-{hash8}`) and any error.

**Hash computation:** A deterministic SHA256 hash of the data map (keys sorted) is computed, and the first 8 hex characters are appended to the base name. This ensures content-addressable naming — identical data always produces the same name.

---

## policy (K8s extensions)

**Package:** `internal/common/policy`
**Import:** `"github.com/c5c3/forge/internal/common/policy"`

Extends the CC-0004 pure-function policy package with a Kubernetes-client function for loading policy rules from a ConfigMap.

### LoadPolicyFromConfigMap

```go
func LoadPolicyFromConfigMap(ctx context.Context, c client.Client, name, namespace string) (map[string]string, error)
```

Fetches a ConfigMap and parses the `policy.yaml` key into a `map[string]string` of policy rules. Returns an error if the ConfigMap does not exist or the key is missing. (CC-0005, REQ-008)

| Parameter   | Type              | Description                           |
|-------------|-------------------|---------------------------------------|
| `ctx`       | `context.Context` | Context for the API call              |
| `c`         | `client.Client`   | Kubernetes client                     |
| `name`      | `string`          | Name of the ConfigMap                 |
| `namespace` | `string`          | Namespace of the ConfigMap            |

**Returns:** `(map[string]string, error)` — parsed policy rules and any error.

---

## secrets

**Package:** `internal/common/secrets`
**Import:** `"github.com/c5c3/forge/internal/common/secrets"`

Secret lifecycle management: checking ExternalSecret readiness, reading Secret values, and creating PushSecret CRs.

### IsExternalSecretReady

```go
func IsExternalSecretReady(ctx context.Context, c client.Client, name, namespace string) (bool, error)
```

Fetches an ExternalSecret CR (`external-secrets.io/v1beta1`) via the unstructured client and returns `true` when a `Ready=True` condition exists. Returns `(false, nil)` if the ExternalSecret does not exist. (CC-0005, REQ-002)

| Parameter   | Type              | Description                  |
|-------------|-------------------|------------------------------|
| `ctx`       | `context.Context` | Context for the API call     |
| `c`         | `client.Client`   | Kubernetes client            |
| `name`      | `string`          | ExternalSecret name          |
| `namespace` | `string`          | ExternalSecret namespace     |

**Returns:** `(bool, error)` — readiness state and any error.

### IsSecretReady

```go
func IsSecretReady(ctx context.Context, c client.Client, name, namespace string) (bool, error)
```

Returns `true` if a `core/v1` Secret with the given name exists. Returns `(false, nil)` if the Secret does not exist (NotFound). (CC-0005, REQ-003)

| Parameter   | Type              | Description              |
|-------------|-------------------|--------------------------|
| `ctx`       | `context.Context` | Context for the API call |
| `c`         | `client.Client`   | Kubernetes client        |
| `name`      | `string`          | Secret name              |
| `namespace` | `string`          | Secret namespace         |

**Returns:** `(bool, error)` — existence state and any error.

### GetSecretValue

```go
func GetSecretValue(ctx context.Context, c client.Client, name, namespace, key string) (string, error)
```

Retrieves the value of a specific key from a Kubernetes Secret. Returns an error if the Secret or key does not exist. (CC-0005, REQ-003)

| Parameter   | Type              | Description              |
|-------------|-------------------|--------------------------|
| `ctx`       | `context.Context` | Context for the API call |
| `c`         | `client.Client`   | Kubernetes client        |
| `name`      | `string`          | Secret name              |
| `namespace` | `string`          | Secret namespace         |
| `key`       | `string`          | Data key to retrieve     |

**Returns:** `(string, error)` — decoded string value and any error.

### EnsurePushSecret

```go
func EnsurePushSecret(ctx context.Context, c client.Client, name, namespace string, spec map[string]interface{}, ownerRefs ...metav1.OwnerReference) error
```

Creates a PushSecret CR (`external-secrets.io/v1alpha1`) via the unstructured client. Idempotent — AlreadyExists is not an error. (CC-0005, REQ-003, REQ-009, REQ-010)

| Parameter   | Type                        | Description                                      |
|-------------|-----------------------------|--------------------------------------------------|
| `ctx`       | `context.Context`           | Context for the API call                         |
| `c`         | `client.Client`             | Kubernetes client                                |
| `name`      | `string`                    | PushSecret name                                  |
| `namespace` | `string`                    | PushSecret namespace                             |
| `spec`      | `map[string]interface{}`    | PushSecret spec as unstructured data             |
| `ownerRefs` | `...metav1.OwnerReference`  | Optional owner references for garbage collection |

**Returns:** `error` — nil on success or if already exists.

---

## tls

**Package:** `internal/common/tls`
**Import:** `"github.com/c5c3/forge/internal/common/tls"`

TLS certificate lifecycle: creating cert-manager Certificate CRs and retrieving the resulting TLS Secrets.

### EnsureCertificate

```go
func EnsureCertificate(ctx context.Context, c client.Client, name, namespace string, dnsNames []string, secretName, issuerName, issuerKind, issuerGroup string, ownerRefs ...metav1.OwnerReference) error
```

Creates a cert-manager Certificate CR (`cert-manager.io/v1`) via the unstructured client. Idempotent — AlreadyExists is not an error. (CC-0005, REQ-007, REQ-009, REQ-010)

| Parameter     | Type                        | Description                                      |
|---------------|-----------------------------|--------------------------------------------------|
| `ctx`         | `context.Context`           | Context for the API call                         |
| `c`           | `client.Client`             | Kubernetes client                                |
| `name`        | `string`                    | Certificate name                                 |
| `namespace`   | `string`                    | Certificate namespace                            |
| `dnsNames`    | `[]string`                  | DNS SANs for the certificate                     |
| `secretName`  | `string`                    | Name of the Secret to store the TLS keypair      |
| `issuerName`  | `string`                    | cert-manager Issuer/ClusterIssuer name           |
| `issuerKind`  | `string`                    | Issuer kind (`"Issuer"` or `"ClusterIssuer"`)    |
| `issuerGroup` | `string`                    | Issuer API group (e.g. `"cert-manager.io"`)      |
| `ownerRefs`   | `...metav1.OwnerReference`  | Optional owner references for garbage collection |

**Returns:** `error` — nil on success or if already exists.

### GetTLSSecret

```go
func GetTLSSecret(ctx context.Context, c client.Client, name, namespace string) (*corev1.Secret, error)
```

Retrieves a typed `corev1.Secret` by name and namespace. Returns the Secret or an error if it does not exist. (CC-0005, REQ-007)

| Parameter   | Type              | Description              |
|-------------|-------------------|--------------------------|
| `ctx`       | `context.Context` | Context for the API call |
| `c`         | `client.Client`   | Kubernetes client        |
| `name`      | `string`          | Secret name              |
| `namespace` | `string`          | Secret namespace         |

**Returns:** `(*corev1.Secret, error)` — the Secret object and any error.

---

## database

**Package:** `internal/common/database`
**Import:** `"github.com/c5c3/forge/internal/common/database"`

MariaDB database lifecycle management via the mariadb-operator CRDs (`k8s.mariadb.com/v1alpha1`). All functions use the unstructured client for CRD operations and batch/v1 for schema migration Jobs.

### EnsureDatabase

```go
func EnsureDatabase(ctx context.Context, c client.Client, name, namespace, databaseName, mariadbRefName string, ownerRefs ...metav1.OwnerReference) error
```

Creates a MariaDB Database CR via the unstructured client. Idempotent — AlreadyExists is not an error. (CC-0005, REQ-004, REQ-009, REQ-010)

| Parameter       | Type                        | Description                                      |
|-----------------|-----------------------------|--------------------------------------------------|
| `ctx`           | `context.Context`           | Context for the API call                         |
| `c`             | `client.Client`             | Kubernetes client                                |
| `name`          | `string`                    | Database CR name                                 |
| `namespace`     | `string`                    | Database CR namespace                            |
| `databaseName`  | `string`                    | Logical database name (`spec.name`)              |
| `mariadbRefName`| `string`                    | MariaDB instance to create the database in       |
| `ownerRefs`     | `...metav1.OwnerReference`  | Optional owner references for garbage collection |

**Returns:** `error` — nil on success or if already exists.

### EnsureDatabaseUser

```go
func EnsureDatabaseUser(ctx context.Context, c client.Client, name, namespace, mariadbRefName, passwordSecretName, passwordSecretKey string, databaseName string, privileges []string, ownerRefs ...metav1.OwnerReference) error
```

Creates a MariaDB User CR and a Grant CR via the unstructured client. Both operations are idempotent. The Grant CR is named `{name}-grant` and grants the specified privileges on the given database. (CC-0005, REQ-004, REQ-009)

| Parameter           | Type                        | Description                                      |
|---------------------|-----------------------------|--------------------------------------------------|
| `ctx`               | `context.Context`           | Context for the API call                         |
| `c`                 | `client.Client`             | Kubernetes client                                |
| `name`              | `string`                    | User CR name (also used as `spec.name`)          |
| `namespace`         | `string`                    | User CR namespace                                |
| `mariadbRefName`    | `string`                    | MariaDB instance reference                       |
| `passwordSecretName`| `string`                    | Secret containing the password                   |
| `passwordSecretKey` | `string`                    | Key within the Secret                            |
| `databaseName`      | `string`                    | Database to grant privileges on                  |
| `privileges`        | `[]string`                  | Privileges to grant (e.g. `["SELECT", "INSERT"]`)|
| `ownerRefs`         | `...metav1.OwnerReference`  | Optional owner references for garbage collection |

**Returns:** `error` — nil on success or if resources already exist.

### RunDBSyncJob

```go
func RunDBSyncJob(ctx context.Context, c client.Client, name, namespace, image string, command []string, env []corev1.EnvVar, ownerRefs ...metav1.OwnerReference) error
```

Creates a `batch/v1` Job for database schema migration. Idempotent — AlreadyExists is not an error. The Job runs a single container with the specified image, command, and environment variables. (CC-0005, REQ-004, REQ-009)

| Parameter   | Type                        | Description                                      |
|-------------|-----------------------------|--------------------------------------------------|
| `ctx`       | `context.Context`           | Context for the API call                         |
| `c`         | `client.Client`             | Kubernetes client                                |
| `name`      | `string`                    | Job name                                         |
| `namespace` | `string`                    | Job namespace                                    |
| `image`     | `string`                    | Container image for the db-sync container        |
| `command`   | `[]string`                  | Command to run in the container                  |
| `env`       | `[]corev1.EnvVar`           | Environment variables for the container          |
| `ownerRefs` | `...metav1.OwnerReference`  | Optional owner references for garbage collection |

**Returns:** `error` — nil on success or if already exists.

---

## deployment

**Package:** `internal/common/deployment`
**Import:** `"github.com/c5c3/forge/internal/common/deployment"`

Deployment and Service lifecycle management using server-side apply, plus a pure readiness check function.

### IsDeploymentReady

```go
func IsDeploymentReady(deploy *appsv1.Deployment) bool
```

Returns `true` if the Deployment has reached its desired state: all replicas are updated, available, and ready, with no unavailable replicas. Pure function — no API calls. (CC-0005, REQ-005)

| Parameter | Type                  | Description            |
|-----------|-----------------------|------------------------|
| `deploy`  | `*appsv1.Deployment`  | Deployment to inspect  |

**Returns:** `bool` — `true` when `ObservedGeneration >= Generation` and `UpdatedReplicas == ReadyReplicas == AvailableReplicas == desired` and `UnavailableReplicas == 0`. Returns `false` for nil input.

### EnsureDeployment

```go
func EnsureDeployment(ctx context.Context, c client.Client, deploy *appsv1.Deployment, fieldManager string, ownerRefs ...metav1.OwnerReference) error
```

Uses server-side apply to create or update the given Deployment. (CC-0005, REQ-005, REQ-009, REQ-010)

| Parameter      | Type                        | Description                                      |
|----------------|-----------------------------|--------------------------------------------------|
| `ctx`          | `context.Context`           | Context for the API call                         |
| `c`            | `client.Client`             | Kubernetes client                                |
| `deploy`       | `*appsv1.Deployment`        | Deployment to apply                              |
| `fieldManager` | `string`                    | Field manager identifier for SSA                 |
| `ownerRefs`    | `...metav1.OwnerReference`  | Optional owner references for garbage collection |

**Returns:** `error` — nil on success.

### EnsureService

```go
func EnsureService(ctx context.Context, c client.Client, svc *corev1.Service, fieldManager string, ownerRefs ...metav1.OwnerReference) error
```

Uses server-side apply to create or update the given Service. (CC-0005, REQ-005, REQ-009, REQ-010)

| Parameter      | Type                        | Description                                      |
|----------------|-----------------------------|--------------------------------------------------|
| `ctx`          | `context.Context`           | Context for the API call                         |
| `c`            | `client.Client`             | Kubernetes client                                |
| `svc`          | `*corev1.Service`           | Service to apply                                 |
| `fieldManager` | `string`                    | Field manager identifier for SSA                 |
| `ownerRefs`    | `...metav1.OwnerReference`  | Optional owner references for garbage collection |

**Returns:** `error` — nil on success.

---

## job

**Package:** `internal/common/job`
**Import:** `"github.com/c5c3/forge/internal/common/job"`

Job and CronJob lifecycle management, plus a pure completion check function.

### IsJobComplete

```go
func IsJobComplete(job *batchv1.Job) bool
```

Returns `true` if the Job has completed successfully by checking for a `Complete` condition with status `True`. Pure function — no API calls. (CC-0005, REQ-006)

| Parameter | Type            | Description      |
|-----------|-----------------|------------------|
| `job`     | `*batchv1.Job`  | Job to inspect   |

**Returns:** `bool` — `true` when the Job has a `Complete=True` condition. Returns `false` for nil input.

### RunJob

```go
func RunJob(ctx context.Context, c client.Client, job *batchv1.Job, ownerRefs ...metav1.OwnerReference) error
```

Creates the given `batch/v1` Job. Idempotent — AlreadyExists is not an error. (CC-0005, REQ-006, REQ-009, REQ-010)

| Parameter   | Type                        | Description                                      |
|-------------|-----------------------------|--------------------------------------------------|
| `ctx`       | `context.Context`           | Context for the API call                         |
| `c`         | `client.Client`             | Kubernetes client                                |
| `job`       | `*batchv1.Job`              | Job to create                                    |
| `ownerRefs` | `...metav1.OwnerReference`  | Optional owner references for garbage collection |

**Returns:** `error` — nil on success or if already exists.

### EnsureCronJob

```go
func EnsureCronJob(ctx context.Context, c client.Client, cronJob *batchv1.CronJob, fieldManager string, ownerRefs ...metav1.OwnerReference) error
```

Uses server-side apply to create or update the given CronJob. (CC-0005, REQ-006, REQ-009, REQ-010)

| Parameter      | Type                        | Description                                      |
|----------------|-----------------------------|--------------------------------------------------|
| `ctx`          | `context.Context`           | Context for the API call                         |
| `c`            | `client.Client`             | Kubernetes client                                |
| `cronJob`      | `*batchv1.CronJob`          | CronJob to apply                                 |
| `fieldManager` | `string`                    | Field manager identifier for SSA                 |
| `ownerRefs`    | `...metav1.OwnerReference`  | Optional owner references for garbage collection |

**Returns:** `error` — nil on success.

---

## testutil/simulators (CC-0005 additions)

**Package:** `internal/common/testutil/simulators`
**Import:** `"github.com/c5c3/forge/internal/common/testutil/simulators"`

Test helpers that simulate operator reconciliation in envtest environments where third-party operators (cert-manager, external-secrets) are not running. CC-0005 adds two new simulators.

### SimulateCertificateReady

```go
func SimulateCertificateReady(ctx context.Context, c client.Client, name, namespace string) error
```

Creates a cert-manager Certificate CR (if it does not already exist) and patches its status sub-resource to set a `Ready` condition with status `True`. Simulates cert-manager operator behavior in envtest. (CC-0005, REQ-011)

| Parameter   | Type              | Description              |
|-------------|-------------------|--------------------------|
| `ctx`       | `context.Context` | Context for the API call |
| `c`         | `client.Client`   | Kubernetes client        |
| `name`      | `string`          | Certificate name         |
| `namespace` | `string`          | Certificate namespace    |

**Returns:** `error` — nil on success.

### SimulatePushSecretReady

```go
func SimulatePushSecretReady(ctx context.Context, c client.Client, name, namespace string) error
```

Creates an ESO PushSecret CR (if it does not already exist) and patches its status sub-resource to set a `Ready` condition with status `True`. Simulates external-secrets operator behavior in envtest. (CC-0005, REQ-011)

| Parameter   | Type              | Description              |
|-------------|-------------------|--------------------------|
| `ctx`       | `context.Context` | Context for the API call |
| `c`         | `client.Client`   | Kubernetes client        |
| `name`      | `string`          | PushSecret name          |
| `namespace` | `string`          | PushSecret namespace     |

**Returns:** `error` — nil on success.

---

## applyconfig

**Package:** `internal/common/applyconfig`
**Import:** `"github.com/c5c3/forge/internal/common/applyconfig"`

Internal utility for Kubernetes server-side apply operations. Centralises conversion of typed Kubernetes objects into `runtime.ApplyConfiguration` values so that resource packages (`deployment`, `job`) use consistent SSA behaviour.

### DefaultFieldManager

```go
const DefaultFieldManager = "cobaltcore-operator"
```

Recommended server-side apply field manager name for controllers using this module. Callers may use their own value for controller-specific ownership tracking. (CC-0005)

### ToApplyConfiguration

```go
func ToApplyConfiguration(obj k8sruntime.Object) (k8sruntime.ApplyConfiguration, error)
```

Converts a typed Kubernetes object into a `runtime.ApplyConfiguration` suitable for `client.Client.Apply()`. The object must have its GVK set before calling. (CC-0005)

| Parameter | Type                 | Description                          |
|-----------|----------------------|--------------------------------------|
| `obj`     | `k8sruntime.Object`  | Typed Kubernetes object with GVK set |

**Returns:** `(k8sruntime.ApplyConfiguration, error)` — the apply configuration and any conversion error.

---

## Design Decisions

- **Unstructured client for CRDs:** All third-party CRDs (MariaDB, cert-manager, external-secrets) are accessed via the unstructured client to avoid importing operator-specific Go types as dependencies.
- **Server-side apply for core resources:** Deployments, Services, and CronJobs use server-side apply (`client.Patch` with `client.Apply`) for declarative, conflict-free reconciliation.
- **Idempotent creates for one-shot resources:** Jobs, ConfigMaps, Databases, Users, Grants, Certificates, and PushSecrets use `client.Create` with `AlreadyExists` checks, since these resources are typically created once and not updated.
- **Owner references via variadic args:** All Ensure/Create functions accept optional `ownerRefs` for garbage collection, keeping the API flexible for both owned and cluster-scoped resources.

## Dependencies

- **CC-0004** — provides pure-function packages (`config`, `policy`, `plugins`, `conditions`, `types`)
- **CC-0005** — adds Kubernetes-client functions documented here
- **CC-0011** — will consume types in CRD definitions with kubebuilder markers
