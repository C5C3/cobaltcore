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
`os_glance_staging_store` volume, and moves it into the backing store only after
the last byte has arrived; the async task keeps its working copy on a second
volume, `os_glance_tasks_store`. Both are `emptyDir`s on the node filesystem,
and `spec.staging.sizeLimit` is what bounds them. The operator enables
`web-download` and `copy-image` only, so a client upload never stages.

## Sizing the staging bound

The bound is a ControlPlane knob, projected onto the Glance child:

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

Read back what the pods actually got. The value lands on both scratch volumes:

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
  onto the same replica draw from one 40Gi volume, and when their combined
  staging usage crosses the bound the kubelet evicts the pod, killing both — and
  every other transfer in flight on that replica. Size for the concurrency the
  deployment expects, not for its largest single image.
- **Conversion needs headroom.** An import plugin that converts the downloaded
  image writes the converted copy alongside the original before replacing it, so
  a converting import can occupy roughly twice its image size in staging.

The operator's default of `10Gi` covers the stock distribution cloud images with
room to convert. Raise it for a deployment that imports disk images built for
databases or appliances, where a single qcow2 can exceed that on its own.

::: warning The bound applies retroactively on operator upgrade
A Glance that predates this block ran with unbounded scratch volumes. Upgrading
the operator stamps the resolved `10Gi` onto the existing `Deployment`, rolls
the pods — killing any in-flight import — and then evicts the pod on an import
that used to succeed above `10Gi`.

You cannot decide this in advance. `staging` reaches the `ControlPlane` schema in
the same chart version that introduces the bound, so either patch on this page
names a field the pre-upgrade CRD does not have: the API server prunes it, the
request succeeds having stored nothing, and `kubectl patch` carries no
`--validate` flag to turn that silence into an error. Quiesce the imports you
care about, upgrade, then patch once the field exists — which rolls the pods a
second time, onto the value you chose.
:::

Opting out means `unbounded`, which renders both scratch volumes with no
`sizeLimit` at all — the shape the pods had before the block existed:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"glance":{"staging":{"sizeLimit":null,"unbounded":true}}}}}'
```

It is the escape hatch, not the recommendation. An unbounded volume puts nothing
between one runaway `web-download` import and the node's disk, and once that
disk fills the kubelet evicts across every pod on the node rather than only the
Glance ones. Setting it together with `sizeLimit` is rejected at admission,
which is why the patch above clears the one while setting the other.

### What the bound does not do

It is not a filesystem quota. `emptyDir.sizeLimit` is an eviction threshold the
kubelet evaluates on its periodic local-storage housekeeping pass (~10 s), so
peak usage is the bound plus whatever the writer appends within one pass — an
import pulling at local-disk line rate overshoots by that much, and one that
finishes inside a single pass is never noticed at all. Leave headroom on the
node for the overshoot.

It is not a scheduling reservation either. The operator derives no
`resources.requests.ephemeral-storage` from it, so the scheduler does not know
what a glance-api pod may claim, and replicas — or pods from separate `Glance`
CRs — co-schedule freely. Four pods at the default may legitimately reach 80 GiB
of scratch on one node, and once the node crosses its eviction threshold the
kubelet ranks and evicts across every pod on it, not only the Glance ones. Size
nodes against `replicas × 2 × sizeLimit`. To make the scheduler account for it,
add `ephemeral-storage` to `spec.deployment.resources.requests` — and repeat the
CPU and memory values there, because a `resources` block that is present at all
suppresses the operator's resource defaults.

Finally, it does not meter tenants. It caps how much local disk the imports on
one pod may consume before the kubelet steps in, and says nothing about how many
images a project may create or how much it may keep in the backend. Those are
Glance quotas.

## When an import breaches the bound

Glance knows nothing about the bound and keeps writing. The kubelet notices on
its next local-storage housekeeping pass, and the sequence from there is fixed:

1. local-storage eviction evicts the glance-api pod;
2. the in-flight import dies with it, so its image never reaches `active`;
3. the Deployment replaces the pod and the API recovers without operator or
   human intervention.

The eviction is visible on the pod and in the event stream:

```bash
kubectl get pods -n openstack -l app.kubernetes.io/instance=controlplane-glance \
  -o 'custom-columns=NAME:.metadata.name,PHASE:.status.phase,REASON:.status.reason'
