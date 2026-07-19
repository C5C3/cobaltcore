---
name: prepare-new-service
description: >-
  Analyze and prepare the onboarding of a new OpenStack service into forge —
  inventory the five layers (container image, service operator, CI/e2e,
  ControlPlane integration, documentation) against the Keystone reference
  implementation, check what keystone scaffolding must be generalized into
  internal/common first, and draft the phased meta issue ready to be split
  into sub-issues. Use when asked to onboard or add a new OpenStack service
  (e.g. Glance, Nova, Neutron, Placement), to prepare a service meta issue,
  or to assess readiness for the next service operator.
---

# Prepare a new service onboarding

This skill turns "we want service X next" into a **phased meta issue** whose
checkboxes are sized to become sub-issues, plus (when needed) a separate
**generalization pre-work issue**. It analyzes; it does not implement.

Worked examples: Horizon — meta issue #552, generalization pre-work #551;
Glance — meta issue #656 with pre-work #653 (Garage S3 test infra), #654
(c5c3 groundwork: role assignments, generalized catalog, cross-namespace
credential delivery), #655 (common extractions). Read them before drafting
a new one; they are the calibration for scope, tone, and checkbox
granularity. #656 also demonstrates the alternative implementation mode —
Phases 2–4 as one continuous stacked-PR arc instead of fanned-out
sub-issues — and a Phase-0 decision record (D1–D10) worth imitating.

## The five layers

Every service in forge threads through five layers. Keystone
(`operators/keystone`) is the reference implementation for all of them.

