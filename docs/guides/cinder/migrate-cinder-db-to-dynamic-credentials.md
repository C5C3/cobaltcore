---
title: Migrate Cinder DB to Dynamic Credentials
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Migrate Cinder DB to Dynamic Credentials

This guide takes a managed-mode ControlPlane from a long-lived **static** Cinder
database credential to **dynamic**, engine-issued credentials, without database
downtime. It is the operator-facing side of the OpenBao MariaDB database secrets
engine wired for the Cinder service DB user.

The credential this migration moves is the one Cinder's volume, snapshot and
backup metadata is reached with. The volume files on the attached NFS exports
are a separate concern and stay untouched.

## What changes

- **Before:** the Cinder DB password is a long-lived value materialised from an
  OpenBao KV path (`openstack/cinder/{namespace}/{name}/db`) into the
  `{name}-cinder-db-credentials` Secret. Nothing seeds that path, so on the
  static branch an operator writes it and rotates it by hand.
- **After:** the c5c3 operator projects a per-ControlPlane
  [`VaultDynamicSecret`](https://external-secrets.io/) generator that reads
  short-lived credentials from the OpenBao database engine
  (`database/mariadb/creds/cinder-{namespace}`, where `{namespace}` is the
  Cinder service namespace, the ControlPlane's own namespace unless Cinder runs
  in a dedicated one). The External Secrets Operator re-issues a fresh lease
  before the previous one expires and materialises the current username and
  password into the same Secret. No long-lived static DB password remains at
  rest.

The engine issues an ephemeral MySQL user per lease (for example `v-kube-...`)
with `ALL PRIVILEGES` on the `cinder` database and drops it at lease end. That
schema name is the one the projection forces onto the child, so the engine role
and the child always agree on which database the credential opens.

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

- The ControlPlane declares `spec.services.cinder` with at least one backend
  entry. Without it the operator projects no Cinder child and no DB credential
  of either kind. See
  [Attach an NFS Backend to Cinder](./attach-an-nfs-backend.md).
- The OpenBao `database` secrets engine is mounted at `database/mariadb` (the
  `setup-secret-engines.sh` bootstrap step). A greenfield cluster already has
  it; on a brownfield cluster, re-apply the bootstrap scripts (see below).
- cert-manager and its `openbao-ca-issuer` ClusterIssuer are installed. They
  issue the per-ControlPlane mTLS client certificate the generator presents to
  the OpenBao listener.
- The External Secrets Operator can request ServiceAccount tokens
  (`serviceaccounts/token` `create`) so the generator can authenticate to
  OpenBao as the `cinder-db-creds` ServiceAccount.

## Migration steps

### 1. Re-apply the OpenBao bootstrap on brownfield clusters

The database engine mount, the `cinder-db` Kubernetes-auth role, and the
`cinder-db-dynamic` policy are written by the (idempotent) bootstrap scripts.
Re-apply them so a cluster provisioned before Cinder was onboarded picks them
up:

```bash
# From the repo root, with BAO_TOKEN exported (the OpenBao root token).
bash deploy/openbao/bootstrap/setup-secret-engines.sh
bash deploy/openbao/bootstrap/setup-auth.sh
bash deploy/openbao/bootstrap/setup-policies.sh
```

`make deploy-infra` runs these for you on a fresh kind cluster.

The `cinder-db` role binds the fixed ServiceAccount name `cinder-db-creds` in
any namespace. The `cinder-db-dynamic` policy behind it templates the readable
path to the caller's own service-account namespace, so a token minted for one
tenant reads that tenant's creds path and no other. The fixed SA name is what
keeps a Cinder generator off the Keystone, Glance, Placement, Barbican and
Neutron creds paths when those services share a namespace with it.

### 2. Onboard the per-tenant database-engine role

The engine role for a tenant only exists once its MariaDB is Ready and
`setup-database-tenant.sh` has configured the connection and role against it:

```bash
# BAO_TOKEN must be exported; <namespace>/<controlplane> identify the tenant.
bash deploy/openbao/bootstrap/setup-database-tenant.sh <namespace> <controlplane>
```

When the ControlPlane declares `spec.services.cinder` on the shared managed
database, the script provisions the Cinder leg:

- `database/mariadb/config/cinder-<cinder-namespace>`, the connection to the
  tenant's MariaDB, authenticated as root.
- `database/mariadb/roles/cinder-<cinder-namespace>`, the role that issues
  short-lived users on the `cinder` schema (`default_ttl` 48h, `max_ttl` 72h by
  default; override with `DB_CREDS_DEFAULT_TTL` / `DB_CREDS_MAX_TTL`).

The script reads the live ControlPlane spec on every run and skips a service the
CR does not declare. The bundled kind ControlPlane ships without a
`services.cinder` block, so the automatic onboarding under
`make deploy-infra WITH_CONTROLPLANE=true WITH_CONTROLPLANE_CR=true` covers no
Cinder leg: run the script again after the block-storage block is added.

When Cinder lives in a dedicated service namespace, that namespace's MariaDB
must be Ready first. The script resolves that namespace's root secret and fails
loudly otherwise. A ControlPlane whose Cinder declares a *dedicated* database is
skipped, because a dedicated Cinder database is Static-only. The c5c3 admission
webhook enforces the same rule from the other side and rejects
`spec.services.cinder.databaseCredentialsMode: Dynamic` on a Cinder that
declares `dedicatedBackingServices.database`. A Dynamic override on a brownfield
shared database (one with no `clusterRef`) is rejected for the same reason: no
engine role exists that could issue its credentials.

### 3. (Optional) Stage the cutover with `databaseCredentialsMode: Static`

Dynamic is the default effective mode for a managed ControlPlane on the shared
database. To keep Cinder on a static credential, for example while Keystone and
Glance already run Dynamic, pin the per-service override so the blast radius
stays on Cinder:

```yaml
spec:
  services:
    cinder:
      databaseCredentialsMode: Static
```

The shared `spec.infrastructure.database.credentialsMode: Static` opts out
ControlPlane-wide instead; the per-service override scopes the opt-out to Cinder
alone. Nothing seeds the static KV path, so while staging you must write
`kv-v2/openstack/cinder/<namespace>/<controlplane>/db` (`username`, `password`)
by hand, with `username` set to the Cinder child's name
(`<controlplane>-cinder`), which is the login the operator's MariaDB
`User`/`Grant` provisions. Remove the override (or set it to `Dynamic`) to cut
over.

### 4. Upgrade the operators and observe the cutover

Upgrade the c5c3 and cinder operators to a build that includes the dynamic
engine wiring. On the next reconcile the c5c3 operator projects the generator,
ServiceAccount, and Certificate, and the ExternalSecret switches to drawing from
the generator.

On a cluster that ran the static path, delete the materialised credential Secret
once so the engine-issued value is the only thing that can ever be read from it:

```bash
kubectl delete secret <controlplane>-cinder-db-credentials -n <namespace>
```

The ExternalSecret is updated in place (same name, same target Secret), so until
ESO's first generator-backed sync lands the Secret still holds whatever the
previous static sync wrote. Deleting it forces ESO to re-materialise from the
generator immediately instead of at the next refresh.

You do not have to get this right for Cinder to stay up: the c5c3 operator will
not project `credentialsMode: Dynamic` onto the Cinder child until that
ExternalSecret reports Ready **and** the Secret behind it carries an
engine-issued username. While either is outstanding, `CinderReady` is `False`
with reason `WaitingForCinderDBCredential`, the running child keeps its current
mode, and the message names either the
`database/mariadb/creds/cinder-<namespace>` path from step 2 or the stale
username it found. A ControlPlane stuck on the path message has not been
onboarded; re-run step 2. One stuck on the username message is waiting for the
sync this deletion shortcuts.

Watch for:

- The `controlplane-cinder-db-credentials` ExternalSecret spec changing from
  static `data[].remoteRef` to `dataFrom[].sourceRef.generatorRef` (kind
  `VaultDynamicSecret`).
- The materialised Secret's `username` becoming an engine-issued login rather
  than `controlplane-cinder`.
- A rollout of every Cinder Deployment: the API, the scheduler, one
  `cinder-volume` per attached backend, and the backup service. The operator
  stamps a `cinder.c5c3.io/db-connection-hash` pod-template annotation on all of
  them, so a rotated credential rolls them. The DSN travels in the
  `OS_DATABASE__CONNECTION` environment variable, read from the derived
  `{name}-db-connection` Secret, which only takes effect on a Pod restart.

The engine's `GRANT` overlaps the pre-existing operator-provisioned `User` and
`Grant` from the static deployment, so Cinder keeps serving through the roll.
The API Deployment scales horizontally and carries a PodDisruptionBudget, so it
stays answerable; the single-pod services are covered under
[Operational considerations](#operational-considerations).

### 5. Retire the static credential

Once the ControlPlane reports `CinderReady=True` on the dynamic path:

1. Delete the leftover static MariaDB `User` and `Grant` CRs (they carry the
   long-lived `<controlplane>-cinder` login the engine no longer uses):

   ```bash
   kubectl delete user,grant <cinder-cr-name> -n <namespace> --ignore-not-found
   ```

2. Remove the retired static KV secret, if step 3 ever seeded it:

   ```bash
   # Inside the OpenBao pod, or with a bao client configured for it.
   bao kv metadata delete kv-v2/openstack/cinder/<namespace>/<controlplane>/db
   ```

## Rollback

To revert to the static credential, set the mode back to `Static` (the
per-service `spec.services.cinder.databaseCredentialsMode` override, or the
shared `spec.infrastructure.database.credentialsMode`), re-seed the KV path
(step 3), and let the next Static-mode reconcile re-create the `User`/`Grant`.
Roll back the operators if you also need to remove the generator objects.

## Operational considerations

- **The volume and backup services restart on every rotation.** The DSN is
  consumed via an environment variable, so a rotated engine credential only
  takes effect on a Pod restart, and Cinder rolls each time the ExternalSecret
  re-issues the credential. Every refresh is a new credential; there is no
  stable value to renew in place. Each `cinder-volume` Deployment is pinned to
  one replica with `strategy.type: Recreate`, because two processes under one
  host identity would have the same volume state open twice, and the backup
  Deployment carries the same pin. A rotation therefore takes each of them down
  for the length of one pod restart. The defaults keep that churn bounded: the
  24h refresh interval holds the cadence to at most once a day, while the 48h
  `default_ttl` leaves a 24h gap (`default_ttl` minus refresh) in which to roll
  the pods before the previous, still-in-use lease is revoked. Raise
  `DB_CREDS_DEFAULT_TTL` / `DB_CREDS_MAX_TTL` (and the operator's refresh
  interval) to trade churn against lease headroom.
- **Auth-token TTL bounds the lease:** OpenBao revokes a dynamic-secret lease
  together with the auth token that minted it, so the effective credential
  lifetime is `min(lease TTL, minting token TTL)`. The `cinder-db` auth role
  therefore pins `token_ttl` and `token_max_ttl` to 72h, the `DB_CREDS_MAX_TTL`
  default. When raising `DB_CREDS_*` beyond that, raise the role's TTLs in
  lockstep. A shorter token silently drops the ephemeral MySQL user under a
  running Cinder long before the advertised lease end.
- **No per-user connection cap on the dynamic path.** In Static mode the
  operator sizes the MariaDB `User` CR's `max_user_connections` for the CR's
  topology, counting the API workers, the schedulers, one connection pair per
  attached backend, the backup service, and headroom for a migration Job. The
  engine's `CREATE USER` statement sets no cap, so once Cinder runs on an
  engine-issued login the server's own `max_connections` is the only bound.
- **The db-purge CronJob rides the same credential.** Cinder soft-deletes, so a
  `{name}-db-purge` CronJob sweeps the rows on a schedule of its own (daily at
  `1 0 * * *` by default). It reads the DSN at pod start from the derived
  `{name}-db-connection` Secret, which the operator re-renders from each
  engine-issued credential. A run that starts between a revocation and that
  re-render fails and purges no rows.
- **Revocation semantics:** revoking a lease runs `DROP USER`, which rejects new
  connections. Already-open sessions of a dropped user may persist until they
  disconnect.
- **ESO or OpenBao outage longer than the lease:** the materialised credential
  expires before a refresh lands. Running Pods keep pooled connections, but new
  connections fail until ESO recovers. This surfaces through the Cinder child's
  own `DatabaseReady` and, ControlPlane-side, as `CinderReady=False`.

## See also

- [OpenBao Bootstrap reference](../../reference/infrastructure/openbao-bootstrap.md) —
  engines, auth roles, policies, and secret paths.
- [ControlPlane reconciler reference](../../reference/c5c3/controlplane-reconciler.md#reconcilecinder) —
  the Cinder projection and the conditions it holds on.
- [Cinder CRD API Reference](../../reference/cinder/cinder-crd.md#spec) —
  `spec.database.credentialsMode` and the fields the ControlPlane projects onto
  the child.
- [Migrate Barbican DB to Dynamic Credentials](../barbican/migrate-barbican-db-to-dynamic-credentials.md) —
  the sibling migration for the Barbican service DB user.

## Tested by

The dynamic, engine-issued per-ControlPlane database credential this guide
migrates to is asserted on the live CI e2e kind cluster by the chainsaw suite
below. Its block-storage link waits for `CinderReady`, then reads the projected
child's `spec.database.credentialsMode` back as `Dynamic`, checks that the
`controlplane-cinder-db-credentials` ExternalSecret draws from a
`VaultDynamicSecret` generator and carries no static `spec.data` refs, reads the
generator's path back as `database/mariadb/creds/cinder-<namespace>`, confirms
the `cinder-db-creds` ServiceAccount exists, and fails the run if the username in
the materialised Secret is empty or still the placeholder `cinder`.

```bash
chainsaw test --test-dir tests/e2e/c5c3/full-controlplane-keystone
```
