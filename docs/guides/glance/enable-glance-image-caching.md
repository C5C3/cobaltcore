---
title: Enable Glance Image Caching
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Enable Glance Image Caching

Without an image cache, every image download Glance serves reads from the object
store. Booting fifty instances from the same image means fifty reads for bytes
that have not changed. `spec.imageCache` lets each API pod keep copies of images
it has served on node-local disk, so a repeat download can avoid the store when
it reaches a pod that already has the image. This guide covers what that saves,
how to size the bound it needs, and how to tell a warm cache from a cold one.

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the projected
`controlplane-glance` Glance child is `Ready` in the `openstack` namespace, its
`default` S3 store is attached, and the image API answers on
`https://glance.127-0-0-1.nip.io:8443`. Every resource name below is one that
devstack produces.
:::

- The `OS_*` environment variables from the tutorial's token-issue step, so the
  `openstack` client reaches the ControlPlane's Keystone.

## What the cache changes

`GlanceBackend` supports one store type, `S3`. Without the image cache, the API
pod opens a connection to the store for each download, streams the object back,
and pays that latency and egress every time. A boot storm of N instances from
one image is N such reads.

With the cache on, a download to a replica without the image reads it from the
store and writes it to that replica's cache directory. Later downloads routed to
that replica can be served from the local copy while it remains cached. Other
replicas still read from the store until their own caches contain the image.

The figure shows a download as path C. The dashed parts exist only while the
cache is enabled.

![Three paths of image bytes through one glance-api pod, and the three volumes on the disk of its node. Path A, a client upload with PUT /v2/images/{id}/file: the request body streams through the pod straight into the S3 store and touches no volume. Path B, a web-download import with POST /v2/images/{id}/import: the request returns at once, the pod fetches the whole image from a web server that the import filter admits onto the volume staging, an import plugin may convert it there into a second file, and after the last byte the image moves to the store. Path C, a download with GET /v2/images/{id}/file: the pod reads the image from the store, and with the image cache enabled it also writes it to the volume image-cache and serves later downloads of that image from there. The cache belongs to one replica, and a sidecar prunes it down to 80 percent of its size limit. The volumes staging, tasks-work and image-cache are emptyDirs. A sizeLimit is an eviction threshold, not a quota; periodic kubelet checks mean usage can exceed it before a pod is evicted. No import writes image bytes to tasks-work.](../../diagrams/service-glance-image-paths.svg)

Two properties shape what that is worth:

**The cache is per replica.** Each pod fills its own, so an image is fetched
from the store once per replica that serves it, and a download is a hit only
when it lands on a replica that already holds the image. A three-replica
deployment pulls a popular image three times before every pod has it, and
claims three times the configured bound in node disk. Running more replicas for
availability therefore costs cache efficiency.

**Every rollout starts it cold.** The cache is an `emptyDir`, so a config
change, a release upgrade, a node drain, or an eviction discards it. Nothing
warms it in advance, and no CR field changes that. Glance can serve the bytes
for a warm cache hit without reading the image from the object store, but the
request still needs the API and image metadata. A cache miss needs the store, so
a local cache does not guarantee that Glance as a whole remains available
during an outage.

## Enable it

The cache is a ControlPlane setting projected onto the Glance child. The block's
presence is the switch, so an empty block is enough to turn it on at the
operator defaults (`10Gi`, pruned every `5m`):

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"glance":{"imageCache":{"sizeLimit":"20Gi","maintenanceInterval":"5m"}}}}}'
kubectl rollout status deploy/controlplane-glance -n openstack
```

::: warning Set the cache on the ControlPlane, never on the projected child
The `controlplane-glance` Glance CR is **projected** by the c5c3-operator, so a
`spec.imageCache` you patch onto the child is reverted on the next reconcile.
The projection is unconditional in the other direction too: clearing
`services.glance.imageCache` on the `ControlPlane` removes the child's field,
which switches the cache off on the next rollout and is how you revert this
guide.
:::

Enabling, resizing, and retuning each cost one rollout. The pod template gains
the volume and the sidecar in the same reconcile that updates the configuration
ConfigMap, so the two changes do not restart the pods twice.

Read back what the pods actually got:

```bash
kubectl get deploy controlplane-glance -n openstack -o jsonpath='bound={.spec.template.spec.volumes[?(@.name=="image-cache")].emptyDir.sizeLimit}{"\n"}containers={range .spec.template.spec.containers[*]}{.name}{" "}{end}{"\n"}'
```

```
bound=20Gi
containers=glance-api cache-maintenance
```

Confirm the service is healthy again on the ControlPlane's per-service
condition. Its aggregate `Ready` also covers every other service, so it answers
a different question:

```bash
kubectl get controlplane controlplane -n openstack \
  -o jsonpath='{.status.conditions[?(@.type=="GlanceReady")].status}{" "}{.status.conditions[?(@.type=="GlanceReady")].reason}{"\n"}'
