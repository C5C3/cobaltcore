---
title: ControlPlane E2E Test Suites
quadrant: operator
---

# ControlPlane E2E Test Suites

Reference documentation for the Chainsaw E2E test suites covering the
c5c3-operator's `ControlPlane` orchestration. These suites live in
`tests/e2e/c5c3/` and exercise the full ControlPlane → Keystone chain:
infrastructure projection, per-CR credential scoping in OpenBao, the K-ORC
application-credential handoff, catalog registration, deletion orchestration,
and multi-tenant isolation.

For the reconciler architecture and sub-reconciler contracts, see
[ControlPlane Reconciler](../c5c3/controlplane-reconciler.md). For the
Keystone-level suites, see [Keystone E2E Test Suites](./keystone-e2e-tests.md).

## Overview

The `tests/e2e/c5c3/` directory holds the ControlPlane suites. Each applies one
or more `ControlPlane` CRs (`c5c3.io/v1alpha1`) and asserts operator behaviour
end to end against the live cluster. The directory is the canonical inventory;
the table below is a guide, not a count.

Unlike the Keystone suites, the ControlPlane chain additionally requires K-ORC
and the c5c3-operator (on top of the keystone-operator, OpenBao, ESO, MariaDB,
and Memcached stack). The default kind E2E wiring does not install these, so
every suite follows the repo's **belt-and-braces presence-guard pattern**: a
runtime guard probes for the required CRDs, the OpenBao
`ClusterSecretStore`, and — for the suites whose ControlPlanes must actually
converge — **running keystone-operator pods**, exiting with a SKIP line when
any of it is absent. The pod probe matters because CRDs alone no longer imply
the stack: the `e2e-operator` c5c3 matrix leg installs every CRD the c5c3
controller watches (its informers cannot start otherwise) without deploying
the sibling operators.
Because Chainsaw has no step-level skip and the shared config runs with
`failFast: true`, the guard and all assertions live in a single script step.

Setting `E2E_REQUIRE_CONTROLPLANE_STACK=true` flips the guard from SKIP to a
hard failure. The dedicated `e2e-controlplane` CI job does exactly that: it
deploys keystone-operator, c5c3-operator, and K-ORC as local dev images
(`CONTROLPLANE_OPERATORS=external`), seeds the per-CR OpenBao paths
(`CONTROLPLANE_NAME=controlplane-keystone`), and runs the
`full-controlplane-keystone` suite so broken wiring in the live chain fails
the build instead of skipping. See the
[CI workflow reference](../ci-cd/ci-workflow.md) for the job definition.

## Running the Tests

```bash
# Bring up the full stack locally (kind)
WITH_CONTROLPLANE=true CONTROLPLANE_OPERATORS=external \
  CONTROLPLANE_NAME=controlplane-keystone make deploy-infra
hack/ci-deploy-korc.sh
OPERATOR=keystone IMAGE_REPO=<registry>/keystone-operator NAMESPACE=keystone-system hack/ci-deploy-operator.sh
OPERATOR=c5c3 IMAGE_REPO=<registry>/c5c3-operator NAMESPACE=c5c3-system hack/ci-deploy-operator.sh

# Run a single suite, failing loudly if the stack is missing
E2E_REQUIRE_CONTROLPLANE_STACK=true chainsaw test \
  --config tests/e2e/chainsaw-config.yaml \
  tests/e2e/c5c3/full-controlplane-keystone/
```

Without the stack the suites skip cleanly, so `make e2e` (which runs the whole
`tests/e2e/` tree) stays safe on clusters that only carry the Keystone wiring.

## Test Suite Inventory

