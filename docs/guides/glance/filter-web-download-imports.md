---
title: Filter web-download Image Imports
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Filter web-download Image Imports

A `web-download` import hands Glance a URI and lets the API pod fetch it, which
makes that pod an HTTP client inside the cluster network on behalf of whoever
submitted the URI. `spec.importFiltering` decides which URIs it accepts before
a single byte is fetched. This guide covers reading the filter a deployment
actually runs, widening it for a mirror that is not HTTPS on port 443, pinning
imports to the mirrors they are supposed to use, and what the filter still
cannot see.

The figure shows where the filter acts: on path B, before the pod fetches the
URI onto its staging volume.

![Three paths of image bytes through one glance-api pod, and the three volumes on the disk of its node. Path A, a client upload with PUT /v2/images/{id}/file: the request body streams through the pod straight into the S3 store and touches no volume. Path B, a web-download import with POST /v2/images/{id}/import: the request returns at once, the pod fetches the whole image from a web server that the import filter admits onto the volume staging, an import plugin may convert it there into a second file, and after the last byte the image moves to the store. Path C, a download with GET /v2/images/{id}/file: the pod reads the image from the store, and with the image cache enabled it also writes it to the volume image-cache and serves later downloads of that image from there. The cache belongs to one replica, and a sidecar prunes it down to 80 percent of its size limit. The volumes staging, tasks-work and image-cache are emptyDirs. Each size limit is an eviction threshold: once a volume grows past it, the kubelet evicts the pod. No import writes image bytes to tasks-work.](../../diagrams/service-glance-image-paths.svg)

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
- Outbound connectivity from the cluster to whichever mirror the verification
  step imports from. The devstack sets no per-CR `spec.networkPolicy`, so pod
  egress is unrestricted; on a deployment that sets one, see
  [Containing what a widened filter can reach](#containing-what-a-widened-filter-can-reach).

## What the default filter allows

The operator renders all six `[import_filtering_opts]` options on every
reconcile, so the effective filter is always readable from the config the pods
mounted:

```bash
kubectl get cm -n openstack "$(kubectl get deploy controlplane-glance -n openstack \
  -o 'jsonpath={.spec.template.spec.volumes[?(@.name=="config")].configMap.name}')" \
  -o 'jsonpath={.data.glance-api\.conf}' | grep -A6 '^\[import_filtering_opts\]'
```

With `importFiltering` unset, as in the devstack, the operator applies this
restrictive default:

```
[import_filtering_opts]
allowed_hosts = []
allowed_ports = [443]
allowed_schemes = [https]
disallowed_hosts = [localhost,127.0.0.1,0.0.0.0,::1,169.254.169.254,kubernetes,kubernetes.default,kubernetes.default.svc,kubernetes.default.svc.cluster.local]
disallowed_ports = []
disallowed_schemes = []
```

Three properties of how Glance reads those lists decide what a change to them
buys:

**An allow-list disables its deny-list.** Glance evaluates one half of each
allow/deny pair: whenever the allow-list is non-empty it ignores the matching
deny-list entirely. Setting both halves of one pair is therefore rejected at
admission. A deny-list becomes authoritative only when its matching allow-list
is explicitly empty; see
[Making a deny-list authoritative](#making-a-deny-list-authoritative).

**The port pin only bites on URIs that spell the port out.** Glance checks the
port it parsed from the URI. A URI without a port carries no port to check, and
Glance derives no port from the scheme. `https://mirror.example.com/img.raw`
passes the `[443]` pin, `https://mirror.example.com:8443/img.raw` does not, and
the scheme pin is what actually keeps an import off plaintext transport.

**A host must resolve, and encoded spellings are refused.** Before matching,
Glance normalizes the host: an IP literal is canonicalized, a decimal, hex, or
shorthand encoding of one is refused outright, and a name is accepted only if
it resolves as an absolute FQDN. In-cluster lookups append a trailing dot,
which defeats the pod's `resolv.conf` search
list, so an in-cluster mirror must be named in full
(`garage.shared-services.svc.cluster.local`, not `garage`).

A submitted URI the filter refuses never reaches the fetch: the import call
returns `400` with Glance's own message naming the URI, before any staging space
is claimed.

## Pin imports to a known mirror

The tightest posture names the mirrors imports may use and refuses every other
host. Set it on the ControlPlane; the operator projects it onto the Glance child:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"glance":{"importFiltering":{"allowedHosts":["mirror.example.com"]}}}}}'
kubectl rollout status deploy/controlplane-glance -n openstack
```

Substitute the mirrors the deployment imports from; the list holds up to 64
entries, each using the host spelling in its import URI.

::: warning Set the filter on the ControlPlane, never on the projected child
The `controlplane-glance` Glance CR is **projected** by the c5c3-operator, so a
`spec.importFiltering` you patch onto the child is reverted on the next
reconcile. Clearing
`services.glance.importFiltering` on the `ControlPlane` removes the child's
field, which restores the operator defaults on the next rollout and is how you
revert this guide:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"glance":{"importFiltering":null}}}}'
```
:::

