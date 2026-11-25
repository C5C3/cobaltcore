---
title: Migrate Placement DB to Dynamic Credentials
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Migrate Placement DB to Dynamic Credentials

This guide moves a managed-mode ControlPlane from a long-lived **static**
Placement database credential to **dynamic**, engine-issued credentials. The
Placement operator's default rolling update keeps existing API replicas
available while replacements start, provided the new credential works and the
cluster has capacity for a surge. This is the operator-facing side of the OpenBao
MariaDB database secrets engine wired for the Placement service DB user.

## What changes

- **Before:** the Placement DB password is a long-lived value copied from
  an OpenBao KV path (`openstack/placement/{namespace}/{name}/db`) into the
  `{name}-placement-db-credentials` Secret. Nothing seeds that path, so on the
  static branch an operator writes it and rotates it by hand.
- **After:** the c5c3 operator projects a per-ControlPlane
  [`VaultDynamicSecret`](https://external-secrets.io/) generator that reads
  short-lived credentials from the OpenBao database engine
  (`database/mariadb/creds/placement-{namespace}`, where `{namespace}` is the
  Placement service namespace, the ControlPlane's own namespace unless Placement
  runs in a dedicated one). The External Secrets Operator (ESO) refreshes the
  generator and stores its issued username and password in the Kubernetes
  Secret. Dynamic mode does not read a static password from the
  OpenBao KV path. Any existing static KV value remains until you delete it in
  step 5.

The engine issues an ephemeral MySQL user per lease (for example `v-kube-...`)
with `ALL PRIVILEGES` on the Placement database and drops it at lease end.

The figure shows both chains. `{svc}` stands for `placement` here, and `{cp}` is
`controlplane` on the devstack.

![Database credentials in two modes. Static: a person writes username and password to an OpenBao KV path, an ExternalSecret copies them through the secret store into the Secret {cp}-{svc}-db-credentials, and MariaDB User and Grant resources create one long-lived SQL user from it. Dynamic: a VaultDynamicSecret generator logs in to OpenBao as the ServiceAccount {svc}-db-creds over a client certificate and draws a user from database/mariadb/creds/{svc}-{ns}, the database engine creates that user in MariaDB for one lease, and a changed credential rolls the Deployment through the db-connection-hash annotation. In both modes the service operator builds the DSN Secret that the pods read. A time strip shows the 24 hour refresh inside the 48 hour default TTL and the 72 hour maximum TTL.](../../diagrams/secrets-db-credentials.svg)

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step. On this devstack the
ControlPlane is `controlplane` in `openstack`, so its projected Placement is
`controlplane-placement` and its DB-credential Secret is
`controlplane-placement-db-credentials`. Those are the names the migration below
manages.
:::

- The ControlPlane declares `spec.services.placement`. Without it the operator
  projects no Placement child and no DB credential of either kind.
- The OpenBao `database` secrets engine is mounted at `database/mariadb` (the
  `setup-secret-engines.sh` bootstrap step). A greenfield cluster already has it;
  on a brownfield cluster, re-apply the bootstrap scripts (see below).
- cert-manager and its `openbao-ca-issuer` ClusterIssuer are installed. They
  issue the per-ControlPlane mutual TLS (mTLS) client certificate the generator
  presents to the OpenBao listener.
- The External Secrets Operator can request ServiceAccount tokens
  (`serviceaccounts/token` `create`) so the generator can authenticate to OpenBao
  as the `placement-db-creds` ServiceAccount.

## Migration steps

### 1. Re-apply the OpenBao bootstrap on brownfield clusters

The database engine mount, the `placement-db` Kubernetes-auth role, and the
`placement-db-dynamic` policy are written by the (idempotent) bootstrap scripts.
Re-apply them so a cluster provisioned before Placement was onboarded picks them
up:

For a kind devstack, obtain and export `BAO_TOKEN` as described in
[Running the Full Bootstrap](../../reference/infrastructure/openbao-bootstrap.md#running-the-full-bootstrap).
That procedure reads the root token from `openbao-init-keys` and exports it for
the bootstrap scripts.

```bash
# From the repo root, with BAO_TOKEN exported (the OpenBao root token).
bash deploy/bases/shared-openbao/bootstrap/setup-secret-engines.sh
bash deploy/bases/shared-openbao/bootstrap/setup-auth.sh
bash deploy/bases/shared-openbao/bootstrap/setup-policies.sh
```

`make deploy-infra` runs these for you on a fresh kind cluster.

### 2. Onboard the per-tenant database-engine role

The engine role for a tenant only exists once its MariaDB is Ready and
`setup-database-tenant.sh` has configured the connection and role against it:

```bash
# BAO_TOKEN must be exported. Set these to the ControlPlane's namespace and name.
CONTROLPLANE_NAMESPACE=openstack
CONTROLPLANE_NAME=controlplane
bash deploy/bases/shared-openbao/bootstrap/setup-database-tenant.sh \
  "${CONTROLPLANE_NAMESPACE}" "${CONTROLPLANE_NAME}"
```

When the ControlPlane declares `spec.services.placement` on the shared managed
database, the script configures the Placement engine connection and role:

- `database/mariadb/config/placement-<placement-namespace>`, the connection to
  the tenant's MariaDB, authenticated as root.
- `database/mariadb/roles/placement-<placement-namespace>`, the role that issues
  short-lived users on the `placement` schema (`default_ttl` 48h, `max_ttl` 72h
  by default; override with `DB_CREDS_DEFAULT_TTL` / `DB_CREDS_MAX_TTL`).

When Placement lives in a dedicated service namespace, that namespace's MariaDB
must be Ready first: the script resolves that namespace's root secret and fails
loudly otherwise. A ControlPlane whose Placement declares a *dedicated* database
is skipped, because a dedicated Placement database is Static-only. The c5c3
admission webhook enforces the same rule from the other side and rejects
`spec.services.placement.databaseCredentialsMode: Dynamic` on a Placement that
declares `dedicatedBackingServices.database`.

`make deploy-infra WITH_CONTROLPLANE=true WITH_CONTROLPLANE_CR=true` runs this
automatically for the bundled ControlPlane after its MariaDB becomes Ready.

### 3. (Optional) Stage the cutover with `databaseCredentialsMode: Static`

Dynamic is the default effective mode for a managed ControlPlane on the shared
database. To keep Placement on a static credential, for example while Keystone
and Glance already run Dynamic, pin the per-service override so the blast radius
stays on Placement:

```yaml
spec:
  services:
    placement:
      databaseCredentialsMode: Static
```

The shared `spec.infrastructure.database.credentialsMode: Static` opts out
ControlPlane-wide instead; the per-service override scopes the opt-out to
Placement alone. Nothing seeds the static KV path, so while staging you must
write `kv-v2/openstack/placement/<placement-namespace>/<controlplane>/db`
(`username`, `password`) by hand. Set `username` to the Placement child's name
(`<controlplane>-placement`), which is the login the operator's MariaDB
`User`/`Grant` provisions. This example uses the devstack names; set
`PLACEMENT_NAMESPACE` to the Placement service namespace if it differs from the
ControlPlane namespace. `BAO_TOKEN` must be exported.

For the kind devstack, the shared bootstrap helper runs `bao` in the OpenBao
pod with its client TLS configuration. On other installations, use a `bao`
client configured for your OpenBao server.

```bash
source deploy/bases/shared-openbao/bootstrap/common.sh
PLACEMENT_NAMESPACE=openstack
CONTROLPLANE_NAME=controlplane
PLACEMENT_CR_NAME="${CONTROLPLANE_NAME}-placement"
printf 'Static Placement DB password: '
IFS= read -r -s DB_PASSWORD
printf '\n'
printf '%s' "$DB_PASSWORD" | bao_exec_stdin bao kv put \
  "kv-v2/openstack/placement/${PLACEMENT_NAMESPACE}/${CONTROLPLANE_NAME}/db" \
  "username=${PLACEMENT_CR_NAME}" password=-
unset DB_PASSWORD
```

Removing the per-service override inherits the shared mode and cuts over only
when that mode is `Dynamic`. If the shared mode is `Static`, set the Placement
override to `Dynamic` to cut over.

### 4. Upgrade the operators and observe the cutover

Upgrade the c5c3 and placement operators to a build that includes the dynamic
database integration. On the next reconcile the c5c3 operator projects the generator,
ServiceAccount, and Certificate, and the ExternalSecret switches to drawing from
the generator.

On a cluster that ran the static path, check the ExternalSecret before
deleting the old credential Secret:

```bash
PLACEMENT_NAMESPACE=openstack
CONTROLPLANE_NAME=controlplane
kubectl get externalsecret "${CONTROLPLANE_NAME}-placement-db-credentials" \
  -n "${PLACEMENT_NAMESPACE}" -o yaml
```

Wait until `spec.dataFrom[].sourceRef.generatorRef` names a
`VaultDynamicSecret`, then delete the credential Secret once so ESO syncs the
engine-issued credential:

```bash
kubectl delete secret "${CONTROLPLANE_NAME}-placement-db-credentials" \
  -n "${PLACEMENT_NAMESPACE}" --ignore-not-found
```

The ExternalSecret is updated in place (same name, same target Secret), so until
ESO completes its first generator-backed sync, the Secret still holds
whatever the previous static sync wrote. Deleting it forces ESO to sync from the
generator immediately instead of at the next refresh.

You do not have to get this right for Placement to stay up: the c5c3 operator
will not project `credentialsMode: Dynamic` onto the Placement child until that
ExternalSecret reports Ready **and** the Secret behind it carries an
engine-issued username. While either is outstanding, `PlacementReady` is `False`
with reason `WaitingForPlacementDBCredential`, the running child keeps its
current mode, and the message names either the
`database/mariadb/creds/placement-<namespace>` path from step 2 or the stale
username it found. A ControlPlane stuck on the path message has not been
onboarded; re-run step 2. One stuck on the username message is waiting for the
sync this deletion shortcuts.

Watch for:

- The `controlplane-placement-db-credentials` ExternalSecret spec changing from
  static `data[].remoteRef` to `dataFrom[].sourceRef.generatorRef` (kind
  `VaultDynamicSecret`).
- The Kubernetes Secret's `username` becoming an engine-issued login; the static
  username is `controlplane-placement`.
- A Placement Deployment rollout: the operator sets a
  `placement.c5c3.io/db-connection-hash` pod-template annotation, so a rotated
  credential rolls the Deployment. The DSN travels in the
  `OS_PLACEMENT_DATABASE__CONNECTION` environment variable, which only takes
  effect on a Pod restart.

The engine's `GRANT` overlaps the pre-existing operator-provisioned `User` and
`Grant` from the static deployment. By default, the Deployment uses a
RollingUpdate strategy with `maxUnavailable: 0` and `maxSurge: 1`, so existing
replicas remain available while ready replacements start, provided the cluster
has surge capacity and the new credential works. The PodDisruptionBudget
protects against voluntary disruptions, while the Deployment strategy controls
the rollout.

### 5. Retire the static credential

Once the ControlPlane reports `PlacementReady=True`, inspect the Placement
child and the credential username:

```bash
# Set to the Placement service namespace; Quick Start uses openstack.
PLACEMENT_NAMESPACE=openstack
CONTROLPLANE_NAME=controlplane
PLACEMENT_CR_NAME="${CONTROLPLANE_NAME}-placement"
kubectl get placement "${PLACEMENT_CR_NAME}" \
  -n "${PLACEMENT_NAMESPACE}" -o yaml
kubectl get secret "${PLACEMENT_CR_NAME}-db-credentials" \
  -n "${PLACEMENT_NAMESPACE}" -o jsonpath='{.data.username}' | base64 -d
echo
```

Confirm `spec.database.credentialsMode` is `Dynamic`, the username starts
with `v-`, the Placement `Ready` condition is `True`, and
`status.observedGeneration` matches `metadata.generation`. Then wait for the
Deployment rollout to finish.
`PlacementReady=True` confirms the desired replicas are available. An old pod
can still remain in the Deployment:

```bash
kubectl rollout status "deployment/${PLACEMENT_CR_NAME}" \
  -n "${PLACEMENT_NAMESPACE}" --timeout=10m
```

Only continue when the rollout succeeds. If it fails or times out, resolve the
rollout before deleting the static user.

1. Delete the leftover static MariaDB `User` and `Grant` CRs (they carry the
   long-lived `<controlplane>-placement` login the engine no longer uses):

   ```bash
   kubectl delete user,grant "${PLACEMENT_CR_NAME}" \
     -n "${PLACEMENT_NAMESPACE}" --ignore-not-found
   ```

2. Remove the retired static KV secret, if step 3 ever seeded it:

   ```bash
   # BAO_TOKEN must be exported.
   source deploy/bases/shared-openbao/bootstrap/common.sh
   # Set to the Placement service namespace; Quick Start uses openstack.
   PLACEMENT_NAMESPACE=openstack
   CONTROLPLANE_NAME=controlplane
   bao_exec bao kv metadata delete \
     "kv-v2/openstack/placement/${PLACEMENT_NAMESPACE}/${CONTROLPLANE_NAME}/db"
   ```

## Rollback

To revert to the static credential, first re-seed the KV path as in step 3, then
set the mode to `Static` (the per-service
`spec.services.placement.databaseCredentialsMode` override, or the shared
`spec.infrastructure.database.credentialsMode`). The next reconcile switches
the ExternalSecret back to the static KV path. The c5c3 operator attempts to
remove the dynamic generator objects, and the Placement operator reconciles the
static `User`/`Grant`.
Wait for the Secret's username to return to `<controlplane>-placement` and for
the Placement Deployment rollout to complete before considering the rollback
finished.

## Operational considerations

- **Rotation churn against lease headroom:** the DSN is consumed via an
  environment variable, so a rotated engine credential only takes effect on a Pod
  restart and Placement rolls each time the ExternalSecret re-issues the
  credential. Every refresh is a new credential; there is no stable value to
  renew in place. The defaults set a 24h refresh interval and a 48h
  `default_ttl`, leaving a nominal 24h interval for the Deployment to roll before
  the previous, still-in-use lease is revoked. Delayed refreshes or rollouts
  reduce that headroom. Raise `DB_CREDS_DEFAULT_TTL` / `DB_CREDS_MAX_TTL` and the
  operator's refresh interval together when changing the balance between
  refresh frequency and lease headroom. The default RollingUpdate strategy uses
  `maxUnavailable: 0` and `maxSurge: 1`. The PodDisruptionBudget applies to
  voluntary disruptions; the Deployment strategy governs the rollout.
- **Auth-token TTL bounds the lease:** OpenBao revokes a dynamic-secret lease
  together with the auth token that minted it, so the effective credential
  lifetime is `min(lease TTL, minting token TTL)`. The `placement-db` auth role
  therefore pins `token_ttl` and `token_max_ttl` to 72h, the `DB_CREDS_MAX_TTL`
  default. When raising `DB_CREDS_*` beyond that, raise the role's TTLs in
  lockstep. A shorter token can cause OpenBao to revoke the ephemeral MySQL user
  before the database lease's stated TTL expires.
- **No per-user connection cap on the dynamic path.** In Static mode the
  operator sizes the MariaDB `User` CR's `max_user_connections` for the CR's
  topology, counting two connections per uWSGI API thread across the API pods
  and the rollout surge, plus headroom for the db-sync Job. The engine's
  `CREATE USER` statement sets no per-user cap. Dynamic-login connections
  therefore share the MariaDB server's connection budget; account for them in
  `max_connections`.
- **Revocation semantics:** revoking a lease runs `DROP USER`, which rejects new
  connections. Already-open sessions of a dropped user may persist until they
  disconnect.
- **ESO or OpenBao outage longer than the lease:** the leased credential
  expires before a refresh completes. Running Pods may keep pooled connections;
  new connections fail after the engine revokes the user. The Placement
  `DatabaseReady` condition tracks database provisioning and schema work. Its
  API health check probes `/` without connecting to the database. Neither signal
  checks live lease validity. The ControlPlane can report
  `PlacementReady=False` when it observes the ExternalSecret as not Ready. A
  Ready condition can still coexist with an expired or soon-to-expire lease.
  Monitor ESO/OpenBao and Placement database connection errors, and restore
  credential issuance before the lease expires.

## See also

- [OpenBao Bootstrap reference](../../reference/infrastructure/openbao-bootstrap.md):
  engines, auth roles, policies, and secret paths.
- [ControlPlane reconciler reference](../../reference/c5c3/controlplane-reconciler.md):
  the `reconcilePlacement` DB-credential projection flow.
- [Placement CRD API Reference](../../reference/placement/placement-crd.md):
  `spec.database.credentialsMode` and the fields the ControlPlane projects onto
  the child.
- [Migrate Glance DB to Dynamic Credentials](../glance/migrate-glance-db-to-dynamic-credentials.md):
  the sibling migration for the Glance service DB user.

## Tested by

The live CI suite validates the resulting Dynamic state; it does not exercise a
Static-to-Dynamic cutover. It checks the projected Dynamic mode, the
`VaultDynamicSecret` and its engine path, the generator-backed
`controlplane-placement-db-credentials` ExternalSecret, the ServiceAccount, and
a non-empty Secret username that differs from the fixture's static `placement`
login. The suite also waits for the Placement child to become Ready after the
API Deployment and database sync complete. Run it with the Kyverno Chainsaw
test runner:

```bash
chainsaw test --test-dir tests/e2e/c5c3/full-controlplane-keystone \
  --config tests/e2e/chainsaw-config.yaml
```
