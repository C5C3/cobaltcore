---
name: check-service-parity
description: >-
  Audit whether every onboarded OpenStack service stays in structural
  lockstep with the keystone reference implementation across the five
  onboarding layers — container image under images/<svc>/, service
  operator under operators/<svc>/, CI/e2e/deploy wiring, ControlPlane
  integration in operators/c5c3/, and the documentation set under
  docs/. Use when asked to check service parity, after merging or
  while reviewing a service-onboarding PR, or when a second (or later)
  service starts drifting from the scaffolding conventions keystone
  defines.
---

# Check service parity

This skill verifies that every onboarded service **mirrors the keystone
reference across all five onboarding layers**: the same image contract,
the same operator scaffolding, the same CI enumeration points, the same
e2e/chaos/deploy coverage, the same docs set, and the same ControlPlane
projection. With more than one service in the tree, divergence between
services is the dominant drift risk — a layer that auto-discovers new
services stays silent when one is missing, and CI cannot flag a job it
was never told to run.

It is repeatable — run it any time, especially while reviewing a
service-onboarding PR (they are typically too large for review bots),
after merging one, or before tagging a release.

## What service parity means here

A service is the unit the repo onboards end-to-end (keystone, horizon,
…) — discovered as the keys of `releases/<latest>/source-refs.yaml`.
`c5c3` is the ControlPlane operator, not a service. Each service
threads through five layers:

| Layer | Where it lives | Source of truth |
|---|---|---|
| Container image | `images/<svc>/Dockerfile`, `tests/container-images/verify_<svc>.sh`, `releases/*/`(source-refs, extra-packages, test-excludes) | the keystone image contract and `verify_release_config.sh` |
| Service operator | `operators/<svc>/` (module, CRD, webhook, helm chart, dashboards) | the keystone operator scaffolding on `internal/common` |
| CI / e2e / deploy | `.github/workflows/*.yaml`, `hack/ci-resolve-changes.sh` env, `tests/e2e/<svc>/`, `tests/e2e-chaos/`, `deploy/flux-system/` | the enumeration points and canonical suite set keystone populates |
| ControlPlane integration | `operators/c5c3/api/` `ServicesSpec`, `internal/controller/reconcile_<svc>.go`, c5c3 chart RBAC | the keystone projection (`reconcile_keystone.go`, `KeystoneReady`) |
| Documentation | `docs/reference/<svc>/`, `docs/guides/enable-<svc>-operator-*.md`, `docs/.vitepress/config.ts` | the keystone reference/guide set |

There is no single authoritative gate for parity — that is the point of
this skill. CI runs exactly the matrices it enumerates, so a service
missing from an enumeration point does not fail CI; it silently never
runs. The per-layer gates (`make verify-crd-sync`, `make
verify-helm-schema`, `make chainsaw-lint`,
`tests/container-images/verify_release_config.sh`) each guard their own
layer once the service is wired in; this skill adds the cross-layer
inventory none of them can express: "is every service wired into every
layer keystone is wired into?"

A parity finding is any layer artefact the keystone reference carries
that another service lacks (or vice versa) without a recorded
deviation.

## Procedure

Work through these steps in order and report findings at the end.

### 1. Run the deterministic audit

```bash
bash .claude/skills/check-service-parity/scripts/audit-service-parity.sh
```

The script catches the mechanically-checkable gaps and prints an
inventory. Exit code `1` means at least one `[FAIL]`. Interpret:

- **P1** — image layer: `images/<svc>/Dockerfile`, the
  `verify_<svc>.sh` contract script, and the per-release config
  (source-refs key, extra-packages block, test-excludes file) in
  *every* release. A missing release entry means the build matrix —
  which auto-discovers from these files — silently skips the
  service×release combination.
- **P2** — operator module: `operators/<svc>/go.mod`, the `go.work`
  use entry, the Makefile `OPERATORS ?=` default, and the
  `operators/Dockerfile` module-manifest COPY line. A miss here means
  `make lint`/`make test` never visit the module, or the operator
  image build fails at the COPY step.
- **P3** — helm chart: `crds/` copy, generated `values.schema.json`,
  and the helm-unittest suite set at parity with the keystone chart.
  A missing suite is an untested template that `helm-validate` renders
  but never asserts on.
- **P4** — observability: the Grafana dashboard JSON plus its
  `dashboard_test.go` drift test, which pins every referenced metric
  to a registered one.
- **P5** — CI wiring: the paths-filter block, `ALL_OPERATORS`,
  `FILTER_<svc>`, the unit/integration test matrices, the
  helm-validate chart loop, and the build/cleanup image matrices.
  These are the enumeration points `hack/ci-resolve-changes.sh`
  cannot reach on its own — a missing entry means the service's tests
  never run and CI stays green.
- **P6** — e2e coverage: the canonical chainsaw suite set
  (basic-deployment, scale, healthcheck, httproute, network-policy,
  deletion-cleanup, pod-security-restricted, invalid-cr), the
  latest-release `basic-deployment-<slug>` variant, and at least one
  chaos suite. Keystone's chaos suites predate multi-service naming
  and are unprefixed; later services prefix theirs (`<svc>-*`).
