<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Advanced Configuration

Beyond the minimal `Keystone` CR from the [Quick Start (Extended)](../quick-start-extended.md), the operator
supports a number of configuration options for real cluster deployments. This guide
walks through the four that cover the most common real-world needs and points to the
reference for the rest.

**Prerequisites:** A running Keystone CR from the [Quick Start (Extended)](../quick-start-extended.md).
Each pattern below is an independent recipe — apply only what you need.

---

## Brownfield database

The Quick Start uses the "managed mode" where the operator creates the `MariaDB`
Database, User, and Grant CRs for you (`spec.database.clusterRef`). If you already run
MariaDB/Galera outside the operator's reach — managed by another team, hosted
externally, or on a different operator — use **brownfield mode** with an explicit host.

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: Keystone
metadata:
  name: keystone
  namespace: openstack
spec:
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

::: warning In brownfield mode you own schema setup
The operator does **not** create the database, user, or grants. You must provision
them before the CR reconciles:

```sql
CREATE DATABASE keystone DEFAULT CHARACTER SET utf8 COLLATE utf8_general_ci;
CREATE USER 'keystone'@'%' IDENTIFIED BY '<password-from-secretRef>';
GRANT ALL PRIVILEGES ON keystone.* TO 'keystone'@'%';
FLUSH PRIVILEGES;
```

The secret referenced by `secretRef` must contain a `password` key matching the SQL
user. Once those exist, `db_sync` will create the Keystone schema on first reconcile.
:::

The webhook enforces that exactly one of `clusterRef` or `host` is set — never both —
for both `database` and `cache`.

---

## Autoscaling (HPA)

Replace hand-patching `spec.replicas` with a `HorizontalPodAutoscaler` managed by the
operator. When `spec.autoscaling` is present, the HPA owns the Deployment's replica
count.

```yaml
spec:
  replicas: 3       # seeds the Deployment; HPA owns the Deployment replica count once created
  autoscaling:
    minReplicas: 2
    maxReplicas: 10
    targetCPUUtilization: 80
    targetMemoryUtilization: 70
```

- At least one of `targetCPUUtilization` or `targetMemoryUtilization` is required.
- `minReplicas` defaults to `spec.replicas` if unset — omitting it will floor the HPA at your current hand-set replica count, not at 1.
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
`spec.replicas`. See [HPA Resource Mapping in the CRD reference](../reference/keystone/keystone-crd.md#hpa-resource-mapping)
for the exact field-to-resource mapping.

---

## Network policy

When set, `spec.networkPolicy` creates a Kubernetes `NetworkPolicy` that restricts
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

Each list entry supports `namespaceSelector` and/or `podSelector` (label-selector
semantics) and an optional `ports` list. When the list is non-empty, all other ingress
is blocked by default — **including kubelet probes from other namespaces, which is
normally not an issue because probes originate from the node, but verify in your
cluster topology.**

Removing `spec.networkPolicy` deletes the NetworkPolicy and restores unrestricted
traffic. See the [NetworkPolicy reference](../reference/keystone/keystone-crd.md#networkpolicyspec)
for the auto-derived egress rules (Keystone API → MariaDB, Memcached, DNS).

---

## ExtraConfig — free-form INI sections

The typed fields on the CR cover the supported configuration surface. For everything
else — logging levels, oslo.messaging tuning, experimental Keystone flags —
`spec.extraConfig` takes a `map[section][key] = value` that is rendered into the
generated `keystone.conf`.

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

## Feature pointer table

Everything else the CR supports — the flags you did not see above. One-line hints plus
a link to the full reference.

| Feature | Field | What it does | Reference |
|---------|-------|--------------|-----------|
| Credential-key rotation | `spec.credentialKeys` | Separate rotation schedule for the credential encryption key | [CredentialKeysSpec](../reference/keystone/keystone-crd.md#credentialkeysspec) |
| Trust flush | `spec.trustFlush` | CronJob running `keystone-manage trust_flush` on a schedule. Default-on (hourly) — to pause without deleting the CronJob, set `spec.trustFlush.suspend: true` rather than removing the field | [TrustFlushSpec](../reference/keystone/keystone-crd.md#trustflushspec) |
| uWSGI tuning | `spec.uwsgi` | Worker processes, threads, HTTP keep-alive | [UWSGISpec](../reference/keystone/keystone-crd.md#uwsgispec) |
| Topology spread | `spec.topologySpreadConstraints` | Pod spread across zones/hostnames | [TopologySpreadConstraints](../reference/keystone/keystone-crd.md#topologyspreadconstraints) |
| Priority class | `spec.priorityClassName` | Scheduling priority and preemption class | [PriorityClassName](../reference/keystone/keystone-crd.md#priorityclassname) |
| Policy overrides | `spec.policyOverrides` | Custom `oslo.policy` rules (inline or ConfigMap) | [PolicySpec](../reference/keystone/keystone-crd.md#policyspec) |
| Middleware | `spec.middleware` | Custom WSGI filters in the `api-paste.ini` pipeline | [MiddlewareSpec](../reference/keystone/keystone-crd.md#middlewarespec) |
| Plugins | `spec.plugins` | Service-side Keystone plugins/drivers | [PluginSpec](../reference/keystone/keystone-crd.md#pluginspec) |
| Federation | `spec.federation` | Enables Keystone federation (SAML/OIDC, Shibboleth) | [FederationSpec](../reference/keystone/keystone-crd.md#federationspec) |
| Resource requests/limits | `spec.resources` | CPU/memory requests and limits on API pods | [KeystoneSpec](../reference/keystone/keystone-crd.md#keystonespec) |
| Public endpoint | `spec.bootstrap.publicEndpoint` | External URL written to the Keystone service catalogue | [BootstrapSpec](../reference/keystone/keystone-crd.md#bootstrapspec) |

---

## Further reading

- [Keystone CRD API Reference](../reference/keystone/keystone-crd.md) — complete field-by-field reference with validation rules and examples
- [Observability & Diagnostics](./observability.md) — how to verify a new configuration took effect
- [Day 2 Operations](./day-2-operations.md) — scale, upgrade, rotate using the configured CR
