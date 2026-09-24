---
title: Migrate Nova DB to Dynamic Credentials
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Migrate Nova DB to Dynamic Credentials

This guide takes a managed-mode ControlPlane from long-lived **static** Nova
database credentials to **dynamic**, engine-issued credentials, without database
downtime. It is the operator-facing side of the OpenBao MariaDB database secrets
engine wired for the two Nova service DB users.

Nova is the one service with two databases. The `nova_api` schema holds the cell
map, the flavors and the instance mappings; the cell schema `nova` holds the
instances, with its `nova_cell0` twin reached by the same user. Each takes a
credential chain of its own, and this guide moves both.

## What changes

- **Before:** each Nova DB password is a long-lived value materialised from an
  OpenBao KV path into a Secret of its own. Nothing seeds those paths, so on the
  static branch an operator writes them and rotates them by hand.

  | Chain | KV path | Secret | SQL user |
  | --- | --- | --- | --- |
  | `nova_api` | `openstack/nova/openstack/controlplane/api-db` | `controlplane-nova-api-db-credentials` | `controlplane-nova-api` |
  | cell | `openstack/nova/openstack/controlplane/db` | `controlplane-nova-db-credentials` | `controlplane-nova` |

- **After:** the c5c3 operator projects two
  [`VaultDynamicSecret`](https://external-secrets.io/) generators, one per chain,
  that read short-lived credentials from the OpenBao database engine. The
  External Secrets Operator re-issues a fresh lease before the previous one
  expires and materialises the current username and password into the same two
  Secrets. No long-lived static Nova DB password remains at rest.

  | Chain | Auth role | ServiceAccount | Creds path | Engine grant |
  | --- | --- | --- | --- | --- |
  | `nova_api` | `nova-api-db` | `nova-api-db-creds` | `database/mariadb/creds/nova-api-openstack` | `ALL PRIVILEGES` on `nova_api` |
  | cell | `nova-cell-db` | `nova-cell-db-creds` | `database/mariadb/creds/nova-cell-openstack` | `ALL PRIVILEGES` on `nova` and `nova_cell0` |

The `openstack` suffix of both creds paths is the Nova service namespace, the
ControlPlane's own namespace unless Nova runs in a dedicated one. The engine
issues an ephemeral MySQL user per lease (for example `v-kube-...`) and drops it
at lease end.

One field decides the mode for both chains: `services.nova.databaseCredentialsMode`
on the ControlPlane, or the shared `spec.infrastructure.database.credentialsMode`
when that is empty. The Nova CRD rejects a child whose `apiDatabase` and
`database` carry different modes, so the two chains always switch together.

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through the `nova` block of Step 3, the onboarding of
Step 4 and the **Boot a first server** catalog check in Step 6, so the projected
`controlplane-nova` child is `Ready` in `openstack`. Every resource name in the
examples below is one that devstack produces.
:::

On that devstack Nova already runs on dynamic credentials, because Step 4
onboarded both engine roles. To walk the migration there, stage the static mode
first with step 3 below, then cut over with step 4.

::: warning Set the credential mode on the ControlPlane, never on the projected child
The `controlplane-nova` Nova CR is **projected** by the c5c3-operator, so a
`credentialsMode` you edit on the child by hand is reverted on the next
reconcile. Change `services.nova` on the `ControlPlane` CR and let the operator
project it down; the commands below only read the child.
:::

- The ControlPlane declares `spec.services.nova` on the managed shared database.
  A Nova that declares a dedicated database under
  `services.nova.dedicatedBackingServices` is `Static`-only, and the webhook
  rejects `databaseCredentialsMode: Dynamic` on it.
- The OpenBao `database` secrets engine is mounted at `database/mariadb` (the
  `setup-secret-engines.sh` bootstrap step). A greenfield cluster already has
  it; on a brownfield cluster, re-apply the bootstrap scripts (step 1).
- cert-manager and its `openbao-ca-issuer` ClusterIssuer are installed. They
  issue the two per-ControlPlane mTLS client certificates the generators present
  to the OpenBao listener.
- The External Secrets Operator can request ServiceAccount tokens
  (`serviceaccounts/token` `create`), so each generator can authenticate to
  OpenBao as its own ServiceAccount.

## Migration steps

### 1. Re-apply the OpenBao bootstrap on brownfield clusters

The database engine mount, the `nova-api-db` and `nova-cell-db`
Kubernetes-auth roles, and the `nova-api-db-dynamic` and `nova-cell-db-dynamic`
policies are written by the idempotent bootstrap scripts. Re-apply them so a
cluster provisioned before Nova was onboarded picks them up:

```bash
# From the repo root, with BAO_TOKEN exported (the OpenBao root token).
bash deploy/openbao/bootstrap/setup-secret-engines.sh
bash deploy/openbao/bootstrap/setup-auth.sh
bash deploy/openbao/bootstrap/setup-policies.sh
```

`make deploy-infra` runs these for you on a fresh kind cluster.

Each role binds its fixed ServiceAccount name in any namespace, and each policy
templates the readable path to the caller's own service-account namespace. The
two names are what keep the chains apart: a token minted for
`nova-api-db-creds` reads the `nova-api-` creds path and never the `nova-cell-`
one, and neither reads another service's path.

### 2. Onboard the per-tenant database-engine roles

The engine roles for a tenant exist only once its MariaDB is Ready and
`setup-database-tenant.sh` has configured the connections and roles against it:

```bash
export BAO_TOKEN=$(kubectl get secret openbao-init-keys -n shared-services \
  -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token')
bash deploy/openbao/bootstrap/setup-database-tenant.sh openstack controlplane
unset BAO_TOKEN
```

When the ControlPlane declares `spec.services.nova` on the shared managed
database, the script provisions two Nova legs:

- `database/mariadb/config/nova-api-openstack` and
  `database/mariadb/roles/nova-api-openstack`, the role that issues short-lived
  users on `nova_api`.
- `database/mariadb/config/nova-cell-openstack` and
  `database/mariadb/roles/nova-cell-openstack`, the role that issues short-lived
  users on `nova` and `nova_cell0`.

Both roles default to `default_ttl` 48h and `max_ttl` 72h; override them with
`DB_CREDS_DEFAULT_TTL` / `DB_CREDS_MAX_TTL`. The script reads the live
ControlPlane spec on every run and skips a service the CR does not declare, so
run it again after adding the `nova` block to a ControlPlane that was onboarded
without one.

### 3. (Optional) Stage the cutover with `databaseCredentialsMode: Static`

Dynamic is the default effective mode for a managed ControlPlane on the shared
database. To keep Nova on static credentials while the other services already
run Dynamic, pin the per-service override so the blast radius stays on Nova.

Nothing seeds the static KV paths, so seed both before you switch the mode. The
`username` of each must be the name of the SQL user the nova operator provisions
for that schema in Static mode. Enter each password at the prompt, so it stays
out of your shell history and off the command line (`password=-` reads it from
stdin):

```bash
# Inside the OpenBao pod, or with a bao client configured for it.
read -rs -p 'nova_api DB password: ' PW; echo
printf '%s' "$PW" | bao kv put kv-v2/openstack/nova/openstack/controlplane/api-db \
  username=controlplane-nova-api password=-
read -rs -p 'cell DB password: ' PW; echo
printf '%s' "$PW" | bao kv put kv-v2/openstack/nova/openstack/controlplane/db \
  username=controlplane-nova password=-
unset PW
```

The two passwords are values you choose. In Static mode the nova operator
provisions the MariaDB `User` and `Grant` pairs `controlplane-nova-api` (on
`nova_api`) and `controlplane-nova` (on `nova`), plus the Grant
`controlplane-nova-nova-cell0` that gives the cell user `nova_cell0`. Then switch
the mode:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"nova":{"databaseCredentialsMode":"Static"}}}}'
```

The switch reaches the child at once, possibly before ESO has re-synced the two
Secrets from the KV paths, and the nova operator creates each `User` with the
password its Secret holds at that moment. mariadb-operator does not watch that
Secret, so it keeps that password until something makes it reconcile the `User`
again. Wait until both Secrets carry the static usernames, then touch both
`User` CRs so mariadb-operator applies the password the Secrets hold now:

```bash
ok=1
for user in controlplane-nova-api controlplane-nova; do
  for i in $(seq 60); do
    [ "$(kubectl get secret "$user-db-credentials" -n openstack \
      -o jsonpath='{.data.username}' | base64 -d)" = "$user" ] && break
    if [ "$i" -eq 60 ]; then
      ok=
      echo "$user-db-credentials does not carry $user after 5m: check its ExternalSecret" >&2
    fi
    sleep 5
  done
