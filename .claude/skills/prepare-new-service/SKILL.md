---
name: prepare-new-service
description: >-
  Analyze and prepare the onboarding of a new OpenStack service into CobaltCore —
  inventory the five layers (container image, service operator, CI/e2e,
  ControlPlane integration, documentation) against the Keystone reference
  implementation and the services onboarded since, check what scaffolding
  must be generalized into internal/common first, and draft the phased meta
  issue that /planwerk:meta splits into sub-issues. Use when asked to
  onboard or add a new OpenStack service (e.g. Designate, Octavia, Manila,
  Heat), to prepare a service meta issue, or to assess readiness for the
  next service operator. Also covers OpenStack-adjacent services from
  outside the upstream (e.g. the Aurora dashboard) — see
  references/non-openstack-upstream-services.md.
---

# Prepare a new service onboarding

This skill turns "we want service X next" into a **phased meta issue** whose
checkboxes are sized to become sub-issues, plus (when needed) a separate
**generalization pre-work issue**. It analyzes; it does not implement.

The meta is the input of the author's planwerk chain — `/planwerk:meta`
splits it into sub-issues, `/planwerk:decide` settles the deferred Phase-0
decisions, `/planwerk:elaborate` plans each sub-issue, `/planwerk:revisit`
re-checks one a sibling made stale — so size it for that chain.
[references/worked-examples.md](references/worked-examples.md) walks the
chain and maps each past onboarding to the situation it teaches: Horizon
#552 (no database), Glance #656 (backing-store pre-work, satellites),
Aurora #758 (non-OpenStack upstream), Neutron + OVN #898 (message bus),
Cinder #979 (a meta split across many sub-issues), Nova #1014 with #1012
(two databases, cross-service dependencies) and #1013 (data plane on other
clusters). Read the ones matching the profile before drafting.

## The five layers

Every service in CobaltCore threads through five layers. Keystone
(`operators/keystone`) is the reference implementation for all of them.

