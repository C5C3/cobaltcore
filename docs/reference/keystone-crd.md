---
title: Keystone CRD API Reference
quadrant: operator
feature: CC-0011, CC-0012, CC-0016, CC-0038, CC-0040, CC-0042
---

# Keystone CRD API Reference

Reference documentation for the Keystone Custom Resource Definition (CC-0011). The
Keystone CRD is the reference implementation for all CobaltCore service operators —
the patterns established here (types, webhooks, generation, scheme registration) will
be replicated for Nova, Neutron, Glance, and other OpenStack service operators.

## API Group and Version

| Field | Value |
| --- | --- |
| Group | `keystone.openstack.c5c3.io` |
| Version | `v1alpha1` |
| Kind | `Keystone` |
| List Kind | `KeystoneList` |
| Scope | Namespaced |

**Import path:**

```go
import keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
```

**Scheme registration:**

The `init()` function in `keystone_types.go` registers `Keystone` and `KeystoneList`
with the `SchemeBuilder`. Operator `main.go` calls `AddToScheme` to register the types
with the manager's scheme.

---

## Resource Shape

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: Keystone
metadata:
  name: keystone
  namespace: openstack
spec:
  replicas: 3
  image:
    repository: c5c3/keystone
    tag: "2025.1"
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
  fernet:
    rotationSchedule: "0 0 * * 0"
    maxActiveKeys: 3
  autoscaling:
    minReplicas: 2
    maxReplicas: 10
    targetCPUUtilization: 80
  resources:
    requests:
      memory: 256Mi
      cpu: 100m
    limits:
      memory: 512Mi
      cpu: 500m
  uwsgi:
    processes: 4
    threads: 4
    httpKeepAlive: true
  bootstrap:
    adminUser: admin
    adminPasswordSecretRef:
      name: keystone-admin
      key: password
    region: RegionOne
status:
  conditions:
    - type: Ready
      status: "True"
      reason: AllSubResourcesReady
      message: All sub-resources are ready
      lastTransitionTime: "2026-03-09T00:00:00Z"
  endpoint: https://keystone.openstack.svc:5000/v3
