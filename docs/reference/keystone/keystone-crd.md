---
title: Keystone CRD API Reference
quadrant: operator
---

# Keystone CRD API Reference

Reference documentation for the Keystone Custom Resource Definition. The
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

## Sub-Resource Naming Convention

All operator-managed sub-resources for a Keystone CR are named after the CR itself
with **no `-api` suffix**. For a Keystone CR named `keystone` in namespace `openstack`,
the operator creates:

| Sub-resource | Name | Cluster-internal DNS |
| --- | --- | --- |
| `Deployment` | `keystone` | — |
| `Service` (ClusterIP) | `keystone` | `keystone.openstack.svc.cluster.local` |
| `HorizontalPodAutoscaler` | `keystone` | — |
| `PodDisruptionBudget` | `keystone` | — |
| `NetworkPolicy` | `keystone` | — |
| `HTTPRoute` | `keystone` | — |
| Container & named port | `keystone` | port 5000 |

This convention replaces the historical form that appended an `-api` suffix to each
sub-resource (so the same CR would have produced `-api`-suffixed sub-resources).
The change aligns the internal Service DNS with the public Gateway hostname posture
and removes the redundant suffix that no longer reflected a meaningful split — the
Keystone CR has only ever owned the API role.

For migration semantics (catalog refresh, ownerReference cascade GC of legacy
sub-resources, and operator workflows for upgrading a pre-rename cluster), see the
[Keystone Upgrade Flow reference](./keystone-upgrade-flow.md).

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
  credentialKeys:
    rotationSchedule: "0 0 * * 0"
    maxActiveKeys: 3
  trustFlush:
    schedule: "0 * * * *"
  autoscaling:
    minReplicas: 2
    maxReplicas: 10
    targetCPUUtilization: 80
  networkPolicy:
    ingress:
      - namespaceSelector:
          kubernetes.io/metadata.name: openstack
  topologySpreadConstraints:
    - maxSkew: 1
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: ScheduleAnyway
      labelSelector:
        matchLabels:
          app.kubernetes.io/name: keystone
          app.kubernetes.io/instance: keystone
  priorityClassName: system-cluster-critical
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
    publicEndpoint: https://keystone.example.com/v3
status:
  conditions:
    - type: Ready
      status: "True"
      reason: AllReady
      message: All sub-resources are ready
      lastTransitionTime: "2026-03-09T00:00:00Z"
    - type: KeystoneAPIReady
      status: "True"
      reason: APIHealthy
      message: "Keystone API is responding at http://keystone.openstack.svc.cluster.local:5000/v3"
      lastTransitionTime: "2026-03-09T00:00:00Z"
  endpoint: http://keystone.openstack.svc.cluster.local:5000/v3
  installedRelease: "2025.2"
