---
title: Brownfield Keystone Adoption
quadrant: operator
---

# Brownfield Keystone Adoption

> **Status: idea sketch.** Nothing on this page is implemented or scheduled.
> It analyzes what a phased takeover of an existing Keystone installation
> would require, using the current codebase as the baseline.

## Motivation

Today a `ControlPlane` CR always ends in a running, operator-deployed Keystone:
the reconciler chain provisions infrastructure, projects a Keystone CR, runs
the bootstrap job, and only then wires up K-ORC credentials and the catalog.
For an existing (brownfield) OpenStack installation this is a big-bang
proposition — the operator cannot add value before it owns the whole stack.

The idea: invert the adoption order. In a first step, a **service-less
ControlPlane** deploys *nothing* — no Keystone, no MariaDB, no cache — and
takes over only the **identity-plane management** that K-ORC provides against
the existing, externally running Keystone API:

- minting and rotating the admin **application credential**,
- managing **service accounts** (service users, projects, role assignments)
  and their password rotation,
- stewarding **catalog services and endpoints**,
- storing all of it per-CR in OpenBao with the existing ExternalSecrets /
  PushSecret round-trip.

Explicitly *out of scope* for this first step is anything that requires
database access — in particular the admin password, which the managed flow
seeds via `keystone-manage bootstrap`. On a brownfield the admin password is
supplied by the platform owner and rotated out-of-band.

The actual takeover of MariaDB, messaging, and the Keystone deployment itself
then happens in later, individually reversible phases on the **same CR**.

## Baseline: what exists today

The building blocks are further along than one might expect — brownfield is
already a first-class concept, just not for the service layer.

**Already in place:**

- **Brownfield infrastructure modes.** `spec.infrastructure.database` accepts
  either a `clusterRef` (managed MariaDB) or a `host` (existing external
  database); `spec.infrastructure.cache` mirrors this with `clusterRef` vs
  `servers`. The XOR is enforced by CEL rules, and the `DBCredentials` and
  `AdminPassword` sub-reconcilers already short-circuit in brownfield mode
  with the condition reason `BrownfieldUserSuppliedCredential` — the user
  supplies the Secrets, the operator projects nothing.
- **Adoption semantics for unowned infrastructure.** The infrastructure
  sub-reconciler detects a pre-existing MariaDB or Memcached CR it does not
  control and adopts it as-is instead of re-projecting defaults.
- **K-ORC imports for pre-existing identity resources.** The admin user and
  the `Default` domain are created as *unmanaged* K-ORC resources — imports of
  objects that already exist in Keystone. This is exactly the mechanism a
  brownfield needs; today it merely happens to import objects the operator's
  own bootstrap job created moments earlier.
- **Credential lifecycle machinery.** The admin application credential is
  minted by a managed K-ORC `ApplicationCredential`, backed up to OpenBao via
  PushSecret, re-materialized via ExternalSecret, and re-minted
  (delete + recreate) whenever the admin-password hash changes or a
  `CredentialRotation` CR nudges it. None of this logic cares *where* the
  Keystone API lives — only the endpoint URL does.

**Gaps that block a service-less mode** (all in `operators/c5c3`):

1. **`services.keystone` is a required struct.** There is no way to express
   "manage identity, deploy nothing". The `Keystone` sub-reconciler always
   projects a child Keystone CR.
2. **The auth URL is hardwired to the in-cluster Service.** Both clouds.yaml
   builders (password-based and application-credential-based) use
   `http://{name}-keystone.{namespace}.svc:5000/v3` with
   `endpoint_type: internal`. No spec field, env var, or Secret can point
   K-ORC at an external Keystone; the only configurable URL today,
   `services.keystone.publicEndpoint`, affects catalog advertisement and the
   bootstrap job — never the auth path.
3. **`spec.infrastructure` is required.** Even in brownfield database mode the
   CR must carry database coordinates, because the projected Keystone consumes
   them. A service-less ControlPlane needs neither database nor cache.
