---
title: Run Barbican on a Dedicated OpenBao
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

<!-- operator namespace is `barbican-system`; workload (Barbican CR) stays in `openstack`. -->

# How-to: Run Barbican on a Dedicated OpenBao

This guide adds the Key Manager service to a running ControlPlane in the mode
that needs nothing from outside the cluster. With
`services.barbican.secretStore.dedicated`, the c5c3-operator provisions an
OpenBao instance for this one Barbican, attaches the secret store to it, and lets
the barbican-operator mint the AppRole credentials the API pods authenticate
with. The walkthrough ends with a secret stored and read back from the host, and
with the teardown that removes the service again.

The instance the ControlPlane provisions is proving-grade. It runs the
openbao-operator's Development profile at a single replica with no
PodDisruptionBudget, so any disruption of that one pod stops every secret read
and write. It is sealed by a static key held in a plain Secret
(`controlplane-barbican-bao-unseal-key`) in the same namespace as the raft volume
that key seals, so read access to that namespace's Secrets, or a single etcd or
namespace backup, yields the ciphertext and the key together. Admission repeats
that as a warning on every apply. For a production key manager, point the service
at a hardened server instead: see
[Attach an External OpenBao to Barbican](./attach-external-openbao.md).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so a `ControlPlane`
CR named `controlplane` is `Ready` in the `openstack` namespace. Its Step 3 CR
already carries the `services.barbican` block step 1 shows, so on a devstack
brought up from it the service is reconciled and step 1 reads as a check. A
ControlPlane created before that block existed, or from the bundled minimal CR
(`deploy/kind/controlplane/controlplane.yaml`, keystone and horizon only), gets
the block added in step 1. Every resource name in the examples that follow is
one this devstack produces.
:::

1. **The `python-barbicanclient` plugin on `PATH`.** The `openstack secret`
   subcommands of step 5 come from it; without it the CLI rejects
   `secret store` as an unknown command.
2. **`jq` on `PATH`** for the condition readouts in step 3.
3. **The OpenBao root token**, for the one-time database-engine onboarding in
   step 2. On kind it is read from the `openbao-init-keys` Secret, as in the
   tutorial's Step 4.

## Step 1 — Declare the Barbican service

Barbican is one block on the `ControlPlane` CR. Edit the CR the tutorial created
and add it beside the existing services:

```bash
kubectl edit controlplane controlplane -n openstack
```

```yaml
spec:
  services:
    barbican:
      replicas: 1
      # The ControlPlane provisions the OpenBao instance and derives everything
      # else (its name, its KV mount, its AppRole) by convention, so the block
      # carries no fields.
      secretStore:
        dedicated: {}
      # The key-manager API on the Barbican listener the kind overlay adds for
      # barbican.127-0-0-1.nip.io, on the same shared Envoy Gateway as Keystone.
      publicEndpoint: https://barbican.127-0-0-1.nip.io:8443
      gateway:
        parentRef:
          name: openstack-gw
        hostname: barbican.127-0-0-1.nip.io
```

`publicEndpoint` is what carries the `:8443` host port into the public
key-manager catalog row. Dropped, the operator derives
`https://barbican.127-0-0-1.nip.io` from the gateway hostname, which is the
default-443 form and unreachable on a kind cluster mapped to 8443.

The operator projects a `KeystoneService` registration named after the Barbican
child, carrying the key-manager catalog entry and the `barbican` service account
(user `barbican`, project `service-barbican`, role `service`), so Barbican has a
Keystone user to validate tokens as. The database, the cache, and the Keystone
endpoint are derived from `spec.infrastructure` and from the Keystone child's
naming convention; none of them is set here.

## Step 2 — Onboard the barbican database-engine tenant

On the managed shared database the Barbican DB credential is engine-issued, and
`setup-database-tenant.sh` provisions one engine connection and one role per
declared service. It reads the live ControlPlane spec on every run and skips a
service the CR does not declare, so a tenant setup that ran before
`services.barbican` was declared provisioned no Barbican pair. Re-run it in that
case:

