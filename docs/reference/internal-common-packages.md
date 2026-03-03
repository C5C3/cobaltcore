---
title: Shared Utility Packages
quadrant: backend
---

# Shared Utility Packages

**Module:** `internal/common`
**Feature:** CC-0004, CC-0005

Shared utility packages for OpenStack operator reconcilers. CC-0004 provides pure-function utilities that operate on Go maps, slices, and structs with no Kubernetes client dependency (except `conditions/` which uses `metav1.Condition`). CC-0005 adds Kubernetes-interacting packages that use `controller-runtime`'s `client.Client` to create, read, and manage cluster resources.

## conditions

**Package:** `internal/common/conditions`
**Import:** `"github.com/c5c3/forge/internal/common/conditions"`

Helper functions for managing Kubernetes status conditions on `[]metav1.Condition`. Zero K8s client dependency — operates on in-memory slices only.

### SetCondition

```go
func SetCondition(conditions *[]metav1.Condition, conditionType string, status metav1.ConditionStatus, reason, message string)
```

Inserts or updates a condition by type. When the status is unchanged, `LastTransitionTime` is preserved to avoid spurious reconcile triggers.

| Parameter       | Type                      | Description                              |
|-----------------|---------------------------|------------------------------------------|
| `conditions`    | `*[]metav1.Condition`     | Pointer to the conditions slice to modify|
| `conditionType` | `string`                  | Condition type (e.g. `"Ready"`, `"DatabaseReady"`) |
| `status`        | `metav1.ConditionStatus`  | One of `metav1.ConditionTrue`, `ConditionFalse`, `ConditionUnknown` |
| `reason`        | `string`                  | CamelCase reason token                   |
| `message`       | `string`                  | Human-readable message                   |

**Behavior:** If a condition with the given type exists, its fields are updated in place. If the status changed, `LastTransitionTime` is set to the current time. If no matching condition exists, a new one is appended. A nil `conditions` pointer is a no-op.

### GetCondition

```go
func GetCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition
```

Returns a pointer to the condition with the given type, or `nil` if not found. Safe to call with nil or empty slices.

| Parameter       | Type                  | Description                     |
|-----------------|-----------------------|---------------------------------|
| `conditions`    | `[]metav1.Condition`  | Conditions slice to search      |
| `conditionType` | `string`              | Condition type to find          |

**Returns:** `*metav1.Condition` — pointer into the original slice, or `nil`.

### IsReady

```go
func IsReady(conditions []metav1.Condition) bool
```

Returns `true` if a condition with type `"Ready"` exists and has status `True`. Shorthand for checking the aggregate readiness gate.

| Parameter    | Type                  | Description                |
|--------------|-----------------------|----------------------------|
| `conditions` | `[]metav1.Condition`  | Conditions slice to check  |

**Returns:** `bool` — `true` only when `Ready` condition has status `True`.

### AllTrue

```go
func AllTrue(conditions []metav1.Condition, types ...string) bool
```

Returns `true` if every named condition type has status `True`. Returns `true` (vacuously) when no types are provided. Useful for gating the aggregate `Ready` condition on multiple sub-conditions.

| Parameter    | Type                  | Description                           |
|--------------|-----------------------|---------------------------------------|
| `conditions` | `[]metav1.Condition`  | Conditions slice to check             |
| `types`      | `...string`           | Condition types that must all be True |

**Returns:** `bool` — `false` if any named type is missing or not `True`.

---

## config

**Package:** `internal/common/config`
**Import:** `"github.com/c5c3/forge/internal/common/config"`

INI configuration rendering pipeline for OpenStack services. All functions are pure — they return new maps without mutating inputs. No Kubernetes package imports.

### RenderINI

```go
func RenderINI(config map[string]map[string]string) string
```

Produces a deterministic INI-format string from a nested config map. Sections and keys are sorted alphabetically to ensure stable ConfigMap content hashes.

| Parameter | Type                            | Description                        |
|-----------|---------------------------------|------------------------------------|
| `config`  | `map[string]map[string]string`  | Nested map of section → key → value|

**Returns:** `string` — valid INI text with `[section]` headers and `key = value` lines. Empty map returns `""`.

**Example output:**

```ini
[DEFAULT]
debug = false

[database]
connection = mysql+pymysql://user:pass@host:3306/db
```

### MergeDefaults

```go
func MergeDefaults(userConfig, defaults map[string]map[string]string) map[string]map[string]string
```

Merges defaults into user config with user-wins precedence. User values always override defaults; sections and keys from defaults fill gaps.