done
[ -n "$ok" ] &&
  kubectl wait user/controlplane-nova-api user/controlplane-nova -n openstack \
    --for=condition=Ready --timeout=5m &&
  kubectl annotate user controlplane-nova-api controlplane-nova -n openstack \
    --overwrite "password-resync=$(date +%s)"
```

Every change to a `User` makes mariadb-operator reconcile it, and every
reconcile sets the SQL user's password from the Secret again, so the annotation
changes nothing where the password was already right.

If a Secret never carries its static username, the block prints which one and
touches neither `User`: the Secret still holds the engine-issued password, and a
resync would apply that password again. Fix the ExternalSecret it names,
usually a KV path that does not match your ControlPlane's name or a store policy
that denies the read, then run the block again.

The shared `spec.infrastructure.database.credentialsMode: Static` opts out
ControlPlane-wide instead; the per-service override scopes the opt-out to Nova.
Remove the override (or set it to `Dynamic`) to cut over.

### 4. Cut over and observe

Remove the override:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"nova":{"databaseCredentialsMode":null}}}}'
```

On the next reconcile the c5c3 operator projects both generators, their
ServiceAccounts and client Certificates, and both ExternalSecrets switch to
drawing from their generator. On a cluster that ran the static path, delete the
two materialised Secrets once, so ESO re-materialises them from the generators
immediately instead of at the next refresh:

