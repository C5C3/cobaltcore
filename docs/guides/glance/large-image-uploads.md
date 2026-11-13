---
title: Large Image Uploads through the Gateway
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Large Image Uploads through the Gateway

A stock cloud image is measured in gibibytes, and moving one through a public
Gateway takes minutes to hours. This guide covers what has to hold for such a
transfer to finish: how to size the node-local staging bound that image imports
consume, what a deployment sees when an import breaches it, and which gateway
properties the upload path depends on.

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

## How image bytes reach the store

Two paths lead into the backing store, and only one of them touches local disk.

A **client upload** (`PUT /v2/images/{id}/file`, what `openstack image create
--file` issues) streams the request body through the gateway straight into the
store. Nothing is staged on the API pod, so its size is bounded by the client's
patience and by the gateway.

A **`web-download` import** (`POST /v2/images/{id}/import`) hands Glance a URI
and returns `202` at once. The API pod then fetches the whole image onto its
`staging` volume, the reserved `os_glance_staging_store`, and moves it into the
backing store only after the last byte has arrived. A second volume,
`tasks-work`, backs the reserved `os_glance_tasks_store`, to which no import
writes image bytes. Both are `emptyDir`s on the node filesystem, and
`spec.staging.sizeLimit` bounds each of them. The operator enables
`web-download` and `copy-image` only, so a client upload never stages.

The figure shows both paths as A and B, and the download through the image
cache as C. The bound of the next section is the eviction threshold on the
volume `staging`.

![Three paths of image bytes through one glance-api pod, and the three volumes on the disk of its node. Path A, a client upload with PUT /v2/images/{id}/file: the request body streams through the pod straight into the S3 store and touches no volume. Path B, a web-download import with POST /v2/images/{id}/import: the request returns at once, the pod fetches the whole image from a web server that the import filter admits onto the volume staging, an import plugin may convert it there into a second file, and after the last byte the image moves to the store. Path C, a download with GET /v2/images/{id}/file: the pod reads the image from the store, and with the image cache enabled it also writes it to the volume image-cache and serves later downloads of that image from there. The cache belongs to one replica, and a sidecar prunes it down to 80 percent of its size limit. The volumes staging, tasks-work and image-cache are emptyDirs. A sizeLimit is an eviction threshold, not a quota; periodic kubelet checks mean usage can exceed it before a pod is evicted. No import writes image bytes to tasks-work.](../../diagrams/service-glance-image-paths.svg)

## Sizing the staging bound

The bound is a ControlPlane setting projected onto the Glance child:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"glance":{"staging":{"sizeLimit":"40Gi"}}}}}'
kubectl rollout status deploy/controlplane-glance -n openstack
```

::: warning Set the bound on the ControlPlane, never on the projected child
The `controlplane-glance` Glance CR is **projected** by the c5c3-operator, so a
`spec.staging` you patch onto the child is reverted on the next reconcile. The
projection is unconditional in the other direction too: clearing
`services.glance.staging` on the `ControlPlane` removes the child's field, and
the glance operator's own default of `10Gi` applies again.
:::

Read back the values on both scratch volumes:

```bash
kubectl get deploy controlplane-glance -n openstack -o jsonpath='staging={.spec.template.spec.volumes[?(@.name=="staging")].emptyDir.sizeLimit}{"\n"}tasks-work={.spec.template.spec.volumes[?(@.name=="tasks-work")].emptyDir.sizeLimit}{"\n"}'
```

```
staging=40Gi
tasks-work=40Gi
```

Three properties decide the number:

- **Both volumes carry it.** One glance-api pod is expected to occupy at most
  twice the configured limit, so `40Gi` budgets 80Gi per replica.
- **Concurrent imports share it, and breaching it evicts the pod.** An
  `emptyDir` is per pod, and nothing accounts per import: two imports scheduled
  onto the same replica draw from one 40Gi volume. When their combined
  staging usage crosses the bound, the kubelet evicts that pod, killing both
  imports and all other transfers in flight on the replica. Size for expected
  concurrency as well as the largest single image.
- **Conversion needs headroom.** An import plugin that converts the downloaded
  image writes the converted copy alongside the original before replacing it, so
  a converting import can occupy roughly twice its image size in staging.

The operator's default of `10Gi` covers the stock distribution cloud images with
room to convert. Raise it for a deployment that imports disk images built for
databases or appliances, where a single qcow2 can exceed that on its own.

::: warning The bound applies retroactively on operator upgrade
A Glance that predates this block ran with unbounded scratch volumes. Upgrading
the operator sets the resolved `10Gi` on the existing `Deployment` and rolls
the pods, killing any in-flight import. It then evicts the pod if an import
exceeds `10Gi`.

The `staging` field reaches the `ControlPlane` schema in the same chart version
that introduces the bound. Either patch on this page
names a field the pre-upgrade CRD does not have: the API server prunes it, the
request succeeds having stored nothing, and `kubectl patch` carries no
`--validate` flag to turn that silence into an error. Quiesce the imports you
care about, upgrade, then patch once the field exists. That patch rolls the
pods a second time with the value you chose.
:::

Opting out sets `unbounded`, which renders both scratch volumes without a
`sizeLimit`, as they were before this field existed:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"glance":{"staging":{"sizeLimit":null,"unbounded":true}}}}}'
```