```

## Sizing the bound

`sizeLimit` bounds the cache volume; the threshold of the pruner is 80% of it,
and the API does not check it when it writes. The operator renders
`image_cache_max_size` as `sizeLimit / 10 * 8`, so a `20Gi` bound gives the
pruner `17179869184` bytes to prune down to and leaves the remaining 20% as
headroom. Glance's pruner only prunes down to that threshold, and only when the
maintenance loop runs it, so the cache can sit above the mark between passes.
The headroom gives writes room before the `emptyDir` eviction threshold. Large
or concurrent downloads can still exceed it before the kubelet's next check.

Three numbers decide the value:

- **Size for the working set.** The cache is worth its disk for images a
  fleet boots repeatedly. Size it around those, and accept that a
  rarely used image gets pruned out and re-fetched.
- **Replicas multiply it.** Every replica holds its own copy, so
  `spec.sizing.glance.api.replicas` scales the node-disk cost linearly, and the
  scheduler is not told about it: the operator derives no
  `resources.requests.ephemeral-storage` from this field, just as it derives
  none from `spec.staging`. When staging is bounded, both scratch volumes use
  its resolved limit (`10Gi` by default); `spec.staging.unbounded` removes that
  bound. Across the deployment, the configured emptyDir limits total
  `replicas × (2 × resolved staging limit + resolved image-cache limit)`.
  The kubelet checks emptyDir usage periodically, so actual use can exceed a
  limit between checks. Size each node for the replicas it may host, plus that
  overshoot and other node-local writes.
- **Large downloads can outrun pruning.** Glance writes a download under
  `incomplete/` before it becomes a prunable cache entry, and the size poll
  excludes that directory. A single image larger than `image_cache_max_size` is
  cached in full before a later pass can prune it. Keep enough space for the
  largest image and concurrent downloads; the kubelet can evict the pod if a
  download exceeds `emptyDir.sizeLimit` before its next check.

`maintenanceInterval` sets the cadence for passes while the cache is below the
threshold. It bounds how long a stalled entry remains. The sidecar's 30-second
size poll starts a pass when it observes reclaimable cache entries above
`image_cache_max_size`, regardless of the interval. It can take up to 30
seconds to observe that threshold, and it cannot prune in-progress downloads.
The interval's floor is `1m` because every scheduled tick walks the cache
directory and its sqlite index.

## Verification

With the `OS_*` variables exported, upload an image and download it twice. The
cache is written on the download path, so the upload alone leaves the directory
empty:

```bash
dd if=/dev/urandom of=/tmp/cache-demo.img bs=1048576 count=64
openstack --insecure image create --disk-format raw --container-format bare \
  --file /tmp/cache-demo.img cache-demo
openstack --insecure image save --file /tmp/cache-demo.out cache-demo
IMAGE_ID=$(openstack --insecure image show cache-demo -f value -c id)
echo "Image ID: ${IMAGE_ID}"
```

The download landed on one replica, and only that pod cached it, so look at all
of them. Glance completes the cache entry after it streams the final byte to the
client, so allow up to a minute for the file to appear:

```bash
PODS=$(kubectl get pods -n openstack \
  -l app.kubernetes.io/instance=controlplane-glance -o name)
CACHED_ON=""
for _ in $(seq 1 12); do
  for pod in $PODS; do
    listing=$(kubectl exec -n openstack "${pod#pod/}" -c glance-api -- \
      ls -1 /var/lib/glance/image-cache 2>/dev/null || true)
    if printf '%s\n' "$listing" | grep -qx "$IMAGE_ID" \
      && printf '%s\n' "$listing" | grep -qx 'cache.db'; then
      CACHED_ON="${pod#pod/}"
      break 2
    fi
  done
  sleep 5
done
if [ -z "$CACHED_ON" ]; then
  echo "ERROR: image ${IMAGE_ID} did not appear in any replica's cache." >&2
  exit 1
fi
echo "Cached on pod ${CACHED_ON}:"
kubectl exec -n openstack "$CACHED_ON" -c glance-api -- \
  ls -1 /var/lib/glance/image-cache
```

One pod lists a file named after the image ID the command above printed, next to
`cache.db`, the metadata the sqlite driver keeps inside the cache directory:

```
2b1c9f3a-4e77-4a1c-9f0d-7c2e5b8a9d41
cache.db
```

Download the image once more. It hits the cache when it lands on that same
replica, and returns the same bytes either way:

```bash
openstack --insecure image save --file /tmp/cache-demo.again cache-demo
if cmp -s /tmp/cache-demo.img /tmp/cache-demo.out \
  && cmp -s /tmp/cache-demo.img /tmp/cache-demo.again; then
  echo "Both downloads match the uploaded image."
