---
title: DB Credential Scoping (per-ControlPlane)
quadrant: operator
---

# DB Credential Scoping (per-ControlPlane)

This document explains how the Keystone database credential is scoped per
`ControlPlane` (feature `CC-0116`), why the OpenBao path layout was chosen the
way it is, and the direction the credential model is expected to take next. It
records both the **implemented** state and the **deferred** stage-b direction so
the reserved path shapes and migration story are captured before the work
lands.

## Problem: one credential for the whole cluster

Earlier releases stored the Keystone database credential at a single, flat
OpenBao KV path, `openstack/keystone/db`, with a matching static ESO
`ExternalSecret` (`keystone-db`) and a `keystone-db-credentials` Kubernetes
Secret. That layout implicitly assumed **one control plane per cluster**: a
second `ControlPlane` would read and overwrite the same path, colliding on a
cluster-global backend. The same single-tenant assumption was removed for the
admin Application Credential, the admin bootstrap password, and the Fernet /
credential signing keys, which the c5c3 operator now keys per CR (see the
reconciler's
[Migration: legacy flat paths → per-ControlPlane paths](../../reference/c5c3/controlplane-reconciler.md#migration-legacy-flat-paths--per-controlplane-paths)).

`CC-0116` brings the Keystone DB credential into line with that model: the path
is keyed by the owning `ControlPlane`'s `{namespace}/{name}`, so multiple
control planes — one per namespace — never collide in OpenBao.

## Per-ControlPlane DB path layout (implemented, CC-0116)

The Keystone DB credential lives at the per-ControlPlane KV path

```text
openstack/keystone/{namespace}/{name}/db
```

where `{namespace}/{name}` is the `ControlPlane` CR's own coordinates
(`dbCredentialRemoteKeyFor`). The value carries two keys, `username` and
`password`. For the default single-instance deployment whose identity is
`openstack/controlplane`, this resolves to
`openstack/keystone/openstack/controlplane/db`.

| Concern | Implemented behaviour (CC-0116) |
| --- | --- |
| KV path | `openstack/keystone/{namespace}/{name}/db` |
| Keys | `username`, `password` |
| Default instance | `openstack/controlplane` → `openstack/keystone/openstack/controlplane/db` |
| Seeded by | `deploy/openbao/bootstrap/write-bootstrap-secrets.sh` (in its `KORC_CONTROLPLANES` loop) |
| Seed value | `username=keystone`, generated `password` |
| ESO-managed? | **No** — deliberately not marked managed; the operator never pushes it back |
| Consumed by | operator-projected `{controlplane.Name}-keystone-db-credentials` ExternalSecret |

The bootstrap script seeds the path **read-only**: it writes
`username=keystone` and a generated password once, per `ControlPlane` identity,
and deliberately does **not** mark the path ESO-managed. The credential is a
static KV value the operator only ever reads.

On the operator side, the `reconcileDBCredentials` sub-reconciler owns a
per-ControlPlane `ExternalSecret` named
`{controlplane.Name}-keystone-db-credentials` (CreationPolicy `Owner`, refresh
`1h`, store `openbao-cluster-store` / `ClusterSecretStore`). ESO materialises a
same-named `Secret` from the per-CP KV path. In **managed** mode,
`reconcileKeystone` then substitutes that Secret into the projected Keystone
CR's `spec.database.secretRef` (key `password`). In **brownfield** mode
(`database.host` set), the user-supplied `secretRef` is kept untouched and the
operator provisions nothing. See the reconciler reference's
[`reconcileDBCredentials`](../../reference/c5c3/controlplane-reconciler.md#reconciledbcredentials)
section for the sub-reconciler contract and `DBCredentialsReady` condition.

The existing read policies already cover the per-CP path via wildcards —
`eso-management.hcl` (`kv-v2/data/openstack/keystone/*`) and
`eso-control-plane.hcl` (`kv-v2/data/openstack/*`). No write ACL is granted to
any `…/db` path: `push-keystone-keys.hcl` grants write only to the
`{fernet,credential}-keys` paths.

## Reserved multi-database form `.../db/<dbname>` (not implemented — YAGNI)

Today a `ControlPlane` projects a **single** Keystone database, so the path
terminates at `/db` and carries exactly that one credential. A future control
plane that projected **multiple** service databases under one `ControlPlane`
could extend the path with a trailing database segment:

```text
openstack/keystone/{namespace}/{name}/db/<dbname>
# e.g. .../db/keystone, .../db/nova
```

This shape is **reserved, not implemented**. With a single database per control
plane the extra segment buys nothing, so it is deliberately left out (YAGNI).
The current `…/db` terminal form is a strict prefix of the reserved
`…/db/<dbname>` form, so adopting per-database segments later is additive and
does not move the existing credential.

## Deferred: operator-owned PushSecret + `push-keystone-db.hcl`

Today the DB credential is a **static** KV value: seeded once by the bootstrap
script and only ever read by the operator. A future stage in which the operator
**generates and rotates** the DB password itself would mirror the existing
admin Application-Credential PushSecret: an operator-owned `PushSecret` would
write the generated password to `openstack/keystone/{namespace}/{name}/db`, and
ESO would round-trip it back into the Kubernetes Secret.

That write path would require a new, narrowly-scoped policy —
`push-keystone-db.hcl` — granting `create` / `update` on:

```text
kv-v2/data/openstack/keystone/+/+/db
kv-v2/metadata/openstack/keystone/+/+/db
```

where the `+/+` segments match `{namespace}/{name}`. This is **deferred and not
implemented**. The current path is intentionally read-only — no write ACL is
granted today, and `push-keystone-keys.hcl` grants no write to any `…/db` path —
so the static seed cannot be overwritten by the operator until this policy and
PushSecret are added.

## Stage-b: dynamic database secret engine (`database/mariadb/creds/…`)

The end-state replaces the static KV credential entirely with OpenBao's
[Database Secret Engine](https://openbao.org/docs/secrets/databases/). Instead
of a long-lived password stored in KV, the operator (via ESO) would request
**dynamic, short-lived** MariaDB credentials from a role under
`database/mariadb/creds/<role>`.

Each request mints a fresh MariaDB user with the role's configured grants and a
lease carrying a TTL (default `1h`). OpenBao tracks the lease and
**automatically revokes** the underlying MariaDB user when the lease expires, so
a leaked credential is valid only for the remainder of its short TTL. ESO holds
the lease for the materialised Kubernetes Secret and refreshes the credential
**ahead of** expiry, requesting a new dynamic user before the current one is
revoked so the consuming workload sees uninterrupted access. No password is ever
stored at rest in KV — the only durable state is the role definition and the
connection configuration on the `database/mariadb/` mount.

The architecture today models only **read-only** exporter roles on this engine —
e.g. `database/mariadb/creds/keystone-ro` (SELECT-only), consumed by the
per-service database exporters such as `openstack-database-exporter-keystone`.
Stage-b adds a **read-write** role for the Keystone service itself, e.g.
`database/mariadb/creds/keystone-rw`, so the Keystone API container obtains its
own short-lived, auto-revoked credential rather than a static KV password.

## Migration from the static KV credential

The credential model migrates in stages, each one strictly forward from the
last:

1. **Today (CC-0116).** Per-ControlPlane static KV credential at
   `openstack/keystone/{namespace}/{name}/db` — seeded once by the bootstrap
   script, long-lived, read-only (no operator write-back).
2. **Interim (deferred, §4).** Operator-owned generation + `PushSecret` writing
   the same per-CP path, gated by `push-keystone-db.hcl`. The path and consumer
   Secret are unchanged; only the producer (bootstrap seed → operator) and the
   credential's lifecycle (static → rotated) change.
3. **End-state (stage-b, §5).** Dynamic `database/mariadb/creds/<role>`
   credentials replace the KV path entirely; the per-CP `…/db` KV value is
   retired.

Because the credential is already keyed per `ControlPlane`, each control plane
can retire its own `…/db` KV path independently as it migrates — there is no
cross-control-plane collision and no cluster-wide cutover. The per-CP layout
chosen now is the forward-compatible foundation the later stages build on.

## References / Further reading

- [ControlPlane Reconciler Architecture](../../reference/c5c3/controlplane-reconciler.md) —
  the [`reconcileDBCredentials`](../../reference/c5c3/controlplane-reconciler.md#reconciledbcredentials)
  sub-reconciler and the
  [Migration: legacy flat paths → per-ControlPlane paths](../../reference/c5c3/controlplane-reconciler.md#migration-legacy-flat-paths--per-controlplane-paths)
  section.
- [ControlPlane CRD API Reference](../../reference/c5c3/controlplane-crd.md) —
  the `database` spec and managed-vs-brownfield modes.
- [OpenBao Bootstrap](../../reference/infrastructure/openbao-bootstrap.md) —
  the bootstrap secret seeding and ESO integration.
- Architecture: `architecture/docs/05-deployment/02-secret-management.md`
  (sections "Secret Engines", "Database Secret Engine", "Complete Secret
  Inventory") in the `architecture/` submodule
  ([github.com/C5C3/C5C3](https://github.com/C5C3/C5C3)) — the authoritative
  design source for the dynamic Database Secret Engine model described in §5.
