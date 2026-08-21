---
title: Standalone Placement
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Standalone Placement

Placement can run without a ControlPlane. The placement-operator reconciles a
`Placement` CR on its own: it provisions the schema, renders `placement.conf`,
runs the db-sync Job, and serves the API behind a Gateway API HTTPRoute. What it
does not do is talk to Keystone's admin API. On a ControlPlane the c5c3 operator
mints the service user, assigns its role, and registers the service and its
endpoints through K-ORC. Standalone, that half is yours.

This guide walks the whole bring-your-own path against the Quick Start devstack:
install the operator, create the Keystone identity objects, apply the CR, and
hand-register the catalog rows. For the field-by-field contract, see the
[Placement CRD API Reference](../../reference/placement/placement-crd.md); for
the sub-reconciler order and the conditions each step reports, see the
[Reconciler Architecture](../../reference/placement/placement-reconciler.md).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start](../../quick-start.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the Keystone CR
`keystone` is `Ready` in the `openstack` namespace and `openstack token issue`
answers. Every resource name below is one that devstack produces: the admin
credentials Secret `keystone-admin`, the shared `openstack-db` and
`openstack-memcached`, the ESO-synced `placement-db` Secret, and the
`openstack-gw` Gateway.
:::

- The OpenStack CLI
  ([`python-openstackclient`](https://docs.openstack.org/python-openstackclient/latest/))
  on `PATH`, plus the
  [`osc-placement`](https://docs.openstack.org/osc-placement/latest/) plugin for
  the verification in Step 5.
- The devstack's `placement-db` Secret in `openstack`. External Secrets
  materializes it from OpenBao as part of `make deploy-infra`; the Placement CR
  below points `spec.database.secretRef` at it.

## Step 1 — Install the placement-operator

The Quick Start already applied the `c5c3-charts` HelmRepository source, and the
kind overlay ships the `placement-operator` HelmRelease suspended. Applying the
release manifest clears the suspension and lets Flux install the chart:

```bash
kubectl apply -f deploy/flux-system/releases/placement-operator.yaml
kubectl wait helmrelease/placement-operator -n placement-system \
  --for=condition=Ready --timeout=300s
```

The controller runs in `placement-system`; the Placement workload it manages runs
in `openstack`, the same controller-vs-workload split the keystone-operator uses.

## Step 2 — Create the Keystone identity objects

Placement authenticates every incoming token against Keystone using a service
user of its own, so that user, its project, and its role assignment have to exist
before the API pods start. Point the CLI at the devstack Keystone as `admin`:

```bash
export OS_AUTH_URL=https://keystone.127-0-0-1.nip.io:8443/v3
export OS_USERNAME=admin
export OS_PASSWORD=$(kubectl get secret keystone-admin -n openstack -o jsonpath='{.data.password}' | base64 -d)
export OS_PROJECT_NAME=admin
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
```

Pick a password for the service user — `openssl rand -base64 24` produces one —
and create the objects, pasting it at the `--password-prompt` prompt. Each
create uses `--or-show`, which returns the existing object where a plain create
would fail, and `role add` on an assignment that already exists is a no-op, so
the whole block is safe to re-run:

```bash
openstack --insecure project create --domain Default --or-show service
openstack --insecure role create --or-show service
openstack --insecure user create --domain Default --or-show --password-prompt placement
openstack --insecure role add --project service --user placement service
```

`--password-prompt` rather than `--password`: a password passed as a command-line
argument lands in argv, so it is written to the shell's history file and is
readable out of `/proc/<pid>/cmdline` by any local user running `ps auxww` while
the command runs.

Do not reach for `openstack ... show` to test whether an object exists first. In
this CLI a `show` of a missing identity object exits `0`, so a `show || create`
chain silently skips the create. `--or-show` is the idiom that works.

`--or-show` on `user create` returns the existing user untouched, so on a re-run
the password you type at the prompt is not the one Keystone holds. To rotate it
deliberately, use `openstack user set --password-prompt placement` and update the
Secret below in the same breath.

Store the password where the CR can read it. `--from-file=password=/dev/stdin`
keeps it on a pipe for the same reason `--password-prompt` does above. The two
prompts are independent, and nothing so far has compared what you type here
against what Keystone took, so issue a token with it and write the Secret only
if that succeeds. The write goes through `apply --server-side` because a bare
`create` exits `AlreadyExists` once the Secret is there, which is the state every
rotation starts from:

```bash
read -rs -p 'placement service-user password: ' PW; echo

if OS_USERNAME=placement OS_PROJECT_NAME=service OS_PASSWORD="$PW" \
    openstack --insecure token issue >/dev/null; then
  printf '%s' "$PW" | kubectl create secret generic placement-service-user -n openstack \
    --from-file=password=/dev/stdin --dry-run=client -o yaml | kubectl apply --server-side -f -
else
  echo 'token issue failed (error above) — Secret left untouched' >&2
fi
unset PW
```

The gate leaves `openstack`'s stderr on the terminal. `token issue` fails for
reasons that have nothing to do with the password: a fresh shell without the
`OS_AUTH_URL` and domain exports above, an unreachable Gateway, a `service` role
assignment that never got made. Suppressing the error would make each of them
read as a rejected password, and on a rotation `user set` has already changed the
credential in Keystone while the Secret still holds the old one.

`--server-side` keeps the password out of the object's metadata. Client-side
apply stores what it sent, `data.password` included, in the
`kubectl.kubernetes.io/last-applied-configuration` annotation, and
`kubectl describe secret` redacts the data block while printing annotations
verbatim.

The defaulting webhook fills the rest of the identity: username `placement`,
project `service`, `Default` for both domains, and `password` as the Secret key.
The CR therefore names nothing but the Secret.

## Step 3 — Apply the Placement CR

```yaml
# placement.yaml
apiVersion: placement.openstack.c5c3.io/v1alpha1
kind: Placement
metadata:
  name: placement
  namespace: openstack
spec:
  openStackRelease: "2025.2"
  deployment:
    replicas: 3
  image:
    repository: ghcr.io/c5c3/placement
    tag: "2025.2"
  database:
    clusterRef:
      name: openstack-db
    database: placement
    secretRef:
      name: placement-db
  cache:
    clusterRef:
      name: openstack-memcached
  keystoneEndpoint: http://keystone.openstack.svc.cluster.local:5000/v3
  serviceUser:
    secretRef:
      name: placement-service-user
  gateway:
    parentRef:
      name: openstack-gw
    hostname: placement.127-0-0-1.nip.io
    path: /
```

`keystoneEndpoint` is the cluster-local Service URL because keystonemiddleware
resolves it from inside the pod, not from your browser. The gateway hostname has
to be `placement.127-0-0-1.nip.io`: that is the one the devstack's `openstack-gw`
terminates TLS for, with a cert-manager certificate issued for that name.

The first reconcile pulls `ghcr.io/c5c3/placement:2025.2` onto the kind node. To
take that out of the critical path, load the image the way the Quick Start loads
Keystone's:

```bash
docker pull ghcr.io/c5c3/placement:2025.2
kind load docker-image ghcr.io/c5c3/placement:2025.2 --name cobaltcore
```

```bash
kubectl apply -f placement.yaml
kubectl wait placement/placement -n openstack \
  --for=condition=Ready --timeout=10m
```

The `placement-db-sync` Job migrates the schema before the API pods go ready,
which is what the 10-minute bound covers.

## Step 4 — Register the service and its endpoints

The API is now serving, but nothing in the catalog points at it, so a client that
discovers its endpoints from Keystone cannot find Placement. This is the half a
ControlPlane would project for you:

```bash
openstack --insecure service create --name placement placement
openstack --insecure endpoint create --region RegionOne placement internal \
  http://placement.openstack.svc.cluster.local:8778
openstack --insecure endpoint create --region RegionOne placement public \
  https://placement.127-0-0-1.nip.io:8443
```

Neither `service create` nor `endpoint create` takes `--or-show`, and both are
happy to create a duplicate row, so run each once. `openstack catalog list`
shows what is registered.

The internal endpoint is the in-cluster Service the operator owns (`placement` in
`openstack`, port 8778). The public one goes through the Gateway on the kind host
port from `KIND_HOST_PORT=8443`.

## Step 5 — Verify

`resource class list` is a read-only call that Placement answers only for an
authenticated request, so a listing proves the catalog entry, the gateway, and
the service user's token validation all work:

```bash
openstack --insecure resource class list
```

The output is a one-column table of the standard resource classes a fresh
Placement seeds, `VCPU`, `MEMORY_MB` and `DISK_GB` among them.

An `EndpointNotFound` here means Step 4 did not land. A `401` means the service
user cannot validate tokens: re-check the `service` role assignment from Step 2
and that `placement-service-user` holds the password Keystone actually has.

## See also

- [Placement CRD API Reference](../../reference/placement/placement-crd.md) —
  every `spec` field, including the ones this walkthrough leaves at their
  defaults.
- [Controller Events](../../reference/placement/placement-events.md) — what the
  operator emits while the CR converges.
- [Quick Start (ControlPlane)](../../quick-start-controlplane.md) — the managed
  path, where the identity objects and catalog rows of Steps 2 and 4 are
  projected instead of typed.

## Tested by

The standalone deployment path and the Gateway route this guide sets up are
asserted on the CI e2e kind cluster by these chainsaw suites:

```bash
chainsaw test --test-dir tests/e2e/placement/basic-deployment
chainsaw test --test-dir tests/e2e/placement/gateway-quick-start-smoke
```

::: details The Placement CR the basic-deployment suite applies
The suite shares the `openstack` namespace and the `openstack-db` /
`openstack-memcached` backing services with the rest of the parallel suite pool,
so its fixture carries isolation identifiers instead of the devstack names used
above: the CR is `placement-basic` on its own `placement_basic` schema, and it
authenticates as the pre-seeded `admin` service user from `keystone-admin` rather
than the `placement` user Step 2 creates. It declares no `spec.gateway`; the
`gateway-quick-start-smoke` suite covers that with a `placement-smoke` CR of its
own.

<<< @/../tests/e2e/placement/basic-deployment/00-placement-cr.yaml#placement-cr
:::