else
  echo "ERROR: a downloaded image differs from the uploaded image." >&2
  exit 1
fi
```

Clean up:

```bash
openstack --insecure image delete cache-demo
rm -f /tmp/cache-demo.img /tmp/cache-demo.out /tmp/cache-demo.again
```

## Reading the maintenance loop

The `cache-maintenance` sidecar runs `glance-cache-pruner` and
`glance-cache-cleaner` on schedule and whenever its 30-second size poll finds
reclaimable cache entries above `image_cache_max_size`. The poll does not see
in-progress downloads, so it cannot guarantee that concurrent writes stay
within the `emptyDir` bound. The sidecar logs are the only place either
command's output appears:

```bash
kubectl logs -n openstack deploy/controlplane-glance -c cache-maintenance --tail=30
```

No failure of a maintenance pass takes the pod down. The loop prints
`glance-cache-maintenance failed: 1 consecutive, cache at <n> KiB of <m>` on
stderr and tries again on the next pass, and it keeps doing that however long
the failure lasts. The sidecar shares the pod with `glance-api`. If it exits,
the container restarts, the pod becomes `NotReady`, and the Service drops that
API replica. A binary or shared-configuration failure can affect every replica
at once; a corrupted sqlite index can affect only its owning replica. A
degraded cache must not become an immediate Glance outage.

Watch the consecutive count, because absorbing the failure only delays the
outage. `1 consecutive` on an otherwise quiet log is a transient, and the
matching `glance-cache-maintenance recovered: after <n> consecutive failures`
line says it cleared. A count that keeps climbing means the pruner cannot run in
that pod. Nothing else enforces `image_cache_max_size`, so that replica's cache
can grow to its `emptyDir` bound and the kubelet can evict the pod under
ephemeral-storage pressure. That eviction does not pass through the eviction
API, so the `PodDisruptionBudget` does not stagger it. If a shared failure
affects all replicas, they can cross their identical bounds together, then
refill and go again. Meanwhile the `Glance` CR shows only
`DeploymentReady=False` with reason `WaitingForDeployment`; nothing in its
status names the cache. **Alert on
`glance-cache-maintenance failed`.** It is the only place this failure is
visible before the evictions start.

A `glance-cache-maintenance unmeasured` line means the size poll failed. The
loop still runs a pass and does not treat an unreadable directory as an empty
cache. `du`'s own error is on the line above it.

That is the whole observability story. The operator exports no Prometheus
metrics for cache hits, cache size, or pruner runs, emits no event, and no
status condition tracks the cache: these logs are what you have. To answer "is
the cache doing anything", compare download latency, or count the files under
the cache directory with the `kubectl exec` loop above.

## Standalone Glance, without a ControlPlane

Without a ControlPlane nothing projects the Glance child, so the same block sits
on the `Glance` CR you own:

```yaml
apiVersion: glance.openstack.c5c3.io/v1alpha1
kind: Glance
metadata:
  name: glance
  namespace: openstack
spec:
  imageCache:
    sizeLimit: 20Gi
    maintenanceInterval: 5m
```

Everything else in this guide is unchanged: the operator resolves unset fields
to `10Gi` and `5m`, derives the pruner threshold at 80% of the bound, mounts the
volume into `glance-api`, and runs the same sidecar next to it. Removing the
block again disables the cache on the next rollout.

## See also

- [Glance CRD API Reference](../../reference/glance/glance-crd.md#imagecachespec):
  the full `ImageCacheSpec` contract, the rendered config keys, and the reserved
  `cache` middleware name.
- [Glance Operator](../../reference/glance/index.md#design-decisions): the
  driver is `sqlite` and the cache directory is a pod-local `emptyDir` volume.
- [ControlPlane CRD API Reference](../../reference/c5c3/controlplane-crd.md):
  `services.glance.imageCache` and the rest of the projected Glance surface.
- [Large Image Uploads through the Gateway](./large-image-uploads.md): the
  staging bound the node-sizing formula above shares its node with.

## Tested by

The suite asserts the cache shape, a cached download using Glance's
`cached_images.hits` counter, a pruner pass that brings an over-threshold cache
back under its bound without kubelet eviction, and the revert. Unsetting the
block removes the sidecar and volume from the live Deployment. These checks run
end-to-end on the CI e2e kind cluster with a `256Mi` bound and a `1m` interval,
so the arithmetic plays out inside a test:

```bash
chainsaw test --test-dir tests/e2e/glance/image-cache
```