4. **The catalog sub-reconciler only creates.** It manages a K-ORC `Service`
   (type `identity`) and `Endpoint` (interface `public`) as *managed*
   resources. Pointed at an existing installation this would duplicate catalog
   entries — Keystone does not enforce unique service names.
5. **The bootstrap seed assumes the managed flow.** The write-if-empty
   password clouds.yaml seed and the OpenBao bootstrap path
   (`bootstrap/{namespace}/{keystone}/admin`) are built around the
   operator-generated admin password.

## Phase model

Each phase is a spec transition on the same `ControlPlane` CR, forward-only,
and leaves the system in a supported steady state. A brownfield operator can
stop at any phase indefinitely.

| Phase | Name | Operator manages | Existing installation keeps |
|-------|------|------------------|------------------------------|
| 1 | Identity-only (service-less) | App credentials, service accounts, catalog entries, secret rotation via K-ORC + OpenBao | Keystone API, database, admin password, all data |
| 2 | Infrastructure attach | Phase 1 + database/cache coordinates (brownfield `host`/`servers` mode), later managed replacements with data migration | Keystone API, admin password |
| 3 | Service takeover | Phase 2 + the Keystone deployment itself (fernet/credential keys imported, bootstrap skipped, endpoint cutover) | — |
| 4 | Steady state | Everything — indistinguishable from a greenfield ControlPlane | — |

Messaging (RabbitMQ) is not modeled in the ControlPlane CRD at all today —
Keystone needs none. The phase model reserves its takeover for the point where
additional services (which do need messaging) join the ControlPlane; it would
follow the same pattern as the database: brownfield coordinates first, managed
replacement with migration later.

## Phase 1: the service-less ControlPlane

### API sketch

A mode discriminator on the service entry, mirroring the existing
managed-vs-brownfield split of the infrastructure specs:

```yaml
apiVersion: c5c3.io/v1alpha1
kind: ControlPlane
metadata:
  name: brownfield
  namespace: openstack
spec:
  openStackRelease: "2025.1"        # advisory in this mode — no images are deployed
  region: RegionOne
  # infrastructure: omitted — must become optional (forbidden in External mode)
  services:
    keystone:
      mode: External                 # new discriminator; default Managed
      external:
        authURL: https://keystone.example.com/v3
        endpointType: public         # interface K-ORC selects from the catalog
        caBundleSecretRef:           # optional, for private CAs
          name: brownfield-keystone-ca
  korc:
    adminCredential:
      passwordSecretRef:
        name: brownfield-admin-password   # user-supplied, from the existing installation
```

Alternative shapes considered:

- **Make `services.keystone` a pointer** (absent = not deployed). Rejected:
  absence cannot carry the external endpoint configuration, and an omitted
  required-feeling field reads like an authoring mistake rather than intent.
- **A separate CRD** (e.g. `IdentityPlane`). Rejected: the whole point of the
  phase model is that adoption is a sequence of spec transitions on one CR;
  a CRD migration in the middle of it would break ownership, status history,
  and the one-ControlPlane-per-namespace invariant.

The `mode: External` → `mode: Managed` transition is precisely the phase 3
takeover and must therefore be *gated*, not immutable — unlike the existing
infrastructure mode fields, which are immutable today and would need the same
relaxation for phase 2. The reverse transition (`Managed` → `External`) should
be rejected outright.

### Sub-reconciler behavior in External mode

The chain keeps its order; External mode changes what each link does:

| Sub-reconciler | Managed (today) | External mode (proposed) |
|----------------|-----------------|--------------------------|
| Infrastructure | Creates MariaDB / Memcached CRs | Skipped — `InfrastructureReady` with a dedicated reason (e.g. `ExternallyManaged`) |
| DBCredentials | Projects ExternalSecret from OpenBao | Skipped — no database involvement at all |
| AdminPassword | Projects ExternalSecret from the OpenBao bootstrap path | Skipped — the user-supplied `passwordSecretRef` Secret is the source, exactly as in today's brownfield database mode |
| Keystone | Creates the child Keystone CR | Skipped — no child CR; `KeystoneReady` reports `ExternallyManaged` |
| KORC | Builds clouds.yaml against the in-cluster Service URL | Builds clouds.yaml against `spec…external.authURL` with the configured `endpointType`; admin user/domain imports, application-credential mint, OpenBao round-trip all unchanged |
| AdminCredential | Assembles app-credential clouds.yaml, force-push/force-sync | Unchanged — the assembled clouds.yaml simply carries the external `auth_url` |
| Catalog | Creates managed Service + Endpoint | Import-first: unmanaged imports of the existing identity service and endpoints; managed creation only as an explicit opt-in for genuinely new entries |