- **P7** — deploy stack: the FluxCD HelmRelease under
  `deploy/flux-system/releases/`, the `<svc>-system` namespace, and
  the kustomization resource entry.
- **P8** — documentation: `docs/reference/<svc>/` (index, CRD,
  reconciler), the metrics and networkpolicy guides, and the
  vitepress nav entry.
- **P9** — ControlPlane integration: the `services.<svc>` field on
  `ServicesSpec`, the `reconcile_<svc>.go` projection, the
  `<Svc>Ready` condition mirror, and the RBAC group in the c5c3
  chart helpers.
- The **inventory** lists, per service, the helm-unittest, e2e, and
  chaos suite counts. Cross-reference outliers by hand in step 2.

### 2. Cross-reference by hand

The script checks presence, not content. Using the inventory, confirm:

1. For each non-reference service, that its sub-reconciler chain
   builds on the shared scaffolding in `internal/common` rather than
   re-implementing keystone code (grep for copy-pasted helpers that
   should have been generalized first — the rule-of-three from
   [[prepare-new-service]]).
2. That deliberate thin-profile gaps (a service without a database has
   no db-sync/fernet machinery, hence fewer suites) are recorded in
   the `ALLOWED_DEVIATIONS` list inside the audit script — an
   unrecorded gap and a forgotten layer look identical to the script.
3. That the per-service condition types are registered in that
   operator's instrumentation map (hand off to
   [[check-condition-coverage]]).
4. That suite *content* tracks the reference where it applies — e.g. a
   new assertion added to keystone's `network-policy` suite usually
   has an analogue in every other service's suite.

### 3. Run the per-layer authoritative gates

The script does not render, build, or deploy anything. Run the real
per-layer gates and report the exact outcomes:

```bash
bash tests/container-images/verify_release_config.sh   # release config layer
make verify-crd-sync                                   # CRD layer
make verify-helm-schema                                # helm chart layer
make chainsaw-lint                                     # e2e suite layer
```

Trust these over the P1–P9 smoke checks for the layers they cover;
the smoke checks exist for the cross-layer absences these gates cannot
see.

### 4. Report

Produce a concise summary grouped by severity:

- **HIGH** — a service missing from a CI enumeration point (P5) or
  from a release config file (P1): its pipeline coverage silently
  does not run; a missing ControlPlane projection artefact (P9) for a
  service the ControlPlane models.
- **MEDIUM** — a missing canonical e2e suite, chaos suite, or
  helm-unittest suite without an `ALLOWED_DEVIATIONS` entry; a
  missing dashboard drift test; a missing deploy-stack entry.
- **LOW** — a missing docs page or nav entry; an inventory outlier
  (suite counts far apart) that turns out to be a recorded deviation.

For each finding give one line with a `file:line` (or path) reference
for both the keystone reference side and the lagging service side. End
with a two- to three-sentence parity verdict per service.

## Drift patterns

These recurring shapes are worth grepping for first:

1. **The invisible CI gap.** A service operator exists and builds
   locally, but `ALL_OPERATORS` or a `target: [...]` matrix was never
   extended. Every PR stays green because the missing job never runs —
   the failure mode CI cannot self-report.
2. **Reference moved, followers did not.** A new suite, helm test, or
   guide lands for keystone (the reference) and no analogue lands for
   the other services. Parity drift accumulates one keystone
   improvement at a time.
3. **Unrecorded thin-profile gap.** A service legitimately lacks a
   layer artefact (no database → no brownfield suite) but the
   deviation was never added to `ALLOWED_DEVIATIONS` — the next audit
   cries wolf, and real gaps hide behind the noise.
4. **Copy-paste instead of generalize.** A second service vendored a
   keystone helper rather than promoting it to `internal/common`
   first; both copies now evolve independently. The onboarding
   pre-work rule in [[prepare-new-service]] exists to prevent exactly
   this.
5. **Release config added for one release only.** The service key
   landed in the newest `releases/<version>/source-refs.yaml` but not
   the older ones (or vice versa), so the build matrix builds the
   service for half the supported releases.

## Notes

- This skill is read-only; the deterministic script edits nothing.
  Apply fixes (wire the missing enumeration point, add the missing
  suite, record the deviation) as a separate, explicitly-scoped task.
- Keystone is the canon: the canonical e2e suite set and the reference
  helm-unittest set are derived live from the keystone tree, so a
  keystone regression surfaces as every other service suddenly
  "leading" the reference. Read multi-service failures with that in
  mind.
- `ALLOWED_DEVIATIONS` (top of the audit script) is the single place
  deliberate deviations live, one `<svc>:<check>:<item>` token per
  line. Record the *why* in a comment next to the token.
- This skill is the repeatable audit counterpart to
  [[prepare-new-service]], which plans an onboarding before it starts.
  Pair it with [[check-crd-drift]], [[check-condition-coverage]],
  [[check-fixture-drift]], and [[check-doc-drift]] — those audit each
  layer in depth; this skill audits that every service is present in
  every layer at all.
