---
title: Helm Values Schema
quadrant: backend
---

# Helm Values Schema

Reference documentation for the `values.schema.json` JSON Schema that validates
the Helm chart values of every CobaltCore operator — **keystone-operator** and
**c5c3-operator**, plus the glance-operator, horizon-operator, and
placement-operator siblings. Helm
enforces this schema automatically during `helm install`, `helm upgrade`,
`helm lint`, and `helm template`.

All charts share one schema source, so the value tables below apply to every
operator unchanged. The few keys that exist on only some charts are called out
in the [per-operator applicability matrix](#per-operator-applicability). Chart
values themselves — the keystone-operator and c5c3-operator defaults, resource
recommendations, and ready-to-use override snippets — are in
[Values by operator](#values-by-operator) at the end.

## File Location

Each chart ships its own generated copy of the schema:

| Chart | Schema path |
| --- | --- |
| keystone-operator | `operators/keystone/helm/keystone-operator/values.schema.json` |
| c5c3-operator | `operators/c5c3/helm/c5c3-operator/values.schema.json` |
| glance-operator | `operators/glance/helm/glance-operator/values.schema.json` |
| horizon-operator | `operators/horizon/helm/horizon-operator/values.schema.json` |
| placement-operator | `operators/placement/helm/placement-operator/values.schema.json` |

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
| `additionalProperties` | `false` at all object levels |

Each chart carries its own version in its `Chart.yaml`; the schema is not tied
to a chart version, so it is not repeated here.

## Per-Operator Applicability

Every chart exposes the same core value keys. Three keys are conditional on what
a chart actually ships, so they are present only where they apply:

| Key | keystone-operator | c5c3-operator | glance-operator | horizon-operator | placement-operator |
| --- | :---: | :---: | :---: | :---: | :---: |
| `image`, `replicas`, `resources`, `rbac`, `leaderElection`, `webhook`, `metrics`, `logging`, `monitoring`, `serviceAccount`, `controller`, `nameOverride`, `fullnameOverride` | Yes | Yes | Yes | Yes | Yes |
| `networkPolicy` | Yes | — | Yes | Yes | Yes |
| `federation` | Yes | — | — | — | — |

- `networkPolicy` is emitted for every chart that ships a
  `templates/networkpolicy.yaml` (all service operators); the c5c3-operator does
  not, so the key is rejected there.
- `federation` is keystone-only — only the keystone-operator registers the
  SSRF-guarded federation-metadata client flag.
- `controller.maxConcurrentReconciles` is accepted by every chart, but only the
  controllers that opt in consume it (see [controller](#controller)).

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

### controller

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `controller.maxConcurrentReconciles` | `integer` | minimum: `1` | unset |

The maximum number of CRs that may reconcile concurrently
(controller-runtime `MaxConcurrentReconciles`), rendered as
`--max-concurrent-reconciles`. Left unset in the c5c3-operator chart; the
keystone-operator chart defaults it to `2` (the upstream default of `1`
serialises reconciles across CRs, so one slow or flapping CR delays every
other). Raise to 5–10 for fleets with many CRs. The key is accepted by every
chart, but only controllers that opt in consume it — the c5c3-operator accepts
the flag without acting on it yet.

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

See [How to enable the Keystone operator metrics endpoint](../../guides/keystone/enable-keystone-operator-metrics.md).

### networkPolicy

The `networkPolicy` block (default-off operator pod hardening with fail-closed
render guards) is validated by the schema on every chart that ships a
`templates/networkpolicy.yaml` — keystone-operator, glance-operator,
horizon-operator, and placement-operator. The c5c3-operator does not ship the
template, so the key is rejected there. Its fields are documented in
[Keystone Operator NetworkPolicy](../keystone/keystone-operator-networkpolicy.md).

### federation

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `federation.metadataAllowCidrs` | `array` | each item matches the shared `cidr` pattern definition | `[]` |

The `federation` block is keystone-only — it is layered onto the
keystone-operator schema alone (unlike `networkPolicy`, which every service
operator registers; the sibling operators do not register the federation flag).
Each `metadataAllowCidrs` entry is validated against the same shared
`cidr` pattern definition the `networkPolicy` block reuses. The rendered
`--federation-metadata-allow-cidrs` flag and its runtime semantics are covered in
the [keystone-operator packaging reference](./keystone-operator-packaging.md#federation).

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

## Values by Operator

The schema above is shared; the shipped **defaults** differ only where a chart
does not carry a key. The table below is the practical values summary for the
two operators this reference centres on. The glance-operator, horizon-operator, and
placement-operator follow the keystone-operator defaults — including `controller.maxConcurrentReconciles: 2`
— and differ only by their own `image.repository`, their `webhook.enabled`
description, and by carrying no `federation` key (see the
[applicability matrix](#per-operator-applicability)).

| Key | keystone-operator | c5c3-operator |
| --- | --- | --- |
| `image.repository` | `ghcr.io/c5c3/keystone-operator` | `ghcr.io/c5c3/c5c3-operator` |
| `image.tag` | `""` (falls back to `.Chart.AppVersion`) | `""` (falls back to `.Chart.AppVersion`) |
| `image.pullPolicy` | `IfNotPresent` | `IfNotPresent` |
| `replicas` | `2` | `2` |
| `resources.requests` | `cpu: 10m`, `memory: 64Mi` | `cpu: 10m`, `memory: 64Mi` |
| `resources.limits` | `cpu: 500m`, `memory: 128Mi` | `cpu: 500m`, `memory: 128Mi` |
| `rbac.namespaceScoped` | `false` | `false` |
| `leaderElection.enabled` | `true` | `true` |
| `controller.maxConcurrentReconciles` | `2` | unset (accepted, not consumed) |
| `webhook.enabled` | `true` | `true` |
| `metrics.port` | `8080` | `8080` |
| `monitoring.serviceMonitor.enabled` | `false` | `false` |
| `networkPolicy.enabled` | `false` | key not present |
| `federation.metadataAllowCidrs` | `[]` | key not present |

`replicas: 2` runs an active/standby pair: leader election keeps exactly one
replica reconciling, and the standby takes over on pod loss. The two operators
never reconcile the same CRs, so they are sized independently.

### Resource Recommendations

The shipped defaults (`requests: 10m/64Mi`, `limits: 500m/128Mi`) are tuned for a
laptop-sized kind cluster and a handful of CRs. They are a floor, not a
production target: the requests are deliberately tiny so a dev cluster schedules
the pod anywhere, and the 128Mi memory limit is close to the working set once an
operator reconciles many CRs concurrently.

| Profile | requests | limits | Notes |
| --- | --- | --- | --- |
| kind / dev (default) | `cpu: 10m`, `memory: 64Mi` | `cpu: 500m`, `memory: 128Mi` | Ships as-is; fits a single-node kind. |
| Production baseline | `cpu: 100m`, `memory: 128Mi` | `cpu: "1"`, `memory: 256Mi` | Reserve real headroom; raise the memory limit so a reconcile burst does not OOM-kill the pod. |
| Large fleet | `cpu: 250m`, `memory: 256Mi` | `cpu: "2"`, `memory: 512Mi` | Pair with a higher `controller.maxConcurrentReconciles` (5–10) so the extra CPU is usable. |

Treat these as starting points and confirm against your own load — the
[reconcile-performance benchmark](../testing/reconcile-performance-benchmark.md)
measures per-reconcile cost, and a `VerticalPodAutoscaler` in recommendation
mode is a reliable way to right-size the requests before pinning them.

### Example Overrides

Both charts install with `helm install <release> <chart> -f overrides.yaml`. Only
the keys you change need to appear; everything else keeps its default.

**keystone-operator — production hardening.** Raise the resources, publish a
`ServiceMonitor`, widen concurrency, allow federation-metadata fetches against
the in-cluster identity provider, and turn on the operator NetworkPolicy:

```yaml
# keystone-overrides.yaml
resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: "1"
    memory: 256Mi
controller:
  maxConcurrentReconciles: 5
monitoring:
  serviceMonitor:
    enabled: true          # requires the prometheus-operator CRDs in-cluster
federation:
  metadataAllowCidrs:
    - 10.96.0.0/12         # cluster Service CIDR — reach a trusted in-cluster IdP
networkPolicy:
  enabled: true
  kubeApiServer:
    # cluster-specific — discover with: kubectl get endpoints kubernetes -o json
    cidrs: ["10.96.0.1/32"]
    ports: [6443]
```

**c5c3-operator — single-namespace, hardened.** The ControlPlane operator has no
`federation` or `networkPolicy` key; namespace-scoped RBAC is the main hardening
knob (it requires `webhook.enabled: false`):

```yaml
# c5c3-overrides.yaml
resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: "1"
    memory: 256Mi
rbac:
  namespaceScoped: true    # bounds the operator to its release namespace
webhook:
  enabled: false           # required by namespaceScoped: true
monitoring:
  serviceMonitor:
    enabled: true
```

See the [Multi-Tenant Deployment guide](../../guides/multi-tenant-deployment.md#security-trade-off-the-cluster-wide-rbac-default)
for the privilege-escalation path `rbac.namespaceScoped: true` closes and the
[capabilities that still need cluster scope](../../guides/multi-tenant-deployment.md#when-cluster-wide-rbac-is-still-required).