Use this escape hatch only when you accept the node-disk risk. An unbounded
volume puts nothing between one runaway `web-download` import and the node's
disk. If that disk fills, the kubelet can evict any pod on the node, including
non-Glance workloads. Setting it together with `sizeLimit` is rejected at admission,
which is why the patch above clears the one while setting the other.

### What the bound does not do

The kubelet uses `emptyDir.sizeLimit` as its eviction threshold. It evaluates
the bound on its periodic local-storage housekeeping pass (~10 s), and does not
enforce a filesystem quota.
Peak usage can exceed the bound by the data a writer appends within one pass;
an import that finishes inside one pass is never noticed. Leave headroom on the
node for the overshoot.

The setting also creates no scheduling reservation. The operator derives no
`resources.requests.ephemeral-storage` from it, so the scheduler does not know
what a glance-api pod may claim, and replicas or pods from separate `Glance`
CRs can co-schedule freely. Four pods at the default may reach 80 GiB of scratch
on one node. If the node crosses its eviction threshold, the kubelet ranks all
pods on it, including Glance and non-Glance workloads, for eviction. Size
nodes against `replicas × 2 × sizeLimit`. To make the scheduler account for it,
add `ephemeral-storage` to `spec.deployment.resources.requests`. The operator
fills the CPU and memory defaults per resource, so they stay beside it.

The setting also imposes no tenant quota. It caps local disk used by imports on
one pod. Glance quotas control how many images a project may create and how much
it may keep in the backend.

## When an import breaches the bound

Glance knows nothing about the bound and keeps writing. The kubelet notices on
its next local-storage housekeeping pass, and the sequence from there is fixed:

1. local-storage eviction evicts the glance-api pod;
2. the in-flight import dies with it, so its image never reaches `active`;
3. the Deployment replaces the pod and the API recovers without operator or
   human intervention.

The event stream records the eviction:

```bash
kubectl get events -n openstack --field-selector reason=Evicted
```

The pod's own status does not always show it. glance-api drains its requests on
SIGTERM and exits 0, so an evicted pod whose requests ended within the
eviction's two-second window shows phase `Succeeded` (`Completed` in
`kubectl get pods`) and no reason. A pod whose request outlived that window
shows `Failed` with reason `Evicted`.

The image record remains in a non-active status. Delete it, then retry with a
larger bound or a smaller image; nothing about the failed import is retried
automatically.

Two things are worth knowing before relying on this. An eviction takes the whole
pod, so a well-behaved import that happened to run on the same replica dies with
the oversized one. And with a single replica the API is briefly unreachable
while the replacement pod starts, which is one more reason to run more than one.

## Gateway requirements for the upload path

**The route timeout is raised to four hours.** The glance operator renders
`timeouts.request: "4h"` on the HTTPRoute it manages, so a conforming
implementation lets a transfer run far longer than its default allows:

```bash
kubectl get httproute controlplane-glance -n openstack \
  -o jsonpath='{.spec.rules[0].timeouts.request}{"\n"}'
```

No Glance or ControlPlane field configures this. The 15-second Envoy Gateway
default truncates legitimate image transfers. The Glance route uses four hours;
the identity and dashboard routes keep the default.

The operator sets a four-hour bound and does not disable it. `"0s"` is the
Gateway API spelling of a disabled timeout. The route
matches a bare `/` prefix, so it covers every Glance path, and the route timeout
is then the only request-duration cap in front of the API. An idle timeout is
insufficient because it resets on every byte, so a client trickling one byte at
a time never goes idle. Four hours clears any legitimate transfer while
still capping how long one stalled request holds its worker.

::: warning The route timeout bounds duration, not concurrency
The timeout does not protect a saturated worker pool. A glance-api pod serves
`processes × threads` concurrent requests. It defaults to two
(`--processes 2 --threads 1`, with uWSGI `harakiri` off unless
`spec.apiServer.uwsgi.harakiri` is set), so a three-replica
deployment has six request slots for the whole API. Requests that keep trickling
occupy a slot for the full four hours, and while every slot is occupied,
unrelated calls such as an image list queue behind them. Six concurrent
multi-gibibyte uploads over slow links can occupy those slots too.

Size the pool for expected concurrency within the deadline. The CRs let you
widen it with `spec.sizing.glance.api.replicas`, `.processes` and `.threads` on
the `ControlPlane`. From 2026.1, the counts are in the child's
`spec.apiServer.uwsgi`; earlier releases use `spec.apiServer.workers` for the
process count. A standalone `Glance` uses `spec.apiServer.uwsgi` `processes` and
`threads`. An Envoy Gateway `BackendTrafficPolicy` can shed
excess requests by capping concurrency per backend. That policy belongs to the
Gateway infrastructure and is not rendered by this operator.
:::

