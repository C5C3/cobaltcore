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

Follow that tutorial through the block-storage block of Step 4 and the
**Create a first volume** check in Step 7, so the projected `controlplane-cinder`
child is `Ready` in `openstack` with `nfs1` and `nfsbk` attached. The devstack
provides the `controlplane` and `nfs1` resources; the verification creates its
own `demo-cache-img` image and `demo-img-*` volumes.
:::

Keep the admin `OS_*` variables from the tutorial's token-issue step exported;
the `--all-projects` volume listing requires those credentials. Cinder must also
have a readable Glance endpoint. In this devstack, `services.glance` provides it
and projects `glanceEndpoint` into the Cinder child.

## What the cache changes

The first `create volume from image` on a backend leaves a second volume behind,
named `image-<image-id>`. Later creates from the same image on the same backend
clone that volume on the backend instead of pulling the image through Glance
again. The bound is per backend, because the clone is a backend operation: a
cached entry serves the backend it was written on and no other.

The cached volume belongs to the deployment's internal tenant. Cinder creates
it as the project and user named in `spec.internalTenant`. On a ControlPlane,
the `KeystoneService` registration publishes the service account's `projectID`
and `userID` on `status.account`, and the reconciler copies both to the child.
Read them back:

```bash
kubectl get cinder controlplane-cinder -n openstack \
  -o jsonpath='{.spec.internalTenant.projectID}{"\n"}{.spec.internalTenant.userID}{"\n"}'
```

Both outputs should be non-empty. If either is blank, wait for the Keystone
service-account registration to publish its IDs before proceeding.

Two consequences follow from that ownership. A cached volume is invisible in a
tenant's own volume list, and nothing a tenant deletes removes it.

## Enable it

`imageVolumeCache` sits on a backend entry, so the switch is per backend. Add it
to the `nfs1` entry:

::: warning Set storage on the ControlPlane, never on the projected children
The `controlplane-cinder` Cinder CR, the `CinderBackend` `nfs1` and the
`CinderBackupBackend` `nfsbk` are **projected** by the c5c3-operator. A child you
edit, or delete, by hand is reverted (or recreated) on the next reconcile.
Change `services.cinder` on the `ControlPlane` CR and let the operator project
it down; that block is the single source of truth for the projected storage.
:::

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

The operator re-renders the backend configuration and rolls out its volume
Deployment:

```bash
kubectl rollout status deploy/controlplane-cinder-volume-nfs1 -n openstack --timeout=5m
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

Use example names that are unused in this OpenStack environment; the cleanup
below deletes them. Create an image and check its status:

```bash
image_file=$(mktemp /tmp/cinder-cache.XXXXXX)
dd if=/dev/urandom of="$image_file" bs=1024 count=1024
openstack --insecure image create --disk-format raw --container-format bare \
  --file "$image_file" demo-cache-img
rm "$image_file"
openstack --insecure image show demo-cache-img -f value -c status
```

If image creation fails, remove the temporary file with `rm "$image_file"`.
Repeat the status check until the image is `active`. Keep
this shell open, then create a volume from it:

```bash
IMG=$(openstack --insecure image show demo-cache-img -f value -c id)
openstack --insecure volume create --size 1 --image "$IMG" demo-img-1
openstack --insecure volume show demo-img-1 -f value -c status
```

Repeat the status check until the volume is `available`, then inspect the cache:

```bash
openstack --insecure volume list --all-projects --name "image-$IMG"
```

There should be one entry named `image-$IMG`. The list uses `--all-projects`
because Cinder stores it under the internal tenant; a tenant's own volume list
omits it. Create a second volume and check that it reuses the same cache entry:

```bash
openstack --insecure volume create --size 1 --image "$IMG" demo-img-2
openstack --insecure volume show demo-img-2 -f value -c status
```

Repeat the status check until the second volume is `available`, then list the
cache entry again:

```bash
openstack --insecure volume list --all-projects --name "image-$IMG"
```

The list should still contain one entry.

Delete the test volumes. If one was not created, omit it; if Cinder rejects a
delete while a volume is still creating, wait for its state to settle and retry:

```bash
openstack --insecure volume delete demo-img-1 demo-img-2
openstack --insecure volume list
```

Repeat the list until both test volumes are gone. If `IMG` was not set earlier,
recover the image ID first:

```bash
IMG=$(openstack --insecure image show demo-cache-img -f value -c id)
```

The cache belongs to the deployment, so deleting the test volumes does not
remove it. Find the ID of the entry named `image-$IMG` and replace
`CACHE_VOLUME_ID` with that ID:

```bash
openstack --insecure volume list --all-projects --name "image-$IMG"
openstack --insecure volume delete CACHE_VOLUME_ID
openstack --insecure volume list --all-projects --name "image-$IMG"
```

Repeat the list until the cache volume is gone, then delete the image. If the
cache entry was not created, skip its deletion.

```bash
openstack --insecure image delete "$IMG"
```

On the ControlPlane's `nfs1` entry, set `enabled: false` or remove
`imageVolumeCache` to turn the cache off. Cinder keeps cache volumes already
written, so delete them by ID as above.

## Standalone Cinder, without a ControlPlane

Use this path when Cinder is managed directly and no ControlPlane projects the
service account. Set `spec.internalTenant` on the existing Cinder first, then
enable the cache on its existing `CinderBackend`. Merge these fragments into
the current specs; do not apply them as complete manifests.

Read the Keystone project and user IDs. Replace `service` or `cinder` if your
Keystone account uses different names:

```bash
openstack --insecure project show service -f value -c id
openstack --insecure user show cinder -f value -c id
```

Edit the Cinder object named `cinder` in this example:

```bash
kubectl edit cinder cinder -n openstack
```

Add these fields to its existing `spec`:

```yaml
spec:
  internalTenant:
    projectID: "<project-id>"
    userID: "<user-id>"
```

Both values must be IDs, not names. Cinder passes them to the volume API without
resolving names through Keystone, so a name reaches the backend as a project
that does not exist.

Edit the existing backend, replacing `BACKEND_NAME` with its resource name:

```bash
kubectl edit cinderbackend BACKEND_NAME -n openstack
```

Add this field to its existing `spec`, keeping its current driver settings:

```yaml
spec:
  imageVolumeCache:
    enabled: true
    maxSizeGB: 10
    maxCount: 5
```

If the referenced Cinder exists without `spec.internalTenant` when you create or
update the backend, the webhook warns but still admits it. If the backend arrives
before its Cinder, the webhook cannot inspect the parent and emits no warning at
that point. In either order, the cache stays inactive until the Cinder has both
IDs.

## See also

- [CinderBackend CRD API Reference](../../reference/cinder/cinder-backend-crd.md#imagevolumecachespec):
  the three fields, the rendered keys, and the eviction rule.
- [Cinder CRD](../../reference/cinder/cinder-crd.md#internaltenantspec): why
  both internal-tenant fields are IDs.
- [ControlPlane CRD API Reference](../../reference/c5c3/controlplane-crd.md#cinderimagevolumecachespec):
  the curated `imageVolumeCache` entry and the derived `internalTenant`.
- [Attach an NFS Backend to Cinder](./attach-an-nfs-backend.md): the backend
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

<<< ../../../tests/e2e/c5c3/full-controlplane-keystone/00-controlplane-cr.yaml#controlplane-cr
:::
