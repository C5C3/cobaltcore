---
title: Advanced Configuration
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Advanced Configuration

Beyond the minimal control plane from the
[Quick Start (ControlPlane)](../quick-start-controlplane.md), the operators
support a number of configuration options for real cluster deployments. This
guide covers the ones the `ControlPlane` CR exposes and points to the reference
for the rest. See [Standalone Keystone](#standalone-keystone-without-a-controlplane)
for knobs that live only on a Keystone CR you own.

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so a `ControlPlane`
CR named `controlplane` is `Ready` in the `openstack` namespace and its projected
`controlplane-keystone` Keystone child is running. Every resource name in the
examples below is one that devstack produces.
:::

::: warning The Keystone child is operator-owned
On a ControlPlane deployment the `controlplane-keystone` Keystone CR is
**projected** by the c5c3-operator; the projected fields are re-asserted on every
reconcile, so editing them on the child is reverted. Configure the knobs the
`ControlPlane` CRD exposes on the `ControlPlane` CR. Set a knob the CRD does not
expose on a Keystone CR you own, in the
[Standalone Keystone](#standalone-keystone-without-a-controlplane) section. See
the [ControlPlane Reconciler](../reference/c5c3/controlplane-reconciler.md) for
the projection contract.
:::

Each section covers an independent recipe. Apply only what you need.

---

## Infrastructure database

### Brownfield database and cache

The Quick Start uses managed mode, where the operator provisions the MariaDB and
Memcached the control plane connects to (`spec.infrastructure.database.clusterRef`
and `cache.clusterRef`). If MariaDB/Galera and Memcached already run outside the
operator's reach, use **brownfield mode** with explicit connection parameters on
the `ControlPlane` CR.

Using an existing database is a choice you make when you create the
`ControlPlane`. The validating webhook freezes database parameters (name,
replicas, and `storageSize`) after creation. It also freezes the database and
cache modes: managed mode uses `clusterRef`, while brownfield mode uses `host` or
`servers`. Set `spec.infrastructure` when you first apply the CR:

```yaml
apiVersion: c5c3.io/v1alpha1
kind: ControlPlane
metadata:
  name: controlplane
  namespace: openstack
spec:
  openStackRelease: "2025.2"
  # services.keystone and korc as in the Quick Start (ControlPlane)
  infrastructure:
    database:
      # brownfield: explicit host/port, no clusterRef
      host: mariadb.db.example.com
      port: 3306
      database: keystone
      secretRef:
        name: keystone-db
    cache:
      backend: dogpile.cache.pymemcache
      # brownfield cache: explicit server list, no clusterRef
      servers:
        - "memcached.cache.example.com:11211"
```

The reconciler copies the `infrastructure.database` and `infrastructure.cache`
blocks onto the `controlplane-keystone` child, so the child connects to the
servers declared here.

::: warning Brownfield expects an existing database
In brownfield mode (no `clusterRef`), the operator does not create the supplied
Secret, database, user, or grants. Create those before the control plane
reconciles:

```sql
CREATE DATABASE keystone DEFAULT CHARACTER SET utf8 COLLATE utf8_general_ci;
CREATE USER 'keystone'@'%' IDENTIFIED BY '<password-from-secretRef>';
GRANT ALL PRIVILEGES ON keystone.* TO 'keystone'@'%';
FLUSH PRIVILEGES;
```

The Secret referenced by `secretRef` must contain matching `username` and
`password` keys. The keystone-operator requires both for `SecretsReady`; a Secret
with only `password` leaves `controlplane-keystone` at `SecretsReady=False`.
After the database, user, grants, and Secret exist, `db_sync` creates the
Keystone schema on first reconcile. Step 4 of the
[Quick Start (ControlPlane)](../quick-start-controlplane.md) applies only to
managed mode's engine-issued database credentials. Brownfield mode does not use
the OpenBao database engine.
:::

The webhook requires exactly one of `clusterRef` or `host` (`servers` for cache)
for both `database` and `cache`; never set both.

For a managed database, replica count and volume size cannot be changed through
the `ControlPlane` after creation. The MariaDB operator treats volume size as
immutable. Set an adequate size before creating the control plane. For later
growth, follow the MariaDB operator and storage provider's supported expansion
or migration procedure. CobaltCore does not provide an in-place resize workflow.

---

## Free-form service configuration

The `ControlPlane` exposes the same free-form configuration escape hatch as the
service CRs, at two levels. `spec.globalExtraConfig` applies to every declared
INI-configured service, such as Keystone, Glance, Placement, Barbican, and
Neutron. `spec.services.<svc>.extraConfig` sets one service's block. For INI
services, the map shape is `map[section][key] = value`. Horizon uses a flat map
of Django settings instead; see
[Horizon settings are flat, not INI](#horizon-settings-are-flat-not-ini).

```yaml
apiVersion: c5c3.io/v1alpha1
kind: ControlPlane
metadata:
  name: controlplane
  namespace: openstack
spec:
  openStackRelease: "2025.2"
  # Applied to every declared INI service (for example Keystone, Glance,
  # Placement, Barbican, and Neutron):
  globalExtraConfig:
    database:
      pool_timeout: "30"
  services:
    keystone:
      extraConfig:
        database:
          pool_timeout: "60"           # wins over the global value for Keystone
        token:
          expiration: "43200"          # 12h instead of the default 1h
    horizon:
      extraConfig:
        SESSION_TIMEOUT: 7200           # flat Django setting, not INI
```

The reconciler projects the merged INI result onto each INI service child's
`spec.extraConfig`. It projects the Horizon block verbatim onto the dashboard
child; see [Horizon settings are flat, not INI](#horizon-settings-are-flat-not-ini).

### Merge semantics

For each INI service the global and per-service blocks merge key by key, with
the per-service value taking precedence. Whole sections are unioned: a
per-service `[database]` block that sets only `pool_timeout` still inherits every
other `[database]` key from the global block. A global key with no per-service
counterpart stays effective. In the example above Keystone renders
`[database] pool_timeout = 60`, while Glance with no block of its own renders
`30` from the global block.

The option catalog checks the merged result, so each global key must be valid
for every declared INI service. A Keystone-only option in
`spec.globalExtraConfig` can fail validation against another declared service.
Put that key in `spec.services.keystone.extraConfig` instead.

### Horizon settings are flat, not INI

The dashboard renders `local_settings.py`, so `spec.services.horizon.extraConfig`
is a flat `map[setting] = value` of Django settings (`SESSION_TIMEOUT` above),
projected verbatim onto the Horizon child. `spec.globalExtraConfig` is INI and
never reaches the dashboard, and there is no merge for the Horizon block.

### External keystone mode

`spec.services.keystone.extraConfig` is forbidden when
`services.keystone.mode` is `External`: no Keystone workload is deployed to
render it. Both a CEL rule and the webhook reject it. `spec.globalExtraConfig`
and `spec.globalPolicyOverrides` remain allowed but have no effect in External
mode because no INI-configured workload consumes them. Glance, Horizon,
Placement, and Barbican are forbidden in External mode, so their service blocks
cannot appear.

### Admission checks

The validating webhook runs two families of checks at `ControlPlane` admission,
using option catalogs and ownership registries embedded from the service API
packages.

**Shape and ownership** run on every create and update. Empty section or key
names in any INI block are rejected. Horizon setting names must be non-empty
Python identifiers. The webhook also rejects keys the ControlPlane projects
itself: Glance's `[keystone_authtoken] password` (always), the Horizon
`SECRET_KEY` and every WebSSO / multi-domain setting (always, since the
ControlPlane projects those dynamically from the attached identity backends), and
Keystone's `[federation] trusted_dashboard` only when the ControlPlane derives a
dashboard endpoint from `services.horizon`. With no Horizon block that Keystone
key is admitted with a warning, so an externally-run dashboard can still do WebSSO
against the managed Keystone. Any other operator-owned key is honored but draws an
admission warning naming the key, its owner, its impact, and the block that set it.

**Option-catalog** validation runs on every create. Updates rerun it only when a
catalog input changes: either INI block, `spec.openStackRelease`,
`services.keystone.image`, or a newly-declared service. A replicas bump alone does
not re-run it, so a stored CR whose `extraConfig` went stale-invalid against a
regenerated catalog is not rejected by an unrelated edit. Each declared INI
service's merged result is checked against that service's per-release option
catalog. Keystone uses `services.keystone.image.tag` when its image is overridden
and otherwise uses `spec.openStackRelease`; the other ControlPlane catalogs use
`spec.openStackRelease`. The standalone service operators run the same option
check against their own configured image or release. Unknown sections and options
are rejected; a deprecated-but-accepted option draws a warning naming its
replacement. Plugin-registered INI sections are rejected as unknown because the
ControlPlane has no plugins field and does not set `spec.plugins` on a child.
Configure plugin sections on the service CR directly. Neither family has a CEL or
CRD-schema backstop; both checks live only in the webhook.

When the c5c3-operator cannot map an image or release to an embedded catalog, it
skips the option-name check and returns a warning instead of rejecting the CR.
This can happen with a digest-pinned image, a tag that does not identify a
release, or a release missing from the operator build. In those cases admission
does not verify whether the option or section exists.

::: warning The projected children are operator-owned
The merged INI result and the Horizon block are re-asserted on the service
children on every reconcile. While no `ControlPlane` block is set the projection
carries no `extraConfig`, so a value set directly on a child stays untouched. Once
you set any `ControlPlane` block, the projection owns the child's field: a direct
edit on the child is reverted on the next reconcile. Clearing every `ControlPlane`
block projects nothing and the child's field reverts to unset. Configure the
free-form config on the `ControlPlane`, not on the child.
:::

### Catalog skew across operator builds

The c5c3-operator and a service operator may embed different catalogs. If the
service webhook rejects the projected child, the ControlPlane reports
`KeystoneProjectionRejected` or `GlanceProjectionRejected` on its conditions.

---

## Feature pointer table

| Feature | Keystone CR field | ControlPlane path | Reference |
|---------|-------------------|-------------------|-----------|
| Replica count | `spec.deployment.replicas` | `spec.sizing.keystone.api.replicas` | [Scale replicas](./day-2-operations.md#scale-replicas) |
| Release / image | `spec.image` | `spec.openStackRelease` (tag) + `spec.services.keystone.image` (override) | [Upgrade release](./day-2-operations.md#upgrade-the-openstack-release) |
| Policy overrides | `spec.policyOverrides` | `spec.services.keystone.policyOverrides` (+ `spec.globalPolicyOverrides`) | [PolicySpec](../reference/keystone/keystone-crd.md#policyspec) |
| Federation proxy image | `spec.federation.proxyImage` | `spec.services.keystone.federationProxyImage` | [Attach an OIDC Federation Backend](./keystone/oidc-federation.md) |
| Public endpoint / gateway | `spec.bootstrap.publicEndpoint`, `spec.gateway` | `spec.services.keystone.publicEndpoint`, `spec.services.keystone.gateway` | [BootstrapSpec](../reference/keystone/keystone-crd.md#bootstrapspec) |
| Fernet / credential-key schedule | `spec.fernet`, `spec.credentialKeys` | `spec.services.keystone.rotationInterval` (schedule only) | [Rotate Fernet keys](./day-2-operations.md#rotate-fernet-keys-manually) |
| Database TLS/mTLS | `spec.database.tls` | `spec.infrastructure.database.tls` | [Enable Keystone Database TLS/mTLS](./keystone/enable-keystone-database-tls.md) |
| Autoscaling (HPA) | `spec.autoscaling` | `spec.sizing.keystone.api.autoscaling` | [Autoscaling (HPA)](#autoscaling-hpa) |
| Network policy | `spec.networkPolicy` | standalone CR only | [Network policy](#network-policy) |
| Free-form config (`extraConfig`) | `spec.extraConfig` | `spec.services.<svc>.extraConfig` (+ `spec.globalExtraConfig`) | [Free-form service configuration](#free-form-service-configuration) |
| Scheduled admin-password rotation | `spec.passwordRotation` | standalone CR only | [Schedule Admin Password Rotation](./keystone/keystone-admin-password-scheduled-rotation.md) |
| uWSGI tuning | `spec.uwsgi` | `spec.sizing.keystone.api.processes`, `.threads` (other uWSGI settings are standalone CR only) | [UWSGISpec](../reference/keystone/keystone-crd.md#uwsgispec) |
| Logging | `spec.logging` | standalone CR only | [LoggingSpec](../reference/keystone/keystone-crd.md#loggingspec) |
| Trust flush | `spec.trustFlush` | standalone CR only | [TrustFlushSpec](../reference/keystone/keystone-crd.md#trustflushspec) |
| Middleware | `spec.middleware` | standalone CR only | [MiddlewareSpec](../reference/keystone/keystone-crd.md#middlewarespec) |
| Plugins | `spec.plugins` | standalone CR only | [PluginSpec](../reference/keystone/keystone-crd.md#pluginspec) |
| Rollout strategy | `spec.deployment.strategy` | standalone CR only | [Graceful-termination fields](../reference/keystone/keystone-crd.md#graceful-termination-fields) |
| Graceful termination | `spec.deployment.terminationGracePeriodSeconds`, `spec.deployment.preStopSleepSeconds` | standalone CR only | [Graceful-termination fields](../reference/keystone/keystone-crd.md#graceful-termination-fields) |
| Topology spread | `spec.deployment.topologySpreadConstraints` | `spec.sizing.keystone.api.spreadConstraints` (the ControlPlane adds the pod selector) | [TopologySpreadConstraints](../reference/keystone/keystone-crd.md#topologyspreadconstraints) |
| Priority class | `spec.deployment.priorityClassName` | `spec.sizing.keystone.api.priorityClassName` (+ `spec.sizing.priorityClassName`) | [PriorityClassName](../reference/keystone/keystone-crd.md#priorityclassname) |
| Resource requests/limits | `spec.deployment.resources` | `spec.sizing.keystone.api.resources` | [KeystoneSpec](../reference/keystone/keystone-crd.md#keystonespec) |
| Node placement | `spec.deployment.nodeSelector`, `spec.deployment.tolerations`, `spec.deployment.affinity` | `spec.sizing.keystone.api.nodeSelector`, `.tolerations` (+ `spec.sizing.nodeSelector`, `.tolerations`); affinity is standalone CR only | [NodePlacementSpec](../reference/keystone/keystone-crd.md#nodeplacementspec) |
| Job and CronJob pods | `spec.jobs` | `spec.sizing.keystone.jobs` (resources and priority class) | [JobSpec](../reference/keystone/keystone-crd.md#jobspec) |

The `spec.sizing` paths size every service the same way (`spec.sizing.<svc>.api`
and the service's other components), starting from a built-in `Minimal` or
`Standard` profile or a site `SizingProfile`; see
[SizingSpec](../reference/c5c3/controlplane-crd.md#sizingspec). Fields marked
"standalone CR only" are not exposed through the `ControlPlane` CRD. Set them
on a Keystone CR you own, as shown in the
[Standalone Keystone](#standalone-keystone-without-a-controlplane) section.

---

## Standalone Keystone, without a ControlPlane

The [Quick Start](../quick-start.md) and [Quick Start (Extended)](../quick-start-extended.md)
devstacks create a standalone Keystone CR named `keystone`. The recipes below
apply to that CR. `spec.networkPolicy` is not exposed through the `ControlPlane`
CRD, so configure it on a standalone Keystone CR.

### Brownfield database

The standalone equivalent of the ControlPlane brownfield recipe above uses
explicit `host`/`port` and `servers` on the Keystone CR:

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: Keystone
metadata:
  name: keystone
  namespace: openstack
spec:
  deployment:
    replicas: 1
  image:
    repository: ghcr.io/c5c3/keystone
    tag: "2025.2"
  database:
    # brownfield: explicit host/port, no clusterRef
    host: mariadb.db.example.com
    port: 3306
    database: keystone
    secretRef:
      name: keystone-db
  cache:
    backend: dogpile.cache.pymemcache
    # brownfield cache: explicit server list, no clusterRef
    servers:
      - "memcached.cache.example.com:11211"
  fernet:
    rotationSchedule: "0 0 * * 0"
    maxActiveKeys: 3
  bootstrap:
    adminUser: admin
    adminPasswordSecretRef:
      name: keystone-admin
    region: RegionOne
```

The same SQL provisioning and `username`+`password` Secret contract from the
ControlPlane recipe apply. For both `database` and `cache`, the webhook requires
exactly one of `clusterRef` or `host` and rejects both being set.

### Autoscaling (HPA)

Keystone is the example in this section. The same autoscaling behavior applies
to other service APIs that expose `spec.autoscaling`. On a ControlPlane, set the
Keystone block as `spec.sizing.keystone.api.autoscaling`;
the reconciler projects it onto the child's `spec.autoscaling`. On a standalone
Keystone, replace hand-patching `spec.deployment.replicas` with a
`HorizontalPodAutoscaler` managed by the operator. When `spec.autoscaling` is
present, the HPA owns the Deployment's replica count.

```yaml
spec:
  deployment:
    replicas: 3       # seeds the Deployment; HPA owns the Deployment replica count once created
  autoscaling:
    minReplicas: 2
    maxReplicas: 10
    targetCPUUtilization: 80
    targetMemoryUtilization: 70
```

- At least one of `targetCPUUtilization` or `targetMemoryUtilization` is required.
- `minReplicas` defaults to `spec.deployment.replicas` if unset.
- The API PodDisruptionBudget follows `minReplicas`. At `minReplicas: 1` it
  switches to `maxUnavailable: 1`, so a node drain can still evict the one pod the
  HPA may leave running. Above one it keeps `minAvailable: 1`.
- The HPA measures a target against the summed requests of every container in
  the API pod. While a target is set, the webhook rejects a zero or negative
  request for the resource it measures, on the API container and on a sidecar
  such as Keystone's federation proxy or Glance's image-cache maintenance
  container. A block that names no request gets a positive default at render
  time.
- A target may exceed 100. The operators set no default CPU limit, so an API
  pod regularly uses several times its CPU request, and a CPU target of 80% of
  a 50m request scales out on almost any traffic. A CPU target such as 150 or
  300 matches what the pods can really use. Memory differs: a container whose
  block names no memory renders the same figure as request and limit, so it
  can never use more than 100% of its request. The webhook rejects a target
  that no container of the API pod can reach, with `can never be reached`; a
  memory target above 100 needs a container that names a memory limit above
  its request, or a memory request without a limit.
- `behavior` tunes how fast the HPA scales. The operator copies the block into
  the HPA's `spec.behavior` and sets no default of its own, so without it
  Kubernetes scales up at once and scales down after a 300-second
  stabilization window. The webhook applies the `autoscaling/v2` bounds at
  admission (window 0 to 3600 s, policy period 1 to 1800 s). `tolerance` is a
  Kubernetes quantity: write it as a string or a milli-value (`"0.05"` or
  `50m`), because the CRD schema rejects a bare decimal such as `0.05`. It
  needs Kubernetes 1.33 or newer. This block keeps the default scale-up and
  releases one surplus pod per 30 seconds once the load has stayed low for a
  minute:

  ```yaml
  spec:
    autoscaling:
      minReplicas: 2
      maxReplicas: 10
      targetCPUUtilization: 150
      behavior:
        scaleDown:
          stabilizationWindowSeconds: 60
          policies:
            - type: Pods
              value: 1
              periodSeconds: 30
  ```
- The generated HPA references `deploy/keystone` and uses the Kubernetes standard
  The generated HPA references `deploy/keystone` and uses the Kubernetes standard
  `metrics-server`. The Quick Start kind cluster does **not** ship one by default.
  Without a resource-metrics API, the HPA reports `unknown/80%`.

  On the kind devstack, opt in with the `WITH_METRICS_SERVER` flag. Bring the
  devstack up with it set (the recipe here also needs the ControlPlane, so the
  flags compose):

  ```bash
  KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_METRICS_SERVER=true make deploy-infra
  ```

  Or, if the devstack is already running, apply the kind overlay additively and
  wait for it to reconcile:

  ```bash
  kubectl apply -k deploy/kind/metrics-server
  kubectl wait helmrelease/metrics-server -n kube-system --for=condition=Ready --timeout=5m
  kubectl top pods -n openstack   # sanity check: real utilisation, not an error
  ```

  The overlay pins the chart to a single major range and enables
  `--kubelet-insecure-tls`, which kind requires.

  On non-kind clusters, `metrics-server` is usually already present: most managed
  Kubernetes distributions ship it. If yours does not, install it per the
  [upstream project](https://github.com/kubernetes-sigs/metrics-server) rather
  than copy-pasting an unpinned manifest.

Inspect the HPA:

```bash
kubectl get hpa -n openstack -l app.kubernetes.io/instance=keystone
kubectl describe hpa keystone -n openstack
```

Removing `spec.autoscaling` deletes the HPA and returns replica control to
`spec.deployment.replicas`. See [HPA Resource Mapping in the CRD reference](../reference/keystone/keystone-crd.md#hpa-resource-mapping)
for the exact field-to-resource mapping.

### Network policy

Keystone is the example in this section. Its `spec.networkPolicy` is not
exposed through the `ControlPlane` CRD. The policy restricts ingress to the
Keystone API pods and derives egress rules for its database, cache, and DNS.
Other service operators derive egress rules from their own backends and settings.
Only ingress sources are configured here.

```yaml
spec:
  networkPolicy:
    ingress:
      # Allow the ingress gateway to reach the Keystone API
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: envoy-gateway-system
      # Allow the monitoring namespace to scrape metrics
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: monitoring
```

Each list entry requires a `namespaceSelector` and may narrow it with an optional
`podSelector`. Both are Kubernetes `metav1.LabelSelector`s and support
`matchLabels` or set-based `matchExpressions`. Within one entry the selectors
AND together; multiple entries OR. Keystone ingress is restricted to TCP 5000.
There is no per-entry port configuration. A non-empty list blocks all other
ingress by default.

For targets that auto-derivation cannot see, such as an off-cluster MariaDB host
or an external IdP, add rules with `spec.networkPolicy.additionalEgress`. These
rules are added after the auto-derived rules and do not replace them.

Removing `spec.networkPolicy` deletes the NetworkPolicy and restores unrestricted
traffic. The [Keystone NetworkPolicy reference](../reference/keystone/keystone-crd.md#networkpolicyspec)
covers its derived egress rules. Other services have different rules based on
their configured backends.

### ExtraConfig: free-form INI sections

On a standalone Keystone CR, set `spec.extraConfig` directly. On a ControlPlane,
use `spec.globalExtraConfig` or `spec.services.<svc>.extraConfig` (see
[Free-form service configuration](#free-form-service-configuration)). Use
`spec.extraConfig` for settings not represented by typed fields, such as logging
levels, oslo.messaging tuning, and experimental Keystone flags. It takes a
`map[section][key] = value` rendered into `keystone.conf`.

For INI-file services such as Keystone, Glance, Placement, and Barbican, the
operator merges configuration in this order: `plugins < operator defaults <
spec.extraConfig`. A plugin cannot override an operator-computed value, and
`spec.extraConfig` is the only way to override the operator's defaults.
Each operator ships a registry of configuration keys it computes. An override
through `spec.extraConfig` takes effect and is reported by the
`ExtraConfigHealthy=False` condition, which names the overridden keys. The
operator emits a one-shot `ExtraConfigOwnedKeyOverride` Warning event when the
condition first changes to that state. Most registered keys are report-only.
The service webhook rejects some keys at admission when a typed spec field owns
them, for example Keystone's `[federation] trusted_dashboard`, Glance's
`[keystone_authtoken] password`, and Horizon's `SECRET_KEY`.

```yaml
spec:
  extraConfig:
    DEFAULT:
      debug: "true"
      log_dir: "/var/log/keystone"
    token:
      expiration: "43200"        # 12h instead of default 1h
      allow_expired_window: "172800"
    oslo_messaging_rabbit:
      heartbeat_timeout_threshold: "60"
```

The `[DEFAULT] debug` override above is an operator-owned key. Keystone computes
it from the typed `spec.logging.debug` field, so this example draws an
`ExtraConfigOwnedKeyOverride` Warning event and flips `ExtraConfigHealthy` to
`False` while still taking effect.

Beyond the ownership registry, the validating webhook checks every option name in
`spec.extraConfig` against a catalog of the options the service accepts. Each
operator embeds one catalog per OpenStack release, generated from the
`oslo-config-generator` output for the service image shipped for that release.
The catalog contains option names. Values are not inspected. Each service
operator selects a catalog from the release or image tag configured on its CR.
An unknown option or section is rejected at apply time.

Three classes of section or key are exempt from the catalog check. A section
declared by a `spec.plugins` entry's `configSection` is trusted, because the
plugin owns it and its options are not in the base catalog. Every key in the
operator-ownership registry above is exempt too, since the operator already
governs those. For Glance, the reserved store sections `os_glance_staging_store`
and `os_glance_tasks_store` are additionally allowed.

When the operator cannot match the configured release or image tag to an
embedded catalog, admission skips the option-name check and returns a warning
instead of rejecting the CR. This can happen with a digest-pinned Keystone
image, a tag that does not identify a release, or a release without a catalog in
the operator build. Admission does not verify that the option exists in these
cases. A deprecated option that the service still accepts is admitted with a
warning naming its replacement, such as `[DEFAULT] logfile`, superseded by
`[DEFAULT] log_file`.

This check lives only in the webhook. There is no CEL or CRD-schema backstop, so
a cluster with the validating webhook disabled accepts a misspelled option name
and surfaces it only at render time. Updates re-run the check only when
`spec.extraConfig`, the plugin section list, or the release field changes.

The operator does not validate option values. A wrong value can be ignored or
cause a crash loop, so test changes in a lab before rollout. Changing
`extraConfig` triggers a ConfigMap rehash and a rolling Deployment update.

---

## Further reading

- [Keystone CRD API Reference](../reference/keystone/keystone-crd.md) — complete field-by-field reference with validation rules and examples
- [ControlPlane CRD API Reference](../reference/c5c3/controlplane-crd.md) — the `spec.*` fields the ControlPlane exposes, including `spec.infrastructure`
- [Observability & Diagnostics](./observability.md) — how to verify a new configuration took effect
- [Day 2 Operations](./day-2-operations.md) — scale, upgrade, rotate using the configured CR

## Tested by

The recipes above are exercised on the CI e2e kind cluster — the operator
installed with a dev image — by these chainsaw suites:

```bash
chainsaw test --test-dir tests/e2e/keystone/brownfield-database
chainsaw test --test-dir tests/e2e/keystone/autoscaling
chainsaw test --test-dir tests/e2e/keystone/network-policy
```
