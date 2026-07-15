---
title: Deploy Services into Dedicated Namespaces
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Deploy Services into Dedicated Namespaces

By default every service a ControlPlane projects lands in the ControlPlane's
own namespace, so no NetworkPolicy, RBAC, or quota line can be drawn between
the services of one control plane. This guide places the **Keystone service in
a namespace of its own** — `openstack-internal`, created and owned by the
operator — while the **Horizon dashboard stays in `openstack`**, the
ControlPlane's namespace. The backing services, the per-tenant secret store,
and the credential material follow each service into its namespace; the
dashboard keeps reaching the identity service across the namespace boundary
with no extra wiring.

The walkthrough builds on the ControlPlane Quick Start and replaces its
`ControlPlane` CR with a namespace-assigned one — the assignment is
**create-only**, so it cannot be patched onto a live ControlPlane.

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial **through Step 2 only** (cluster + operator stack) and
stop **before Step 3**: the namespace assignment is create-only, so this
guide's Step 3 applies its own `ControlPlane` CR in place of the tutorial's.
Every resource name in the examples below is one that devstack produces.
:::

::: warning The Keystone child is operator-owned
On a ControlPlane deployment the `controlplane-keystone` Keystone CR is
**projected** by the c5c3-operator, so a knob you set directly on the child is
reverted on the next reconcile. Set operational knobs on the `ControlPlane` CR
and let the operator project them down. Where the `ControlPlane` CRD does not
expose a knob, this guide points to the
[Standalone Keystone](#standalone-keystone-without-a-controlplane) section,
which drives a Keystone CR you own.
:::

1. **A fresh environment.** If the tutorial's `ControlPlane` already exists,
   the placement cannot be added to it (create-only), and admission permits
   only one ControlPlane per namespace. Run `make teardown-infra` and bring the
   devstack up again rather than deleting and re-creating the CR in place.

---

## Background: what a namespace assignment moves

A `namespace` block on `spec.services.keystone` (or `services.horizon`) places
that service — and everything scoped to it — in the named namespace:

- the projected service child (`controlplane-keystone`) and its Deployment;
- the **backing services** materialized from the shared
  `spec.infrastructure` block: each namespace that hosts a service gets its own
  MariaDB / Memcached instances, so here `openstack-internal` receives
  `openstack-db` and `openstack-memcached` for Keystone, and `openstack` keeps
  an `openstack-memcached` for the dashboard;
- the **credential material**: the `controlplane-keystone-admin-credentials`
  and `controlplane-keystone-db-credentials` ExternalSecrets and Secrets are
  materialised beside the Keystone child, and their OpenBao paths are re-keyed
  on the Keystone service namespace;
- a per-namespace **`openbao-tenant-store`** SecretStore, because an ESO store
  is namespace-local.

Cross-namespace children carry the ControlPlane's ownership labels
(`c5c3.io/controlplane-name`, `c5c3.io/controlplane-namespace`) instead of an
owner reference — Kubernetes rejects one across namespaces — and the deletion
finalizer tears them down explicitly. The
[Service Namespaces](../reference/c5c3/controlplane-crd.md#service-namespaces)
reference covers the full contract, including the `Managed` / `External`
lifecycles and the tenant-key uniqueness rules.

A service **without** an assignment stays in the ControlPlane's namespace —
that is the whole configuration for Horizon in this guide: its spec block
simply carries no `namespace`.

## Steps

### 1. Allow the Keystone route onto the shared Gateway

The devstack's Envoy Gateway `openstack-gw` lives in `openstack` and ships
with `allowedRoutes.namespaces.from: Same` on both listeners. With Keystone
placed in `openstack-internal`, the HTTPRoute the keystone-operator projects
now lives **there**, and the Gateway must explicitly admit routes from that
namespace — otherwise the route is never `Accepted`, the Keystone child parks
on `HTTPRouteReady=False`, and the ControlPlane never reaches `Ready`.

Open the Keystone listener (the first one, `https`) to the
`openstack-internal` namespace; the `https-horizon` listener keeps
`from: Same` because the dashboard stays in `openstack`:

```bash
kubectl patch gateway openstack-gw -n openstack --type=json -p='[{
  "op": "replace",
  "path": "/spec/listeners/0/allowedRoutes/namespaces",
  "value": {
    "from": "Selector",
    "selector": {
      "matchLabels": {"kubernetes.io/metadata.name": "openstack-internal"}
    }
  }
}]'
```

The `kubernetes.io/metadata.name` label is stamped on every namespace by
Kubernetes itself, so the selector needs no labelling step and resolves as
soon as the operator creates the namespace. No `ReferenceGrant` is needed:
route-to-Gateway attachment is governed by the Gateway's `allowedRoutes`
alone, and the route's backend Service stays namespace-local to the route.

> Re-running `make deploy-infra` re-applies the stock Gateway manifest and
> restores `from: Same` — re-apply this patch afterwards.

### 2. Seed the admin password on the re-keyed OpenBao path

The bootstrap admin password is read from
`bootstrap/<keystone-namespace>/controlplane-keystone/admin` — the path
follows the Keystone service. The devstack bring-up seeded
`bootstrap/openstack/...`, so the path this ControlPlane will read,
`bootstrap/openstack-internal/controlplane-keystone/admin`, does not exist
yet. The seeding script accepts the Keystone service namespace as an optional
third identity segment and is idempotent — paths that already exist are
skipped:

```bash
export BAO_TOKEN=$(kubectl get secret openbao-init-keys -n openbao-system \
  -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token')
KORC_CONTROLPLANES="openstack/controlplane/openstack-internal" \
  deploy/openbao/bootstrap/write-bootstrap-secrets.sh
unset BAO_TOKEN
```

Besides writing the password, the script stamps the path with the
`managed-by=external-secrets` metadata marker, so the keystone-operator's
admin-password rotation `PushSecret` can later adopt and overwrite it. The
Horizon `SECRET_KEY` path is keyed on the ControlPlane's namespace and was
seeded by the bring-up, so the dashboard needs nothing here.

### 3. Create the ControlPlane with the namespace assignment

The CR is the tutorial's Step 3 CR plus two additions on the Keystone block: the
`namespace` assignment, and an explicit `gateway.parentRef.namespace` — when
the field is empty the projected child's **own** namespace is assumed, which
would now point at a Gateway that does not exist in `openstack-internal`.
Horizon carries no `namespace` block, so it stays in `openstack`:

```yaml
# controlplane.yaml
apiVersion: c5c3.io/v1alpha1
kind: ControlPlane
metadata:
  name: controlplane
  namespace: openstack
spec:
  openStackRelease: "2025.2"
  # Single-node backing services for kind, as in the Quick Start. Every
  # namespace that hosts a service materializes its instances from this one
  # shared block.
  infrastructure:
    database:
      replicas: 1
      storageSize: 512Mi
    cache:
      replicas: 1
  services:
    keystone:
      replicas: 1
      # Place the identity service — and its database, cache, secret store,
      # and credential material — in a namespace of its own. Managed: the
      # operator creates, labels, and (on deletion) removes the namespace.
      namespace:
        name: openstack-internal
        lifecycle: Managed
      publicEndpoint: https://keystone.127-0-0-1.nip.io:8443/v3
      gateway:
        parentRef:
          name: openstack-gw
          # The shared Gateway stays in the ControlPlane's namespace. Without
          # this line the Keystone child's own namespace would be assumed.
          namespace: openstack
        hostname: keystone.127-0-0-1.nip.io
        path: /
    horizon:
      # No namespace block: the dashboard stays in the ControlPlane's own
      # namespace (openstack), exactly as in the Quick Start.
      replicas: 1
      gateway:
        parentRef:
          name: openstack-gw
        hostname: horizon.127-0-0-1.nip.io
```

```bash
kubectl apply -f controlplane.yaml
```

Do **not** pre-create `openstack-internal`: under the `Managed` lifecycle the
operator creates and labels the namespace itself, and it refuses to adopt a
pre-existing namespace that lacks its ownership labels
(`NamespacesReady=False`, reason `NamespaceNotOwned`). A namespace you
provision yourself is the `External` lifecycle —
[see below](#using-a-pre-existing-namespace-external-lifecycle).

Two rules to know before applying:

- **Create-only.** The block's presence, its `name`, and its `lifecycle` are
  frozen after creation — moving a live service would strand its backing
  services and every OpenBao path keyed on the old namespace. Delete and
  re-create the ControlPlane to change the placement.
- **One ControlPlane per namespace.** The service namespace is the tenant key
  the secret stack is scoped by, so admission rejects an assignment naming a
  namespace another ControlPlane already occupies.

### 4. Onboard the OpenBao database-engine tenant

Same one-time onboarding as the tutorial's Step 4, with one difference: the
managed MariaDB now lives in `openstack-internal`, so the readiness wait moves
there. The script arguments are unchanged — they name the **ControlPlane**,
and the script resolves the Keystone service namespace from the live spec and
provisions the database-engine role `keystone-openstack-internal` accordingly:

```bash
kubectl wait mariadb/openstack-db -n openstack-internal --for=condition=Ready --timeout=10m

export BAO_TOKEN=$(kubectl get secret openbao-init-keys -n openbao-system \
  -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token')
deploy/openbao/bootstrap/setup-database-tenant.sh openstack controlplane
unset BAO_TOKEN
```

::: warning If you skip it
The chain stalls exactly as in the Quick Start, one namespace over: the
ControlPlane reports `DBCredentialsReady=False`, the
`controlplane-keystone-db-credentials` ExternalSecret — now in
`openstack-internal` — sits in `SecretSyncedError`, and the external-secrets
controller logs `unknown role: keystone-openstack-internal`. Run the
onboarding script and ESO syncs the credential on its next retry.
:::

## Verification

The condition chain gains `NamespacesReady` at its head; wait for the
aggregate as usual:

```bash
kubectl get controlplane controlplane -n openstack \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
kubectl wait controlplane/controlplane -n openstack --for=condition=Ready --timeout=15m
```

**The namespace is operator-owned.** `openstack-internal` exists and carries
the ownership labels plus the managed-by stamp:

```bash
kubectl get namespace openstack-internal --show-labels
```

```
NAME                 STATUS   AGE   LABELS
openstack-internal   Active   5m    app.kubernetes.io/managed-by=c5c3-operator,c5c3.io/controlplane-name=controlplane,c5c3.io/controlplane-namespace=openstack,kubernetes.io/metadata.name=openstack-internal
```

**Each service sits in its namespace, with its backing services.** Keystone,
its MariaDB, its Memcached, and its credential Secrets are in
`openstack-internal`; the dashboard and its own Memcached are in `openstack`:

```bash
kubectl get keystone,mariadb,memcached -n openstack-internal
kubectl get horizon,memcached -n openstack
```

The cross-namespace children carry the ownership labels and **no** owner
reference (garbage collection cannot cross a namespace, so the deletion
finalizer owns their teardown):

```bash
kubectl get mariadb openstack-db -n openstack-internal \
  -o jsonpath='{.metadata.labels.c5c3\.io/controlplane-name}{" / "}{.metadata.ownerReferences}{"\n"}'
```

**The API answers through the shared Gateway**, exactly as on the unsplit
devstack — the route attaches across namespaces thanks to Step 1:

```bash
curl -k https://keystone.127-0-0-1.nip.io:8443/v3
```

**The admin credential follows the Keystone service**, so read it from
`openstack-internal` now:

```bash
export OS_AUTH_URL=https://keystone.127-0-0-1.nip.io:8443/v3
export OS_USERNAME=admin
export OS_PASSWORD=$(kubectl get secret controlplane-keystone-admin-credentials \
  -n openstack-internal -o jsonpath='{.data.password}' | base64 -d)
export OS_PROJECT_NAME=admin
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
openstack --insecure token issue
```

**The dashboard logs in across the namespace boundary.** The horizon-operator
derives the identity endpoint as namespace-qualified Service DNS
(`http://controlplane-keystone.openstack-internal.svc:5000/v3`), which
resolves across namespaces unchanged — open
`https://horizon.127-0-0-1.nip.io:8443/` and log in with `admin` / the
password read above (domain `Default`), as in the Quick Start.

::: tip Network policies are yours to write
The operator creates no NetworkPolicies — splitting services across namespaces
is what makes writing them possible. The kind devstack's default CNI does not
enforce NetworkPolicy, so nothing is needed here; for a default-deny
production posture, the flows to allow (dashboard → Keystone `5000`, K-ORC →
Keystone `5000`, each service → its own database/cache) are tabulated in the
[cross-namespace traffic matrix](../reference/c5c3/controlplane-crd.md#network-policies).
:::

## Using a pre-existing namespace (External lifecycle)

When the namespace's quotas, RBAC, and policies are provisioned out-of-band,
hand the operator a namespace you own instead: create it first, and declare
`lifecycle: External` —

```yaml
      namespace:
        name: openstack-internal
        lifecycle: External
```

The operator then only **verifies** the namespace exists (a missing one parks
on `NamespacesReady=False`, reason `NamespaceNotFound`), never labels or
mutates it, and on ControlPlane deletion the namespace **survives** — the
residue the ControlPlane placed in it is swept by name, but the namespace
stands. Note that an External namespace shared with unrelated third-party
workloads also shares the namespace-scoped OpenBao path scope; pick a
dedicated namespace when that isolation matters. Everything else in this
guide — the Gateway patch, the seed path, the onboarding — is identical for
both lifecycles.

## Deletion

Deleting the ControlPlane tears the split deployment down completely: the
finalizer deletes the cross-namespace children explicitly (labels, not garbage
collection, connect them to their owner), then removes the `Managed`
`openstack-internal` namespace with everything left in it. The devstack-wide
`make teardown-infra` covers this too.

## Standalone Keystone, without a ControlPlane

The namespace assignment is a ControlPlane-level knob — it orchestrates
namespace lifecycle, backing-service placement, secret-store distribution, and
OpenBao path re-keying across namespaces. A standalone Keystone has no
orchestrator to do any of that: the Keystone CR simply lives in whatever
namespace you create it in, and everything it consumes (the MariaDB, the
Memcached, the admin and DB Secrets, an ESO store) must be provisioned in that
same namespace by hand, as the [Quick Start](../quick-start.md) does for
`openstack`. There is no `Managed`/`External` distinction and no
cross-namespace teardown — you own the namespace and its contents end to end.

## See also

- [ControlPlane CRD reference — Service Namespaces](../reference/c5c3/controlplane-crd.md#service-namespaces) —
  the `ServiceNamespaceSpec` fields, lifecycles, ownership labels, secret
  distribution, and uniqueness/immutability rules.
- [ControlPlane Reconciler](../reference/c5c3/controlplane-reconciler.md) —
  where `reconcileNamespaces` sits in the chain and the cross-namespace
  deletion ordering.
- [Multi-Tenant Deployment](./multi-tenant-deployment.md) — the other tenancy
  axis: namespace-scoped operator installs and several ControlPlanes side by
  side. Note that the Helm chart's namespace-scoped RBAC mode does **not**
  support dedicated service namespaces — the operator needs cluster-scoped
  namespace and cross-namespace child access.
- [Quick Start (ControlPlane)](../quick-start-controlplane.md) — the devstack
  this guide builds on.

## Tested by

The flow above mirrors the following end-to-end suite:

```bash
chainsaw test --test-dir tests/e2e/c5c3/dedicated-namespaces
```

The suite asserts the placement and lifecycle half on a live cluster — both
lifecycles, backing-service placement, ownership labels, per-namespace tenant
stores, and the deletion sweep. The credential re-keying and the projected
Keystone child are hard-asserted against the real CRD schema and webhook by
the envtest scenario `TestIntegration_DedicatedNamespaces`
(`operators/c5c3/internal/controller/integration_test.go`), which runs on
every PR.

The suite's fixture below is **isolation-named**, not devstack-named: the CR
is called `cp`, it places Keystone under the `Managed` and Horizon under the
`External` lifecycle to cover both, and the `@KEYSTONE_NS@` / `@HORIZON_NS@`
tokens are substituted per run from chainsaw's ephemeral namespace so parallel
suites never collide. The walkthrough above keeps the names your devstack
actually produces.

::: details The ControlPlane fixture the suite applies
<<< @/../tests/e2e/c5c3/dedicated-namespaces/00-controlplane-cr.yaml#controlplane
:::
