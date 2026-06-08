---
title: Deploy a Tenant as a ControlPlane
quadrant: operator
---

# How-to: Deploy a Tenant as a ControlPlane

The `ControlPlane` CR is the tenancy unit in C5C3: one `ControlPlane` projects a
full `MariaDB` + `Memcached` + `Keystone` stack, mints the K-ORC admin
application credential, and registers the identity catalog — all reconciled to
one aggregate `Ready`. A validating webhook enforces **at most one
`ControlPlane` per namespace**, so a tenant maps one-to-one onto a namespace
with its own per-CR-scoped credentials.

This guide onboards a **new** tenant into its own namespace on an already-running
control-plane stack. For the single-tenant happy path from scratch on kind, start
from the [ControlPlane Quick Start](../quick-start-controlplane.md). For confining
the **operator process itself** to a namespace (the complementary RBAC concern),
see [Multi-Tenant Deployment](./multi-tenant-deployment.md).

---

## The one-ControlPlane-per-namespace contract

The ControlPlane validating webhook rejects a **second** `ControlPlane` `CREATE`
in a namespace that already holds one. The rejection is a `Forbidden` error on
`metadata.namespace` that names the incumbent:

```
admission webhook denied the request: ControlPlane.c5c3.io "cp-b" is invalid:
metadata.namespace: Forbidden: only one ControlPlane is permitted per namespace;
"cp-a" already exists in namespace "tenant-a"
```

- **`CREATE` only.** An existing `ControlPlane` stays fully mutable — `UPDATE` is
  never blocked by this rule.
- **Why.** Every per-tenant credential lives at an OpenBao path scoped by
  `namespace`+`name`, and the credential-rotation reconciler resolves its target
  by **listing `ControlPlane`s in the namespace and expecting exactly one**.
  Permitting two would make both the credential paths and the rotation target
  ambiguous. Enforcing uniqueness at admission keeps that resolution
  deterministic.

The operational model that falls out of this: **one tenant = one namespace = one
`ControlPlane`**. To run several tenants, give each its own namespace.

---

## Per-CR credential scoping — how tenants stay isolated

Every credential a tenant owns is stored at an OpenBao path that embeds the
tenant's namespace and CR name, so two tenants on the **same** shared OpenBao
backend never collide:

| Secret | OpenBao path (KV-v2, store-relative) | Keyed on |
|--------|--------------------------------------|----------|
| Admin password (bootstrap source) | `bootstrap/<ns>/<cp>-keystone/admin` | projected Keystone CR name |
| Fernet keys | `openstack/keystone/<ns>/<cp>-keystone/fernet-keys` | projected Keystone CR name |
| Credential keys | `openstack/keystone/<ns>/<cp>-keystone/credential-keys` | projected Keystone CR name |
| K-ORC admin application credential | `openstack/keystone/<ns>/<cp>/admin/app-credential` | ControlPlane name |

`<ns>` is the tenant namespace, `<cp>` the `ControlPlane` name, and
`<cp>-keystone` the Keystone CR the operator projects from it. Because the
webhook guarantees one `ControlPlane` per namespace, these prefixes are disjoint
across tenants — `tenant-a` and `tenant-b` each get their own subtree and can be
rotated, audited, or revoked independently.

> **Current limitation — per-tenant ExternalSecret wiring is manual.** The
> Kubernetes `ExternalSecret`s that materialise `keystone-admin` and
> `k-orc-clouds-yaml` from those paths are shipped pinned to the **default**
> identity (`ControlPlane "controlplane"` in namespace `openstack`). Onboarding a
> non-default tenant therefore requires creating per-tenant copies that point at
> the tenant's own paths (step 3 below). Operator-owned per-CR templating that
> renders one `ExternalSecret` set per `ControlPlane` automatically is planned;
> until it lands, treat step 3 as a required manual step.

---

## Prerequisites

1. **A running control-plane stack** — the keystone-operator, K-ORC, and
   c5c3-operator installed, plus the shared infrastructure (OpenBao, the
   `openbao-cluster-store` `ClusterSecretStore`, External Secrets, MariaDB and
   Memcached operators). `WITH_CONTROLPLANE=true make deploy-infra` brings all of
   this up; see the [ControlPlane Quick Start](../quick-start-controlplane.md).
2. **Write access to OpenBao** to seed the tenant's paths (a `BAO_TOKEN` with
   write on the `kv-v2` mount).
3. **`openstack` CLI and `jq`** for verification.

This guide uses a worked example: tenant **`tenant-a`**, `ControlPlane`
**`cp-a`**, whose projected Keystone is therefore **`cp-a-keystone`**.