The striking observation from the baseline analysis: **the two heaviest
sub-reconcilers (KORC, AdminCredential) need almost no change.** Their logic
is endpoint-agnostic; only the two clouds.yaml builders take a different
`auth_url` and `endpoint_type`. The bulk of the work is API surface
(mode/external fields, optionality of `infrastructure`, webhook rules) and the
catalog import semantics.

### What phase 1 delivers on a brownfield

- **Admin application credential under management.** Minted by K-ORC against
  the existing Keystone, stored per-CR in OpenBao, re-minted automatically
  when the admin password changes (hash-driven) or on demand via a
  `CredentialRotation` CR. Consumers read it from OpenBao / the materialized
  Secret instead of sharing long-lived admin passwords.
- **A home for service accounts.** The `bootstrapResources` field already
  reserved in the CRD (kinds `Project` and `Role`, currently not reconciled)
  grows into the service-account story: K-ORC `User`, `Project`, and role
  assignment resources for the service users of other OpenStack services
  (nova, glance, …), with passwords generated into OpenBao and rotated on
  schedule. For an existing installation this is the single biggest
  operational win of phase 1.
- **Catalog stewardship.** Existing services and endpoints become visible as
  imports; endpoint changes (new URLs, TLS migrations) become declarative
  once entries are promoted to managed.
- **Zero blast radius.** No database connection, no message queue, no running
  service is touched. Deleting the ControlPlane in phase 1 tears down only the
  K-ORC-managed application credential (its finalizer revokes it) and the
  OpenBao-backed Secrets — imports are left untouched by definition.

### Open questions for phase 1

- **Reachability and trust.** K-ORC runs in-cluster and must reach the
  external API: egress/NetworkPolicy posture, and CA-bundle delivery for
  private CAs. gophercloud honors a `cacert` key, but how a bundle travels
  from a Secret into K-ORC's credential resolution needs verification —
  possibly a documented constraint of the first iteration.
- **`endpoint_type` semantics.** In-cluster the operator deliberately forces
  `internal`; against an external installation the reachable interface is
  usually `public`. Making it configurable per External spec (as sketched)
  seems right, but the interplay with catalog entries that also list internal
  endpoints unreachable from the cluster needs a test matrix.
- **Readiness semantics.** What does `Ready` mean when nothing is deployed?
  The natural proxy is the K-ORC import conditions (a failed admin-user import
  means the external Keystone is unreachable or the credential is wrong).
  An active health probe would require an OpenStack client in the operator,
  which it deliberately does not have today — everything is K-ORC-mediated,
  and phase 1 should keep it that way.
- **Admin password rotation stays out.** The scheduled password rotation
  (rotation CronJob, staging/commit, push to OpenBao) lives in the Keystone
  operator and presumes the managed deployment. On a brownfield the password
  rotates out-of-band; updating the referenced Secret already triggers the
  hash-driven application-credential re-mint. Whether the ControlPlane should
  *validate* freshness (e.g. warn on stale seeds) is open.
- **Release skew.** `openStackRelease` drives image selection, which External
  mode doesn't use. Should the field validate against the actual version of
  the external Keystone (discoverable via the root endpoint), or is it purely
  advisory until phase 3? At takeover time it *must* match the existing
  database schema.
- **OpenBao path layout.** The per-CR scoped paths carry over unchanged, but
  the admin-password seed path is bootstrap-oriented; External mode reads the
  password only from the referenced Secret and never seeds, so the OpenBao
  seeding scripts and policies need an audit for assumptions about the
  managed flow.

