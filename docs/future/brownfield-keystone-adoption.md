---
title: Brownfield Keystone Adoption
quadrant: operator
---

# Brownfield Keystone Adoption

> **Status: phase 1 is implemented.** The service-less, External-mode
> ControlPlane described below now exists. Use the
> [Adopt an External Keystone](../guides/adopt-external-keystone.md) guide to run
> it, and the [ControlPlane CRD](../reference/c5c3/controlplane-crd.md#externalkeystonespec)
> and [reconciler](../reference/c5c3/controlplane-reconciler.md) references for
> the authoritative behavior. **Phases 2–4 remain idea sketches** — nothing beyond
> phase 1 is implemented or scheduled.

## Motivation

In **Managed** mode a `ControlPlane` CR always ends in a running,
operator-deployed Keystone: the reconciler chain provisions infrastructure,
projects a Keystone CR, runs the bootstrap job, and only then wires up K-ORC
credentials and the catalog. For an existing (brownfield) OpenStack installation
that alone would be a big-bang proposition — the operator could not add value
before it owned the whole stack.

Phase 1 inverts the adoption order. A **service-less ControlPlane** deploys
*nothing* — no Keystone, no MariaDB, no cache — and takes over only the
**identity-plane management** that K-ORC provides against the existing,
externally running Keystone API:

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
would then happen in later, individually reversible phases on the **same CR**.
Those phases are still sketches.

## Baseline: what phase 1 built on

The building blocks were further along than one might expect — brownfield was
already a first-class concept, just not yet for the service layer.

**Already in place before phase 1:**

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
  the domain are created as *unmanaged* K-ORC resources — imports of objects that
  already exist in Keystone. This was exactly the mechanism a brownfield needs; in
  Managed mode it merely happens to import objects the operator's own bootstrap
  job created moments earlier. Phase 1 points the same mechanism at objects the
  operator never created.
- **Credential lifecycle machinery.** The admin application credential is
  minted by a managed K-ORC `ApplicationCredential`, backed up to OpenBao via
  PushSecret, re-materialized via ExternalSecret, and re-minted
  (delete + recreate) whenever the admin-password hash changes or a
  `CredentialRotation` CR nudges it. None of this logic cares *where* the
  Keystone API lives — only the endpoint URL does.

**Gaps that blocked a service-less mode** (all in `operators/c5c3`) — **all five
are closed by phase 1**:

1. **No way to say "manage identity, deploy nothing".** `services.keystone` is
   already an *optional pointer* (`*ServiceKeystoneSpec`); when it is nil the
   ControlPlane manages no identity plane and reports `KeystoneNotManaged`. But
   nil is deliberately **not** "external": absence carries no endpoint
   configuration, and an omitted field reads like an authoring mistake rather
   than intent. That is exactly why phase 1 added a `mode` discriminator
   (`Managed` | `External`) with an `external` block, rather than overloading
   absence. **Closed:** `mode: External` projects no Keystone child and reports
   `KeystoneReady=True/ExternallyManaged`.
2. **The auth URL was hardwired to the in-cluster Service.** Both clouds.yaml
   builders used `http://{name}-keystone.{namespace}.svc:5000/v3` with
   `endpoint_type: internal`; nothing could point K-ORC at an external Keystone.
   **Closed:** `external.authURL` plus a configurable `external.endpointType`
   (default `public`) and an optional `external.caBundleSecretRef`, projected
   into both credentials Secrets.
3. **`spec.infrastructure` was required.** **Closed:** it is now optional, and
   the validating webhook *forbids* it in External mode — a service-less
   ControlPlane needs neither database nor cache.
4. **The catalog sub-reconciler only created.** Pointed at an existing
   installation it would have duplicated catalog entries, since Keystone does not
   enforce unique service names. **Closed:** External mode is **import-first** —
   the existing identity service and its endpoints are imported as unmanaged
   K-ORC resources, and managed entries are created only on explicit opt-in. An
   ambiguous catalog fails loudly rather than guessing.
5. **The bootstrap seed assumed the managed flow.** **Closed:** External mode
   **never seeds** the OpenBao bootstrap path — the admin password is read only
   from the referenced Secret. Only the application-credential and service-account
   paths exist.

## Phase model

Each phase is a spec transition on the same `ControlPlane` CR, forward-only,
and leaves the system in a supported steady state. A brownfield operator can
stop at any phase indefinitely.

| Phase | Name | Status | Operator manages | Existing installation keeps |
|-------|------|--------|------------------|------------------------------|
| 1 | Identity-only (service-less) | **Implemented** | App credentials, service accounts, catalog entries, secret rotation via K-ORC + OpenBao | Keystone API, database, admin password, all data |
| 2 | Infrastructure attach | Sketch | Phase 1 + database/cache coordinates (brownfield `host`/`servers` mode), later managed replacements with data migration | Keystone API, admin password |
| 3 | Service takeover | Sketch | Phase 2 + the Keystone deployment itself (fernet/credential keys imported, bootstrap skipped, endpoint cutover) | — |
| 4 | Steady state | Sketch | Everything — indistinguishable from a greenfield ControlPlane | — |

Messaging (RabbitMQ) is not modeled in the ControlPlane CRD at all today —
Keystone needs none. The phase model reserves its takeover for the point where
additional services (which do need messaging) join the ControlPlane; it would
follow the same pattern as the database: brownfield coordinates first, managed
replacement with migration later.

## Phase 1: the service-less ControlPlane

**Implemented.** This section summarizes what shipped and how each of the
sketch's open questions was resolved. For the authoritative material see the
[Adopt an External Keystone](../guides/adopt-external-keystone.md) guide, the
[ControlPlane CRD reference](../reference/c5c3/controlplane-crd.md#externalkeystonespec),
and the [reconciler reference](../reference/c5c3/controlplane-reconciler.md).

### What shipped

A `mode` discriminator on the Keystone service entry, with an `external` block:

```yaml
spec:
  services:
    keystone:
      mode: External                 # default Managed
      external:
        authURL: https://keystone.example.com/v3
        endpointType: public         # default; the catalog interface K-ORC authenticates against
        caBundleSecretRef:           # optional, for private CAs
          name: brownfield-keystone-ca
        catalog:
          identityServiceName: keystone   # only needed to disambiguate duplicates
  korc:
    adminCredential:
      passwordSecretRef:
        name: brownfield-admin-password   # user-supplied, from the existing installation
    serviceAccounts:
      - name: nova
        project:
          name: service-nova
          create: true
```

`spec.infrastructure` became optional and is **forbidden** in External mode, as is
`services.horizon`. `mode` transitions are rejected in **both** directions, so
adoption is always a new CR — `External → Managed` is reserved for the phase-3
takeover, and `Managed → External` has no meaning.

The baseline analysis held up: the two heaviest sub-reconcilers (KORC,
AdminCredential) needed almost no change, because their logic is
endpoint-agnostic. The bulk of the work was API surface, the import-first catalog,
and the failure-classification vocabulary.

Sub-reconcilers that would deploy something short-circuit with
`Status=True, Reason=ExternallyManaged` (Infrastructure, DBCredentials,
AdminPassword, Keystone), so the condition schema is identical across modes. The
conditions that carry signal are `KORCReady`, `AdminCredentialReady`,
`CatalogReady`, and `ServiceAccountsReady`.

Beyond the original sketch, phase 1 also delivered **declarative service
accounts** (`korc.serviceAccounts`): managed K-ORC users and projects with
operator-generated, OpenBao-backed, rotatable passwords, collision-gated against
pre-existing users and adoptable on explicit consent. This is the operational win
the sketch predicted, and it landed in phase 1 rather than later.

### How the open questions resolved

- **Reachability and trust.** The CA bundle travels from a Secret into K-ORC as
  the inline `cacert` key beside `clouds.yaml` — which gophercloud reads natively,
  so no mount and no upstream change were needed. Egress remains the operator's
  responsibility: nothing restricts it by default, and a restrictive-egress cluster
  must allow K-ORC to reach the endpoint. A *rotated* bundle only takes effect
  after K-ORC's provider cache expires — a documented constraint, not a bug.
- **`endpoint_type` semantics.** Configurable per External spec, defaulting to
  `public`. The hazard the sketch worried about — a catalog whose entries point at
  interfaces unreachable from the cluster — is handled by failing **loudly**:
  a mismatch surfaces as `CatalogEndpointMismatch`, and a silently-empty import is
  caught by a stall detector that reports `ImportStalled` naming `endpoint_type`
  and `region`. Nothing waits forever.
- **Readiness semantics.** As the sketch proposed: K-ORC-proxied, with no
  OpenStack client in the operator. The failure classes (`AuthenticationFailed`,
  `EndpointUnreachable`, `TLSVerificationFailed`, `CatalogEndpointMismatch`,
  `CredentialDrift`, `ImportStalled`) are recovered from K-ORC's message, since
  K-ORC collapses every hard API failure into one transient condition.
- **Admin password rotation stays out.** Confirmed: it rotates out-of-band at the
  installation, and updating the referenced Secret drives the hash-driven
  application-credential re-mint. Freshness is **not** validated; a stale Secret
  surfaces as drift (`AuthenticationFailed`), which the operator reports and never
  remediates.
- **Release skew.** `openStackRelease` stays required but is **advisory** in
  External mode — no images are deployed. It must match the existing database
  schema only at the phase-3 takeover.
- **OpenBao path layout.** External mode **never seeds**. The bootstrap path does
  not exist for it; only the per-CR application-credential and service-account
  paths do, created on first push.

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

The phase-1 risks are **addressed by the implemented posture**; they are kept
here because phases 2–4 inherit them.

- **Catalog duplication.** Keystone happily stores duplicate service entries;
  a bug in import-vs-create logic would pollute the production catalog.
  *Addressed:* External mode is import-first and never creates unless explicitly
  told to; an ambiguous catalog fails loudly instead of guessing.
- **Re-mint revokes.** The application-credential re-mint is delete + recreate;
  the old credential is revoked at the Keystone level the moment the K-ORC
  finalizer runs. *Addressed:* the consumer contract — always read the credential
  from the materialized Secret or its OpenBao path, never from a copy — is stated
  in the [adoption guide](../guides/adopt-external-keystone.md).
- **Spec/reality drift.** A service-less ControlPlane describes an installation it
  does not control; the external Keystone can change under it (endpoints edited by
  hand, admin password rotated without updating the Secret). *Addressed:* drift is
  surfaced as dedicated conditions (`AuthenticationFailed`, `CredentialDrift`,
  `ImportStalled`) and never fought — the operator does not write to the external
  installation.
- **Scope creep in phase 1.** The temptation was to "just add" a health probe, a
  schema check, a password rotator. *Held:* phase 1 stayed K-ORC-only and touches
  nothing stateful; readiness is K-ORC-proxied and the operator carries no
  OpenStack client.

## Suggested next steps

Phases 2–4 remain sketches. The natural next step is to triage **phase 2**
(infrastructure attach) through the usual feature-triage flow, now that phase 1
has proven the model — in particular that the mode discriminator, the import-first
catalog, and the K-ORC-proxied readiness hold up against a real external
installation.

The two hard problems phase 2 and phase 3 still owe a design:

- **Data migration** for the database cutover (phase 2b) — replication or
  dump/restore with a maintenance window; no tooling exists.
- **Key material import** (fernet and credential encryption keys) before the first
  operator-managed pod starts, plus an explicit bootstrap skip/adopt gate
  (phase 3).
