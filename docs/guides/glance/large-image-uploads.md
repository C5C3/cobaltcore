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

Also export the `OS_*` environment variables from the tutorial's token-issue
step; the `openstack` client needs them to authenticate to the ControlPlane's
Keystone.

## How image bytes reach the store

Two paths lead into the backing store, and only one of them touches local disk.

A **client upload** (`PUT /v2/images/{id}/file`, what `openstack image create
--file` issues) streams the request body through the gateway straight into the
store. It does not use local staging, so `spec.staging.sizeLimit` does not cap
it. Glance, client, or gateway settings may impose other limits.

A **`web-download` import** (`POST /v2/images/{id}/import`) hands Glance a URI
and returns `202` at once. The API pod then fetches the whole image onto its
`staging` volume, the reserved `os_glance_staging_store`, and moves it into the
backing store only after the last byte has arrived. A second volume,
`tasks-work`, backs the reserved `os_glance_tasks_store`, to which no import
writes image bytes. Both are `emptyDir`s on the node filesystem, and
`spec.staging.sizeLimit` bounds each of them. The operator enables
`web-download` and `copy-image`; `glance-direct` is disabled because it stages
data on the API pod's local disk, which is not shared across replicas. A client
`PUT /file` upload bypasses the import-method pipeline and writes to the backing
store.

The figure shows both paths as A and B, and the download through the image
cache as C. The bound of the next section is the eviction threshold on the
volume `staging`.

![Three paths of image bytes through one glance-api pod, and the three volumes on the disk of its node. Path A, a client upload with PUT /v2/images/{id}/file: the request body streams through the pod straight into the S3 store and touches no volume. Path B, a web-download import with POST /v2/images/{id}/import: the request returns at once, the pod fetches the whole image from a web server that the import filter admits onto the volume staging, an import plugin may convert it there into a second file, and after the last byte the image moves to the store. Path C, a download with GET /v2/images/{id}/file: the pod reads the image from the store, and with the image cache enabled it also writes it to the volume image-cache and serves later downloads of that image from there. The cache belongs to one replica, and a sidecar prunes it down to 80 percent of its size limit. The volumes staging, tasks-work and image-cache are emptyDirs. A sizeLimit is an eviction threshold, not a quota; periodic kubelet checks mean usage can exceed it before a pod is evicted. No import writes image bytes to tasks-work.](../../diagrams/service-glance-image-paths.svg)

## Sizing the staging bound

The bound is a ControlPlane setting projected onto the Glance child:

```bash
set -euo pipefail
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"glance":{"staging":{"sizeLimit":"40Gi"}}}}}'
projected=
for _ in $(seq 1 60); do
  projected="$(kubectl get deploy controlplane-glance -n openstack \
    -o 'jsonpath={.spec.template.spec.volumes[?(@.name=="staging")].emptyDir.sizeLimit}')"
  [ "$projected" = "40Gi" ] && break
  sleep 5
done
if [ "$projected" != "40Gi" ]; then
  echo "Timed out waiting for the ControlPlane to project staging.sizeLimit=40Gi" >&2
  exit 1
fi
kubectl rollout status deploy/controlplane-glance -n openstack --timeout=5m
```

Wait for the projected value before checking the rollout. Otherwise
`kubectl rollout status` can succeed for the previous, already-stable
Deployment.

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

- **Both volumes carry it.** One glance-api pod has a nominal scratch budget of
  twice the configured limit, so `40Gi` budgets 80Gi per replica before the
  periodic-check overshoot described below.
- **Concurrent imports share the staging volume.** An `emptyDir` is per pod, and
  nothing accounts per import: two imports scheduled onto the same replica draw
  from one 40Gi volume. If a periodic local-storage check finds their combined
  staging usage above the limit, the kubelet can evict that pod, killing both
  imports and all other transfers in flight on the replica. Size for expected
  concurrency as well as the largest single image.
- **Conversion needs headroom.** An import plugin that converts the downloaded
  image writes the converted copy alongside the original before replacing it, so
  a converting import can occupy roughly twice its image size in staging.

The operator default is `10Gi`. Compare it with the largest image your
deployment imports; a converting import may need about twice the source image
size on staging. Raise the limit when that workload needs more space.

**Before upgrading a Glance that predates this field, quiesce in-flight
imports.** Its scratch volumes are unbounded. The upgrade sets the resolved
`10Gi` on the existing `Deployment` and rolls the pods, killing any in-flight
import. The kubelet can then evict the pod if an import exceeds `10Gi`.

