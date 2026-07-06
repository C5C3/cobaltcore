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

Worked example: Horizon — meta issue #552, generalization pre-work #551.
Read both before drafting a new one; they are the calibration for scope,
tone, and checkbox granularity.

## The five layers

Every service in forge threads through five layers. Keystone
(`operators/keystone`) is the reference implementation for all of them.

| Layer | Canonical locations | Auto-extends? |
|---|---|---|
| 1. Container image | `images/<svc>/Dockerfile`, `releases/*/source-refs.yaml`, `releases/*/extra-packages.yaml`, `tests/container-images/verify_<svc>.sh` | build/test matrix: **yes** (from source-refs keys); hadolint matrix in `build-images.yaml`: **no** |
| 2. Service operator | `operators/<svc>/` (api, controller, webhook, helm chart on `operators/shared/helm/operator-library`), `go.work`, `Makefile` `OPERATORS` | **no** — module + enumerations by hand |
| 3. CI / e2e / deploy | `ci.yaml` paths-filter + `ALL_OPERATORS` + matrices, `tests/e2e/<svc>/`, `tests/e2e/<svc>-operator/`, `tests/e2e-chaos/`, `tests/tempest/<svc>-*/`, `deploy/flux-system/releases/<svc>-operator.yaml` | chainsaw suites: **yes** (auto-discovered); ci.yaml wiring: **no** (3-step procedure in `hack/ci-resolve-changes.sh` header) |
| 4. ControlPlane (c5c3) | `ServicesSpec` in `operators/c5c3/api/v1alpha1/controlplane_types.go`, `reconcile_<svc>.go`, condition/instrumentation maps, RBAC markers + helm `_helpers.tpl`, scheme, webhook | **no** — ~10 enumeration points |
| 5. Documentation | `docs/reference/<svc>/` (hand-written, `quadrant: operator` frontmatter), VitePress sidebar, guides, `tests/unit/docs/` conventions; `architecture/` **submodule** (separate repo C5C3/C5C3) | **no** — no doc generator exists |

## Procedure

### 1. Profile the service

Answer these before anything else — they decide which keystone machinery
applies and which decisions need a Phase-0 spike:

- **Database?** Which migration tool (alembic `db_sync` vs Django
  `migrate` vs none)? No DB ⇒ drop the database/db-sync/upgrade
  sub-reconcilers entirely.
- **Message bus?** No shared RabbitMQ spec or backing service exists yet —
  the **first** RabbitMQ consumer must add a `commonv1` messaging type and
  extend `InfrastructureSpec` + `hack/deploy-infra.sh`. That is its own
  pre-work issue.
- **Service-catalog endpoints?** If yes, mirror the c5c3 catalog
  reconciler pattern (K-ORC `Service` + `Endpoint`).
- **Config format?** oslo INI is covered by `internal/common/config`;
  anything else (Django settings, JSON) needs a renderer decision first.
- **Stateful key material?** (fernet-like) — keystone's rotation machinery
  is deliberately NOT extracted; a second consumer changes that calculus.
- **Depends on other services?** Determines the gating condition in the
  c5c3 sub-reconciler chain (e.g. Horizon gates on `KeystoneReady`).
- **Tempest plugin maintained upstream?** If not (e.g. horizon), plan
  HTTP-level chainsaw assertions instead and say so explicitly.
- **Ingress?** `commonv1.GatewaySpec` / HTTPRoute.

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
for layer 1 and 3 details:

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
#551 is the template; check first whether it (or a successor) is still
open and simply needs extending. Also check open API-shape issues
(e.g. #471) — a new CRD must be born with the target shape, not the
legacy one.

### 5. Draft the meta issue

Follow the house format (#552, #550, #481): `Meta:` title prefix,
Background, phases with checkbox scope, explicit blocking relations,
Out of scope, italic footer with date + `main` SHA + relations.
Standard phase skeleton (drop/merge phases the profile rules out):

- **Phase 0 — decisions (spike):** session/config/secret-sourcing choices,
  upper-constraints handling, WSGI server, endpoint wiring.
- **Phase 1 — container image** (usually independent of pre-work).
- **Phase 2 — service operator scaffold** (blocked on generalization).
- **Phase 3 — CI, e2e, deploy stack** (alongside Phase 2).
- **Phase 4 — ControlPlane integration** (blocked on Phase 2).
- **Phase 5 — documentation** (continuous, gates each phase).

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
[[check-condition-coverage]], [[check-doc-drift]], [[check-renovate-coverage]],
[[check-go-workspace-deps]], [[check-spdx-reuse]].

## Known gotchas (verified 2026-07, re-verify at HEAD)

- **upper-constraints pin conflict:** some services (horizon, most
  clients' dashboards/libraries) are already pinned in
  `releases/*/upper-constraints.txt`. Installing from source with
  `--constraint` then requires the source ref to match the pin exactly,
  or a `-<svc>` line in `overrides/<release>/constraints.txt`
  (`scripts/apply-constraint-overrides.sh`). Keystone never hit this.
- **hadolint matrix is static** in `build-images.yaml` — new Dockerfiles
  must be added by hand even though the build matrix auto-discovers.
- **`tests/container-images/verify_release_config.sh` and
  `verify_deviation_comments.sh` are keystone-hardcoded** — extend or
  generalize them, or the new service's release config goes unvalidated.
- **Operator Dockerfiles are coupled to `go.work`:** each copies every
  module's go.mod, so adding a module edits all existing operator
  Dockerfiles (until a parameterized `ARG OPERATOR` Dockerfile lands).
- **`architecture/` is a git submodule** of a separate repo — architecture
  chapters (esp. `docs/09-implementation/`) are planned there, not here,
  and need `git submodule update --init architecture` to even read.
- **Tempest matrix naming** (`hack/ci-generate-tempest-matrix.sh`) and the
  chaos CI job's image list are keystone-specific today.
- **WSGI entry points:** `uv pip install --prefix` skips PBR
  `wsgi_scripts` generation — service Dockerfiles hand-write their WSGI
  launcher (see `images/keystone/Dockerfile`).

## Notes

- This skill is read-only with respect to the codebase; its outputs are
  GitHub issues (and this analysis). Implementation belongs to the
  sub-issues.
- If the user only wants the analysis, deliver the phase plan as text and
  skip issue creation — but still report what the inventory script found.