## Phase 2: infrastructure attach

Two sub-steps, deliberately separated:

- **2a — attach.** Populate `spec.infrastructure` with the *existing*
  coordinates using the brownfield modes that already exist
  (`database.host`, `cache.servers`). The operator still deploys nothing; this
  validates connectivity and credential handling (user-supplied Secrets) while
  the external installation keeps running unchanged. Requires relaxing the
  rule that External mode forbids `infrastructure`, turning it into
  "optional".
- **2b — replace.** Transition database/cache from brownfield to managed mode
  (`clusterRef`), which today is blocked by mode immutability. The cache is
  trivial (stateless — flip the mode, endpoints move). The database is not:
  it needs a data-migration story (replication into the managed MariaDB, or
  dump/restore with a maintenance window) and a controlled cutover. That
  tooling does not exist and is a design of its own; this sketch only reserves
  the phase for it.

## Phase 3: Keystone service takeover

Flipping `mode: External` back to `Managed` — the gated transition. The known
hazards, all rooted in "the managed flow assumes a fresh database":

- **Key material must be imported first.** Fernet keys and credential
  encryption keys from the existing installation must land in OpenBao *before*
  the first operator-managed pod starts, otherwise every issued token and
  every stored credential invalidates at cutover. Today's key management only
  generates fresh keys; it needs an import path.
- **Bootstrap must not run.** `keystone-manage bootstrap` against an
  already-bootstrapped database is exactly the failure mode the upgrade path
  already works around (the re-run gate keyed on the admin-password digest
  exists because a re-run fails on the pre-existing admin user). Adoption
  needs an explicit skip/adopt gate for the bootstrap job rather than relying
  on digest coincidence.
- **Schema match, then upgrade.** `openStackRelease` must match the existing
  database schema at takeover; release upgrades (expand/migrate/contract)
  happen only after adoption, through the existing upgrade flow.
- **Endpoint cutover.** Either the catalog endpoints flip to the new gateway
  URL (declaratively, via the phase 1 catalog stewardship), or the existing
  URLs are kept and DNS/load-balancer targets move. Both should be supported;
  the first is where import-then-promote catalog semantics pay off.

## Phase 4: steady state

After phase 3 the ControlPlane is indistinguishable from a greenfield one:
managed infrastructure, managed Keystone, scheduled key and password rotation,
and the full credential lifecycle — the brownfield history survives only in
the (now historical) spec transitions.

## Risks

- **Catalog duplication.** Keystone happily stores duplicate service entries;
  a bug in import-vs-create logic pollutes the production catalog. Import
  semantics must be conservative (never create unless explicitly told to).
- **Re-mint revokes.** The application-credential re-mint is delete + recreate;
  the old credential is revoked at the Keystone level the moment the K-ORC
  finalizer runs. Anything in the existing installation that was handed the
  minted credential must consume it from OpenBao (or the materialized Secret),
  never from a copy.
- **Spec/reality drift.** A service-less ControlPlane describes an
  installation it does not control; the external Keystone can change under it
  (endpoints edited by hand, admin password rotated without updating the
  Secret). Conditions must surface drift loudly rather than fighting it.
- **Scope creep in phase 1.** The temptation is to "just add" a health probe,
  a schema check, a password rotator. Phase 1 stays credible exactly because
  it is K-ORC-only and touches nothing stateful.

## Suggested next steps

1. **Spike:** run K-ORC from a kind cluster against an external Keystone
   (clouds.yaml with an external `auth_url`, `endpoint_type: public`, private
   CA) and confirm imports, application-credential mint, and revoke-on-delete
   behave as assumed above.
2. **API design review:** settle the `mode`/`external` shape (or an
   alternative) and the optionality/immutability matrix for
   `infrastructure` — this decides how painful phases 2–3 become.
3. **Triage:** open a GitHub issue for phase 1 following the usual
   feature-triage flow; phases 2–4 remain sketches until phase 1 proves the
   model.
