---
title: Helm Values Schema
quadrant: backend
---

# Helm Values Schema

Reference documentation for the `values.schema.json` JSON Schema that validates
Helm chart values for the keystone-operator chart. Helm enforces this schema automatically
during `helm install`, `helm upgrade`, `helm lint`, and `helm template`.

## File Location

```text
operators/keystone/helm/keystone-operator/values.schema.json
```

::: warning Generated file
This schema is generated from the shared source in
`hack/gen-helm-values-schema.py`, which also emits the c5c3-operator schema from
the same definitions so the two cannot drift. Edit the generator and run
`make gen-helm-schema`; do not hand-edit `values.schema.json` —
`make verify-helm-schema` (run in CI) fails on drift.
:::

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

**Conditional constraint:** When `rbac.namespaceScoped` is `true`, the schema
requires `webhook.enabled` to be `false`. This is enforced via a top-level `if`/`then`
rule. Namespace-scoped RBAC cannot coexist with webhooks because
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

| Category | Example |
| --- | --- |
| Type violations | `replicas: "abc"` (string instead of integer) |
| Enum violations | `image.pullPolicy: "InvalidPolicy"` |
| Range violations | `replicas: 0`, `metrics.port: 65536` |
| Unknown properties | `image.digest: "sha256:abc"` |
| Invalid quantities | `resources.limits.cpu: "not-valid"` |
| Exponent+suffix | `cpu: "1e3m"`, `memory: "1e3Ki"` |
| Conditional constraint | `rbac.namespaceScoped=true` with `webhook.enabled=true` |

### Positive Tests (acceptance)

| Category | Example |
| --- | --- |
| Custom replicas | `replicas: 5` |
| Custom metrics port | `metrics.port: 9090` |
| String resource quantities | `cpu: "2"`, `memory: "1Gi"` |
| Numeric resource quantities | `cpu: 0.5` |
| Exponent-only quantities | `cpu: "1e3"` |
| Conditional constraint | `rbac.namespaceScoped=true` with `webhook.enabled=false` |
