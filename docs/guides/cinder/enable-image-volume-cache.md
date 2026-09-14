---
title: Enable the Image-Volume Cache
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Enable the Image-Volume Cache

Every `create volume from image` pulls the image through Glance, converts it, and
writes it onto the backend. Booting twenty instances off one image means twenty
of those downloads, all for bytes that have not changed since the first.
`imageVolumeCache` keeps the first one as a volume on the backend and clones it
for the rest. This guide turns the cache on for the devstack's `nfs1` backend,
watches it engage, and cleans the cached volume up again.

For the full field reference, see the
[CinderBackend CRD API Reference](../../reference/cinder/cinder-backend-crd.md#imagevolumecachespec)
and the [Cinder CRD](../../reference/cinder/cinder-crd.md#internaltenantspec).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_NFS=true make deploy-infra
```

Follow that tutorial through the block-storage block of Step 3 and the
**Create a first volume** check in Step 6, so the projected `controlplane-cinder`
child is `Ready` in `openstack` with `nfs1` and `nfsbk` attached. Every resource
name in the examples below is one that devstack produces.
:::

::: warning Set storage on the ControlPlane, never on the projected children
The `controlplane-cinder` Cinder CR, the `CinderBackend` `nfs1` and the
`CinderBackupBackend` `nfsbk` are **projected** by the c5c3-operator. A child you
edit, or delete, by hand is reverted (or recreated) on the next reconcile.
Change `services.cinder` on the `ControlPlane` CR and let the operator project
it down; that block is the single source of truth for the projected storage.
:::

- The `OS_*` environment variables from the tutorial's token-issue step, so the
  `openstack` client reaches the ControlPlane's Keystone.
- A Glance the block-storage service can read images through. The devstack's
  `services.glance` block provides it, and it is what gives the Cinder child its
  `glanceEndpoint`.

## What the cache changes

The first `create volume from image` on a backend leaves a second volume behind,
named `image-<image-id>`. Later creates from the same image on the same backend
clone that volume on the backend instead of pulling the image through Glance
again. The bound is per backend, because the clone is a backend operation: a
cached entry serves the backend it was written on and no other.

The cached volume belongs to the deployment rather than to the tenant whose
request populated it, so cinder creates it as the project and user
`spec.internalTenant` names on the Cinder. On a ControlPlane that block is
derived, not declared: the `KeystoneService` registration publishes the service
account's `projectID` and `userID` on its `status.account`, and the reconciler
copies both onto the child as soon as they are there. Read them back:

```bash
kubectl get cinder controlplane-cinder -n openstack \
  -o jsonpath='{.spec.internalTenant}' | jq
```

Two consequences follow from that ownership. A cached volume is invisible in a
tenant's own volume list, and nothing a tenant deletes removes it.

## Enable it

`imageVolumeCache` sits on a backend entry, so the switch is per backend. Add it
to the `nfs1` entry:

```bash
kubectl edit controlplane controlplane -n openstack
```

```yaml
spec:
  services:
    cinder:
      backends:
        - name: nfs1
          type: NFS
          nfs:
            server: nfs-server.openstack.svc.cluster.local
            path: /volumes
          imageVolumeCache:
            enabled: true
            maxSizeGB: 10
            maxCount: 5
```

The switch is the `enabled` field itself, so a block carrying `enabled: false`
keeps the two bounds configured in a ControlPlane that runs without the cache.
Both bounds are optional and independent, with a minimum of 1 each; whichever is
reached first evicts the least recently used entry. Leaving both unset caches
without a bound.

The operator re-renders the backend's section and rolls its volume pod:

```bash
kubectl rollout status deploy/controlplane-cinder-volume-nfs1 -n openstack
```

The rendered section lives in the content-hashed Secret
`controlplane-cinder-backend-nfs1-<hash>`, which only this backend's Deployment
mounts. Read it off the pod:

```bash
kubectl exec -n openstack deploy/controlplane-cinder-volume-nfs1 -- \
  cat /etc/cinder/backends.conf.d/backend.conf
```

```ini
[nfs1]
backend_host = controlplane-cinder
image_volume_cache_enabled = true
image_volume_cache_max_count = 5
image_volume_cache_max_size_gb = 10
...
```

The two `image_volume_cache_max_*` keys are written only when the matching field
is set. `image_volume_cache_enabled = true` on its own is the cache running
unbounded.

## Verification

Upload a small raw image, create two volumes from it, and count the cache entries
after each. A cache that never engaged leaves none, and one that engaged twice
leaves two:

```bash
dd if=/dev/urandom of=/tmp/cache.img bs=1024 count=1024
openstack --insecure image create --disk-format raw --container-format bare \
  --file /tmp/cache.img demo-cache-img
IMG=$(openstack --insecure image show demo-cache-img -f value -c id)
```

The first create is the one that downloads:

```bash
openstack --insecure volume create --size 1 --image "$IMG" demo-img-1
openstack --insecure volume show demo-img-1 -c status -f value
openstack --insecure volume list --all-projects -f value -c Name | grep -cx "image-$IMG"
```

The count is `1`. `--all-projects` is what makes it visible: the cache volume
belongs to the internal tenant, not to the project that issued the create, so a
plain `volume list` never shows it.

The second create clones it:

```bash
openstack --insecure volume create --size 1 --image "$IMG" demo-img-2
openstack --insecure volume show demo-img-2 -c status -f value
openstack --insecure volume list --all-projects -f value -c Name | grep -cx "image-$IMG"
```

The count is still `1`. A `2` would mean the second request downloaded the image
again and wrote its own entry.

Clean up. The two tenant volumes go the usual way, and the cache volume has to be
deleted by id, since nothing above owns it:

```bash
openstack --insecure volume delete demo-img-2 demo-img-1
CACHE_ID=$(openstack --insecure volume list --all-projects -f value -c ID -c Name \
  | awk -v n="image-$IMG" '$2 == n {print $1}')
openstack --insecure volume delete "$CACHE_ID"
openstack --insecure image delete demo-cache-img
rm -f /tmp/cache.img
```

Clearing `imageVolumeCache` on the entry turns the cache off again on the next
rollout. It leaves the entries already written behind, which is the same delete
by id.

## Standalone Cinder, without a ControlPlane

Without a ControlPlane nothing derives the internal tenant from a registration,
so you supply it on the `Cinder` CR alongside the cache block on the backend:

```yaml
apiVersion: cinder.openstack.c5c3.io/v1alpha1
kind: Cinder
metadata:
  name: cinder
  namespace: openstack
spec:
  internalTenant:
    projectID: 8a1c4c0e7f5b4d2a9c6e3f8b1d7a4c52
    userID: 3f9d2b7c1a8e4f60b5d3c7a9e2b6f841
```

Both fields are required and both are IDs, not names. Cinder passes them to the
volume API without resolving them through Keystone, so a name reaches the backend
as a project that does not exist. Read the real values off Keystone:

```bash
openstack --insecure project show service -f value -c id
openstack --insecure user show cinder -f value -c id
```

A backend that enables the cache on a Cinder carrying no `spec.internalTenant` is
admitted, with an admission warning. The two CRs are applied independently, so
GitOps ordering may present the backend first. Until the block
is there the cache stays inactive.

## See also

- [CinderBackend CRD API Reference](../../reference/cinder/cinder-backend-crd.md#imagevolumecachespec) —
  the three fields, the rendered keys, and the eviction rule.
- [Cinder CRD](../../reference/cinder/cinder-crd.md#internaltenantspec) — why
  both internal-tenant fields are IDs.
- [ControlPlane CRD API Reference](../../reference/c5c3/controlplane-crd.md#cinderimagevolumecachespec) —
  the curated `imageVolumeCache` entry and the derived `internalTenant`.
- [Attach an NFS Backend to Cinder](./attach-an-nfs-backend.md) — the backend
  entry this block hangs off.

## Tested by

A create-from-image that leaves one cache entry, a second create that clones it
instead of adding another, and the delete-by-id above are asserted end-to-end on
the CI e2e kind cluster by this chainsaw suite:

```bash
chainsaw test --test-dir tests/e2e/c5c3/full-controlplane-keystone
```

::: details The ControlPlane fixture the suite applies
The fixture is isolation-named: the `ControlPlane` is called
`controlplane-keystone`, so its projected children carry that prefix
(`controlplane-keystone-cinder`, `controlplane-keystone-ovn`) where the
walkthrough above uses the `controlplane` / `controlplane-cinder` names the
devstack produces. Its `services.cinder` block is the one this guide edits: the
`nfs1` entry carries `imageVolumeCache.enabled: true`, with no bounds set, beside
the `nfsbk` backup target. The backend entry names are the same ones the devstack
uses, because cinder keys every volume by the backend it was created on and the
entry name reaches the satellite unprefixed.

<<< @/../tests/e2e/c5c3/full-controlplane-keystone/00-controlplane-cr.yaml#controlplane-cr
:::
