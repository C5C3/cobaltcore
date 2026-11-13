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
`https://glance.127-0-0-1.nip.io:8443`. The Kubernetes resource names below are
from the devstack.
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

The plugin selection is a ControlPlane setting projected onto the Glance child.
The presence of the `conversion` sub-block is the switch, so an empty block runs
it at the operator default output format, `raw`:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"glance":{"importPlugins":{"conversion":{}}}}}}'
```

Set `outputFormat` explicitly for anything else; the accepted values are
`raw`, `qcow2`, and `vmdk`, and admission rejects the rest.

::: warning Set the plugins on the ControlPlane, never on the projected child
The `controlplane-glance` Glance CR is **projected** by the c5c3-operator, so a
`spec.importPlugins` you patch onto the child is reverted on the next reconcile.
The projection is unconditional in the other direction too: clearing a plugin's
sub-block on the `ControlPlane` removes it from the child. Clear the
`conversion` sub-block to [revert this guide](#turn-the-plugin-off-again).
:::

Wait for the ControlPlane projection before checking the rollout. If the
ControlPlane has not updated the Deployment yet, `rollout status` can return
for the old, already-stable revision:

```bash
set -o pipefail
projected=
for _ in $(seq 60); do
  if config_map=$(kubectl get deploy controlplane-glance -n openstack \
      -o 'jsonpath={.spec.template.spec.volumes[?(@.name=="config")].configMap.name}' 2>/dev/null) \
    && [ -n "$config_map" ] \
    && kubectl get cm "$config_map" -n openstack \
      -o 'jsonpath={.data.glance-api\.conf}' 2>/dev/null \
      | grep -F 'image_import_plugins = ' | grep -Fq 'image_conversion'; then
    projected=yes
    break
  fi
  sleep 5
done
if [ "$projected" != yes ]; then
  echo "Timed out after 5 minutes waiting for the projected conversion config" >&2
  exit 1
fi
kubectl rollout status deploy/controlplane-glance -n openstack --timeout=5m
```

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

The list is what enables the plugin at all; on this fresh devstack,
`image_import_plugins = []` is the state before this patch and after the
revert. Confirm Glance is Ready on the
ControlPlane's per-service condition, which answers a narrower question than
its aggregate `Ready`:

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
set -e
status=
seen_importing=
for _ in $(seq 120); do
  status=$(openstack --insecure image show conversion-demo -f value -c status)
  [ "$status" = active ] && break
  if [ "$status" = importing ]; then
    seen_importing=yes
  elif [ "$status" = queued ] && [ "$seen_importing" = yes ]; then
    echo "Import returned to queued; check the Glance API logs for the conversion error" >&2
    exit 1
  fi
  sleep 5
done
if [ "$status" != active ]; then
  echo "Timed out after 10 minutes; conversion-demo is still $status" >&2
  exit 1
fi
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

If an image is declared as `iso` and Glance detects ISO content, it remains
`iso` even when `outputFormat` is `raw`. Glance rejects ISO content declared
under a different disk format. This walkthrough uses a `qcow2` image to
demonstrate conversion.

The plugin runs `qemu-img info` on the staged file first and `qemu-img convert`
only when the format it finds differs from `outputFormat`, so importing an image
that already matches costs one `qemu-img info` call. That binary comes from the
`qemu-utils` package the operator's Glance image ships.

A conversion that fails leaves the API serving but returns the image record to
`queued`, not `active`. The plugin's error is in the API log:

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

The figure shows the path of such an import as B. The second file of a
conversion lands on the volume `staging`, beside the first.

![Three paths of image bytes through one glance-api pod, and the three volumes on the disk of its node. Path A, a client upload with PUT /v2/images/{id}/file: the request body streams through the pod straight into the S3 store and touches no volume. Path B, a web-download import with POST /v2/images/{id}/import: the request returns at once, the pod fetches the whole image from a web server that the import filter admits onto the volume staging, an import plugin may convert it there into a second file, and after the last byte the image moves to the store. Path C, a download with GET /v2/images/{id}/file: the pod reads the image from the store, and with the image cache enabled it also writes it to the volume image-cache and serves later downloads of that image from there. The cache belongs to one replica, and a sidecar prunes it down to 80 percent of its size limit. The volumes staging, tasks-work and image-cache are emptyDirs. A sizeLimit is an eviction threshold, not a quota; periodic kubelet checks mean usage can exceed it before a pod is evicted. No import writes image bytes to tasks-work.](../../diagrams/service-glance-image-paths.svg)

Size the staging budget for the conversion output. A `raw` result is the image's
full virtual size, and a
sparse `qcow2` download hides that number: a few hundred megabytes on the mirror
can be several gigabytes on the staging volume. The bound is an eviction
threshold the kubelet enforces, so an import that crosses it evicts the
glance-api pod and takes every in-flight import on that replica with it. See
[StagingSpec](../../reference/glance/glance-crd.md#stagingspec) for what that
bound does and does not guarantee, and
[Large Image Uploads through the Gateway](./large-image-uploads.md) for the
other half of the same budget.

## Turn the plugin off again

Clear only the conversion block on the ControlPlane, preserving any other
enabled import plugins:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"glance":{"importPlugins":{"conversion":null}}}}}'
```

