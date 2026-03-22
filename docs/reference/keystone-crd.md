---
title: Keystone CRD API Reference
quadrant: operator
feature: CC-0011, CC-0012, CC-0016
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
| `extraConfig` | `map[string]map[string]string` | No | `nil` | Free-form INI sections for additional configuration. |

### CEL Validation Rules

The CRD includes structural validation rules enforced by the API server before
webhooks are invoked:

| Field | Rule | Error Message |
| --- | --- | --- |
| `spec.database` | `has(self.clusterRef) != has(self.host)` | "exactly one of clusterRef or host must be set" |
| `spec.policyOverrides` | `self.rules != null \|\| self.configMapRef != null` | "at least one of rules or configMapRef must be set" |
| `spec.replicas` | Minimum: 1 | — |
| `spec.fernet.maxActiveKeys` | Minimum: 3 | — |

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

### Chainsaw E2E Tests

E2E tests live in `tests/e2e/keystone/` and use the Chainsaw framework
(`chainsaw.kyverno.io/v1alpha2`). The `invalid-cr` suite below verifies webhook
rejection in a real cluster with the operator deployed. For the 9 reconciler E2E test
suites (basic-deployment, missing-secret, fernet-rotation, scale, deletion-cleanup,
policy-overrides, middleware-config, brownfield-database, image-upgrade), see
[Keystone E2E Test Suites](./keystone-e2e-tests.md) (CC-0016).

| Step | Manifest | Requirement | Expected Error |
| --- | --- | --- | --- |
| `invalid-cron-expression-rejected` | `00-invalid-cron.yaml` | Invalid cron | Error containing "rotationSchedule" and "invalid cron expression" |
| `duplicate-plugin-config-section-rejected` | `01-duplicate-plugins.yaml` | Duplicate configSection | Error containing "configSection" and "Duplicate value" |

Each step uses `apply` with `expect` to assert that the `$error` variable is non-null
and contains the expected field-level error message.

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
└── invalid-cr/
    ├── chainsaw-test.yaml            Chainsaw E2E test definition (CC-0012)
    ├── 00-invalid-cron.yaml          Invalid cron expression CR manifest
    └── 01-duplicate-plugins.yaml     Duplicate plugin configSection CR manifest
```

This layout is the canonical pattern for all CobaltCore operators. New operators
should replicate this directory structure.
