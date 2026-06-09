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
connection (K-ORC is cloned from GitHub at a pinned tag). Docker Desktop needs
ample CPU/memory — the ControlPlane provisions a production-shaped (Galera)
MariaDB, heavier than the single-replica database the per-service path uses.

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
(managed Memcached), `keystone-db` (DB credentials Secret), `keystone-admin` /
`password` (admin password Secret and key), `k-orc-clouds-yaml` with cloud entry
`admin` (K-ORC clouds.yaml) — which match the Secrets and clusters the
infrastructure layer (Step 2) seeds. The c5c3-operator seeds the K-ORC bootstrap
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
        name: keystone-db         # DB credentials, seeded by infra via ESO
    cache:
      clusterRef:
        name: openstack-memcached
      backend: dogpile.cache.pymemcache
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
        name: keystone-admin         # admin password, seeded by infra via ESO
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
export OS_PASSWORD=$(kubectl get secret keystone-admin -n openstack -o jsonpath='{.data.password}' | base64 -d)
export OS_PROJECT_NAME=admin
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
openstack --insecure token issue
```

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
