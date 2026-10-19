---
title: Migrate Keystone DB to Dynamic Credentials
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Migrate Keystone DB to Dynamic Credentials

This guide takes a managed-mode ControlPlane from a long-lived **static** Keystone
database credential to **dynamic**, engine-issued credentials, without database
downtime. It is the operator-facing side of the OpenBao MariaDB database secrets
engine wired for the Keystone service DB user (issue #439).

## What changes

- **Before:** the Keystone DB password is a long-lived value copied from an
  OpenBao KV path (`openstack/keystone/{namespace}/{name}/db`) into the
  `{name}-keystone-db-credentials` Secret. Nothing seeds that path, so on the
  static branch an operator writes it and rotates it by hand.
- **After:** the c5c3 operator projects a per-ControlPlane
  [`VaultDynamicSecret`](https://external-secrets.io/) generator that reads
  short-lived credentials from the OpenBao database engine
  (`database/mariadb/creds/keystone-{namespace}`, where `{namespace}` is the
  Keystone service namespace, the ControlPlane's own namespace unless Keystone
  runs in a dedicated one). The External Secrets Operator re-issues a fresh
  lease before the previous one expires and stores its username and password in
  the Kubernetes Secret. Dynamic mode does not read a static password from the
  OpenBao KV path. Any existing static KV value remains until you delete it in
  step 5.

The engine issues an ephemeral MySQL user per lease (for example `v-kube-...`)
with `ALL PRIVILEGES` on the Keystone database and drops it at lease end.

The figure shows both chains. `{svc}` stands for `keystone` here, and `{cp}` is
`controlplane` on the devstack.

![Database credentials in two modes. Static: a person writes username and password to an OpenBao KV path, an ExternalSecret copies them through the secret store into the Secret {cp}-{svc}-db-credentials, and MariaDB User and Grant resources create one long-lived SQL user from it. Dynamic: a VaultDynamicSecret generator logs in to OpenBao as the ServiceAccount {svc}-db-creds over a client certificate and draws a user from database/mariadb/creds/{svc}-{ns}, the database engine creates that user in MariaDB for one lease, and a changed credential rolls the Deployment through the db-connection-hash annotation. In both modes the service operator builds the DSN Secret that the pods read. A time strip shows the 24 hour refresh inside the 48 hour default TTL and the 72 hour maximum TTL.](../../diagrams/secrets-db-credentials.svg)

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

On this devstack the ControlPlane is `controlplane` in `openstack`, so its
projected Keystone is `controlplane-keystone` and its DB-credential Secret is
`controlplane-keystone-db-credentials`. For an existing Static ControlPlane,
follow the tutorial through its final **Verify** step. To create a fresh Static
starting state, stop before Quick Start Step 4 applies the ControlPlane. Use
migration step 2 to seed the static KV path, then apply the ControlPlane and its
inline Static-mode patch before Quick Start Step 5. Skip that step's
dynamic-role onboarding and finish the tutorial's Verify step.
:::

The Quick Start defaults the managed shared database to Dynamic credentials.
Do not use the Static-mode patch to switch an already-running Dynamic Keystone
to Static.

- The OpenBao `database` secrets engine is mounted at `database/mariadb` (the
  `setup-secret-engines.sh` bootstrap step). On a greenfield cluster this is
  already in place; on a brownfield cluster, re-apply the bootstrap scripts (see
  below).
- cert-manager and its `openbao-ca-issuer` ClusterIssuer are installed (they
  issue the per-ControlPlane mTLS client certificate the generator presents to
  the OpenBao listener).
- The External Secrets Operator can request ServiceAccount tokens
  (`serviceaccounts/token` `create`) so the generator can authenticate to OpenBao
  as the per-ControlPlane `keystone-db-creds` ServiceAccount.

## Migration steps

### 1. Re-apply the OpenBao bootstrap on brownfield clusters

The database engine mount, the `keystone-db` Kubernetes-auth role, and the
`keystone-db-dynamic` policy are added by the (idempotent) bootstrap scripts.
Re-apply them so a cluster provisioned before this change picks them up:

For a kind devstack, obtain and export `BAO_TOKEN` as described in
[Running the Full Bootstrap](../../reference/infrastructure/openbao-bootstrap.md#running-the-full-bootstrap).
That procedure reads the root token from `openbao-init-keys` and exports it for
the bootstrap scripts.

```bash
# From the repo root, with BAO_TOKEN exported (the OpenBao root token).
bash deploy/openbao/bootstrap/setup-secret-engines.sh
bash deploy/openbao/bootstrap/setup-auth.sh
bash deploy/openbao/bootstrap/setup-policies.sh
```

`make deploy-infra` runs these for you on a fresh kind cluster.

### 2. (Optional) Stage or reproduce a Static starting state

Dynamic is the default effective mode for a managed ControlPlane. To keep a
ControlPlane on the static credential while you onboard the engine, set:

```yaml
spec:
  infrastructure:
    database:
      credentialsMode: Static
```

A ControlPlane pinned to `Static` has no automatic per-ControlPlane KV seed.
Seed the path manually and keep the username aligned with the SQL user the
Keystone operator manages. The old seed was retired.

To reproduce a real Static-to-Dynamic migration on a fresh Quick Start
devstack, seed the static credential before applying the ControlPlane in Quick
Start Step 4. Use the inline per-service patch below so other services keep
their current mode. For this devstack, the static SQL username is
`controlplane-keystone`.

```bash
# Obtain BAO_TOKEN as described in Running the Full Bootstrap above.
printf 'Static Keystone DB password: '
IFS= read -r -s DB_PASSWORD
printf '\n'
printf '%s' "$DB_PASSWORD" | bao kv put \
  kv-v2/openstack/keystone/openstack/controlplane/db \
  username=controlplane-keystone password=-
unset DB_PASSWORD
```

After Quick Start Step 4 creates the ControlPlane, apply this patch before
Quick Start Step 5. It sets only Keystone's per-service mode, leaving other
services and ControlPlane fields unchanged. Do not run
`make deploy-infra WITH_CONTROLPLANE_CR=true` for this lab.

```bash
kubectl patch controlplane controlplane -n openstack --type=merge \
  --patch '{"spec":{"services":{"keystone":{"databaseCredentialsMode":"Static"}}}}'
```

Check that ESO synced the seeded username and the projected Keystone is Static:

```bash
kubectl wait externalsecret/controlplane-keystone-db-credentials -n openstack \
  --for=condition=Ready --timeout=5m
kubectl get secret controlplane-keystone-db-credentials -n openstack \
  -o jsonpath='{.data.username}' | base64 -d
kubectl get keystone controlplane-keystone -n openstack \
  -o jsonpath='{.spec.database.credentialsMode}{"\n"}'
```

Skip Quick Start Step 5 while Keystone is Static, then finish its Verify step.
For an existing brownfield ControlPlane that already runs Static, skip the
patch and use its existing static KV path.

The shared `spec.infrastructure.database.credentialsMode: Static` above stages
**every** service on the shared database at once. To scope the staging to
Keystone alone, set `spec.services.keystone.databaseCredentialsMode: Static`
instead. This per-service override lets another service, such as Glance, run
Dynamic while Keystone stays Static. An empty
override inherits the shared mode; clear it only when the shared mode is
Dynamic, or set the override to `Dynamic` to cut Keystone over when the shared
mode is Static.

### 3. Onboard the per-tenant database-engine role

Run this after the tenant's MariaDB is Ready. The script configures the
connection and role that issue short-lived Keystone users:

```bash
# BAO_TOKEN must be exported; <namespace>/<controlplane> identify the tenant.
bash deploy/openbao/bootstrap/setup-database-tenant.sh <namespace> <controlplane>
```

This writes:

- `database/mariadb/config/keystone-<namespace>`: the connection to the
  tenant's MariaDB, authenticated as root.
- `database/mariadb/roles/keystone-<namespace>`: the role that issues
  short-lived users (`default_ttl` 48h, `max_ttl` 72h by default; override with
  `DB_CREDS_DEFAULT_TTL` / `DB_CREDS_MAX_TTL`).

`make deploy-infra WITH_CONTROLPLANE=true WITH_CONTROLPLANE_CR=true` runs this
automatically for the bundled ControlPlane after its MariaDB becomes Ready.

### 4. Upgrade the operators and observe the cutover

Upgrade the c5c3 and keystone operators to a build that includes the dynamic
database integration. If Keystone currently runs Static, switch its per-service
mode to Dynamic after step 3 onboards the engine role:

```bash
kubectl patch controlplane controlplane -n openstack --type=merge \
  -p '{"spec":{"services":{"keystone":{"databaseCredentialsMode":"Dynamic"}}}}'
```

On the next reconcile the c5c3 operator projects the generator, ServiceAccount,
and Certificate, and the ExternalSecret switches to drawing from the generator.
If the ExternalSecret still targets a Secret populated by a Static sync, delete
that Secret once after the ExternalSecret switches to the generator. This
prompts ESO to write the engine-issued credential immediately instead of
waiting for the next refresh:

```bash
kubectl delete secret controlplane-keystone-db-credentials -n openstack
```

The c5c3 operator holds the Keystone child in Static mode until the Secret has
an engine-issued username. Watch for:

- The `{name}-keystone-db-credentials` ExternalSecret spec changing from static
  `data[].remoteRef` to `dataFrom[].sourceRef.generatorRef` (kind
  `VaultDynamicSecret`).
- The Kubernetes Secret's `username` becoming an engine-issued login (not
  `keystone`).
- A Keystone Deployment rollout: the operator sets a
  `keystone.c5c3.io/db-connection-hash` pod-template annotation in Dynamic mode,
  so a rotated credential rolls the Deployment (the DSN is consumed via the
  `OS_DATABASE__CONNECTION` env var, which only takes effect on a Pod restart).

Because the engine's GRANT overlaps any pre-existing operator-provisioned
`User`/`Grant` from the static deployment, Keystone keeps serving throughout.
The rolling restart is protected by the Keystone PodDisruptionBudget and moves
Keystone onto an engine-issued login without interrupting database service.

### 5. Retire the static credential

Once the ControlPlane reports `DBCredentialsReady=True` on the dynamic path and
Keystone is Ready:

1. Delete the leftover static MariaDB `User` and `Grant` CRs (they carry the
   `<controlplane>-keystone` login the engine no longer uses):

   ```bash
   kubectl delete user,grant <keystone-cr-name> -n <namespace> --ignore-not-found
   ```

2. Remove the retired static KV secret:

   ```bash
   # Inside the OpenBao pod, or with a bao client configured for it.
   bao kv metadata delete kv-v2/openstack/keystone/<namespace>/<controlplane>/db
   ```

The `push-keystone-db.hcl` PushSecret policy that stage (a) deferred is **not
needed** and is not created: a dynamic engine has no static password to push
back.

## Rollback

To revert to the static credential, set `credentialsMode: Static`, re-seed the
KV path (step 2), and re-create the `User`/`Grant` (the operator recreates them
on the next reconcile in Static mode). Roll back the operators if you also need
to remove the generator objects.

## Operational considerations

- **Rotation churn vs. lease headroom:** the DSN comes from an environment
  variable, so Keystone must restart to use a rotated credential. ESO creates a
  new credential on each refresh; OpenBao cannot renew the old credential in
  place. The 24h refresh interval limits rollouts to at most one per day. The
  48h `default_ttl` leaves a 24h window for pods to roll before OpenBao revokes
  the previous lease. A stalled rollout should page on-call before it becomes an
  outage.
  Raise `DB_CREDS_DEFAULT_TTL` / `DB_CREDS_MAX_TTL` (and the operator's refresh
  interval) further to trade churn against lease headroom; the PodDisruptionBudget
  and the surge-before-remove rollout strategy keep each roll zero-downtime.
- **Auth-token TTL bounds the lease:** OpenBao revokes a dynamic-secret lease
  together with the auth token that minted it, so the effective credential
  lifetime is `min(lease TTL, minting token TTL)`. The `keystone-db` auth role
  therefore pins its token TTLs to `DB_CREDS_MAX_TTL` (72h). When raising
  `DB_CREDS_*` beyond that, raise the role's `token_ttl`/`token_max_ttl` in
  lockstep. A shorter token silently drops the ephemeral MySQL user under a
  running Keystone long before the advertised lease end.
- **Revocation semantics:** revoking a lease runs `DROP USER`, which rejects
  *new* connections. Already-open sessions of a dropped user may persist until
  they disconnect.
- **ESO/OpenBao outage longer than the lease:** the leased credential
  expires before a refresh completes. Running Pods keep pooled connections, but new
  connections fail until ESO recovers. This surfaces as `DBCredentialsReady=False`
  via the ClusterSecretStore gate.

## See also

- [OpenBao Bootstrap reference](/reference/infrastructure/openbao-bootstrap):
  engines, auth roles, policies, and secret paths.
- [ControlPlane reconciler reference](/reference/c5c3/controlplane-reconciler):
  `reconcileDBCredentials` projection flow.

## Tested by

The CI e2e kind cluster asserts the destination state: the `VaultDynamicSecret`,
the owned `controlplane-keystone-db-credentials` ExternalSecret, and the
transient engine-issued login. Run the Chainsaw suite with:

```bash
chainsaw test --test-dir tests/e2e/c5c3/db-credential-scoping
```
