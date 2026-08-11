---
title: Attach an External OpenBao to Barbican
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

<!-- operator namespace is `barbican-system`; workload (Barbican CR) stays in `openstack`. -->

# How-to: Attach an External OpenBao to Barbican

This guide attaches the Key Manager service to an OpenBao or HashiCorp Vault
server that already exists, run and hardened outside this control plane. It is
the production shape of the service: the operator reads two Secrets, verifies the
credentials in them, and renders the store configuration. It creates no mount, no
policy, and no AppRole on your server, and it never writes to it.

That read-only posture is what turns the rest of this page into a contract. The
server side has to be provisioned out of band, in a specific shape, before a
`ControlPlane` referencing it can converge. Sections 1 and 2 below are that
shape; sections 3 to 5 attach the service to it on the devstack.

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so a `ControlPlane`
CR named `controlplane` is `Ready` in the `openstack` namespace. That devstack
also brings up the proving `OpenBaoCluster` `openbao-instance` in `openstack`,
which stands in below for the server you already run.
:::

1. **An OpenBao or HashiCorp Vault server reachable from the cluster over
   `https://`.** Plain HTTP is rejected at admission: the role ID, the secret ID,
   and every secret Barbican stores travel that URL. On the devstack the stand-in
   is `https://openbao-instance.openstack.svc:8200`.
2. **A token with write access to that server**, for the provisioning in
   section 1. The operator never has one.
3. **`jq` on `PATH`** for the condition readouts in section 5.

::: warning Not the shared management OpenBao
The devstack's other OpenBao, the management instance in the `shared-services`
namespace, is not a candidate. It requires client certificates on every
connection, and castellan's vault plugin authenticates with an AppRole and a CA
bundle only, with no way to present one. Attaching to it produces a connection
reset that looks like an unreachable server. Use a server that accepts AppRole
logins, which on this devstack is `openbao-instance`.
:::

## 1. What the server must carry

Three things, provisioned by you.

**A KV v2 mount.** castellan's vault plugin speaks KV version 2 and nothing else,
so a version 1 mount is not usable. `kvMountpoint` defaults to `barbican`, which
matches a mount enabled as `bao secrets enable -path=barbican -version=2 kv`. A
server that keeps its secrets on the stock `secret/` mount names it explicitly
instead.

**A policy scoping an identity to that mount.** This is the policy the delivered
bootstrap writes, and it is the exact grant the operator probes for
(`deploy/openbao/policies/barbican-secretstore.hcl`):

```hcl
path "barbican/data/*" {
  capabilities = ["create", "read", "update", "delete", "list"]
}

path "barbican/metadata/*" {
  capabilities = ["create", "read", "update", "list"]
}
```

The absence of `delete` on `metadata/*` is deliberate. In KV v2 a delete on the
metadata path permanently destroys every version of a secret along with its
metadata, which is the only in-store recovery path tenant key material has. The
delete castellan issues goes through `data/`, where it is a recoverable soft
delete. On a mount other than `barbican`, substitute the mount path in both
blocks.

**An AppRole carrying that policy.** The shape the bootstrap writes
(`deploy/openbao/bootstrap/setup-auth.sh`):

```bash
bao write auth/approle/role/barbican \
  token_policies=barbican-secretstore \
  token_ttl=1h \
  token_max_ttl=4h \
  secret_id_ttl=720h
```

