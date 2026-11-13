---
title: Enable Volume Encryption with Barbican
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Enable Volume Encryption with Barbican

A Cinder volume type can select LUKS (Linux Unified Key Setup) encryption. When
a user creates a volume with that type, Cinder stores its encryption key in the
configured key manager. The NFS driver writes the volume file as a qcow2 disk
image protected by LUKS. This guide creates such a type on the ControlPlane
devstack, creates a volume of it, verifies that Barbican stores its key, and
deletes both again.

For the full field reference, see the
[Cinder CRD](../../reference/cinder/cinder-crd.md#keymanagerspec) and the
[Barbican Operator](../../reference/barbican/index.md).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_NFS=true make deploy-infra
```

Follow that tutorial through the block-storage block of Step 4 and the
**Create a first volume** check in Step 7, so the projected `controlplane-cinder`
child is `Ready` in `openstack` with `nfs1` and `nfsbk` attached. Devstack
provides those ControlPlane and backend names; Step 1 creates the `demo-luks`
volume type and `demo-enc` volume.
:::

Export the `OS_*` variables from the tutorial's token-issue step. The
`openstack secret` subcommands require the `python-barbicanclient` plugin listed
in the [Quick Start prerequisites](../../quick-start-controlplane.md#prerequisites).

## What the ControlPlane projects

Nothing in `services.cinder` turns encryption on. The key manager follows the
sibling block: while `services.barbican` is declared, the reconciler gives the
Cinder child a `keyManager` of type `Barbican` pointing at the projected
Barbican's in-cluster Service, and leaves the block absent otherwise. The
devstack declares Barbican, so the child already carries it:

::: warning Do not edit the projected Cinder child
On a ControlPlane-managed deployment, the reconciler derives `spec.keyManager`
from `services.barbican`. A manual edit to `controlplane-cinder` is overwritten
on the next reconcile. Declare or remove `services.barbican` on the
`ControlPlane` CR instead.
:::

```bash
kubectl get cinder controlplane-cinder -n openstack \
  -o yaml | yq '.spec.keyManager'
```

```json
{
  "type": "Barbican",
  "barbican": { "endpoint": "http://controlplane-barbican.openstack.svc:9311" }
}
```

Cinder's Castellan library calls the configured key manager with the same
Keystone credentials as the service user. That is why setting a key manager
requires `keystoneEndpoint` beside it.

The `KeystoneService` registration the ControlPlane projects for Cinder creates
the service account in its own project, `service-cinder`, with the roles
`service` **and** `admin`. Cinder needs the second role to delete an encrypted
volume's Barbican secret as a fallback when its owner cannot. The operator renders
`[oslo_policy] enforce_new_defaults = true` by default; its secure-RBAC defaults
put `secret:get` and `secret:delete` across projects behind a bare `admin`.
An `extraConfig` override can change that setting. Without the role, the
fallback fails and the key outlives the volume. The
[ControlPlane CRD API Reference](../../reference/c5c3/controlplane-crd.md#servicecinderspec)
carries the same reasoning field by field.

### The `admin` role opens cross-project access

Under these policy defaults, a user holding bare `admin` on any project can use
`secret:get` to read secret metadata and `secret:delete` to delete secrets in
every project. A compromised Cinder pod carries the password in its environment;
anyone who can read the `controlplane-cinder-credentials` Secret can use it too.
These credentials can expose the metadata of and delete another tenant's
volume-key secret. `secret:get` does not expose the key payload; `secret:decrypt`
and `secret:put` still require the secret's own project. Deleting the Barbican
secret can make its LUKS volume unrecoverable; Cinder keeps the key ID, while
the configured Barbican store holds the key material.

The role enables one fallback path: deleting the key of a volume removed by
someone other than its owner. If leaving orphaned keys is the cheaper failure,
grant `service` alone and remove orphaned secrets out of band. If you grant
`admin`, limit Kubernetes API access to the credentials Secret to principals
that need it, and alert when the Cinder user deletes a secret in another project.

## Step 1: Create an encrypted type and a volume

Keep this shell open through Step 3; that step compares the secret count with
`BASELINE_SECRETS`. The volume API returns before the NFS driver finishes
creating the volume, so the polling function waits for `available` and reports
an error state or timeout:

```bash
set -e
wait_volume_status() {
  local name=$1 wanted=$2 timeout=$3 deadline status
  deadline=$(( $(date +%s) + timeout ))
  while true; do
    status=$(openstack --insecure volume show "$name" -f value -c status 2>/dev/null || true)
    if [ "$status" = "$wanted" ]; then
      return 0
    fi
    case "$status" in
      error*|killed)
        echo "ERROR: volume $name reached $status" >&2
        openstack --insecure volume show "$name" >&2 || true
        return 1
        ;;
    esac
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "ERROR: volume $name status '${status:-<none>}' != $wanted within ${timeout}s" >&2
      openstack --insecure volume show "$name" >&2 || true
      return 1
    fi
    sleep 5
  done
}

