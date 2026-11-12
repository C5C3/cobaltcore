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
the production shape of the service: the operator reads the AppRole credentials
Secret and, when needed, a CA-bundle Secret; it verifies the credentials and
renders the store configuration. It creates no mount, no policy, and no AppRole
on your server, and it never writes to it.

Because the operator is read-only, the server must be provisioned out of band
before a `ControlPlane` referencing it can converge. Sections 1 and 2 specify
the required server configuration; sections 3 to 5 attach the service on the
devstack.

The right panel of the figure shows this mode. The left panel is the mode of
[Run Barbican on a Dedicated OpenBao](./barbican-dedicated-openbao.md). The
management OpenBao of the devstack stands in both and is the server of neither.

![The two modes of a Barbican secret store, side by side. Dedicated: the c5c3-operator creates an OpenBaoCluster {cp}-barbican-bao and declares its self-init, which provisions the KV mount barbican/, a policy, the AppRole role barbican and a Kubernetes auth role. The barbican-operator logs in to that instance through Kubernetes auth, checks the mount, reads the role ID and mints a secret ID into the Secret {cp}-barbican-store-approle. The API pods of {cp}-barbican read the secret ID from that Secret and the CA from {cp}-barbican-bao-tls-ca, and store their secrets in the instance. External: an OpenBao or Vault server runs outside the control plane, and its KV mount, policy and AppRole are provisioned by hand. A required Secret carries the role ID and secret ID; an optional Secret carries the CA bundle. The barbican-operator reads the credentials and optional CA, logs in with the AppRole and checks its capabilities; it writes nothing to the server. The API pods use the same Secret references. On the kind devstack the OpenBaoCluster openbao-instance stands in for the outside server. In both modes the management OpenBao in shared-services is a separate server that delivers the database credentials of Barbican and no store credential.](../../diagrams/service-barbican-secret-stores.svg)

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

The OpenBao or HashiCorp Vault server must be reachable from the cluster over
`https`; plain HTTP is rejected at admission. Have the `bao` or `vault` CLI
configured for that server and a token with write access for section 1. The
section's commands use `bao`; use the equivalent `vault` commands for Vault.
`jq` must be on `PATH` for section 4's token extraction and section 5's
condition readouts. On the devstack, the server stand-in is
`https://openbao-instance.openstack.svc:8200`.

## 1. What the server must carry

Three things, provisioned by you.

::: warning Not the shared management OpenBao
The devstack's other OpenBao, the management instance in the `shared-services`
namespace, is not a candidate. It requires client certificates on every
connection, and castellan's vault plugin authenticates with an AppRole and a CA
bundle only, with no way to present one. Attaching to it produces a connection
reset that looks like an unreachable server. Use a server that accepts AppRole
logins, which on this devstack is `openbao-instance`.
:::

**A KV v2 mount.** castellan's vault plugin speaks KV version 2 and nothing else,
so a version 1 mount is not usable. `kvMountpoint` defaults to `barbican`, which
matches a mount enabled as `bao secrets enable -path=barbican -version=2 kv`. A
server that keeps its secrets on the stock `secret/` mount names it explicitly
instead.

**A policy scoping an identity to that mount.** This is the policy the delivered
bootstrap writes, and it is the exact grant the operator probes for
(`deploy/openbao/policies/barbican-secretstore.hcl`):

```bash
bao policy write barbican-secretstore - <<'EOF'
path "barbican/data/*" {
  capabilities = ["create", "read", "update", "delete", "list"]
}

path "barbican/metadata/*" {
  capabilities = ["create", "read", "update", "list"]
}
EOF
```

The absence of `delete` on `metadata/*` is deliberate. In KV v2 a delete on the
metadata path permanently destroys every version of a secret along with its
metadata, which is the only in-store recovery path tenant key material has. The
delete castellan issues goes through `data/`, where it is a recoverable soft
delete. On a mount other than `barbican`, substitute the mount path in both
policy blocks before applying them.