```bash
kubectl delete secret controlplane-nova-api-db-credentials \
  controlplane-nova-db-credentials -n openstack
```

You do not have to get this right for Nova to stay up. The c5c3 operator does
not project `credentialsMode: Dynamic` onto the child until both ExternalSecrets
report Ready **and** both Secrets carry an engine-issued username. It checks the
`nova_api` chain first, because the cell mappings are registered in that schema,
so `NovaReady` reads `False` under `WaitingForNovaAPIDBCredential` and then under
`WaitingForNovaCellDBCredential` until each chain has delivered:

```bash
kubectl get controlplane controlplane -n openstack \
  -o jsonpath='{.status.conditions[?(@.type=="NovaReady")].reason}{"\n"}'
```

The message names the creds path the chain waits on, or the stale static
username it found. A ControlPlane stuck on the path message has not been
onboarded; re-run step 2. One stuck on the username message is waiting for the
sync the Secret deletion above shortcuts.

Once `NovaReady` is `True`, both database blocks of the child read `Dynamic`:

```bash
kubectl get nova controlplane-nova -n openstack \
  -o jsonpath='{.spec.apiDatabase.credentialsMode} {.spec.database.credentialsMode}{"\n"}'
```

The command prints `Dynamic Dynamic`. Watch for:

- Both ExternalSecrets, `controlplane-nova-api-db-credentials` and
  `controlplane-nova-db-credentials`, changing from static `data[].remoteRef` to
  `dataFrom[].sourceRef.generatorRef` (kind `VaultDynamicSecret`).
- The materialised Secrets' `username` becoming engine-issued logins rather than
  `controlplane-nova-api` and `controlplane-nova`.
- A rollout of all five Nova Deployments: `controlplane-nova`,
  `controlplane-nova-metadata`, `controlplane-nova-scheduler`,
  `controlplane-nova-conductor` and `controlplane-nova-novncproxy`. The nova
  operator stamps the `nova.c5c3.io/api-db-connection-hash` and
  `nova.c5c3.io/db-connection-hash` pod-template annotations on every workload,
  so a changed credential rolls all of them. Both DSNs travel in environment
  variables, which only take effect on a pod restart.

The cell mappings stay as they were. They hold templates that carry no
credential, and every process expands them with the connection it now runs
with, so the cutover needs no `nova-manage cell_v2 update_cell`. Confirm it with
the plain `list_cells` (never `--verbose`, which prints the passwords):

```bash
kubectl exec -n openstack deploy/controlplane-nova-conductor -c conductor -- \
  nova-manage --config-dir /etc/nova/nova.conf.d cell_v2 list_cells
kubectl get nova controlplane-nova -n openstack -o jsonpath='{.status.cells}{"\n"}'
```

The two UUIDs in the table are the ones `status.cells` reported before the
cutover.

### 5. Retire the static credentials

Once the ControlPlane reports `NovaReady=True` on the dynamic path:

1. Delete the leftover static MariaDB `User` and `Grant` CRs. They carry the
   long-lived `controlplane-nova-api` and `controlplane-nova` logins the engine
   no longer uses. Leave the `Database` CRs of the same names alone: they are
   the schemas.

   ```bash
   kubectl delete user controlplane-nova-api controlplane-nova \
     -n openstack --ignore-not-found
   kubectl delete grant controlplane-nova-api controlplane-nova \
     controlplane-nova-nova-cell0 -n openstack --ignore-not-found
   ```