BASELINE_SECRETS=$(openstack --insecure secret list -f value -c 'Secret href' | wc -l)
printf 'Baseline Barbican secrets: %s\n' "$BASELINE_SECRETS"
```

Create a volume type with a LUKS encryption profile and a volume of that type.
Use these example names only if `demo-luks` and `demo-enc` are unused; Step 3
deletes both.

```bash
openstack --insecure volume type create --encryption-provider luks \
  --encryption-cipher aes-xts-plain64 --encryption-key-size 256 \
  --encryption-control-location front-end demo-luks
openstack --insecure volume create --size 1 --type demo-luks demo-enc
wait_volume_status demo-enc available 120
AFTER_CREATE=$(openstack --insecure secret list -f value -c 'Secret href' | wc -l)
if [ "$AFTER_CREATE" -ne "$((BASELINE_SECRETS + 1))" ]; then
  echo "ERROR: Barbican secret count $AFTER_CREATE != $((BASELINE_SECRETS + 1)) after creating an encrypted volume" >&2
  exit 1
fi
```

In this devstack, the count should increase by one. The volume can reach
`available` even if Cinder never calls a key manager, so the secret-count check
verifies that Barbican stored the key.

## Step 2: Where the LUKS format happens

The NFS driver formats an encrypted volume while creating its file. Each volume
has its own `encryption_key_id`; the volume type selects the encryption
settings. When that field is set, `_create_encrypted_volume_file` in
`cinder/volume/drivers/remotefs.py` fetches the key from the key manager, writes
the passphrase to a temporary file, and creates the volume image with
`qemu-img`, QEMU's disk-image utility:

```text
qemu-img create -f qcow2 \
  -o encrypt.format=luks,encrypt.key-secret=sec1,encrypt.cipher-alg=...,encrypt.cipher-mode=...,encrypt.ivgen-alg=... \
  --object secret,id=sec1,format=raw,file=<temporary-passphrase-file>
```

The ellipses and `<temporary-passphrase-file>` are placeholders; the driver
supplies those values. The encrypted-volume branch runs before the driver
consults its format options, so it applies even though the operator renders
`nfs_qcow2_volumes = false` for every backend. Unencrypted volumes on this
backend are raw files; encrypted ones are qcow2 images protected by LUKS.

Cinder's API creates the key before backend work begins. The
`ExtractVolumeRequestTask` in the volume-create flow calls the key manager with
the requesting user's context. The volume service later fetches the key through
Castellan when it writes the file.

## Step 3: Delete the volume

Delete the volume and wait for Cinder to remove it before checking Barbican.
The deletion is asynchronous, so the polling loop avoids a false orphan report:

```bash
wait_volume_gone() {
  local id=$1 timeout=$2 deadline ids
  deadline=$(( $(date +%s) + timeout ))
  while true; do
    ids=$(openstack --insecure volume list -f value -c ID) || {
      echo "ERROR: could not list volumes while waiting for $id" >&2
      return 1
    }
    if ! printf '%s\n' "$ids" | grep -Fxq "$id"; then
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "ERROR: volume $id is still listed after ${timeout}s" >&2
      openstack --insecure volume show "$id" >&2 || true
      return 1
    fi
    sleep 5
  done
}

