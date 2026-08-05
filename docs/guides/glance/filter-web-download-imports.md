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

With `importFiltering` unset — the devstack's state — that is the operator's
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
admission rather than silently half-applied, and a deny-list only becomes
authoritative once its sibling allow-list is explicitly emptied — see
[Making a deny-list authoritative](#making-a-deny-list-authoritative).

**The port pin only bites on URIs that spell the port out.** Glance checks the
port it parsed from the URI, and a URI without one carries no port to check —
no default is derived from the scheme. So `https://mirror.example.com/img.raw`
passes the `[443]` pin, `https://mirror.example.com:8443/img.raw` does not, and
the scheme pin is what actually keeps an import off plaintext transport.

**A host must resolve, and encoded spellings are refused.** Before matching,
Glance normalizes the host: an IP literal is canonicalized, a decimal, hex, or
shorthand encoding of one is refused outright, and a name is accepted only if
it resolves as an absolute FQDN. That last point matters in-cluster — the
lookup appends a trailing dot, which defeats the pod's `resolv.conf` search
list, so an in-cluster mirror must be named in full
(`garage.shared-services.svc.cluster.local`, not `garage`).

A submitted URI the filter refuses never reaches the fetch: the import call
returns `400` with Glance's own message naming the URI, before any staging space
is claimed.

## Pin imports to a known mirror

The tightest posture names the mirrors imports may use and refuses every other
host. It is a ControlPlane knob, projected onto the Glance child:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"glance":{"importFiltering":{"allowedHosts":["mirror.example.com"]}}}}}'
kubectl rollout status deploy/controlplane-glance -n openstack
```

Substitute the mirrors the deployment imports from; the list holds up to 64
entries, each spelled exactly as an import URI would spell it.

::: warning Set the filter on the ControlPlane, never on the projected child
The `controlplane-glance` Glance CR is **projected** by the c5c3-operator, so a
`spec.importFiltering` you patch onto the child is reverted on the next
reconcile. The projection is unconditional in the other direction too: clearing
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
hash rolls the Deployment — one rollout per change. Confirm the service is
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

The warning is the whole point of the two knobs being separate: `disallowedHosts`
survives the widening — an unset field keeps resolving to the operator baseline,
and an entry you add is unioned onto it rather than replacing it — but it is now
the only control left, and it matches host strings literally. Pair a widened
allow-list with `allowedHosts`, an egress restriction, or both.

## Making a deny-list authoritative

`disallowedSchemes` and `disallowedPorts` are inert on their own: Glance
evaluates a deny-list only while the matching allow-list is empty, and an unset
allow-list resolves to a non-empty operator default. Admission warns rather than
rejects, because the shape is legal, only ineffective:

```
Warning: spec.services.glance.importFiltering.disallowedSchemes is set while
spec.services.glance.importFiltering.allowedSchemes is unset and therefore resolves to the
operator default [https]. Glance evaluates a deny-list only while the matching allow-list is
empty, so this list is inert; set spec.services.glance.importFiltering.allowedSchemes to an
explicitly empty list ([]) to make the deny-list authoritative.
```

The empty-list opt-out the warning names does not survive the ControlPlane,
though: the lists serialize with `omitempty`, so an explicitly empty one reaches
the Glance child as unset and resolves straight back to the operator default.
The projection can only ever land more restrictively than asked, never less —
which means this shape is expressible on a `Glance` CR you own, and only there.
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

The devstack's Garage object store answers on plaintext HTTP on port 3900, which
is exactly what the default posture is there to refuse:

```bash
openstack --insecure image import --method web-download \
  --uri http://garage.shared-services.svc.cluster.local:3900/glance-images/demo.img filter-demo
