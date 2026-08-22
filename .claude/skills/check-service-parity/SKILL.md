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
| CI / e2e / deploy | `.github/workflows/*.yaml`, `hack/ci-resolve-changes.sh` env, `tests/e2e/<svc>/`, `tests/e2e-chaos/`, `tests/e2e-multicluster/` (when the service joins the placed-services suite: fixtures **and** the hard-coded image list in the ci.yaml `e2e-multicluster` job), `deploy/flux-system/`, the per-service lists in `hack/deploy-infra.sh` + `hack/refresh-operator-image-digests.sh` | the enumeration points and canonical suite set keystone populates |
| ControlPlane integration | `operators/c5c3/api/` `ServicesSpec`, `internal/controller/reconcile_<svc>.go`, c5c3 chart RBAC | the keystone projection (`reconcile_keystone.go`, `KeystoneReady`) |
| Documentation | `docs/reference/<svc>/`, `docs/guides/<svc>/enable-<svc>-operator-*.md`, `docs/.vitepress/config.ts` | the keystone reference/guide set |

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
  helm-validate chart loop, the build-images coverage, and both GHCR
  cleanup package lists (`cleanup-packages` and `cleanup-e2e-packages`,
  each needing `<svc>-operator` and `<svc>`), with exact list-item
  matches so a bare service name cannot satisfy a check by matching
  its `<svc>-operator` sibling. The cleanup lists come from
  `hack/ci-generate-cleanup-matrix.sh`, which derives them from
  `images/` and `operators/`, so a miss there means a missing
  directory rather than a forgotten matrix entry.
  These are the enumeration points `hack/ci-resolve-changes.sh`
  cannot reach on its own — a missing entry means the service's tests
  never run and CI stays green.
- **P6** — e2e coverage: the canonical chainsaw suite set
  (basic-deployment, scale, healthcheck, httproute, network-policy,
  deletion-cleanup, pod-security-restricted, invalid-cr,
  gateway-quick-start-smoke), the
  latest-release `basic-deployment-<slug>` variant, and at least one
  chaos suite. Keystone's chaos suites predate multi-service naming
  and are unprefixed; later services prefix theirs (`<svc>-*`).
- **P7** — deploy stack: the FluxCD HelmRelease under
  `deploy/flux-system/releases/`, the `<svc>-system` namespace, the
  kustomization resource entry, and the three literal per-service
  lists in the deploy helpers: the `hack/deploy-infra.sh` un-suspend
  patch on the flux ControlPlane path (the kind overlay suspends every
  self-built operator HelmRelease; a service missing from the
  un-suspend list stays suspended forever, its CRDs never install, and
  the c5c3-operator's controlplane cache never syncs — the glance
  gap), the `hack/refresh-operator-image-digests.sh` target tuple
  (without it the `:latest` image is never digest-pinned), and the
  `enable_operator_servicemonitor` call site (WITH_PROMETHEUS).
- **P8** — documentation: `docs/reference/<svc>/` (index, CRD,
  reconciler), the metrics and networkpolicy guides, and the
  vitepress nav entry.
- **P9** — ControlPlane integration: the `services.<svc>` field on
  `ServicesSpec`, the `reconcile_<svc>.go` projection, the
  `<Svc>Ready` condition mirror, and the RBAC group in the c5c3
  chart helpers.
