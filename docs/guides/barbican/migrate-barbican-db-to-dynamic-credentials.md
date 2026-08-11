---
title: Migrate Barbican DB to Dynamic Credentials
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Migrate Barbican DB to Dynamic Credentials

This guide takes a managed-mode ControlPlane from a long-lived **static**
Barbican database credential to **dynamic**, engine-issued credentials, without
database downtime. It is the operator-facing side of the OpenBao MariaDB database
secrets engine wired for the Barbican service DB user.

The credential this migration moves is the one Barbican's metadata schema is
reached with. The secret material itself lives in the service's OpenBao secret
store, which is a separate concern and is untouched here.

## What changes

- **Before:** the Barbican DB password is a long-lived value materialised from an
  OpenBao KV path (`openstack/barbican/{namespace}/{name}/db`) into the
  `{name}-barbican-db-credentials` Secret. Nothing seeds that path, so on the
  static branch an operator writes it and rotates it by hand.
- **After:** the c5c3 operator projects a per-ControlPlane
  [`VaultDynamicSecret`](https://external-secrets.io/) generator that reads
  short-lived credentials from the OpenBao database engine
  (`database/mariadb/creds/barbican-{namespace}`, where `{namespace}` is the
  Barbican service namespace, the ControlPlane's own namespace unless Barbican
  runs in a dedicated one). The External Secrets Operator re-issues a fresh lease
  before the previous one expires and materialises the current username and
  password into the same Secret. No long-lived static DB password remains at
  rest.

The engine issues an ephemeral MySQL user per lease (for example `v-kube-...`)
with `ALL PRIVILEGES` on the Barbican database and drops it at lease end.

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step. On this devstack the
ControlPlane is `controlplane` in `openstack`, so its projected Barbican is
`controlplane-barbican` and its DB-credential Secret is
`controlplane-barbican-db-credentials`. Those are the names the migration below
manages.
:::

- The ControlPlane declares `spec.services.barbican`. Without it the operator
  projects no Barbican child and no DB credential of either kind. See
  [Run Barbican on a Dedicated OpenBao](./barbican-dedicated-openbao.md).
- The OpenBao `database` secrets engine is mounted at `database/mariadb` (the
  `setup-secret-engines.sh` bootstrap step). A greenfield cluster already has it;
  on a brownfield cluster, re-apply the bootstrap scripts (see below).
- cert-manager and its `openbao-ca-issuer` ClusterIssuer are installed. They
  issue the per-ControlPlane mTLS client certificate the generator presents to
  the OpenBao listener.
- The External Secrets Operator can request ServiceAccount tokens
  (`serviceaccounts/token` `create`) so the generator can authenticate to OpenBao
  as the `barbican-db-creds` ServiceAccount.

## Migration steps

### 1. Re-apply the OpenBao bootstrap on brownfield clusters

The database engine mount, the `barbican-db` Kubernetes-auth role, and the
`barbican-db-dynamic` policy are written by the (idempotent) bootstrap scripts.
Re-apply them so a cluster provisioned before Barbican was onboarded picks them
up:

```bash
# From the repo root, with BAO_TOKEN exported (the OpenBao root token).
bash deploy/openbao/bootstrap/setup-secret-engines.sh
bash deploy/openbao/bootstrap/setup-auth.sh
bash deploy/openbao/bootstrap/setup-policies.sh
```

`make deploy-infra` runs these for you on a fresh kind cluster.

### 2. Onboard the per-tenant database-engine role

The engine role for a tenant only exists once its MariaDB is Ready and
`setup-database-tenant.sh` has configured the connection and role against it:

```bash
# BAO_TOKEN must be exported; <namespace>/<controlplane> identify the tenant.
bash deploy/openbao/bootstrap/setup-database-tenant.sh <namespace> <controlplane>
```

When the ControlPlane declares `spec.services.barbican` on the shared managed
database, the script provisions the Barbican leg:

- `database/mariadb/config/barbican-<barbican-namespace>`, the connection to the
  tenant's MariaDB, authenticated as root.
- `database/mariadb/roles/barbican-<barbican-namespace>`, the role that issues
  short-lived users on the `barbican` schema (`default_ttl` 48h, `max_ttl` 72h by
  default; override with `DB_CREDS_DEFAULT_TTL` / `DB_CREDS_MAX_TTL`).

The script reads the live ControlPlane spec on every run and skips a service the
CR does not declare, so adding `services.barbican` to an existing ControlPlane
means running it again.

When Barbican lives in a dedicated service namespace, that namespace's MariaDB
must be Ready first: the script resolves that namespace's root secret and fails
loudly otherwise. A ControlPlane whose Barbican declares a *dedicated* database is
skipped, because a dedicated Barbican database is Static-only. The c5c3 admission
webhook enforces the same rule from the other side and rejects
`spec.services.barbican.databaseCredentialsMode: Dynamic` on a Barbican that
declares `dedicatedBackingServices.database`. A Dynamic override on a brownfield
shared database (one with no `clusterRef`) is rejected for the same reason: no
engine role exists that could issue its credentials.

The bundled kind ControlPlane declares no `services.barbican`, so the automatic
onboarding under `make deploy-infra WITH_CONTROLPLANE=true WITH_CONTROLPLANE_CR=true`
covers no Barbican leg. Run the script yourself after adding the block.

### 3. (Optional) Stage the cutover with `databaseCredentialsMode: Static`

Dynamic is the default effective mode for a managed ControlPlane on the shared
database. To keep Barbican on a static credential, for example while Keystone and
Glance already run Dynamic, pin the per-service override so the blast radius
stays on Barbican:

```yaml
spec:
  services:
    barbican:
      databaseCredentialsMode: Static
```

The shared `spec.infrastructure.database.credentialsMode: Static` opts out
ControlPlane-wide instead; the per-service override scopes the opt-out to
Barbican alone. Nothing seeds the static KV path, so while staging you must write
`kv-v2/openstack/barbican/<namespace>/<controlplane>/db` (`username`, `password`)
by hand, with `username` set to the Barbican child's name
(`<controlplane>-barbican`), which is the login the operator's MariaDB
`User`/`Grant` provisions. Remove the override (or set it to `Dynamic`) to cut
over.

### 4. Upgrade the operators and observe the cutover

Upgrade the c5c3 and barbican operators to a build that includes the dynamic
engine wiring. On the next reconcile the c5c3 operator projects the generator,
ServiceAccount, and Certificate, and the ExternalSecret switches to drawing from
the generator.

On a cluster that ran the static path, delete the materialised credential Secret
once so the engine-issued value is the only thing that can ever be read from it:

```bash
kubectl delete secret <controlplane>-barbican-db-credentials -n <namespace>
```

The ExternalSecret is updated in place (same name, same target Secret), so until
ESO's first generator-backed sync lands the Secret still holds whatever the
previous static sync wrote. Deleting it forces ESO to re-materialise from the
generator immediately instead of at the next refresh.

You do not have to get this right for Barbican to stay up: the c5c3 operator will
not project `credentialsMode: Dynamic` onto the Barbican child until that
ExternalSecret reports Ready **and** the Secret behind it carries an
engine-issued username. While either is outstanding, `BarbicanReady` is `False`
with reason `WaitingForBarbicanDBCredential`, the running child keeps its current
mode, and the message names either the
`database/mariadb/creds/barbican-<namespace>` path from step 2 or the stale
username it found. A ControlPlane stuck on the path message has not been
onboarded; re-run step 2. One stuck on the username message is waiting for the
sync this deletion shortcuts.

Watch for:

- The `controlplane-barbican-db-credentials` ExternalSecret spec changing from
  static `data[].remoteRef` to `dataFrom[].sourceRef.generatorRef` (kind
  `VaultDynamicSecret`).
- The materialised Secret's `username` becoming an engine-issued login rather
  than `controlplane-barbican`.
- A Barbican Deployment rollout: the operator stamps a
  `barbican.c5c3.io/db-connection-hash` pod-template annotation, so a rotated
  credential rolls the Deployment. The DSN travels in the
  `OS_DATABASE__CONNECTION` environment variable, which only takes effect on a
  Pod restart.

The engine's `GRANT` overlaps the pre-existing operator-provisioned `User` and
`Grant` from the static deployment, so Barbican keeps serving throughout. The
rolling restart moves it onto an engine-issued login one pod at a time; at two or
more replicas the API stays answerable across the roll.

### 5. Retire the static credential

Once the ControlPlane reports `BarbicanReady=True` on the dynamic path:

1. Delete the leftover static MariaDB `User` and `Grant` CRs (they carry the
   long-lived `<controlplane>-barbican` login the engine no longer uses):

   ```bash
   kubectl delete user,grant <barbican-cr-name> -n <namespace> --ignore-not-found
   ```

2. Remove the retired static KV secret, if step 3 ever seeded it:

   ```bash
   # Inside the OpenBao pod, or with a bao client configured for it.
   bao kv metadata delete kv-v2/openstack/barbican/<namespace>/<controlplane>/db
   ```

## Rollback

To revert to the static credential, set the mode back to `Static` (the
per-service `spec.services.barbican.databaseCredentialsMode` override, or the
shared `spec.infrastructure.database.credentialsMode`), re-seed the KV path
(step 3), and let the next Static-mode reconcile re-create the `User`/`Grant`.
Roll back the operators if you also need to remove the generator objects.

## Operational considerations

- **Rotation churn against lease headroom:** the DSN is consumed via an
  environment variable, so a rotated engine credential only takes effect on a Pod
  restart and Barbican rolls each time the ExternalSecret re-issues the
  credential. Every refresh is a new credential; there is no stable value to
  renew in place. The defaults balance two concerns: the 24h refresh interval
  keeps the roll cadence to at most once a day, while the 48h `default_ttl`
  leaves a 24h gap (`default_ttl` minus refresh) in which to roll the pods before
  the previous, still-in-use lease is revoked. Raise `DB_CREDS_DEFAULT_TTL` /
  `DB_CREDS_MAX_TTL` (and the operator's refresh interval) to trade churn against
  lease headroom. Run more than one replica if the roll must not interrupt the
  API: the Deployment uses the default rolling update, and a single-replica
  Barbican is unavailable for the length of one pod restart.
- **Auth-token TTL bounds the lease:** OpenBao revokes a dynamic-secret lease
  together with the auth token that minted it, so the effective credential
  lifetime is `min(lease TTL, minting token TTL)`. The `barbican-db` auth role
  therefore pins `token_ttl` and `token_max_ttl` to 72h, the `DB_CREDS_MAX_TTL`
  default. When raising `DB_CREDS_*` beyond that, raise the role's TTLs in
  lockstep. A shorter token silently drops the ephemeral MySQL user under a
  running Barbican long before the advertised lease end.
- **The db-clean CronJob rides the same credential.** Barbican never
  hard-deletes, so a `{name}-db-clean` CronJob runs on a schedule of its own. It
  reads the DSN at pod start from the derived `{name}-db-connection` Secret, which
  the operator re-renders from each engine-issued credential. A run that starts
  after a lease was revoked and before the re-render lands fails and is retried on
  the next schedule; it removes no rows in between.
- **Revocation semantics:** revoking a lease runs `DROP USER`, which rejects new
  connections. Already-open sessions of a dropped user may persist until they
  disconnect.
- **ESO or OpenBao outage longer than the lease:** the materialised credential
  expires before a refresh lands. Running Pods keep pooled connections, but new
  connections fail until ESO recovers. This surfaces through the Barbican child's
  own database readiness and, ControlPlane-side, as `BarbicanReady=False`.

## See also

- [OpenBao Bootstrap reference](../../reference/infrastructure/openbao-bootstrap.md) —
  engines, auth roles, policies, and secret paths.
- [Barbican CRD API Reference](../../reference/barbican/barbican-crd.md) —
  `spec.database.credentialsMode` and the fields the ControlPlane projects onto
  the child.
- [Migrate Placement DB to Dynamic Credentials](../placement/migrate-placement-db-to-dynamic-credentials.md) —
  the sibling migration for the Placement service DB user.

## Tested by

The dynamic, engine-issued per-ControlPlane database credential this guide
migrates to (the projected Dynamic mode, the `VaultDynamicSecret`, the
generator-backed `controlplane-barbican-db-credentials` ExternalSecret, and the
transient engine-issued login) is asserted on the live CI e2e kind cluster by
this chainsaw suite:

```bash
chainsaw test --test-dir tests/e2e/c5c3/full-controlplane-keystone
```
