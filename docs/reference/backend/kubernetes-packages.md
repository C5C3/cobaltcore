---
title: Kubernetes-Interacting Packages
quadrant: shared-library
---

# Kubernetes-Interacting Packages

Reference documentation for the `internal/common/` packages that interact with the
Kubernetes API server. These packages provide reconciler building blocks for
managing external operator CRDs (MariaDB, External Secrets Operator, cert-manager)
and Kubernetes resources such as Deployments, Services, Jobs, CronJobs, ConfigMaps,
Secrets, and cert-manager Certificates.

Packages that use `apply.EnsureObject` share these conventions:

- **Server-Side Apply:** The helper applies desired fields under the stable field
  manager `cobaltcore-operator`. It owns only fields the builder sets, so omitted
  server defaults are not claimed or overwritten. A converged object is applied
  without a write. The helper retries field-manager conflicts internally with
  `retry.RetryOnConflict`.
- **Ownership claims:** `apply.EnsureObject` sets a controller owner reference
  for local children and re-enforces it on each apply. A target-cluster child
  carries the three ownership labels because that cluster cannot resolve an
  owner from the management cluster. The remote-child finalizer uses those
  labels to find children during deletion.
- **Readiness reporting:** Helpers that gate reconciliation on a resource's readiness
  return `(bool, error)`: `true` means it is ready, and `false` means it exists but is
  not yet ready. Helpers that only apply or create an object return an `error`, or the
  created object's name and an `error` when callers need that name.
- **Error wrapping:** Functions that add context to an error use `fmt.Errorf` with
  `%w`, preserving `errors.Is` and `errors.As` checks.

## Import Paths

| Package | Import Path |
| --- | --- |
| `apply` | `github.com/c5c3/cobaltcore/internal/common/apply` |
| `config` | `github.com/c5c3/cobaltcore/internal/common/config` |
| `database` | `github.com/c5c3/cobaltcore/internal/common/database` |
| `deployment` | `github.com/c5c3/cobaltcore/internal/common/deployment` |
| `job` | `github.com/c5c3/cobaltcore/internal/common/job` |
| `multicluster` | `github.com/c5c3/cobaltcore/internal/common/multicluster` |
| `policy` | `github.com/c5c3/cobaltcore/internal/common/policy` |
| `satellite` | `github.com/c5c3/cobaltcore/internal/common/satellite` |
| `secrets` | `github.com/c5c3/cobaltcore/internal/common/secrets` |
| `tls` | `github.com/c5c3/cobaltcore/internal/common/tls` |

## External CRD Dependencies

These packages import typed Go structs from external operator modules:

| Operator | Go Module | API Version | Types Used |
| --- | --- | --- | --- |
| mariadb-operator | `github.com/mariadb-operator/mariadb-operator/v26` | `v1alpha1` | `Database`, `User`, `Grant` |
| External Secrets Operator | `github.com/external-secrets/external-secrets/apis` | `v1` (ExternalSecret, ClusterSecretStore), `v1alpha1` (PushSecret) | `ExternalSecret`, `PushSecret` |
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

Creates or updates `obj` via Server-Side Apply under `fieldManager` and claims it for
`owner`: a controller reference for a local child, or ownership labels for a child on a
target cluster.

**Behavior:**

- Claims the object through `multicluster.Claim` and stamps its GVK (objects built
  in-code carry an empty `TypeMeta`, which Server-Side Apply requires). A same-named
  foreign object on a target cluster is refused when it carries neither the owner
  labels nor the owner's controller reference.
- Applies the object with `client.FieldOwner(fieldManager)` and `client.ForceOwnership`,
  wrapped in `retry.RetryOnConflict`. This retries benign field-manager conflicts
  internally and avoids surfacing them as condition noise.
- The field manager owns only the fields the builder sets, so server-defaulted fields the
  builder omits are never claimed and a converged object is applied without a write.
- Decodes the server response back into `obj`, so callers may read fresh status (e.g.
  readiness) without an extra `Get`.

**Field manager:** the package exports `apply.FieldManager` (`"cobaltcore-operator"`), the
stable field-manager name used by helpers that call `apply.EnsureObject`.

---

## Package: `config`