```bash
export BAO_TOKEN=$(kubectl get secret openbao-init-keys -n shared-services \
  -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token')
deploy/openbao/bootstrap/setup-database-tenant.sh openstack controlplane
unset BAO_TOKEN
```

The script is idempotent: every config and role write is an upsert, so a repeat
refreshes the pairs of the other services in place and writes
`database/mariadb/config/barbican-openstack` and
`database/mariadb/roles/barbican-openstack`, new when the earlier run skipped
Barbican. Without that pair the next step parks on `BarbicanReady=False` with
reason `WaitingForBarbicanDBCredential`, and the message names the
`database/mariadb/creds/barbican-openstack` path it is waiting for.

## Step 3 — Watch BarbicanReady

Watch the service's own condition, `BarbicanReady`, rather than the
ControlPlane-wide `Ready`. The aggregate also covers Keystone, Horizon, Glance,
and Placement, so it answers a broader question than the one this guide asks:

```bash
kubectl wait controlplane/controlplane -n openstack \
  --for=condition=BarbicanReady --timeout=15m
```

While it converges, the reason names the step in progress:

```bash
kubectl get controlplane controlplane -n openstack \
  -o jsonpath='{.status.conditions[?(@.type=="BarbicanReady")]}' | jq
```

| Reason | What it is waiting for |
| --- | --- |
| `WaitingForKeystone` | `KeystoneReady` is not `True` yet. Barbican validates every token against the Keystone child, so its projection is deferred until then. |
| `WaitingForServiceRegistration` | The `KeystoneService` registration projected above has not provisioned the `barbican` Keystone user and its password yet. No Barbican child is written until it has. |
| `ServiceRegistrationError` | Projecting, reading or mirroring that registration child failed. The message relays what went wrong. |
| `WaitingForBarbicanDBCredential` | No engine-issued credential has landed. Re-run step 2. |
| `WaitingForOpenBaoInstance` | The dedicated instance is not `Available`. The store and the child are projected once it serves requests. |
| `WaitingForBarbican` | The Barbican child exists and is not `Ready` yet (db-sync, rollout, store projection). |

## Step 4 — Inspect what the ControlPlane projected

Three CRs carry the service. Their names are derived from the ControlPlane's:

```bash
kubectl get barbican controlplane-barbican -n openstack
kubectl get barbicansecretstore controlplane-barbican-store -n openstack
kubectl get openbaocluster controlplane-barbican-bao -n openstack
```

The `BarbicanSecretStore` is the attachment: it points at the Barbican by name
and at the instance by `openBao.instanceRef`, and it is the Barbican's default
store. Its `Ready` condition decomposes into `CredentialsReady`,
`ProvisioningReady`, and `ConfigProjected`:

```bash
kubectl get barbicansecretstore controlplane-barbican-store -n openstack \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\n"}{end}'
```

`ProvisioningReady=True/Provisioned` means the barbican-operator logged in to the
instance as its provisioner ServiceAccount and found the `barbican/` KV mount and
the `barbican` AppRole the self-init created. The credentials it mints from that
AppRole land in `controlplane-barbican-store-approle`, which is owned by the
store rather than by the Barbican.

Around the instance sits the ensemble the openbao-operator's contracts require.
Every object is named after the instance:

| Object | Name | Purpose |
| --- | --- | --- |
| Secret | `controlplane-barbican-bao-unseal-key` | The static seal key, generated once and never rotated |
| Secret | `controlplane-barbican-bao-tls-server` | The API listener's cert-manager keypair (External TLS mode) |
| Secret | `controlplane-barbican-bao-tls-ca` | The trust bundle the operator and the API pods verify against |
| ServiceAccount | `controlplane-barbican-bao-provisioner` | The identity the barbican-operator's Kubernetes-auth login binds to |
| Role + RoleBinding | `controlplane-barbican-bao-provisioner-token` | Lets the barbican-operator mint a bound token for that one account |
| ClusterRoleBinding | `controlplane-barbican-bao-<hash>-auth-delegator` | Grants the instance pods the TokenReview every Kubernetes-auth login needs |
| OpenBaoTenant | `controlplane-barbican-bao-tenant` | Admits the namespace to the multi-tenant openbao-operator |

