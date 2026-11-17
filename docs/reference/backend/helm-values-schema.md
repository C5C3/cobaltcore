---
title: Helm Values Schema
quadrant: backend
---

# Helm Values Schema

Reference documentation for the `values.schema.json` JSON Schema that validates
the Helm chart values of every CobaltCore OpenStack operator chart:
keystone-operator, c5c3-operator, glance-operator, horizon-operator,
placement-operator, barbican-operator, ovn-operator, neutron-operator,
cinder-operator and nova-operator. Helm enforces this schema automatically
during `helm install`, `helm upgrade`, `helm lint`, and `helm template`.

All charts share one schema generator, so the constraints and field shapes
below apply across the operator charts. The `image.repository` default is read
from each chart's `values.yaml`; other keys that exist on only some charts are
called out in the [per-operator applicability matrix](#per-operator-applicability).
Chart values themselves, including the keystone-operator and c5c3-operator
defaults, resource recommendations, and ready-to-use override snippets, are in
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
| barbican-operator | `operators/barbican/helm/barbican-operator/values.schema.json` |
| ovn-operator | `operators/ovn/helm/ovn-operator/values.schema.json` |
| neutron-operator | `operators/neutron/helm/neutron-operator/values.schema.json` |
| cinder-operator | `operators/cinder/helm/cinder-operator/values.schema.json` |
| nova-operator | `operators/nova/helm/nova-operator/values.schema.json` |
| operator-library-testbed | `operators/shared/helm/operator-library-testbed/values.schema.json` (the library's test consumer, never published) |

::: warning Generated file
This schema is generated from the shared source in
`hack/gen-helm-values-schema.py`, which discovers every application chart under
`operators/*/helm/` and emits each chart's schema from the same definitions so
they cannot drift. The generator names no operator: the image repository
default comes from the chart's `values.yaml`, the `webhook.enabled`
description lists the CR kinds found in the operator's `config/webhook`
manifests, and `networkPolicy` is included when the chart ships
`templates/networkpolicy.yaml`. What a single chart adds on top lives next to
that chart in `values.schema.extras.json`, a partial schema that is deep-merged
into the shared one (objects merge recursively, lists concatenate). Edit the
generator or a chart's extras file and run `make gen-helm-schema`; do not
hand-edit `values.schema.json`. `make verify-helm-schema` (run in CI) fails
when generated schemas drift.
:::

## Schema Overview

The schema uses JSON Schema Draft-07 and defines constraints for every configurable
parameter in `values.yaml`. The root and objects whose schemas set
`additionalProperties: false` reject undeclared properties. Other maps and
passthrough values follow their own schemas, which may allow dynamic keys or
Kubernetes fields.

| Property | Value |
| --- | --- |
| JSON Schema Draft | `draft-07` |
| `additionalProperties` | `false` at the root and selected objects; other properties use field-specific rules |

Each chart carries its own version in its `Chart.yaml`; the schema is not tied
to a chart version, so it is not repeated here.

## Per-Operator Applicability

Every chart exposes the same core value keys: `image`, `replicas`,
`resources`, `nodeSelector`, `tolerations`, `priorityClassName`, `rbac`,
`leaderElection`, `controller`, `webhook`, `metrics`,
`logging`, `monitoring`, `serviceAccount`, `extraArgs`, `extraEnv`,
`extraVolumes`, `extraVolumeMounts`, `nameOverride`, `fullnameOverride`. A few keys are conditional on what a chart
ships, so they are present only where they apply:

| Key | Charts | Source |
| --- | --- | --- |
| `networkPolicy` | every chart that ships `templates/networkpolicy.yaml` — all but the c5c3-operator | shared generator |
| `networkPolicy.openBao.enabled` | barbican-operator | `values.schema.extras.json` |
| `rbac.secretStoreNamespaces` | barbican-operator | `values.schema.extras.json` |
| `federation` | keystone-operator | `values.schema.extras.json` |
| `barbicanOperator` | c5c3-operator | `values.schema.extras.json` |
| `openbaoOperator` | c5c3-operator | `values.schema.extras.json` |

- A key a chart does not carry is rejected there (`additionalProperties: false`).
- `controller.maxConcurrentReconciles` is accepted by every chart, but only the
  controllers that opt in consume it. The c5c3-operator passes it to its
  reconcilers (see [controller](#controller)).
- `controller.defaultImagePullPolicy` is accepted by every chart and applies to
  managed workload containers. The c5c3 operator does not render workload
  containers.
- Types, defaults, and usage notes for chart-specific keys appear under
  [Chart-specific values](#chart-specific-values).

## Validated Properties

### image

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `image.repository` | `string` | — | Chart-specific; see its `values.yaml` |
| `image.tag` | `string` | — | `""` |
| `image.digest` | `string` | empty or `sha256:` followed by 64 lowercase hexadecimal characters | `""` |
| `image.pullPolicy` | `string` | enum: `Always`, `IfNotPresent`, `Never` | `IfNotPresent` |

When `image.digest` is set, the chart renders the manager image as
`repository:tag@digest`; an empty `image.tag` uses `.Chart.AppVersion`.

### replicas

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `replicas` | `integer` | minimum: `1` | `2` |

### resources

Resource fields (`cpu`, `memory`) use a shared `resourceQuantity` definition
that accepts either a string matching the schema's resource-quantity pattern
`^(\.[0-9]+|[0-9]+(\.[0-9]*)?)((e[0-9]+)|(m|k|M|G|T|P|E|Ki|Mi|Gi|Ti|Pi|Ei))?$`
or a non-negative number. The pattern excludes exponent notation combined with SI or binary suffixes.
Values like `1e3m` or `1e3Ki` are rejected because the Kubernetes quantity
grammar does not allow combining both.

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `resources.limits.cpu` | `resourceQuantity` | pattern or number >= 0 | `500m` |
| `resources.limits.memory` | `resourceQuantity` | pattern or number >= 0 | `128Mi` |
| `resources.requests.cpu` | `resourceQuantity` | pattern or number >= 0 | `10m` |
| `resources.requests.memory` | `resourceQuantity` | pattern or number >= 0 | `64Mi` |

### nodeSelector, tolerations and priorityClassName

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `nodeSelector` | `object` | string values only (`stringMap`) | `{}` |
| `tolerations` | `array` of `object` | item keys `key`, `operator`, `value`, `effect`, `tolerationSeconds` and no others; `operator` enum: `Equal`, `Exists`, `Lt`, `Gt`; `effect` enum: `""`, `NoSchedule`, `PreferNoSchedule`, `NoExecute`; `tolerationSeconds` integer | `[]` |
| `priorityClassName` | `string` | maxLength: `253` | `""` |

Each key renders on the operator pod spec (`spec.template.spec`) only when it is
non-empty, so the defaults render the same pod template as a chart without the
keys. The fixed soft hostname spread (`topologySpreadConstraints`) renders beside
them, and the charts expose no affinity key. A toleration without `operator`
means `Equal`. `Lt` and `Gt` need the API server's
`TaintTolerationComparisonOperators` feature gate. The PriorityClass must exist
before the pods are created, or the API server rejects them.

The keys need operator-library 0.9.0, which the chart versions keystone-operator
0.11.0, c5c3-operator 0.14.0, horizon-operator 0.4.0, cinder-operator and
nova-operator 0.2.0, and barbican-, glance-, neutron-, ovn- and
placement-operator 0.3.0 carry; older chart versions reject them.

### rbac

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `rbac.namespaceScoped` | `boolean` | conditional: requires `webhook.enabled=false` | `false` |

**Conditional constraint:** When `rbac.namespaceScoped` is `true`, the schema
requires `webhook.enabled` to be `false`. This is enforced via a top-level `if`/`then`
rule. Helm renders the `ValidatingWebhookConfiguration` and
`MutatingWebhookConfiguration` of a release, and both are cluster-scoped: each
intercepts the CR kind in every namespace and routes to its own release. Several
namespace-scoped releases of one chart would each intercept every tenant's CRs,
and the webhooks read cluster-scoped kinds a namespaced `Role` grants nothing
for. A namespace-scoped install is admitted by a standalone webhook release
instead (see [webhook](#webhook)).

**Production recommendation:** For a control plane confined to a single
namespace, set `rbac.namespaceScoped: true` to bound a compromised operator pod
to one namespace's Secrets instead of the cluster-wide Secret access the default
`ClusterRole` grants. See
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
| `controller.defaultImagePullPolicy` | `string` | enum: `Always`, `IfNotPresent`, `Never` | unset |

The maximum number of CRs that may reconcile concurrently
(controller-runtime `MaxConcurrentReconciles`), rendered as
`--max-concurrent-reconciles`. The Keystone chart defaults it to `2`; the
binary default is also `2`. The upstream default of `1` serialises reconciles
across CRs, so one slow or flapping CR delays every other. Raise the value to
5–10 for fleets with many CRs. Controllers must opt in to use the setting. The
c5c3 operator passes it to its reconcilers; when the chart value is unset, they
use the binary default.

`controller.defaultImagePullPolicy` sets the fallback `imagePullPolicy` for
managed workload containers whose [`ImageSpec`](../keystone/keystone-crd.md#imagespec)
does not name a `pullPolicy`. The chart renders it as
`--default-image-pull-policy`. If unset, the operator uses `IfNotPresent` for a
digest and `Always` for a tag. Set it to `IfNotPresent` when nodes hold
preloaded images under the published tag, as CI does. No chart sets this value
by default; the nine service operator charts include it as a commented key.
This setting does not configure the operator manager container, which uses
`image.pullPolicy`.

### webhook

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `webhook.enabled` | `boolean` | — | `true` |
| `webhook.standalone` | `boolean` | conditional: requires `webhook.enabled=true` and `rbac.namespaceScoped=false` | `false` |

`webhook.standalone: true` runs the release as a standalone admission webhook:
the manager is started with `--enable-controllers=false` and without
`--leader-elect`, so it registers no controller and elects no leader, and the
`ClusterRole` renders the chart's webhook rule set
(`templates/_webhook-rbac-rules.tpl`) instead of the manager rules. One such
release per cluster serves the CRs of every namespace, for the namespace-scoped
installs of the chart, which set `webhook.enabled=false`. See
[Multi-Tenant Deployment → Admission webhooks](../../guides/multi-tenant-deployment.md#admission-webhooks-the-standalone-webhook-release).

**Conditional constraint:** When `webhook.standalone` is `true`, a second
top-level `if`/`then` rule requires `webhook.enabled` to be `true` and
`rbac.namespaceScoped` to be `false`: the release serves the webhooks of every
namespace under a `ClusterRole`.

The key needs operator-library 0.11.0, which the chart versions keystone-operator
0.13.0, c5c3-operator 0.17.0, horizon-operator 0.6.0, cinder-operator and
nova-operator 0.4.0, and barbican-, glance-, neutron-, ovn- and
placement-operator 0.5.0 carry; older chart versions reject it.

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
render guards) is validated by every chart that ships
`templates/networkpolicy.yaml`. All operator charts except c5c3 ship this
template, so c5c3 rejects the `networkPolicy` key. The template uses the shared
`operator-library.networkPolicy` implementation. Each service chart adds an
egress rule for its API health check through the
`operator-library.chart.networkPolicyEgress` hook. Barbican also adds OpenBao
egress, controlled by `networkPolicy.openBao.enabled`. Shared fields are
documented in
[Keystone Operator NetworkPolicy](../keystone/keystone-operator-networkpolicy.md).

### federation

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `federation.metadataAllowCidrs` | `array` | each item matches the shared `cidr` pattern definition | `[]` |

The `federation` block is keystone-only. That chart's
`values.schema.extras.json` adds it to the keystone-operator schema. The shared
generator emits `networkPolicy` for every chart that ships the template.
Each `metadataAllowCidrs` entry is validated against the same shared
`cidr` pattern definition used by the `networkPolicy` block. The rendered
`--federation-metadata-allow-cidrs` flag and its runtime semantics are covered in
the [keystone-operator packaging reference](./keystone-operator-packaging.md#federation).

### Chart-specific values

These values are defined by individual charts and are rejected by charts that
do not carry them.

| Chart | Field | Type or constraint | Default |
| --- | --- | --- | --- |
| barbican-operator | `rbac.secretStoreNamespaces` | map with non-empty namespace keys; each value is an array of non-empty ServiceAccount-name strings | `{}` |
| barbican-operator | `networkPolicy.openBao.enabled` | `boolean`; disable only when every store uses a non-standard port or an egress proxy covered by another policy | `true` |
| c5c3-operator | `barbicanOperator.namespace` | `string` | `barbican-system` |
| c5c3-operator | `barbicanOperator.serviceAccount` | `string` | `barbican-operator` |
| c5c3-operator | `openbaoOperator.namespace` | `string` | `openbao-operator-system` |

When Barbican's `networkPolicy.enabled` is `true`, the chart permits OpenBao
egress on TCP 8200 by default. Keep `networkPolicy.openBao.enabled` enabled
unless every store uses another port or an egress proxy allowed by a separate
policy.

### extraArgs, extraEnv, extraVolumes and extraVolumeMounts

| Field | Type | Constraint | Default |
| --- | --- | --- | --- |
| `extraArgs` | `array` | non-empty strings | `[]` |
| `extraEnv` | `array` | objects with a non-empty `name` (Kubernetes `EnvVar`) | `[]` |
| `extraVolumes` | `array` | objects with a non-empty `name` (Kubernetes `Volume`) | `[]` |
| `extraVolumeMounts` | `array` | objects with a non-empty `name` and `mountPath` (Kubernetes `VolumeMount`) | `[]` |

`extraArgs` and `extraEnv` are appended verbatim to the manager container.
`extraArgs` follows the flags the chart renders, and `extraEnv` follows the
variables derived from chart values. Use these fields for a flag or variable
the operator binary accepts but the chart exposes no value for.

`extraVolumes` goes verbatim into the pod's `volumes` and `extraVolumeMounts`
into the manager container's `volumeMounts`, each after the `webhook-certs`
entry. The chart renders `volumes` and `volumeMounts` when the webhook is
enabled or the list is non-empty, so the default render carries the webhook
certificate alone. The CI deploy helper uses them for the coverage counter
directory of the `e2e-operator`, `e2e-chaos` and `tempest` jobs
([hack/ci-deploy-operator.sh](../ci-cd/ci-workflow.md#hack-ci-deploy-operator-sh)).

The two volume keys need operator-library 0.12.0, which the chart versions
keystone-operator 0.15.0, c5c3-operator 0.20.0, horizon-operator 0.8.0,
cinder-, nova- and ovn-operator 0.6.0, and barbican-, glance-, neutron- and
placement-operator 0.7.0 carry. The root-level `additionalProperties: false`
of an older chart's schema rejects both keys.

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

### Released values

Each operator chart is also rendered with the `values.yaml` of its released
chart, passed as a user values file
(`helm template test <chart> -f <released values.yaml>`). A key that the chart
removed or renamed since that release fails the render with a schema error,
the error a deployment that still sets the key gets from `helm upgrade`. For a
released `webhook.oldName` key:

```text
Error: values don't meet the specifications of the schema(s) in the following chart(s):
keystone-operator:
- at '/webhook': additional properties 'oldName' not allowed
```

Any other render failure with the released values fails the chart too.

The released chart is the highest tag at `oci://ghcr.io/c5c3/charts/<chart>`,
pulled without a login. Main overwrites that tag on every publish without a
version bump, so in practice it is the chart main last published. A chart with
no published version is skipped with a notice. Keys that a deployment can set
but the released `values.yaml` does not carry, such as `nameOverride`, are not
checked.

The `Render the released chart values` step of
[helm-validate](../ci-cd/ci-workflow.md#helm-validate) runs the check on every
pull request that touches a chart. To run it locally for every chart or for one
operator:

```bash
make verify-helm-released-values
make verify-helm-released-values OPERATOR=keystone
```

An intended removal is waived in `hack/helm-removed-values.yaml`. The file maps
a chart name to a list of entries, each with the dotted values path of the key
and a reason that must not be empty:

```yaml
keystone-operator:
  - key: webhook.oldName
    reason: renamed to webhook.newName
```

The check deletes each waived key from the released values before the render
and prints the reason. Add the entry in the pull request that removes or renames
the key. Delete it once the check warns that the key is not in the values of the
released chart, which happens after main has published the chart without it.

### Resource Quantity Definition

The `resourceQuantity` definition uses `anyOf` to accept two formats:

1. **String format**: matches the schema pattern for decimal quantities and
   SI or binary suffixes
   (e.g., `500m`, `128Mi`, `1Gi`, `0.5`).
2. **Numeric format**: any non-negative number (e.g., `0`, `0.5`, `1`).

This allows both `cpu: "500m"` (string) and `cpu: 0.5` (number) as valid inputs while
rejecting malformed strings like `cpu: "not-valid"` and negative numbers like `cpu: -1`.

## Test Coverage

Schema validation is tested with helm-unittest once for the shared schema, in
`operators/shared/helm/operator-library-testbed/tests/schema_validation_test.yaml`;
each operator chart's own `tests/schema_validation_test.yaml` proves the chart
ships an enforced schema and covers the keys its extras file adds.

### Negative Tests (rejection)

| Category | Example |
| --- | --- |
| Type violations | `replicas: "abc"` (string instead of integer) |
| Enum violations | `image.pullPolicy: "InvalidPolicy"`, `controller.defaultImagePullPolicy: "Sometimes"` |
| Range violations | `replicas: 0`, `metrics.port: 65536` |
| Unknown properties | `unknownField: true`, `image.registry: ghcr.io` |
| Invalid image digest | `image.digest: "sha256:abc"` |
| Invalid quantities | `resources.limits.cpu: "not-valid"` |
| Exponent+suffix | `cpu: "1e3m"`, `memory: "1e3Ki"` |
| Conditional constraint | `rbac.namespaceScoped=true` with `webhook.enabled=true`; `webhook.standalone=true` with `webhook.enabled=false` or with `rbac.namespaceScoped=true` |
| Standalone webhook type | `webhook.standalone: "yes"` |
| Logging constraints | `logging.development: "yes"`, `logging.encoder: "xml"`, `logging.level: "verbose"` |
| Placement constraints | `nodeSelector: {role: 1}`, a toleration with `operator: Sometimes`, `effect: NoSchedul`, `tolerationSeconds: "300"` or the unknown key `taint`, `priorityClassName: 5`, a `priorityClassName` of 254 characters |
| Extra volumes | an `extraVolumeMounts` entry without `mountPath`, an `extraVolumes` entry without `name` |

### Positive Tests (acceptance)

| Category | Example |
| --- | --- |
| Custom replicas | `replicas: 5` |
| Custom metrics port | `metrics.port: 9090` |
| String resource quantities | `cpu: "2"`, `memory: "1Gi"` |
| Numeric resource quantities | `cpu: 0.5` |
| Exponent-only quantities | `cpu: "1e3"` |
| Conditional constraint | `rbac.namespaceScoped=true` with `webhook.enabled=false` |
| Standalone webhook | `webhook.standalone=true` |
| Logging overrides | `development: true`, `level: debug`, `encoder: console` |
| Operator image pull policy | `controller.defaultImagePullPolicy: Never` |
| Placement keys | `priorityClassName`, a `nodeSelector` label and two tolerations, one with `tolerationSeconds`; a toleration with `effect: ""`, which matches every effect |

## Values by Operator

The shared schema defines common values; chart-specific values appear above.
The table below compares common defaults for Keystone and c5c3. The Glance,
Horizon, Placement, OVN, Neutron, Cinder and Nova charts share Keystone's
common defaults, including `controller.maxConcurrentReconciles: 2`. They use
their own `image.repository` and omit the Keystone-only `federation` key.
Barbican shares those common defaults and has the additional values listed
above.

| Key | keystone-operator | c5c3-operator |
| --- | --- | --- |
| `image.repository` | `ghcr.io/c5c3/keystone-operator` | `ghcr.io/c5c3/c5c3-operator` |
| `image.tag` | `""` (falls back to `.Chart.AppVersion`) | `""` (falls back to `.Chart.AppVersion`) |
| `image.pullPolicy` | `IfNotPresent` | `IfNotPresent` |
| `replicas` | `2` | `2` |
| `resources.requests` | `cpu: 10m`, `memory: 64Mi` | `cpu: 10m`, `memory: 64Mi` |
| `resources.limits` | `cpu: 500m`, `memory: 128Mi` | `cpu: 500m`, `memory: 128Mi` |
| `nodeSelector` | `{}` | `{}` |
| `tolerations` | `[]` | `[]` |
| `priorityClassName` | `""` | `""` |
| `rbac.namespaceScoped` | `false` | `false` |
| `leaderElection.enabled` | `true` | `true` |
| `controller.maxConcurrentReconciles` | `2` | unset (binary default: `2`) |
| `controller.defaultImagePullPolicy` | unset | unset (no managed workload containers) |
| `webhook.enabled` | `true` | `true` |
| `webhook.standalone` | `false` | `false` |
| `metrics.port` | `8080` | `8080` |
| `monitoring.serviceMonitor.enabled` | `false` | `false` |
| `networkPolicy.enabled` | `false` | key not present |
| `federation.metadataAllowCidrs` | `[]` | key not present |

`replicas: 2` runs an active/standby pair. Leader election keeps exactly one
replica reconciling, and the standby takes over on pod loss. The two operators
never reconcile the same CRs, so they are sized independently.

### Resource Recommendations

The shipped defaults (`requests: 10m/64Mi`, `limits: 500m/128Mi`) suit a
laptop-sized kind cluster and a handful of CRs. Production deployments need
larger requests and limits. The requests are small so a dev cluster can schedule
the pod anywhere, and the 128Mi memory limit is close to the working set once an
operator reconciles many CRs concurrently.

| Profile | requests | limits | Notes |
| --- | --- | --- | --- |
| kind / dev (default) | `cpu: 10m`, `memory: 64Mi` | `cpu: 500m`, `memory: 128Mi` | Ships as-is; fits a single-node kind. |
| Production baseline | `cpu: 100m`, `memory: 128Mi` | `cpu: "1"`, `memory: 256Mi` | Reserve real headroom; raise the memory limit so a reconcile burst does not OOM-kill the pod. |
| Large fleet | `cpu: 250m`, `memory: 256Mi` | `cpu: "2"`, `memory: 512Mi` | Pair with a higher `controller.maxConcurrentReconciles` (5–10) so the extra CPU is usable. |

Treat these as starting points and confirm against your own load. The
[reconcile-performance benchmark](../testing/reconcile-performance-benchmark.md)
measures per-reconcile cost, and a `VerticalPodAutoscaler` in recommendation
mode can help right-size the requests before pinning them.

### Example Overrides

Both charts install with `helm install <release> <chart> -f overrides.yaml`.
Include only the keys you want to change; the chart applies its other defaults.

**keystone-operator: production hardening.** Raise the resources, publish a
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
    # cluster-specific: the kubernetes Endpoints addresses, not the Service VIP;
    # discover with: kubectl get endpoints kubernetes -n default -o json;
    # on Cilium, node addresses match only with --policy-cidr-match-mode=nodes
    # (cluster-wide); prefer a CiliumNetworkPolicy toEntities: [kube-apiserver]
    cidrs: ["10.0.0.10/32", "10.0.0.11/32", "10.0.0.12/32"]
    ports: [6443]
```

**c5c3-operator: resources and monitoring.** The ControlPlane operator has no
`federation` or `networkPolicy` key, and its chart refuses
`rbac.namespaceScoped: true`: the ControlPlane controller watches cluster-scoped
kinds and creates ClusterRoleBindings, which a namespaced Role cannot grant.

```yaml
# c5c3-overrides.yaml
resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: "1"
    memory: 256Mi
monitoring:
  serviceMonitor:
    enabled: true
```

See the [Multi-Tenant Deployment guide](../../guides/multi-tenant-deployment.md#when-cluster-wide-rbac-is-still-required)
for the charts that refuse the mode and the other capabilities that still need
cluster scope.

**Any operator chart: dedicated nodes and a priority class.** Run the operator
pods on nodes labelled and tainted for the platform, and rank them above tenant
workloads. The PriorityClass must exist first:

```yaml
# placement-overrides.yaml
nodeSelector:
  node.c5c3.io/role: platform
tolerations:
  - key: node.c5c3.io/role
    operator: Equal
    value: platform
    effect: NoSchedule
priorityClassName: cobaltcore-platform
```

On the Flux install the same keys go into each operator HelmRelease's
`spec.values` through a kustomize overlay; see
[Sizing and placement overrides](../infrastructure/infrastructure-manifests.md#sizing-and-placement-overrides).
