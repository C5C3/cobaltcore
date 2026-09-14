---
title: Enable Volume Encryption with Barbican
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Enable Volume Encryption with Barbican

An encrypted volume is a volume whose type carries an encryption profile. Cinder
asks its key manager for a key when the volume is created, the NFS driver writes
the file as a LUKS container, and the key manager holds the passphrase for the
life of the volume. This guide creates such a type on the ControlPlane devstack,
creates a volume of it, proves the key reached Barbican, and deletes both again.

For the full field reference, see the
[Cinder CRD](../../reference/cinder/cinder-crd.md#keymanagerspec) and the
[Barbican Operator](../../reference/barbican/index.md).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_NFS=true make deploy-infra
```

Follow that tutorial through the block-storage block of Step 3 and the
**Create a first volume** check in Step 6, so the projected `controlplane-cinder`
child is `Ready` in `openstack` with `nfs1` and `nfsbk` attached. Every resource
name in the examples below is one that devstack produces.
:::

::: warning Set storage on the ControlPlane, never on the projected children
The `controlplane-cinder` Cinder CR, the `CinderBackend` `nfs1` and the
`CinderBackupBackend` `nfsbk` are **projected** by the c5c3-operator. A child you
edit, or delete, by hand is reverted (or recreated) on the next reconcile.
Change `services.cinder` on the `ControlPlane` CR and let the operator project
it down; that block is the single source of truth for the projected storage.
:::

- The `OS_*` environment variables from the tutorial's token-issue step, and the
  `python-barbicanclient` plugin its prerequisites name, which is where the
  `openstack secret` subcommands come from.

## What the ControlPlane projects

Nothing in `services.cinder` turns encryption on. The key manager follows the
sibling block: while `services.barbican` is declared, the reconciler gives the
Cinder child a `keyManager` of type `Barbican` pointing at the projected
Barbican's in-cluster Service, and leaves the block absent otherwise. The
devstack declares Barbican, so the child already carries it:

```bash
kubectl get cinder controlplane-cinder -n openstack \
  -o jsonpath='{.spec.keyManager}' | jq
```

```json
{
  "type": "Barbican",
  "barbican": { "endpoint": "http://controlplane-barbican.openstack.svc:9311" }
}
```

Castellan authenticates with the same credentials the service user holds, which
is why setting a key manager requires `keystoneEndpoint` beside it.

The roles that service user holds are the other half. The `KeystoneService`
registration the ControlPlane projects for Cinder creates the account in a
project of its own, `service-cinder`, with the roles `service` **and** `admin`.
No peer registration takes the second one. Cinder deletes the Barbican secret of
an encrypted volume as a fallback when the volume's owner cannot, and every
Barbican the operator renders carries
`[oslo_policy] enforce_new_defaults = true`, whose secure-RBAC defaults put
`secret:get` and `secret:delete` across projects behind a bare `admin`. Without
that role the fallback fails and the key outlives the volume. The
[ControlPlane CRD API Reference](../../reference/c5c3/controlplane-crd.md#servicecinderspec)
carries the same reasoning field by field.

::: warning What the `admin` role opens
Across every project, not only Cinder's own. `secret:get` and `secret:delete`
accept a bare `admin` wherever it is held, so anything that reaches this service
user's credential — a compromised Cinder pod, which carries the password in its
environment, or a reader of the `cinder-service-user` Secret — can read and
permanently delete every tenant's volume encryption key. A deleted key makes its
LUKS volume unrecoverable; nothing else holds the passphrase.

What the role buys is one fallback path: the key of a volume deleted by someone
other than its owner. Weigh the two. Where an orphaned key is the cheaper
failure, grant `service` alone and sweep orphans out of band. Where the role is
kept, restrict `get` on Secrets in the service namespace to the operator's
ServiceAccount, and alert on a `secret:delete` this user issues against a
project that is not its own.
:::

## Step 1 — Create an encrypted type and a volume

Count the secrets first, so the next command has a baseline to move:

```bash
openstack --insecure secret list -f value -c 'Secret href' | wc -l
```

Create a volume type with a LUKS encryption profile, then a volume of that type:

```bash
openstack --insecure volume type create --encryption-provider luks \
  --encryption-cipher aes-xts-plain64 --encryption-key-size 256 \
  --encryption-control-location front-end demo-luks
openstack --insecure volume create --size 1 --type demo-luks demo-enc
openstack --insecure volume show demo-enc -c status -f value
```

Once the volume reads `available`, count again:

```bash
openstack --insecure secret list -f value -c 'Secret href' | wc -l
```

The count is one higher. That single number is what separates a working key
manager from a decorative one: the type and the volume are created just the same
against a key manager that is never called, and only the secret proves cinder
reached Barbican.

## Step 2 — Where the LUKS format happens

The format is the driver's work, not a later step's. For a volume whose type
carries an `encryption_key_id`, the NFS driver's `_create_encrypted_volume_file`
in `cinder/volume/drivers/remotefs.py` fetches the key from the key manager,
writes the passphrase to a temporary file, and creates the backing file itself:

```
qemu-img create -f qcow2 \
  -o encrypt.format=luks,encrypt.key-secret=sec1,encrypt.cipher-alg=...,encrypt.cipher-mode=...,encrypt.ivgen-alg=... \
  --object secret,id=sec1,format=raw,file=<passphrase file>
```

That branch is taken before the driver consults its format options, so it applies
under the `nfs_qcow2_volumes = false` the operator renders for every backend: an
unencrypted volume on this backend is a raw file, an encrypted one is a qcow2
LUKS container.

The key itself is older than the file. It is created in the API, in the
`ExtractVolumeRequestTask` of the volume-create flow, with the request context of
the user issuing the create. The volume service then fetches it back through
castellan when it writes the file.

## Step 3 — Delete the volume

Deleting the volume is what returns the key:

```bash
openstack --insecure volume delete demo-enc
openstack --insecure secret list -f value -c 'Secret href' | wc -l
```

The count is back at the baseline from Step 1. A key still there after the volume
is gone is the symptom the `admin` role exists to prevent, and it points at the
service user's roles rather than at cinder.

Remove the type:

```bash
openstack --insecure volume type delete demo-luks
```

::: warning No Nova on this stack
Unlocking the LUKS container is os-brick's work in the compute service at attach
time, which is what `--encryption-control-location front-end` selects. CobaltCore
has not onboarded Nova. An encrypted volume can therefore be created, listed,
backed up and deleted here, and never attached or written on.
:::

## Standalone Cinder, without a ControlPlane

Without a ControlPlane nothing derives the key manager from a sibling service, so
the block goes on the `Cinder` CR you own:

```yaml
apiVersion: cinder.openstack.c5c3.io/v1alpha1
kind: Cinder
metadata:
  name: cinder
  namespace: openstack
spec:
  keyManager:
    type: Barbican
    barbican:
      endpoint: http://barbican.openstack.svc.cluster.local:9311
```

A CEL rule requires `keystoneEndpoint` beside it, because castellan authenticates
as the same service user. The endpoint has to parse to a URL with a host, and the
webhook checks that too.

Grant that service user the `admin` role on its project in addition to `service`.
Under Barbican's secure-RBAC defaults a project-scoped delete is not enough for
the fallback path, so a volume deleted by anyone other than its owner leaves its
key behind in Barbican with nothing left to name it. The grant reaches further
than that path: `admin` held on any project carries `secret:get` and
`secret:delete` on every project's secrets, so this service user can read and
destroy key material Cinder does not own. Weigh that against leaving the
fallback path off, as
[What the ControlPlane projects](#what-the-controlplane-projects) sets out
above, and scope who may read the Secret holding this password accordingly.

## See also

- [Cinder CRD](../../reference/cinder/cinder-crd.md#keymanagerspec) — the
  `KeyManagerSpec` union rule and the `keystoneEndpoint` dependency.
- [Barbican Operator](../../reference/barbican/index.md#design-decisions) — why
  `enforce_new_defaults` is on for every Barbican and what the new rules grant.
- [ControlPlane CRD API Reference](../../reference/c5c3/controlplane-crd.md#servicecinderspec) —
  the projected registration, its two roles, and the derived `keyManager`.
- [Attach an NFS Backend to Cinder](./attach-an-nfs-backend.md) — the backend
  whose driver writes the LUKS container.

## Tested by

The encrypted-type block of `tests/e2e/c5c3/full-controlplane-keystone/01-openstack-verify-job.yaml`
runs the same four commands against the projected ControlPlane: it creates the
LUKS type, creates a volume of it, deletes the volume, and deletes the type. It
counts the Barbican secrets on both sides of each half, and fails the run when
the count does not rise by one after the create or fall back after the delete.

```bash
chainsaw test --test-dir tests/e2e/c5c3/full-controlplane-keystone
```

This guide embeds no fixture. Those commands live inside that Job's container
script, a YAML block scalar, and the column-0 `# region` marker a VitePress
snippet import needs would end the scalar where it was written.