```

### Printer Columns

`kubectl get keystones` displays these columns:

| Column | JSON Path | Type |
| --- | --- | --- |
| Ready | `.status.conditions[?(@.type=='Ready')].status` | string |
| Endpoint | `.status.endpoint` | string |
| Release | `.status.installedRelease` | string |
| Age | `.metadata.creationTimestamp` | date |

---

## KeystoneSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `replicas` | `int32` | No | `3` | Number of Keystone API replicas. Minimum: 1. The webhook provides a secondary default of 3 when zero. |
| `image` | [`ImageSpec`](#imagespec) | Yes | — | Keystone container image reference. |
| `database` | [`DatabaseSpec`](#databasespec) | Yes | — | MariaDB connection configuration. Includes the optional [`tls`](#databasetlsspec) sub-block that opts in to TLS / mTLS for the connection; when `nil`, the connection is plaintext TCP — preserving the previous behavior for all existing CRs. |
| `cache` | [`CacheSpec`](#cachespec) | Yes | — | Memcached cache configuration. |
| `fernet` | [`FernetSpec`](#fernetspec) | No | See below | Fernet key rotation configuration. |
| `credentialKeys` | [`CredentialKeysSpec`](#credentialkeysspec) | No | See below | Credential-key rotation configuration. Drives the per-CR CronJob that rotates and `credential_migrate`s the credential keys used for encrypting application credentials. |
| `trustFlush` | [`*TrustFlushSpec`](#trustflushspec) | No | `{schedule: "0 * * * *", suspend: false}` (materialized by the defaulting webhook) | Trust flush CronJob configuration. Default-on: when the field is omitted, the defaulting webhook populates an hourly schedule so `keystone-manage trust_flush` runs by default; there is no nil-back path on a webhook-enabled cluster (a `kubectl patch ... 'spec/trustFlush'='null'` round-trips through admission and is re-materialized). To pause without deleting the CronJob, set `suspend: true` — the resource and `TrustFlushReady=True` condition are preserved. |
| `federation` | [`*FederationSpec`](#federationspec) | No | `nil` | Federation configuration (optional). |
| `bootstrap` | [`BootstrapSpec`](#bootstrapspec) | Yes | — | Initial Keystone bootstrap parameters. |
| `middleware` | `[]MiddlewareSpec` | No | `nil` | WSGI middleware filters for api-paste.ini. |
| `plugins` | `[]PluginSpec` | No | `nil` | Service plugins/drivers to configure. |
| `policyOverrides` | [`*PolicySpec`](#policyspec) | No | `nil` | Custom oslo.policy rules. |
| `autoscaling` | [`*AutoscalingSpec`](#autoscalingspec) | No | `nil` | Horizontal pod autoscaling configuration. When set, an HPA is created targeting the `{name}` Deployment. When removed, the HPA is deleted. |
| `networkPolicy` | [`*NetworkPolicySpec`](#networkpolicyspec) | No | `nil` | Network isolation for Keystone API pods. When set, a NetworkPolicy restricting ingress to TCP 5000 and auto-deriving egress rules for DNS, MariaDB, and Memcached is created. When `nil`, no NetworkPolicy is managed and traffic is unrestricted. |
| `gateway` | [`*GatewaySpec`](#gatewayspec) | No | `nil` | Gateway API HTTPRoute configuration. When set, an HTTPRoute is created targeting the `{name}` Service on port 5000 and attached to the referenced pre-existing Gateway; `status.endpoint` is updated to `https://{hostname}/v3`. When removed, the HTTPRoute is deleted and `status.endpoint` reverts to the cluster-local Service URL. |
| `resources` | [`*corev1.ResourceRequirements`](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/pod-v1/#resources) | No | See below | CPU and memory requests and limits for the Keystone API container. When unset, the defaulting webhook injects sensible defaults to ensure Burstable QoS class and enable HPA utilization calculations. |
| `uwsgi` | [`*UWSGISpec`](#uwsgispec) | No | `nil` | uWSGI application server parameters. When set, the operator uses these values for the Deployment container command. When `nil`, hardcoded defaults (processes=2, threads=1, httpKeepAlive=true) are used in the reconciler. |
| `logging` | [`*LoggingSpec`](#loggingspec) | No | See below | oslo.log configuration for the Keystone API container. When `nil`, the defaulting webhook materializes a baseline (`format=text`, `level=INFO`, `debug=false`, no per-logger overrides) so downstream reconciler code never sees a nil pointer. When set, zero-valued sub-fields are partially filled with the same baseline. |
| `topologySpreadConstraints` | [`[]corev1.TopologySpreadConstraint`](https://kubernetes.io/docs/concepts/scheduling-eviction/topology-spread-constraints/) | No | See [below](#topologyspreadconstraints) | Scheduler hints for spreading pods across zones and nodes. `nil` injects two defaults (zone + hostname, MaxSkew=1, `ScheduleAnyway`); a non-nil value (including `[]`) is used verbatim. |
| `priorityClassName` | `*string` | No | `nil` | PriorityClass attached to the Keystone API pod spec. When set, the webhook verifies the class exists; when unset, no priority class is configured. |
| `terminationGracePeriodSeconds` | `*int64` | No | `nil` | Grace period (seconds) granted to Keystone API pods between SIGTERM and SIGKILL during rolling updates. When `nil`, the reconciler applies `30` (the CRD schema emits no `default:` so pre-existing CRs are not mutated on operator upgrade). Minimum: `10`. Must be strictly greater than `preStopSleepSeconds`. Drives the PodSpec `terminationGracePeriodSeconds`. See [Graceful-termination fields](#graceful-termination-fields) and the HA rollout sequence in `architecture/docs/04-architecture/04-high-availability.md`. |
| `preStopSleepSeconds` | `*int64` | No | `nil` | Sleep duration (seconds) of the preStop lifecycle hook, covering the window between EndpointSlice removal and kube-proxy/ingress propagation. When `nil`, the reconciler applies `5` (the CRD schema emits no `default:` so pre-existing CRs are not mutated on operator upgrade). Minimum: `0`. Must be strictly less than `terminationGracePeriodSeconds`. See [Graceful-termination fields](#graceful-termination-fields). |
| `strategy` | [`*appsv1.DeploymentStrategy`](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/deployment-v1/#DeploymentSpec) | No | `RollingUpdate(maxSurge=1, maxUnavailable=0)` | Overrides the Deployment rollout strategy. When `nil`, the reconciler injects `RollingUpdate` with `maxUnavailable=0` and `maxSurge=1` so available capacity never drops below `spec.replicas` during an image-tag patch. Set to customize surge/unavailable counts or switch to `Recreate`. |
| `extraConfig` | `map[string]map[string]string` | No | `nil` | Free-form INI sections for additional configuration. |

### CEL Validation Rules

The CRD includes structural validation rules enforced by the API server before
webhooks are invoked:

| Field | Rule | Error Message |
| --- | --- | --- |
| `spec.database` | `has(self.clusterRef) != has(self.host)` | "exactly one of clusterRef or host must be set" |
| `spec.database` | `!has(self.tls) \|\| !self.tls.enabled \|\| (self.tls.caBundleSecretRef.name != '' && self.tls.clientCertSecretRef.name != '')` | "when database.tls.enabled is true, both database.tls.caBundleSecretRef.name and database.tls.clientCertSecretRef.name must be set" |
| `spec.database.tls.mode` | Enum: `prefer`, `require`, `verify-ca`, `verify-full` | — |
| `spec.cache` | `has(self.clusterRef) != (has(self.servers) && size(self.servers) > 0)` | "exactly one of clusterRef or servers must be set" |
| `spec.policyOverrides` | `(has(self.rules) && size(self.rules) > 0) \|\| self.configMapRef != null` | "at least one of rules or configMapRef must be set" |
| `spec.policyOverrides.rules` | `!has(self.rules) \|\| self.rules.all(k, size(k) > 0)` | "policy rule name must not be empty" |
| `spec.policyOverrides.rules` | `!has(self.rules) \|\| self.rules.all(k, size(self.rules[k]) > 0)` | "policy rule value must not be empty" |
| `spec.autoscaling` | `has(self.targetCPUUtilization) \|\| has(self.targetMemoryUtilization)` | "at least one of targetCPUUtilization or targetMemoryUtilization must be set" |
| `spec.networkPolicy` | `size(self.ingress) > 0` | "at least one ingress source must be specified" |
| `spec.replicas` | Minimum: 1 | — |
| `spec.fernet.maxActiveKeys` | Minimum: 3 | — |
| `spec.credentialKeys.maxActiveKeys` | Minimum: 3 | — |
| `spec.autoscaling.maxReplicas` | Minimum: 1 | — |
| `spec.autoscaling.minReplicas` | Minimum: 1 | — |
| `spec.autoscaling.targetCPUUtilization` | Range: 1–100 | — |
| `spec.autoscaling.targetMemoryUtilization` | Range: 1–100 | — |
| `spec.uwsgi.processes` | Minimum: 1 | — |
| `spec.uwsgi.threads` | Minimum: 1 | — |
| `spec.uwsgi.harakiri` | Minimum: 1 | — |
| `spec.uwsgi.httpKeepAliveTimeout` | Minimum: 1 | — |
| `spec.terminationGracePeriodSeconds` | Minimum: 10 | — |
| `spec.preStopSleepSeconds` | Minimum: 0 | — |
| `spec.gateway.hostname` | MinLength: 1 | (empty string rejected by API server) |
| `spec.gateway.parentRef.name` | MinLength: 1 | (empty string rejected by API server) |

> **Known limitation:** `spec.uwsgi.processes` and `spec.uwsgi.threads`
> have no upper-bound validation. A user could set an extremely high value (e.g.,
> `processes: 10000`), causing the Deployment to request more workers than the node
> can sustain. A `+kubebuilder:validation:Maximum` marker should be added once the
> team agrees on a safe ceiling. Track this as a follow-up product decision.

---

## AutoscalingSpec

Configures horizontal pod autoscaling for the Keystone API Deployment.
This is a pointer field (`*AutoscalingSpec`) on `KeystoneSpec` — when `nil`,
no HPA is created and the `HPAReady` condition is set to `True` with reason
`HPANotRequired`. When set, a `HorizontalPodAutoscaler` (autoscaling/v2) is
created targeting the `{name}` Deployment. Removing the field deletes the
existing HPA.

While `spec.autoscaling` is set, the operator leaves the Deployment's
`.spec.replicas` unmanaged (nil) so the HPA owns the replica count and the
reconciler does not reset it on each pass. `spec.replicas` is then used only
as the initial replica count and as the `minReplicas` default when
`autoscaling.minReplicas` is unset.

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
| `metadata.name` | `{name}` |
| `metadata.labels` | `commonLabels` (same as Deployment) |
| `spec.scaleTargetRef.apiVersion` | `apps/v1` |
| `spec.scaleTargetRef.kind` | `Deployment` |
| `spec.scaleTargetRef.name` | `{name}` |
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

Configures the uWSGI application server parameters for the Keystone API container. This is a pointer field (`*UWSGISpec`) on `KeystoneSpec` — when `nil`,
the reconciler uses hardcoded defaults (processes=2, threads=1, httpKeepAlive=true)
and the webhook does **not** inject a default `UWSGISpec`. When set (even as
`uwsgi: {}`), the webhook defaults zero-valued sub-fields and the reconciler reads
from the spec.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `processes` | `int32` | No | `2` | Number of uWSGI worker processes. Minimum: 1. Maps to `--processes` in the container command. |
| `threads` | `int32` | No | `1` | Number of threads per uWSGI worker process. Minimum: 1. Maps to `--threads` in the container command. |
| `httpKeepAlive` | `bool` | No | `true` | Enables the `--http-keepalive` flag on the uWSGI process. When `false`, the flag is omitted. See [HTTPKeepAlive defaulting](#httpkeepalive-defaulting-caveat) for the zero-value caveat. |
| `harakiri` | `*int32` | No | `nil` (flag omitted) | Caps the per-request worker lifetime (seconds) via `--harakiri`. Minimum: `1`. The webhook additionally enforces `harakiri < terminationGracePeriodSeconds − preStopSleepSeconds` so the worst-case per-request kill fits inside the shutdown drain window. See the HA rollout sequence in `architecture/docs/04-architecture/04-high-availability.md`. |
| `httpKeepAliveTimeout` | `*int32` | No | `nil` (flag omitted) | Idle timeout (seconds) for keep-alive connections via `--http-keepalive-timeout`. Minimum: `1`. Emitted only when `httpKeepAlive=true` (the webhook rejects a non-nil timeout combined with `httpKeepAlive=false`). Recommended to set `≤ preStopSleepSeconds` so idle sockets close before SIGTERM reaches uWSGI. See the HA rollout sequence in `architecture/docs/04-architecture/04-high-availability.md`. |

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

## Graceful-termination fields

Five CR fields control the shutdown envelope applied during Keystone rolling
updates — `spec.terminationGracePeriodSeconds`, `spec.preStopSleepSeconds`,
`spec.strategy`, `spec.uwsgi.harakiri`, and `spec.uwsgi.httpKeepAliveTimeout`.
Each field is listed in its owning section (top-level `KeystoneSpec` or
`UWSGISpec`); this section consolidates their semantics, interaction rules,
and defaulting behavior.

For the rollout sequence diagram and tunable-selection guidance, see
`architecture/docs/04-architecture/04-high-availability.md` (section
"Keystone Rolling Update").

### Field Summary

| Field                                     | Type                              | Default                                      | Minimum | Effect                                                                                                                             |
| ----------------------------------------- | --------------------------------- | -------------------------------------------- | ------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| `spec.terminationGracePeriodSeconds`      | `*int64`                          | `30`                                         | `10`    | PodSpec `terminationGracePeriodSeconds` — total envelope between SIGTERM and SIGKILL.                                              |
| `spec.preStopSleepSeconds`                | `*int64`                          | `5`                                          | `0`     | Sleep duration of the preStop hook (`/bin/sh -c 'sleep <n>'`). Covers the EndpointSlice / kube-proxy propagation window.           |
| `spec.strategy`                           | `*appsv1.DeploymentStrategy`      | `RollingUpdate(maxSurge=1, maxUnavailable=0)` | —       | Deployment rollout strategy. Default guarantees surge-before-remove so capacity never dips below `spec.replicas`.                  |
| `spec.uwsgi.harakiri`                     | `*int32`                          | unset (flag omitted)                         | `1`     | Per-request worker kill bound (`--harakiri <n>`). Prevents a single stuck request from holding a worker past the shutdown envelope. |
| `spec.uwsgi.httpKeepAliveTimeout`         | `*int32`                          | unset (flag omitted)                         | `1`     | Idle keep-alive socket timeout (`--http-keepalive-timeout <n>`). Only emitted when `httpKeepAlive=true`.                           |

### Interaction Rules Enforced by the Webhook

The validating webhook enforces the following cross-field invariants so that the
shutdown envelope is always internally consistent. Violations are returned as
`field.Invalid` errors.

| Rule                                                                                                    |
| ------------------------------------------------------------------------------------------------------- |
| `preStopSleepSeconds < terminationGracePeriodSeconds` (with `nil` pointers resolved to defaults 5 / 30) |
| `harakiri < terminationGracePeriodSeconds − preStopSleepSeconds` (only when `harakiri` is set)          |
| `httpKeepAliveTimeout` requires `httpKeepAlive=true`                                                    |
| `strategy.type=Recreate` must not carry a `strategy.rollingUpdate` block                                |

### Operator Guidance (not webhook-enforced)

- **`httpKeepAliveTimeout ≤ preStopSleepSeconds`** — when the keep-alive
  timeout exceeds the preStop sleep, a client may still hold a warm
  keep-alive socket to the Pod when SIGTERM fires, returning a connection
  reset on the client's next request. Tune `httpKeepAliveTimeout` at or below
  `preStopSleepSeconds` to close idle sockets before the kubelet signals
  uWSGI and preserve the zero-reset SLO. The webhook does not enforce this
  because slow clients may legitimately need a longer keep-alive window at
  the cost of occasional resets on rollout.

### Reconciler Fallbacks

The reconciler applies internal defaults when the CR field is `nil` so
older CRs continue to reconcile without the fields set:

| Field                                | Fallback when `nil`                                         |
| ------------------------------------ | ----------------------------------------------------------- |
| `spec.terminationGracePeriodSeconds` | PodSpec receives `30`                                       |
| `spec.preStopSleepSeconds`           | preStop command is `sleep 5`                                |
| `spec.strategy`                      | `RollingUpdate` with `maxUnavailable=0`, `maxSurge=1`       |
| `spec.uwsgi.harakiri`                | `--harakiri` flag is omitted                                |
| `spec.uwsgi.httpKeepAliveTimeout`    | `--http-keepalive-timeout` flag is omitted                  |

These fallbacks live in `internal/controller/reconcile_deployment.go`
(`terminationGracePeriodSeconds`, `preStopSleepCommand`, `deploymentStrategy`,
`uwsgiCommand`) and are the single source of truth for the no-op upgrade path.

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
  terminationGracePeriodSeconds: 60
  preStopSleepSeconds: 10
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
  uwsgi:
    processes: 4
    threads: 4
    httpKeepAlive: true
    httpKeepAliveTimeout: 10
    harakiri: 45
```

---

## LoggingSpec

Configures oslo.log output for the Keystone API container. This is a
pointer field (`*LoggingSpec`) on `KeystoneSpec`. When `nil`, the defaulting
webhook materializes a baseline `LoggingSpec{Format: "text", Level: "INFO",
Debug: false}` (no per-logger overrides) so downstream reconciler code never
sees a nil pointer — matching the documented production baseline (stdout/stderr,
oslo.log line format, no debug noise). When set (even as `logging: {}`), the
webhook partially fills zero-valued sub-fields with the same baseline values
and the validating webhook enforces the enum constraints described below.

The reconciler always emits `[DEFAULT] use_stderr=true` and `[DEFAULT] debug=<spec.logging.debug>`
into `keystone.conf`. When `spec.logging.format == "json"`, an additional
`logging.conf` ConfigMap entry is rendered (oslo.log JSON formatter wired to a
stderr StreamHandler) and `[DEFAULT] log_config_append=/etc/keystone/keystone.conf.d/logging.conf`
is appended; toggling `format` back to `text` drops the `logging.conf` key.
A `Warning` event with reason `LoggingStderrDisabled` is emitted when
`spec.extraConfig` overrides `[DEFAULT].use_stderr` to a non-`true` value,
because doing so silently breaks the cluster log-aggregation pipeline.
The reconciler also surfaces this misconfiguration via an informational
`LoggingHealthy` status condition (`Reason=StderrDisabled` when overridden,
`Reason=StderrEnabled` otherwise). The condition is intentionally **not**
aggregated into the top-level `Ready` condition so an explicit operator
override is honoured rather than blocking the rollout; the gated event
fires only on transition into `StderrDisabled`. See
[keystone-events.md, Logging](keystone-events.md#logging) for the
full event/condition contract.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `format` | `string` | No | `text` | On-wire layout of oslo.log records. `text` emits the standard oslo.log line format; `json` emits one JSON object per record for direct ingest by Loki/OpenSearch. Enforced as `+kubebuilder:validation:Enum=text;json`. |
| `level` | `string` | No | `INFO` | Root logger level applied to oslo.log. One of `DEBUG`, `INFO`, `WARNING`, `ERROR`, `CRITICAL`. Enforced as `+kubebuilder:validation:Enum=DEBUG;INFO;WARNING;ERROR;CRITICAL`. |
| `debug` | `bool` | No | `false` | Toggles oslo.log `[DEFAULT] debug=true`. Independent of `level` because oslo.log gates several extra-verbose code paths on the debug flag specifically (SQL echo, auth-backend tracing). |
| `perLoggerLevels` | `map[string]string` | No | `nil` | Overrides the level of named loggers, mirroring oslo.log's `default_log_levels`. Each value must be one of `DEBUG`/`INFO`/`WARNING`/`ERROR`/`CRITICAL` — enforced by the validating webhook (CRD v1 `additionalProperties` does not support enum constraints). Empty-string keys are rejected. Rendered into `[DEFAULT].default_log_levels` in deterministic alphabetical order to keep ConfigMap content-hashes stable across reconciles. |

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
  logging:
    format: json
    level: INFO
    debug: false
    perLoggerLevels:
      sqlalchemy.engine: WARNING
      keystone.middleware: DEBUG
```

---

## FernetSpec

Configures Fernet token key rotation.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `rotationSchedule` | `string` | No | `"0 0 * * 0"` | Cron expression (5-field standard format) for key rotation. Validated by `robfig/cron/v3` `ParseStandard`. |
| `maxActiveKeys` | `int32` | No | `3` | Maximum number of active Fernet keys. Minimum: 3. |
| `suspend` | `bool` | No | `false` | Suspends the Fernet rotation CronJob without deleting it. Maps to the CronJob `spec.suspend` field. Set `true` to pause key rotation during an incident; the schedule is unchanged so resuming is churn-free. |

---

## CredentialKeysSpec

Configures credential-key rotation. Credential keys encrypt the
application-credential passwords stored in the database. Rotation uses the same
32-byte base64url format as Fernet but runs `keystone-manage credential_migrate`
after generating a new primary key so that existing rows stay readable after the
old key is purged. Rotation is driven by a CronJob that pushes the regenerated
key set back to the `{name}-credential-keys` Secret via a minimally-scoped
ServiceAccount. The Secret is also mirrored to OpenBao through a `PushSecret`.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `rotationSchedule` | `string` | No | `"0 0 * * 0"` | Cron expression (5-field standard format). Validated by `robfig/cron/v3` `ParseStandard` in the webhook. |
| `maxActiveKeys` | `int32` | No | `3` | Maximum number of active credential keys. Minimum: 3. Exposed to `keystone-manage` via the `OS_credential__max_active_keys` environment variable on the rotation CronJob. |
| `suspend` | `bool` | No | `false` | Suspends the credential rotation CronJob without deleting it. Maps to the CronJob `spec.suspend` field. Set `true` to pause key rotation during an incident; the schedule is unchanged so resuming is churn-free. |

---

## TrustFlushSpec

Configures periodic purging of expired trust delegations. This is a pointer
field (`*TrustFlushSpec`) on `KeystoneSpec`, but on a webhook-enabled cluster
it is **default-on**: the defaulting webhook materializes
`{schedule: "0 * * * *", suspend: false}` whenever the field is omitted (or
patched to `null`), so the operator always creates a CronJob named
`{name}-trust-flush` running `keystone-manage trust_flush` and the
`TrustFlushReady` condition is set to `True` with reason `TrustFlushReady`.

There is no nil-back path on a webhook-enabled cluster — a
`kubectl patch ... 'spec/trustFlush'='null'` round-trips through admission and
is re-materialized, preserving the existing CronJob (no delete/recreate). To
pause the schedule without deleting the CronJob, set `suspend: true` — the
resource and `TrustFlushReady=True` condition are preserved while suspended.

The pointer shape is retained for envtest fixtures and other webhook-less
clusters where the defaulting webhook is not wired up. In that legacy bypass
posture the reconciler logs a warning, deletes any existing CronJob, and sets
`TrustFlushReady=True` with reason `TrustFlushNotRequired` and a message
identifying the bypass — see [`reconcileTrustFlush`](./keystone-reconciler.md#reconciletrustflush).

For brownfield CRs that omit `spec.trustFlush` at the time of an operator
upgrade and the recommended pre-upgrade actions on clusters with very large
trust tables, see
[Default-on Trust Flush at Upgrade Time](./keystone-upgrade-flow.md#default-on-trust-flush-at-upgrade-time)
in the upgrade-flow reference.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `schedule` | `string` | No | `"0 * * * *"` | Cron expression (5-field standard format) for trust flush. Validated by `robfig/cron/v3` `ParseStandard`. Default is hourly. |
| `suspend` | `bool` | No | `false` | Suspends the CronJob without deleting it. Maps to the CronJob `spec.suspend` field. The CronJob resource and `TrustFlushReady=True` condition are preserved while suspended. |
| `args` | `[]string` | No | `nil` | Additional CLI flags appended after `keystone-manage trust_flush`. Flags such as `--keystone-user`, `--keystone-group`, `--date` are passed through verbatim. |

### CronJob Resource Mapping

The CronJob created from this spec has the following shape. Field values
sourced from `trustFlush.*` are populated either by the user or — when the
field was omitted on submission — by the defaulting webhook, which
materializes `{schedule: "0 * * * *", suspend: false}` before the reconciler
ever sees the object.

| CronJob Field | Value |
| --- | --- |
| `metadata.name` | `{name}-trust-flush` |
| `metadata.labels` | `commonLabels` (same as Deployment) |
| `spec.schedule` | `trustFlush.schedule` (webhook-defaulted to `"0 * * * *"` when omitted) |
| `spec.suspend` | `&trustFlush.suspend` (pointer to bool; webhook-defaulted to `false` when omitted) |
| `spec.jobTemplate.spec.template.spec.restartPolicy` | `OnFailure` |
| Container name | `trust-flush` |
| Container image | `{spec.image.repository}:{spec.image.tag}` |
| Container command | `["keystone-manage", "--config-dir=/etc/keystone/keystone.conf.d/", "trust_flush"]` + `args` |
| Container securityContext | `restrictedSecurityContext()` (PSS Restricted) |
| `ownerReferences` | Points to the Keystone CR (controller: true) |

### Volume Mounts

The trust-flush container mounts the same configuration and key volumes as the
Deployment, all read-only:

| Volume Name | Mount Path | Source | ReadOnly |
| --- | --- | --- | --- |
| `config` | `/etc/keystone/keystone.conf.d/` | ConfigMap `{configMapName}` | Yes |
| `fernet-keys` | `/etc/keystone/fernet-keys` | Secret `{name}-fernet-keys` | Yes |
| `credential-keys` | `/etc/keystone/credential-keys` | Secret `{name}-credential-keys` | Yes |

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
  trustFlush:
    schedule: "30 2 * * 0"
    args: ["--date", "2024-01-01"]
```

---

## NetworkPolicySpec

Configures network isolation for the Keystone API pods. This is a
pointer field (`*NetworkPolicySpec`) on `KeystoneSpec` — when `nil`, no
NetworkPolicy is managed and the `NetworkPolicyReady` condition is set to
`True` with reason `NetworkPolicyNotRequired`. When set, the operator creates
a NetworkPolicy that restricts ingress on TCP 5000 to the declared sources —
plus the operator's own namespace (so the operator health check can reach the
API) and, when `spec.gateway` is set, the gateway namespace — and auto-derives
egress rules for DNS, the kube-apiserver, the database, and the cache. Removing
the field deletes the NetworkPolicy on the next reconcile.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `ingress` | `[]NetworkPolicyIngressSource` | Yes | — | Sources allowed to reach Keystone API on TCP 5000. At least one entry required (enforced by CEL and webhook). |
| `additionalEgress` | `[]networkingv1.NetworkPolicyEgressRule` | No | `nil` | Extra egress rules appended after the auto-derived rules. The auto-derived rules already cover the database and cache in both managed and brownfield modes, so reserve this for external integrations beyond those backends. |

### NetworkPolicyIngressSource

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `namespaceSelector` | `map[string]string` | Yes | Label selector for source namespaces. All pods in matching namespaces may reach Keystone on TCP 5000 unless `podSelector` narrows the set. |
| `podSelector` | `map[string]string` | No | Optional label selector restricting allowed pods within the selected namespaces (AND logic within a single peer). |

### Auto-added Ingress peers

Beyond the declared `ingress` sources, the operator appends these ingress peers
on the TCP 5000 rule:

| Peer | Trigger | Notes |
| --- | --- | --- |
| Operator namespace | Operator namespace resolvable | Selected by `kubernetes.io/metadata.name`. The operator's health check GETs the Keystone Service on TCP 5000; without this peer `KeystoneAPIReady` would flip `False` permanently for a healthy deployment. Omitted only when the operator namespace cannot be determined. |
| Gateway namespace | `spec.gateway` set | Selected by `kubernetes.io/metadata.name` of `parentRef.namespace` (the CR namespace when empty), so the Gateway data plane can reach the API. |

### Auto-derived Egress

The operator appends the following egress rules before `additionalEgress`.
All rules are port-only — the destination is unrestricted (tightening to
backend pod labels is deferred). Rule order is deterministic: DNS, apiserver,
database, cache.

| Rule | Trigger | Notes |
| --- | --- | --- |
| DNS UDP+TCP 53 | Always | Destination is unrestricted because CoreDNS may run in any namespace (e.g. NodeLocal DNSCache). |
| kube-apiserver TCP 443+6443 | Always | The fernet/credential/admin-password rotation CronJob pods share this policy's pod selector and PATCH the rotated keys back to a Secret via `kubernetes.default.svc`; without apiserver egress every scheduled rotation stops at its first run. `443` is the ClusterIP Service port; `6443` covers the post-DNAT kube-apiserver pod port on enforcing CNIs. |
| Database TCP `dbPort` | Always | Port from `spec.database.port` (default `3306`), emitted in both managed (`database.clusterRef`) and brownfield (`database.host`) modes — the readiness probe TCP-connects to exactly this port, so an enforcing CNI would otherwise depool every pod. |
| Cache TCP (derived) | `cache.clusterRef` or `cache.servers` set | Managed mode → `11211`; brownfield mode → the distinct ports parsed from the `cache.servers` `host:port` strings (default `11211` when a server omits the port). |

A defensive guard in the reconciler refuses to create a NetworkPolicy with an
empty `ingress` list, even if CEL validation was bypassed (stored objects,
disabled webhooks, direct etcd writes) — the operator fails closed rather than
open.

### Example

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: Keystone
metadata:
  name: keystone
  namespace: openstack
spec:
  # ... required fields ...
  networkPolicy:
    ingress:
      - namespaceSelector:
          kubernetes.io/metadata.name: openstack
      - namespaceSelector:
          kubernetes.io/metadata.name: ingress-gateway
        podSelector:
          app.kubernetes.io/name: envoy
    additionalEgress:
      - to:
          - ipBlock:
              cidr: 10.0.0.0/24
        ports:
          - protocol: TCP
            port: 443
```

---

## GatewaySpec

`GatewaySpec` is a shared type from `internal/common/types` (imported as
`commonv1`), the single source of truth for the Gateway API HTTPRoute shape.
Both the Keystone operator and the c5c3 ControlPlane reuse it instead
of maintaining their own field-for-field copies; `keystonev1alpha1.GatewaySpec`
is a type alias to `commonv1.GatewaySpec`, so existing references compile
unchanged. The field table below is the canonical reference that the
[c5c3 ControlPlane CRD doc](../c5c3/controlplane-crd.md#gatewayspec) links back
to. The reconciler behavior described in this section is Keystone-specific.

Configures external exposure of the Keystone API via a Gateway API HTTPRoute. This is a pointer field (`*GatewaySpec`) on `KeystoneSpec` — when `nil`,
no HTTPRoute is created and the `HTTPRouteReady` condition is set to `True` with
reason `HTTPRouteNotRequired`. When set, an `HTTPRoute` (from
`gateway.networking.k8s.io/v1`) is created in the Keystone CR's namespace, attached
to the referenced pre-existing Gateway, and pointing to the `{name}` Service on
port 5000. Removing the field deletes the existing HTTPRoute.

The operator plays the **application-developer** role in the Gateway API model: it
manages only the `HTTPRoute`. The referenced `Gateway` (and its `GatewayClass`) are
**platform-team** concerns and must be pre-provisioned — this operator does not
create or reconcile them. Cross-namespace `parentRef` references additionally
require a `ReferenceGrant` in the target namespace, which is out of scope for this
operator.

**Gateway API CRD prerequisite:** the `gateway.networking.k8s.io/v1` `HTTPRoute`
CRD must be installed in the cluster before the Keystone operator starts. The
operator probes for the CRD at startup (via the manager `RESTMapper`); when the
CRD is missing it disables the HTTPRoute watch so Keystone CRs without
`spec.gateway` still reconcile, and reports `HTTPRouteReady=False` with reason
`GatewayAPINotInstalled` for any CR that sets `spec.gateway`. Installing the
CRD after the operator has started requires restarting the operator for the
watch to become active. The quickstart stack (`make deploy-infra`) installs
the upstream Gateway API standard CRDs for this reason; the pinned version is
set via `GATEWAY_API_VERSION` in `hack/deploy-infra.sh` and tracks
`sigs.k8s.io/gateway-api` in `operators/keystone/go.mod`.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `parentRef` | [`GatewayParentRefSpec`](#gatewayparentrefspec) | Yes | — | Gateway the HTTPRoute attaches to. |
| `hostname` | `string` | Yes | — | Externally reachable hostname (SNI / `Host` header) matched by the HTTPRoute. Used for both route hostname matching and deriving `status.endpoint` (`https://{hostname}/v3`). Minimum length: 1. |
| `path` | `string` | No | `"/"` | URL path prefix matched by the HTTPRoute. The reconciler applies the default when the field is empty. Uses `PathPrefix` match type. |
| `annotations` | `map[string]string` | No | `nil` | Annotations passed through verbatim to the HTTPRoute `metadata.annotations`, allowing implementation-specific configuration (rate limits, timeouts, CORS). Operator-managed labels are preserved — user annotations do not shadow them. |

### GatewayParentRefSpec

References the pre-existing `Gateway` that the operator attaches the HTTPRoute to.
Like `GatewaySpec`, this is a shared `commonv1` type reused by both the
Keystone operator and the c5c3 ControlPlane; `keystonev1alpha1.GatewayParentRefSpec`
aliases `commonv1.GatewayParentRefSpec`.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` | Yes | — | Gateway resource name. Minimum length: 1. |
| `namespace` | `string` | No | CR namespace | Namespace of the referenced Gateway. When empty, the Gateway is assumed to live in the Keystone CR's namespace. Cross-namespace references require a `ReferenceGrant`. |
| `sectionName` | `string` | No | `""` | Targets a specific listener on the Gateway (e.g., `"https"`) when the Gateway defines multiple listeners. When empty, the HTTPRoute attaches to all compatible listeners. |

### HTTPRoute Resource Mapping

The HTTPRoute created from this spec has the following shape
(`gateway.networking.k8s.io/v1`, `Kind: HTTPRoute`):

| HTTPRoute Field | Value |
| --- | --- |
| `metadata.name` | `{name}` (matches the backend Service, Deployment, HPA, NetworkPolicy naming) |
| `metadata.namespace` | Keystone CR namespace |
| `metadata.labels` | `commonLabels` (same as Deployment) |
| `metadata.annotations` | Merged from `spec.gateway.annotations` |
| `spec.parentRefs[0].name` | `spec.gateway.parentRef.name` |
| `spec.parentRefs[0].namespace` | `spec.gateway.parentRef.namespace` when non-empty; omitted otherwise |
| `spec.parentRefs[0].sectionName` | `spec.gateway.parentRef.sectionName` when non-empty; omitted otherwise |
| `spec.hostnames[0]` | `spec.gateway.hostname` |
| `spec.rules[0].matches[0].path.type` | `PathPrefix` |
| `spec.rules[0].matches[0].path.value` | `spec.gateway.path` (or `"/"` when empty) |
| `spec.rules[0].backendRefs[0].kind` | `Service` |
| `spec.rules[0].backendRefs[0].name` | `{name}` |
| `spec.rules[0].backendRefs[0].port` | `5000` |
| `ownerReferences` | Points to the Keystone CR (controller: true) — enables garbage collection |

### status.endpoint Derivation

`status.endpoint` reflects the externally reachable Keystone API URL and is
recomputed on every reconcile:

| `spec.gateway` | `status.endpoint` Value |
| --- | --- |
| `nil` | `http://{name}.{namespace}.svc.cluster.local:5000/v3` (cluster-local fallback) |
| Set | `https://{hostname}/v3` — HTTPS is fixed because Gateways are the public-ingress hop and terminate TLS |

`status.endpoint` does **not** include `spec.gateway.path`. The `/v3` suffix is
appended unconditionally because Keystone API v3 is served at that fixed path; the
`PathPrefix` match on the HTTPRoute routes any prefix under `spec.gateway.path` to
the backend. `spec.publicEndpoint` (if set) still takes precedence over the
gateway-derived URL for the `--bootstrap-public-url` argument passed to
`keystone-manage bootstrap`; the precedence is unchanged from earlier behavior.

### Interaction with NetworkPolicy

When both `spec.gateway` and `spec.networkPolicy` are configured, the operator
automatically appends an extra ingress peer to the managed NetworkPolicy so that
the Gateway's data-plane pods can reach Keystone on TCP 5000:

- **Peer selector:** `namespaceSelector` matching
  `kubernetes.io/metadata.name={gatewayNamespace}`. The gateway data plane's pod
  labels are implementation-specific (Kong/Envoy/NGINX/…) and not known to this
  operator, so selection is by entire gateway namespace rather than by pod labels.
- **Namespace source:** `spec.gateway.parentRef.namespace` when set; otherwise the
  Keystone CR's own namespace (mirroring the ParentRef lookup semantics).
- **Removal:** Clearing `spec.gateway` removes the extra peer on the next reconcile.
- **networkPolicy nil:** When `spec.networkPolicy` is `nil`, no NetworkPolicy is
  managed at all and no extra peer is added (gateway-only deployments rely on
  the namespace's default network policy or absence thereof).

### Example — Basic Gateway Exposure

> **kind Quick Start note:** a ready-made
> `Gateway/openstack-gw` ships in the kind overlay
> (`deploy/kind/base/openstack-gateway.yaml`) and is reachable on the host
> at `https://keystone.127-0-0-1.nip.io/v3` — see the
> [Quick Start (Extended) / Access Keystone section](../../quick-start-extended.md#access-keystone-from-your-local-machine).
> On a Quick Start cluster, setting `spec.gateway.parentRef.name:
> openstack-gw` plus `hostname: keystone.127-0-0-1.nip.io` makes
> `status.endpoint = https://keystone.127-0-0-1.nip.io/v3` actually
> resolve from your workstation — no `/etc/hosts` edit, no
> `kubectl port-forward`. Production overlays do **not** ship
> `openstack-gw`; operators pick their own Gateway implementation and
> parent reference there.

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
  gateway:
    parentRef:
      name: public-gateway
      namespace: istio-ingress
      sectionName: https
    hostname: keystone.example.com
    path: /identity
    annotations:
      konghq.com/plugins: rate-limit-sha
```

Resulting `status.endpoint`: `https://keystone.example.com/v3`.

### Example — Gateway with NetworkPolicy

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: Keystone
metadata:
  name: keystone
  namespace: openstack
spec:
  # ... required fields ...
  gateway:
    parentRef:
      name: public-gateway
      namespace: istio-ingress
    hostname: keystone.example.com
  networkPolicy:
    ingress:
      - namespaceSelector:
          kubernetes.io/metadata.name: openstack
```

The operator-managed NetworkPolicy allows ingress from:

1. The `openstack` namespace (user-declared).
2. The `istio-ingress` namespace (auto-added because `spec.gateway` is set).

---

## TopologySpreadConstraints

`spec.topologySpreadConstraints` attaches scheduler spread hints to the
Keystone API Deployment's pod template. Uses the upstream
`corev1.TopologySpreadConstraint` type verbatim, except that the webhook
restricts `labelSelector` to exact `matchLabels` matching the Deployment
selector (see below).

| `spec.topologySpreadConstraints` | Effect |
| --- | --- |
| `nil` (unset) | Operator injects two defaults: `topology.kubernetes.io/zone` and `kubernetes.io/hostname`, both `MaxSkew=1` with `ScheduleAnyway`, selecting pods via `app.kubernetes.io/name=keystone` + `app.kubernetes.io/instance={name}`. |
| `[]` (empty slice) | Defaults disabled; no spread constraints configured. Explicit opt-out. |
| Non-empty slice | User value is applied verbatim; no defaults merged. |

### Webhook Constraint

Each entry must set `labelSelector.matchLabels` equal to the Deployment
selector (`app.kubernetes.io/name=keystone`, `app.kubernetes.io/instance={CR name}`).
`matchExpressions` is rejected. This prevents constraints that widen or narrow
beyond the Deployment's intent, which would otherwise silently produce wrong
spread behavior.

### Example

```yaml
spec:
  # ... required fields ...
  topologySpreadConstraints:
    - maxSkew: 1
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: DoNotSchedule
      labelSelector:
        matchLabels:
          app.kubernetes.io/name: keystone
          app.kubernetes.io/instance: keystone
```

---

## PriorityClassName

`spec.priorityClassName` (pointer) passes through to
`pod.spec.priorityClassName` on the Keystone API pods. Uses the standard
`scheduling.k8s.io/v1` `PriorityClass` resource model.

| Value | Effect |
| --- | --- |
| `nil` | No priority class is configured; the cluster default applies. |
| `""` (empty string) | No priority class — explicit opt-out, useful when clearing a previously set value via `kubectl patch`. |
| Non-empty string | Value is written to the Deployment PodSpec. The webhook performs a direct (uncached) cluster-scoped `Get` of the `PriorityClass` at admission time and rejects unknown names with `field.NotFound`. |

The rotation CronJobs (Fernet, credential) reuse the same `priorityClassName`
to stay co-scheduled with the API pods.

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
| `publicEndpoint` | `string` | No | Cluster-local service DNS | Externally routable Keystone endpoint URL. Used for the `--bootstrap-public-url` argument passed to `keystone-manage bootstrap`. Required by external clients (CLI users, Horizon, federation partners) that cannot resolve the cluster-local service DNS. |

---

## KeystoneStatus

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | Latest available observations of the Keystone state. |
| `endpoint` | `string` | Keystone API endpoint URL (set by the controller when ready). Defaults to `http://{name}.{namespace}.svc.cluster.local:5000/v3`. |
| `installedRelease` | `string` | OpenStack release version currently deployed. Set by the controller after a successful `db_sync`; reflects the value extracted from `spec.image.tag`. |
| `targetRelease` | `string` | Upgrade target release during an active upgrade. Set while `upgradePhase` is one of `Expanding`/`Migrating`/`RollingUpdate`/`Contracting`; cleared after `Contracting` completes. |
| `upgradePhase` | [`UpgradePhase`](#upgradephase) | Current phase of an active database upgrade. Empty outside upgrades. |

The status subresource is enabled via `+kubebuilder:subresource:status`.

### UpgradePhase

`UpgradePhase` is a string enum (`+kubebuilder:validation:Enum=Expanding;Migrating;RollingUpdate;Contracting`)
representing the current phase of a sequential release upgrade driven by
`reconcileDatabase`. Phase transitions follow the expand-migrate-contract
pattern:

| Value | Meaning |
| --- | --- |
| `Expanding` | Additive schema migrations running (new columns/tables). Old pods keep serving. |
| `Migrating` | Backfill/data-migration jobs running against the expanded schema. |
| `RollingUpdate` | New image is rolling out; old and new pods read the expanded schema side-by-side. |
| `Contracting` | Destructive schema migrations running (drop old columns/tables) after the rollout completes. |

`spec.image.tag` must be parseable by `ParseRelease` (`YYYY.N` or `YYYY.N-patch`).
Sequential upgrades are limited to one minor step (`2025.1 → 2025.2`) or a
year-boundary crossing (`2025.2 → 2026.1`); downgrades and skip-level upgrades
are rejected by the reconciler.

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
| `tls` | [`*DatabaseTLSSpec`](#databasetlsspec) | No | Optional TLS/mTLS configuration. The pointer keeps the field opt-in and non-mutating: a `nil` `tls` means plaintext TCP, preserving the previous behavior for all existing CRs. |

Exactly one of `clusterRef` or `host` must be set (enforced by CEL validation).

### DatabaseTLSSpec

Configures opt-in TLS (and mutual TLS) for the Keystone-to-database connection.
Referenced as an optional pointer from
[`DatabaseSpec`](#databasespec); a `nil` value preserves the previous plaintext
behavior.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `enabled` | `bool` | No | `false` | Turns on TLS for the database connection. When `true`, the operator provisions the client certificate (`<name>-db-client`), appends the `ssl_*` DSN parameters, and mounts the certificate material into every workload that opens a connection. Opt-in only — the defaulting webhook never sets this to `true`. |
| `mode` | `string` | No | `"require"` (materialized by the defaulting webhook when `tls` is non-nil and `mode` is empty) | Verification strength applied to the connection. Enum: `prefer`, `require`, `verify-ca`, `verify-full`. `prefer`/`require` encrypt the connection only (no peer verification); `verify-ca` additionally verifies the server certificate chain against the trusted CA bundle; `verify-full` additionally verifies that the server hostname matches the certificate identity. |
| `caBundleSecretRef` | [`SecretRefSpec`](#secretrefspec) | Yes (when `enabled=true`) | — | Secret holding the server CA bundle the client trusts when verifying the database endpoint. Required by both the CRD CEL rule and the validating webhook when `enabled` is `true`. |
| `clientCertSecretRef` | [`SecretRefSpec`](#secretrefspec) | Yes (when `enabled=true`) | — | Secret holding the client keypair presented to the database for mutual TLS. In managed mode (`database.clusterRef` set) the operator provisions a cert-manager `Certificate` into a Secret named `<name>-db-client`; in brownfield mode (`database.host` set) the keypair must be supplied out-of-band. Required by both the CRD CEL rule and the validating webhook when `enabled` is `true`. |

#### Mode → connect-args mapping

The reconciler's `reconcile_dbconnection_secret.go` appends `ssl_*` query parameters
to the database DSN according to `mode`. The mapping is implemented by
`modeToSSLParams` in `operators/keystone/internal/controller/dbtls_mode.go`.
The on-pod paths come from the read-only volume `db-tls`
mounted at `/etc/keystone/db-tls/`:

| `mode` | `ssl_ca` | `ssl_cert` | `ssl_key` | `ssl_verify_cert` | `ssl_verify_identity` |
| --- | --- | --- | --- | --- | --- |
| `prefer` | `/etc/keystone/db-tls/ca.crt` | `/etc/keystone/db-tls/tls.crt` | `/etc/keystone/db-tls/tls.key` | — | — |
| `require` | `/etc/keystone/db-tls/ca.crt` | `/etc/keystone/db-tls/tls.crt` | `/etc/keystone/db-tls/tls.key` | — | — |
| `verify-ca` | `/etc/keystone/db-tls/ca.crt` | `/etc/keystone/db-tls/tls.crt` | `/etc/keystone/db-tls/tls.key` | `true` | — |
| `verify-full` | `/etc/keystone/db-tls/ca.crt` | `/etc/keystone/db-tls/tls.crt` | `/etc/keystone/db-tls/tls.key` | `true` | `true` |

Parameters are emitted via `url.Values.Encode()`, which sorts keys lexically — so
the resulting query string is deterministic across reconciles regardless of the
insertion order shown in this table. Any other `mode` value is rejected by
`modeToSSLParams` (and earlier by the CRD enum and validating webhook) before the
DSN is assembled, so a partially-formed DSN can never reach a workload.

#### Status condition

`reconcileDatabaseTLS` reports its outcome via the `DatabaseTLSReady` status
condition using these typed reasons:

| Reason | When |
| --- | --- |
| `NotRequired` | `spec.database.tls` is `nil` or `enabled=false` — plaintext connection. |
| `ExternallyManaged` | `enabled=true` but the database is brownfield (`spec.database.host` set, no `clusterRef`) — the operator does not own the trust domain and expects the client keypair to be supplied out-of-band via `clientCertSecretRef`. |
| `CertificatePending` | Managed mode; the operator has created the cert-manager `Certificate` but cert-manager has not yet issued it. The condition is `False` until issuance completes. |
| `CertificateIssued` | Managed mode; the client `Certificate` is issued into Secret `<name>-db-client` and ready for mount. |

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
| `rules` | `map[string]string` | No | Inline policy rule overrides. Keys are oslo.policy rule names; values are rule definitions. Inline rules take precedence over ConfigMap rules. Both the key and the value of every rule must be non-empty. |
| `configMapRef` | `*corev1.LocalObjectReference` | No | Reference to a ConfigMap containing a `policy.yaml` key with rule overrides. |

When `policyOverrides` is set on `KeystoneSpec`, at least one of `rules` or
`configMapRef` must be provided. Every `rules` entry must have a non-empty name
and a non-empty value — an empty value previously passed admission and reached
oslo.policy. All three constraints are enforced by both CEL validation on the
shared `PolicySpec` type and the validating webhook.

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

### GatewaySpec

Gateway API HTTPRoute exposure configuration, promoted into `commonv1` and reused
by both the Keystone operator and the c5c3 ControlPlane. See
[`GatewaySpec`](#gatewayspec) above for the field table and the Keystone-specific
HTTPRoute reconciler behavior, and
[`GatewayParentRefSpec`](#gatewayparentrefspec) for the parent-reference fields.

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
| `spec.credentialKeys.maxActiveKeys` | `== 0` | `3` |
| `spec.cache.backend` | `== ""` | `"dogpile.cache.pymemcache"` |
| `spec.bootstrap.adminUser` | `== ""` | `"admin"` |
| `spec.bootstrap.region` | `== ""` | `"RegionOne"` |
| `spec.uwsgi.processes` | `== 0` (when `spec.uwsgi` is non-nil) | `2` — webhook only; when `spec.uwsgi` is `nil`, the reconciler applies this default internally. |
| `spec.uwsgi.threads` | `== 0` (when `spec.uwsgi` is non-nil) | `1` — same nil-pointer caveat as processes. |
| `spec.uwsgi.httpKeepAlive` | Field absent from JSON payload | `true` — defaulted by the CRD schema (`+kubebuilder:default=true`), **not** by the webhook. The webhook cannot distinguish "not set" from "explicitly false" for a bool field. See [HTTPKeepAlive defaulting](#httpkeepalive-defaulting-caveat). |
| `spec.resources` | `== nil` or empty (`requests` and `limits` both unset) | `{requests: {memory: 256Mi, cpu: 100m}, limits: {memory: 512Mi, cpu: 500m}}` — ensures Burstable QoS class and enables HPA utilization calculations. |
| `spec.database.tls.mode` | `spec.database.tls != nil && mode == ""` | `"require"` — `DefaultDatabaseTLSMode` in `keystone_webhook.go`. Only materialized when the `tls` block is explicitly present; the webhook never materializes the block itself. |

**Not defaulted by the webhook:**

- `spec.fernet.rotationSchedule`, `spec.credentialKeys.rotationSchedule`,
  `spec.trustFlush.schedule`, `spec.autoscaling.minReplicas`, `spec.topologySpreadConstraints`,
  `spec.priorityClassName` — these rely on CRD schema defaults or reconciler-level
  fallbacks. For `topologySpreadConstraints` the reconciler distinguishes `nil`
  (inject zone+hostname defaults) from `[]` (opt out), so the webhook must not
  materialise a struct.
- `spec.database.tls` itself and `spec.database.tls.enabled` — the webhook never
  materializes the `tls` block and never sets `enabled`. TLS
  is strictly opt-in, so an upgrade of a previously plaintext CR cannot silently
  turn encryption on (which would also trigger Certificate provisioning). The
  webhook only partial-fills `tls.mode` when the parent block is explicitly
  present, mirroring the `TrustFlush` / `UWSGI` / `Logging` non-mutating
  discipline.

**Design note:** `spec.fernet.rotationSchedule` is NOT defaulted by the webhook — it
relies solely on the Kubebuilder `+kubebuilder:default="0 0 * * 0"` marker.
The webhook uses conditional checks (`== 0` / `== ""`) rather
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
| Cache mutual exclusivity | `spec.cache` | `field.Invalid` | Both `clusterRef` and `servers` set, or neither. Defense-in-depth alongside the CEL XValidation rule. |
| Database mutual exclusivity | `spec.database` | `field.Invalid` | Both `clusterRef` and `host` set, or neither. Defense-in-depth alongside the CEL XValidation rule. |
| Database TLS mode out-of-enum | `spec.database.tls.mode` | `field.NotSupported` | `tls.mode` is non-empty but not one of `prefer`/`require`/`verify-ca`/`verify-full`. Defense-in-depth alongside the `+kubebuilder:validation:Enum` marker. Empty `mode` is tolerated because `Default()` materializes `"require"` before validation in the normal admission path. |
| Database TLS caBundleSecretRef required | `spec.database.tls.caBundleSecretRef.name` | `field.Required` | `tls.enabled=true` but `caBundleSecretRef.name` is empty. Defense-in-depth alongside the CEL XValidation rule on `spec.database`. |
| Database TLS clientCertSecretRef required | `spec.database.tls.clientCertSecretRef.name` | `field.Required` | `tls.enabled=true` but `clientCertSecretRef.name` is empty. Defense-in-depth alongside the CEL XValidation rule on `spec.database`. |
| Fernet maxActiveKeys minimum | `spec.fernet.maxActiveKeys` | `field.Invalid` | `maxActiveKeys < 3`. Defense-in-depth alongside the `+kubebuilder:validation:Minimum=3` marker. |
| Fernet schedule required | `spec.fernet.rotationSchedule` | `field.Required` | Empty after admission (bypass paths). |
| Fernet cron expression | `spec.fernet.rotationSchedule` | `field.Invalid` | `cron.ParseStandard()` fails. Error message includes the parse failure details. |
| CredentialKeys maxActiveKeys minimum | `spec.credentialKeys.maxActiveKeys` | `field.Invalid` | `maxActiveKeys < 3`. Defense-in-depth alongside the `+kubebuilder:validation:Minimum=3` marker. |
| CredentialKeys schedule required | `spec.credentialKeys.rotationSchedule` | `field.Required` | Empty after admission (bypass paths). |
| CredentialKeys cron expression | `spec.credentialKeys.rotationSchedule` | `field.Invalid` | `cron.ParseStandard()` fails. |
| Duplicate plugin sections | `spec.plugins[i].configSection` | `field.Duplicate` | Two or more plugins share the same `configSection` value. |
| Policy source required | `spec.policyOverrides` | `field.Required` | `policyOverrides` is set but both `rules` and `configMapRef` are nil/empty. |
| Empty policy rule name | `spec.policyOverrides.rules[<key>]` | `field.Required` | A key in the `rules` map is the empty string. Enforced via the shared `policy.ValidatePolicyRules`. |
| Empty policy rule value | `spec.policyOverrides.rules[<key>]` | `field.Required` | A value in the `rules` map is the empty string. Enforced via the shared `policy.ValidatePolicyRules`. |
| Autoscaling maxReplicas minimum | `spec.autoscaling.maxReplicas` | `field.Invalid` | `maxReplicas < 1`. Defense-in-depth alongside the `+kubebuilder:validation:Minimum=1` marker. |
| Autoscaling minReplicas minimum | `spec.autoscaling.minReplicas` | `field.Invalid` | `minReplicas < 1` when set. Defense-in-depth alongside the `+kubebuilder:validation:Minimum=1` marker. |
| Autoscaling min exceeds max | `spec.autoscaling.minReplicas` | `field.Invalid` | `minReplicas > maxReplicas` when set. |
| Autoscaling maxReplicas vs replicas | `spec.autoscaling.maxReplicas` | `field.Invalid` | `minReplicas` is unset and `spec.replicas > autoscaling.maxReplicas`. Would otherwise produce an HPA the API server rejects, because `minReplicas` defaults to `spec.replicas`. |
| Autoscaling CPU utilization range | `spec.autoscaling.targetCPUUtilization` | `field.Invalid` | Value outside `1..100` when set. |
| Autoscaling memory utilization range | `spec.autoscaling.targetMemoryUtilization` | `field.Invalid` | Value outside `1..100` when set. |
| Autoscaling no metric targets | `spec.autoscaling` | `field.Required` | Neither `targetCPUUtilization` nor `targetMemoryUtilization` is set. Defense-in-depth alongside the CEL XValidation rule. |
| NetworkPolicy ingress required | `spec.networkPolicy.ingress` | `field.Required` | `networkPolicy` is set but `ingress` is empty. Defense-in-depth alongside the CEL XValidation rule. |
| uWSGI processes minimum | `spec.uwsgi.processes` | `field.Invalid` | `processes < 1` when `spec.uwsgi` is non-nil. Defense-in-depth alongside the `+kubebuilder:validation:Minimum=1` marker. |
| uWSGI threads minimum | `spec.uwsgi.threads` | `field.Invalid` | `threads < 1` when `spec.uwsgi` is non-nil. Defense-in-depth alongside the `+kubebuilder:validation:Minimum=1` marker. |
| uWSGI harakiri minimum | `spec.uwsgi.harakiri` | `field.Invalid` | `harakiri < 1` when set. Defense-in-depth alongside the `+kubebuilder:validation:Minimum=1` marker. |
| uWSGI keep-alive timeout minimum | `spec.uwsgi.httpKeepAliveTimeout` | `field.Invalid` | `httpKeepAliveTimeout < 1` when set. A zero value is rejected because uWSGI interprets it as unbounded, defeating the graceful-termination contract. |
| uWSGI keep-alive timeout without keep-alive | `spec.uwsgi.httpKeepAliveTimeout` | `field.Invalid` | `httpKeepAliveTimeout` is set while `httpKeepAlive=false`. The `--http-keepalive-timeout` flag is only emitted when keep-alive is enabled, so the combination is rejected to avoid silently dropping user intent. |
| TerminationGracePeriodSeconds minimum | `spec.terminationGracePeriodSeconds` | `field.Invalid` | `terminationGracePeriodSeconds < 10` when set. Defense-in-depth alongside the `+kubebuilder:validation:Minimum=10` marker. |
| PreStopSleepSeconds minimum | `spec.preStopSleepSeconds` | `field.Invalid` | `preStopSleepSeconds < 0` when set. Defense-in-depth alongside the `+kubebuilder:validation:Minimum=0` marker. |
| PreStopSleep ≥ grace period | `spec.preStopSleepSeconds` | `field.Invalid` | Resolved `preStopSleepSeconds >= terminationGracePeriodSeconds` (nil pointers resolve to defaults 5/30). Guarantees a non-zero drain window between the end of the preStop sleep and SIGKILL. |
| Harakiri ≥ drain window | `spec.uwsgi.harakiri` | `field.Invalid` | `harakiri >= terminationGracePeriodSeconds − preStopSleepSeconds` (nil pointers resolve to defaults). Guarantees the per-request kill fits inside the shutdown envelope. |
| Recreate strategy with RollingUpdate | `spec.strategy.rollingUpdate` | `field.Invalid` | `strategy.type = Recreate` combined with a non-nil `strategy.rollingUpdate` block. The Deployment controller would reject the object at apply time; the webhook catches the misconfiguration up-front. |
| Resource request exceeds limit | `spec.resources.requests.<resource>` | `field.Invalid` | A resource request exceeds its corresponding limit (e.g., CPU request 1000m > limit 500m). Checked per resource type when both requests and limits are set. |
| Trust flush schedule required | `spec.trustFlush.schedule` | `field.Required` | `trustFlush` is set but `schedule` is empty. Defense-in-depth — the `+kubebuilder:default` marker normally prevents this, but bypass paths (e.g., `kubectl patch`) may produce an empty string. |
| Trust flush cron expression | `spec.trustFlush.schedule` | `field.Invalid` | `cron.ParseStandard()` fails on `trustFlush.schedule`. Error message includes the parse failure details. |
| PriorityClass existence | `spec.priorityClassName` | `field.NotFound` / `field.InternalError` | The webhook performs a direct (uncached) cluster-scoped `Get` of the referenced `scheduling.k8s.io/v1` `PriorityClass` when the field is non-empty, so a just-created class is never rejected off a stale cache. Missing classes produce `NotFound`; transient API errors produce `InternalError`. |
| TopologySpread labelSelector required | `spec.topologySpreadConstraints[i].labelSelector` | `field.Required` | Entry has no `labelSelector`. |
| TopologySpread matchLabels mismatch | `spec.topologySpreadConstraints[i].labelSelector` | `field.Invalid` | `matchLabels` does not exactly equal `{app.kubernetes.io/name: keystone, app.kubernetes.io/instance: {CR name}}`. |
| TopologySpread matchExpressions forbidden | `spec.topologySpreadConstraints[i].labelSelector.matchExpressions` | `field.Invalid` | `matchExpressions` is non-empty. Only exact `matchLabels` are allowed. |

**Error format:** All validation errors are returned as a structured
`apierrors.StatusError` with `GroupKind{Group: "keystone.openstack.c5c3.io", Kind: "Keystone"}`,
providing clear, field-specific error messages to the operator.

---

## Testing

The Keystone CRD has a three-layer test strategy:

1. **Unit tests** — fast, in-process tests for webhook logic.
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

**Design decisions:**

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
| `TestIntegration_CELRejectsPolicyRuleEmptyValue` | Non-empty rule values | `policyOverrides.rules` with a rule whose value is the empty string | Invalid/Forbidden containing "policyOverrides" |

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
| `TestIntegration_ResourcesDefaultedWhenNil` | Resources defaulted | Creates a CR with `spec.resources` unset (`nil`); verifies the defaulting webhook injects `{requests: {memory: 256Mi, cpu: 100m}, limits: {memory: 512Mi, cpu: 500m}}`. |
| `TestIntegration_ResourcesPreservedWhenExplicit` | Explicit resources preserved | Creates a CR with explicit `spec.resources` (1Gi/2Gi memory, 200m/1 CPU); verifies the defaulting webhook does not overwrite them. |
| `TestIntegration_UWSGIDefaultsAppliedWhenEmpty` | uWSGI defaults applied | Creates a CR with `spec.uwsgi: {}` (all zero values); verifies processes=2, threads=1, httpKeepAlive=true after admission. |
| `TestIntegration_UWSGIExplicitValuesPreserved` | Explicit uWSGI preserved | Creates a CR with `spec.uwsgi.processes=4, threads=4`; verifies these values are not overwritten by the defaulting webhook. |
| `TestIntegration_UWSGIPartialDefaulting` | Partial uWSGI defaults | Creates a CR with only `spec.uwsgi.processes=4`; verifies threads=1 is defaulted while processes=4 is preserved. |
| `TestIntegration_UWSGINilPreserved` | uWSGI nil preserved | Creates a CR without `spec.uwsgi`; verifies the field remains `nil` after admission — webhook does not inject a default struct. |

#### Webhook Validation Rejection

| Test | Requirement | Trigger | Expected Error |
| --- | --- | --- | --- |
| `TestIntegration_ResourcesRequestExceedsLimitRejected` | Request must not exceed limit | `spec.resources` with CPU request 1000m > limit 500m | Invalid/Forbidden containing "resources". |
| `TestIntegration_UWSGIProcessesBelowMinimumRejected` | Processes minimum | `spec.uwsgi.processes` below minimum (bypassing defaulting) | Invalid/Forbidden containing "uwsgi". |
| `TestIntegration_UWSGIThreadsBelowMinimumRejected` | Threads minimum | `spec.uwsgi.threads` below minimum (bypassing defaulting) | Invalid/Forbidden containing "uwsgi". |

### Chainsaw E2E Tests

E2E tests live in `tests/e2e/keystone/` and use the Chainsaw framework
(`chainsaw.kyverno.io/v1alpha2`). The `invalid-cr` suite below verifies webhook
rejection in a real cluster with the operator deployed. For the full reconciler
E2E test suite inventory (basic-deployment, scale, fernet-rotation,
credential-rotation, network-policy, topology-spread, priority-class,
release-upgrade, schema-drift-detection, events, healthcheck, graceful-shutdown,
policy-validation, config-pruning, `database-tls`, …), see
[Keystone E2E Test Suites](../testing/keystone-e2e-tests.md).

#### invalid-cr Suite

The full webhook + CEL rejection matrix extends the original
two-step suite so that every implemented `XValidation` rule and
every `webhook.validate()` branch in `operators/keystone/api/v1alpha1/`
is pinned by a Chainsaw step.

| Step | Manifest | Requirement | Expected Error |
| --- | --- | --- | --- |
| `invalid-cron-expression-rejected` | `00-invalid-cron.yaml` | Invalid cron | Error containing "rotationSchedule" and "invalid cron expression" |
| `duplicate-plugin-config-section-rejected` | `01-duplicate-plugins.yaml` | Duplicate configSection | Error containing "configSection" and "Duplicate value" |
| `database-both-modes-rejected` | `02-database-both-modes.yaml` | DatabaseSpec mutual exclusivity | Error containing "spec.database" and "exactly one of clusterRef or host must be set" |
| `cache-both-modes-rejected` | `03-cache-both-modes.yaml` | CacheSpec mutual exclusivity | Error containing "spec.cache" and "exactly one of clusterRef or servers must be set" |
| `autoscaling-no-target-rejected` | `04-autoscaling-no-target.yaml` | AutoscalingSpec target required | Error containing "spec.autoscaling" and "at least one of targetCPUUtilization or targetMemoryUtilization" |
| `policy-overrides-no-source-rejected` | `05-policy-overrides-no-source.yaml` | PolicyOverrides source required | Error containing "spec.policyOverrides" and "at least one of rules or configMapRef must be set" |
| `policy-overrides-empty-rule-key-rejected` | `06-policy-overrides-empty-rule-key.yaml` | Non-empty rule names | Error containing "spec.policyOverrides" and "policy rule name must not be empty" |
| `networkpolicy-empty-ingress-rejected` | `07-networkpolicy-empty-ingress.yaml` | NetworkPolicy ingress required | Error containing "spec.networkPolicy" and "at least one ingress source" |
| `replicas-negative-rejected` | `09-replicas-negative.yaml` | Replicas Minimum=1 (subsumes the dropped `08-replicas-zero.yaml` case — see layer-ordering aside) | Error containing "replicas" |
| `hpa-min-greater-than-max-rejected` | `10-hpa-min-greater-than-max.yaml` | minReplicas ≤ maxReplicas | Error containing "spec.autoscaling.minReplicas" and "must not exceed maxReplicas" |
| `fernet-maxactivekeys-below-minimum-rejected` | `11-fernet-maxactivekeys-below-minimum.yaml` | Fernet maxActiveKeys Minimum=3 | Error containing "maxActiveKeys" |
| `credentialkeys-maxactivekeys-below-minimum-rejected` | `12-credentialkeys-maxactivekeys-below-minimum.yaml` | CredentialKeys maxActiveKeys Minimum=3 | Error containing "maxActiveKeys" |
| `policy-overrides-empty-rule-value-rejected` | `13-policy-overrides-empty-rule-value.yaml` | Non-empty rule values | Error containing "spec.policyOverrides" and "policy rule value must not be empty" |

Each step uses `apply` with `expect` to assert that the `$error` variable is non-null
and contains the expected field-level error message. Kubernetes admission evaluates
validation in a fixed pipeline — **mutating webhook (defaulting) → CRD structural
schema (incl. CEL `XValidation` rules) → validating webhook** — and the first layer
that rejects an object is the one whose message Chainsaw sees. The mutating step is
listed first because it can silently rewrite a value out from under a downstream
rule: `keystone_webhook.go:80-82` coerces `spec.replicas == 0` to `3` BEFORE the
`+kubebuilder:validation:Minimum=1` marker is evaluated, so a manifest using
`spec.replicas: 0` would be silently accepted. This is the precise reason the
`08-replicas-zero.yaml` case was dropped from the suite: the
`09-replicas-negative.yaml` fixture (`spec.replicas: -1`) uses a
value the defaulter does not touch (the defaulter only fires on `== 0`) and exercises
the same `Minimum=1` and webhook-defense-in-depth path. The same trap applies to
`maxActiveKeys: 0`, which is why the `maxActiveKeys` fixtures use `2` rather than `0`.

For most rules the producing layer is unambiguous (CEL emits the exact
"exactly one of …", "at least one of …", "must not exceed maxReplicas" wording),
so the assertions match the full webhook-equivalent message. The
`06-policy-overrides-empty-rule-key.yaml` and
`07-networkpolicy-empty-ingress.yaml` fixtures are the dual-layer exceptions where the
fieldPath emitted by CEL is the parent path (`spec.policyOverrides` /
`spec.networkPolicy`) — the path where the `XValidation` rule is declared — and
NOT the deeper path the validating webhook would emit (`…rules` / `…ingress`).
Because CEL fails first and short-circuits the admission pipeline, the validating
webhook's deeper-path message never reaches Chainsaw, so the assertions match only
the parent path. The `11-fernet-maxactivekeys-below-minimum.yaml` and
`12-credentialkeys-maxactivekeys-below-minimum.yaml` fixtures are the field-substring
exceptions: they trip the CRD structural schema's `Minimum=N` first, whose generated
wording ("must be greater than or equal to N") differs from the webhook's
defense-in-depth wording ("maxActiveKeys must be at least 3"). Both layers carry
the field name, so the loose-substring assertion (`maxActiveKeys`) keeps the tests
stable regardless of which layer fires first and across upstream Kubernetes
admission-pipeline changes.

The 10 generated fixtures (`02-…` through `12-…`, with the `08-replicas-zero.yaml`
gap explained above) share an otherwise-identical minimal valid Keystone scaffold
and differ only by the field under test. To prevent that scaffold from drifting
across files, the fixtures are generated from a
single canonical source in `tests/e2e/keystone/invalid-cr/_generate.py`. After
editing the scaffold or any per-fixture override, regenerate via
`python3 tests/e2e/keystone/invalid-cr/_generate.py`. The
`verify-invalid-cr-fixtures` CI job (and the matching
`make verify-invalid-cr-fixtures` Makefile target) runs `_generate.py --check`
in drift mode and the `test_generate.py` unit suite (`len(FIXTURES) == 10` plus
a cross-reference assertion that every `Fixture.filename` appears as a
`file:` step in `chainsaw-test.yaml`), so a hand-edit to any generated fixture
— or a rename/removal that desynchs `FIXTURES` from `chainsaw-test.yaml` —
fails the build before the cluster-bound `e2e-operator` job runs. The
`00-invalid-cron.yaml` and `01-duplicate-plugins.yaml` fixtures predate the
generator and are intentionally NOT regenerated.

The following follow-up gaps are intentionally **not** covered by this
suite — they require new validation rules that do not exist yet, and each one
is tracked as its own feature ticket:

- Empty / malformed `spec.image.tag` (no `MinLength` or pattern on `ImageSpec.Tag`).
- `topologySpreadConstraints[*].maxSkew: 0` (no CRD-level minimum on the upstream type, no defense-in-depth in the Keystone webhook).
- Mutation of immutable fields (`spec.database.clusterRef`, `spec.cache.clusterRef`) on `ValidateUpdate` — old-vs-new comparison is not yet implemented.

#### uwsgi Suite

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
- `NetworkPolicySpec`
- `NetworkPolicyIngressSource`
- `UWSGISpec`
- `TrustFlushSpec`
- `FernetSpec`
- `CredentialKeysSpec`
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
│   ├── integration_test.go           envtest integration tests
│   └── zz_generated.deepcopy.go     Generated DeepCopy methods
├── config/crd/bases/
│   └── keystone.openstack.c5c3.io_keystones.yaml  Generated CRD manifest
├── config/webhook/
│   ├── manifests.yaml                Generated webhook configurations
│   └── ...
├── internal/testutil/
│   └── envtest_setup.go              Keystone-specific envtest helper
└── main.go                           Scheme registration + bootstrap + webhook wiring

tests/e2e/keystone/
├── basic-deployment/                 Happy-path reconciliation E2E
├── missing-secret/                   Secret dependency recovery E2E
├── fernet-rotation/                  Fernet key rotation E2E
├── scale/                            Replica scaling E2E
├── deletion-cleanup/                 Garbage collection E2E
├── policy-overrides/                 oslo.policy integration E2E
├── middleware-config/                Middleware pipeline E2E
├── brownfield-database/              External database mode E2E
├── database-tls/                     Database TLS/mTLS E2E
│   ├── chainsaw-test.yaml            Chainsaw E2E test definition
│   └── 00-keystone-cr.yaml           Keystone CR with spec.database.tls (verify-full)
├── image-upgrade/                    Rolling image upgrade E2E
├── uwsgi/                            uWSGI field propagation E2E
│   ├── chainsaw-test.yaml            Chainsaw E2E test definition
│   ├── 00-keystone-cr.yaml           Keystone CR without explicit uWSGI
│   └── 01-patch-custom-uwsgi.yaml    Patch with custom uWSGI values
└── invalid-cr/
    ├── chainsaw-test.yaml                                  Chainsaw E2E test definition
    ├── 00-invalid-cron.yaml                                Invalid cron expression CR manifest
    ├── 01-duplicate-plugins.yaml                           Duplicate plugin configSection CR manifest
    ├── 02-database-both-modes.yaml                         Database clusterRef + host both set
    ├── 03-cache-both-modes.yaml                            Cache clusterRef + servers both set
    ├── 04-autoscaling-no-target.yaml                       Autoscaling without utilization target
    ├── 05-policy-overrides-no-source.yaml                  PolicyOverrides without rules or configMapRef
    ├── 06-policy-overrides-empty-rule-key.yaml             PolicyOverrides rule with empty key
    ├── 07-networkpolicy-empty-ingress.yaml                 NetworkPolicy with empty ingress array
    ├── 09-replicas-negative.yaml                           spec.replicas: -1 (subsumes the dropped 08-replicas-zero case)
    ├── 10-hpa-min-greater-than-max.yaml                    HPA minReplicas > maxReplicas
    ├── 11-fernet-maxactivekeys-below-minimum.yaml          Fernet maxActiveKeys < 3
    └── 12-credentialkeys-maxactivekeys-below-minimum.yaml  CredentialKeys maxActiveKeys < 3
```

This layout is the canonical pattern for all CobaltCore operators. New operators
should replicate this directory structure.
