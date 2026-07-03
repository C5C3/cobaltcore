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

- **Before:** the Keystone DB password is a long-lived value materialised from an
  OpenBao KV path (`openstack/keystone/{namespace}/{name}/db`) into the
  `{name}-keystone-db-credentials` Secret. It is only rotated when an operator
  rotates it.
- **After:** the c5c3 operator projects a per-ControlPlane
  [`VaultDynamicSecret`](https://external-secrets.io/) generator that reads
  short-lived credentials from the OpenBao database engine
  (`database/mariadb/creds/keystone-{namespace}`). The External Secrets
  Operator re-issues a fresh lease before the previous one expires and
  materialises the current username and password into the same Secret. No
  long-lived static DB password remains at rest.

The engine issues an ephemeral MySQL user per lease (for example `v-kube-...`)
with `ALL PRIVILEGES` on the Keystone database and drops it at lease end.

## Preconditions

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

This writes:

- `database/mariadb/config/keystone-<namespace>` — the connection
  to the tenant's MariaDB, authenticated as root.
- `database/mariadb/roles/keystone-<namespace>` — the role that
  issues short-lived users (`default_ttl` 48h, `max_ttl` 72h by default; override
  with `DB_CREDS_DEFAULT_TTL` / `DB_CREDS_MAX_TTL`).

`make deploy-infra WITH_CONTROLPLANE=true WITH_CONTROLPLANE_CR=true` runs this
automatically for the bundled ControlPlane after its MariaDB becomes Ready.

### 3. (Optional) Stage the cutover with `credentialsMode: Static`

Dynamic is the default effective mode for a managed ControlPlane. To keep a
ControlPlane on the static credential while you onboard the engine, set:

```yaml
spec:
  infrastructure:
    database:
      credentialsMode: Static
```

A ControlPlane pinned to `Static` after this change no longer has its static KV
path seeded automatically (the per-ControlPlane seed is retired), so you must
seed `kv-v2/openstack/keystone/<namespace>/<controlplane>/db` (`username`,
`password`) by hand while staging. Remove the field (or set it to `Dynamic`) to
cut over.

### 4. Upgrade the operators and observe the cutover

Upgrade the c5c3 and keystone operators to a build that includes the dynamic
engine wiring. On the next reconcile the c5c3 operator projects the generator,
ServiceAccount, and Certificate, and the ExternalSecret switches to drawing from
the generator. Watch for:

- The `{name}-keystone-db-credentials` ExternalSecret spec changing from static
  `data[].remoteRef` to `dataFrom[].sourceRef.generatorRef` (kind
  `VaultDynamicSecret`).
- The materialised Secret's `username` becoming an engine-issued login (not
  `keystone`).
- A Keystone Deployment rollout: the operator stamps a
  `keystone.c5c3.io/db-connection-hash` pod-template annotation in Dynamic mode,
  so a rotated credential rolls the Deployment (the DSN is consumed via the
  `OS_DATABASE__CONNECTION` env var, which only takes effect on a Pod restart).

Because the engine's GRANT overlaps any pre-existing operator-provisioned
`User`/`Grant` from the static deployment, Keystone keeps serving throughout —
the rolling restart (protected by the Keystone PodDisruptionBudget) simply moves
it onto an engine-issued login. This is the no-downtime property.

### 5. Retire the static credential

Once the ControlPlane reports `DBCredentialsReady=True` on the dynamic path and
Keystone is Ready:

1. Delete the leftover static MariaDB `User` and `Grant` CRs (they carry the
   long-lived `keystone` login the engine no longer uses):

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
KV path (step 3), and re-create the `User`/`Grant` (the operator recreates them
on the next reconcile in Static mode). Roll back the operators if you also need
to remove the generator objects.

## Operational considerations

- **Rotation churn vs. lease headroom:** because the DSN is consumed via an
  environment variable, a rotated engine credential only takes effect on a Pod
  restart, so Keystone rolls each time the ExternalSecret re-issues the credential
  (a rotating dynamic credential means every refresh is a *new* credential — there
  is no stable value to renew in place). The defaults balance two concerns: the
  24h refresh interval keeps the roll cadence to at most once a day, while the 48h
  `default_ttl` keeps a 24h gap (`default_ttl` − refresh) so the operator has a
  full day to roll the pods before the previous, still-in-use lease is revoked —
  long enough that a stalled rollout pages on-call before it can become an outage.
  Raise `DB_CREDS_DEFAULT_TTL` / `DB_CREDS_MAX_TTL` (and the operator's refresh
  interval) further to trade churn against lease headroom; the PodDisruptionBudget
  and the surge-before-remove rollout strategy keep each roll zero-downtime.
- **Revocation semantics:** revoking a lease runs `DROP USER`, which rejects
  *new* connections. Already-open sessions of a dropped user may persist until
  they disconnect.
- **ESO/OpenBao outage longer than the lease:** the materialised credential
  expires before a refresh lands; running Pods keep pooled connections but new
  connections fail until ESO recovers. This surfaces as `DBCredentialsReady=False`
  via the ClusterSecretStore gate.

## See also

- [OpenBao Bootstrap reference](/reference/infrastructure/openbao-bootstrap) —
  engines, auth roles, policies, and secret paths.
- [ControlPlane reconciler reference](/reference/c5c3/controlplane-reconciler) —
  `reconcileDBCredentials` projection flow.