Read back what the pods got. The host allow-list is now populated and the
denylist is gone, because Glance would ignore it anyway:

```
allowed_hosts = [mirror.example.com]
disallowed_hosts = []
```

Changing the filter rewrites the content-hashed config ConfigMap, and the new
hash rolls the Deployment, once per change. Confirm the service is
healthy again on the ControlPlane's per-service condition; its aggregate `Ready`
also covers every other service, so it answers a different question:

```bash
kubectl get controlplane controlplane -n openstack \
  -o jsonpath='{.status.conditions[?(@.type=="GlanceReady")].status}{" "}{.status.conditions[?(@.type=="GlanceReady")].reason}{"\n"}'
```

## Allow a mirror that serves plaintext HTTP

A mirror on `http://` or off port 443 needs both halves widened, since scheme
and port are filtered independently:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"glance":{"importFiltering":{"allowedSchemes":["http","https"],"allowedPorts":[80,443]}}}}}'
```

Admission accepts that and warns on it, once per widened list:

```
Warning: spec.services.glance.importFiltering.allowedSchemes is set to [http https], widening
the operator default [https]. The scheme and port pin is the primary web-download control — the
link-local metadata endpoint answers on http/80 — and once widened only the host denylist
remains, which matches literally: alternate spellings of the same address (an integer or
IPv4-mapped form, a raw ClusterIP, a trailing-dot FQDN) are not covered. Confine the reachable
endpoints with spec.networkPolicy.additionalEgress, or pin allowedHosts to the mirrors imports
are supposed to use.
```

The warning explains why the controls are separate. Widening the scheme and
port leaves `disallowedHosts` in place: an unset field resolves to the operator
baseline, and added entries are unioned with that baseline. Host matching is
literal, so pair a widened
allow-list with `allowedHosts`, an egress restriction, or both.

## Making a deny-list authoritative

`disallowedSchemes` and `disallowedPorts` are inert on their own: Glance
evaluates a deny-list only while the matching allow-list is empty, and an unset
allow-list resolves to a non-empty operator default. Admission permits this
shape but warns because the deny-list is ineffective:

```
Warning: spec.services.glance.importFiltering.disallowedSchemes is set while
spec.services.glance.importFiltering.allowedSchemes is unset and therefore resolves to the
operator default [https]. Glance evaluates a deny-list only while the matching allow-list is
empty, so this list is inert; set spec.services.glance.importFiltering.allowedSchemes to an
explicitly empty list ([]) to make the deny-list authoritative.
```

The ControlPlane projection cannot preserve this empty-list opt-out. The lists
serialize with `omitempty`, so the Glance child receives an unset field and
uses the operator default. Only a `Glance` CR you own can express this opt-out.
See [Standalone Glance, without a ControlPlane](#standalone-glance-without-a-controlplane).

`disallowedHosts` is the exception: `allowedHosts` has no operator default, so a
host deny-list on its own is authoritative and needs no opt-out.

## Verification

With the `OS_*` variables exported, create one image record and hand it URIs the
filter should refuse. A rejected import leaves the record `queued`, so the same
record serves every attempt:

```bash
openstack --insecure image create --disk-format qcow2 --container-format bare filter-demo
```

The devstack's Garage object store answers over plaintext HTTP on port 3900.
The default posture refuses that endpoint:

```bash
openstack --insecure image import --method web-download \
  --uri http://garage.shared-services.svc.cluster.local:3900/glance-images/demo.img filter-demo
```

```
URI for web-download does not pass filtering: http://garage.shared-services.svc.cluster.local:3900/glance-images/demo.img
```

The call fails synchronously with `400` because the filter rejects the URI.
This is not a network or mirror fault. The link-local metadata endpoint and the in-cluster API server are
refused by the host denylist even when the scheme and port pass:

```bash
openstack --insecure image import --method web-download \
  --uri https://169.254.169.254/latest/meta-data/ filter-demo
openstack --insecure image import --method web-download \
  --uri https://kubernetes.default.svc.cluster.local/version filter-demo