Implements the INI configuration rendering pipeline for CobaltCore OpenStack
operators. The `CreateImmutableConfigMap` function provides
Kubernetes-interacting config management.

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
| `scheme` | `*runtime.Scheme` | Scheme for GVK and ownership-label resolution |
| `owner` | `client.Object` | Owning CR; local children use a controller reference and target-cluster children use ownership labels |
| `baseName` | `string` | Base name for the ConfigMap (hash is appended as `-<hash>`) |
| `namespace` | `string` | Namespace for the ConfigMap |
| `data` | `map[string]string` | ConfigMap data entries |

**Returns:**

| Value | Description |
| --- | --- |
| `string` | Actual ConfigMap name including the 8-character SHA256 hash suffix |
| `error` | Non-nil on ownership claim/refusal or API server failure |

**Behavior:**

- Computes a deterministic SHA256 hash from sorted `data` keys and values.
- Truncates the hash to 8 hex characters and appends it as `baseName-<hash>`.
- Sets `Immutable: true` on the ConfigMap.
- Claims the ConfigMap for its owner: a controller reference locally or ownership
  labels on a target cluster.
- If a ConfigMap with the same name already exists, verifies that it belongs to the
  owner and returns the name; otherwise, it returns an ownership error.
- Same data always produces the same hash (deterministic).
- Different data normally produces a different hash. The eight-hex-character suffix
  has a collision probability of about 1 in 4 billion per base name.

**Example:**

```go
name, err := config.CreateImmutableConfigMap(ctx, client, scheme, owner,
    "keystone-config", "openstack",
    map[string]string{"keystone.conf": renderedINI},
)
// name == "keystone-config-a1b2c3d4"
```

### CreateImmutableSecret

```go
func CreateImmutableSecret(
    ctx context.Context,
    c client.Client,
    scheme *runtime.Scheme,
    owner client.Object,
    baseName, namespace string,
    data map[string][]byte,
) (string, error)
```

Creates an immutable Secret whose name includes an 8-character SHA-256 hash of
its data. Sorted keys and length-prefixed key/value pairs make the hash
deterministic. A changed data map normally produces a new name; the truncated
hash can collide. A new Secret name lets a workload roll out when it mounts the
Secret. The helper claims the Secret for its owner and verifies ownership when
the same name already exists.

Use this helper for rendered sensitive configuration, such as per-domain
Keystone files with LDAP bind credentials.

### PruneImmutableConfigMaps