2. Remove the retired static KV secrets, if step 3 ever seeded them:

   ```bash
   # Inside the OpenBao pod, or with a bao client configured for it.
   bao kv metadata delete kv-v2/openstack/nova/openstack/controlplane/api-db
   bao kv metadata delete kv-v2/openstack/nova/openstack/controlplane/db
   ```

## Rollback

To revert to static credentials, follow step 3 in its order: re-seed both KV
paths, set the mode back to `Static` (the per-service
`spec.services.nova.databaseCredentialsMode` override, or the shared
`spec.infrastructure.database.credentialsMode`), and wait for the static
usernames before you touch the `User` CRs. The Static-mode reconcile re-creates
the two `User`/`Grant` pairs and the cell0 `Grant`, and all five Deployments
roll back onto the static DSNs. The two generators, their ServiceAccounts and
their Certificates are torn down with the switch.

## Operational considerations

- **Every Nova role restarts on every rotation.** Both DSNs are consumed through
  environment variables, so a rotated engine credential only takes effect on a
  pod restart, and all five Deployments roll each time either ExternalSecret
  re-issues a credential. On the quick-start devstack each runs one replica, so
  each role is briefly down per roll; the scheduler and the conductor drain their
  RPC server for up to 200 seconds before they stop. The defaults keep the
  churn bounded: the 24h refresh interval holds it to at most once a day per
  chain, while the 48h `default_ttl` leaves a 24h gap in which to roll the pods
  before the previous, still-in-use lease is revoked.
- **Auth-token TTL bounds the lease.** OpenBao revokes a dynamic-secret lease
  together with the auth token that minted it, so the effective credential
  lifetime is `min(lease TTL, minting token TTL)`. The `nova-api-db` and
  `nova-cell-db` auth roles therefore pin `token_ttl` and `token_max_ttl` to
  72h, the `DB_CREDS_MAX_TTL` default. When raising `DB_CREDS_*` beyond that,
  raise both roles' TTLs in lockstep.
- **No per-user connection cap on the dynamic path.** In Static mode the nova
  operator sizes each MariaDB `User`'s `max_user_connections` for the CR's
  topology. The engine's `CREATE USER` sets no cap, so once Nova runs on
  engine-issued logins the server's own `max_connections` is the only bound.
- **The archive CronJob rides both credentials.** `controlplane-nova-db-archive`
  runs `nova-manage db archive_deleted_rows --all-cells` with the same two DSNs
  the workloads carry, read at pod start from the derived Secrets the nova
  operator re-renders from each engine-issued credential. A run that starts
  between a revocation and that re-render fails and archives nothing; the next
  run continues where it stopped.
- **Revocation semantics:** revoking a lease runs `DROP USER`, which rejects new
  connections. Already-open sessions of a dropped user may persist until they
  disconnect.
- **ESO or OpenBao outage longer than the lease:** the materialised credentials
  expire before a refresh lands. Running pods keep pooled connections, but new
  connections fail until ESO recovers. This surfaces through the child's own
  `DatabaseReady` and, ControlPlane-side, as `NovaReady=False`.

## See also

- [OpenBao Bootstrap reference](../../reference/infrastructure/openbao-bootstrap.md):
  engines, auth roles, policies, and secret paths.
- [ControlPlane CRD API Reference](../../reference/c5c3/controlplane-crd.md#servicenovaspec):
  `services.nova.databaseCredentialsMode` and the `NovaReady` reasons.
- [Nova Cells](../../reference/nova/nova-cells.md): why the cell mappings carry
  no credential.
- [Migrate Cinder DB to Dynamic Credentials](../cinder/migrate-cinder-db-to-dynamic-credentials.md):
  the sibling migration for a single-database service.

## Tested by

The dynamic, engine-issued credentials this guide migrates to are asserted on
the live CI e2e kind cluster by the chainsaw suite below. Its compute link waits
for `NovaReady`, reads both database blocks of the projected child back as
`Dynamic`, and checks for each chain that the ExternalSecret draws from a
`VaultDynamicSecret` generator and carries no static `spec.data` refs, that the
generator reads `database/mariadb/creds/nova-api-<namespace>` or
`database/mariadb/creds/nova-cell-<namespace>`, that the `nova-api-db-creds` or
`nova-cell-db-creds` ServiceAccount exists, and that the materialised username is
neither empty nor the schema name.

```bash
chainsaw test --test-dir tests/e2e/c5c3/full-controlplane-keystone
```
