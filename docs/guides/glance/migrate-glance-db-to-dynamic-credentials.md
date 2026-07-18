---
title: Migrate Glance DB to Dynamic Credentials
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Migrate Glance DB to Dynamic Credentials

This guide takes a managed-mode ControlPlane from a long-lived **static** Glance
database credential to **dynamic**, engine-issued credentials, without database
downtime. It is the operator-facing side of the OpenBao MariaDB database secrets
engine wired for the Glance service DB user.

## What changes

- **Before:** the Glance DB password is a long-lived value materialised from an
  OpenBao KV path (`openstack/glance/{namespace}/{name}/db`) into the
  `{name}-glance-db-credentials` Secret. It is only rotated when an operator
  rotates it.
- **After:** the c5c3 operator projects a per-ControlPlane
  [`VaultDynamicSecret`](https://external-secrets.io/) generator that reads
  short-lived credentials from the OpenBao database engine
  (`database/mariadb/creds/glance-{namespace}`, where `{namespace}` is the Glance
  service namespace — the ControlPlane's own namespace unless Glance runs in a
  dedicated one). The External Secrets Operator re-issues a fresh lease before
  the previous one expires and materialises the current username and password
  into the same Secret. No long-lived static DB password remains at rest.

The engine issues an ephemeral MySQL user per lease (for example `v-kube-...`)
with `ALL PRIVILEGES` on the Glance database and drops it at lease end.

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step. On this devstack the
ControlPlane is `controlplane` in `openstack`, so its projected Glance is
`controlplane-glance` and its DB-credential Secret is
`controlplane-glance-db-credentials` — the names the migration below manages.
:::

- The OpenBao `database` secrets engine is mounted at `database/mariadb` (the
  `setup-secret-engines.sh` bootstrap step). On a greenfield cluster this is
  already in place; on a brownfield cluster, re-apply the bootstrap scripts (see
  below).
- cert-manager and its `openbao-ca-issuer` ClusterIssuer are installed (they
  issue the per-ControlPlane mTLS client certificate the generator presents to
  the OpenBao listener).
- The External Secrets Operator can request ServiceAccount tokens
  (`serviceaccounts/token` `create`) so the generator can authenticate to OpenBao
  as the per-ControlPlane `glance-db-creds` ServiceAccount.

## Migration steps

### 1. Re-apply the OpenBao bootstrap on brownfield clusters

The database engine mount, the `glance-db` Kubernetes-auth role, and the
`glance-db-dynamic` policy are added by the (idempotent) bootstrap scripts.
Re-apply them so a cluster provisioned before this change picks them up:

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

When the ControlPlane declares `spec.services.glance` on the shared managed
database, the script provisions the Glance leg:

- `database/mariadb/config/glance-<glance-namespace>` — the connection to the
  tenant's MariaDB, authenticated as root.
- `database/mariadb/roles/glance-<glance-namespace>` — the role that issues
  short-lived users (`default_ttl` 48h, `max_ttl` 72h by default; override with
  `DB_CREDS_DEFAULT_TTL` / `DB_CREDS_MAX_TTL`).

When Glance lives in a dedicated service namespace, that namespace's MariaDB must
be Ready first — the script resolves that namespace's root secret and fails
loudly otherwise. A ControlPlane whose Glance declares a *dedicated* database is
skipped: a dedicated Glance database is Static-only.

`make deploy-infra WITH_CONTROLPLANE=true WITH_CONTROLPLANE_CR=true` runs this
automatically for the bundled ControlPlane after its MariaDB becomes Ready.

### 3. (Optional) Stage the cutover with `databaseCredentialsMode: Static`

Dynamic is the default effective mode for a managed ControlPlane on the shared
database. To keep Glance on the static credential — for example while Keystone
already runs Dynamic — pin the per-service override so the blast radius stays on
Glance:

```yaml
spec:
  services:
    glance:
      databaseCredentialsMode: Static
```

The shared `spec.infrastructure.database.credentialsMode: Static` opts out
ControlPlane-wide instead; the per-service override scopes the opt-out to Glance
alone. A ControlPlane staged on `Static` after this change no longer has its
static KV path seeded automatically (the per-ControlPlane seed is retired), so
you must seed `kv-v2/openstack/glance/<namespace>/<controlplane>/db` (`username`,
`password`) by hand while staging. Remove the override (or set it to `Dynamic`)
to cut over.

### 4. Upgrade the operators and observe the cutover

Upgrade the c5c3 and glance operators to a build that includes the dynamic engine
wiring. On the next reconcile the c5c3 operator projects the generator,
ServiceAccount, and Certificate, and the ExternalSecret switches to drawing from
the generator.

On a cluster that ran the static path, delete the materialised credential Secret
once so the engine-issued value is the only thing that can ever be read from it:

```bash
kubectl delete secret <controlplane>-glance-db-credentials -n <namespace>
```

The ExternalSecret switches to the generator in place — same name, same target
Secret — so until ESO's first generator-backed sync lands, the Secret still holds
whatever the previous static sync wrote, including the retired `username=glance`
placeholder the old bootstrap seeded. That name is a *syntactically valid*
username naming a MySQL user that has never existed (the static login is the
Glance CR name, `<controlplane>-glance`). Deleting the Secret forces ESO to
re-materialise it from the generator immediately instead of at the next refresh.

You do not have to get this right for Glance to stay up: the c5c3 operator will
not project `credentialsMode: Dynamic` onto the Glance child until that
ExternalSecret reports Ready **and** the Secret behind it carries an engine-issued
username. While either is outstanding, `GlanceReady` is `False` with reason
`WaitingForGlanceDBCredential`, the running child keeps its current mode, and the
message names either the `database/mariadb/creds/glance-<namespace>` path from
step 2 or the stale username it found. A ControlPlane stuck on the path message
has not been onboarded; re-run step 2. One stuck on the username message is
waiting for the sync this deletion shortcuts.

Watch for:

- The `controlplane-glance-db-credentials` ExternalSecret spec changing from
  static `data[].remoteRef` to `dataFrom[].sourceRef.generatorRef` (kind
  `VaultDynamicSecret`).
- The materialised Secret's `username` becoming an engine-issued login (not
  `glance`).