| Suite | CR Name(s) | Behaviour Validated |
| --- | --- | --- |
| [full-controlplane-keystone](#full-controlplane-keystone) | `controlplane-keystone` | The entire orchestration chain, link by link, through aggregate `Ready` and a live API check |
| [external-keystone](#external-keystone) | `controlplane-external` (+ 3 negative CRs) | External mode against a plain, operator-free Keystone: convergence with zero children, imports, the app-credential round-trip, no catalog pollution, service accounts, drift + rotation, `endpoint_type` detection, and zero-blast-radius deletion |
| [federated-controlplane](#federated-controlplane) | `controlplane-sso` | The end-user SSO experience: websso projection, the login page's SSO choice and domain field, the websso round trip through the gateway |
| [deletion-orchestration](#deletion-orchestration) | `deletion-orch` | ORC-teardown finalizer sequencing; deletion completes even when Keystone is already gone |
| [admin-password-scoping](#admin-password-scoping) | `controlplane` | Per-CR OpenBao-backed admin password projection |
| [db-credential-scoping](#db-credential-scoping) | `controlplane` | Per-CR OpenBao-backed service DB credential projection |
| [dedicated-backing-services](#dedicated-backing-services) | `cp` (ephemeral namespace) | Opt-in per-service dedicated database/cache: provisioning, ownership, sizing, and collective readiness gating |
| [dedicated-namespaces](#dedicated-namespaces) | `cp` (ephemeral namespace) | Per-service dedicated namespaces: Managed/External lifecycles, backing-service placement, ownership labels, per-namespace tenant stores, and the deletion sweep |
| [multi-controlplane](#multi-controlplane) | `controlplane-a`, `controlplane-b` | Per-CR admin-credential isolation across two tenants; rotation non-interference |
| [secret-store-scoping](#secret-store-scoping) | — (namespace-only) | Per-ControlPlane OpenBao identity via a namespaced `SecretStore`; OpenBao-enforced cross-tenant isolation |

## Test Suite Details

### full-controlplane-keystone

Applies one `ControlPlane` CR and asserts the whole chain link by link, gating
each link on the previous one:

1. **Infrastructure** — owned MariaDB (`openstack-db`) and Memcached
   (`openstack-memcached`) created and owned by the ControlPlane;
   `InfrastructureReady=True`. The suite then onboards the per-tenant OpenBao
   database-engine role (`setup-database-tenant.sh`), waits for
   `DBCredentialsReady=True`, and asserts the generator-backed ExternalSecret and
   engine-issued username.
2. **Keystone** — owned Keystone CR (`controlplane-keystone-keystone`) with
   the image tag derived from `spec.openStackRelease`, database/cache clusterRefs
   wired to the infra CRs, and `spec.database.credentialsMode: Dynamic`;
   `KeystoneReady=True`.
3. **ApplicationCredential** — owned K-ORC ApplicationCredential minted with
   `restricted: true`; `KORCReady=True`.
4. **Credential chain** — minted credential → operator Secret → PushSecret →
   OpenBao → operator-created per-CR `k-orc-clouds-yaml` ExternalSecret Ready;
   `AdminCredentialReady=True`.
5. **Catalog** — owned K-ORC Service and Endpoint; `CatalogReady=True`.
6. **Aggregate** — `Ready=True` with reason `AllReady`.
6b. **Dynamic DB credential engine** — no static DB password remains at rest (the
   retired per-CR KV path is absent, AC 2/6); an engine-issued credential
   authenticates against MariaDB and is rejected after `bao lease revoke` (AC 3);
   and an unrelated lease survives another's revoke while the ControlPlane stays
   Ready (AC 4 single-tenant isolation).
7. **API reachable** — Keystone `/v3` returns HTTP 200, and a verify Job runs
   `openstack token issue` and `openstack catalog list` (using the `openstack`
   CLI bundled in the tempest image) against the materialised admin
   clouds.yaml, proving the minted, pushed, re-materialised application
   credential actually authenticates.

### external-keystone

Proves the External-mode adoption contract against a Keystone the operator does
**not** own. The suite stands up a plain, operator-free Keystone fixture
(`00-fixture-keystone.yaml`, a single SQLite-backed pod with its own bootstrap
history and admin Secret) in the `brownfield-keystone` namespace and populates
its catalog with a non-default admin identity (domain `heimdall`, project
`platform-admin`, user `brownfield-admin`) and a duplicate identity service
(`01-fixture-catalog-setup-job.yaml`). It then drives four External
`ControlPlane`s against that fixture and asserts, in one consolidated script:

1. **Converge** — the main External CR reaches `Ready=True/AllReady` with **zero**
   MariaDB/Memcached/Keystone/Horizon children; the skipped sub-reconcilers report
   `ExternallyManaged` and Horizon reports `HorizonNotManaged`.
2. **Imports resolve** — `KORCReady=ApplicationCredentialMinted`,
   `CatalogReady=CatalogImported`, and `status.catalog.imports` shows the identity
   Service plus three Endpoint interfaces resolved with OpenStack ids.
3. **App credential** — minted against the external Keystone, present at the per-CR
   OpenBao path with the ESO `managed-by` stamp and in a materialised clouds.yaml
   targeting the external API; a verify Job authenticates a client with it.
4. **No pollution** — the external services, endpoints, and domains are byte-for-byte
   identical to a pre-recorded baseline; users and projects gained exactly the one
   declared service account.
5. **Service accounts** — the declared account issues an unscoped token, and a
   `CredentialRotation` round-trips its password (new works, old is rejected).
6. **Drift + rotation** — changing the external admin password without updating the
   Secret makes a forced re-mint fail loudly (a documented drift reason, no
   remediation); updating the Secret then drives a hash-driven re-mint to a fresh
   credential, and the old application credential is invalid.
7. **endpoint_type detection** — a ControlPlane pinned to an unreachable internal
   interface fails loudly (never a silent-empty import), while `public` converges.
8. **Negative paths** — a wrong password yields a distinct `AuthenticationFailed`
   condition; an ambiguous identity catalog fails with `CatalogFailed`.
9. **Zero blast radius** — deleting the main CP revokes the app credential, removes
   the OpenBao-backed Secrets, emits `ORCTeardownComplete`, leaves the external
   users/domains/catalog bit-for-bit identical to the baseline, and the fixture
   keeps serving tokens.

Run it locally against a full ControlPlane stack with
`E2E_REQUIRE_CONTROLPLANE_STACK=true make e2e-external-keystone`. It has its own
dedicated `e2e-external-keystone` CI job (see the
[CI workflow reference](../ci-cd/ci-workflow.md)).

### deletion-orchestration

Covers the `c5c3.io/orc-teardown` finalizer. Drives a ControlPlane to Ready,
initiates deletion, then deletes the projected Keystone CR so K-ORC can no
longer revoke the admin credential against a live API. Asserts that
ControlPlane deletion still **completes** within a window larger than the
bounded stall timeout (`orcTeardownStallTimeout`, 5m): the finalizer waits,
then force-removes the stuck `openstack.k-orc.cloud/*` finalizers. Also
asserts the projected Keystone, MariaDB, Memcached, and all five K-ORC CRs are
garbage-collected and an ORC-teardown event (`ORCTeardownComplete`, or the
Warning `ORCTeardownStalled` on the stalled path) was emitted.

### admin-password-scoping

Asserts that `reconcileAdminPassword` projects a per-ControlPlane,
OpenBao-backed admin password: an owned ExternalSecret
`controlplane-keystone-admin-credentials` whose `password` remoteRef reads the
per-CR OpenBao key `bootstrap/{namespace}/{keystoneName}/admin`, plus the
materialised Secret of the same name. The path is keystone-name scoped so it
matches the keystone-operator's scheduled admin-password rotation PushSecret,
which reads and writes the same key.

### db-credential-scoping

Onboards the per-tenant OpenBao database-engine role
(`setup-database-tenant.sh`) and asserts that `reconcileDBCredentials` projects a
per-ControlPlane, DYNAMIC (engine-issued) DB credential: a `VaultDynamicSecret`
generator reading `database/mariadb/creds/keystone-{namespace}`, an owned
`ExternalSecret` `controlplane-keystone-db-credentials` drawing from that
generator via `dataFrom.sourceRef.generatorRef` (no static Data refs), a
`keystone-db-creds` ServiceAccount, and a materialised Secret carrying an
engine-issued username (not the static `keystone` user). The stage-(a) static
per-CR KV seed is retired (#439).

### dedicated-backing-services

Asserts the opt-in per-service
[dedicated backing services](../c5c3/controlplane-crd.md#dedicatedbackingservices):
a `ControlPlane` whose Keystone service takes a dedicated database **and** cache
and whose Horizon dashboard takes a dedicated cache. It proves a dedicated
instance carries the shared block's lifecycle — it is provisioned as a `MariaDB` /
`Memcached` child, owned with a **controller owner reference and
`blockOwnerDeletion`** (the mechanism that tears it down with the ControlPlane),
sized from its **own** `replicas` / `storageSize`, and gates `InfrastructureReady`
so the consuming service waits for the database it actually talks to.

The fixture's **shared** block is brownfield, so the ControlPlane provisions
nothing for it: the exact set of `MariaDB` / `Memcached` CRs in the namespace *is*
the dedicated set, which the suite asserts as an exact set rather than a superset
— the proof that a service which opted out no longer gets the shared instance.

Unlike the sibling suites it runs in **chainsaw's ephemeral namespace** with a
ControlPlane of its own (`cp`) rather than reusing the canonical `openstack` one.
Two contracts rule that out: the webhook permits one ControlPlane per namespace,
and the shared↔dedicated presence flip is frozen on a live CR, so the dedicated
declaration cannot be patched onto the pre-existing shared ControlPlane — it has
to be created with it.

The suite's presence guard probes every CRD the ControlPlane controller
watches (the Keystone, KeystoneIdentityBackend, Horizon, and K-ORC kinds
alongside the ones asserted): a controller-runtime informer for a kind whose
CRD is absent never syncs, so on a cluster missing any of them the operator's
elected leader dies on the cache-sync timeout and the reconciler this suite
drives never runs at all.

The projected-child assertions (the Keystone child pointing at the dedicated
instances, with `credentialsMode: Static`) are deliberately not part of the
suite: reaching the Keystone projection requires the DB-credential and
admin-password machinery to converge, which needs OpenBao seeded for the
ControlPlane's namespace — and this suite runs in an ephemeral namespace by
design. They are hard-asserted in the envtest scenario
`TestIntegration_DedicatedBackingServices`, which runs against the real CRD
schema and webhook on every PR.

### dedicated-namespaces

Asserts per-service
[dedicated namespaces](../c5c3/controlplane-crd.md#service-namespaces): a
`ControlPlane` that places its Keystone service in an operator-owned (`Managed`)
namespace and its Horizon dashboard in a pre-existing (`External`) one. It proves
the placement and lifecycle contract on a live cluster:

- `NamespacesReady` goes `True` once the `External` namespace (pre-created by the
  test) is present and the `Managed` one has been created by the operator;
- the `Managed` namespace carries the ownership labels plus
  `app.kubernetes.io/managed-by`; the `External` namespace is left **unlabelled**;
- the one shared `spec.infrastructure` block materializes its backing services in
  **each service's** namespace — a `MariaDB` and `Memcached` in the Keystone
  namespace, a `Memcached` in the Horizon namespace — each carrying the ownership
  labels and **no owner reference** (Kubernetes forbids a cross-namespace one),
  and nothing in the ControlPlane's own namespace;
- a per-tenant `openbao-tenant-store` `SecretStore` is provisioned in every
  namespace the ControlPlane occupies;
- on deletion the cross-namespace children are torn down explicitly (no GC
  cascade reaches them), the `Managed` namespace is deleted, and the `External`
  namespace **survives** with its ControlPlane residue swept.

Like the sibling dedicated-backing-services suite it runs in **chainsaw's
ephemeral namespace** with a ControlPlane of its own, deriving the two service
namespaces from it: the namespace-assignment fields are frozen after creation and
a service namespace is a tenant key admission reserves to one ControlPlane, so the
suite cannot reuse the canonical `openstack` ControlPlane. It carries the same CRD
presence guard and `E2E_REQUIRE_CONTROLPLANE_STACK` escalation.

The credential material and the projected Keystone child sit behind the
OpenBao-seeded DB-credential / admin-password machinery this ephemeral suite
cannot reach; the full cross-namespace readiness (the credential ExternalSecrets,
the projected child, the OpenBao path re-keying) is hard-asserted in the envtest
scenario `TestIntegration_DedicatedNamespaces` on every PR.

### multi-controlplane

Brings up two ControlPlanes in two namespaces (`tenant-a/controlplane-a`,
`tenant-b/controlplane-b`), onboards each tenant's distinct database-engine role,
and asserts admin-credential isolation (each CR's minted admin application
credential lands on a distinct per-CR path with different material; rotating only
tenant-a's credential leaves tenant-b unchanged) **and** dynamic DB-credential
isolation: the two tenants draw from distinct per-tenant roles, and revoking
tenant-a's DB leases by prefix leaves tenant-b's credential authenticating and
tenant-b Ready (AC 4).

### secret-store-scoping

Exercises the half of the per-ControlPlane secret-store feature (#605) that unit
and integration tests cannot reach: the **live OpenBao identity** a ControlPlane
gets through a namespaced `SecretStore`, and OpenBao's own enforcement of
cross-tenant isolation. Running in the ephemeral test namespace, the suite:

1. runs `setup-eso-tenant.sh <namespace>`, which provisions the tenant
   `ServiceAccount` (`eso-tenant-auth`), the cert-manager mTLS `Certificate`, and
   the namespaced `SecretStore` (`openbao-tenant-store`);
2. asserts that `SecretStore` reaches `Ready=True` — proving the `eso-tenant`
   auth role, the `eso-tenant` templated policy, and mTLS actually authenticate
   the per-tenant identity against OpenBao;
3. mints a token from the tenant's `eso-tenant-auth` ServiceAccount, logs in as
   the `eso-tenant` role, and proves the token can read **its own** namespace's
   Keystone key path but is **denied** on a foreign namespace's path — the
   templated-policy isolation that replaces the naming convention;
4. logs in as the shared `eso-management` role and proves it is **denied** both
   read and write on a Keystone key path (#606 retired the `push-*` write
   policies and dropped `eso-management`'s `openstack/keystone/*` read) while
   still reading the retained shared `bootstrap/*` subtree;
5. applies an `ExternalSecret` referencing the shared `openbao-cluster-store`
   from the non-allow-listed ephemeral namespace and proves it never goes
   `Ready`, because #606 restricted the cluster store with `spec.conditions`;
6. drives the **never-seeded first-push** round-trip on a per-CP app-credential
   path (`openstack/keystone/{ns}/{cp}/admin/app-credential`, the shape the
   External-mode round-trip uses) through the operator-default tenant store:
   the first `PushSecret` creates the leaf and ESO stamps
   `managed-by=external-secrets` itself (the managed-by guard's inverse), a
   read-back `ExternalSecret` materialises the exact value, and
   `DeletionPolicy: Delete` purges the leaf via the `eso-tenant` delete grant —
   with zero seeded state and nothing per-CP beyond the one-time cluster
   bootstrap.

The ControlPlane→Keystone/Horizon projection and the `SecretsReady` gating are
covered by the c5c3 operator integration test
(`TestIntegration_SecretStoreRefProjectedAndGated`), so this suite focuses on the
behaviour only a live OpenBao can prove. It SKIPs cleanly when the stack — or the
`eso-tenant` role (bootstrap predating #605) — is absent.

## File Layout

```text
tests/e2e/c5c3/
├── admin-password-scoping/
│   ├── chainsaw-test.yaml              Per-CR admin-password projection
│   └── 00-controlplane-cr.yaml         Canonical ControlPlane CR
├── db-credential-scoping/
│   ├── chainsaw-test.yaml              Per-CR DB-credential projection
│   └── 00-controlplane-cr.yaml         Canonical ControlPlane CR
├── dedicated-backing-services/
│   ├── chainsaw-test.yaml              Per-service dedicated database/cache opt-in
│   └── 00-controlplane-cr.yaml         ControlPlane CR (cp; ephemeral namespace)
├── dedicated-namespaces/
│   ├── chainsaw-test.yaml              Per-service dedicated namespaces + lifecycles
│   └── 00-controlplane-cr.yaml         ControlPlane CR (cp; @KEYSTONE_NS@/@HORIZON_NS@ tokens)
├── deletion-orchestration/
│   ├── chainsaw-test.yaml              ORC-teardown finalizer sequencing
│   └── 00-controlplane-cr.yaml         ControlPlane CR (deletion-orch)
├── external-keystone/
│   ├── chainsaw-test.yaml              External mode vs a plain, operator-free Keystone
│   ├── 00-fixture-keystone.yaml        Plain SQLite Keystone fixture (no operator)
│   ├── 01-fixture-catalog-setup-job.yaml  Non-default identities + duplicate service
│   ├── 02-admin-password-secret.yaml   Fixture admin password for the main CR
│   ├── 02-controlplane-external.yaml   Main External CR + service account
│   ├── 03-controlplane-wrong-password.yaml  Wrong-password negative case
│   ├── 04-controlplane-stalled.yaml    Unreachable-internal-endpoint case
│   ├── 05-controlplane-ambiguous.yaml  Ambiguous-catalog negative case
│   └── 06-openstack-verify-job.yaml    openstack CLI verify Job
├── full-controlplane-keystone/
│   ├── chainsaw-test.yaml              Full chain, link by link
│   ├── 00-controlplane-cr.yaml         ControlPlane CR (controlplane-keystone)
│   └── 01-openstack-verify-job.yaml    openstack CLI verify Job
├── multi-controlplane/
│   ├── chainsaw-test.yaml              Two-tenant isolation contract
│   ├── 00-tenant-a-controlplane.yaml   ControlPlane CR controlplane-a
│   ├── 01-tenant-b-controlplane.yaml   ControlPlane CR controlplane-b
│   └── 02-tenant-a-rotation.yaml       Rotation trigger for tenant-a only
└── secret-store-scoping/
    └── chainsaw-test.yaml              Per-tenant OpenBao identity + isolation
```

## Related Resources

- [ControlPlane CRD API Reference](../c5c3/controlplane-crd.md) — CRD types, webhooks, validation rules
- [ControlPlane Reconciler](../c5c3/controlplane-reconciler.md) — Sub-reconciler contracts, finalizer, credential re-push
- [CI Workflow](../ci-cd/ci-workflow.md) — The dedicated `e2e-controlplane` job
- [Infrastructure E2E Deployment](../infrastructure/e2e-deployment.md) — `WITH_CONTROLPLANE` deployment wiring
- `tests/e2e/chainsaw-config.yaml` — Shared Chainsaw configuration

### federated-controlplane

`tests/e2e-controlplane-sso/` — the end-user SSO experience the ControlPlane
drives from its Keystone child's identity backends.

A **separate suite and a separate CI job** (`e2e-controlplane-sso`), not an
extension of `full-controlplane-keystone`: the identity-provider and directory
fixtures would otherwise lengthen that chain and couple its credential
assertions to federation, and — decisively — the ControlPlane webhook permits
one ControlPlane per namespace while `openstack-gw` sets
`allowedRoutes.namespaces.from: Same`. The two suites can share neither the
`openstack` namespace nor the Gateway, so each needs its own kind cluster.

It lives **outside `tests/e2e/`** (like `tests/e2e-operator-upgrade/`) because
it keeps declarative `assert` steps rather than the single guarded script step
`full-controlplane-keystone` uses. Chainsaw has no step-level skip, so a
presence guard cannot stop those asserts from running; moving the suite is what
keeps the per-CR `e2e-operator` job and `make e2e` from sweeping it up.

| Step | Behaviour Validated |
| --- | --- |
| 1. `controlplane-ready` | Keycloak, OpenLDAP, and the per-CP Horizon `SECRET_KEY` ExternalSecret come up; the ControlPlane reaches aggregate `Ready` |
| 2. `backends-ready` | Both `KeystoneIdentityBackend` CRs reach `Ready` and the Keystone child reports `IdentityBackendsReady=AllBackendsProjected` |
| 3. `projections` | Attaching the backends is the ONLY action taken, yet the Horizon child now carries the websso choices and multi-domain support, and the Keystone child the trusted origin and the `dev`-tagged sidecar image |
| 4. `rendered-settings` | The rendered `local_settings.py` carries `WEBSSO_ENABLED`, `WEBSSO_USE_HTTP_REFERER = False`, `SECURE_PROXY_SSL_HEADER`, and multi-domain support — but neither `OPENSTACK_KEYSTONE_DOMAIN_DROPDOWN` nor `OPENSTACK_KEYSTONE_DOMAIN_CHOICES`, which would bound the domain field to the domains the operator can enumerate |
| 5. `browser-sso-round-trip` | One in-cluster browser, one cookie jar, three flows: (a) the login page offers the SSO choice and a free-text domain field; (b) the websso round trip completes through the gateway against the origin Keystone matches verbatim; (c) an LDAP-domain user logs in through that field |
| 6. `detach` | Deleting both backends clears `spec.websso` and `spec.multiDomain`; `trustedDashboards` survives, since it is derived from `services.horizon`, not from the backends |

**The browser runs in-cluster.** Unlike the gateway quick-start smokes (which
curl from the CI host through the kind `:443` → NodePort bridge), this suite
cannot: mid-flow the browser is redirected to Keycloak's issuer, the in-cluster
`keycloak.openstack.svc.cluster.local` name the host cannot resolve. Exposing
Keycloak through the gateway instead would need a split-horizon DNS rewrite,
since `mod_auth_openidc` must reach the same issuer from inside the cluster.
The browser is therefore the Keystone pod (the image ships `python3`, no
`curl`), dialling the Envoy data-plane ClusterIP with the gateway hostname as
SNI and `Host`, so traffic traverses Envoy exactly as a real browser's would.

The ControlPlane CR pins `services.keystone.federationProxyImage.tag: dev` so
the suite exercises the `mod_auth_openidc` sidecar built by the pipeline, not
the `:latest` already published on `main`.
