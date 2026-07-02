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

`tests/e2e/c5c3/` holds five suites. Each applies one or more `ControlPlane`
CRs (`c5c3.io/v1alpha1`) and asserts operator behaviour end to end against the
live cluster. The directory is the canonical inventory.

Unlike the Keystone suites, the ControlPlane chain additionally requires K-ORC
and the c5c3-operator (on top of the keystone-operator, OpenBao, ESO, MariaDB,
and Memcached stack). The default kind E2E wiring does not install these, so
every suite follows the repo's **belt-and-braces presence-guard pattern**: a
runtime guard probes for the required CRDs and the OpenBao
`ClusterSecretStore`, and exits with a SKIP line when the stack is absent.
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
| [deletion-orchestration](#deletion-orchestration) | `deletion-orch` | ORC-teardown finalizer sequencing; deletion completes even when Keystone is already gone |
| [admin-password-scoping](#admin-password-scoping) | `controlplane` | Per-CR OpenBao-backed admin password projection |
| [db-credential-scoping](#db-credential-scoping) | `controlplane` | Per-CR OpenBao-backed service DB credential projection |
| [multi-controlplane](#multi-controlplane) | `controlplane-a`, `controlplane-b` | Per-CR admin-credential isolation across two tenants; rotation non-interference |

## Test Suite Details

### full-controlplane-keystone

Applies one `ControlPlane` CR and asserts the whole chain link by link, gating
each link on the previous one:

1. **Infrastructure** — owned MariaDB (`openstack-db`) and Memcached
   (`openstack-memcached`) created and owned by the ControlPlane;
   `InfrastructureReady=True`.
2. **Keystone** — owned Keystone CR (`controlplane-keystone-keystone`) with
   the image tag derived from `spec.openStackRelease` and database/cache
   clusterRefs wired to the infra CRs; `KeystoneReady=True`.
3. **ApplicationCredential** — owned K-ORC ApplicationCredential minted with
   `restricted: true`; `KORCReady=True`.
4. **Credential chain** — minted credential → operator Secret → PushSecret →
   OpenBao → operator-created per-CR `k-orc-clouds-yaml` ExternalSecret Ready;
   `AdminCredentialReady=True`.
5. **Catalog** — owned K-ORC Service and Endpoint; `CatalogReady=True`.
6. **Aggregate** — `Ready=True` with reason `AllReady`.
7. **API reachable** — Keystone `/v3` returns HTTP 200, and a verify Job runs
   `openstack token issue` and `openstack catalog list` (using the `openstack`
   CLI bundled in the tempest image) against the materialised admin
   clouds.yaml, proving the minted, pushed, re-materialised application
   credential actually authenticates.

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

Asserts that `reconcileDBCredentials` projects a per-ControlPlane,
OpenBao-backed DB credential: an owned ExternalSecret
`controlplane-keystone-db-credentials` whose `username` and `password`
remoteRefs read the per-CR OpenBao key
`openstack/keystone/{namespace}/{name}/db`, plus the materialised Secret. The
legacy flat shared-key DB ExternalSecret is gone.

### multi-controlplane

Brings up two ControlPlanes in two namespaces (`tenant-a/controlplane-a`,
`tenant-b/controlplane-b`) and asserts, against a live OpenBao, that each CR's
minted admin application credential lands on a distinct per-CR path
(`openstack/keystone/{namespace}/{name}/admin/app-credential`) with different
material, that both `status.adminApplicationCredential.id` values are
non-empty and distinct, and that rotating only tenant-a's credential leaves
tenant-b's status, OpenBao material, and K-ORC ApplicationCredential health
unchanged. Also asserts the operator-created per-CR `k-orc-clouds-yaml`
ExternalSecret (ownership and per-CR `remoteRef.key`) for both tenants.

## File Layout

```text
tests/e2e/c5c3/
├── admin-password-scoping/
│   ├── chainsaw-test.yaml              Per-CR admin-password projection
│   └── 00-controlplane-cr.yaml         Canonical ControlPlane CR
├── db-credential-scoping/
│   ├── chainsaw-test.yaml              Per-CR DB-credential projection
│   └── 00-controlplane-cr.yaml         Canonical ControlPlane CR
├── deletion-orchestration/
│   ├── chainsaw-test.yaml              ORC-teardown finalizer sequencing
│   └── 00-controlplane-cr.yaml         ControlPlane CR (deletion-orch)
├── full-controlplane-keystone/
│   ├── chainsaw-test.yaml              Full chain, link by link
│   ├── 00-controlplane-cr.yaml         ControlPlane CR (controlplane-keystone)
│   └── 01-openstack-verify-job.yaml    openstack CLI verify Job
└── multi-controlplane/
    ├── chainsaw-test.yaml              Two-tenant isolation contract
    ├── 00-tenant-a-controlplane.yaml   ControlPlane CR controlplane-a
    ├── 01-tenant-b-controlplane.yaml   ControlPlane CR controlplane-b
    └── 02-tenant-a-rotation.yaml       Rotation trigger for tenant-a only
```

## Related Resources

- [ControlPlane CRD API Reference](../c5c3/controlplane-crd.md) — CRD types, webhooks, validation rules
- [ControlPlane Reconciler](../c5c3/controlplane-reconciler.md) — Sub-reconciler contracts, finalizer, credential re-push
- [CI Workflow](../ci-cd/ci-workflow.md) — The dedicated `e2e-controlplane` job
- [Infrastructure E2E Deployment](../infrastructure/e2e-deployment.md) — `WITH_CONTROLPLANE` deployment wiring
- `tests/e2e/chainsaw-config.yaml` — Shared Chainsaw configuration
