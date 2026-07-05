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
`hack/gen-helm-values-schema.py`, which discovers every chart under
`operators/*/helm/*-operator/` and emits each chart's schema from the same
definitions so they cannot drift (a new operator only needs a
`WEBHOOK_ENABLED_DESCRIPTIONS` entry naming its CR kind). Edit the generator
and run
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
| Chart version | `0.5.0` |
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

**Production recommendation:** For a control plane confined to a single
namespace, set `rbac.namespaceScoped: true` to bound a compromised operator pod
to one namespace's Secrets instead of the cluster-wide Secret access the default
`ClusterRole` grants — see
[Multi-Tenant Deployment → Security trade-off](../../guides/multi-tenant-deployment.md#security-trade-off-the-cluster-wide-rbac-default)
for the privilege-escalation path this closes. The default stays `false` because
[some capabilities still need cluster scope](../../guides/multi-tenant-deployment.md#when-cluster-wide-rbac-is-still-required).

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

### monitoring

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `monitoring.serviceMonitor.enabled` | `boolean` | requires the `monitoring.coreos.com` CRDs in-cluster when enabled | `false` |
| `monitoring.serviceMonitor.interval` | `string` | pattern: Go duration (`15s`, `30s`, `1m`) or `0` for the global default | `30s` |

See [How to enable the Keystone operator metrics endpoint](../../guides/enable-keystone-operator-metrics.md).

### networkPolicy

The `networkPolicy` block (default-off operator pod hardening with fail-closed
render guards) is validated by the schema as well; its fields are documented in
[Keystone Operator NetworkPolicy](../keystone/keystone-operator-networkpolicy.md).

### operator-library

A reserved, empty values namespace for the `operator-library` library subchart.
The library carries no configurable values; Helm injects this key during values
coalescing, so the root-level `additionalProperties: false` must permit it.

### logging

The operator runs the controller-runtime zap logger in the production profile by
default (`development: false`): JSON encoder, info-level verbosity, and stack traces
only at error level. Override these for human-readable console output during local
development. Each value maps to a controller-runtime `--zap-*` flag and is omitted
from the operator args when left at its default, so the production profile is the
behaviour unless a field is set explicitly.

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `logging.development` | `boolean` | — | `false` |
| `logging.level` | `string` | pattern: `debug`, `info`, `error`, `panic`, or a positive integer (empty allowed) | `""` |
| `logging.encoder` | `string` | enum: `json`, `console` (empty allowed) | `""` |

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
| Logging constraints | `logging.development: "yes"`, `logging.encoder: "xml"`, `logging.level: "verbose"` |

### Positive Tests (acceptance)

| Category | Example |
| --- | --- |
| Custom replicas | `replicas: 5` |
| Custom metrics port | `metrics.port: 9090` |
| String resource quantities | `cpu: "2"`, `memory: "1Gi"` |
| Numeric resource quantities | `cpu: 0.5` |
| Exponent-only quantities | `cpu: "1e3"` |
| Conditional constraint | `rbac.namespaceScoped=true` with `webhook.enabled=false` |
| Logging overrides | `development: true`, `level: debug`, `encoder: console` |