```

```
URI for web-download does not pass filtering: http://garage.shared-services.svc.cluster.local:3900/glance-images/demo.img
```

The call fails synchronously with `400` — a filter rejection, not a network or
mirror fault. The link-local metadata endpoint and the in-cluster API server are
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
  --uri https://download.cirros-cloud.net/0.6.3/cirros-0.6.3-x86_64-disk.img filter-demo
for _ in $(seq 60); do
  [ "$(openstack --insecure image show filter-demo -f value -c status)" = active ] && break
  sleep 5
done
openstack --insecure image show filter-demo -f value -c status -c size
```

The status reaches `active`. Redirects along the way are re-checked against the
same six lists, so a mirror that redirects into a denied host fails the import
asynchronously — the record does not reach `active` and the failure surfaces on
the import task, not as the synchronous `400` above.

Clean up:

```bash
openstack --insecure image delete filter-demo
```

## Containing what a widened filter can reach

Host matching is literal string membership. Glance supports no CIDR ranges, no
wildcards, and no resolution-based blocking, so the denylist covers the
spellings it lists and nothing else: a raw ClusterIP, a trailing-dot FQDN, an
IPv4-mapped IPv6 form, or an in-cluster HTTPS service whose name is absent all
reach their endpoint under a name no list can enumerate. Two controls hold where
the denylist cannot:

- **`allowedHosts`** turns the question around — an allow-list refuses every
  host it does not name, including the spellings nobody thought to deny.
- **`spec.networkPolicy.additionalEgress`** bounds where the API pods may
  connect at all. The auto-derived egress covers DNS, the database, the cache,
  and the backends' S3 hosts only, so a per-CR NetworkPolicy without an explicit
  rule blocks every `web-download` mirror — and grants nothing else. See
  [Enable the Glance Operator NetworkPolicy](./enable-glance-operator-networkpolicy.md).

Neither of these is reachable through `spec.extraConfig`: all six
`[import_filtering_opts]` keys are operator-owned and **rejected** at admission
when set there, because an override would skip the exclusivity rules and the
warnings above and leave an audit reading `spec.importFiltering` with the
restrictive default while the rendered config says otherwise.

## Standalone Glance, without a ControlPlane

Without a ControlPlane nothing projects the Glance child, so the same block sits
on the `Glance` CR you own — including the explicitly empty allow-list the
projection cannot carry:

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

- [Glance CRD API Reference](../../reference/glance/glance-crd.md#importfilteringspec) —
  the full `ImportFilteringSpec` contract, how every unset field resolves, and
  the upgrade note for a deployment that ran before the operator rendered this
  group.
- [ControlPlane CRD API Reference](../../reference/c5c3/controlplane-crd.md) —
  `services.glance.importFiltering` and the rest of the projected Glance surface.
- [Enable the Glance Operator NetworkPolicy](./enable-glance-operator-networkpolicy.md) —
  the egress layer a widened filter should be paired with.
- [Large Image Uploads through the Gateway](./large-image-uploads.md) — the
  staging bound an accepted import consumes while it downloads.

## Tested by

The restrictive default posture a CR without `spec.importFiltering` renders, and
the admission rules that keep the block honest — the three mutually exclusive
allow/deny pairs, the item bounds, the host INI guard, and the rejection of the
six keys through `extraConfig` — are asserted end-to-end on the CI e2e kind
cluster by these chainsaw suites:

```bash
chainsaw test --test-dir tests/e2e/glance/basic-deployment
chainsaw test --test-dir tests/e2e/glance/invalid-cr
```

A widened filter is exercised by the Glance Tempest legs instead, which run the
upstream `tempest.api.image` import tests against a Glance that opts back into
plaintext HTTP on port 80 — the only place CI imports over a widened filter.

::: details The widened filter the Tempest leg applies
The leg isolates its Glance from the parallel suite pool, so the CR name
(`glance-tempest-2025-2`) and its database and Keystone endpoint are the leg's
own rather than the `controlplane-glance` devstack names used above. Its
`disallowedHosts` stays unset, so the operator's host denylist keeps applying
behind the widened scheme and port:

<<< @/../tests/tempest/glance-2025-2/02-glance-cr.yaml#glance-cr
:::
