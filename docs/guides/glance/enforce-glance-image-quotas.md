---
title: Enforce Glance Image Quotas
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Enforce Glance Image Quotas

Without a quota, a project can upload images until the image store is full.
Glance's per-project quotas are Keystone unified limits: Keystone stores a
default for every project (a registered limit) and an optional override per
project (a project limit), and Glance asks Keystone for both on every image
create, upload and import. This guide turns that enforcement on for the
ControlPlane's Glance, sets the defaults, overrides them for one project, reads
a project's usage, and turns enforcement off again.

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so a `ControlPlane`
CR named `controlplane` is `Ready` in the `openstack` namespace and its projected
`controlplane-glance` Glance child is running. Every resource name in the
examples below is one that devstack produces.
:::

On a ControlPlane deployment, the `controlplane-glance` Glance CR and the
`controlplane-glance` KeystoneService registration are **projected** by the
c5c3-operator, so a setting made directly on either is reverted on the next
reconcile. Set the quotas on the `ControlPlane` CR and let the operator project
them down. Two components have to be recent enough: K-ORC at commit
`fdf2df5ce62c` or later, which supports role assignments on the system scope,
and a glance-operator of the same release as the c5c3-operator. The devstack
installs both from this checkout; on another cluster, apply
`deploy/flux-system/` of the same checkout again before you set the block. The
`openstack` commands below use the `OS_*` environment variables from the
tutorial's token-issue step, which authenticate as the cloud admin.

---

## Steps

### 1. Set the defaults on the ControlPlane

`services.glance.quotas` declares the default every project gets. Setting the
block enables enforcement:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"glance":{"quotas":{
        "imageSizeTotal":"50Gi","imageStageTotal":"50Gi",
        "imageCountTotal":1000,"imageCountUploading":100}}}}}'
```

| Field | Keystone resource | Bounds |
| --- | --- | --- |
| `imageSizeTotal` | `image_size_total`, stored image data in MiB | a whole number of MiB, `1Mi` to `2147483647Mi` |
| `imageStageTotal` | `image_stage_total`, data staged for import in MiB | the same |
| `imageCountTotal` | `image_count_total`, non-deleted images | at least 1 |
| `imageCountUploading` | `image_count_uploading`, images being uploaded or imported | at least 1 |

A field you leave out defaults to 2147483647, the largest value Keystone
accepts, so `quotas: {}` enables enforcement with every default unbounded. That
suits a platform that only sets per-project overrides.

The operator first writes the four values into Keystone as registered limits on
the `image` service in the ControlPlane's `spec.region` (`RegionOne` on the
devstack) and grants the `glance` service user the `reader` role on the system.
Only once the registration reports both does it set `spec.keystoneLimits` on the
Glance child, which rolls glance-api once. Until then `GlanceReady` reports
`WaitingForGlanceQuotas`:

```bash
kubectl get controlplane controlplane -n openstack \
  -o jsonpath='{.status.conditions[?(@.type=="GlanceReady")].status}{" "}{.status.conditions[?(@.type=="GlanceReady")].reason}{"\n"}'
kubectl rollout status deploy/controlplane-glance -n openstack
```

Glance needs the system role to find its own endpoint: oslo.limit looks it up in
Keystone's service and endpoint lists, which Keystone shows only to an admin or
to a reader on the system. The role also lets the `glance` credential read
whatever Keystone shows a system reader: every user, project and role
assignment, and every user's credentials, EC2 and TOTP secrets included. Treat
the Glance service password with the same care as an admin password while
quotas are on. Each quota check costs Glance about five Keystone requests, so the
Keystone load grows with the image API's request rate.

Changing a value later changes the registered limit in place. Glance reads it on
the next request, and glance-api does not roll. While a project override of the
same resource exists, Keystone refuses the change: `GlanceReady` turns False and
names the registered limit with Keystone's 403, Glance keeps enforcing the old
default, and the change lands once the overrides of that resource are deleted.

### 2. Override the default for one project

A per-project override is a Keystone project limit. It lives in Keystone, not on
the ControlPlane, and has to name the ControlPlane's region. The example creates
a project of its own to override:

```bash
openstack --insecure project create --domain Default quota-demo
openstack --insecure limit create --service glance --project quota-demo \
  --region RegionOne --resource-limit 20 image_count_total
```

Change it with `openstack limit set --resource-limit <n> <limit-id>` and remove
it with `openstack limit delete <limit-id>`. A project without an override gets
the default from step 1.

Keystone lists every project's overrides to a token with the `admin` role, such
as the tutorial's admin token, and only its own project's to a token without it:

```bash
openstack --insecure limit list --region RegionOne
```

The list carries no `--service glance` filter. python-openstackclient before
10.3.0, the client of the 2026.1 release, sends the whole service record as that
filter, and Keystone answers 400 (upstream bug 2161546). The resource names tell
Glance's limits apart.

### 3. Read a project's usage

Glance reports a project's usage against its effective limits on
`GET /v2/info/usage`, for the project the token is scoped to:

```bash
curl -sk -H "X-Auth-Token: $(openstack --insecure token issue -f value -c id)" \
  https://glance.127-0-0-1.nip.io:8443/v2/info/usage