---

## 1. Create the tenant namespace

```bash
kubectl create namespace tenant-a
```

---

## 2. Seed the tenant's OpenBao paths

On a fresh tenant the admin-password and K-ORC bootstrap `clouds.yaml` paths are
empty. Until they exist, K-ORC cannot authenticate to mint the admin application
credential, the `k-orc-clouds-yaml` `ExternalSecret` never goes `Ready`, and
`AdminCredentialReady` deadlocks on `WaitingForCloudsYaml`. Seed them with the
bootstrap script, scoped to the new identity:

```bash
export BAO_TOKEN=$(kubectl get secret openbao-init-keys -n openbao-system \
  -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token')

KORC_CONTROLPLANES="tenant-a/cp-a" \
  deploy/openbao/bootstrap/write-bootstrap-secrets.sh

unset BAO_TOKEN
```

`KORC_CONTROLPLANES` takes whitespace-separated `<namespace>/<controlplane>`
identities. For each it seeds, idempotently (existing paths are skipped):

- `bootstrap/tenant-a/cp-a-keystone/admin` — a generated admin password
  (created in-pod; cleartext never reaches a host process argument list).
- `openstack/keystone/tenant-a/cp-a/admin/app-credential` — a password-based
  bootstrap `clouds.yaml` derived from that admin password, with
  `endpoint_type: internal` and `auth_url:
  http://cp-a-keystone.tenant-a.svc:5000/v3` (the in-cluster Keystone Service the
  operator will project).

Both paths are then stamped `managed-by=external-secrets` so the operator's
PushSecrets are allowed to overwrite them later (External Secrets refuses to push
to a pre-seeded path that lacks that marker).

> **Leave `KORC_KEYSTONE_AUTH_URL` unset.** It overrides the auth URL for *every*
> identity, so it is only correct for a single tenant. The script derives the
> right per-identity `auth_url` automatically from `<cp>-keystone`.

---

## 3. Wire per-tenant ExternalSecrets

Create the two `ExternalSecret`s that pull the tenant's seeded paths into the
`tenant-a` namespace. The operator projects the K-ORC `ApplicationCredential` /
`Service` / `Endpoint` CRs into the `ControlPlane`'s **own** namespace and K-ORC
resolves their credentials there, so both materialised Secrets must live in
`tenant-a`:

```yaml
# tenant-a-externalsecrets.yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: keystone-admin
  namespace: tenant-a
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: openbao-cluster-store      # cluster-scoped, reachable from any namespace
    kind: ClusterSecretStore
  target:
    name: keystone-admin
    creationPolicy: Owner
  data:
    - secretKey: password
      remoteRef:
        key: bootstrap/tenant-a/cp-a-keystone/admin
        property: password
---
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: k-orc-clouds-yaml
  namespace: tenant-a
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: openbao-cluster-store
    kind: ClusterSecretStore
  target:
    name: k-orc-clouds-yaml
    creationPolicy: Owner
  data:
    - secretKey: clouds.yaml
      remoteRef:
        key: openstack/keystone/tenant-a/cp-a/admin/app-credential
        property: clouds.yaml
```

```bash
kubectl apply -f tenant-a-externalsecrets.yaml
kubectl -n tenant-a get secret keystone-admin k-orc-clouds-yaml
```

Both Secrets should appear within a refresh cycle. (K-ORC's `orc-system` global
`clouds.yaml` mount is not on the per-tenant credential critical path, so a copy
there is optional.)

---

## 4. Apply the ControlPlane CR

```yaml
# cp-a.yaml
apiVersion: c5c3.io/v1alpha1
kind: ControlPlane
metadata:
  name: cp-a
  namespace: tenant-a
spec:
  openStackRelease: "2025.2"
  region: RegionOne
  infrastructure:
    database:
      clusterRef:
        name: openstack-db
      database: keystone
      secretRef:
        name: keystone-db
    cache:
      clusterRef:
        name: openstack-memcached
      backend: dogpile.cache.pymemcache
  services:
    keystone:
      replicas: 1
  korc:
    adminCredential:
      cloudCredentialsRef:
        cloudName: admin               # entry in the seeded k-orc-clouds-yaml Secret
      passwordSecretRef:
        name: keystone-admin           # resolved in tenant-a (from step 3)
        key: password
      applicationCredential:
        rotation:
          mode: PasswordDriven
```

```bash
kubectl apply -f cp-a.yaml
```