- A Glance Deployment rollout: the operator stamps a
  `glance.c5c3.io/db-connection-hash` pod-template annotation in Dynamic mode, so
  a rotated credential rolls the Deployment (the DSN is consumed via an
  environment variable, which only takes effect on a Pod restart).

Because the engine's GRANT overlaps any pre-existing operator-provisioned
`User`/`Grant` from the static deployment, Glance keeps serving throughout — the
rolling restart (protected by the Glance PodDisruptionBudget) simply moves it
onto an engine-issued login. This is the no-downtime property.

### 5. Retire the static credential

Once the ControlPlane reports `GlanceReady=True` on the dynamic path:

1. Delete the leftover static MariaDB `User` and `Grant` CRs (they carry the
   long-lived `glance` login the engine no longer uses):

   ```bash
   kubectl delete user,grant <glance-cr-name> -n <namespace> --ignore-not-found
   ```

2. Remove the retired static KV secret:

   ```bash
   # Inside the OpenBao pod, or with a bao client configured for it.
   bao kv metadata delete kv-v2/openstack/glance/<namespace>/<controlplane>/db
   ```

## Rollback

To revert to the static credential, set the mode back to `Static` (the
per-service `spec.services.glance.databaseCredentialsMode` override, or the shared
`spec.infrastructure.database.credentialsMode`), re-seed the KV path (step 3), and
let the next Static-mode reconcile re-create the `User`/`Grant`. Roll back the
operators if you also need to remove the generator objects.

## Operational considerations

- **Rotation churn vs. lease headroom:** because the DSN is consumed via an
  environment variable, a rotated engine credential only takes effect on a Pod
  restart, so Glance rolls each time the ExternalSecret re-issues the credential
  (a rotating dynamic credential means every refresh is a *new* credential — there
  is no stable value to renew in place). The defaults balance two concerns: the
  24h refresh interval keeps the roll cadence to at most once a day, while the 48h
  `default_ttl` keeps a 24h gap (`default_ttl` − refresh) so the operator has a
  full day to roll the pods before the previous, still-in-use lease is revoked —
  long enough that a stalled rollout pages on-call before it can become an outage.
  Raise `DB_CREDS_DEFAULT_TTL` / `DB_CREDS_MAX_TTL` (and the operator's refresh
  interval) further to trade churn against lease headroom; the PodDisruptionBudget
  and the surge-before-remove rollout strategy keep each roll zero-downtime.
- **Auth-token TTL bounds the lease:** OpenBao revokes a dynamic-secret lease
  together with the auth token that minted it, so the effective credential
  lifetime is `min(lease TTL, minting token TTL)`. The `glance-db` auth role
  therefore pins its token TTLs to `DB_CREDS_MAX_TTL` (72h). When raising
  `DB_CREDS_*` beyond that, raise the role's `token_ttl`/`token_max_ttl` in
  lockstep — a shorter token silently drops the ephemeral MySQL user under a
  running Glance long before the advertised lease end.
- **Revocation semantics:** revoking a lease runs `DROP USER`, which rejects
  *new* connections. Already-open sessions of a dropped user may persist until
  they disconnect.
- **ESO/OpenBao outage longer than the lease:** the materialised credential
  expires before a refresh lands; running Pods keep pooled connections but new
  connections fail until ESO recovers. This surfaces through the Glance child's
  own database readiness and, ControlPlane-side, as `GlanceReady=False`.

## See also

- [OpenBao Bootstrap reference](/reference/infrastructure/openbao-bootstrap) —
  engines, auth roles, policies, and secret paths.
- [ControlPlane reconciler reference](/reference/c5c3/controlplane-reconciler) —
  `reconcileGlance` DB-credential projection flow.
- [Migrate Keystone DB to Dynamic Credentials](/guides/keystone/migrate-keystone-db-to-dynamic-credentials) —
  the sibling migration for the Keystone service DB user.

## Tested by

The dynamic, engine-issued per-ControlPlane database credential this guide
migrates to — the projected Dynamic mode, the `VaultDynamicSecret`, the
generator-backed `controlplane-glance-db-credentials` ExternalSecret, and the
transient engine-issued login — is asserted on the live CI e2e kind cluster by
this chainsaw suite:

```bash
chainsaw test --test-dir tests/e2e/c5c3/full-controlplane-keystone
```