kubectl get events -n openstack --field-selector reason=Evicted
```

The image record survives in a non-active status. Delete it, then retry with a
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

No CR field configures this, on the Glance CR or on the ControlPlane. The
implementation default (15 s on Envoy Gateway) truncates legitimate image
transfers, which is why the identity and dashboard routes keep it and this one
does not.

The bound is raised, never removed. `"0s"` is the Gateway API spelling of a
disabled timeout, and the operator deliberately does not use it: the route
matches a bare `/` prefix, so it covers every Glance path, and the route timeout
is then the only request-duration cap in front of the API. A stream idle timeout
does not substitute for it — it resets on every byte, so a client trickling one
byte at a time never goes idle. Four hours clears any legitimate transfer while
still capping how long one stalled request holds its worker.

::: warning The route timeout bounds duration, not concurrency
It is no defense against a saturated worker pool, and nothing else the operator
renders is either. A glance-api pod serves `processes × threads` concurrent
requests — two by default (`--processes 2 --threads 1`, with uWSGI `harakiri`
off unless `spec.apiServer.uwsgi.harakiri` is set) — so a three-replica
deployment has six request slots for the whole API. Requests that keep trickling
occupy a slot for the full four hours, and while every slot is occupied,
unrelated calls such as an image list queue behind them. Six concurrent
multi-gibibyte uploads over slow links do this as readily as a client abusing it
deliberately.

Size against it rather than against the deadline. Widening the pool is the lever
the CRs expose: `services.glance.replicas` on the `ControlPlane`, and — on a
standalone `Glance` only, since the projection deliberately leaves
`spec.apiServer` to the operator's release defaults — `spec.apiServer.uwsgi`
`processes` and `threads`. Shedding the excess instead of queueing it is a
Gateway concern: on Envoy Gateway a `BackendTrafficPolicy` caps concurrent
requests per backend. That policy belongs to the Gateway infrastructure and is
not rendered by this operator.
:::

The stanza only survives if the cluster's Gateway API CRDs know the field. It
entered the HTTPRoute schema after the versions the stack shipped earlier, so
`hack/deploy-infra.sh` pins `v1.6.1` of the standard channel; an older CRD
prunes `timeouts` silently, and the transfer is then cut at the implementation
default with nothing logged.

**A listener must match the hostname.** `spec.gateway.hostname` routes only if
the Gateway terminates TLS for that SNI hostname. The kind stack's Gateway
`openstack-gw` carries one listener per hostname, each with its own
cert-manager Certificate, including `https-glance-upload` for
`glance-upload.127-0-0-1.nip.io` — a hostname reserved for the large-upload
end-to-end suite so it cannot race the Quick Start smoke suite's route. Check
that the listener a hostname needs is programmed before blaming the upload:

```bash
kubectl get gateway openstack-gw -n openstack \
  -o jsonpath='{range .status.listeners[*]}{.name}{"\t"}{.conditions[?(@.type=="Programmed")].status}{"\n"}{end}'
```

**Body-size limits belong to the gateway stack.** Envoy Gateway caps no request
body by default, so a multi-gibibyte `PUT` reaches Glance intact. Other stacks
disagree: an NGINX-based ingress defaults to a 1 MiB body and answers `413`
within the first megabyte of an image. Fronting Glance with such a stack means
raising that limit there — raise it to the largest image the deployment accepts
rather than removing it, so the gateway keeps a ceiling on what one request may
push at the API. The operator writes no annotation for it, and
`spec.gateway.annotations` is passed through to the HTTPRoute verbatim, so
whichever key that stack reads is yours to set.

**Imports need an egress path.** A `web-download` import makes the API pod an
HTTP client of whatever URI it is handed, which is a second surface next to the
upload path. `spec.importFiltering` decides which URIs are admissible (HTTPS on
port 443 by default), and the per-CR NetworkPolicy decides where the pod may
connect at all — its auto-derived egress covers DNS, the database, the cache,
and the S3 backends only, so a mirror needs an explicit
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
time comes from the URI filter, before any bytes are staged — see
[Filter web-download Image Imports](./filter-web-download-imports.md).

Clean up:

```bash
openstack --insecure image delete large-image big-import
rm -f /tmp/large.img
```

## Standalone Glance, without a ControlPlane

Without a ControlPlane nothing projects the Glance child, so the same knob sits
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
to `10Gi`, stamps the resolved value on both scratch volumes, and renders the
same four-hour route timeout on the HTTPRoute it creates from `spec.gateway`.

## See also

- [Glance CRD API Reference](../../reference/glance/glance-crd.md#stagingspec) —
  the full `StagingSpec` contract and its admission rules.
- [ControlPlane CRD API Reference](../../reference/c5c3/controlplane-crd.md) —
  `services.glance.staging` and the rest of the projected Glance surface.
- [Keystone CRD API Reference](../../reference/keystone/keystone-crd.md#httproute-resource-mapping) —
  the shared HTTPRoute mapping, including the route timeout every operator
  renders.
- [Infrastructure Manifests](../../reference/infrastructure/infrastructure-manifests.md#glance-large-upload-listener) —
  the kind stack's `https-glance-upload` listener and its certificate.

## Tested by

The upload path, the staged (`glance-direct`) upload that fills the bounded
staging volume, and the eviction that contains an oversized one are asserted
end-to-end on the CI e2e kind cluster by this chainsaw suite:

```bash
chainsaw test --test-dir tests/e2e/glance/gateway-large-upload
```