| Layer | Canonical locations | Auto-extends? |
|---|---|---|
| 1. Container image | `images/<svc>/Dockerfile`, `releases/*/source-refs.yaml`, `releases/*/extra-packages.yaml`, `tests/container-images/verify_<svc>.sh` | build/test matrix: **yes** (from source-refs keys); hadolint matrix in `build-images.yaml`: **no** |
| 2. Service operator | `operators/<svc>/` (api, controller, webhook, helm chart on `operators/shared/helm/operator-library`), `go.work`, `Makefile` `OPERATORS` | **no** — module + enumerations by hand |
| 3. CI / e2e / deploy | `ci.yaml` paths-filter + `ALL_OPERATORS` + matrices, `tests/ci/verify_<svc>_ci_pipeline.sh`, `tests/e2e/<svc>/`, `tests/e2e/<svc>-operator/`, `tests/e2e-chaos/<svc>-*/`, `tests/tempest/<svc>-*/`, `deploy/flux-system/releases/<svc>-operator.yaml`, kind devstack wiring (`deploy/kind/base/openstack-gateway.yaml` listener + `deploy/kind/infrastructure/<svc>-nip-io-tls-certificate.yaml`, the `deploy/kind/base/kustomization.yaml` HelmRelease suspend patch **plus its counterparts in `hack/deploy-infra.sh`**: the flux-path un-suspend patch, the `enable_operator_servicemonitor` call, and the `hack/refresh-operator-image-digests.sh` target tuple — a service missing from the un-suspend list stays suspended on the quick-start path, its CRDs never install, and the c5c3-operator's controlplane cache never syncs), OpenBao bootstrap legs (`deploy/openbao/bootstrap/`, `deploy/openbao/policies/`) | chainsaw suites: **yes** (auto-discovered); ci.yaml wiring: **no** (3-step procedure in `hack/ci-resolve-changes.sh` header); devstack/OpenBao wiring: **no** |
| 4. ControlPlane (c5c3) | `ServicesSpec` in `operators/c5c3/api/v1alpha1/controlplane_types.go` (incl. `publicEndpoint` + `databaseCredentialsMode` per-service fields), `reconcile_<svc>.go` (+ `reconcile_<svc>_dbcredentials.go` for DB services), catalog row in `reconcile_catalog.go`, teardown in `reconcile_delete.go`, condition/instrumentation maps, RBAC markers + helm `_helpers.tpl`, scheme, webhook, envtest full chain (`integration_test.go`) + `tests/e2e/c5c3/full-controlplane-keystone/` | **no** — ~10 enumeration points |
| 5. Documentation | `docs/reference/<svc>/` (hand-written, `quadrant: operator` frontmatter), VitePress sidebar, per-service guides under `docs/guides/<svc>/`, quick-start extension (`docs/quick-start-controlplane.md`), `tests/unit/docs/` conventions | **no** — no doc generator exists |

## Procedure

### 1. Profile the service

Answer these before anything else — they decide which keystone machinery
applies and which decisions need a Phase-0 spike:

- **Database?** Which migration tool (alembic `db_sync` vs Django
  `migrate` vs none)? No DB ⇒ drop the database/db-sync/upgrade
  sub-reconcilers entirely. A DB service also inherits the **dynamic
  credential chain**: the shared `credentialsMode` on `DatabaseSpec`, a
  per-service `databaseCredentialsMode` override on its c5c3 service spec,
  a `provision_service_tenant` leg in
  `deploy/openbao/bootstrap/setup-database-tenant.sh`, a `<svc>-db` auth
  role + `<svc>-db-dynamic` policy, the generator-backed ExternalSecret
  glue (`reconcile_<svc>_dbcredentials.go` on the shared builders in
  `reconcile_dbcredentials.go`), and a
  `migrate-<svc>-db-to-dynamic-credentials` guide (keystone and glance are
  the two worked examples).
- **db-sync side-loads?** Some services need seed data beyond the schema
  migration (glance: `db load_metadefs`) — chain the extra `*-manage` step
  into the db-sync Job with an explicit `--path`, because the config-free
  image does not ship the oslo default locations.
- **Message bus?** No shared RabbitMQ spec or backing service exists yet —
  the **first** RabbitMQ consumer must add a `commonv1` messaging type and
  extend `InfrastructureSpec` + `hack/deploy-infra.sh`. That is its own
  pre-work issue.
- **Extra backing store?** (object store, message queue, cache beyond
  memcached) — a new backing service is its own pre-work issue (#653:
  Garage S3 for glance is the template — operator + declarative
  buckets/keys in `deploy/flux-system/infrastructure/`, OpenBao-seeded
  credentials materialized via ESO, kind ExternalSecrets), and it earns a
  **backing-store-outage chaos suite** (`glance-garage-outage`: writes
  fail closed, `/healthcheck` and `Ready` stay up).
- **Consumes another service's API?** A service user
  (`[keystone_authtoken]`-style) needs credential sourcing, c5c3 role
  assignments, and cross-namespace credential delivery — glance was the
  first consumer; #654/#655 are the templates.
- **Pluggable backends?** If users attach a variable number of backends
  (image stores, identity domains), model them as a **satellite CRD**
  mirroring `KeystoneIdentityBackend`/`GlanceBackend`: inverted
  attachment via `<svc>Ref`, dedicated per-backend controller + an
  aggregating sub-reconciler on the parent, curated `backends[]`
  projection from c5c3 with prefix-guarded pruning, plus the three suites
  the pattern demands (multi-instance, default/aggregation-switch with
  last-good retention, `invalid-<child>-cr`).
- **Service-catalog endpoints?** If yes, add a row to the generalized
  `managedCatalogRows` table in `reconcile_catalog.go` (K-ORC `Service` +
  `Endpoint`). Register **public + internal from birth** (the glance D6
  posture) — adding an interface later is a catalog migration. Extend the
  c5c3 teardown (`reconcile_delete.go`) for the new catalog CRs.
- **Config format?** oslo INI is covered by `internal/common/config`;
  anything else (Django settings, JSON) needs a renderer decision first.
- **Behavior breaks between the supported releases?** Check upstream
  release notes for launch-mode/WSGI/paste divergence between the pinned
  releases (glance 2025.2 eventlet vs 2026.1 uWSGI forced a
  release-switched command in the deployment builder). If yes, the
  per-release `basic-deployment-<slug>` e2e variant and the tempest legs
  must genuinely differ, and unit tests must pin the rendered config for
  **both** releases.
- **Stateful key material?** (fernet-like) — keystone's rotation machinery
  is deliberately NOT extracted; a second consumer changes that calculus.
- **Depends on other services?** Determines the gating condition in the
  c5c3 sub-reconciler chain (e.g. Horizon gates on `KeystoneReady`).
- **Tempest plugin maintained upstream?** If not (e.g. horizon), plan
  HTTP-level chainsaw assertions instead and say so explicitly.
- **Ingress?** `commonv1.GatewaySpec` / HTTPRoute via
  `internal/common/gateway` — plus the full public surface that hangs off
  it: an `https-<svc>` listener + hostname on the shared kind Gateway
  (`deploy/kind/base/openstack-gateway.yaml`) with a
  `<svc>-nip-io-tls-certificate.yaml` Certificate, a `publicEndpoint`
  catalog override on the c5c3 service spec (webhook-validated; it may be
  projected into no child, making the webhook the only gate), a
  `gateway-quick-start-smoke` e2e suite driving real host→listener→pod
  traffic, and the quick-start walkthrough doing its user-facing calls
  through the gateway.

### 2. Run the deterministic inventory

```bash
bash .claude/skills/prepare-new-service/scripts/inventory-touchpoints.sh <service>
```

It prints `[DONE]`/`[TODO]` per touch point across the five layers plus
gotcha warnings (e.g. the service already pinned in `upper-constraints.txt`).
It is an inventory, not a gate — for a fresh service everything is `[TODO]`;
its real value is catching **partial** onboarding and stale enumerations
when re-run mid-effort.

### 3. Verify the reference paths still hold

The repo evolves — do not trust this skill's tables blindly. Spot-check
that the enumeration points named above still exist at HEAD (grep for
`ALL_OPERATORS`, `subConditionTypes`, `OPERATORS ?=`, `ServicesSpec`), and
skim the per-layer "Adding a New Service" docs, which are authoritative
for layer 1, 2, and 3 details:

- `docs/contributing/adding-a-new-operator.md` (layer 2 — the documented
  onboarding path over `internal/common`)
- `docs/reference/ci-cd/build-images-workflow.md` § Adding a New Service
- `docs/reference/ci-cd/container-images.md` (release config files)
- `docs/reference/testing/tempest-test-infrastructure.md` § Adding a New Service
- `docs/reference/infrastructure/infrastructure-manifests.md` § Extensibility

### 4. Generalization pre-check (before drafting the meta)

Ask: **what would the new operator copy-paste from `operators/keystone`
a second (or third) time?** Classify keystone internals into:

1. thin wrappers over `internal/common` — copy as pattern, fine;
2. generic logic living in keystone (pipeline/status machinery, workload
   builders, watch mappers, webhook validators) — **extraction candidates**;
3. genuinely keystone-specific (fernet, bootstrap, trust-flush,
   expand-migrate-contract) — leave alone, rule of three.

If category 2 is non-empty, file (or update) a **separate refactor issue**
listing the candidates with file:line references, S/M/L effort, and a
must-before / opportunistic split — then mark the meta **blocked on it**.
#551 is the template (#655 is the second round: DB orchestration +
`keystone_authtoken` renderer for glance); check first whether it (or a
successor) is still open and simply needs extending. Also check open API-shape issues
(e.g. #471) — a new CRD must be born with the target shape, not the
legacy one.

### 5. Draft the meta issue

Follow the house format (#552, #550, #481): `Meta:` title prefix,
Background, phases with checkbox scope, explicit blocking relations,
Out of scope, italic footer with date + `main` SHA + relations.
Standard phase skeleton (drop/merge phases the profile rules out):

- **Phase 0 — decisions (spike):** session/config/secret-sourcing choices,
  upper-constraints handling, WSGI/launch mode per release, endpoint and
  catalog-interface wiring, backend-CRD shape if the profile calls for one
  (record the decisions in the meta as #656 records D1–D10).
- **Phase 1 — container image** (usually independent of pre-work).
- **Phase 2 — service operator scaffold** (blocked on generalization).
- **Phase 3 — CI, e2e, deploy stack** (alongside Phase 2) — including the
  kind Gateway listener/cert, OpenBao bootstrap legs, and chaos suites.
- **Phase 4 — ControlPlane integration** (blocked on Phase 2) — including
  catalog row, credential glue, teardown, envtest + full-ControlPlane e2e.
- **Phase 5 — documentation** (continuous, gates each phase) — reference
  set, per-service guides, and the quick-start extension.

Rules that keep it splittable:

- one checkbox = one sub-issue = one PR (Phase 0 may be a single spike);
- every checkbox names concrete files/paths, not intentions;
- include an ordering diagram when phases overlap;
- recommendations are stated as recommendations ("recommended: no DB
  sessions"), so the sub-issue can overturn them cheaply.

Create the issue with `gh issue create --label enhancement`, then
cross-link the pre-work issue's footer (`blocks #<meta>`).

### 6. Cross-check

Before publishing, sanity-check the claims that rot fastest:

- file:line references — re-grep each one at HEAD;
- "already pinned in upper-constraints" — `grep '^<svc>===' releases/*/upper-constraints.txt`;
- open-issue relations — `gh issue list --state open` for overlaps, so the
  meta references instead of duplicates.

Related skills for the implementation phase (mention them in the meta so
sub-issues use them as gates): [[check-crd-drift]], [[check-fixture-drift]],
[[check-condition-coverage]], [[check-validation-parity]],
[[check-doc-drift]], [[check-renovate-coverage]],
[[check-go-workspace-deps]], [[check-spdx-reuse]], and
[[check-service-parity]] as the closing cross-layer gate once the
onboarding lands (#656 used it exactly that way).

## Known gotchas (verified 2026-07 post-glance, re-verify at HEAD)

- **upper-constraints pin conflict:** some services (horizon, most
  clients' dashboards/libraries) are already pinned in
  `releases/*/upper-constraints.txt`. Installing from source with
  `--constraint` then requires the source ref to match the pin exactly,
  or a `-<svc>` line in `overrides/<release>/constraints.txt`
  (`scripts/apply-constraint-overrides.sh`). Check the service's
  **libraries** too, not just the service: glance itself is unpinned, but
  `glance_store`/`boto3` are — the driver extra must resolve against the
  existing pins.
- **hadolint matrix is static** in `build-images.yaml` — new Dockerfiles
  must be added by hand even though the build matrix auto-discovers.
- **Hand-maintained per-service lists in the verify/matrix scripts:**
  `tests/container-images/verify_release_config.sh` (`SERVICES=` list),
  `verify_deviation_comments.sh` (per-service functions),
  `hack/ci-generate-tempest-matrix.sh` (`for service in …` loop), and the
  chaos CI job's image-load lists in `ci.yaml` are all generalized now but
  still enumerate services by hand — extend each one, or the new
  service's coverage silently never runs.
- **The parameterized `operators/Dockerfile` (`ARG OPERATOR`) is still
  coupled to `go.work`:** it COPYs every module's go.mod and source, so a
  new module still edits that one Dockerfile's COPY lines (no more
  per-operator Dockerfiles, though).
- **WSGI entry points:** `uv pip install --prefix` skips PBR
  `wsgi_scripts` generation — service Dockerfiles hand-write their WSGI
  launcher (see `images/keystone/Dockerfile`). Also verify the stock WSGI
  module actually honors `--config-dir`/`--pyargv`: glance's
  `glance.wsgi.api:application` reads only its default config path and
  needed a hand-shipped shim (`images/glance/glance-wsgi-api`) to load
  the operator's mounted config dirs.
- **Paste-deploy divergence between releases:** factory references
  (`API.factory` vs `API_factory`) and oslo.middleware healthcheck
  semantics (filter tolerated in 2025.2, app-only in 2026.1) differ
  between the pinned releases — pin the rendered paste config in unit
  tests and run the e2e/tempest legs against both releases.

## Notes

- This skill is read-only with respect to the codebase; its outputs are
  GitHub issues (and this analysis). Implementation belongs to the
  sub-issues.
- If the user only wants the analysis, deliver the phase plan as text and
  skip issue creation — but still report what the inventory script found.