- **P10** — recurring maintenance: a database-backed operator projects
  at least one housekeeping CronJob (`job.EnsureCronJob` — glance's
  `db purge`, keystone's `trust_flush`). Nothing else in the cluster
  runs the periodic cleanup a package deployment gets from a cron entry
  on the controller node, and the omission is invisible: the deployment
  stays Ready while the soft-deleted backlog grows. Services that model
  no database are skipped rather than allowlisted. Presence is a smoke
  signal — whether the CronJobs cover the tasks the service actually
  needs is step 2 below.
- **P11** — API endpoint isolation: the API Service selector narrows by
  `app.kubernetes.io/component` (`naming.APISelectorLabels`), pod
  templates are labelled per component (`naming.ComponentLabels`), and
  the API PodDisruptionBudget keys on the absence of
  `batch.kubernetes.io/job-name` (`naming.ExcludeJobPods`) instead of
  the component label. A maintenance pod template with bare
  `commonLabels` satisfies a name+instance Service selector and becomes
  an API endpoint with nothing listening on the API port — the numeric
  `targetPort` admits it, the missing readiness probe makes it ready
  from its first moment, and in-cluster API traffic answers
  `ECONNREFUSED` for its whole runtime (#778, fixed in #785).
- **P12** — target-cluster placement: since the multicluster
  conversion every service CR is born placeable, and the five artefacts
  travel together — `spec.targetClusterRef` (the shared
  `commonv1.TargetClusterRefSpec`), the webhook immutability mirror
  (`validation.TargetClusterRefImmutable`), a `targetclusterref`
  invalid-cr rejection fixture, the multicluster builder
  (`mcbuilder.ControllerManagedBy` with remote child watches), and the
  remote-children deletion sweep
  (`commonmulticluster.SweepRemoteChildren` behind
  `RemoteChildrenFinalizer`). A service missing any of them cannot be
  placed, or strands label-owned remote children on delete — no
  cross-cluster ownerReference exists to cascade for it.
- The **inventory** lists, per service, the helm-unittest, e2e, and
  chaos suite counts, plus whether the service appears in the
  two-cluster placed-services suite (`tests/e2e-multicluster/` —
  keystone and barbican today; membership is a coverage decision, not
  a P12 failure). Cross-reference outliers by hand in step 2.

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
5. That the housekeeping P10 found (or did not find) matches what the
   service actually needs: compare the projected CronJobs against the
   `<svc>-manage` subcommands upstream documents for periodic use
   (`db purge`, `archive_deleted_rows`, `purge_deleted`, cache pruning,
   expiry sweeps — the table in [[prepare-new-service]] § Recurring
   maintenance jobs). A service passing P10 with a key task still
   unscheduled is the same finding as one failing it. Check the shape
   too: `ConcurrencyPolicy: Forbid`, a per-run row cap,
   `ActiveDeadlineSeconds`, and a condition that reports the newest
   terminal run instead of merely confirming the CronJob exists.
6. That every Job/CronJob pod template actually sets a non-`api`
   component value (grep the builders for `componentLabels(` /
   `naming.ComponentLabels`). P11 only proves the helpers are
   referenced somewhere in the package — a single new maintenance
   workload added with bare `commonLabels` still passes it and
   reintroduces the #778 endpoint pollution. Keystone's
   `maintenance-endpoint-isolation` suite pins the isolation
   end-to-end; a service with maintenance CronJobs and no equivalent
   assertion is a candidate for pattern 2 below.
7. That the placement wiring P12 smoke-checks actually routes every
   child write through the resolved children client (grep for direct
   `r.Client.Create`/`Patch` on child kinds that bypass
   `ResolveChildrenClient`), and that a service dialing cluster-local
   endpoints from the operator (OpenBao, DB admin connections) does so
   through the tunnel/proxy seam when placed — barbican's
   port-forward-tunneled OpenBao dials are the worked example. Decide
   whether the service should join the placed-services suite
   (`tests/e2e-multicluster/`) or record why keystone + barbican
   coverage suffices; the ci.yaml `e2e-multicluster` job hard-codes
   the image list it preloads, so suite membership means extending
   that job too.

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
  missing dashboard drift test; a missing deploy-stack entry; a
  database-backed service with no housekeeping CronJob (P10), or one
  whose CronJobs leave a documented periodic task unscheduled — name
  the task and the growth it leaves unbounded.
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
5. **Housekeeping deferred to "later".** The service onboards with a
   database and a deployment but no periodic cleanup, because nothing
   in the onboarding fails without it. Glance ran that way until
   `db purge` was retro-fitted, which then cost an API block, an
   admission bound on `metadata.name` in two operators, and a
   hash-collapse fallback for the CRs admitted before that bound —
   [[prepare-new-service]] § Recurring maintenance jobs exists to keep
   the next service from repeating it.
6. **Release config added for one release only.** The service key
   landed in the newest `releases/<version>/source-refs.yaml` but not
   the older ones (or vice versa), so the build matrix builds the
   service for half the supported releases.
7. **Placement artefacts landing piecemeal.** The five P12 artefacts
   are one contract: a `targetClusterRef` spec field without the
   webhook mirror admits a mutable placement (the children strand on
   the old cluster), and a multicluster builder without the
   remote-children sweep leaks every placed child on delete — remote
   children are label-owned, so nothing cascades for them. The five
   land in one arc for a reason; a follower service picking up only
   the spec field is the drift to catch.
8. **Maintenance pod template with bare `commonLabels`.** A new
   Job/CronJob builder copies the workload's label set instead of
   `naming.ComponentLabels` with its own component value, so its pods
   satisfy the API Service selector again — the #778 shape P11 exists
   for. The failure is invisible in every gate that renders or asserts
   on the CronJob itself; only an EndpointSlice-level assertion (the
   `maintenance-endpoint-isolation` suite) catches it.

## Notes

- This skill is read-only; the deterministic script edits nothing.
  Apply fixes (wire the missing enumeration point, add the missing
  suite, record the deviation) as a separate, explicitly-scoped task.
- Keystone is the canon: the canonical e2e suite set and the reference
  helm-unittest set are derived live from the keystone tree, so a
  keystone regression surfaces as every other service suddenly
  "leading" the reference. Read multi-service failures with that in
  mind. The canon has known holes, though — a later service can pioneer
  a surface keystone lacks (glance: the dual public+internal catalog
  row, the satellite backend CRD, the backing-store-outage chaos
  suite). A pioneered surface is a candidate for back-porting or for a
  recorded deviation, not automatically a finding against the pioneer.
- `ALLOWED_DEVIATIONS` (top of the audit script) is the single place
  deliberate deviations live, one `<svc>:<check>:<item>` token per
  line. Record the *why* in a comment next to the token.
- This skill is the repeatable audit counterpart to
  [[prepare-new-service]], which plans an onboarding before it starts.
  Pair it with [[check-crd-drift]], [[check-condition-coverage]],
  [[check-fixture-drift]], and [[check-doc-drift]] — those audit each
  layer in depth; this skill audits that every service is present in
  every layer at all.