```go
func PruneImmutableConfigMaps(
    ctx context.Context,
    c client.Client,
    scheme *runtime.Scheme,
    owner client.Object,
    opts PruneOptions,
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
| `scheme` | `*runtime.Scheme` | Scheme used to resolve the owner's kind when checking ownership |
| `owner` | `client.Object` | Owning CR whose children may be pruned |
| `opts` | `PruneOptions` | Base name, namespace, current name, and historical retention count |

**Returns:**

| Value | Description |
| --- | --- |
| `error` | Non-nil on list or delete failure; `nil` on success or when no pruning is needed |

`PruneOptions` contains `BaseName`, `Namespace`, `CurrentName`, and `Retain`.
`CurrentName` is excluded from deletion. An empty value leaves no ConfigMap
protected as current. Negative `Retain` values are treated as zero.

**Algorithm:**

1. Lists ConfigMaps matching the `cobaltcore.c5c3.io/config-base` label in the namespace.
2. Filters to ConfigMaps matching the `baseName + "-"` prefix.
3. Excludes the ConfigMap named `currentName` (the active one).
4. Excludes ConfigMaps that `multicluster.Controls` does not identify as owned by `owner`.
5. Sorts remaining candidates by `CreationTimestamp` descending (newest first),
   then by name descending to break timestamp ties deterministically.
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
| Name matches `baseName-*` prefix, owned by `owner` through a local owner reference or remote labels | Yes |
| Name equals `currentName` | No (always excluded) |
| Name does not match `baseName-*` prefix | No |
| Neither a matching local owner reference nor remote ownership labels | No |

**Edge cases:**

| Scenario | Result |
| --- | --- |
| No historical ConfigMaps exist | No-op, returns `nil` |
| Fewer historical ConfigMaps than `retain` | No-op, returns `nil` |
| `retain=0` | All historical ConfigMaps are deleted; the named current ConfigMap remains |
| `currentName=""` | No ConfigMap is protected as current |
| Negative `retain` | Treated as `retain=0` |
| ConfigMap deleted between list and delete | `NotFound` silently ignored |
| Overlapping prefix (e.g., `test-config-` vs `test-config-extra-`) | Strict `baseName + "-"` prefix prevents false matches |
| Pre-existing ConfigMaps without `cobaltcore.c5c3.io/config-base` label | Not pruned — invisible to the server-side selector. Local objects with owner references are garbage-collected with the CR; recognized remote children are handled by the remote-child sweep. |

**Example:**

```go
// After creating a new ConfigMap, prune old ones keeping 3 historical:
err := config.PruneImmutableConfigMaps(ctx, client, scheme, keystoneCR,
    config.PruneOptions{
        BaseName: "keystone-config", Namespace: "openstack",
        CurrentName: "keystone-config-a1b2c3d4", Retain: 3,
    },
)
// With 5 historical ConfigMaps, the 2 oldest are deleted, 3 newest + current remain.
```

### PruneImmutableSecrets

```go
func PruneImmutableSecrets(
    ctx context.Context,
    c client.Client,
    scheme *runtime.Scheme,
    owner client.Object,
    opts PruneOptions,
) error
```

Prunes owned, hash-suffixed Secrets with the same filtering, ordering, and
retention rules as `PruneImmutableConfigMaps`. Set `CurrentName` to the mounted
Secret and `Retain` to the number of historical Secrets to keep. An empty
`CurrentName` with `Retain` set to zero removes all matching owned Secrets.

---

## Package: `database`

Manages MariaDB database resources for CobaltCore OpenStack operators. Uses typed structs from
`github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1`.

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
  manager owns only the fields the builder sets, so server defaults on omitted fields
  (for example `maxUserConnections` on a User whose provision params leave it unsized)
  are preserved and a converged Database is not rewritten.
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

- Applies the User first. It applies the Grant only after the User reports
  `Ready=True`; the MariaDB operator needs the SQL user to exist before it can
  issue the grant.
- Both resources are claimed for the owner: local resources receive controller owner references, while target-cluster resources carry ownership labels.
- Readiness of the User and Grant is determined by the package-internal
  `isUserReady` and `isGrantReady` helpers.

---

## Package: `deployment`

Manages Kubernetes Deployments and Services for CobaltCore OpenStack operators.

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
  to preserve the HPA-owned replica count when the desired `Spec.Replicas` is nil. On
  create, a nil replica count is set to `1`.
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

Creates or updates a Service; the return value contains only apply errors.

**Behavior:**

- Applies the desired Service via `apply.EnsureObject` (Server-Side Apply). Callers
  should leave server-assigned fields (`ClusterIP`, `ClusterIPs`, `IPFamilies`, and
  `NodePort`) unset so the field manager does not claim their values.
- Retains a pre-apply `Get` to reject conflicting explicit values for `ClusterIP`,
  `ClusterIPs`, or `IPFamilies`. `NodePort` is not part of this conflict check.

### IsDeploymentReady

```go
func IsDeploymentReady(deploy *appsv1.Deployment) bool
```

Pure function. Returns `true` when all of these conditions are met:

1. `deploy.Status.ObservedGeneration >= deploy.Generation`.
2. `deploy.Status.ReadyReplicas >= *deploy.Spec.Replicas` (defaults to 1 if
   `Spec.Replicas` is nil).
3. The Deployment has an `Available` condition with status `True`.

**Edge cases:**

| Scenario | Result |
| --- | --- |
| `Spec.Replicas` is nil | Defaults to 1 |
| `ObservedGeneration < Generation` | `false` |
| No `Available` condition | `false` |
| `ReadyReplicas < desired` | `false` |
| `ReadyReplicas >= desired` and `Available=True` | `true` |

---

## Package: `job`

Manages Kubernetes Jobs and CronJobs for CobaltCore OpenStack operators.

### RunJob

```go
func RunJob(
    ctx context.Context,
    c client.Client,
    scheme *runtime.Scheme,
    owner client.Object,
    job *batchv1.Job,
) (bool, *batchv1.Job, error)
```

Creates a Job if it does not already exist and reports completion status. The
second return value is the Job observed at the start of the pass, which lets
callers record terminal metrics without an extra `Get`.

**Returns:** `(true, observed, nil)` when the Job has a `Complete` condition
with status `True` and its re-run key is unchanged; `(false, observed, nil)`
when it is still running or was deleted and re-created because its re-run key
changed; `(false, observed, error)` wrapping `ErrJobFailed` when the Job has
permanently failed and its key is unchanged. API failures after a Job is read
return `(false, observed, error)`; a failed `Get` returns `(false, nil, error)`.

**Behavior:**

- If the Job does not exist, it is claimed with a local controller reference or
  target-cluster ownership labels and created. The return is `(false, nil, nil)`;
  a newly created Job is not complete.
- If the Job already exists: checks completion via the package-internal
  `isJobComplete` helper and permanent failure via `isJobFailed`. A completed
  **or permanently failed** Job whose stored re-run key (the
  `cobaltcore.c5c3.io/pod-spec-hash` annotation) no longer matches the desired pod
  template is deleted (background propagation) and re-created. This lets a Job that
  failed under a since-fixed spec (new container image, corrected ConfigMap,
  rotated password) re-runs instead of wedging. A permanently failed Job whose
  re-run key is unchanged returns an error wrapping `ErrJobFailed` to prevent
  infinite requeue loops; the helper does not update Jobs in place.
- Reconcilers should call `RunJob` on each reconciliation loop. The function is
  idempotent: calling it when the Job already exists and is complete returns
  `(true, observed, nil)`, with the existing Job in `observed`.

`RunJob` uses a SHA-256 hash of the full desired pod template as its re-run key.
Changes to template metadata, including annotations, also change the key.

### RunJobWithRerunKey

```go
func RunJobWithRerunKey(
    ctx context.Context,
    c client.Client,
    scheme *runtime.Scheme,
    owner client.Object,
    job *batchv1.Job,
    rerunKey string,
) (bool, *batchv1.Job, error)
```

Uses the supplied `rerunKey` instead of hashing the pod template. A completed
or permanently failed Job is re-created only when this key changes. The
Keystone bootstrap Job uses the admin-password digest, so a password rotation
triggers a rerun while a container-image change alone does not.

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

Creates or updates a CronJob, claimed locally with a controller owner reference or
on a target cluster with ownership labels.

**Behavior:**

- Applies the desired CronJob via `apply.EnsureObject` (Server-Side Apply); a converged
  CronJob is not rewritten, and spec changes take effect on the next scheduled run.

`isJobComplete` and `isJobFailed` are package-internal checks used by `RunJob`.
The exported `TerminalCondition` helper returns `JobComplete` or `JobFailed`
when that condition is `True`; `JobFailed` takes precedence if both are `True`.

### DeleteCronJob

```go
func DeleteCronJob(
    ctx context.Context,
    c client.Client,
    namespace, name string,
) error
```

Deletes the named CronJob. It returns `nil` if the CronJob does not exist.

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
| `error` | Non-nil when the ConfigMap is missing, the key is absent, YAML is invalid, or YAML parses to `nil` |

**Error conditions:**

| Condition | Error |
| --- | --- |
| ConfigMap does not exist | Wrapped API server error (compatible with `apierrors.IsNotFound`) |
| `policy.yaml` key absent | `ConfigMap <key> does not contain key "policy.yaml"` |
| Invalid YAML content | `parsing policy.yaml from ConfigMap <key>: <parse error>` |
| YAML is empty or parses to `nil` | `ConfigMap <key> key "policy.yaml" is empty or parsed to nil` |

**Example:**

```go
rules, err := policy.LoadPolicyFromConfigMap(ctx, client,
    types.NamespacedName{Namespace: "openstack", Name: "keystone-policy"},
)
// rules == map[string]string{"identity:get_user": "role:admin", ...}
```

---

## Package: `satellite`

Holds the mechanics shared by the satellite-CRD controllers and by the parent
reconcilers that aggregate them. A satellite CR is a namespaced configuration CR
attached to a parent service CR by name, and it observes its own effect through
the Deployment the parent runs. `GlanceBackend`, `CinderBackend` and
`BarbicanSecretStore` consume resolve, observe and collect.
`CinderBackupBackend` consumes resolve, collect and the volume lookup, and
`KeystoneIdentityBackend` the volume lookup alone.

The condition vocabulary, messages, events, and requeue intervals stay in the
operator. Each helper either returns a decision for the operator to use or takes
the condition texts as parameters.

### The handshake {#satellite-handshake}

A satellite and its parent settle in five steps. The figure draws them, and its
numbers are the steps below.

![The handshake between a satellite resource and the service it attaches to, in five numbered steps across two controllers. 1: the satellite controller checks the credentials and sets CredentialsReady on the satellite. 2: the aggregation step of the service controller reads only that condition. 3: it renders one section per satellite that passed into a Secret whose name carries a hash of its content. 4: the pod template of the service's Deployment mounts that Secret, and a new name rolls the pods. 5: the satellite controller finds its section in the mounted Secret and sets ConfigProjected. Ready turns True once both conditions are. An arrow marked never runs from Ready to the aggregation step: reading Ready there would deadlock, because Ready needs ConfigProjected, which needs that step. On a KeystoneIdentityBackend the gate is DomainReady, and ConfigProjected also waits until the rollout has finished.](../../diagrams/service-satellite-handshake.svg)

1. The satellite controller resolves the parent the satellite names, checks
   what the satellite needs, and sets the gate condition on the satellite:
   `CredentialsReady`.
2. The aggregation step in the parent's pipeline lists the satellites that name
   the parent and reads that one condition of each. A satellite whose gate is
   not `True` is skipped, and the pipeline goes on.
3. The step renders one section per satellite that passed into a Secret whose
   name carries a hash of its content.
4. The pod template of the parent's Deployment mounts that Secret. A new
   Secret name is a new pod template, so the pods roll.
5. The satellite controller reads the pod template, follows the volume to the
   Secret, and finds its own section there. It sets `ConfigProjected`, and the
   aggregate `Ready` follows once both conditions are `True`.

The step never reads a satellite's `Ready`. `Ready` needs `ConfigProjected`,
and `ConfigProjected` turns `True` only after the step has projected the
satellite, so a step that waited for `Ready` would wait for itself. Another
controller may gate on `Ready`: the c5c3-operator does, for the websso choices
it projects onto Horizon.

No status field of the parent carries the projection. The Deployment and the
Secret are the evidence, and while `ConfigProjected` is `False` the satellite
controller polls for it every 15 seconds, every 30 on a `BarbicanSecretStore`.

| Kind | Gate the step reads | What the gate needs | Rendered into | Workload | Evidence for `ConfigProjected` |
| --- | --- | --- | --- | --- | --- |
| `GlanceBackend` | `CredentialsReady` | The Secret of `spec.s3.credentialsSecretRef` with both S3 keys | Key `backends.conf` of the Secret `{glance}-backends-{hash}` | Deployment `{glance}` | The header `[{name}]` in the mounted Secret |
| `CinderBackend` | `CredentialsReady` | Nothing for `NFS`, where it is `True/CredentialsNotRequired` once the parent resolves; the Secret of `spec.rbd.keySecretRef` carrying a non-empty base64 `userKey` for `RBD` | The Secret `{cinder}-backend-{name}-{hash}`, one per backend | Deployment `{cinder}-volume-{name}`, one per backend | The header `[{name}]` in the key `backend.conf` |
| `CinderBackupBackend` | `CredentialsReady` | Nothing for `NFS`, where it is `True/CredentialsNotRequired` once the parent resolves; the Secret of `spec.rbd.keySecretRef` carrying a non-empty base64 `userKey` for `RBD` | Key `backup.conf` of a Secret named after the backend | Deployment `{cinder}-backup` | The name of the Secret its `backup` volume references. The content is not read |
| `BarbicanSecretStore` | `CredentialsReady` | The credentials Secret and a login the OpenBao server accepts. A store the operator provisions sets `ProvisioningReady` first, and the step does not read that | The section `[secretstore:{name}]` of `barbican.conf` in the Secret `{barbican}-config-{hash}` | Deployment `{barbican}` | The section header in the mounted Secret |
| `KeystoneIdentityBackend` | `DomainReady` | The Keystone API answering, the admin password Secret, and the domain created or adopted | `keystone.{domain}.conf` in the Secret `{keystone}-domains-{hash}`; the OIDC and SAML documents in `{keystone}-federation-{hash}` | Deployment `{keystone}` | The data key in the mounted Secret, and the rollout finished |

`KeystoneIdentityBackend` is the exception by design. Its gate needs the
Keystone API, so the parent's Deployment already runs when step 1 passes, and
its `ConfigProjected` has a third state, `WaitingForRollout`.

### ResolveParentChildren

```go
func ResolveParentChildren(
    ctx context.Context,
    p ResolveParams,
) (children client.Client, result ctrl.Result, err error)
```

Returns the client a satellite's observations belong on, after reading its
parent and resolving the cluster that parent targets.

**Behavior:**

- `ResolveParams.Client` reads the parent on the management cluster into
  `Parent` at `ParentKey`; `ParentKind` names the kind in the error text.
  `TargetClusterRef` then reads the ref from `Parent`, and `Resolver` looks the
  cluster up. A nil resolver keeps everything local. Conditions are added to the
  satellite's own `Conditions` slice at `Generation`.
- A non-`NotFound` Get on the parent returns a nil client and
  `fetching parent <kind> <key>: <cause>`.
- A parent that does not exist sets `GateConditionType` to `False` with
  `WaitingForParentReason` and `WaitingForParentMessage`, and requeues after
  `RequeueAfter`. There is no fallback to the local client: the parent's
  `targetClusterRef` is what decides which cluster the satellite's credentials
  and observations belong on, so a local guess would read the wrong cluster for
  the whole window between a GitOps apply landing the satellite and the same
  apply landing its parent.
- The projection demotion is opt-in. With `ProjectionConditionType` set, a
  missing parent also flips that condition to `False` under
  `ProjectionDemotionReason` and `ProjectionDemotionMessage`, but only where it
  currently stands `True`. An empty type demotes nothing, and an absent or
  already `False` condition is left untouched, so a satellite that has not
  converged yet keeps the condition absent.
- A target cluster that does not resolve sets `GateConditionType` to `False`
  under the shared `multicluster.TargetClusterUnavailable` reason with the
  resolver's error text, and requeues after `RequeueAfter`.
- On success the children client is non-nil and the result is the zero
  `ctrl.Result`. A nil children client means the caller returns `result` and
  `err` as they are.

### SecretNameForVolume

```go
func SecretNameForVolume(spec *corev1.PodSpec, volumeName string) string
```

Pure function. Returns the name of the Secret a pod-spec volume mounts.

**Behavior:**

- Returns `""` for a nil `spec` and for a volume name absent from
  `spec.Volumes`.
- The scan stops at the first name match. A ConfigMap- or emptyDir-backed volume
  of the searched name yields `""` and the walk does not continue to the next
  volume that happens to carry a Secret.

### SectionPresent

```go
func SectionPresent(rendered []byte, header string) bool
```

Pure function. Reports whether `rendered` carries `header` as a whole line.

**Behavior:**

- The token is bounded on the left by the start of the data or a newline, and on
  the right by a newline (LF or CR) or the end of the data.
- The boundary check rules out substring collisions: a section `[name2]` does
  not satisfy a lookup for `[name]`, and a header appearing inside an option
  value does not either.
- The scan resumes past a hit the boundary check rejected, so a later whole-line
  occurrence still matches.

### SectionProjected

```go
func SectionProjected(
    ctx context.Context,
    p ObserveParams,
) (bool, error)
```

Reports whether a satellite's section reached the config file the parent's
Deployment mounts.

**Behavior:**

- `ObserveParams.Children` reads the Deployment at `DeploymentKey`, then the
  Secret that the pod volume named `VolumeName` mounts, in the Deployment's
  namespace. `SectionHeader` (for example `[store]`) is looked up in that
  Secret's `DataKey`.
- A `NotFound` anywhere on the chain is `(false, nil)`: a missing Deployment, a
  missing or non-Secret volume, a missing Secret and an absent data key all read
  as an authoritative "not projected yet".
- A non-`NotFound` Get failure is wrapped as `fetching Deployment <key>: <cause>`
  or `fetching projection Secret <key>: <cause>`.
- Consumers keep their own parent re-read ahead of this call, and their
  `observeConfigProjected` wrappers own the condition reasons, the messages and
  the requeue intervals.

### Collect

```go
func Collect[T any, PT interface {
    *T
    client.Object
}](p CollectParams[T, PT]) Collection[T, PT]
```

Turns a parent's list of attached satellites into one pass's view of them.

**Behavior:**

- `CollectParams.Items` is the caller's indexed List result, in any order. A
  List type holding values, as `GlanceBackendList.Items` does, becomes a pointer
  slice by appending `&list.Items[i]` over the index.
- `Gate` decides which items may be projected; `GlanceBackend` passes
  `CredentialsReady == True`. A nil gate passes everything. `IsDefault` marks
  the parent's default, and a nil function means the parent has no default rule.
- `Collection.Attached` holds the items carrying no deletion timestamp, sorted
  by name. `Gated` is the subset that passes the gate and `DefaultCandidates`
  the gated items `IsDefault` marks, both in that same order.
- `Collection.Valid` reports whether the default rule holds: no rule at all, or
  one gated candidate. It is a decision, and the parent stamps its own condition
  type with its own reason and message from it.
- A deleting satellite drops out here, so the pass de-projects it without
  waiting for the object to go.
- Sorting by name is what keeps the rendered config, and with it its content
  hash, stable across passes.
- The function returns no error and never touches the API. Everything it needs
  is on the objects the caller listed, and the pointers it hands back alias
  them.

---

## Package: `secrets`

Manages External Secrets Operator resources and Kubernetes Secrets for CobaltCore OpenStack
operators. Uses typed structs from `github.com/external-secrets/external-secrets/apis`.

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

- Evaluates the observed ExternalSecret once, without waiting for completion.
  Reconcilers call it on each loop and requeue while `ready` is `false`.
- The separate `exists` return lets callers surface a clearer status —
  "ExternalSecret not found yet" versus "waiting for ESO to sync" — instead of
  collapsing both into a single not-ready signal.
- Uses ESO's `ExternalSecretReady` condition-type constant.

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

- A `NotFound` result returns `(false, nil)` and does not indicate an API failure.
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

Returns the bytes stored at a specific Secret data key as a Go string. The
Kubernetes client has already decoded the API's base64 representation.

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
| Key not in `Secret.Data` | Wraps `ErrKeyNotFound`: `key not found in Secret: key "<dataKey>" in Secret <namespace>/<name>`; check with `errors.Is(err, secrets.ErrKeyNotFound)` |

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

Creates or updates a PushSecret CR, claimed locally with a controller owner reference
or on a target cluster with ownership labels.

**Behavior:**

- Applies the desired PushSecret via `apply.EnsureObject` (Server-Side Apply). The field
  manager owns only the fields the builder sets, so the server defaults on `updatePolicy`
  and `refreshInterval` are preserved. A converged PushSecret is not rewritten, so ESO
  does not re-push an unchanged credential.
- Uses the ESO `v1alpha1` PushSecret API. This API is unstable; see
  [External CRD Dependencies](#external-crd-dependencies).

### Per-CR secret store selection

These helpers back the optional per-CR `spec.secretStoreRef`. Each CR routes
its ExternalSecrets and PushSecrets through a selected cluster-scoped
`ClusterSecretStore` or namespaced `SecretStore`. `OpenBaoClusterStoreName`
names the default `ClusterSecretStore`, `openbao-cluster-store`.
`IsClusterSecretStoreReady(ctx, c, name) (bool, error)` checks that store's
`Ready` condition. `IsSecretStoreReady` checks the namespaced equivalent.

- `EffectiveStoreRef(ref *commonv1.SecretStoreRefSpec) commonv1.SecretStoreRefSpec`:
  resolves a nil ref to the default cluster store and normalizes an empty kind
  to `ClusterSecretStore`, preserving the supplied name. A CR that omits the
  field uses `{ClusterSecretStore, openbao-cluster-store}`.
- `IsStoreRefReady(ctx, c, ref commonv1.SecretStoreRefSpec, namespace string) (bool, error)`:
  checks the selected store. It looks up a cluster store by name and a namespaced
  store in `namespace`; an unsupported kind returns an error.
- `GateStoreReady(ctx, c, ref, namespace string, namespaceScoped bool, conds *[]metav1.Condition, generation int64, conditionType string) (bool, error)`:
  checks the selected store and sets a `SecretStoreNotReady` condition when it
  is not ready. The message names the kind and name. A namespace-scoped operator
  refuses a `ClusterSecretStore` without a client call and sets the
  `ClusterSecretStoreUnsupported` reason with guidance to select a namespaced
  `SecretStore`.
- `AddStoreWatches(b *mcbuilder.Builder, scheme *runtime.Scheme, targets multicluster.TargetClusterFunc, namespaceScoped bool, c client.Reader, mapper func(client.Reader, commonv1.SecretStoreRefKind) handler.MapFunc) (*mcbuilder.Builder, error)`:
  adds a watch for `ClusterSecretStore` unless `namespaceScoped` is true, and
  always adds a watch for `SecretStore`. Each watch uses `mapper(c, kind)`.
  A nil mapper returns `secrets: store watch mapper must not be nil`. Errors
  from `multicluster.AddInputWatch` are returned unchanged with a nil builder.
- `ESOSecretStoreRef(ref commonv1.SecretStoreRefSpec) esov1.SecretStoreRef`:
  builds an ESO ExternalSecret store ref from a resolved store reference.
- `PushSecretStoreRefs(ref commonv1.SecretStoreRefSpec) []esov1alpha1.PushSecretStoreRef`:
  builds the single-element ESO PushSecret store-ref slice from a resolved
  store reference.

---

## Package: `tls`

Manages TLS certificates and secrets for CobaltCore OpenStack operators. Uses typed structs from
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

- On create: claims the Certificate locally with a controller owner reference or on a
  target cluster with ownership labels, creates it, and returns `(false, nil)`.
- On update: overwrites `existing.Spec` with the provided spec.
- Readiness is determined by the package-internal `isCertificateReady` helper on
  the existing resource.
- cert-manager creates a Secret with the TLS certificate once the Certificate is ready.

---

## Cross-Package Dependencies

These helpers compose lower-level packages. For example,
`database.ReconcileProvision` and `deployment.EnsureDeployment` compose
lower-level packages, and reconcilers compose those helpers in their pipelines.

## Reconciler Integration Pattern

A reconciler calls these packages from the steps of its pipeline. The figure
shows the pipeline of the Keystone operator, and the table names the functions
of this page each of its steps calls.

![The sub-reconciler pipeline of the Keystone operator. A pass runs its entries one after another and ends at the first that returns a requeue or an error: Secrets, DatabaseTLS, DBConnectionSecret, IdentityBackends, Config, a parallel group of FernetKeys, CredentialKeys and NetworkPolicy, then Database, PolicyValidation, Deployment, an unnamed prune step, a second parallel group of HTTPRoute, HealthCheck, HPA, VPA, Bootstrap and TrustFlush, and PasswordRotation. Every member of a group starts, and the group returns the shortest requeue. Each step sets one condition; DBConnectionSecret and Config report through SecretsReady, and the prune step has none of its own. DBConnectionSecret hands the hash of the database connection to Deployment, IdentityBackends hands on the name of the domains Secret and the federation projection, Config hands on the name of the ConfigMap, and Deployment sets status.endpoint, which HealthCheck waits for. The early exit and the full pass both end in updateStatus, which aggregates Ready from the sub-conditions and writes the status only when it changed.](../../diagrams/service-reconciler-pipeline.svg)

| Step | Functions of this page it calls |
| --- | --- |
| `DatabaseTLS` | `tls.EnsureCertificate` |
| `IdentityBackends` | `config.CreateImmutableSecret`, `secrets.GetSecretValue`, `satellite.SecretNameForVolume` |
| `Config` | `policy.LoadPolicyFromConfigMap`, `config.CreateImmutableConfigMap` |
| `FernetKeys`, `CredentialKeys` | `config.CreateImmutableConfigMap`, `job.EnsureCronJob`, `secrets.EnsurePushSecret` |
| `PolicyValidation` | `job.RunJob` |
| `Deployment` | `deployment.EnsureDeployment`, `deployment.EnsureService` |
| `(prune)` | `config.PruneImmutableConfigMaps`, `config.PruneImmutableSecrets` |
| `Bootstrap` | `secrets.GetSecretValue` |
| `TrustFlush` | `job.EnsureCronJob` |
| `PasswordRotation` | `config.CreateImmutableConfigMap`, `config.PruneImmutableConfigMaps`, `job.EnsureCronJob`, `secrets.EnsurePushSecret` |

The steps the table leaves out call shared flows this page does not describe,
such as `database.ReconcileProvision`, `database.ReconcileSyncJobs` and
`deployment.ReconcileHPA`. A step returns a `ctrl.Result` and an error. The
pipeline moves on when the result is zero and the error is nil; otherwise the
pass ends, and the status is still written.