```

### Printer Columns

`kubectl get keystones` displays these columns:

| Column | JSON Path | Type |
| --- | --- | --- |
| Ready | `.status.conditions[?(@.type=='Ready')].status` | string |
| Endpoint | `.status.endpoint` | string |
| Age | `.metadata.creationTimestamp` | date |

---

## KeystoneSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `replicas` | `int32` | No | `3` | Number of Keystone API replicas. Minimum: 1. The webhook provides a secondary default of 3 when zero. |
| `image` | [`ImageSpec`](#imagespec) | Yes | — | Keystone container image reference. |
| `database` | [`DatabaseSpec`](#databasespec) | Yes | — | MariaDB connection configuration. |
| `cache` | [`CacheSpec`](#cachespec) | Yes | — | Memcached cache configuration. |
| `fernet` | [`FernetSpec`](#fernetspec) | No | See below | Fernet key rotation configuration. |
| `federation` | [`*FederationSpec`](#federationspec) | No | `nil` | Federation configuration (optional). |
| `bootstrap` | [`BootstrapSpec`](#bootstrapspec) | Yes | — | Initial Keystone bootstrap parameters. |
| `middleware` | `[]MiddlewareSpec` | No | `nil` | WSGI middleware filters for api-paste.ini. |
| `plugins` | `[]PluginSpec` | No | `nil` | Service plugins/drivers to configure. |
| `policyOverrides` | [`*PolicySpec`](#policyspec) | No | `nil` | Custom oslo.policy rules. |
| `autoscaling` | [`*AutoscalingSpec`](#autoscalingspec) | No | `nil` | Horizontal pod autoscaling configuration. When set, an HPA is created targeting the `{name}-api` Deployment. When removed, the HPA is deleted (CC-0038). |
| `resources` | [`*corev1.ResourceRequirements`](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/pod-v1/#resources) | No | See below | CPU and memory requests and limits for the Keystone API container. When unset, the defaulting webhook injects sensible defaults to ensure Burstable QoS class and enable HPA utilization calculations (CC-0042). |
| `uwsgi` | [`*UWSGISpec`](#uwsgispec) | No | `nil` | uWSGI application server parameters. When set, the operator uses these values for the Deployment container command. When `nil`, hardcoded defaults (processes=2, threads=1, httpKeepAlive=true) are used in the reconciler (CC-0040). |
| `extraConfig` | `map[string]map[string]string` | No | `nil` | Free-form INI sections for additional configuration. |

### CEL Validation Rules

The CRD includes structural validation rules enforced by the API server before
webhooks are invoked:

| Field | Rule | Error Message |
| --- | --- | --- |
| `spec.database` | `has(self.clusterRef) != has(self.host)` | "exactly one of clusterRef or host must be set" |
| `spec.policyOverrides` | `self.rules != null \|\| self.configMapRef != null` | "at least one of rules or configMapRef must be set" |
| `spec.autoscaling` | `has(self.targetCPUUtilization) \|\| has(self.targetMemoryUtilization)` | "at least one of targetCPUUtilization or targetMemoryUtilization must be set" |
| `spec.replicas` | Minimum: 1 | — |
| `spec.fernet.maxActiveKeys` | Minimum: 3 | — |
| `spec.uwsgi.processes` | Minimum: 1 | — |
| `spec.uwsgi.threads` | Minimum: 1 | — |

> **Known limitation (CC-0040):** `spec.uwsgi.processes` and `spec.uwsgi.threads`
> have no upper-bound validation. A user could set an extremely high value (e.g.,
> `processes: 10000`), causing the Deployment to request more workers than the node
> can sustain. A `+kubebuilder:validation:Maximum` marker should be added once the
> team agrees on a safe ceiling. Track this as a follow-up product decision.

---

## AutoscalingSpec

Configures horizontal pod autoscaling for the Keystone API Deployment (CC-0038).
This is a pointer field (`*AutoscalingSpec`) on `KeystoneSpec` — when `nil`,
no HPA is created and the `HPAReady` condition is set to `True` with reason
`HPANotRequired`. When set, a `HorizontalPodAutoscaler` (autoscaling/v2) is
created targeting the `{name}-api` Deployment. Removing the field deletes the
existing HPA.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `minReplicas` | `*int32` | No | `spec.replicas` | Lower bound for the number of replicas. Minimum: 1. Defaults to `spec.replicas` when unset, allowing the HPA to scale down to the static replica count. |
| `maxReplicas` | `int32` | Yes | — | Upper bound for the number of replicas. Minimum: 1. |
| `targetCPUUtilization` | `*int32` | No\* | — | Target average CPU utilization as a percentage. Range: 1–100. At least one of `targetCPUUtilization` or `targetMemoryUtilization` must be set. |
| `targetMemoryUtilization` | `*int32` | No\* | — | Target average memory utilization as a percentage. Range: 1–100. At least one of `targetCPUUtilization` or `targetMemoryUtilization` must be set. |

\* At least one of `targetCPUUtilization` or `targetMemoryUtilization` is required
(enforced by CEL XValidation).

### HPA Resource Mapping

The HPA created from this spec has the following shape:

| HPA Field | Value |
| --- | --- |
| `metadata.name` | `{name}-api` |
| `metadata.labels` | `commonLabels` (same as Deployment) |
| `spec.scaleTargetRef.apiVersion` | `apps/v1` |
| `spec.scaleTargetRef.kind` | `Deployment` |
| `spec.scaleTargetRef.name` | `{name}-api` |
| `spec.minReplicas` | `autoscaling.minReplicas` (or `spec.replicas` if unset) |
| `spec.maxReplicas` | `autoscaling.maxReplicas` |
| `spec.metrics` | CPU and/or memory `Resource` metrics based on which targets are set |
| `ownerReferences` | Points to the Keystone CR (controller: true) |

### Example

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: Keystone
metadata:
  name: keystone
  namespace: openstack
spec:
  replicas: 3
  image:
    repository: c5c3/keystone
    tag: "2025.1"
  # ... other required fields ...
  autoscaling:
    minReplicas: 2
    maxReplicas: 10
    targetCPUUtilization: 80
    targetMemoryUtilization: 70
```

---

## UWSGISpec