| Parameter    | Type                            | Description                 |
|--------------|---------------------------------|-----------------------------|
| `userConfig` | `map[string]map[string]string`  | User-supplied configuration |
| `defaults`   | `map[string]map[string]string`  | Operator default values     |

**Returns:** `map[string]map[string]string` — new merged map.

### InjectSecrets

```go
func InjectSecrets(config map[string]map[string]string, secrets map[string]string) map[string]map[string]string
```

Assembles connection strings from resolved secret values and injects them into the config. Secret keys follow the `"section/key"` pattern.

| Parameter | Type                            | Description                                    |
|-----------|---------------------------------|------------------------------------------------|
| `config`  | `map[string]map[string]string`  | Current configuration to inject into           |
| `secrets` | `map[string]string`             | Resolved secret values keyed by `section/key`  |

**Returns:** `map[string]map[string]string` — new config with secrets injected. Empty secrets are a no-op.

**Database connection assembly:** When individual secret parts (`database/user`, `database/password`, `database/host`, and optionally `database/port`, `database/name`) are present, they are assembled into a connection string:

```
mysql+pymysql://{user}:{password}@{host}:{port}/{database}
```

Special characters in the password (`@`, `:`, `/`) are URL-encoded. A pre-assembled `database/connection` value takes priority over individual parts.

### InjectOsloPolicyConfig

```go
func InjectOsloPolicyConfig(config map[string]map[string]string, policyFilePath string) map[string]map[string]string
```

Adds the `[oslo_policy]` section pointing to the policy file. No-op when `policyFilePath` is empty.

| Parameter        | Type                            | Description                               |
|------------------|---------------------------------|-------------------------------------------|
| `config`         | `map[string]map[string]string`  | Current configuration                     |
| `policyFilePath` | `string`                        | Path to the policy.yaml file on the pod   |

**Returns:** `map[string]map[string]string` — new config with `oslo_policy.policy_file` set (or unchanged if path is empty).

---

## plugins

**Package:** `internal/common/plugins`
**Import:** `"github.com/c5c3/forge/internal/common/plugins"`

PasteDeploy pipeline configuration generation for OpenStack api-paste.ini files. Depends on `internal/common/types`.

### RenderPastePipeline

```go
func RenderPastePipeline(spec types.PipelineSpec) string
```

Generates a PasteDeploy api-paste.ini configuration from a `PipelineSpec`. Renders the `[pipeline:name]` section with middleware inserted at their specified positions, followed by sorted `[filter:name]` blocks.

| Parameter | Type                 | Description                    |
|-----------|----------------------|--------------------------------|
| `spec`    | `types.PipelineSpec` | Pipeline specification         |

**Returns:** `string` — valid api-paste.ini text.

**Middleware insertion:** Each middleware's `Position` determines where its name is inserted in the base pipeline string:

- `Position.After = "tokenX"` — inserted immediately after `tokenX`
- `Position.Before = "tokenY"` — inserted immediately before `tokenY`
- Both empty — appended to the end of the pipeline

**Example output:**

```ini
[pipeline:keystone]
pipeline = cors request_id token_auth admin_service

[filter:cors]
paste.filter_factory = oslo_middleware.cors:filter_factory

[filter:request_id]
paste.filter_factory = oslo_middleware:RequestId

[filter:token_auth]
paste.filter_factory = keystonemiddleware.auth_token:filter_factory
auth_type = password
```

### RenderPluginConfig

```go
func RenderPluginConfig(plugins []types.PluginSpec) map[string]map[string]string
```

Converts a slice of `PluginSpec` into a config map suitable for merging with the main INI configuration via `config.MergeDefaults`.

| Parameter | Type                | Description                        |
|-----------|---------------------|------------------------------------|
| `plugins` | `[]types.PluginSpec`| Plugin specifications to convert   |

**Returns:** `map[string]map[string]string` — one section per plugin, keyed by the plugin's `Section` field. Empty slice returns an empty map.

---

## policy

**Package:** `internal/common/policy`
**Import:** `"github.com/c5c3/forge/internal/common/policy"`

oslo.policy rule merging, validation, and YAML rendering. Depends on `gopkg.in/yaml.v3` and `k8s.io/apimachinery/pkg/util/validation/field`.

### MergePolicies

```go
func MergePolicies(external, inline map[string]string) map[string]string
```

Merges inline policy rules over external rules with inline-wins precedence. Returns a new map without mutating inputs.

