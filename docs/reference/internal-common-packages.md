---
title: Shared Utility Packages
quadrant: backend
---

# Shared Utility Packages

**Module:** `internal/common`
**Feature:** CC-0004

Pure-function utility packages for OpenStack operator reconcilers. All functions operate on Go maps, slices, and structs with no Kubernetes client dependency (except `conditions/` which uses `metav1.Condition`).

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

## Dependencies

- **CC-0004** — implements all packages
- **CC-0005** — adds K8s-client-dependent functions (see [Kubernetes-Interacting Packages](internal-common-k8s-packages.md))
- **CC-0011** — will consume types in CRD definitions with kubebuilder markers