**An AppRole carrying that policy.** The shape the bootstrap writes
(`deploy/openbao/bootstrap/setup-auth.sh`):

Enable AppRole auth at `auth/approle` if it is not already enabled:

```bash
bao auth enable approle
```

Then create the role:

```bash
bao write auth/approle/role/barbican \
  token_policies=barbican-secretstore \
  token_ttl=1h \
  token_max_ttl=4h \
  secret_id_ttl=720h
```

`secret_id_num_uses` is left unset on purpose. castellan re-logs in whenever its
cached token ages out after `token_ttl`, so a use cap could invalidate the
credential during an operation without providing a useful bound. The
`secret_id_ttl` limits the lifetime of a leaked secret ID to 30 days. The plugin
holds one process-global AppRole session, so an expired secret ID causes every
secret-store operation to fail. The store controller does not issue replacement
IDs in this mode. The replacement procedure, under the AppRole Auth heading of the
[OpenBao Bootstrap reference](../../reference/infrastructure/openbao-bootstrap.md#approle-auth),
is the control: it reads the stable role ID, mints a replacement secret ID
straight into the Secret, and destroys the accessor of the one it replaced.

## 2. Credentials and optional CA bundle

The operator resolves the credentials Secret and optional CA Secret by name in
the namespace where the Barbican service is placed. It expects fixed data keys
for each Secret.

| Secret | Data keys | Required |
| --- | --- | --- |
| credentials | `role-id`, `secret-id` | yes |
| CA bundle | `ca.crt` | only when the server's certificate is not already trusted by both the operator and Barbican pods |

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
given. For a real server, create the credentials Secret with that server's role
ID and secret ID, and create the CA Secret when either the operator or Barbican
pods do not already trust the server certificate.

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
      secretStore:
        external:
          url: https://openbao-instance.openstack.svc:8200
          credentialsSecretRef:
            name: barbican-brownfield-approle
          caBundleSecretRef:
            name: openbao-instance-ca
          # kvMountpoint defaults to "barbican"; a stock secret/ mount sets it
          # explicitly. Get it right the first time (see below).
  sizing:
    barbican:
      api:
        replicas: 1
```

On a ControlPlane that already ran the dedicated mode, this edit changes the
store's mode. The `BarbicanSecretStore` CRD freezes that field, so the reconciler
deletes `controlplane-barbican-store` and recreates it against the external
server on the next pass. It reports `BarbicanReady=False` with
reason `RecreatingBarbicanSecretStore` in between. The dedicated instance
`controlplane-barbican-bao` is left in place; nothing in the switch tears it
down. This switch does not migrate secret payloads. Barbican uses the same store
name against a different server, so any existing secret reference whose payload
was not copied there will fail, even when the new store becomes `Ready`. Store
readiness checks cover credentials, capabilities and configuration. They do not
inspect existing secret payloads. Keep the dedicated instance until you have migrated or otherwise
verified the data. If you later drop `services.barbican`, the c5c3-operator
removes the instance only when both deletion annotations are set; a full
ControlPlane teardown also removes it. What deleting the instance destroys is
spelled out in step 6 of
[Run Barbican on a Dedicated OpenBao](./barbican-dedicated-openbao.md).

`external` and `dedicated` are mutually exclusive, and a block naming neither is
rejected at admission. Set these two fields with care:

- **`kvMountpoint`** is frozen on the projected store: secret material written
  under one mount is not reachable under another, so the `BarbicanSecretStore`
  CRD refuses the update to prevent a live store from pointing at material it
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

Watch the service's own `BarbicanReady` condition. The ControlPlane-wide
`Ready` condition covers every other service too:

```bash
kubectl wait controlplane/controlplane -n openstack \
  --for=condition=BarbicanReady --timeout=15m
```

The store CR the ControlPlane projects carries the detail:

```bash
kubectl get barbicansecretstore controlplane-barbican-store -n openstack \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\n"}{end}'
```

`CredentialsReady` reports the login and capability probe. `ConfigProjected`
reports that the rendered `barbican.conf` in the running Deployment carries
this store's section, and aggregate `Ready` summarizes both. The operator omits
`ProvisioningReady` because this store is provisioned out of band.

The figure shows how `CredentialsReady` and `ConfigProjected` lead to aggregate
`Ready`. [The handshake](../../reference/backend/kubernetes-packages.md#satellite-handshake)
lists its five steps and what differs per kind.

![The handshake between a satellite resource and the service it attaches to, in five numbered steps across two controllers. 1: the satellite controller checks the credentials and sets CredentialsReady on the satellite. 2: the aggregation step of the service controller reads only that condition. 3: it renders one section per satellite that passed into a Secret whose name carries a hash of its content. 4: the pod template of the service's Deployment mounts that Secret, and a new name rolls the pods. 5: the satellite controller finds its section in the mounted Secret and sets ConfigProjected. Ready turns True once both conditions are. An arrow marked never runs from Ready to the aggregation step: reading Ready there would deadlock, because Ready needs ConfigProjected, which needs that step. On a KeystoneIdentityBackend the gate is DomainReady, and ConfigProjected also waits until the rollout has finished.](../../diagrams/service-satellite-handshake.svg)

`CredentialsReady=True/CredentialsAvailable` means the operator logged in with
the referenced role ID and secret ID and confirmed, through
`sys/capabilities-self` on `<mount>/data/probe`, that the policy grants all five
of `create`, `read`, `update`, `delete`, and `list`. The probe path is never
written to. The check re-runs every 15 minutes. If a secret ID expires while the
deployment is running, the store status reports it and API operations can also
fail.

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
| `InvalidCredentials` | The server rejected the login. The secret ID expired or was destroyed, the role ID belongs to a different AppRole, or the AppRole lives in an OpenBao namespace `namespace` does not name. | Issue a replacement secret ID into the credentials Secret, or correct the namespace. |
| `InsufficientCapabilities` | The login succeeded and the policy behind the AppRole does not grant all five capabilities on `<mount>/data/*`. The message names the ones missing. | Apply the policy from section 1, with the mount path substituted. |
| `OpenBaoUnreachable` | The server did not answer, or the client could not be built: DNS, a dropped connection, an expired or wrong CA bundle. A NetworkPolicy that does not admit the operator produces a timeout here rather than a refusal. | Check the URL and the bundle in `ca.crt`, then the egress and ingress policies between `barbican-system` and the server. |

## Standalone Barbican, without a ControlPlane

Without a ControlPlane you own the `BarbicanSecretStore` directly, and its
credentials Secret and optional CA Secret are resolved in its own namespace.
The names below are ones you choose; none of the `controlplane-` names from the
walkthrough above applies here.

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

- [BarbicanSecretStore CRD](../../reference/barbican/barbican-secret-store-crd.md):
  both store modes, the credential contract, and the condition catalog.
- [OpenBao Bootstrap reference](../../reference/infrastructure/openbao-bootstrap.md#approle-auth):
  the AppRole role, its policy, and the secret-ID replacement procedure.
- [Run Barbican on a Dedicated OpenBao](./barbican-dedicated-openbao.md): the
  self-service mode, for a cluster with no server to attach to.
- [Barbican Controller Events](../../reference/barbican/barbican-events.md): the
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
The suite isolates its Barbican from the parallel suite pool. Its CR names are
Barbican `barbican-store-brownfield` and store
`barbican-store-brownfield-store`. The walkthrough above uses the
`controlplane-barbican` names. The server, the two Secrets, and the default
`kvMountpoint` are the ones from sections 2 and 3.

<<< ../../../tests/e2e/barbican/secretstore-brownfield/01-barbican-secretstore.yaml#brownfield-store
:::