| Parameter  | Type                | Description                          |
|------------|---------------------|--------------------------------------|
| `external` | `map[string]string` | Rules from external ConfigMap source |
| `inline`   | `map[string]string` | Inline rule overrides from CRD spec  |

**Returns:** `map[string]string` — merged rules. Nil inputs are treated as empty.

### ValidatePolicyRules

```go
func ValidatePolicyRules(rules map[string]string, fldPath *field.Path) field.ErrorList
```

Validates policy rules for webhook compatibility. Returns `field.ErrorList` (Decision #3) with errors for empty keys and empty values. Error ordering is deterministic (sorted by key).

| Parameter | Type          | Description                                   |
|-----------|---------------|-----------------------------------------------|
| `rules`   | `map[string]string` | Policy rules to validate                |
| `fldPath` | `*field.Path` | Field path for error reporting in webhooks    |

**Returns:** `field.ErrorList` — empty list for valid input; one error per violation.

**Validation rules:**

- Empty key → `field.Invalid` error
- Empty value → `field.Invalid` error

### RenderPolicyYAML

```go
func RenderPolicyYAML(rules map[string]string) (string, error)
```

Renders policy rules as a YAML document suitable for oslo.policy. Output is deterministic (keys sorted alphabetically).

| Parameter | Type                | Description              |
|-----------|---------------------|--------------------------|
| `rules`   | `map[string]string` | Policy rules to render   |

**Returns:** `(string, error)` — YAML text and any marshaling error. Empty map produces `"{}\n"`.

---

## Config Pipeline Usage

The typical reconciler config pipeline chains these functions:

```
CRD Spec Fields ──→ MergeDefaults ──→ InjectSecrets ──→ InjectOsloPolicyConfig ──→ RenderINI ──→ ConfigMap
                         ↑                  ↑                    ↑
                    Operator Defaults   Resolved Secrets    Policy File Path

CRD Spec Fields ──→ RenderPluginConfig ──→ MergeDefaults (merged into main config)
CRD Spec Fields ──→ RenderPastePipeline ──→ api-paste.ini ConfigMap
CRD Spec Fields ──→ MergePolicies ──→ ValidatePolicyRules ──→ RenderPolicyYAML ──→ policy.yaml ConfigMap
```

---

## config (K8s-interacting)

**Package:** `internal/common/config`
**Import:** `"github.com/c5c3/forge/internal/common/config"`

Kubernetes-interacting functions added by CC-0005 to the existing config package. These functions require a `controller-runtime` `client.Client`.

### CreateImmutableConfigMap

```go
func CreateImmutableConfigMap(
    ctx context.Context,
    c client.Client,
    name, namespace string,
    data map[string]string,
    owners ...metav1.OwnerReference,
) (*corev1.ConfigMap, error)
```

Creates an immutable Kubernetes ConfigMap whose name includes a content-hash suffix derived from the data. The name is formatted as `{name}-{hash}` where hash is the first 8 hex characters of the SHA-256 digest of the sorted, serialised data content.

| Parameter   | Type                       | Description                                          |
|-------------|----------------------------|------------------------------------------------------|
| `ctx`       | `context.Context`          | Context for the Kubernetes API call                  |
| `c`         | `client.Client`            | controller-runtime Kubernetes client                 |
| `name`      | `string`                   | Base name prefix for the ConfigMap                   |
| `namespace` | `string`                   | Kubernetes namespace                                 |
| `data`      | `map[string]string`        | ConfigMap data entries                               |
| `owners`    | `...metav1.OwnerReference` | Variadic owner references for garbage collection     |

**Returns:** `(*corev1.ConfigMap, error)` -- the created or existing ConfigMap, or an error.

**Behavior:** The function is idempotent. If a ConfigMap with the same hashed name already exists, it is returned without error. Owner references are set from the variadic `owners` parameter so the ConfigMap is garbage-collected when the owning resource is deleted. The ConfigMap is created with `Immutable: true`.

---

## secrets

**Package:** `internal/common/secrets`
**Import:** `"github.com/c5c3/forge/internal/common/secrets"`

Functions for inspecting Kubernetes Secrets and managing external-secrets.io custom resources. All functions require a `controller-runtime` `client.Client`.

### IsExternalSecretReady

```go
func IsExternalSecretReady(ctx context.Context, c client.Client, name, namespace string) (bool, error)
```

Fetches an ExternalSecret CR (`external-secrets.io/v1beta1`) by name and namespace and inspects its `status.conditions` for a `Ready=True` condition.

| Parameter   | Type              | Description                          |
|-------------|-------------------|--------------------------------------|
| `ctx`       | `context.Context` | Context for the Kubernetes API call  |
| `c`         | `client.Client`   | controller-runtime Kubernetes client |
| `name`      | `string`          | Name of the ExternalSecret           |
| `namespace` | `string`          | Kubernetes namespace                 |

**Returns:** `(bool, error)` -- `(true, nil)` if the ExternalSecret exists and has a `Ready=True` condition, `(false, nil)` if it does not exist or is not yet ready, `(false, err)` if the API call fails for any other reason.

### IsSecretReady

```go
func IsSecretReady(ctx context.Context, c client.Client, name, namespace string) (bool, error)
```

Checks whether a `corev1.Secret` exists and contains at least one data key.

| Parameter   | Type              | Description                          |
|-------------|-------------------|--------------------------------------|
| `ctx`       | `context.Context` | Context for the Kubernetes API call  |
| `c`         | `client.Client`   | controller-runtime Kubernetes client |
| `name`      | `string`          | Name of the Secret                   |
| `namespace` | `string`          | Kubernetes namespace                 |

**Returns:** `(bool, error)` -- `(true, nil)` if the Secret exists and has data, `(false, nil)` if it does not exist or exists but has no data keys, `(false, err)` if the API call fails for any other reason.

### GetSecretValue

```go
func GetSecretValue(ctx context.Context, c client.Client, name, namespace, key string) (string, error)
```

Fetches a `corev1.Secret` and returns the string value of the specified key from its `Data` field.

| Parameter   | Type              | Description                          |
|-------------|-------------------|--------------------------------------|
| `ctx`       | `context.Context` | Context for the Kubernetes API call  |
| `c`         | `client.Client`   | controller-runtime Kubernetes client |
| `name`      | `string`          | Name of the Secret                   |
| `namespace` | `string`          | Kubernetes namespace                 |
| `key`       | `string`          | Data key to retrieve                 |

**Returns:** `(string, error)` -- the string value of the requested key, or an error if the Secret does not exist or the key is not present.

### EnsurePushSecret

```go
func EnsurePushSecret(ctx context.Context, c client.Client, name, namespace, secretStoreName, remoteKey string, owners ...metav1.OwnerReference) error
```

Creates an `external-secrets.io/v1alpha1` PushSecret custom resource that pushes the contents of the named Kubernetes Secret to a remote secret store via the specified ClusterSecretStore.

| Parameter         | Type                       | Description                                            |
|-------------------|----------------------------|--------------------------------------------------------|
| `ctx`             | `context.Context`          | Context for the Kubernetes API call                    |
| `c`               | `client.Client`            | controller-runtime Kubernetes client                   |
| `name`            | `string`                   | Name of the PushSecret (and source Secret)             |
| `namespace`       | `string`                   | Kubernetes namespace                                   |
| `secretStoreName` | `string`                   | Name of the ClusterSecretStore to push to              |
| `remoteKey`       | `string`                   | Remote key path in the secret store                    |
| `owners`          | `...metav1.OwnerReference` | Variadic owner references for garbage collection       |

**Returns:** `error` -- `nil` on success or if the PushSecret already exists (idempotent); an error if the API call fails.

**Behavior:** The PushSecret spec references `spec.secretStoreRefs` pointing to the named ClusterSecretStore, `spec.selector.secret.name` set to the source Secret name, and `spec.data` mapping all keys to the given `remoteKey`. Owner references are set from the variadic `owners` parameter.

---

## policy (K8s-interacting)

**Package:** `internal/common/policy`
**Import:** `"github.com/c5c3/forge/internal/common/policy"`

Kubernetes-interacting function added by CC-0005 to the existing policy package. Requires a `controller-runtime` `client.Client`.

### LoadPolicyFromConfigMap

```go
func LoadPolicyFromConfigMap(ctx context.Context, c client.Client, name, namespace string) (map[string]string, error)
```

Fetches a ConfigMap by name and namespace, reads the `"policy.yaml"` key from its data, and parses the YAML content into a flat `map[string]string` representing oslo.policy rules.

| Parameter   | Type              | Description                          |
|-------------|-------------------|--------------------------------------|
| `ctx`       | `context.Context` | Context for the Kubernetes API call  |
| `c`         | `client.Client`   | controller-runtime Kubernetes client |
| `name`      | `string`          | Name of the ConfigMap                |
| `namespace` | `string`          | Kubernetes namespace                 |

**Returns:** `(map[string]string, error)` -- the parsed policy rules, or an error if the ConfigMap is not found, the `"policy.yaml"` key is missing, or the YAML content cannot be parsed.

---

## database

**Package:** `internal/common/database`
**Import:** `"github.com/c5c3/forge/internal/common/database"`

Functions for managing MariaDB databases, users, and schema migration jobs via the mariadb-operator CRDs (`k8s.mariadb.com/v1alpha1`). All functions require a `controller-runtime` `client.Client`.

### EnsureDatabase

```go
func EnsureDatabase(ctx context.Context, c client.Client, name, namespace, mariadbRef string, owners ...metav1.OwnerReference) error
```

Creates a `k8s.mariadb.com/v1alpha1` Database custom resource using an unstructured object. The Database's `spec.mariaDbRef.name` is set to `mariadbRef`, and `spec.name` is set to the given name.

| Parameter    | Type                       | Description                                          |
|--------------|----------------------------|------------------------------------------------------|
| `ctx`        | `context.Context`          | Context for the Kubernetes API call                  |
| `c`          | `client.Client`            | controller-runtime Kubernetes client                 |
| `name`       | `string`                   | Name of the Database resource (also the database name)|
| `namespace`  | `string`                   | Kubernetes namespace                                 |
| `mariadbRef` | `string`                   | Name of the MariaDB instance to reference            |
| `owners`     | `...metav1.OwnerReference` | Variadic owner references for garbage collection     |

**Returns:** `error` -- `nil` on success or if the Database already exists (idempotent); an error if the API call fails.

### EnsureDatabaseUser

```go
func EnsureDatabaseUser(ctx context.Context, c client.Client, name, namespace, mariadbRef, passwordSecretName, passwordSecretKey, databaseName string, owners ...metav1.OwnerReference) error
```

Creates a `k8s.mariadb.com/v1alpha1` User custom resource and a corresponding Grant custom resource using unstructured objects. The User is configured with the given `mariadbRef` and password secret reference. The Grant gives the user ALL privileges scoped to the specified `databaseName` and all tables within it (`databaseName.*`).

| Parameter            | Type                       | Description                                            |
|----------------------|----------------------------|--------------------------------------------------------|
| `ctx`                | `context.Context`          | Context for the Kubernetes API call                    |
| `c`                  | `client.Client`            | controller-runtime Kubernetes client                   |
| `name`               | `string`                   | Name of the User resource (also the database username) |
| `namespace`          | `string`                   | Kubernetes namespace                                   |
| `mariadbRef`         | `string`                   | Name of the MariaDB instance to reference              |
| `passwordSecretName` | `string`                   | Name of the Secret containing the password             |
| `passwordSecretKey`  | `string`                   | Key within the Secret that holds the password          |
| `databaseName`       | `string`                   | Database name to scope the Grant to (privilege separation) |
| `owners`             | `...metav1.OwnerReference` | Variadic owner references for garbage collection       |

**Returns:** `error` -- `nil` on success or if the resources already exist (idempotent); an error if the API call fails.

**Behavior:** Internally delegates to two functions: `ensureUser` creates the User CR, and `ensureGrant` creates the Grant CR named `{name}-grant` scoped to the specified database. Both operations are idempotent.

### RunDBSyncJob

```go
func RunDBSyncJob(ctx context.Context, c client.Client, name, namespace, image string, command []string, volumeMounts []corev1.VolumeMount, volumes []corev1.Volume, owners ...metav1.OwnerReference) error
```

Creates a Kubernetes Job that runs a database sync/migration command.

| Parameter      | Type                       | Description                                          |
|----------------|----------------------------|------------------------------------------------------|
| `ctx`          | `context.Context`          | Context for the Kubernetes API call                  |
| `c`            | `client.Client`            | controller-runtime Kubernetes client                 |
| `name`         | `string`                   | Name of the Job                                      |
| `namespace`    | `string`                   | Kubernetes namespace                                 |
| `image`        | `string`                   | Container image for the db-sync container            |
| `command`      | `[]string`                 | Command to execute in the container                  |
| `volumeMounts` | `[]corev1.VolumeMount`     | Volume mounts for the db-sync container              |
| `volumes`      | `[]corev1.Volume`          | Volumes for the Pod                                  |
| `owners`       | `...metav1.OwnerReference` | Variadic owner references for garbage collection     |

**Returns:** `error` -- `nil` on success or if the Job already exists (idempotent); an error if the API call fails.

**Behavior:** The Job is configured with a backoff limit of 4 retries, a TTL of 300 seconds after completion, and a restart policy of `Never`. The container is named `"db-sync"`. Owner references are set from the variadic `owners` parameter.

---

## deployment

**Package:** `internal/common/deployment`
**Import:** `"github.com/c5c3/forge/internal/common/deployment"`

Functions for managing Kubernetes Deployments and Services. All functions require a `controller-runtime` `client.Client` (except `IsDeploymentReady` which is a pure function).

### EnsureDeployment

```go
func EnsureDeployment(ctx context.Context, c client.Client, deployment *appsv1.Deployment) error
```

Creates the given Deployment if it does not exist, or updates it if it already exists. The caller supplies a fully-constructed Deployment object.

| Parameter    | Type                 | Description                              |
|--------------|----------------------|------------------------------------------|
| `ctx`        | `context.Context`    | Context for the Kubernetes API call      |
| `c`          | `client.Client`      | controller-runtime Kubernetes client     |
| `deployment` | `*appsv1.Deployment` | Fully-constructed Deployment to ensure   |

**Returns:** `error` -- `nil` on success; an error if the API call fails.

**Behavior:** On update, the `ResourceVersion` from the existing object is preserved for optimistic concurrency. If the Deployment does not exist, it is created; otherwise the existing resource is updated in place.

### EnsureService

```go
func EnsureService(ctx context.Context, c client.Client, service *corev1.Service) error
```

Creates the given Service if it does not exist, or updates it if it already exists. The caller supplies a fully-constructed Service object.

| Parameter | Type              | Description                           |
|-----------|-------------------|---------------------------------------|
| `ctx`     | `context.Context` | Context for the Kubernetes API call   |
| `c`       | `client.Client`   | controller-runtime Kubernetes client  |
| `service` | `*corev1.Service` | Fully-constructed Service to ensure   |

**Returns:** `error` -- `nil` on success; an error if the API call fails.

**Behavior:** On update, the `ResourceVersion` from the existing object is preserved for optimistic concurrency. If the caller specifies a non-empty `ClusterIP` that differs from the existing one, the update is rejected with an error because `ClusterIP` is immutable once assigned by Kubernetes. If the caller's `ClusterIP` is empty, the existing `ClusterIP` is preserved. If the Service does not exist, it is created; otherwise the existing resource is updated in place.

### IsDeploymentReady

```go
func IsDeploymentReady(deployment *appsv1.Deployment) bool
```

Returns `true` if the given Deployment has an `Available` condition with status `True`. This indicates that the Deployment has reached its minimum availability as defined by its deployment strategy.

| Parameter    | Type                 | Description                  |
|--------------|----------------------|------------------------------|
| `deployment` | `*appsv1.Deployment` | Deployment to check          |

**Returns:** `bool` -- `true` if the `Available` condition has status `True`; `false` otherwise (including when the deployment is `nil`).

---

## job

**Package:** `internal/common/job`
**Import:** `"github.com/c5c3/forge/internal/common/job"`

Functions for managing Kubernetes Jobs and CronJobs. All functions require a `controller-runtime` `client.Client` (except `IsJobComplete` which is a pure function).

### RunJob

```go
func RunJob(ctx context.Context, c client.Client, job *batchv1.Job) error
```

Creates the given Job if it does not already exist. Jobs are immutable once created, so an `AlreadyExists` error is treated as success.

| Parameter | Type              | Description                          |
|-----------|-------------------|--------------------------------------|
| `ctx`     | `context.Context` | Context for the Kubernetes API call  |
| `c`       | `client.Client`   | controller-runtime Kubernetes client |
| `job`     | `*batchv1.Job`    | Fully-constructed Job to create      |

**Returns:** `error` -- `nil` on success or if the Job already exists (idempotent); an error if the API call fails.

### EnsureCronJob

```go
func EnsureCronJob(ctx context.Context, c client.Client, cronJob *batchv1.CronJob) error
```

Creates or updates the given CronJob. If the CronJob does not exist it is created; otherwise the existing resource is updated in place.

| Parameter | Type               | Description                            |
|-----------|--------------------|----------------------------------------|
| `ctx`     | `context.Context`  | Context for the Kubernetes API call    |
| `c`       | `client.Client`    | controller-runtime Kubernetes client   |
| `cronJob` | `*batchv1.CronJob` | Fully-constructed CronJob to ensure    |

**Returns:** `error` -- `nil` on success; an error if the API call fails.

**Behavior:** On update, the `ResourceVersion` from the existing object is preserved for optimistic concurrency. If the CronJob does not exist, it is created; otherwise the existing resource is updated in place.

### IsJobComplete

```go
func IsJobComplete(j *batchv1.Job) bool
```

Returns `true` if the given Job has a `Complete` condition with status `True`. This indicates the Job has successfully finished all of its work.

| Parameter | Type           | Description     |
|-----------|----------------|-----------------|
| `j`       | `*batchv1.Job` | Job to check    |

**Returns:** `bool` -- `true` if the `Complete` condition has status `True`; `false` otherwise (including when the Job is `nil`).

---

## tls

**Package:** `internal/common/tls`
**Import:** `"github.com/c5c3/forge/internal/common/tls"`

Functions for managing TLS certificates via cert-manager and retrieving TLS secrets. All functions require a `controller-runtime` `client.Client`.

### EnsureCertificate

```go
func EnsureCertificate(ctx context.Context, c client.Client, name, namespace, issuerName, commonName string, dnsNames []string, owners ...metav1.OwnerReference) error
```

Creates a `cert-manager.io/v1` Certificate custom resource using an unstructured object. The Certificate's `spec.secretName` is set to `{name}-tls`, and `spec.issuerRef` points to a ClusterIssuer with the given `issuerName`.

| Parameter    | Type                       | Description                                      |
|--------------|----------------------------|--------------------------------------------------|
| `ctx`        | `context.Context`          | Context for the Kubernetes API call              |
| `c`          | `client.Client`            | controller-runtime Kubernetes client             |
| `name`       | `string`                   | Name of the Certificate resource                 |
| `namespace`  | `string`                   | Kubernetes namespace                             |
| `issuerName` | `string`                   | Name of the ClusterIssuer to reference           |
| `commonName` | `string`                   | Common name (CN) for the certificate             |
| `dnsNames`   | `[]string`                 | Subject Alternative Names (SANs)                 |
| `owners`     | `...metav1.OwnerReference` | Variadic owner references for garbage collection |

**Returns:** `error` -- `nil` on success or if the Certificate already exists (idempotent); an error if the API call fails.

**Behavior:** The resulting TLS secret will be named `{name}-tls`. Owner references are set from the variadic `owners` parameter. The `spec.issuerRef.kind` is set to `ClusterIssuer`.

### GetTLSSecret

```go
func GetTLSSecret(ctx context.Context, c client.Client, name, namespace string) (certPEM []byte, keyPEM []byte, err error)
```

Fetches a `corev1.Secret` by name and namespace, validates that it contains both `"tls.crt"` and `"tls.key"` data keys, and returns the raw certificate and key bytes directly.

| Parameter   | Type              | Description                          |
|-------------|-------------------|--------------------------------------|
| `ctx`       | `context.Context` | Context for the Kubernetes API call  |
| `c`         | `client.Client`   | controller-runtime Kubernetes client |
| `name`      | `string`          | Name of the TLS Secret               |
| `namespace` | `string`          | Kubernetes namespace                 |

**Returns:** `([]byte, []byte, error)` -- the certificate PEM bytes, key PEM bytes, and an error if the Secret is not found or is missing required TLS keys (`"tls.crt"` or `"tls.key"`).

---

## testutil/simulators

**Package:** `internal/common/testutil/simulators`
**Import:** `"github.com/c5c3/forge/internal/common/testutil/simulators"`

Test helpers that simulate the behaviour of external operators (cert-manager, external-secrets, mariadb-operator, memcached-operator) and the Kubernetes job controller in envtest environments where these controllers are not running. Each simulator creates the custom resource if it does not exist and patches its status sub-resource to reflect a ready/complete state.

### SimulateCertificateReady

```go
func SimulateCertificateReady(ctx context.Context, c client.Client, name, namespace string) error
```

Creates a cert-manager Certificate custom resource (if it does not already exist) and patches its status sub-resource so that `ready=true` and a `Ready` condition with status `True` is present.

| Parameter   | Type              | Description                           |
|-------------|-------------------|---------------------------------------|
| `ctx`       | `context.Context` | Context for the Kubernetes API call   |
| `c`         | `client.Client`   | controller-runtime Kubernetes client  |
| `name`      | `string`          | Name of the Certificate               |
| `namespace` | `string`          | Kubernetes namespace                  |

**Returns:** `error` -- `nil` on success; an error if the API call fails.

### SimulatePushSecretReady

```go
func SimulatePushSecretReady(ctx context.Context, c client.Client, name, namespace string) error
```

Creates an external-secrets PushSecret custom resource (if it does not already exist) and patches its status sub-resource so that `ready=true` and a `Ready` condition with status `True` is present.

| Parameter   | Type              | Description                           |
|-------------|-------------------|---------------------------------------|
| `ctx`       | `context.Context` | Context for the Kubernetes API call   |
| `c`         | `client.Client`   | controller-runtime Kubernetes client  |
| `name`      | `string`          | Name of the PushSecret                |
| `namespace` | `string`          | Kubernetes namespace                  |

**Returns:** `error` -- `nil` on success; an error if the API call fails.

### SimulateExternalSecretSync

```go
func SimulateExternalSecretSync(ctx context.Context, c client.Client, name, namespace string, targetSecretData map[string][]byte) error
```

Creates an ExternalSecret custom resource (if it does not already exist), patches its status sub-resource to reflect a successful sync (`Ready=True` condition), and creates the target Kubernetes Secret populated with `targetSecretData`.

| Parameter          | Type                 | Description                                     |
|--------------------|----------------------|-------------------------------------------------|
| `ctx`              | `context.Context`    | Context for the Kubernetes API call             |
| `c`                | `client.Client`      | controller-runtime Kubernetes client            |
| `name`             | `string`             | Name of the ExternalSecret (and target Secret)  |
| `namespace`        | `string`             | Kubernetes namespace                            |
| `targetSecretData` | `map[string][]byte`  | Data to populate in the target Secret           |

**Returns:** `error` -- `nil` on success; an error if the API call fails.

**Behavior:** In a real cluster, the external-secrets operator watches ExternalSecret objects and creates the target Secret automatically. In envtest the operator is absent, so this simulator performs both actions. The function is idempotent -- if the target Secret already exists, its data is updated to match `targetSecretData`.

### SimulateMariaDBReady

```go
func SimulateMariaDBReady(ctx context.Context, c client.Client, name, namespace string) error
```

Creates a MariaDB custom resource (if it does not already exist) and patches its status sub-resource so that `ready=true` and a `Ready` condition with status `True` is present.

| Parameter   | Type              | Description                           |
|-------------|-------------------|---------------------------------------|
| `ctx`       | `context.Context` | Context for the Kubernetes API call   |
| `c`         | `client.Client`   | controller-runtime Kubernetes client  |
| `name`      | `string`          | Name of the MariaDB resource          |
| `namespace` | `string`          | Kubernetes namespace                  |

**Returns:** `error` -- `nil` on success; an error if the API call fails.

### SimulateMemcachedReady

```go
func SimulateMemcachedReady(ctx context.Context, c client.Client, name, namespace string) error
```

Creates a Memcached custom resource (if it does not already exist) and patches its status sub-resource so that `ready=true` and a `Ready` condition with status `True` is present.

| Parameter   | Type              | Description                           |
|-------------|-------------------|---------------------------------------|
| `ctx`       | `context.Context` | Context for the Kubernetes API call   |
| `c`         | `client.Client`   | controller-runtime Kubernetes client  |
| `name`      | `string`          | Name of the Memcached resource        |
| `namespace` | `string`          | Kubernetes namespace                  |

**Returns:** `error` -- `nil` on success; an error if the API call fails.

**Note:** The API group `opsv1.memcached.com` is a fabricated placeholder for testing purposes.

### SimulateJobComplete

```go
func SimulateJobComplete(ctx context.Context, c client.Client, name, namespace string) error
```

Creates a Kubernetes Job (if it does not already exist) and patches its status sub-resource to reflect a successful completion, setting `succeeded=1` and a `Complete` condition with status `True`.

| Parameter   | Type              | Description                           |
|-------------|-------------------|---------------------------------------|
| `ctx`       | `context.Context` | Context for the Kubernetes API call   |
| `c`         | `client.Client`   | controller-runtime Kubernetes client  |
| `name`      | `string`          | Name of the Job                       |
| `namespace` | `string`          | Kubernetes namespace                  |

**Returns:** `error` -- `nil` on success; an error if the API call fails.

**Behavior:** Simulates the Kubernetes job controller in envtest environments where no Pods are actually scheduled and therefore Jobs never transition to a completed state on their own. The Job status includes `startTime`, `completionTime`, and conditions for both `SuccessCriteriaMet` and `Complete`.

---

## Dependencies

- **CC-0004** -- implements pure-function utility packages (conditions, config, plugins, policy)
- **CC-0005** -- adds K8s-client-dependent packages (config/configmap, secrets, policy/loader, database, deployment, job, tls, testutil/simulators)
- **CC-0011** -- will consume types in CRD definitions with kubebuilder markers
