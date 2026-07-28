---
title: Quick Start (ControlPlane)
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Quick Start (ControlPlane): C5C3 + K-ORC on Kind

This guide takes a single **c5c3 ControlPlane** CR from `git clone` to an authenticated
Keystone API call. Compared with the [Quick Start](./quick-start.md), the
c5c3-operator now provisions the `MariaDB`, `Memcached`, `Keystone`, `Horizon`,
`Glance`, and `Placement` children, mints the admin application credential through
[K-ORC](https://github.com/k-orc/openstack-resource-controller), mirrors it to
OpenBao, and registers the identity catalog.

## Prerequisites

Same toolchain as the [Quick Start](./quick-start.md), plus:

- `make` on `PATH` for `install-test-deps`, `deploy-infra`, and `teardown-infra`
- The OpenStack CLI ([`python-openstackclient`](https://docs.openstack.org/python-openstackclient/latest/)) on `PATH` for the auth check in Step 6, plus the [`osc-placement`](https://docs.openstack.org/osc-placement/latest/) plugin for the placement check in the same step
- A stable internet connection while `make deploy-infra` clones K-ORC from GitHub
- Roughly 8 GB RAM, 2 CPU cores, and 10 GB of free disk for a laptop-sized kind cluster
- `yq` v4.x on `PATH` for the `KIND_HOST_PORT=8443` override path in Step 2

Docker Desktop and Podman are both valid kind providers. When using Podman,
ensure its machine is already running and select it explicitly before running
Step 2:

```bash
export KIND_EXPERIMENTAL_PROVIDER=podman
```

- The bundled kind `ControlPlane` CR pins its backing services to a single
  instance (`spec.infrastructure.database.replicas: 1`, `cache.replicas: 1`) so
  the fresh-create chain fits a single-node kind cluster.
- `database.replicas: 1` yields a single-instance, non-Galera MariaDB and
  `cache.replicas: 1` a single Memcached pod. The CRD default for both is `3`,
  which matches the production baseline but OOM-kills a laptop-sized kind.
- On a bigger box, set `CONTROLPLANE_DB_REPLICAS=3` and/or
  `CONTROLPLANE_CACHE_REPLICAS=N` for Step 2. `2` is rejected for the database —
  Galera needs a quorum.
- `database.replicas` is immutable after the CR is created, so change it on a
  fresh environment (`make teardown-infra` first).

- The bundled CR also pins the MariaDB volume to a test size
  (`spec.infrastructure.database.storageSize: 512Mi`).
- The CRD default is `100Gi`, which a kind/CI run never fills, so the managed
  MariaDB requests a small volume instead.
- To mirror the production volume on a bigger box, set
  `CONTROLPLANE_DB_STORAGE=100Gi` for Step 2. Any Kubernetes quantity in
  `Mi`/`Gi`/`Ti` is accepted.

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
ControlPlane operator stack (keystone-operator, horizon-operator,
glance-operator, placement-operator, K-ORC, c5c3-operator) from the published charts — but **not** the `ControlPlane` CR
itself; you create and apply that in Step 3. In this mode the ControlPlane provisions its own MariaDB/Memcached
(managed mode), so deploy-infra does not create the shared ones. `KIND_HOST_PORT=8443`
maps the Gateway to a non-privileged host port for macOS; on Linux with rootful
Docker drop the override and use port `443`. Expect **5–10 minutes**.

If a download or image pull fails, run `make teardown-infra` and repeat Step 2.

::: tip Fresh operator images after a merge
The operator images are published under the mutable `:latest` tag, so
deploy-infra pins them to the digest current at deploy time (per-operator
image-digest ConfigMaps consumed by the HelmReleases via `valuesFrom`). After
a feature merges to `main`, run `make refresh-operator-digests` against the
running cluster: it re-resolves the digests, updates the ConfigMaps, and
requests a Flux reconcile so the operators roll to the freshly built images —
no redeploy needed. The helper prefers `docker buildx`, but falls back to `curl`
if Docker is unavailable.
:::

## Step 3 — Create the ControlPlane CR

Apply a `ControlPlane` CR. You only supply `openStackRelease` and the
`services.keystone` block; the defaulting webhook fills the infrastructure and
admin-credential references with their well-known names:

- `openstack-db` and `openstack-memcached` for the managed infrastructure
- `keystone-db` and `keystone-admin` / `password` as placeholders that the
  operator replaces with per-ControlPlane Secrets in managed mode
- `k-orc-clouds-yaml` with the `admin` cloud entry

The c5c3-operator seeds the K-ORC bootstrap `clouds.yaml` per CR, deriving the
in-cluster Keystone auth URL from the CR name. To use a different name, pass
`CONTROLPLANE_NAME=foo` to Step 2; it renames the bundled CR and seeds the
matching admin password. The defaulting only fills the names and references;
the operator still consumes the pre-seeded Secret content and materialises the
bootstrap `clouds.yaml` itself.

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
    glance:
      replicas: 1
      # Drop publicEndpoint on the default port 443 — the operator then derives
      # https://glance.127-0-0-1.nip.io from the gateway hostname.
      publicEndpoint: https://glance.127-0-0-1.nip.io:8443
      # Exposed through the same shared Envoy Gateway, via the third HTTPS
      # listener the kind overlay adds for glance.127-0-0-1.nip.io.
      gateway:
        parentRef:
          name: openstack-gw
        hostname: glance.127-0-0-1.nip.io
      # One curated S3 image store on the in-cluster Garage object store; the
      # Step 2 stack ships the Garage cluster and the glance-images bucket.
      backends:
        - name: default
          type: S3
          isDefault: true
          s3:
            endpoint: http://garage.openstack.svc.cluster.local:3900
            bucket: glance-images
            region: garage
            credentialsSecretRef:
              name: garage-s3-credentials
    placement:
      replicas: 1
      # Drop publicEndpoint on the default port 443 — the operator then derives
      # https://placement.127-0-0-1.nip.io from the gateway hostname.
      publicEndpoint: https://placement.127-0-0-1.nip.io:8443
      # Exposed through the same shared Envoy Gateway, via the sixth HTTPS
      # listener the kind overlay adds for placement.127-0-0-1.nip.io.
      gateway:
        parentRef:
          name: openstack-gw
        hostname: placement.127-0-0-1.nip.io
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

The `glance` block makes the reconciler project the OpenStack Image service and
one `GlanceBackend` child per `backends` entry — here a single S3 store on the
in-cluster Garage object store. Garage and the ESO-synced
`garage-s3-credentials` Secret ship with the Step 2 stack, so
`credentialsSecretRef` resolves without extra wiring. The `gateway` block
exposes the image API through the same shared Envoy Gateway as Keystone and
Horizon, on the third HTTPS listener the kind overlay adds for
`glance.127-0-0-1.nip.io`, and `publicEndpoint` makes the public image catalog
row advertise the actually reachable host URL — the `:8443` host port — instead
of the default-443 form the operator would otherwise derive from the gateway
hostname. The defaulting webhook injects a `glance` service account (user
`glance`, project `service`, role `service`) into `spec.korc.serviceAccounts` so
Glance can validate the Keystone tokens it receives; its database and cache
derive from `spec.infrastructure`, exactly like Keystone's. On the managed
shared database its DB credential is engine-issued and auto-rotated exactly like
Keystone's — short-lived leases from the OpenBao database engine, and the Step 4
onboarding provisions the engine tenant for all three database services
(keystone, glance, and placement). A `GlanceReady` condition joins the chain —
gating on `KeystoneReady` plus that injected service account — and
`status.services` gains a third entry.

The `placement` block projects a Placement child, `controlplane-placement`: the
API deployment, its own logical schema on the shared MariaDB, and a Keystone
service user. The defaulting webhook injects a `placement` service account into
`spec.korc.serviceAccounts` (user `placement`, role `service`) with a project of
its own, `service-placement`; each `create: true` entry projects a managed
Project, so two entries naming one project would collide. Database and cache
derive from `spec.infrastructure` the same way Glance's do, and on the managed
shared database the DB credential is engine-issued too, from the tenant Step 4
onboards. The `gateway` block puts the API on the sixth HTTPS listener the kind
overlay adds, `placement.127-0-0-1.nip.io`, and `publicEndpoint` carries the
`:8443` host port into the public placement catalog row. A `PlacementReady`
condition joins the chain next to `GlanceReady`, gated the same way, and
`status.services` gains a fourth entry.

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
    glance:
      replicas: 1
      publicEndpoint: https://glance.127-0-0-1.nip.io:8443
      gateway:
        parentRef:
          name: openstack-gw          # same Gateway; third listener
        hostname: glance.127-0-0-1.nip.io
      backends:
        - name: default
          type: S3
          isDefault: true
          s3:
            endpoint: http://garage.openstack.svc.cluster.local:3900
            bucket: glance-images
            region: garage
            credentialsSecretRef:
              name: garage-s3-credentials
    placement:
      replicas: 1
      publicEndpoint: https://placement.127-0-0-1.nip.io:8443
      gateway:
        parentRef:
          name: openstack-gw          # same Gateway; sixth listener
        hostname: placement.127-0-0-1.nip.io
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
    serviceAccounts:
      - name: glance                 # injected because services.glance is set
        userName: glance             # defaults to the account name
        project:
          name: service
          create: true
        roles:
          - service                  # Keystone SRBAC default for identity:validate_token
      - name: placement              # injected because services.placement is set
        userName: placement          # defaults to the account name
        project:
          name: service-placement    # its own project: two create:true entries
                                     # cannot name the same Keystone project
          create: true
        roles:
          - service
```

</details>

## Step 4 — Onboard the OpenBao database-engine tenant

In managed mode the ControlPlane defaults to engine-issued (**Dynamic**) Keystone
DB credentials: ESO draws short-lived MySQL users from the OpenBao
database engine at `database/mariadb/creds/keystone-<namespace>`. The
c5c3-operator only **reads** from that path — the engine connection and the
per-tenant role are provisioned out-of-band, once per ControlPlane, by
`deploy/openbao/bootstrap/setup-database-tenant.sh`.

Here `<namespace>` is the **Keystone service namespace** — the ControlPlane's own
namespace (`openstack`) in this quick start, and only different when
`spec.services.keystone.namespace` places the Keystone service in a namespace of
its own. The onboarding script resolves it from the live ControlPlane spec, so
the two arguments below always name the ControlPlane, wherever its Keystone
lands.

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
  [Migrate Keystone DB to Dynamic Credentials](./guides/keystone/migrate-keystone-db-to-dynamic-credentials.md)).

::: warning If you skip it
The reconcile chain stalls before any Keystone or Horizon child is created: the
ControlPlane reports `DBCredentialsReady=False` (reason
`WaitingForDBCredentialSecret`), the `controlplane-keystone-db-credentials`
ExternalSecret sits in `SecretSyncedError`, and the external-secrets controller
logs `unknown role: keystone-<namespace>`. Nothing is lost — run the onboarding
script and ESO syncs the credential on its next retry.
:::

::: tip Optional: a per-tenant OpenBao identity
By default this ControlPlane reaches OpenBao through the shared cluster store
`openbao-cluster-store`. To give it its own OpenBao identity (so OpenBao itself
enforces isolation from other tenants), run
`deploy/openbao/bootstrap/setup-eso-tenant.sh openstack`, wait for the
`openbao-tenant-store` SecretStore to be `Ready`, then set
`spec.secretStoreRef: {kind: SecretStore, name: openbao-tenant-store}` on the
ControlPlane. See the
[multi-tenant deployment guide](./guides/multi-tenant-deployment.md#per-controlplane-secret-stores-and-openbao-identities).
:::

## Step 5 — Watch the chain reconcile

The aggregate `Ready` flips to `True` once all 13 sub-conditions are met, in
dependency order (`HorizonReady` gates on `KeystoneReady`; `GlanceReady` and
`PlacementReady` gate on `KeystoneReady` plus `ServiceAccountsReady`; the K-ORC
branch runs alongside):

```
NamespacesReady → InfrastructureReady → ESOTenantStoreReady → DBCredentialsReady → AdminPasswordReady → KeystoneReady → HorizonReady → KORCReady → AdminCredentialReady → CatalogReady → ServiceAccountsReady → GlanceReady → PlacementReady
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

::: tip If your host cannot resolve `*.nip.io`
On some local setups that filter or block this specific DNS pattern, the
following command fails:

```bash
curl -k https://keystone.127-0-0-1.nip.io:8443/v3
```

The command returns the following error:

```
curl: (6) Could not resolve host: keystone.127-0-0-1.nip.io
```

`nip.io` deliberately resolves hostnames like `keystone.127-0-0-1.nip.io` to
the loopback address `127.0.0.1`. Some routers ship **DNS rebind
protection**, a security feature that silently drops any DNS response
resolving a public hostname to a private or loopback address, which is
this pattern. When your machine's resolver does this, `nip.io`
never resolves. Confirm the cause by comparing your
router against a public resolver:

```bash
dig +short keystone.127-0-0-1.nip.io @<your-router-ip>   # empty: blocked
dig +short keystone.127-0-0-1.nip.io @1.1.1.1            # 127.0.0.1: as expected
```

The most reliable fix is host-local: add loopback
entries to `/etc/hosts`:

```bash
sudo sh -c 'cat >> /etc/hosts <<EOF
127.0.0.1 keystone.127-0-0-1.nip.io
127.0.0.1 horizon.127-0-0-1.nip.io
127.0.0.1 glance.127-0-0-1.nip.io
127.0.0.1 placement.127-0-0-1.nip.io
EOF'
```

For a one-off `curl` check without touching `/etc/hosts`, use `--resolve` to
override DNS for a single host/port pair — `curl` still sends the original
hostname in the request and TLS handshake, but connects directly to
`127.0.0.1`:

```bash
curl -k --resolve keystone.127-0-0-1.nip.io:8443:127.0.0.1 \
  https://keystone.127-0-0-1.nip.io:8443/v3
```

If you'd rather fix it at the network level, most routers let you exempt
specific domains from rebind protection, or you can add a public resolver
(e.g. `1.1.1.1`) ahead of the router in your machine's network settings.
:::

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
> and drop all three `publicEndpoint` lines (keystone, glance, and placement)
> from the CR in Step 3.

### Upload a first image

With the `OS_*` variables from the token-issue step still exported, confirm the
Image service reached the catalog:

```bash
openstack --insecure catalog list
```

An `image` row proves Glance registered its endpoints. The public image catalog
row now carries the gateway URL (`https://glance.127-0-0-1.nip.io:8443`, the
`publicEndpoint` from Step 3), and the `openstack` CLI resolves the `public`
interface by default, so the upload runs directly from the host through the
shared Gateway — no in-cluster pod needed. `--insecure` accepts the listener's
self-signed certificate, as with the Keystone calls above.

Create a throwaway 1 KiB image, upload it through the gateway, wait for it to
reach `active`, and delete it again:

```bash
dd if=/dev/urandom of=/tmp/first.img bs=1024 count=1
openstack --insecure image create --disk-format raw --container-format bare \
  --file /tmp/first.img first-image
for _ in $(seq 30); do
  [ "$(openstack --insecure image show first-image -f value -c status)" = active ] && break
  sleep 2
done
test "$(openstack --insecure image show first-image -f value -c status)" = active \
  && echo "OK: image uploaded through the gateway and reached active"
openstack --insecure image delete first-image
```

The poll loop matters because `image create` returns as soon as Glance accepts
the upload, while the store write completes asynchronously — a cold S3
connection or a contended Garage backend can leave the image in `saving` for a
few seconds.

A run aborted before the final `delete` leaves `first-image` behind; delete it
(`openstack --insecure image delete first-image`) before retrying, or the next
`image create` fails on a name collision.

::: warning Leave OS_REGION_NAME unset
Do **not** add `OS_REGION_NAME` to the host exports: the projected K-ORC
catalog rows carry no region, image and placement alike, so a region-scoped
lookup finds no endpoint. The upload fails with `public endpoint for image
service in RegionOne region not found`, the placement call with the same message
under its own service name. Keystone's own bootstrap registers RegionOne
identity rows, so identity is unaffected. Only the projected rows need the
filter left clear.
:::

### List placement resource classes

With the same `OS_*` variables still exported, confirm the Placement service
reached the catalog:

```bash
openstack --insecure catalog list
```

A `placement` row proves the ControlPlane registered both endpoints: the
in-cluster one at `http://controlplane-placement.openstack.svc:8778` and the
public one at `https://placement.127-0-0-1.nip.io:8443`, the `publicEndpoint`
from Step 3. Then ask the API for its resource classes:

```bash
openstack --insecure resource class list
```

The call is read-only and Placement answers it only for an authenticated
request, so a listing of the standard classes (`VCPU`, `MEMORY_MB` and `DISK_GB`
among them) covers the catalog row, the gateway listener, and the service user's
token validation in one command. It needs the `osc-placement` plugin from the
prerequisites; without it the `openstack` CLI rejects `resource class list` as
an unknown command. `OS_REGION_NAME` has to stay unset here as well, for the
reason above.

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
- [Glance Operator](./reference/glance/index.md) — the projected Image service,
  its `GlanceBackend` stores, and the reconciler chain.
- [Placement Operator](./reference/placement/index.md) — the projected Placement
  service, its CRD surface, and the reconciler chain.
- [Quick Start](./quick-start.md) — the compact per-service Keystone path.