The `ControlPlane` CRD must include `services.glance.staging` before either
patch can store it. With an older CRD, the API server prunes the unknown field,
the request succeeds without storing it, and `kubectl patch` has no
`--validate` flag to turn that silence into an error. Quiesce the imports you
care about, upgrade the operator and CRDs, then patch once the field exists.
That patch rolls the pods a second time with the value you chose.

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

Glance knows nothing about the bound and keeps writing. If a local-storage
housekeeping pass finds usage above the limit, the kubelet may evict the pod.
An import that finishes before that check can escape eviction. When eviction
occurs, the sequence is:

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

No Glance or ControlPlane field configures this. Envoy Proxy's 15-second
default request timeout, used by Envoy Gateway, truncates transfers that last
longer. The Glance route uses four hours; the identity and dashboard routes
keep the default.

The operator sets a four-hour bound and does not disable it. `"0s"` is the
Gateway API spelling of a disabled timeout. The route
matches a bare `/` prefix, so it covers every Glance path. An idle timeout is
insufficient because it resets on every byte, so a client trickling one byte at
a time never goes idle. Four hours allows long transfers while still bounding
how long one stalled request holds its worker.

**The route timeout leaves concurrency unchanged.** A glance-api pod serves
`processes × threads` concurrent requests. It defaults to two
(`--processes 2 --threads 1`, with uWSGI `harakiri` off unless
`spec.apiServer.uwsgi.harakiri` is set), so a three-replica
deployment has six request slots for the whole API. Requests that keep trickling
occupy a slot for the full four hours, and while every slot is occupied,
unrelated calls such as an image list queue behind them. Six concurrent
multi-gibibyte uploads over slow links can occupy those slots too.

Size the pool for expected concurrency within the deadline. The CRs let you
widen it with `spec.sizing.glance.api.replicas`, `.processes` and `.threads` on
the `ControlPlane`; the c5c3 operator projects the process and thread counts to
the child's `spec.apiServer.uwsgi`. A standalone `Glance` uses
`spec.apiServer.uwsgi.processes` and `.threads` directly.
`spec.apiServer.workers` is deprecated and has no effect on supported releases.
An Envoy Gateway
[`BackendTrafficPolicy`](https://gateway.envoyproxy.io/docs/tasks/traffic/circuit-breaker/)
can cap concurrent requests to a backend. That policy belongs to the Gateway
infrastructure and is not rendered by this operator.

The stanza works only if the cluster's Gateway API CRDs know the field. It
entered the HTTPRoute schema after older releases, so an older CRD can prune
`timeouts` silently and leave Envoy Proxy's default request timeout in effect.
`hack/deploy-infra.sh` currently defaults `GATEWAY_API_VERSION` to `v1.6.3` of
the standard channel.

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

**Body-size limits belong to the gateway stack.** The kind stack configures no
request-body cap, and the large-upload e2e suite proves that a 512 MiB `PUT`
reaches Glance. Other Gateway implementations may impose a cap and reject
oversized requests with `413`. Configure a limit through a policy or setting
that your implementation supports for `HTTPRoute`. The operator passes
`spec.gateway.annotations` through to HTTPRoute metadata, but annotation
semantics are implementation-specific; a controller must recognize an
annotation on that resource for it to have an effect. Set the limit above the
largest image the deployment accepts while keeping an intentional ceiling.

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
dd if=/dev/urandom of=/tmp/large.img bs=1048576 count=512
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

Replace the example URI with a reachable HTTPS URL allowed by
`spec.importFiltering`.

The call returns immediately and the image reaches `active` once the download
and move into the store finish. Poll `openstack image show big-import -f value
-c status` until the status changes to `active`. A synchronous 400 at import
time comes from the URI filter before bytes are staged. See
[Filter web-download Image Imports](./filter-web-download-imports.md).

Clean up:

```bash
openstack --insecure image delete large-image big-import
rm -f /tmp/large.img
```

## Standalone Glance, without a ControlPlane

For a standalone deployment, set `spec.staging` directly on the `Glance` CR:

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

The suite overrides `enabled_import_methods` to enable `glance-direct` on its
single-replica test Glance because the staging volume is pod-local. The normal
operator configuration enables only `web-download` and `copy-image`; this test
override is not a production recommendation. The suite asserts the client
upload, the staged upload that fills the bounded volume, and the eviction that
contains an oversized one end-to-end on the CI e2e kind cluster:

```bash
chainsaw test --test-dir tests/e2e/glance/gateway-large-upload
```