Wait for the projected config to lose the plugin before checking the rollout:

```bash
set -o pipefail
projected=
for _ in $(seq 60); do
  if config_map=$(kubectl get deploy controlplane-glance -n openstack \
      -o 'jsonpath={.spec.template.spec.volumes[?(@.name=="config")].configMap.name}' 2>/dev/null) \
    && [ -n "$config_map" ] \
    && config=$(kubectl get cm "$config_map" -n openstack \
      -o 'jsonpath={.data.glance-api\.conf}' 2>/dev/null) \
    && ! printf '%s\n' "$config" \
      | grep -F 'image_import_plugins = ' | grep -Fq 'image_conversion' \
    && ! printf '%s\n' "$config" | grep -Fqx '[image_conversion]'; then
    projected=yes
    break
  fi
  sleep 5
done
if [ "$projected" != yes ]; then
  echo "Timed out after 5 minutes waiting for the projected conversion-free config" >&2
  exit 1
fi
kubectl rollout status deploy/controlplane-glance -n openstack --timeout=5m
```

The new render removes `image_conversion` from the plugin list and drops its
`[image_conversion]` section, which the read-back command above shows once the
new ConfigMap is mounted. On the fresh devstack, the plugin list returns to
`[]`; any other enabled plugins stay in the list. Images converted while the
plugin was on stay as they are: the conversion happened at import time and
nothing reverses it.

## Standalone Glance, without a ControlPlane

Without a ControlPlane nothing projects the Glance child, so the same block sits
on the `Glance` CR you own. Add this to its `spec`:

```yaml
spec:
  importPlugins:
    conversion:
      outputFormat: raw
```

The operator resolves an empty `outputFormat` to `raw` at render time, writes
the two keys into `glance-api.conf`, and rolls the Deployment once per change.
Removing the `conversion` block disables the plugin on the next rollout. The
commands above use ControlPlane devstack resource names; adapt them to the
Glance CR, Deployment, endpoint, and backend in your standalone deployment.

## See also

- [Glance CRD API Reference](../../reference/glance/glance-crd.md#importpluginsspec):
  the full `ImportPluginsSpec` contract, the two sibling plugins
  (`decompression` and `injectMetadata`), the fixed plugin order, and the four
  `extraConfig` keys this block owns.
- [ControlPlane CRD API Reference](../../reference/c5c3/controlplane-crd.md):
  `services.glance.importPlugins` and the rest of the projected Glance surface.
- [Filter web-download Image Imports](./filter-web-download-imports.md): the
  filter every import URI passes before the plugin ever sees a byte.
- [Large Image Uploads through the Gateway](./large-image-uploads.md): the
  staging bound a conversion draws twice from.

## Tested by

This Chainsaw suite asserts the rendered plugin order, a non-default output
format, the `qemu-img` binary and `lhafile` module, and a return to a
plugin-free config after it removes all plugin blocks. It does not perform an
image import: the Glance e2e suite has no Keystone credentials for the import
task. The import workflow above is therefore a manual verification:

```bash
chainsaw test --test-dir tests/e2e/glance/import-plugins
```
