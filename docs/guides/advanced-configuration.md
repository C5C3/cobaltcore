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
for the rest — and to the [Standalone Keystone](#standalone-keystone-without-a-controlplane)
section for the knobs that live only on a Keystone CR you own.

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
`ControlPlane` CRD exposes on the `ControlPlane` CR. A knob the CRD does not
expose is **standalone-only** — apply it to a Keystone CR you own, in the
[Standalone Keystone](#standalone-keystone-without-a-controlplane) section. See
the [ControlPlane Reconciler](../reference/c5c3/controlplane-reconciler.md) for
the projection contract.
:::

Each pattern below is an independent recipe — apply only what you need.

---

## Brownfield database and cache

The Quick Start uses "managed mode", where the operator provisions the MariaDB
and Memcached the control plane connects to (`spec.infrastructure.database.clusterRef`
/ `cache.clusterRef`). If you already run MariaDB/Galera and Memcached outside the
operator's reach — managed by another team, hosted externally, or on a different
operator — use **brownfield mode** with explicit connection parameters on the
`ControlPlane` CR.

Brownfield is a **creation-time** decision. The validating webhook freezes
infrastructure presence and the database/cache mode (managed `clusterRef` vs
brownfield `host`/`servers`), the database name, replicas, and storageSize after
the ControlPlane is created, so you cannot flip a managed control plane to
brownfield in place — set `spec.infrastructure` when you first apply the CR:

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

The reconciler deep-copies the whole `infrastructure.database` and
`infrastructure.cache` blocks onto the `controlplane-keystone` child, so the
child connects to exactly the servers you declared here.

::: warning In brownfield mode you own schema setup
In brownfield mode (no `clusterRef`) the operator leaves the `secretRef` you
supplied in place — you own that Secret out-of-band — and does **not** create the
database, user, or grants. Provision them before the control plane reconciles:

```sql
CREATE DATABASE keystone DEFAULT CHARACTER SET utf8 COLLATE utf8_general_ci;
CREATE USER 'keystone'@'%' IDENTIFIED BY '<password-from-secretRef>';
GRANT ALL PRIVILEGES ON keystone.* TO 'keystone'@'%';
FLUSH PRIVILEGES;
```

The Secret referenced by `secretRef` must contain both a `username` and a
`password` key matching the SQL user — the keystone-operator gates `SecretsReady`
on the child on both, so a Secret with only `password` leaves
`controlplane-keystone` stuck at `SecretsReady=False`. Once those exist,
`db_sync` creates the Keystone schema on first reconcile. The OpenBao
database-tenant onboarding from the [Quick Start (ControlPlane)](../quick-start-controlplane.md)
(Step 4) applies to **managed** mode's engine-issued (Dynamic) credentials only —
a brownfield control plane draws no credentials from the OpenBao database engine.
:::

The webhook enforces that exactly one of `clusterRef` or `host` (`servers` for
cache) is set — never both — for both `database` and `cache`.

---

## Feature pointer table

Everything else the control plane supports. One-line hints, the ControlPlane knob
that projects it (or "not exposed" where it is standalone-only), and a link to the
full Keystone CR reference.

| Feature | Keystone CR field | ControlPlane path | Reference |
|---------|-------------------|-------------------|-----------|
| Replica count | `spec.deployment.replicas` | `spec.services.keystone.replicas` | [Day 2 — Scale](./day-2-operations.md#scale-replicas) |
| Release / image | `spec.image` | `spec.openStackRelease` (tag) + `spec.services.keystone.image` (override) | [Day 2 — Upgrade](./day-2-operations.md#upgrade-the-openstack-release) |
| Policy overrides | `spec.policyOverrides` | `spec.services.keystone.policyOverrides` (+ `spec.globalPolicyOverrides`) | [PolicySpec](../reference/keystone/keystone-crd.md#policyspec) |
| Federation proxy image | `spec.federation.proxyImage` | `spec.services.keystone.federationProxyImage` | [Attach an OIDC Federation Backend](./oidc-federation.md) |
| Public endpoint / gateway | `spec.bootstrap.publicEndpoint`, `spec.gateway` | `spec.services.keystone.publicEndpoint`, `spec.services.keystone.gateway` | [BootstrapSpec](../reference/keystone/keystone-crd.md#bootstrapspec) |
| Fernet / credential-key schedule | `spec.fernet`, `spec.credentialKeys` | `spec.services.keystone.rotationInterval` (schedule only) | [Day 2 — Rotate Fernet keys](./day-2-operations.md#rotate-fernet-keys-manually) |
| Database TLS/mTLS | `spec.database.tls` | `spec.infrastructure.database.tls` | [Enable Keystone Database TLS/mTLS](./enable-keystone-database-tls.md) |
| Autoscaling (HPA) | `spec.autoscaling` | not exposed — standalone-only | [Autoscaling (HPA)](#autoscaling-hpa) |
| Network policy | `spec.networkPolicy` | not exposed — standalone-only | [Network policy](#network-policy) |
| Free-form INI (`extraConfig`) | `spec.extraConfig` | not exposed — standalone-only | [ExtraConfig](#extraconfig-free-form-ini-sections) |
| Scheduled admin-password rotation | `spec.passwordRotation` | not exposed — standalone-only | [Schedule Admin Password Rotation](./keystone-admin-password-scheduled-rotation.md) |
| uWSGI tuning | `spec.uwsgi` | not exposed — standalone-only | [UWSGISpec](../reference/keystone/keystone-crd.md#uwsgispec) |
| Logging | `spec.logging` | not exposed — standalone-only | [LoggingSpec](../reference/keystone/keystone-crd.md#loggingspec) |
| Trust flush | `spec.trustFlush` | not exposed — standalone-only | [TrustFlushSpec](../reference/keystone/keystone-crd.md#trustflushspec) |
| Middleware | `spec.middleware` | not exposed — standalone-only | [MiddlewareSpec](../reference/keystone/keystone-crd.md#middlewarespec) |
| Plugins | `spec.plugins` | not exposed — standalone-only | [PluginSpec](../reference/keystone/keystone-crd.md#pluginspec) |
| Rollout strategy | `spec.deployment.strategy` | not exposed — standalone-only | [Graceful-termination fields](../reference/keystone/keystone-crd.md#graceful-termination-fields) |
| Graceful termination | `spec.deployment.terminationGracePeriodSeconds`, `spec.deployment.preStopSleepSeconds` | not exposed — standalone-only | [Graceful-termination fields](../reference/keystone/keystone-crd.md#graceful-termination-fields) |
| Topology spread | `spec.deployment.topologySpreadConstraints` | not exposed — standalone-only | [TopologySpreadConstraints](../reference/keystone/keystone-crd.md#topologyspreadconstraints) |
| Priority class | `spec.deployment.priorityClassName` | not exposed — standalone-only | [PriorityClassName](../reference/keystone/keystone-crd.md#priorityclassname) |
| Resource requests/limits | `spec.deployment.resources` | not exposed — standalone-only | [KeystoneSpec](../reference/keystone/keystone-crd.md#keystonespec) |

The "not exposed — standalone-only" knobs are not projectable through the
`ControlPlane` CRD today; set them on a Keystone CR you own, as shown in the
[Standalone Keystone](#standalone-keystone-without-a-controlplane) section.

---

## Standalone Keystone, without a ControlPlane

On the [Quick Start](../quick-start.md) / [Quick Start (Extended)](../quick-start-extended.md)
devstacks a standalone Keystone CR named `keystone` runs with no ControlPlane
projecting it. The recipes below apply to that CR. Several of them —
`spec.autoscaling`, `spec.networkPolicy`, `spec.extraConfig` — are **not exposed
on the `ControlPlane` CRD today**, so a standalone Keystone is the only place they
can be set.

### Brownfield database

The standalone equivalent of the ControlPlane brownfield recipe above — explicit
`host`/`port` and `servers` set directly on the Keystone CR:

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
ControlPlane recipe apply. The webhook enforces that exactly one of `clusterRef`
or `host` is set — never both — for both `database` and `cache`.

### Autoscaling (HPA)

`spec.autoscaling` is not exposed on the `ControlPlane` CRD today, so autoscaling
is standalone-only. Replace hand-patching `spec.deployment.replicas` with a
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
- `minReplicas` defaults to `spec.deployment.replicas` if unset — omitting it will floor the HPA at your current hand-set replica count, not at 1.
- The generated HPA references `deploy/keystone` and uses the Kubernetes standard
  `metrics-server`. The Quick Start kind cluster does **not** ship one — the HPA will
  sit at `unknown/80%` until you install it:

  ```bash
  kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
  ```

  On kind (and most development clusters that use self-signed kubelet certs) patch
  the Deployment to skip TLS verification, otherwise `metrics-server` will fail to
  scrape the kubelet:

  ```bash
  kubectl patch -n kube-system deploy/metrics-server --type=json \
    -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
  ```

Inspect the HPA:

```bash
kubectl get hpa -n openstack -l app.kubernetes.io/instance=keystone
kubectl describe hpa keystone -n openstack
```

Removing `spec.autoscaling` deletes the HPA and returns replica control to
`spec.deployment.replicas`. See [HPA Resource Mapping in the CRD reference](../reference/keystone/keystone-crd.md#hpa-resource-mapping)
for the exact field-to-resource mapping.

### Network policy

`spec.networkPolicy` is not exposed on the `ControlPlane` CRD today, so it is
standalone-only. When set, it creates a Kubernetes `NetworkPolicy` that restricts
ingress to the Keystone API pods. Egress rules for database, cache, and DNS are
derived automatically from the rest of the CR — you only declare the ingress sources.

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
`podSelector`. Both are full Kubernetes `metav1.LabelSelector`s, so you can use
`matchLabels` (as above) or set-based `matchExpressions`.
Within one entry the two selectors AND together; multiple entries OR. Ingress is
always restricted to TCP 5000 — there is no per-entry port configuration. When the
list is non-empty, all other ingress is blocked by default — **including kubelet
probes from other namespaces, which is normally not an issue because probes
originate from the node, but verify in your cluster topology.**

For brownfield or external targets that the auto-derivation cannot see (an off-cluster
MariaDB host, an external IdP), append explicit rules with `spec.networkPolicy.additionalEgress`
— they are added after the auto-derived ones rather than replacing them.

Removing `spec.networkPolicy` deletes the NetworkPolicy and restores unrestricted
traffic. See the [NetworkPolicy reference](../reference/keystone/keystone-crd.md#networkpolicyspec)
for the auto-derived egress rules (Keystone API → MariaDB, Memcached, DNS).

### ExtraConfig — free-form INI sections

`spec.extraConfig` is not exposed on the `ControlPlane` CRD today, so it is
standalone-only. The typed fields on the CR cover the supported configuration
surface. For everything else — logging levels, oslo.messaging tuning, experimental
Keystone flags — `spec.extraConfig` takes a `map[section][key] = value` that is
rendered into the generated `keystone.conf`.

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

The operator does not validate the content of these sections. A typo becomes a silent
no-op at best and a crash loop at worst — test changes in a lab before rolling out.
A change to `extraConfig` triggers a ConfigMap rehash and a rolling Deployment update.

---

## Further reading

- [Keystone CRD API Reference](../reference/keystone/keystone-crd.md) — complete field-by-field reference with validation rules and examples
- [ControlPlane CRD API Reference](../reference/c5c3/controlplane-crd.md) — the `spec.*` fields the ControlPlane exposes, including `spec.infrastructure`
- [Observability & Diagnostics](./observability.md) — how to verify a new configuration took effect
- [Day 2 Operations](./day-2-operations.md) — scale, upgrade, rotate using the configured CR