```bash
kubectl get secret,serviceaccount,role,rolebinding -n openstack \
  | grep controlplane-barbican-bao
kubectl get clusterrolebinding | grep controlplane-barbican-bao
```

The auth-delegator binding is cluster-scoped, so its name carries a hash of
`namespace/instance`: two ControlPlanes of the same name in different namespaces
derive the same instance name and would otherwise collide on one binding.

No `controlplane-barbican-bao-tenant` appears on this devstack. The kind overlay
already ships an `OpenBaoTenant` admitting `openstack`, and the operator skips a
namespace some tenant already targets, because a namespace admitted twice carries
two finalizers. A ControlPlane whose Barbican lands in a fresh service namespace
gets its own tenant there.

## Step 5 — Store and read a secret back

The key-manager row is in the catalog once `BarbicanReady` is `True`, and the
public endpoint is the gateway URL from step 1, so the round-trip runs from the
host. Export the admin credentials the way the tutorial's Verify step does, and
leave `OS_REGION_NAME` unset: the projected K-ORC catalog rows carry no region,
so a region-scoped lookup finds no key-manager endpoint.

```bash
export OS_AUTH_URL=https://keystone.127-0-0-1.nip.io:8443/v3
export OS_USERNAME=admin
export OS_PASSWORD=$(kubectl get secret controlplane-keystone-admin-credentials -n openstack -o jsonpath='{.data.password}' | base64 -d)
export OS_PROJECT_NAME=admin
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default

openstack --insecure catalog list | grep key-manager
```

Then store a secret, read its payload back, and delete it:

```bash
HREF=$(openstack --insecure secret store --name roundtrip \
  --payload 'cobaltcore-barbican-payload' -f value -c 'Secret href')
openstack --insecure secret get -p "$HREF" -f value -c Payload
openstack --insecure secret delete "$HREF"
```

A payload that comes back unchanged covers the whole chain in one call: the
catalog row resolves the endpoint, Barbican validates the token against Keystone,
and the payload travels through castellan's vault plugin into the dedicated
OpenBao instance and out again. A catalogue-only check misses every step after
the first.

`cobaltcore-barbican-payload` is a throwaway. Real key material does not belong behind
`--payload`, which leaves it readable in the process argument vector and in your
shell history; feed it in from a file or from standard input instead. `--insecure`
is here because the kind gateway presents a self-signed certificate. It disables
verification for the whole invocation, including the Keystone call above that
sends `OS_PASSWORD`, so drop it wherever the gateway presents a certificate you
trust.

## Step 6 — Remove the service

Dropping the `services.barbican` block is not by itself enough to remove
anything. Deletion takes an annotation, and destroying the stored secret material
takes a second one.

::: warning Two opt-ins, and never edit the projected children
The three CRs of step 4 are **projected** by the c5c3-operator. A knob you set on
`controlplane-barbican`, `controlplane-barbican-store`, or
`controlplane-barbican-bao` directly is reverted on the next reconcile, and a
projected child you delete by hand is recreated. Drive every change from the
`ControlPlane` CR.

Removing `services.barbican` with no annotation **preserves** the Barbican child,
its store, and the instance; only the dynamic DB-credential generator is torn
down, because a live generator would keep issuing MySQL users for a service this
ControlPlane no longer manages. `BarbicanReady` goes `True` with reason
`BarbicanNotManaged` and the message names what was kept.

`c5c3.io/allow-barbican-deletion=true` authorises the teardown of the Barbican
child, its secret store, its DB-credential objects, and its catalog rows.

That first annotation is already destructive on the database side. Deleting the
Barbican child runs its finalizer, which deletes the MariaDB `Database` CR, and
the CR the operator projects carries no `cleanupPolicy`, so mariadb-operator
applies its `Delete` default and drops the `barbican` schema:
`secret_store_metadata`, every secret and container row, the ACLs, and the
quotas. The payloads in OpenBao outlive that, but nothing references them any
more. Back the schema up before the teardown if you intend to come back to these
secrets.

