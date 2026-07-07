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
c5c3-operator provision the `MariaDB`, `Memcached`, `Keystone`, and `Horizon`
children, mint the admin application credential through
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
ControlPlane operator stack (keystone-operator, horizon-operator, K-ORC,
c5c3-operator) from the published charts — but **not** the `ControlPlane` CR
itself; you create and apply that in Step 3. In this mode the ControlPlane provisions its own MariaDB/Memcached
(managed mode), so deploy-infra does not create the shared ones. `KIND_HOST_PORT=8443`
maps the Gateway to a non-privileged host port for macOS; on Linux with rootful
Docker drop the override and use port `443`. Expect **5–10 minutes**.

::: tip Fresh operator images after a merge
The operator images are published under the mutable `:latest` tag, so
deploy-infra pins them to the digest current at deploy time (per-operator
image-digest ConfigMaps consumed by the HelmReleases via `valuesFrom`). After
a feature merges to `main`, run `make refresh-operator-digests` against the
running cluster: it re-resolves the digests, updates the ConfigMaps, and
requests a Flux reconcile so the operators roll to the freshly built images —
no redeploy needed.
:::

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
    horizon:
      replicas: 1
      # Exposed through the same shared Envoy Gateway as Keystone, via the
      # second HTTPS listener the kind overlay adds for horizon.127-0-0-1.nip.io.
      gateway:
        parentRef:
          name: openstack-gw
        hostname: horizon.127-0-0-1.nip.io
```

```bash
kubectl apply -f controlplane.yaml
```

The `horizon` block makes the reconciler project the OpenStack Dashboard once
its Keystone child is Ready. Everything else is derived: the image tag from
`spec.openStackRelease`, the Memcached wiring from `spec.infrastructure.cache`,
and the Keystone endpoint from the Keystone child's naming convention. The
Django `SECRET_KEY` defaults to the kind-only `horizon-secret-key` Secret
(seeded per the default ControlPlane identity); a second ControlPlane must set
`services.horizon.secretKeyRef` to its own Secret. A `HorizonReady` condition
joins the chain (after `KeystoneReady`) and `status.services` gains a second
entry.

Applying the CR is not the end of the manual work: a hand-applied ControlPlane
needs the one-time OpenBao onboarding in Step 4 before the chain can progress
past its database credentials.

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
    horizon:
      replicas: 1
      gateway:
        parentRef:
          name: openstack-gw          # same Gateway as Keystone; second listener
        hostname: horizon.127-0-0-1.nip.io
      secretKeyRef:
        name: horizon-secret-key       # default-identity kind shim Secret
        key: secret-key
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

## Step 4 — Onboard the OpenBao database-engine tenant

In managed mode the ControlPlane defaults to engine-issued (**Dynamic**) Keystone
DB credentials: ESO draws short-lived MySQL users from the OpenBao
database engine at `database/mariadb/creds/keystone-<namespace>`. The
c5c3-operator only **reads** from that path — the engine connection and the
per-tenant role are provisioned out-of-band, once per ControlPlane, by
`deploy/openbao/bootstrap/setup-database-tenant.sh`.

Run it after the `kubectl apply` from Step 3, as soon as the projected MariaDB
is Ready (the script configures the engine's database connection, so it needs a
reachable database):

```bash
kubectl wait mariadb/openstack-db -n openstack --for=condition=Ready --timeout=10m

export BAO_TOKEN=$(kubectl get secret openbao-init-keys -n openbao-system \
  -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token')
deploy/openbao/bootstrap/setup-database-tenant.sh openstack controlplane
unset BAO_TOKEN
```

The two arguments are the ControlPlane **namespace** and **name** (`openstack
controlplane` here; adjust the second one if you renamed the CR via
`CONTROLPLANE_NAME` in Step 2). `BAO_TOKEN` is read from the `openbao-init-keys`
Secret where deploy-infra stores the root token — kind-only plumbing; against a
production OpenBao use a token with write access to `database/mariadb/*`. The
script is idempotent: re-running it refreshes the connection and role in place.

Skip this step only when:

- Step 2 ran with `WITH_CONTROLPLANE_CR=true` — deploy-infra then onboards the
  bundled ControlPlane automatically, or
- the ControlPlane opts out of Dynamic credentials with
  `spec.infrastructure.database.credentialsMode: Static` (see
  [Migrate Keystone DB to Dynamic Credentials](/guides/migrate-keystone-db-to-dynamic-credentials)).

::: warning If you skip it
The reconcile chain stalls before any Keystone or Horizon child is created: the
ControlPlane reports `DBCredentialsReady=False` (reason
`WaitingForDBCredentialSecret`), the `controlplane-keystone-db-credentials`
ExternalSecret sits in `SecretSyncedError`, and the external-secrets controller
logs `unknown role: keystone-<namespace>`. Nothing is lost — run the onboarding
script and ESO syncs the credential on its next retry.
:::

## Step 5 — Watch the chain reconcile

The aggregate `Ready` flips to `True` once all seven sub-conditions are met, in
dependency order (`HorizonReady` gates on `KeystoneReady`; the K-ORC branch runs
alongside it):

```
InfrastructureReady → DBCredentialsReady → KeystoneReady → HorizonReady → KORCReady → AdminCredentialReady → CatalogReady
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

## Step 6 — Verify

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

### Open the Horizon dashboard

The dashboard is exposed through the same shared Envoy Gateway as Keystone, on
its own `horizon.127-0-0-1.nip.io` listener:

```bash
open https://horizon.127-0-0-1.nip.io:8443/
```

Your browser will warn that the certificate is not trusted — expected for a kind
cluster (the listener terminates with a self-signed certificate). Log in with
`admin` / the password from the `controlplane-keystone-admin-credentials` Secret
above (domain `Default`).

After login the dashboard redirects to `/project/`, which reports
"Unauthorized" — the default landing page needs Compute/Network services this
control plane does not serve yet. Open the Identity panel instead:

```bash
open https://horizon.127-0-0-1.nip.io:8443/identity/
```

> With the default `KIND_HOST_PORT=443` drop the `:8443` and open
> `https://horizon.127-0-0-1.nip.io/`.

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