```

An HTTPS mirror on port 443 is accepted, and the call returns at once while the
API pod stages the download:

```bash
openstack --insecure image import --method web-download \
  --uri https://github.com/cirros-dev/cirros/releases/download/0.6.3/cirros-0.6.3-x86_64-disk.img filter-demo
for _ in $(seq 60); do
  [ "$(openstack --insecure image show filter-demo -f value -c status)" = active ] && break
  sleep 5
done
openstack --insecure image show filter-demo -f value -c status -c size
```

The status reaches `active`. Redirects along the way are re-checked against the
same six lists, so a mirror that redirects into a denied host fails the import
asynchronously. The record does not reach `active`; the failure appears on the
import task instead of as the synchronous `400` above.

Clean up:

```bash
openstack --insecure image delete filter-demo
```

## Containing what a widened filter can reach

Host matching uses literal string membership. Glance supports no CIDR ranges, no
wildcards, and no resolution-based blocking, so the denylist covers the
spellings it lists and nothing else: a raw ClusterIP, a trailing-dot FQDN, an
IPv4-mapped IPv6 form, or an in-cluster HTTPS service whose name is absent all
reach their endpoints under names no list can enumerate. Two controls apply
where the denylist does not:

- **`allowedHosts`** permits only named hosts, including when a URI uses an
  unexpected spelling of a blocked address.
- **`spec.networkPolicy.additionalEgress`** bounds where the API pods may
  connect at all. The auto-derived egress opens the ports of DNS, the database,
  the cache and the S3 backends, to any destination. A per-CR NetworkPolicy
  without an explicit rule therefore blocks a `web-download` mirror on any other
  port, and lets one on those ports through. See
  [Enable the Glance Operator NetworkPolicy](./enable-glance-operator-networkpolicy.md).

You cannot configure these controls through `spec.extraConfig`. All six
`[import_filtering_opts]` keys are operator-owned and **rejected** at admission
when set there. An override would skip the exclusivity rules and warnings, and
leave an audit reading `spec.importFiltering` with the
restrictive default while the rendered config says otherwise.

## Standalone Glance, without a ControlPlane

Without a ControlPlane nothing projects the Glance child, so set the block on
the `Glance` CR you own. This supports an explicitly empty allow-list, which the
ControlPlane projection cannot carry:

```yaml
apiVersion: glance.openstack.c5c3.io/v1alpha1
kind: Glance
metadata:
  name: glance
  namespace: openstack
spec:
  importFiltering:
    allowedSchemes: []          # opt out of the https pin …
    disallowedSchemes: [http]   # … so this deny-list is what applies
```

Everything else in this guide is unchanged: the operator resolves unset fields
at render time, renders all six keys into `glance-api.conf`, and rolls the
Deployment once per filter change. Removing the block again restores the
operator defaults.

## See also

- [Glance CRD API Reference](../../reference/glance/glance-crd.md#importfilteringspec):
  the full `ImportFilteringSpec` contract, how every unset field resolves, and
  the upgrade note for a deployment that ran before the operator rendered this
  group.
- [ControlPlane CRD API Reference](../../reference/c5c3/controlplane-crd.md):
  `services.glance.importFiltering` and the rest of the projected Glance surface.
- [Enable the Glance Operator NetworkPolicy](./enable-glance-operator-networkpolicy.md):
  the egress layer a widened filter should be paired with.
- [Large Image Uploads through the Gateway](./large-image-uploads.md): the
  staging bound an accepted import consumes while it downloads.

## Tested by

The suites assert the restrictive defaults for a CR without
`spec.importFiltering`, the three mutually exclusive allow/deny pairs, item
bounds, the host INI guard, and rejection of the six keys through `extraConfig`:

```bash
chainsaw test --test-dir tests/e2e/glance/basic-deployment
chainsaw test --test-dir tests/e2e/glance/invalid-cr
```

A widened filter is exercised by the Glance Tempest matrix entries, which run the
upstream `tempest.api.image` import tests against a Glance that opts back into
plaintext HTTP on port 80 — the only place CI imports over a widened filter.

::: details The widened filter the Tempest leg applies
The matrix entry isolates its Glance from the parallel suite pool. Its CR name
(`glance-tempest-2026-1`), database, and Keystone endpoint are dedicated to the
entry; the walkthrough above uses the `controlplane-glance` devstack names. Its
`disallowedHosts` stays unset, so the operator's host denylist keeps applying
behind the widened scheme and port:

<<< ../../../tests/tempest/glance-2026-1/02-glance-cr.yaml#glance-cr
:::