`c5c3.io/allow-barbican-secret-store-data-deletion=true` is required **on top of**
it before the teardown reaches the dedicated OpenBao instance. That instance
carries `deletionPolicy: DeletePVCs`, and the teardown removes the static-seal
Secret as well, so a recovered volume stays unreadable. There is no snapshot, no
soft delete, and no grace period: every secret Barbican ever stored is gone the
moment the delete lands. Set this one only when that is what you mean.
:::

To remove the service and keep the OpenBao instance, back the schema up first,
then annotate and drop the block:

```bash
kubectl annotate controlplane controlplane -n openstack \
  c5c3.io/allow-barbican-deletion=true
kubectl edit controlplane controlplane -n openstack   # drop the services.barbican block
```

Re-declaring `services.barbican` later reattaches to the same instance, but not
to the same secrets: db-sync recreates the dropped schema empty, so `secret list`
comes back with nothing and the payloads still in OpenBao stay unreferenced.
Restoring a `barbican` schema dump taken before the teardown is what brings them
back.

To destroy the instance with the service, set the second annotation before
dropping the block:

```bash
kubectl annotate controlplane controlplane -n openstack \
  c5c3.io/allow-barbican-secret-store-data-deletion=true
```

That teardown spans several reconciles. The sweep deletes the `OpenBaoCluster`,
waits for it to leave etcd, and only then removes the seal Secret, the
provisioner RBAC, and the cluster-scoped auth-delegator binding. Both annotations
are read from the CR on every one of those passes. Take one away mid-sweep and
the next pass returns on the preserve branch with the instance already gone,
leaving that last group behind. No namespace deletion collects a cluster-scoped
binding, so the auth-delegator stays until someone removes it by hand.

Wait for the sweep to finish. It has when the instance is gone and the binding
with it:

```bash
kubectl get openbaocluster controlplane-barbican-bao -n openstack   # expect NotFound
kubectl get clusterrolebinding | grep controlplane-barbican-bao     # expect no output
```

Both annotations survive the teardown on the `ControlPlane`, and nothing in the
operator clears them: left in place, they authorise the next drop of the block
without a further confirmation. Remove them once the sweep has finished:

```bash
kubectl annotate controlplane controlplane -n openstack \
  c5c3.io/allow-barbican-deletion- \
  c5c3.io/allow-barbican-secret-store-data-deletion-
```

On a `ControlPlane` reconciled from Git, set and clear both annotations in the
manifest, not with `kubectl annotate`. Flux applies server-side and prunes only
the fields it owns, so an annotation it never saw survives every reconcile:
setting one out of band leaves a live authorisation on the CR that taking it out
of Git cannot clear. If you already set one out of band, take it away the same
way, with the `kubectl annotate ... -` above.

## Standalone Barbican, without a ControlPlane

Without a ControlPlane nothing projects the OpenBao instance, the store, or the
Barbican child, so you own all three. The names below are ones you choose; they
are not produced by any devstack, and none of the `controlplane-` names from the
walkthrough above applies here.

The instance is an `OpenBaoCluster` whose self-init provisions what a managed
store expects to find: the `barbican/` KV v2 mount, the `barbican-secretstore`
policy, the `barbican` AppRole carrying it, the Kubernetes auth method, and a
`provisioner` role the barbican-operator logs in through.

```yaml
apiVersion: openbao.org/v1alpha1
kind: OpenBaoCluster
metadata:
  name: barbican-bao
  namespace: openstack
spec:
  profile: Development
  version: "2.6.2"        # the static seal needs >= 2.4.0
  replicas: 1
  storage:
    size: 1Gi
  tls:
    enabled: true
    mode: External        # consumes barbican-bao-tls-server and barbican-bao-tls-ca
  unseal:
    type: static          # key material: Secret barbican-bao-unseal-key, data key `key`
  selfInit:
    enabled: true
    requests:
      - name: barbican_kv
        operation: create
        path: sys/mounts/barbican
        secretEngine:
          type: kv
          options:
            version: "2"
      # ... the policy, approle, and kubernetes-auth requests
```