`secret_id_num_uses` is left unset on purpose. castellan re-logs in whenever its
cached token ages out after `token_ttl`, so a use cap would expire the credential
mid-operation rather than bound it. That leaves `secret_id_ttl` as the only bound
on a leaked secret ID, and 30 days as a fuse on the credential in use: the plugin
holds one process-global AppRole session, so an expired secret ID surfaces as
every secret-store operation failing at once. Nothing re-mints it for a store in
this mode. The re-mint procedure, under the AppRole Auth heading of the
[OpenBao Bootstrap reference](../../reference/infrastructure/openbao-bootstrap.md#approle-auth),
is the control: it reads the stable role ID, mints a replacement secret ID
straight into the Secret, and destroys the accessor of the one it replaced.

## 2. The two Secrets

The operator resolves both by name in the namespace the Barbican service is
placed in, and their data keys are fixed by contract rather than selectable per
CR.

| Secret | Data keys | Required |
| --- | --- | --- |
| credentials | `role-id`, `secret-id` | yes |
| CA bundle | `ca.crt` | only when the server's certificate is not already trusted by the pods' system store |

On the devstack, the e2e suite's helper mints a secret ID against the proving
instance and writes both Secrets for you. It is the stand-in for the out-of-band
provisioning a real attachment assumes:

```bash
tests/e2e/barbican/approle-credentials.sh openstack
```

It logs in to `openbao-instance` through its Kubernetes auth method as the
`openbao-instance-provisioner` ServiceAccount, reads the `barbican` AppRole's
role ID, mints a secret ID, and writes `barbican-brownfield-approle` (role ID and
secret ID) plus `openbao-instance-ca` (the trust bundle) into the namespace it is
given. Against a real server, create the same two Secrets by hand from the
credentials your operators issued.

## 3. Declare the Barbican service

On a devstack built from the tutorial, `services.barbican` is already declared
with `secretStore: {dedicated: {}}`, so this edit replaces the `dedicated` block
with the `external` one. A ControlPlane that does not declare the service yet
adds the whole block:

```bash
kubectl edit controlplane controlplane -n openstack
```

```yaml
spec:
  services:
    barbican:
      replicas: 1
      secretStore:
        external:
          url: https://openbao-instance.openstack.svc:8200
          credentialsSecretRef:
            name: barbican-brownfield-approle
          caBundleSecretRef:
            name: openbao-instance-ca
          # kvMountpoint defaults to "barbican"; a stock secret/ mount sets it
          # explicitly. Get it right the first time (see below).
```

On a ControlPlane that already ran the dedicated mode, this edit changes the
store's mode, a field the `BarbicanSecretStore` CRD freezes. The reconciler
answers that by deleting `controlplane-barbican-store` and recreating it against
the external server on the next pass, and reports `BarbicanReady=False` with
reason `RecreatingBarbicanSecretStore` in between. The dedicated instance
`controlplane-barbican-bao` is left in place: nothing in the switch tears it
down, and the c5c3-operator only removes it when `services.barbican` is dropped
under both deletion annotations. It keeps running, with the material Barbican
wrote through the old store, until you delete it by hand. What that deletion
destroys is spelled out in step 6 of
[Run Barbican on a Dedicated OpenBao](./barbican-dedicated-openbao.md).

`external` and `dedicated` are mutually exclusive, and a block naming neither is
rejected at admission. Two fields are worth setting deliberately:

- **`kvMountpoint`** is frozen on the projected store: secret material written
  under one mount is not reachable under another, so the `BarbicanSecretStore`
  CRD refuses the update rather than re-pointing a live store at material it
  cannot read. Changing it on the ControlPlane makes the reconciler delete the
  store and recreate it on the next pass, and everything written under the old
  mount stays where it is.
- **`namespace`** scopes every request to an OpenBao or Vault namespace, the
  enterprise-style multi-tenancy header. It exists only in this mode: a dedicated
  instance is provisioned at the root namespace. The operator sends it on its own
  login and capability probe too, so a store whose AppRole lives in a namespace
  it does not name reports `InvalidCredentials` against credentials that are
  correct.

## 4. Onboard the barbican database-engine tenant

On the managed shared database the Barbican DB credential is engine-issued, and
`setup-database-tenant.sh` provisions an engine connection and role only for the
services the ControlPlane declares. It reads the live spec on every run, so a
tenant setup that ran before `services.barbican` was declared provisioned no
Barbican pair. Re-run it in that case; the config and role writes are upserts,
so a repeat on an already-onboarded ControlPlane refreshes the pair in place:

```bash
export BAO_TOKEN=$(kubectl get secret openbao-init-keys -n shared-services \
  -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token')
deploy/openbao/bootstrap/setup-database-tenant.sh openstack controlplane
unset BAO_TOKEN
```

This is the management OpenBao's database engine, unrelated to the server the
secret store points at. The two are separate concerns: one issues the MySQL user
Barbican's metadata schema is reached with, the other holds the secret material.

## 5. Watch the store validate

Watch the service's own condition, `BarbicanReady`, rather than the
ControlPlane-wide `Ready`, which also covers every other service:

```bash
kubectl wait controlplane/controlplane -n openstack \
  --for=condition=BarbicanReady --timeout=15m
```

The store CR the ControlPlane projects carries the detail:

```bash
kubectl get barbicansecretstore controlplane-barbican-store -n openstack \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\n"}{end}'
```

Two conditions, not three. `CredentialsReady` reports the login and the
capability probe; `ConfigProjected` reports that the rendered `barbican.conf` in
the running Deployment carries this store's section. `ProvisioningReady` is
removed rather than reported `False`: it belongs to a store the operator
provisions, and reporting it here would claim a step that never ran.

`CredentialsReady=True/CredentialsAvailable` means the operator logged in with
the referenced role ID and secret ID and confirmed, through
`sys/capabilities-self` on `<mount>/data/probe`, that the policy grants all five
of `create`, `read`, `update`, `delete`, and `list`. The probe path is never
written to. The check re-runs every 15 minutes, so a secret ID that expires under
a running deployment surfaces on the store's status rather than only in the API's
error rate.

### HashiCorp Vault

A Vault server attaches through the same fields, and the projected store keeps
`type: OpenBao`. The type selects Barbican's `vault_plugin`, which speaks the
plain Vault HTTP API that OpenBao stays compatible with, so the two servers are
interchangeable from Barbican's side and do not warrant a second enum value.

## Troubleshooting

Every reason below sits on `CredentialsReady` on the
`controlplane-barbican-store` CR.

| Reason | Cause | What to do |
| --- | --- | --- |
| `WaitingForCredentials` | A referenced Secret is absent, or carries an empty `role-id`, `secret-id`, or `ca.crt`. Routine while GitOps ordering settles. | Create the Secret, or fill the missing key. The message names the Secret and the key. |
| `InvalidCredentials` | The server rejected the login. The secret ID expired or was destroyed, the role ID belongs to a different AppRole, or the AppRole lives in an OpenBao namespace `namespace` does not name. | Re-mint the secret ID into the credentials Secret, or correct the namespace. |
| `InsufficientCapabilities` | The login succeeded and the policy behind the AppRole does not grant all five capabilities on `<mount>/data/*`. The message names the ones missing. | Apply the policy from section 1, with the mount path substituted. |
| `OpenBaoUnreachable` | The server did not answer, or the client could not be built: DNS, a dropped connection, an expired or wrong CA bundle. A NetworkPolicy that does not admit the operator produces a timeout here rather than a refusal. | Check the URL and the bundle in `ca.crt`, then the egress and ingress policies between `barbican-system` and the server. |

## Standalone Barbican, without a ControlPlane

Without a ControlPlane you own the `BarbicanSecretStore` directly, and the same
two Secrets are resolved in its own namespace. The names below are ones you
choose; none of the `controlplane-` names from the walkthrough above applies
here.

```yaml
apiVersion: barbican.openstack.c5c3.io/v1alpha1
kind: BarbicanSecretStore
metadata:
  name: barbican-store
  namespace: openstack
spec:
  barbicanRef:
    name: barbican
  type: OpenBao
  isDefault: true
  openBao:
    server:
      url: https://openbao.example.com:8200
      credentialsSecretRef:
        name: barbican-approle
      caBundleSecretRef:
        name: barbican-openbao-ca
    kvMountpoint: secret
    namespace: platform/openstack
```

`server` and `instanceRef` are the two store modes, and the mode is frozen after
creation for the reason `kvMountpoint` is: switching between them re-points the
plugin at a different server, where the material written through the old one does
not exist. The store attaches to a Barbican that does not have to exist yet; a
dangling `barbicanRef` surfaces as `Ready=False` and resolves when the CR
appears. See the
[BarbicanSecretStore CRD API Reference](../../reference/barbican/barbican-secret-store-crd.md)
for the full field surface.

## See also

- [BarbicanSecretStore CRD](../../reference/barbican/barbican-secret-store-crd.md) —
  both store modes, the credential contract, and the condition catalog.
- [OpenBao Bootstrap reference](../../reference/infrastructure/openbao-bootstrap.md#approle-auth) —
  the AppRole role, its policy, and the secret-ID re-mint procedure.
- [Run Barbican on a Dedicated OpenBao](./barbican-dedicated-openbao.md) — the
  self-service mode, for a cluster with no server to attach to.
- [Barbican Controller Events](../../reference/barbican/barbican-events.md) — the
  events the store controller emits alongside these conditions.

## Tested by

The read-only attachment this guide describes (the two Secrets, the login and
capability probe, the absent `ProvisioningReady`, and the rendered
`[vault_plugin]` section pointing at the configured URL and the mounted CA
bundle) is asserted on the live CI e2e kind cluster by this suite:

```bash
chainsaw test --test-dir tests/e2e/barbican/secretstore-brownfield
```

::: details The store CR the secretstore-brownfield suite applies
The suite isolates its Barbican from the parallel suite pool, so its CR names are
the suite's isolation identifiers (Barbican `barbican-store-brownfield`, store
`barbican-store-brownfield-store`) rather than the `controlplane-barbican` names
the walkthrough above uses. The server, the two Secrets, and the default
`kvMountpoint` are the ones from sections 2 and 3.

<<< @/../tests/e2e/barbican/secretstore-brownfield/01-barbican-secretstore.yaml#brownfield-store
:::