The stanza works only if the cluster's Gateway API CRDs know the field. It
entered the HTTPRoute schema after the versions the stack shipped earlier, so
`hack/deploy-infra.sh` pins `v1.6.2` of the standard channel; an older CRD
prunes `timeouts` silently, and the transfer is then cut at the implementation
default with nothing logged.

**A listener must match the hostname.** `spec.gateway.hostname` routes only if
the Gateway terminates TLS for that SNI hostname. The kind stack's Gateway
`openstack-gw` carries one listener per hostname, each with its own
cert-manager Certificate, including `https-glance-upload` for
`glance-upload.127-0-0-1.nip.io`, a hostname reserved for the large-upload
end-to-end suite to avoid racing the Quick Start smoke suite's route. Check
that the listener a hostname needs is programmed before blaming the upload:

```bash
kubectl get gateway openstack-gw -n openstack \
  -o jsonpath='{range .status.listeners[*]}{.name}{"\t"}{.conditions[?(@.type=="Programmed")].status}{"\n"}{end}'
```

**Body-size limits belong to the gateway stack.** Envoy Gateway caps no request
body by default, so a multi-gibibyte `PUT` reaches Glance intact. Other stacks
disagree: an NGINX-based ingress defaults to a 1 MiB body and answers `413`
within the first megabyte of an image. Fronting Glance with such a stack means
raising that limit there. Set it to the largest image the deployment accepts
and keep the gateway ceiling on request size. The operator writes no annotation
for it, and
`spec.gateway.annotations` is passed through to the HTTPRoute verbatim, so
whichever key that stack reads is yours to set.

**Imports need an egress path.** A `web-download` import makes the API pod an
HTTP client of whatever URI it is handed, which is a second surface next to the
upload path. `spec.importFiltering` decides which URIs are admissible (HTTPS on
port 443 by default), and the per-CR NetworkPolicy decides where the pod may
connect. Its auto-derived egress opens DNS, database, cache, and S3 backend
ports to any destination, so a mirror on another port needs an explicit
`additionalEgress` rule. See
[Enable the Glance Operator NetworkPolicy](./enable-glance-operator-networkpolicy.md)
for both layers.

## Verification

With the `OS_*` variables exported, push an image large enough to outlive any
default route timeout. 512 MiB is what the end-to-end suite uses:

```bash
dd if=/dev/urandom of=/tmp/large.img bs=1M count=512
time openstack --insecure image create --disk-format raw --container-format bare \
  --file /tmp/large.img large-image
openstack --insecure image show large-image -f value -c status -c size
```

The status is `active` and the size is `536870912`. A truncated transfer
surfaces as a client-side error from the gateway, or as an image stuck in
`queued`.

Imports take the staging path. Create the record, then hand Glance a URI:

```bash
openstack --insecure image create --disk-format raw --container-format bare big-import
openstack --insecure image import --method web-download \
  --uri https://mirror.example.com/cloud-image.raw big-import
```

The call returns immediately and the image reaches `active` once the download
and the move into the store have finished. Watch `openstack image show
big-import -f value -c status` for the transition. A synchronous 400 at import
time comes from the URI filter before bytes are staged. See
[Filter web-download Image Imports](./filter-web-download-imports.md).

Clean up:

```bash
openstack --insecure image delete large-image big-import
rm -f /tmp/large.img
```

## Standalone Glance, without a ControlPlane

Without a ControlPlane nothing projects the Glance child, so the same setting sits
on the `Glance` CR you own:

```yaml
apiVersion: glance.openstack.c5c3.io/v1alpha1
kind: Glance
metadata:
  name: glance
  namespace: openstack
spec:
  staging:
    sizeLimit: 40Gi
```

Everything else in this guide is unchanged: the operator resolves an unset field
to `10Gi`, sets the resolved value on both scratch volumes, and renders the
same four-hour route timeout on the HTTPRoute it creates from `spec.gateway`.

## See also

- [Glance CRD API Reference](../../reference/glance/glance-crd.md#stagingspec):
  the full `StagingSpec` contract and its admission rules.
- [ControlPlane CRD API Reference](../../reference/c5c3/controlplane-crd.md):
  `services.glance.staging` and the rest of the projected Glance surface.
- [Keystone CRD API Reference](../../reference/keystone/keystone-crd.md#httproute-resource-mapping):
  the shared HTTPRoute mapping, including the route timeout, which only the
  Glance operator sets.
- [Infrastructure Manifests](../../reference/infrastructure/infrastructure-manifests.md#glance-large-upload-listener):
  the kind stack's `https-glance-upload` listener and its certificate.

## Tested by

The upload path, the staged (`glance-direct`) upload that fills the bounded
staging volume, and the eviction that contains an oversized one are asserted
end-to-end on the CI e2e kind cluster by this chainsaw suite:

```bash
chainsaw test --test-dir tests/e2e/glance/gateway-large-upload
```
