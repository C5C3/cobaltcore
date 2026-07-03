---
title: Quick Start (ControlPlane)
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Quick Start (ControlPlane): C5C3 + K-ORC on Kind

The shortest path from `git clone` to an authenticated Keystone API call driven
by a single **c5c3 ControlPlane** CR. Where the [Quick Start](./quick-start.md)
stops at a hand-applied `Keystone` CR, here one `ControlPlane` CR makes the
c5c3-operator provision the `MariaDB`, `Memcached`, and `Keystone` children, mint
the admin application credential through
[K-ORC](https://github.com/k-orc/openstack-resource-controller), mirror it to
OpenBao, and register the identity catalog — all reconciled to an aggregate
`Ready`.

## Prerequisites

Same toolchain as the [Quick Start](./quick-start.md), plus an internet
connection (K-ORC is cloned from GitHub at a pinned tag).

The bundled kind `ControlPlane` CR pins its backing services to a single instance
(`spec.infrastructure.database.replicas: 1`, `cache.replicas: 1`) so the
fresh-create chain fits a single-node kind cluster: `database.replicas: 1` yields
a single-instance, non-Galera MariaDB (the operator derives Galera from
`replicas > 1`) and `cache.replicas: 1` a single Memcached pod. The CRD default for
both is `3` — a 3-node Galera cluster plus three Memcached pods, matching the
production baseline — which OOM-kills a laptop-sized kind. To provision the
production-shaped topology on a bigger box, set `CONTROLPLANE_DB_REPLICAS=3` and/or
`CONTROLPLANE_CACHE_REPLICAS=N` for Step 2 (`2` is rejected for the database —
Galera needs a quorum). `database.replicas` is immutable after the CR is created,
so change it on a fresh environment (`make teardown-infra` first).

The bundled CR also pins the MariaDB volume to a test size
(`spec.infrastructure.database.storageSize: 512Mi`). The CRD default is `100Gi` —
the production volume size — which a kind/CI run never fills, so the managed MariaDB
requests a small volume instead. To mirror the production volume on a bigger box,
set `CONTROLPLANE_DB_STORAGE=100Gi` for Step 2 (any Kubernetes quantity in
`Mi`/`Gi`/`Ti` is accepted). Like `database.replicas`, `database.storageSize` is
immutable after the CR is created — the MariaDB operator refuses to resize a live
volume — so change it on a fresh environment (`make teardown-infra` first).

```bash
make install-test-deps
export PATH="${HOME}/.local/bin:${PATH}"
```

## Step 1 — Clone

```bash
git clone https://github.com/c5c3/forge.git
cd forge
```

## Step 2 — Cluster + ControlPlane stack

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

`WITH_CONTROLPLANE=true` brings up the shared infrastructure and then the
ControlPlane operator stack (keystone-operator, K-ORC, c5c3-operator) from the
published charts — but **not** the `ControlPlane` CR itself; you create and apply
that in Step 3. In this mode the ControlPlane provisions its own MariaDB/Memcached
(managed mode), so deploy-infra does not create the shared ones. `KIND_HOST_PORT=8443`
maps the Gateway to a non-privileged host port for macOS; on Linux with rootful
Docker drop the override and use port `443`. Expect **5–10 minutes**.

## Step 3 — Create the ControlPlane CR

Apply a `ControlPlane` CR — the c5c3-operator reconciles it into the whole stack.
You only supply `openStackRelease` and the `services.keystone` block: the
defaulting webhook fills the infrastructure and admin-credential references with
their well-known names — `openstack-db` (managed MariaDB), `openstack-memcached`
(managed Memcached), `keystone-db` (DB-credential placeholder — in managed mode
the operator projects a per-ControlPlane `{name}-keystone-db-credentials`
Secret and points the Keystone CR at it instead), `keystone-admin` / `password`
(admin-password placeholder — in managed mode the operator projects a per-ControlPlane
`{name}-keystone-admin-credentials` Secret and points the Keystone CR at it instead),
`k-orc-clouds-yaml` with cloud entry `admin`
(K-ORC clouds.yaml) — which match the Secrets and clusters the infrastructure
layer (Step 2) seeds. The c5c3-operator seeds the K-ORC bootstrap
`clouds.yaml` per-CR, deriving the in-cluster Keystone auth URL from the CR's own
name, so the CR name is no longer pinned by a pre-seeded `clouds.yaml`; to use a
different name, pass `CONTROLPLANE_NAME=foo` to Step 2 — it renames the bundled CR
and seeds the matching admin password. The defaulting only fills the
names/references; the operator still **consumes** the pre-seeded Secret *content*
(DB credentials, admin password) and materialises the bootstrap `clouds.yaml`
itself, so it does **not invent** credentials.

```yaml
# controlplane.yaml
apiVersion: c5c3.io/v1alpha1
kind: ControlPlane
metadata:
  name: controlplane
  namespace: openstack
spec:
  openStackRelease: "2025.2"
  # Single-node backing services for kind. Omit these and both default to 3 (a
  # 3-node Galera MariaDB plus three Memcached pods), which OOM-kills a small kind.
  infrastructure:
    database:
      replicas: 1         # single-instance, non-Galera MariaDB (Galera = replicas > 1)
      storageSize: 512Mi  # test-sized volume; omit to default to 100Gi (production)
    cache:
      replicas: 1     # single Memcached pod
  services:
    keystone:
      replicas: 1
      # Drop publicEndpoint on the default port 443 — the operator then derives
      # https://keystone.127-0-0-1.nip.io/v3 from the gateway hostname.
      publicEndpoint: https://keystone.127-0-0-1.nip.io:8443/v3
      gateway:
        parentRef:
          name: openstack-gw
        hostname: keystone.127-0-0-1.nip.io
        path: /
```

```bash
kubectl apply -f controlplane.yaml
```

::: tip Dynamic DB credentials (#439)
A managed-mode ControlPlane defaults to engine-issued (Dynamic) Keystone DB
credentials from the OpenBao database engine, so its per-tenant engine role must
be onboarded once its MariaDB is Ready:

```bash
BAO_TOKEN=... \
  deploy/openbao/bootstrap/setup-database-tenant.sh openstack controlplane
```

`make deploy-infra` with `WITH_CONTROLPLANE_CR=true` runs this for the bundled
ControlPlane automatically; a ControlPlane you apply by hand needs the one-liner
above (or set `spec.infrastructure.database.credentialsMode: Static` to stay on a
static credential). See [Migrate Keystone DB to Dynamic Credentials](/guides/migrate-keystone-db-to-dynamic-credentials).
:::

<details>
<summary>Equivalent fully-expanded form (what the webhook defaults to)</summary>

```yaml
# controlplane.yaml
apiVersion: c5c3.io/v1alpha1
kind: ControlPlane
metadata:
  name: controlplane
  namespace: openstack
spec:
  openStackRelease: "2025.2"
  region: RegionOne
  infrastructure:
    database:
      clusterRef:
        name: openstack-db        # MariaDB the operator provisions (managed mode)
      database: keystone
      secretRef:
        name: keystone-db         # placeholder default — the operator replaces it
                                  # with {name}-keystone-db-credentials (managed mode)
      replicas: 1                 # single-instance, non-Galera; omit to default to 3 (Galera)
      storageSize: 512Mi          # test-sized volume; omit to default to 100Gi (production)
    cache:
      clusterRef:
        name: openstack-memcached
      backend: dogpile.cache.pymemcache
      replicas: 1                 # single Memcached pod; omit to default to 3
  services:
    keystone:
      replicas: 1
      publicEndpoint: https://keystone.127-0-0-1.nip.io:8443/v3
      gateway:
        parentRef:
          name: openstack-gw
        hostname: keystone.127-0-0-1.nip.io
        path: /
  korc:
    adminCredential:
      cloudCredentialsRef:
        cloudName: admin             # entry in the operator-materialised k-orc-clouds-yaml Secret
        secretName: k-orc-clouds-yaml
      passwordSecretRef:
        name: keystone-admin         # spec-level/brownfield default — in managed mode the
                                     # operator projects {name}-keystone-admin-credentials
                                     # and points the Keystone child at it instead
        key: password
      applicationCredential:
        rotation:
          mode: PasswordDriven
```

</details>

## Step 4 — Watch the chain reconcile

The aggregate `Ready` flips to `True` once all five sub-conditions are met, in
dependency order:

```
InfrastructureReady → KeystoneReady → KORCReady → AdminCredentialReady → CatalogReady
```

```bash
kubectl get controlplane controlplane -n openstack \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
```

Wait for the aggregate condition:

```bash
kubectl wait controlplane/controlplane -n openstack \
  --for=condition=Ready --timeout=15m
```

## Step 5 — Verify

The ControlPlane exposes the projected Keystone through the shared Envoy Gateway
at `https://keystone.127-0-0-1.nip.io:8443/v3` — the same path as the per-service
[Quick Start](./quick-start.md), no port-forward.

```bash
curl -k https://keystone.127-0-0-1.nip.io:8443/v3
```

Then issue a token with the admin password:

```bash
export OS_AUTH_URL=https://keystone.127-0-0-1.nip.io:8443/v3
export OS_USERNAME=admin
export OS_PASSWORD=$(kubectl get secret controlplane-keystone-admin-credentials -n openstack -o jsonpath='{.data.password}' | base64 -d)
export OS_PROJECT_NAME=admin
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
openstack --insecure token issue
```

> The admin password is read from the operator-owned per-ControlPlane Secret
> `controlplane-keystone-admin-credentials` (named `{ControlPlane name}-keystone-admin-credentials`).
> In managed mode the c5c3-operator always projects this Secret, so the command holds
> for any identity — if you set `CONTROLPLANE_NAME=foo` in Step 2, read
> `foo-keystone-admin-credentials` instead.

> With the default `KIND_HOST_PORT=443` use `https://keystone.127-0-0-1.nip.io/v3`
> and drop the `publicEndpoint` line from the CR in Step 3.

## Teardown

```bash
make teardown-infra
```

## Related references

- [ControlPlane CRD API Reference](./reference/c5c3/controlplane-crd.md) — every
  `spec.*` field, the webhooks, and the status conditions.
- [ControlPlane Reconciler](./reference/c5c3/controlplane-reconciler.md) — the
  sub-reconciler ordering and gating semantics.
- [Quick Start](./quick-start.md) — the compact per-service Keystone path.
