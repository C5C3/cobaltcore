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
database credential to **dynamic**, engine-issued credentials, without taking
the database offline. It is the operator-facing side of the OpenBao MariaDB
database secrets engine wired for the Cinder service DB user.

The credential this migration moves lets Cinder access the database that stores
volume, snapshot, and backup metadata. Volume data on attached backends is a
separate concern and stays untouched.

## What changes

- **Before:** the Cinder DB password is a long-lived value copied from an
  OpenBao KV path (`openstack/cinder/{namespace}/{name}/db`, where `namespace`
  is the Cinder service namespace and `name` is the ControlPlane name) into the
  `{name}-cinder-db-credentials` Secret. Nothing seeds that path, so on the
  static branch an operator writes it and rotates it by hand.
- **After:** the c5c3 operator projects a per-ControlPlane
  [`VaultDynamicSecret`](https://external-secrets.io/) generator that reads
  short-lived credentials from the OpenBao database engine
  (`database/mariadb/creds/cinder-{namespace}`, where `{namespace}` is the
  Cinder service namespace, the ControlPlane's own namespace unless Cinder runs
  in a dedicated one). The External Secrets Operator re-issues a fresh lease
  before the previous one expires and stores its username and password in the
  Kubernetes Secret. Dynamic mode does not read a static password from the
  OpenBao KV path. Any existing static KV value remains until you delete it in
  step 5.

The engine issues an ephemeral MySQL user per lease (for example `v-kube-...`)
with `ALL PRIVILEGES` on the `cinder` database and drops it at lease end. That
schema name is the one the projection forces onto the child, so the engine role
and the child always agree on which database the credential opens.

The figure shows both chains. `{svc}` stands for `cinder` here, and `{cp}` is
`controlplane` on the devstack.

![Database credentials in two modes. Static: a person writes username and password to an OpenBao KV path, an ExternalSecret copies them through the secret store into the Secret {cp}-{svc}-db-credentials, and MariaDB User and Grant resources create one long-lived SQL user from it. Dynamic: a VaultDynamicSecret generator logs in to OpenBao as the ServiceAccount {svc}-db-creds over a client certificate and draws a user from database/mariadb/creds/{svc}-{ns}, the database engine creates that user in MariaDB for one lease, and a changed credential rolls the Deployment through the db-connection-hash annotation. In both modes the service operator builds the DSN Secret that the pods read. A time strip shows the 24 hour refresh inside the 48 hour default TTL and the 72 hour maximum TTL.](../../diagrams/secrets-db-credentials.svg)

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_NFS=true make deploy-infra
```

Follow that tutorial through the block-storage block of Step 4 and the
**Create a first volume** check in Step 7, so the projected `controlplane-cinder`
child is `Ready` in `openstack` with `nfs1` and `nfsbk` attached. Every resource
name in the examples below is one that devstack produces.
:::

The ControlPlane must declare `spec.services.cinder` with at least one backend
entry; admission rejects an empty `services.cinder.backends` list. Without the
service block, the operator projects no Cinder child or DB credential. The c5c3
operator also projects the Cinder child and its backend resources. Direct edits
to those children are reverted on reconcile, so make changes under
`services.cinder` on the ControlPlane. The devstack verification below uses NFS
storage. See
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
# BAO_TOKEN must be exported; these identify the ControlPlane.
bash deploy/openbao/bootstrap/setup-database-tenant.sh \
  <controlplane-namespace> <controlplane>
```

When the ControlPlane declares `spec.services.cinder` on the shared managed
database, the script configures the Cinder engine connection and role:

- `database/mariadb/config/cinder-<cinder-namespace>`, the connection to the
  tenant's MariaDB, authenticated as root.
- `database/mariadb/roles/cinder-<cinder-namespace>`, the role that issues
  short-lived users on the `cinder` schema (`default_ttl` 48h, `max_ttl` 72h by
  default; override with `DB_CREDS_DEFAULT_TTL` / `DB_CREDS_MAX_TTL`).

The script reads the live ControlPlane spec on every run and skips a service the
CR does not declare. The bundled kind ControlPlane ships without a
`services.cinder` block, so automatic onboarding under
`make deploy-infra WITH_CONTROLPLANE=true WITH_CONTROLPLANE_CR=true` covers no
Cinder database role. Run the script again after adding the block-storage block.

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
`kv-v2/openstack/cinder/<cinder-namespace>/<controlplane>/db` (`username`,
`password`) by hand, with `username` set to the Cinder child's name
(`<controlplane>-cinder`), which is the login the operator's MariaDB
`User`/`Grant` provisions. Remove the override (or set it to `Dynamic`) to cut
over.

### 4. Upgrade the operators and observe the cutover

Upgrade the c5c3 and cinder operators to a build that includes the dynamic
database integration. On the next reconcile the c5c3 operator projects the generator,
ServiceAccount, and Certificate, and the ExternalSecret switches to drawing from
the generator.

On a cluster that ran the static path, delete the credential Secret once to
remove the old value before ESO syncs the engine-issued credential:

```bash
# Use the Cinder service namespace; it may differ from the
# ControlPlane namespace.
kubectl delete secret <controlplane>-cinder-db-credentials \
  -n <cinder-namespace>
```

The ExternalSecret is updated in place (same name, same target Secret), so until
ESO completes its first generator-backed sync, the Secret still holds
whatever the previous static sync wrote. Deleting it forces ESO to sync from the
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
- The Kubernetes Secret's `username` becoming an engine-issued login rather
  than `controlplane-cinder`.
- A rollout of every Cinder Deployment: the API, the scheduler, one
  `cinder-volume` per attached backend, and the backup service. The operator
  sets a `cinder.c5c3.io/db-connection-hash` pod-template annotation on all of
  them, so a rotated credential rolls them. The DSN travels in the
  `OS_DATABASE__CONNECTION` environment variable, read from the derived
  `{name}-db-connection` Secret, which only takes effect on a Pod restart.

The engine's `GRANT` overlaps the pre-existing operator-provisioned `User` and
`Grant` from the static deployment, so the old login can remain until workloads
have picked up the engine-issued credential. Rotation restarts the API, scheduler,
volume, and backup workloads; see
[Operational considerations](#operational-considerations) for their availability
impact.

### 5. Retire the static credential

Once the ControlPlane reports `CinderReady=True` on the dynamic path:

1. Delete the leftover static MariaDB `User` and `Grant` CRs (they carry the
   long-lived `<controlplane>-cinder` login the engine no longer uses):

   ```bash
   # Use the Cinder service namespace.
   kubectl delete user,grant <cinder-cr-name> \
     -n <cinder-namespace> --ignore-not-found
   ```

2. Remove the retired static KV secret, if step 3 ever seeded it:

   ```bash
   # Inside the OpenBao pod, or with a bao client configured for it.
   bao kv metadata delete \
     kv-v2/openstack/cinder/<cinder-namespace>/<controlplane>/db
   ```

## Rollback

To revert to the static credential, set the mode back to `Static` (the
per-service `spec.services.cinder.databaseCredentialsMode` override, or the
shared `spec.infrastructure.database.credentialsMode`), re-seed the KV path
(step 3), and let the next Static-mode reconcile re-create the `User`/`Grant`.
The c5c3 operator switches the ExternalSecret back to the KV path and removes
the dynamic credential objects during that reconcile. Wait for the static
credential to sync and the child to return to Static mode before rolling back
operator versions.

## Operational considerations

- **Credential rotation restarts Cinder workloads.** The DSN is consumed via an
  environment variable, so a rotated engine credential only takes effect after
  a Pod restart. Each ExternalSecret refresh issues a new credential and changes
  the DSN hash on the API, scheduler, every `cinder-volume`, and backup
  Deployment. The scheduler has one replica by default. Each volume and backup
  Deployment is pinned to one replica with `strategy.type: Recreate`, so those
  services are unavailable while their Pod restarts. The API's
  PodDisruptionBudget limits voluntary evictions; it does not guarantee
  availability during a credential-triggered rollout. Keep enough API replicas
  and cluster capacity if uninterrupted API service matters. The defaults keep
  rotation churn bounded: ESO refreshes every 24h, while the 48h `default_ttl`
  leaves a 24h gap (`default_ttl` minus refresh) for workloads to pick up the new
  credential before the previous lease is revoked. Changing either
  `DB_CREDS_DEFAULT_TTL` or `DB_CREDS_MAX_TTL` requires coordinating the
  operator's refresh interval; changing that interval requires an operator
  change.
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
- **ESO or OpenBao outage longer than the lease:** the issued credential expires
  before ESO can renew it. Existing database sessions may remain open, but new
  connections fail until ESO can issue a fresh credential. Do not rely on
  `CinderReady` alone to prove the lease is valid: `DatabaseReady` tracks
  database provisioning and schema work, and the API `/healthcheck` does not
  test database connectivity. Monitor ESO/OpenBao and Cinder database errors,
  and restore credential issuance before the lease expires.

## See also

- [OpenBao Bootstrap reference](../../reference/infrastructure/openbao-bootstrap.md):
  engines, auth roles, policies, and secret paths.
- [ControlPlane reconciler reference](../../reference/c5c3/controlplane-reconciler.md#reconcilecinder):
  the Cinder projection and the conditions it holds on.
- [Cinder CRD API Reference](../../reference/cinder/cinder-crd.md#spec):
  `spec.database.credentialsMode` and the fields the ControlPlane projects onto
  the child.
- [Migrate Barbican DB to Dynamic Credentials](../barbican/migrate-barbican-db-to-dynamic-credentials.md):
  the sibling migration for the Barbican service DB user.

## Tested by

The live CI suite below exercises Cinder through the projected API by creating
volumes and backups, testing an encrypted volume, and checking the image-volume
cache. It does not assert the database credential generator, the
generator-backed ExternalSecret, or the migration readiness gate.

```bash
chainsaw test --test-dir tests/e2e/c5c3/full-controlplane-keystone
```
