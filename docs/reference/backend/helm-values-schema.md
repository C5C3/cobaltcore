---
title: Helm Values Schema
quadrant: backend
feature: CC-0069
---

# Helm Values Schema

Reference documentation for the `values.schema.json` JSON Schema (CC-0069) that validates
Helm chart values for the keystone-operator chart. Helm enforces this schema automatically
during `helm install`, `helm upgrade`, `helm lint`, and `helm template`.

## File Location

```text
operators/keystone/helm/keystone-operator/values.schema.json
```

## Schema Overview

The schema uses JSON Schema Draft-07 and defines constraints for every configurable
parameter in `values.yaml`. No additional properties are allowed at any object level,
ensuring typos and unsupported keys are caught at deploy time rather than silently ignored.

| Property | Value |
| --- | --- |
| JSON Schema Draft | `draft-07` |
| Chart version | `0.2.0` (bumped from `0.1.0` to signal the schema validation feature) |
| `additionalProperties` | `false` at all object levels |

## Validated Properties

### image

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `image.repository` | `string` | — | `ghcr.io/c5c3/keystone-operator` |
| `image.tag` | `string` | — | `""` |
| `image.pullPolicy` | `string` | enum: `Always`, `IfNotPresent`, `Never` | `IfNotPresent` |

### replicas

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `replicas` | `integer` | minimum: `1` | `2` |

### resources

Resource fields (`cpu`, `memory`) use a shared `resourceQuantity` definition that accepts
either a Kubernetes quantity string matching the pattern
`^(\.[0-9]+|[0-9]+(\.[0-9]*)?)((e[0-9]+)|(m|k|M|G|T|P|E|Ki|Mi|Gi|Ti|Pi|Ei))?$`
or a non-negative number. The pattern enforces mutual exclusion between exponent notation
(`e[0-9]+`) and SI/binary suffixes — values like `1e3m` or `1e3Ki` are rejected because
the Kubernetes quantity grammar does not allow combining both.

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `resources.limits.cpu` | `resourceQuantity` | pattern or number >= 0 | `500m` |
| `resources.limits.memory` | `resourceQuantity` | pattern or number >= 0 | `128Mi` |
| `resources.requests.cpu` | `resourceQuantity` | pattern or number >= 0 | `10m` |
| `resources.requests.memory` | `resourceQuantity` | pattern or number >= 0 | `64Mi` |

### rbac

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `rbac.namespaceScoped` | `boolean` | conditional: requires `webhook.enabled=false` | `false` |

**Conditional constraint (REQ-010):** When `rbac.namespaceScoped` is `true`, the schema
requires `webhook.enabled` to be `false`. This is enforced via a top-level `if`/`then`
rule. Namespace-scoped RBAC (CC-0043) cannot coexist with webhooks because
`ValidatingWebhookConfiguration` and `MutatingWebhookConfiguration` are cluster-scoped
resources that require a `ClusterRole` to manage.

### leaderElection

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `leaderElection.enabled` | `boolean` | — | `true` |

### webhook

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `webhook.enabled` | `boolean` | — | `true` |

### metrics

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `metrics.port` | `integer` | minimum: `1`, maximum: `65535` | `8080` |

### serviceAccount

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `serviceAccount.create` | `boolean` | — | `true` |
| `serviceAccount.name` | `string` | — | `""` |

### Name Overrides

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `nameOverride` | `string` | — | `""` |
| `fullnameOverride` | `string` | — | `""` |

## Validation Behavior

Helm validates the merged values object against the schema before template rendering.
Validation failures produce errors with the JSON path of the offending value:

```text
Error: values don't meet the specifications of the schema(s) in the following chart(s):
keystone-operator:
- at '/image/pullPolicy': value must be one of 'Always', 'IfNotPresent', 'Never'
```

### Commands That Trigger Validation

| Command | Validates |
| --- | --- |
| `helm install` | Yes |
| `helm upgrade` | Yes |
| `helm lint` | Yes |
| `helm template` | Yes |

### Resource Quantity Definition

The `resourceQuantity` definition uses `anyOf` to accept two formats:

1. **String format** — matches the Kubernetes resource quantity pattern
   (e.g., `500m`, `128Mi`, `1Gi`, `0.5`).
2. **Numeric format** — any non-negative number (e.g., `0`, `0.5`, `1`).

This allows both `cpu: "500m"` (string) and `cpu: 0.5` (number) as valid inputs while
rejecting malformed strings like `cpu: "not-valid"` and negative numbers like `cpu: -1`.

## Test Coverage

Schema validation is tested with helm-unittest in
`operators/keystone/helm/keystone-operator/tests/schema_validation_test.yaml`.

### Negative Tests (rejection)

| Category | Example | Requirement |
| --- | --- | --- |
| Type violations | `replicas: "abc"` (string instead of integer) | REQ-001, REQ-002 |
| Enum violations | `image.pullPolicy: "InvalidPolicy"` | REQ-003 |
| Range violations | `replicas: 0`, `metrics.port: 65536` | REQ-004 |
| Unknown properties | `image.digest: "sha256:abc"` | REQ-008 |
| Invalid quantities | `resources.limits.cpu: "not-valid"` | REQ-009 |
| Exponent+suffix | `cpu: "1e3m"`, `memory: "1e3Ki"` | REQ-009 |
| Conditional constraint | `rbac.namespaceScoped=true` with `webhook.enabled=true` | REQ-010 |

### Positive Tests (acceptance)

| Category | Example | Requirement |
| --- | --- | --- |
| Custom replicas | `replicas: 5` | CC-0069 |
| Custom metrics port | `metrics.port: 9090` | CC-0069 |
| String resource quantities | `cpu: "2"`, `memory: "1Gi"` | CC-0069 |
| Numeric resource quantities | `cpu: 0.5` | CC-0069 |
| Exponent-only quantities | `cpu: "1e3"` | CC-0069 |
| Conditional constraint | `rbac.namespaceScoped=true` with `webhook.enabled=false` | CC-0069 |
