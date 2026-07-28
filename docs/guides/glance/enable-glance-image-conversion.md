---
title: Enable Glance Image Conversion
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Enable Glance Image Conversion

An image reaches the store in whatever format its publisher chose, and every
consumer of it inherits that choice. A store on Ceph/RBD clones a `raw` image
copy-on-write and pays a full flatten per boot for anything else, so a `qcow2`
cloud image downloaded from a distribution mirror costs that flatten again and
again. `spec.importPlugins.conversion` moves the work to the import: `qemu-img`
rewrites the image once, on its way in, and every later boot reads the format
the store wants. This guide turns the plugin on, imports a real cloud image
through it, and covers what the conversion costs in staging space.

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
- Outbound connectivity from the cluster to the mirror the import below fetches
  from. It is an HTTPS URL on port 443, which is what the operator's default
  import filter allows; see
  [Filter web-download Image Imports](./filter-web-download-imports.md).

## Which uploads the plugin reaches

Conversion is a stage of the interoperable image import, so it applies to
`POST /v2/images/{id}/import` and to nothing else. Three consequences follow:

- A plain `PUT /v2/images/{id}/file` upload bypasses the plugin. Whoever uploads
  that way decides the format, so a deployment that depends on conversion has to
  keep that path out of its policy.
- Glance skips the plugins for the `copy-image` method, which moves an image
  already held in one store into another.
- `glance-direct` is not among the import methods this operator enables.

What is left is the `web-download` import the walkthrough below uses.

## Enable it

The plugin selection is a ControlPlane knob, projected onto the Glance child.
The presence of the `conversion` sub-block is the switch, so an empty block runs
it at the operator default output format, `raw`:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"glance":{"importPlugins":{"conversion":{}}}}}}'
kubectl rollout status deploy/controlplane-glance -n openstack
```

Set `outputFormat` explicitly for anything else; the accepted values are
`raw`, `qcow2`, and `vmdk`, and admission rejects the rest.

::: warning Set the plugins on the ControlPlane, never on the projected child
The `controlplane-glance` Glance CR is **projected** by the c5c3-operator, so a
`spec.importPlugins` you patch onto the child is reverted on the next reconcile.
The projection is unconditional in the other direction too: clearing
`services.glance.importPlugins` on the `ControlPlane` removes the child's field,
which takes the plugin back off on the next rollout and is how you
[revert this guide](#turn-the-plugin-off-again).
:::

Read back what the pods actually mounted. The plugin list and the plugin's own
section are separate keys in the rendered config:

```bash
kubectl get cm -n openstack "$(kubectl get deploy controlplane-glance -n openstack \
  -o 'jsonpath={.spec.template.spec.volumes[?(@.name=="config")].configMap.name}')" \
  -o 'jsonpath={.data.glance-api\.conf}' | grep -A1 -E '^\[(image_conversion|image_import_opts)\]$'
```

```
[image_conversion]
output_format = raw
--
[image_import_opts]
image_import_plugins = [image_conversion]
```

The list is what enables the plugin at all; `image_import_plugins = []` is the
state before this patch and after the revert. Confirm the service is healthy
again on the ControlPlane's per-service condition, which answers a narrower
question than its aggregate `Ready`:

```bash
kubectl get controlplane controlplane -n openstack \
  -o jsonpath='{.status.conditions[?(@.type=="GlanceReady")].status}{" "}{.status.conditions[?(@.type=="GlanceReady")].reason}{"\n"}'
```

## Import an image and watch the format change

With the `OS_*` variables exported, create an image record that declares the
format the mirror publishes, then import into it:

```bash
openstack --insecure image create --disk-format qcow2 --container-format bare \
  conversion-demo
openstack --insecure image import --method web-download \
  --uri https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2 \
  conversion-demo
```

The import call returns at once and the API pod fetches the image in the
background. Wait for the record to go `active`:

```bash
for _ in $(seq 120); do
  [ "$(openstack --insecure image show conversion-demo -f value -c status)" = active ] && break
  sleep 5
done
openstack --insecure image show conversion-demo -f value -c status
```

The record was created as `qcow2`. Glance rewrote the staged file before it
reached the S3 store and updated the image accordingly:

```bash
openstack --insecure image show conversion-demo -f value -c disk_format
```

```
raw
```

The plugin runs `qemu-img info` on the staged file first and `qemu-img convert`
only when the format it finds differs from `outputFormat`, so importing an image
that already matches costs one `qemu-img info` call. That binary comes from the
`qemu-utils` package the operator's Glance image ships.

A conversion that fails takes its import task down with it and leaves the API
serving. The record stays out of `active`, and the plugin's own error is in the
API log:

```bash
kubectl logs -n openstack deploy/controlplane-glance -c glance-api --tail=50
```

Clean up:

```bash
openstack --insecure image delete conversion-demo
```

## What the conversion costs in staging

Every import lands on the API pod's node-local staging area before the data
reaches the store, and a conversion adds a second file next to the first: the
source and the converted result coexist there until the source is deleted. One
import can therefore draw about twice the image size from the `spec.staging`
budget, which the devstack leaves at the operator default of `10Gi` per scratch
volume.

Size that budget against what the conversion produces rather than against what
travels over the wire. A `raw` result is the image's full virtual size, and a
sparse `qcow2` download hides that number: a few hundred megabytes on the mirror
can be several gigabytes on the staging volume. The bound is an eviction
threshold the kubelet enforces, so an import that crosses it evicts the
glance-api pod and takes every in-flight import on that replica with it. See
[StagingSpec](../../reference/glance/glance-crd.md#stagingspec) for what that
bound does and does not guarantee, and
[Large Image Uploads through the Gateway](./large-image-uploads.md) for the
other half of the same budget.

## Turn the plugin off again

Clear the block on the ControlPlane:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"glance":{"importPlugins":null}}}}'
kubectl rollout status deploy/controlplane-glance -n openstack
```

The next render writes `image_import_plugins = []` and drops the
`[image_conversion]` section, which the read-back command above shows once the
new ConfigMap is mounted. Images converted while the plugin was on stay as they
are: the conversion happened at import time and nothing reverses it.

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
  importPlugins:
    conversion:
      outputFormat: raw
```

Everything else in this guide is unchanged: the operator resolves an empty
`outputFormat` to `raw` at render time, writes the two keys into
`glance-api.conf`, and rolls the Deployment once per change. Removing the block
again disables the plugin on the next rollout.

## See also

- [Glance CRD API Reference](../../reference/glance/glance-crd.md#importpluginsspec) —
  the full `ImportPluginsSpec` contract, the two sibling plugins
  (`decompression` and `injectMetadata`), the fixed plugin order, and the four
  `extraConfig` keys this block owns.
- [ControlPlane CRD API Reference](../../reference/c5c3/controlplane-crd.md) —
  `services.glance.importPlugins` and the rest of the projected Glance surface.
- [Filter web-download Image Imports](./filter-web-download-imports.md) — the
  filter every import URI passes before the plugin ever sees a byte.
- [Large Image Uploads through the Gateway](./large-image-uploads.md) — the
  staging bound a conversion draws twice from.

## Tested by

The rendered plugin list in the operator's fixed order, the non-default output
format, the `qemu-img` and `lhafile` binaries the plugins call at import time,
and the revert above (clearing the block, then watching the config converge back
to an empty list with neither per-plugin section left behind) are asserted
end-to-end on the CI e2e kind cluster by this chainsaw suite:

```bash
chainsaw test --test-dir tests/e2e/glance/import-plugins
```