| Layer | Canonical locations | Auto-extends? |
|---|---|---|
| 1. Container image | `images/<svc>/Dockerfile`, `releases/*/source-refs.yaml`, `releases/*/extra-packages.yaml`, `patches/<svc>/<release>/`, `tests/container-images/verify_<svc>.sh`, option catalogs `operators/<svc>/api/v1alpha1/catalogs/<release>.json` (a `<svc>)` arm in `hack/gen-option-catalog.sh`) | build/test matrix: **yes** (from source-refs keys); hadolint matrix, `ALL_SERVICES` and the `svc_<svc>` filter in `build-images.yaml`: **no** |
| 2. Service operator | `operators/<svc>/` (api, controller — including the recurring-maintenance CronJobs of [references/recurring-maintenance-jobs.md](references/recurring-maintenance-jobs.md) and the target-cluster placement artefacts of [references/placement-on-target-clusters.md](references/placement-on-target-clusters.md) —, webhook, helm chart on `operators/shared/helm/operator-library`, `dashboards/<svc>-operator.json`), `go.work`, `Makefile` `OPERATORS`, the `operators/Dockerfile` COPY lines | **no** — module + enumerations by hand |
| 3. CI / e2e / deploy | `ci.yaml` paths filters + `ALL_OPERATORS` + `SERVICE_OPERATORS` + matrices, `.codecov.yml`, `tests/unit/ci/<svc>_e2e_matrix_test.sh` (the older `tests/ci/verify_<svc>_ci_pipeline.sh` pattern runs nowhere), `tests/e2e/<svc>/` + its `invalid-cr/_generate.py` wired into `make verify-invalid-cr-fixtures`, `tests/e2e/<svc>-operator/`, `tests/e2e-chaos/<svc>-*/`, `tests/tempest/<svc>-<release-slug>/` + `ALL_TEMPEST_SERVICES`, `deploy/flux-system/releases/<svc>-operator.yaml`, kind devstack wiring (a `deploy/kind/base/openstack-gateway.yaml` listener + `deploy/kind/infrastructure/<svc>-nip-io-tls-certificate.yaml` per public hostname, the `deploy/kind/base/kustomization.yaml` HelmRelease suspend patch **plus its counterparts in `hack/deploy-infra.sh`**: the flux-path un-suspend patch, the `enable_operator_servicemonitor` call, and the `hack/refresh-operator-image-digests.sh` target tuple — a service missing from the un-suspend list stays suspended on the quick-start path, its CRDs never install, and the c5c3-operator's controlplane cache never syncs), OpenBao bootstrap legs (`deploy/openbao/bootstrap/`, `deploy/openbao/policies/`) | chainsaw suites: **yes** (auto-discovered); ci.yaml wiring: **no** (4-step procedure in `hack/ci-resolve-changes.sh` header, `tests/unit/ci/change_classes_wiring_test.sh` fails on a missed step); devstack/OpenBao wiring: **no** |
| 4. ControlPlane (c5c3) | `ServicesSpec` in `operators/c5c3/api/v1alpha1/controlplane_types.go` (incl. `publicEndpoint`, `databaseCredentialsMode`, and `targetClusterRef` per-service fields), `reconcile_<svc>.go` (+ `reconcile_<svc>_dbcredentials.go` for DB services, + a `<svc>MessagingTarget` for a bus consumer), the projected `KeystoneService` registration (a `desired<Svc>Registration` builder in `builtin_registrations.go`, the `reconcileBuiltinRegistration` and `foldBuiltinRegistrationReady` calls in `reconcile_<svc>.go`, the `projectedBuiltinRegistrations` entry in `reconcile_serviceaccounts.go`, the `<svc>CatalogURL` helper in `reconcile_catalog.go`, the `catalog: true` row in `declaredServiceTargetClusters` in `controlplane_webhook.go`, and the registration delete in `deleteOrphaned<Svc>`), teardown in `reconcile_delete.go` + the placed-namespace sweep, condition/instrumentation maps, RBAC markers (the chart's `_rbac-rules.tpl` is generated from them by `make sync-helm-rbac`), scheme, webhook (incl. the per-service placement rules — `tests/e2e/c5c3/invalid-cr/` pins them — and `validate<Svc>ChildName`), envtest full chain (`integration_test.go`) + `tests/e2e/c5c3/full-controlplane-keystone/` | **no** — ~10 enumeration points |
| 5. Documentation | `docs/reference/<svc>/` (hand-written, `quadrant: operator` frontmatter), VitePress sidebar, per-service guides under `docs/guides/<svc>/`, quick-start extension (`docs/quick-start-controlplane.md`), `docs/reference/testing/<svc>-e2e-tests.md`, `tests/unit/docs/` conventions (`<svc>_crd_naming_convention_test.sh`) | **no** — no doc generator exists |

## Procedure

### 1. Profile the service

Answer these before anything else — they decide which keystone machinery
applies and which decisions need a Phase-0 spike. The mechanics behind
the database, bus, service-user and catalog answers are in
[references/controlplane-integration.md](references/controlplane-integration.md);
read [references/known-gotchas.md](references/known-gotchas.md) now too,
since its image and runtime entries become spike items. A service from
outside `opendev.org/openstack` answers
[references/non-openstack-upstream-services.md](references/non-openstack-upstream-services.md)
instead of the Python questions.

- **Database?** Which migration tool (alembic `db_sync` vs Django
  `migrate` vs none)? No DB ⇒ drop the database/db-sync/upgrade
  sub-reconcilers entirely. Which zero-downtime upgrade mechanism does
  the manage tool support (expand-migrate-contract, single-pass
  `db sync`, online data migrations), and which phases must straddle the
  workload rollout? How many schemas, and on how many SQL users? A second
  `DatabaseSpec` block, multi-schema OpenBao roles with prefix-free names
  and a per-block credential target are the #1012 recipe; a DB service
  inherits the dynamic credential chain either way.
- **db-sync side-loads?** Some services need seed data beyond the schema
  migration (glance: `db load_metadefs`) — chain the extra `*-manage` step
  into the db-sync Job with an explicit `--path`, because the config-free
  image does not ship the oslo default locations.
- **Recurring maintenance?** Which housekeeping does the service expect
  somebody to run on a schedule — above all the database cleanup every
  soft-deleting service needs? Enumerate it here, in the profile, and
  carry it into Phase 2; it is not follow-up work.
  [references/recurring-maintenance-jobs.md](references/recurring-maintenance-jobs.md)
  is the build sheet and the retro-fit bill.
- **Message bus?** The shared bus and its consumer helper exist
  (`spec.infrastructure.messaging`, `internal/common/messaging`,
  `reconcileServiceMessaging`); Neutron, Cinder and Nova consume it. Does
  every process need a live broker (cinder, nova) or does it only parse
  the URL (neutron's standalone fixtures)? A new service still has to
  answer whether it shares that bus or wants a **dedicated** one: no
  dedicated-bus slot exists on any service block. The recipe for building
  one is `### Adding a backing-service class` in
  `docs/reference/c5c3/controlplane-crd.md`.
- **Extra backing store?** (object store, message queue, cache beyond
  memcached) — a new backing service is its own pre-work issue (#653:
  Garage S3 for glance is the template — operator + declarative
  buckets/keys in `deploy/flux-system/infrastructure/`, OpenBao-seeded
  credentials materialized via ESO, kind ExternalSecrets; #978: kind-only
  NFS in `deploy/kind/nfs/` behind `WITH_NFS`), and it earns a
  **backing-store-outage chaos suite** (`glance-garage-outage`: writes
  fail closed, `/healthcheck` and `Ready` stay up; `cinder-nfs-outage`).
- **Consumes another service's API?** A service user is the `account`
  block of the projected `KeystoneService` child, in a project of its own.
  Decide the roles (`service` alone, or `service` + `admin` when it acts
  on another service's user-owned resources in admin contexts, as cinder
  and nova do) and whether a second account is needed (neutron's `neutron-nova`
  notifier).
- **Pluggable backends?** If users attach a variable number of backends
  (image stores, identity domains, volume and backup backends), model them
  as a **satellite CRD** on the extracted `internal/common/satellite`
  mechanics (#977) — shape and suites in
  [references/shared-operator-scaffold.md](references/shared-operator-scaffold.md).
  No `isDefault` when the service has no default-backend concept (Cinder,
  #979 D5).
- **Service-catalog endpoints?** If yes, they are the `catalog` block of
  the same projected child, public and internal from birth. Decide the
  service type and the API path both URLs carry. A service the
  ControlPlane will not manage skips all of
  this and registers itself: see
  `docs/guides/register-a-foreign-service.md`.
- **Config format?** oslo INI is covered by `internal/common/config`;
  anything else (Django settings, JSON) needs a renderer decision first.
  Which options does the operator own, and how is `extraConfig` validated
  (the option catalog of layer 1)?
- **Behavior breaks between the supported releases?** Check upstream
  release notes for launch-mode/WSGI/paste divergence between the pinned
  releases (glance 2025.2 eventlet vs 2026.1 uWSGI forced a
  release-switched command in the deployment builder). If yes, the
  per-release `basic-deployment-<slug>` e2e variant and the tempest legs
  must genuinely differ, and unit tests must pin the rendered config for
  **both** releases.
- **Stateful key material?** (fernet-like) — keystone's rotation machinery
  is deliberately NOT extracted; a second consumer changes that calculus.
- **Several workloads?** One CR owning several Deployments (neutron 3,
  nova 5, cinder one volume Deployment per backend) narrows every selector per component
  from birth ([references/shared-operator-scaffold.md](references/shared-operator-scaffold.md)).
- **Privileges and host access?** `restricted` usually holds (cinder kept
  it via privsep config and a one-line image patch, #979 D3); host access
  (the OVN chassis) forces a privileged namespace and kernel-dependent CI.
- **Operator-side dial-outs when placed?** Every service CR is born
  placeable on a target cluster ([references/placement-on-target-clusters.md](references/placement-on-target-clusters.md)
  — the five artefacts are scaffold, not follow-up). The profile question is
  what *breaks* when the children live elsewhere: operator dial-outs to a
  cluster-local endpoint, HTTP health probes, optional child kinds.
- **Depends on other services?** Determines the gating condition in the
  c5c3 sub-reconciler chain (e.g. Horizon gates on `KeystoneReady`), and
  a hard runtime dependency becomes a ControlPlane admission rule
  (`validateNovaDependencies`) and a heavier e2e substrate.
- **Agents on other clusters?** Keep the meta to the control plane,
  publish a contract (Nova's `{nova}-compute-config` Secret), run CI on a
  fake driver, and file the data plane as a follow-on meta (#1013).
- **Tempest plugin maintained upstream?** If not (e.g. horizon), plan
  HTTP-level chainsaw assertions instead and say so explicitly.
- **Ingress?** `commonv1.GatewaySpec` / HTTPRoute via
  `internal/common/gateway`, plus the full public surface in
  [references/controlplane-integration.md](references/controlplane-integration.md) § Public surface.

### 2. Run the deterministic inventory

```bash
bash .claude/skills/prepare-new-service/scripts/inventory-touchpoints.sh <service>
```

It prints `[DONE]`/`[TODO]` per touch point across the five layers plus
gotcha warnings (e.g. the service already pinned in `upper-constraints.txt`).
It is an inventory, not a gate — for a fresh service everything is `[TODO]`;
its real value is catching **partial** onboarding and stale enumerations
when re-run mid-effort. Checks marked "skip if …" are legitimately `[TODO]`
for a service the profile rules them out for.

### 3. Verify the reference paths still hold

The repo evolves — do not trust this skill's tables blindly. Spot-check
that the enumeration points named above still exist at HEAD (grep for
`ALL_OPERATORS`, `subConditionTypes`, `OPERATORS ?=`, `ServicesSpec`,
`desiredGlanceRegistration`, `SERVICE_TENANTS`), and
skim the per-layer "Adding a New Service" docs, which are authoritative
for layer 1, 2, and 3 details:

- `docs/contributing/adding-a-new-operator.md` (layer 2 — the documented
  onboarding path over `internal/common`, incl. "A second database block")
- `docs/reference/ci-cd/build-images-workflow.md` § Adding a New Service
- `docs/reference/ci-cd/container-images.md` (release config files)
- `docs/reference/testing/tempest-test-infrastructure.md` § Adding a New Service
- `docs/reference/infrastructure/infrastructure-manifests.md` § Extensibility

### 4. Generalization pre-check (before drafting the meta)

Ask: **what would the new operator copy-paste from `operators/keystone`
(or from the closest sibling) a second (or third) time?** Classify
keystone internals into:

1. thin wrappers over `internal/common` — copy as pattern, fine;
2. generic logic living in keystone (pipeline/status machinery, watch
   mappers, webhook validators) — **extraction candidates**;
3. genuinely keystone-specific (fernet, bootstrap, trust-flush) — leave
   alone, rule of three.

Read [references/shared-operator-scaffold.md](references/shared-operator-scaffold.md)
first: pod-template and Service
assembly, scheme wiring, the `ValidateDelete` shim, the controller
skeleton, the instrumentation glue, the satellite mechanics, the bus
helper and the multi-database flow have already been extracted, so they are
no longer candidates — they are consumption, and a copy of the
pre-extraction keystone shapes reintroduces boilerplate the repo has
deleted.

If category 2 is non-empty, file (or update) a **separate refactor issue**
listing the candidates with file:line references, S/M/L effort, and a
must-before / opportunistic split — then mark the meta **blocked on it**.
#551 is the template (#655 is the second round: DB orchestration +
`keystone_authtoken` renderer for glance; #757 the third: the shared
workload builder plus the boilerplate collapse; #977 the satellite
mechanics once three copies existed; #1012 the multi-database flow ahead
of its only consumer); check first whether it
(or a successor) is still open and simply needs extending. Also check open
API-shape issues (e.g. #471) — a new CRD must be born with the target
shape, not the legacy one.

Extract **with** a consumer, never ahead of one. #757 deliberately left
out both API bits its own issue sketched — `ContainerParams.EnvFrom` and
a `webhook.Setup[T]` wrapper — because no existing operator sets either;
the fourth operator adds `EnvFrom` when its env contract needs it. A
field no caller sets is untested surface, and the same calculus applies
to whatever the new service seems to ask for (`internal/common/messaging`
was cut from #935 for exactly that reason and built with Neutron).

### 5. Draft the meta issue

Follow the house format (#552, #550, #481; #979 and #1014 are the current
shape): `Meta:` title prefix,
Background, phases with checkbox scope, explicit blocking relations,
Out of scope, italic footer with date + `main` SHA + relations.
Standard phase skeleton (drop/merge phases the profile rules out):

- **Phase 0 — decisions (spike):** session/config/secret-sourcing choices,
  upper-constraints handling, WSGI/launch mode per release, endpoint and
  catalog-interface wiring, backend-CRD shape if the profile calls for one,
  the recurring-maintenance inventory with its cadence and retention
  defaults (record the decisions in the meta as #656 records D1–D10), the
  security posture, the bus and CPU budget. Write each as a recommendation
  with its evidence; the spike sub-issue proves the runtime ones on a
  throw-away kind cluster and `/planwerk:decide` settles the rest.
- **Phase 1 — container image** (usually independent of pre-work).
- **Phase 2 — service operator scaffold** (blocked on generalization) —
  built on the shared forms of [references/shared-operator-scaffold.md](references/shared-operator-scaffold.md),
  with the rendered Deployment and Service pinned in the same checkbox that adds
  them; one checkbox per recurring-maintenance task from the profile,
  which is part of the scaffold, not a follow-up; the five placement artefacts
  are scaffold too — [[check-service-parity]] P12 flags a service that
  lands without them. The ci.yaml operator lists (`ALL_OPERATORS` and
  `SERVICE_OPERATORS` in one edit), the invalid-CR corpora and the
  CRD/reconciler/events pages land here too: without them the module is
  untested and its e2e leg fails on a missing directory (PRs #942, #993, #1026).
- **Phase 3 — CI, e2e, deploy stack** (blocked on Phase 2 in practice,
  #905←#904) — including the kind Gateway listener/cert, OpenBao bootstrap
  legs, the `image_`/`tests_e2e_`/`tempest_<svc>` filters, and chaos suites.
- **Phase 4 — ControlPlane integration** (blocked on Phase 2) — including
  the projected `KeystoneService` registration: a
  `desired<Svc>Registration` builder in `builtin_registrations.go`, the
  `reconcileBuiltinRegistration` and `foldBuiltinRegistrationReady` calls
  in the service leg, the `projectedBuiltinRegistrations` entry, the
  consumer-Secret glue, and the orphan delete; plus envtest
  (`integration_test.go`) and the registrations step of
  `tests/e2e/c5c3/full-controlplane-keystone/`.
- **Phase 5 — documentation** (continuous, gates each phase) — reference
  set, per-service guides, and the quick-start extension.

Rules that keep it splittable:

- one checkbox = one sub-issue = one PR (Phase 0 may be a single spike);
- every checkbox names concrete files/paths, not intentions;
- include an ordering diagram when phases overlap;
- recommendations are stated as recommendations ("recommended: no DB
  sessions"), so the sub-issue can overturn them cheaply;
- the body stays under GitHub's 65,536-character limit: write it to the
  scratchpad, measure with `wc -m`, target 63 KB, and move Follow-ups /
  Out of scope into a continuation comment when it does not fit (#1014).

Create the issue with `gh issue create --label enhancement`, then
cross-link the pre-work issue's footer (`blocks #<meta>`).

### 6. Cross-check

Before publishing, sanity-check the claims that rot fastest:

- file:line references — re-grep each one at HEAD;
- "already pinned in upper-constraints" — `grep '^<svc>===' releases/*/upper-constraints.txt`;
- open-issue relations — `gh issue list --state open` for overlaps, so the
  meta references instead of duplicates;
- the budgets — node capacity and leg walls from a recent CI run, not from
  an older meta ([references/known-gotchas.md](references/known-gotchas.md) § Layer 3).

Related skills for the implementation phase (mention them in the meta so
sub-issues use them as gates): [[check-crd-drift]], [[check-fixture-drift]],
[[check-condition-coverage]], [[check-validation-parity]],
[[check-doc-drift]], [[check-renovate-coverage]],
[[check-go-workspace-deps]], [[check-spdx-reuse]], and
[[check-service-parity]] as the closing cross-layer gate once the
onboarding lands (#656 used it exactly that way); [[add-validation-rule]],
[[add-image-patch]] and [[debug-e2e-failure]] are the matching runbooks.

## Notes

- This skill is read-only with respect to the codebase; its outputs are
  GitHub issues (and this analysis). Implementation belongs to the
  sub-issues.
- If the user only wants the analysis, deliver the phase plan as text and
  skip issue creation — but still report what the inventory script found.