Configures the uWSGI application server parameters for the Keystone API container
(CC-0040). This is a pointer field (`*UWSGISpec`) on `KeystoneSpec` — when `nil`,
the reconciler uses hardcoded defaults (processes=2, threads=1, httpKeepAlive=true)
and the webhook does **not** inject a default `UWSGISpec`. When set (even as
`uwsgi: {}`), the webhook defaults zero-valued sub-fields and the reconciler reads
from the spec.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `processes` | `int32` | No | `2` | Number of uWSGI worker processes. Minimum: 1. Maps to `--processes` in the container command. |
| `threads` | `int32` | No | `1` | Number of threads per uWSGI worker process. Minimum: 1. Maps to `--threads` in the container command. |
| `httpKeepAlive` | `bool` | No | `true` | Enables the `--http-keepalive` flag on the uWSGI process. When `false`, the flag is omitted. See [HTTPKeepAlive defaulting](#httpkeepalive-defaulting-caveat) for the zero-value caveat. |

### Deployment Command Mapping

The reconciler's `uwsgiCommand()` helper constructs the container command from
`spec.uwsgi` (or defaults when `nil`). Fixed flags are always present regardless
of configuration:

| Command Flag | Source |
| --- | --- |
| `uwsgi` | Binary name (always first) |
| `--http :5000` | Fixed — Keystone API listen port |
| `--http-keepalive` | Included when `httpKeepAlive` is `true` (or default); omitted when `false` |
| `--wsgi-file /var/lib/openstack/bin/keystone-wsgi-public` | Fixed — Keystone WSGI entry point |
| `--master` | Fixed — enables uWSGI master process |
| `--lazy-apps` | Fixed — loads apps in each worker after fork |
| `--need-app` | Fixed — exits if no WSGI app is found |
| `--processes <N>` | `spec.uwsgi.processes` (default: 2) |
| `--threads <N>` | `spec.uwsgi.threads` (default: 1) |
| `--pyargv=--config-dir=/etc/keystone/keystone.conf.d/` | Fixed — passes config directory to Keystone |

### HTTPKeepAlive Defaulting Caveat

Go's `bool` zero value is `false`, making it impossible for the webhook to
distinguish "not set" from "explicitly set to `false`". Therefore, the defaulting
webhook **does not** touch `httpKeepAlive` at all — it only defaults `processes`
and `threads`. The CRD schema default (`+kubebuilder:default=true`) handles
`httpKeepAlive` in the normal admission path (API server applies the schema
default before the webhook runs). This means:

- `uwsgi: {}` → processes=2 (webhook), threads=1 (webhook),
  httpKeepAlive=true (CRD schema default via normal admission)
- `uwsgi: {processes: 4}` → processes=4, threads=1 (webhook),
  httpKeepAlive=true (CRD schema default)
- `uwsgi: {httpKeepAlive: false}` → httpKeepAlive stays `false` (explicit value
  is preserved by the API server)

**Bypass paths** (e.g., `kubectl patch`, upgrades, or when admission webhooks are
temporarily unavailable) may not apply the CRD schema default. In those cases,
`httpKeepAlive` remains at its Go zero value (`false`). The `uwsgiCommand`
function in the controller applies a defense-in-depth clamp but does not
override `httpKeepAlive`, so the `--http-keepalive` flag will be omitted from
the uWSGI invocation in bypass scenarios.

### Example

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: Keystone
metadata:
  name: keystone
  namespace: openstack
spec:
  replicas: 3
  image:
    repository: c5c3/keystone
    tag: "2025.1"
  # ... other required fields ...
  uwsgi:
    processes: 4
    threads: 4
    httpKeepAlive: false
```

---

## FernetSpec

Configures Fernet token key rotation.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `rotationSchedule` | `string` | No | `"0 0 * * 0"` | Cron expression (5-field standard format) for key rotation. Validated by `robfig/cron/v3` `ParseStandard`. |
| `maxActiveKeys` | `int32` | No | `3` | Maximum number of active Fernet keys. Minimum: 3. |

---

## FederationSpec

Configures Keystone federation support. This is a pointer field (`*FederationSpec`)
on `KeystoneSpec` — when `nil`, federation is disabled.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `enabled` | `bool` | Yes | — | Activates federation support. |

---

## BootstrapSpec

Configures the initial Keystone bootstrap.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `adminUser` | `string` | No | `"admin"` | Admin username for the bootstrap. |
| `adminPasswordSecretRef` | [`SecretRefSpec`](#secretrefspec) | Yes | — | Secret containing the admin password. |
| `region` | `string` | No | `"RegionOne"` | Keystone region name. |

---

## KeystoneStatus

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | Latest available observations of the Keystone state. |
| `endpoint` | `string` | Keystone API endpoint URL (set by the controller when ready). |

The status subresource is enabled via `+kubebuilder:subresource:status`.

---

## Shared Types (from `internal/common/types`)

The following types are imported as `commonv1` from
`github.com/c5c3/forge/internal/common/types`. They are shared across all CobaltCore
operator CRDs.

### ImageSpec

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `repository` | `string` | Yes | Container image repository (e.g., `c5c3/keystone`). |
| `tag` | `string` | Yes | Image tag (e.g., `2025.1`). |

### DatabaseSpec

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `clusterRef` | `*corev1.LocalObjectReference` | No | Reference to a MariaDB CR (managed mode). |
| `host` | `string` | No | Database hostname (brownfield mode). |
| `port` | `int32` | No | Database port (brownfield mode, default 3306). |
| `database` | `string` | Yes | Database name. |
| `secretRef` | [`SecretRefSpec`](#secretrefspec) | Yes | Secret with database credentials. |

Exactly one of `clusterRef` or `host` must be set (enforced by CEL validation).

### CacheSpec

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `clusterRef` | `*corev1.LocalObjectReference` | No | Reference to a Memcached CR (managed mode). |
| `backend` | `string` | Yes | Cache backend (e.g., `dogpile.cache.pymemcache`). |
| `servers` | `[]string` | No | Cache server endpoints (brownfield mode). |

### SecretRefSpec

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | `string` | Yes | Name of the Kubernetes Secret. |
| `key` | `string` | No | Key within the Secret's data. |

### PolicySpec

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `rules` | `map[string]string` | No | Inline policy rule overrides. Keys are oslo.policy rule names; values are rule definitions. Inline rules take precedence over ConfigMap rules. |
| `configMapRef` | `*corev1.LocalObjectReference` | No | Reference to a ConfigMap containing a `policy.yaml` key with rule overrides. |

When `policyOverrides` is set on `KeystoneSpec`, at least one of `rules` or
`configMapRef` must be provided (enforced by both CEL validation and the webhook).

### PluginSpec

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | `string` | Yes | Plugin name (e.g., `keystone-keycloak-backend`). |
| `configSection` | `string` | Yes | INI section name (e.g., `keycloak`). Must be unique across all plugins. |
| `config` | `map[string]string` | No | Key-value pairs for the plugin's INI section. |

### MiddlewareSpec

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | `string` | Yes | Filter name (e.g., `audit`). |
| `filterFactory` | `string` | Yes | Python entry point (e.g., `audit_middleware:filter_factory`). |
| `position` | `PipelinePosition` | Yes | Pipeline insertion point: `"before"` or `"after"`. |
| `config` | `map[string]string` | No | Key-value pairs for the filter section. |

---

## Webhooks

The `KeystoneWebhook` struct implements both defaulting and validating admission
webhooks via the `admission.Defaulter[*Keystone]` and `admission.Validator[*Keystone]`
interfaces from controller-runtime.

### Registration

```go
func (w *KeystoneWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error
```

Registers both webhooks with the manager using `builder.WebhookManagedBy[*Keystone]`.

### Defaulting Webhook

```go
func (w *KeystoneWebhook) Default(_ context.Context, obj *Keystone) error
```

Sets spec fields to their documented defaults when they carry zero values. Explicit
(non-zero) values are never overridden.

| Field | Condition | Default Value |
| --- | --- | --- |
| `spec.replicas` | `== 0` | `3` |
| `spec.fernet.maxActiveKeys` | `== 0` | `3` |
| `spec.cache.backend` | `== ""` | `"dogpile.cache.pymemcache"` |
| `spec.bootstrap.adminUser` | `== ""` | `"admin"` |
| `spec.bootstrap.region` | `== ""` | `"RegionOne"` |
| `spec.uwsgi.processes` | `== 0` (when `spec.uwsgi` is non-nil) | `2` — webhook only; when `spec.uwsgi` is `nil`, the reconciler applies this default internally (CC-0040). |
| `spec.uwsgi.threads` | `== 0` (when `spec.uwsgi` is non-nil) | `1` — same nil-pointer caveat as processes (CC-0040). |
| `spec.uwsgi.httpKeepAlive` | Field absent from JSON payload | `true` — defaulted by the CRD schema (`+kubebuilder:default=true`), **not** by the webhook. The webhook cannot distinguish "not set" from "explicitly false" for a bool field. See [HTTPKeepAlive defaulting](#httpkeepalive-defaulting-caveat) (CC-0040). |
| `spec.resources` | `== nil` or empty (`requests` and `limits` both unset) | `{requests: {memory: 256Mi, cpu: 100m}, limits: {memory: 512Mi, cpu: 500m}}` — ensures Burstable QoS class and enables HPA utilization calculations (CC-0042). |

**Design note:** `spec.fernet.rotationSchedule` is NOT defaulted by the webhook — it
relies solely on the Kubebuilder `+kubebuilder:default="0 0 * * 0"` marker (plan
decision #3, CC-0011). The webhook uses conditional checks (`== 0` / `== ""`) rather
than always-set to cooperate with the remaining Kubebuilder `+default` markers, which
also provide schema-level defaults. Both layers are intentional — schema defaults apply
at deserialization time, while webhook defaults catch zero values that bypass schema
defaults (e.g., explicit `replicas: 0`).

### Validating Webhook

```go
func (w *KeystoneWebhook) ValidateCreate(_ context.Context, obj *Keystone) (admission.Warnings, error)
func (w *KeystoneWebhook) ValidateUpdate(_ context.Context, _, newObj *Keystone) (admission.Warnings, error)
func (w *KeystoneWebhook) ValidateDelete(_ context.Context, _ *Keystone) (admission.Warnings, error)
```

- `ValidateCreate` and `ValidateUpdate` both delegate to the internal `validate()`
  method. There are no create-specific or update-specific rules.
- `ValidateDelete` always returns `nil` — deletion is unconditionally allowed.

### Validation Rules

The `validate()` method accumulates all errors in a `field.ErrorList` and returns a
single `apierrors.NewInvalid` error. It does **not** short-circuit on the first error.

| Rule | Field Path | Error Type | Condition |
| --- | --- | --- | --- |
| Replicas minimum | `spec.replicas` | `field.Invalid` | `replicas < 1`. Defense-in-depth alongside the `+kubebuilder:validation:Minimum=1` marker. |
| Cron expression | `spec.fernet.rotationSchedule` | `field.Invalid` | `cron.ParseStandard()` fails. Error message includes the parse failure details. |
| Duplicate plugin sections | `spec.plugins[i].configSection` | `field.Duplicate` | Two or more plugins share the same `configSection` value. |
| Policy source required | `spec.policyOverrides` | `field.Required` | `policyOverrides` is set but both `rules` and `configMapRef` are nil/empty. |
| Empty policy rule name | `spec.policyOverrides.rules` | `field.Invalid` | A key in `rules` map is the empty string. |
| Autoscaling maxReplicas minimum | `spec.autoscaling.maxReplicas` | `field.Invalid` | `maxReplicas < 1`. Defense-in-depth alongside the `+kubebuilder:validation:Minimum=1` marker (CC-0038). |
| Autoscaling minReplicas minimum | `spec.autoscaling.minReplicas` | `field.Invalid` | `minReplicas < 1` when set. Defense-in-depth alongside the `+kubebuilder:validation:Minimum=1` marker (CC-0038). |
| Autoscaling min exceeds max | `spec.autoscaling.minReplicas` | `field.Invalid` | `minReplicas > maxReplicas` when set (CC-0038). |
| Autoscaling no metric targets | `spec.autoscaling` | `field.Required` | Neither `targetCPUUtilization` nor `targetMemoryUtilization` is set. Defense-in-depth alongside the CEL XValidation rule (CC-0038). |
| uWSGI processes minimum | `spec.uwsgi.processes` | `field.Invalid` | `processes < 1` when `spec.uwsgi` is non-nil. Defense-in-depth alongside the `+kubebuilder:validation:Minimum=1` marker (CC-0040). |
| uWSGI threads minimum | `spec.uwsgi.threads` | `field.Invalid` | `threads < 1` when `spec.uwsgi` is non-nil. Defense-in-depth alongside the `+kubebuilder:validation:Minimum=1` marker (CC-0040). |
| Resource request exceeds limit | `spec.resources.requests.<resource>` | `field.Invalid` | A resource request exceeds its corresponding limit (e.g., CPU request 1000m > limit 500m). Checked per resource type when both requests and limits are set (CC-0042). |

**Error format:** All validation errors are returned as a structured
`apierrors.StatusError` with `GroupKind{Group: "keystone.openstack.c5c3.io", Kind: "Keystone"}`,
providing clear, field-specific error messages to the operator.

---

## Testing

The Keystone CRD has a three-layer test strategy (CC-0012):

1. **Unit tests** — fast, in-process tests for webhook logic (existing from CC-0011).
2. **Integration tests** — envtest-based tests that run a real API server + etcd to
   validate CRD schema, CEL rules, and webhooks through the full admission pipeline.
3. **E2E tests** — Chainsaw tests that deploy the operator to a real cluster and verify
   webhook rejection in a production-like environment.

### Running the Tests

| Layer | Command | Prerequisites |
| --- | --- | --- |
| Unit | `go test ./operators/keystone/api/v1alpha1/` | None |
| Integration | `go test -tags=integration ./operators/keystone/api/v1alpha1/` | `KUBEBUILDER_ASSETS` set to envtest binaries |
| E2E | `chainsaw test --test-dir tests/e2e/keystone/invalid-cr/` | Operator deployed to a cluster with webhooks active |

### envtest Integration Helper

The `operators/keystone/internal/testutil` package provides a Keystone-specific envtest
setup helper that configures CRD installation and webhook serving for integration tests.

```go
func SetupKeystoneEnvTest(
    t testing.TB,
    addToScheme func(*runtime.Scheme) error,
    registerWebhooks func(ctrl.Manager) error,
) (client.Client, context.Context, context.CancelFunc)
```

**Design decisions (CC-0012):**

- Uses a **local scheme** — `SharedScheme()` from `internal/common` is not modified.
  Only Keystone tests need Keystone types registered.
- Resolves CRD and webhook manifest paths via `runtime.Caller(0)` relative navigation,
  matching the pattern in `internal/common/testutil/envtest/setup.go`.
- Starts a controller-runtime manager with a webhook server bound to the envtest-allocated
  host, port, and certificate directory.
- Waits for the webhook server TLS endpoint to accept connections before returning.
- Tears down the environment automatically via `t.Cleanup()`.

**Parameters:**

| Name | Type | Description |
| --- | --- | --- |
| `addToScheme` | `func(*runtime.Scheme) error` | Registers Keystone API types (breaks import cycle between testutil and v1alpha1). |
| `registerWebhooks` | `func(ctrl.Manager) error` | Sets up webhook handlers with the manager. |

The `SkipIfEnvTestUnavailable` guard is re-exported from
`internal/common/testutil/envtest` for convenience.

### Integration Test Coverage

All integration tests use the `//go:build integration` tag and call
`testutil.SkipIfEnvTestUnavailable(t)` as the first statement.

#### CRD Installation and Valid CR Acceptance

| Test | Requirement | Behavior |
| --- | --- | --- |
| `TestIntegration_CRDInstalled` | CRD discoverable | Lists CRDs via apiextensions API; verifies `keystones.keystone.openstack.c5c3.io` is present. |
| `TestIntegration_ValidCRAccepted` | Happy-path admission | Creates a valid Keystone CR (brownfield database mode), verifies HTTP 201 and successful Get. |
| `TestIntegration_ValidCRWithClusterRefAccepted` | ClusterRef mode | Creates a valid CR using `database.clusterRef` and `cache.clusterRef`, verifies acceptance and readback. |

#### CEL Validation Rejection

| Test | Requirement | Trigger | Expected Error |
| --- | --- | --- | --- |
| `TestIntegration_CELRejectsDBBothClusterRefAndHost` | Mutual exclusivity | Both `database.clusterRef` and `database.host` set | Invalid/Forbidden containing "database" |
| `TestIntegration_CELRejectsCacheBothClusterRefAndServers` | Mutual exclusivity | Both `cache.clusterRef` and `cache.servers` set | Invalid/Forbidden containing "cache" |
| `TestIntegration_CELRejectsReplicasBelowMinimum` | Minimum constraint | `replicas = -1` (note: 0 is converted to 3 by the defaulting webhook, so -1 is used) | Invalid/Forbidden |
| `TestIntegration_CELRejectsMaxActiveKeysBelowMinimum` | Minimum constraint | `fernet.maxActiveKeys = 1` (below minimum of 3; 0 is defaulted to 3 by webhook) | Invalid/Forbidden |
| `TestIntegration_CELRejectsPolicyOverridesEmpty` | Policy source required | `policyOverrides` set with neither `rules` nor `configMapRef` | Invalid/Forbidden containing "policyOverrides" |

**Admission pipeline note:** In Kubernetes, the admission order is: mutating webhooks
then schema validation (CEL) then validating webhooks. The defaulting webhook converts
`replicas: 0` to `3` and `maxActiveKeys: 0` to `3` before CEL validation runs, so these
tests use values that bypass defaulting (negative or non-zero-but-below-minimum) to
exercise the CRD schema constraints.

#### Webhook Defaulting

| Test | Requirement | Behavior |
| --- | --- | --- |
| `TestIntegration_WebhookDefaultsSetsZeroValues` | Defaults applied | Creates a CR with zero-valued defaultable fields; verifies `replicas=3`, `cache.backend="dogpile.cache.pymemcache"`, `bootstrap.adminUser="admin"`, `bootstrap.region="RegionOne"`, `fernet.maxActiveKeys=3` after admission. |
| `TestIntegration_WebhookDefaultsPreservesExplicit` | Explicit values preserved | Creates a CR with `replicas=5` and `region="EU-West"`; verifies these values are not overwritten by the defaulting webhook. |
| `TestIntegration_ResourcesDefaultedWhenNil` | Resources defaulted | Creates a CR with `spec.resources` unset (`nil`); verifies the defaulting webhook injects `{requests: {memory: 256Mi, cpu: 100m}, limits: {memory: 512Mi, cpu: 500m}}` (CC-0042). |
| `TestIntegration_ResourcesPreservedWhenExplicit` | Explicit resources preserved | Creates a CR with explicit `spec.resources` (1Gi/2Gi memory, 200m/1 CPU); verifies the defaulting webhook does not overwrite them (CC-0042). |
| `TestIntegration_UWSGIDefaultsAppliedWhenEmpty` | uWSGI defaults applied | Creates a CR with `spec.uwsgi: {}` (all zero values); verifies processes=2, threads=1, httpKeepAlive=true after admission (CC-0040). |
| `TestIntegration_UWSGIExplicitValuesPreserved` | Explicit uWSGI preserved | Creates a CR with `spec.uwsgi.processes=4, threads=4`; verifies these values are not overwritten by the defaulting webhook (CC-0040). |
| `TestIntegration_UWSGIPartialDefaulting` | Partial uWSGI defaults | Creates a CR with only `spec.uwsgi.processes=4`; verifies threads=1 is defaulted while processes=4 is preserved (CC-0040). |
| `TestIntegration_UWSGINilPreserved` | uWSGI nil preserved | Creates a CR without `spec.uwsgi`; verifies the field remains `nil` after admission — webhook does not inject a default struct (CC-0040). |

#### Webhook Validation Rejection

| Test | Requirement | Trigger | Expected Error |
| --- | --- | --- | --- |
| `TestIntegration_ResourcesRequestExceedsLimitRejected` | Request must not exceed limit | `spec.resources` with CPU request 1000m > limit 500m | Invalid/Forbidden containing "resources" (CC-0042). |
| `TestIntegration_UWSGIProcessesBelowMinimumRejected` | Processes minimum | `spec.uwsgi.processes` below minimum (bypassing defaulting) | Invalid/Forbidden containing "uwsgi" (CC-0040). |
| `TestIntegration_UWSGIThreadsBelowMinimumRejected` | Threads minimum | `spec.uwsgi.threads` below minimum (bypassing defaulting) | Invalid/Forbidden containing "uwsgi" (CC-0040). |

### Chainsaw E2E Tests

E2E tests live in `tests/e2e/keystone/` and use the Chainsaw framework
(`chainsaw.kyverno.io/v1alpha2`). The `invalid-cr` suite below verifies webhook
rejection in a real cluster with the operator deployed. For the 9 reconciler E2E test
suites (basic-deployment, missing-secret, fernet-rotation, scale, deletion-cleanup,
policy-overrides, middleware-config, brownfield-database, image-upgrade), see
[Keystone E2E Test Suites](./keystone-e2e-tests.md) (CC-0016).

#### invalid-cr Suite

| Step | Manifest | Requirement | Expected Error |
| --- | --- | --- | --- |
| `invalid-cron-expression-rejected` | `00-invalid-cron.yaml` | Invalid cron | Error containing "rotationSchedule" and "invalid cron expression" |
| `duplicate-plugin-config-section-rejected` | `01-duplicate-plugins.yaml` | Duplicate configSection | Error containing "configSection" and "Duplicate value" |

Each step uses `apply` with `expect` to assert that the `$error` variable is non-null
and contains the expected field-level error message.

#### uwsgi Suite (CC-0040)

The `uwsgi` suite (`tests/e2e/keystone/uwsgi/`) validates that `spec.uwsgi` values
propagate to the Deployment container command in a real cluster with the operator
deployed and reconciling.

| Step | Description | Assertion |
| --- | --- | --- |
| Step 1 | Apply Keystone CR without explicit `spec.uwsgi` | CR created |
| Step 2 (`step-2-assert-default-uwsgi-args`) | Assert Deployment command contains default uWSGI args | Container command includes `--processes 2 --threads 1 --http-keepalive` |
| Step 3 | Patch CR with `spec.uwsgi: {processes: 3, threads: 3, httpKeepAlive: false}` | Patch applied |
| Step 4 (`step-4-assert-custom-uwsgi-args`) | Assert Deployment command updated with custom values | Container command includes `--processes 3 --threads 3`; `--http-keepalive` is absent |

---

## CRD Generation

The CRD manifest and DeepCopy methods are generated by `controller-gen`:

| Target | Command | Output |
| --- | --- | --- |
| DeepCopy | `make generate` | `operators/keystone/api/v1alpha1/zz_generated.deepcopy.go` |
| CRD YAML | `make manifests` | `operators/keystone/config/crd/bases/keystone.openstack.c5c3.io_keystones.yaml` |

Both targets are parameterized by operator directory in the Makefile. Generated
`zz_generated.*.go` files are excluded from linting via `.golangci.yml`.

### Generated DeepCopy Types

`zz_generated.deepcopy.go` provides `DeepCopyObject()` and `DeepCopyInto()` for:

- `Keystone`
- `KeystoneList`
- `KeystoneSpec`
- `KeystoneStatus`
- `AutoscalingSpec`
- `UWSGISpec`
- `FernetSpec`
- `FederationSpec`
- `BootstrapSpec`

---

## File Layout

```text
operators/keystone/
├── api/v1alpha1/
│   ├── groupversion_info.go          GroupVersion, SchemeBuilder, AddToScheme
│   ├── keystone_types.go             CRD types + init() scheme registration
│   ├── keystone_webhook.go           Defaulting + validating webhooks
│   ├── keystone_types_test.go        Type and scheme registration tests
│   ├── keystone_webhook_test.go      Webhook unit tests (table-driven)
│   ├── integration_test.go           envtest integration tests (CC-0012)
│   └── zz_generated.deepcopy.go     Generated DeepCopy methods
├── config/crd/bases/
│   └── keystone.openstack.c5c3.io_keystones.yaml  Generated CRD manifest
├── config/webhook/
│   ├── manifests.yaml                Generated webhook configurations
│   └── ...
├── internal/testutil/
│   └── envtest_setup.go              Keystone-specific envtest helper (CC-0012)
└── main.go                           Scheme registration + bootstrap + webhook wiring

tests/e2e/keystone/
├── basic-deployment/                 Happy-path reconciliation E2E (CC-0016)
├── missing-secret/                   Secret dependency recovery E2E (CC-0016)
├── fernet-rotation/                  Fernet key rotation E2E (CC-0016)
├── scale/                            Replica scaling E2E (CC-0016)
├── deletion-cleanup/                 Garbage collection E2E (CC-0016)
├── policy-overrides/                 oslo.policy integration E2E (CC-0016)
├── middleware-config/                Middleware pipeline E2E (CC-0016)
├── brownfield-database/              External database mode E2E (CC-0016)
├── image-upgrade/                    Rolling image upgrade E2E (CC-0016)
├── uwsgi/                            uWSGI field propagation E2E (CC-0040)
│   ├── chainsaw-test.yaml            Chainsaw E2E test definition
│   ├── 00-keystone-cr.yaml           Keystone CR without explicit uWSGI
│   └── 01-patch-custom-uwsgi.yaml    Patch with custom uWSGI values
└── invalid-cr/
    ├── chainsaw-test.yaml            Chainsaw E2E test definition (CC-0012)
    ├── 00-invalid-cron.yaml          Invalid cron expression CR manifest
    └── 01-duplicate-plugins.yaml     Duplicate plugin configSection CR manifest
```

This layout is the canonical pattern for all CobaltCore operators. New operators
should replicate this directory structure.