```

Each resource answers with `usage` and `limit`, the latter being the override
where one exists and the default otherwise.

## Verification

An override of 0 refuses the next create of that project, with HTTP 413 and a
message that names the resource:

```bash
PROJECT_ID="$(openstack --insecure token issue -f value -c project_id)"
LIMIT_ID="$(openstack --insecure limit create --service glance --project "${PROJECT_ID}" \
  --region RegionOne --resource-limit 0 image_count_total -f value -c id)"
openstack --insecure image create --disk-format raw --container-format bare quota-check
openstack --insecure limit delete "${LIMIT_ID}"
```

The `image create` fails while the override exists; run it again after the
delete and it succeeds (remove the image with
`openstack --insecure image delete quota-check`). The four registered limits
and the system role are visible to the admin as well:

```bash
openstack --insecure registered limit list --service glance --region RegionOne
openstack --insecure role assignment list --user glance --system all --names
```

## How the limits behave

The storage limits are soft. Glance cannot know an image's size before the
upload ends, so one upload may overshoot `image_size_total`, and the next one is
refused.

The defaults are never written as -1. Keystone reads -1 as unlimited, but
oslo.limit 2.10.0, which the 2026.1 release pins, compares
`usage + delta > limit` without a special case and reads -1 as a limit below any
usage, which refuses every request. 2026.2's oslo.limit 2.12.0 skips a negative
limit. An override of -1 on a 2026.1 Glance therefore blocks the project; use a
large positive value instead.

A resource that has no limit at all counts as limit 0, which is why the operator
holds `spec.keystoneLimits` back until the registered limits exist.

## Turn enforcement off again

Delete the project overrides first. Keystone refuses to change or delete a
registered limit while a project limit of the same resource exists, and until
they are gone the registration holds at `CatalogReady=False`:

```bash
for res in image_size_total image_stage_total image_count_total image_count_uploading; do
  for id in $(openstack --insecure limit list --resource-name "${res}" -f value -c ID); do
    openstack --insecure limit delete "${id}"
  done
done
openstack --insecure project delete quota-demo
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"glance":{"quotas":null}}}}'
kubectl rollout status deploy/controlplane-glance -n openstack
```

The Glance child loses `spec.keystoneLimits` at once and glance-api rolls. The
operator keeps the registered limits and the system role until every glance-api
replica runs the new template and the old pods have terminated, because a pod
that still enforces would count a deleted limit as 0. Then K-ORC deletes the
registered limits and revokes the system role.

## Standalone Glance, without a ControlPlane

Without a ControlPlane nothing creates the Keystone side, so a Glance CR you own
needs three things in place before you set the block:

1. Registered limits for all four resources on the `image` service in the
   region the Glance CR names:

   ```bash
   for r in image_size_total image_stage_total image_count_total image_count_uploading; do
     openstack --insecure registered limit create --service glance --region RegionOne \
       --default-limit 2147483647 "${r}"
   done
   ```

2. A role on the system for the Glance service user:
   `openstack --insecure role add --system all --user glance --user-domain Default reader`.
3. An `internal` endpoint of the `image` service in that region, the interface
   the operator's `[oslo_limit]` section looks the endpoint up on.

Then set `spec.region` and `spec.keystoneLimits` on the Glance CR:

```yaml
apiVersion: glance.openstack.c5c3.io/v1alpha1
kind: Glance
metadata:
  name: glance
  namespace: openstack
spec:
  region: RegionOne
  keystoneLimits: {}
```

Overrides, usage and the soft-limit behaviour are the same as above. Removing
`keystoneLimits` needs no hold here: the hand-made registered limits stay in
Keystone, so a pod of the old template still finds them.

## See also

- [ControlPlane CRD API Reference](../../reference/c5c3/controlplane-crd.md#glancequotasspec):
  `GlanceQuotasSpec` and the `WaitingForGlanceQuotas` reason.
- [Glance CRD API Reference](../../reference/glance/glance-crd.md#keystonelimitsspec):
  the `[oslo_limit]` keys `spec.keystoneLimits` renders and the `extraConfig` keys
  it owns.
- [KeystoneService CRD API Reference](../../reference/c5c3/keystoneservice-crd.md#keystoneserviceregisteredlimitspec):
  `catalog.registeredLimits` and `account.systemRoles`, the registration fields
  the quotas reach Keystone through.
- [Large Image Uploads through the Gateway](./large-image-uploads.md): the
  upload path the size quota counts.
- [Keystone unified limits](https://docs.openstack.org/keystone/latest/admin/unified-limits.html):
  the upstream model of registered and project limits.

## Tested by

The flow above mirrors the following end-to-end suites. The first runs the
devstack's ControlPlane with the four defaults of step 1, reads them and the
system role back from Keystone, and refuses an image create under an
`image_count_total` override of 0. The second renders and removes the
`[oslo_limit]` section on a standalone Glance:

```bash
chainsaw test --test-dir tests/e2e/c5c3/full-controlplane-keystone
chainsaw test --test-dir tests/e2e/glance/keystone-limits
```