test -n "$BASELINE_SECRETS" || {
  echo "ERROR: run Step 1 in this shell first" >&2
  exit 1
}
VOLUME_ID=$(openstack --insecure volume show demo-enc -f value -c id)
openstack --insecure volume delete demo-enc
wait_volume_gone "$VOLUME_ID" 120
AFTER_DELETE=$(openstack --insecure secret list -f value -c 'Secret href' | wc -l)
if [ "$AFTER_DELETE" -ne "$BASELINE_SECRETS" ]; then
  echo "ERROR: Barbican secret count $AFTER_DELETE != baseline $BASELINE_SECRETS after deleting the volume" >&2
  exit 1
fi
openstack --insecure volume type delete demo-luks
```

If Cinder rejects the delete while the volume is still creating, wait for its
status to settle and retry. A key that remains after the volume disappears
indicates that Cinder could not delete it under the configured Barbican policy;
the ControlPlane's `admin` role provides that fallback.

This devstack has no hypervisor. At attach time, the compute host uses os-brick,
OpenStack's volume-connector library, to unlock the LUKS container; the
`front-end` setting selects that path. You can create, list, and delete an
encrypted volume here, but no server can attach it and perform guest I/O.

## Standalone Cinder, without a ControlPlane

Without a ControlPlane, nothing derives the key manager from a sibling service.
Add this field to the existing `Cinder` CR alongside its required
`keystoneEndpoint` and `serviceUser` configuration:

```yaml
spec:
  keyManager:
    type: Barbican
    barbican:
      endpoint: http://barbican.openstack.svc.cluster.local:9311
```

This endpoint assumes a Barbican Service named `barbican` in the `openstack`
namespace; replace it with a URL reachable from the Cinder pods. A CEL rule
requires `keystoneEndpoint` when `keyManager` is set, and the webhook validates
the endpoint URL.

Grant that service user the `admin` role on its project in addition to `service`.
Under Barbican's secure-RBAC defaults, a project-scoped delete is not enough for
the fallback path, so a volume deleted by anyone other than its owner leaves its
key in Barbican. The grant also enables the cross-project metadata access and
secret-deletion risk described in
[The `admin` role opens cross-project access](#the-admin-role-opens-cross-project-access).
Restrict access to the Secret holding this password accordingly.

## See also

- [Cinder CRD](../../reference/cinder/cinder-crd.md#keymanagerspec): the
  `KeyManagerSpec` union rule and the `keystoneEndpoint` dependency.
- [Barbican Operator](../../reference/barbican/index.md#design-decisions): why
  `enforce_new_defaults` is on by default and what the new rules grant.
- [ControlPlane CRD API Reference](../../reference/c5c3/controlplane-crd.md#servicecinderspec):
  the projected registration, its two roles, and the derived `keyManager`.
- [Attach an NFS Backend to Cinder](./attach-an-nfs-backend.md): the backend
  whose driver writes the LUKS container.

## Tested by

The encrypted-type block of `tests/e2e/c5c3/full-controlplane-keystone/01-openstack-verify-job.yaml`
runs the equivalent workflow against the projected ControlPlane: it creates a
LUKS type, creates a volume of it, deletes the volume, and deletes the type. It
checks the Barbican secret count before volume creation, after creation, and
after deletion. The run fails unless the count increases by one after creation
and returns to baseline after deletion.

```bash
chainsaw test --test-dir tests/e2e/c5c3/full-controlplane-keystone
```

The Job stores its commands inside a YAML block scalar, so a VitePress snippet
marker would break the manifest. The guide therefore documents the workflow
instead of importing the test script.