`deploy/kind/infrastructure/openbao-instance.yaml` is the complete worked
example, with every self-init request spelled out and the objects that have to
exist beside the CR: the two cert-manager `Certificate`s backing the fixed-name
TLS Secrets, the provisioner `ServiceAccount`, and the `ClusterRoleBinding` that
grants the instance pods `system:auth-delegator`, without which every
Kubernetes-auth login fails with 403. Self-init runs once against freshly
initialised storage and OpenBao revokes the root token afterwards, so changing
the request list means recreating the instance and its PVC.

The store attaches to the instance by name and marks itself the default:

```yaml
apiVersion: barbican.openstack.c5c3.io/v1alpha1
kind: BarbicanSecretStore
metadata:
  name: barbican-store
  namespace: openstack
spec:
  barbicanRef:
    name: barbican
  type: OpenBao
  isDefault: true
  openBao:
    instanceRef:
      name: barbican-bao
```

`kvMountpoint` is left at its default. A store in `instanceRef` mode is pinned to
`barbican/` and to the root namespace, because that is the only mount the
self-init contract provisions.

The Barbican CR names its own database, cache, Keystone endpoint, and service
user:

```yaml
apiVersion: barbican.openstack.c5c3.io/v1alpha1
kind: Barbican
metadata:
  name: barbican
  namespace: openstack
spec:
  openStackRelease: "2025.2"
  deployment:
    replicas: 1
  image:
    repository: ghcr.io/c5c3/barbican
    tag: "2025.2"
  database:
    clusterRef:
      name: openstack-db
    database: barbican
    secretRef:
      name: barbican-db
  cache:
    clusterRef:
      name: openstack-memcached
  keystoneEndpoint: http://keystone.openstack.svc.cluster.local:5000/v3
  serviceUser:
    username: barbican
    projectName: service
    userDomainName: Default
    projectDomainName: Default
    secretRef:
      name: barbican-service-user
      key: password
```

The store may be applied before the Barbican CR: the attachment is inverted, so a
dangling `barbicanRef` surfaces as `Ready=False` and resolves when the CR
appears. See the
[Barbican CRD API Reference](../../reference/barbican/barbican-crd.md) for the
full standalone field surface.

## See also

- [BarbicanSecretStore CRD](../../reference/barbican/barbican-secret-store-crd.md) —
  both store modes, the credential contract, and the condition catalog.
- [Barbican Reconciler Architecture](../../reference/barbican/barbican-reconciler.md) —
  the sub-reconciler pipeline behind the child's `Ready`.
- [Attach an External OpenBao to Barbican](./attach-external-openbao.md) — the
  same service against a server run outside this control plane.
- [Migrate Barbican DB to Dynamic Credentials](./migrate-barbican-db-to-dynamic-credentials.md) —
  the credential mode step 2 onboards.

## Tested by

The projected chain this guide walks through (the `controlplane-barbican-bao`
instance, the `controlplane-barbican-store` attachment, the minted AppRole
Secret, and a secret round-trip through the catalog) is asserted on the live CI
e2e kind cluster by the first suite below. The second drives a managed store
directly, without a ControlPlane.

```bash
chainsaw test --test-dir tests/e2e/c5c3/full-controlplane-keystone
chainsaw test --test-dir tests/e2e/barbican/secretstore-managed
```

::: details The store CR the secretstore-managed suite applies
The suite isolates its Barbican from the parallel suite pool, so its CR names are
the suite's isolation identifiers (Barbican `barbican-store-managed`, store
`barbican-store-managed-store`, instance `openbao-instance`) rather than the
`controlplane-barbican` names the walkthrough above uses.

<<< @/../tests/e2e/barbican/secretstore-managed/01-barbican-secretstore.yaml#managed-store
:::