> Database and cache `clusterRef`s use **managed mode**: the operator provisions a
> `MariaDB`/`Memcached` per tenant for these references. To share infrastructure or
> point at external hosts, see the
> [ControlPlane CRD API Reference](../reference/c5c3/controlplane-crd.md#infrastructurespec).
>
> To expose this tenant's Keystone externally, add `spec.services.keystone.gateway`
> (and `publicEndpoint`) — see [Expose Keystone via the Gateway](./expose-keystone-via-gateway.md).
> Each tenant needs a **distinct** hostname, since a Gateway listener matches one.

---

## 5. Verify

Watch the aggregate `Ready` resolve through its sub-conditions in dependency
order:

```bash
kubectl get controlplane cp-a -n tenant-a \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
```

```
InfrastructureReady → KeystoneReady → KORCReady → AdminCredentialReady → CatalogReady
```

If `AdminCredentialReady` lingers on `WaitingForCloudsYaml`, force the tenant's
`k-orc-clouds-yaml` `ExternalSecret` to sync (it otherwise refreshes only on its
hourly interval):

```bash
kubectl annotate externalsecret/k-orc-clouds-yaml -n tenant-a \
  force-sync="$(date +%s)" --overwrite
```

Then wait for the aggregate condition and confirm the minted credential:

```bash
kubectl wait controlplane/cp-a -n tenant-a --for=condition=Ready --timeout=15m

# The minted admin application credential and its per-CR OpenBao mirror path:
kubectl get controlplane cp-a -n tenant-a \
  -o jsonpath='{.status.adminApplicationCredential}{"\n"}'
```

`status.adminApplicationCredential` reporting the credential ID — and
`CatalogReady=True` — confirms K-ORC authenticated as admin, minted the
restricted application credential, mirrored it to
`openstack/keystone/tenant-a/cp-a/admin/app-credential`, and registered the
identity catalog. The tenant is live and fully isolated from any other
namespace's `ControlPlane`.

---

## Composition with namespace-scoped operator RBAC

This guide is about the **tenancy aggregate** — the `ControlPlane` and its per-CR
credentials. It is orthogonal to *who* reconciles it. To additionally confine the
keystone-operator's RBAC to a single tenant namespace (one operator instance per
tenant, `Role`/`RoleBinding` instead of cluster-wide), combine this flow with
[Multi-Tenant Deployment](./multi-tenant-deployment.md). The two compose: a
namespace-scoped operator reconciles the one `ControlPlane`'s projected Keystone in
its namespace, and the per-CR credential paths keep the OpenBao backend shared but
non-overlapping.

---

## Decommissioning a tenant

```bash
# 1. Delete the ControlPlane — owner references garbage-collect the projected
#    MariaDB / Memcached / Keystone / K-ORC CRs in the same namespace.
kubectl delete controlplane cp-a -n tenant-a

# 2. Delete the per-tenant ExternalSecrets and the namespace.
kubectl delete -f tenant-a-externalsecrets.yaml
kubectl delete namespace tenant-a
```

OpenBao state is **external** and is not garbage-collected with the CR. To fully
reclaim the tenant, also remove its seeded paths (this destroys the admin
password and the mirrored application credential — do it only when the tenant is
truly gone):

```bash
export BAO_TOKEN=$(kubectl get secret openbao-init-keys -n openbao-system \
  -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token')

for p in \
  "kv-v2/bootstrap/tenant-a/cp-a-keystone/admin" \
  "kv-v2/openstack/keystone/tenant-a/cp-a-keystone/fernet-keys" \
  "kv-v2/openstack/keystone/tenant-a/cp-a-keystone/credential-keys" \
  "kv-v2/openstack/keystone/tenant-a/cp-a/admin/app-credential"; do
  kubectl exec -n openbao-system openbao-0 -- \
    env BAO_TOKEN="${BAO_TOKEN}" sh -c "bao kv metadata delete \"${p}\""
done

unset BAO_TOKEN
```

Because the paths are scoped per `<ns>/<cp>`, deleting one tenant's subtree never
touches another's.

---

## See also

- [ControlPlane Quick Start](../quick-start-controlplane.md) — the single-tenant
  end-to-end path on kind.
- [Multi-Tenant Deployment](./multi-tenant-deployment.md) — running the operator
  process itself namespace-scoped (complementary RBAC confinement).
- [ControlPlane CRD API Reference](../reference/c5c3/controlplane-crd.md) — every
  `spec.*` field, the webhooks, and the per-ControlPlane OpenBao path note.
- [ControlPlane Reconciler](../reference/c5c3/controlplane-reconciler.md) — the
  sub-reconciler ordering, the K-ORC admin credential chain, and the multi-instance
  model.
- [Expose Keystone via the Gateway](./expose-keystone-via-gateway.md) — publishing a
  tenant's Keystone externally.
