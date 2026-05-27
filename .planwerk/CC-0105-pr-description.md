# CC-0105 — Deploy each service in its own dedicated Namespace

Closes the CC-0105 feature: split the `keystone-operator` controller off
into its own dedicated `keystone-system` Namespace, and retire the
unused `monitoring-system` Namespace by consolidating monitoring
HelmReleases onto the live `monitoring` Namespace.

## Summary

Before CC-0105, the `keystone-operator` HelmRelease, the
`keystone-operator` controller Deployment, and every Keystone workload
(custom resources, rendered Deployment/Service/Secrets/HTTPRoute) all
co-tenanted the `openstack` Namespace. That collapsed two distinct
operational concerns — the platform-owned controller and the
tenant-owned workload — onto a single Namespace, so every Keystone
NetworkPolicy, RBAC binding, and PSS label had to accommodate both
roles at once. It also left a dead `monitoring-system` Namespace in
the production overlay even though the live monitoring stack runs in
`monitoring`.

This PR moves the controller to a dedicated `keystone-system`
Namespace (controller-only; the Keystone CR and rendered workload
stay in `openstack`), retires `monitoring-system` in favour of
`monitoring`, retargets all CI / e2e / chaos / docs lookups for the
operator pod accordingly, and adds regression-guard unit tests so the
controller-vs-workload split cannot silently regress.

Changed surfaces:

- **Production overlay** (`deploy/flux-system/`):
  - `namespaces.yaml` — added `keystone-system` and `monitoring`
    Namespace resources; removed `monitoring-system`.
  - `releases/keystone-operator.yaml` — `metadata.namespace` changed
    from `openstack` to `keystone-system`; `dependsOn` entries
    unchanged.
  - `releases/prometheus-operator-crds.yaml` —
    `metadata.namespace` changed from `monitoring-system` to
    `monitoring`.
  - `releases/memcached-operator.yaml` — `dependsOn[prometheus-operator-crds].namespace`
    changed from `monitoring-system` to `monitoring`; controller
    Namespace unchanged.
- **Kind overlay** (`deploy/kind/base/kustomization.yaml`): suspend
  patch retargeted to the new operator Namespace.
- **Chart tests** (`operators/keystone/helm/keystone-operator/tests/`):
  added `release_namespace_test.yaml` asserting every namespaced
  resource lands in the release Namespace.
- **CI scripts**: `hack/ci-deploy-operator.sh` now defaults
  `NAMESPACE=keystone-system` and passes `--namespace
  keystone-system --create-namespace` to `helm install` (CC-0105,
  REQ-011).
- **E2E tests** (`tests/e2e/keystone/`): all operator-pod lookups
  (`-n openstack -l app.kubernetes.io/name=keystone-operator …`)
  retargeted to `-n keystone-system`. Keystone-CR / workload lookups
  retained on `-n openstack`.
- **Chaos tests** (`tests/e2e-chaos/operator-pod-{crash,kill}/`):
  PodChaos selector and `kubectl` commands retargeted from `default`
  to `keystone-system`. The pre-CC-0105 comments stating "operator
  runs in `default` because `helm install` is invoked without
  `--namespace`" were updated to reflect the new
  `--namespace keystone-system --create-namespace` invocation.
- **Docs** (`docs/`, `architecture/docs/05-deployment/`): every
  operator-pod example retargeted; new `04-namespace-policy.md` plus
  HelmRelease migration runbook documenting the
  suspend → uninstall → annotation-cleanup → reinstall sequence.
- **Unit tests** (`tests/unit/`): four new regression guards
  (`deploy/namespaces_test.sh`, `deploy/helmrelease_namespaces_test.sh`,
  `deploy/kind_overlay_test.sh`, `hack/ci_deploy_operator_namespace_test.sh`,
  `docs/namespace_consistency_test.sh`,
  `architecture/namespace_policy_doc_test.sh`).
- **Integration tests** (`tests/integration/`): two new gates
  (`deploy/kustomize_render_test.sh`, `ci/full_kind_up_test.sh`)
  fail closed if a future change reintroduces `monitoring-system` to
  the rendered overlay or redeploys the operator into `openstack`.

## Expected `production_posture_test.sh` failure (Task 8.1)

`tests/unit/deploy/production_posture_test.sh` (CC-0088, REQ-012)
asserts byte-identity of `deploy/flux-system/fluxinstance.yaml` and
`deploy/flux-system/releases/*` against `origin/main` and is **expected
to fail on this branch** by design. The diff is intentional and confined
to exactly the three HelmReleases this PR retargets:

```
$ bash tests/unit/deploy/production_posture_test.sh
Test: deploy/flux-system/{fluxinstance,releases/*} unchanged vs origin/main (CC-0088, REQ-012)
  FAIL: production overlay has unexpected changes vs origin/main

--- diff in deploy/flux-system/releases (excluding chaos-mesh.yaml relocated by CC-0097) ---
deploy/flux-system/releases/keystone-operator.yaml      — metadata.namespace: openstack → keystone-system
deploy/flux-system/releases/memcached-operator.yaml     — dependsOn[prometheus-operator-crds].namespace: monitoring-system → monitoring
deploy/flux-system/releases/prometheus-operator-crds.yaml — metadata.namespace: monitoring-system → monitoring
```

Intentional diffs (full list):

| File | Change | Requirement |
| --- | --- | --- |
| `deploy/flux-system/releases/keystone-operator.yaml` | `metadata.namespace: openstack → keystone-system`; new CC-0105 header comment | REQ-001 |
| `deploy/flux-system/releases/prometheus-operator-crds.yaml` | `metadata.namespace: monitoring-system → monitoring`; appended `CC-0105` to feature marker | REQ-005 |
| `deploy/flux-system/releases/memcached-operator.yaml` | `dependsOn[prometheus-operator-crds].namespace: monitoring-system → monitoring`; new CC-0105 header comment | REQ-006 |

`deploy/flux-system/fluxinstance.yaml` is **not** modified — that file
remains byte-identical to `origin/main` and is therefore still covered
by the byte-identity check after merge.

Per Task 8.1: **no carve-out, exclusion, or test-logic change is
introduced** on this branch. Once CC-0105 lands, the next push to
`main` becomes the new baseline and `production_posture_test.sh`
returns to a passing state automatically without any test edit. If a
CI gate blocks merge on this single failure, the narrowly-scoped
remediation is to add a per-path `:(exclude)…` pathspec to the
`groups_specs` array (modelled on the existing CC-0097
`chaos-mesh.yaml` carve-out at lines 84–87) tagged `CC-0105` with a
TODO to remove after merge — but this should **not** be applied
preemptively.

## Final cross-reference audit (Task 8.2)

Two greps were re-run at HEAD `79694fa9`:

```
$ rg -n 'monitoring-system'
$ rg -n -- '-n openstack.*app\.kubernetes\.io/name=keystone-operator|app\.kubernetes\.io/name=keystone-operator.*-n openstack'
```

Every match falls into category **(a)** workload reference (correct),
**(b)** historical / migration note / regression guard (acceptable),
or **(c)** updated to `keystone-system` / `monitoring` (correct).
**Zero category-(d) current-state references remain.** Breakdown:

### `monitoring-system` matches (10 files)

| File | Category | Justification |
| --- | --- | --- |
| `deploy/flux-system/namespaces.yaml:12` | (b) | Header-comment rationale: `"the dead monitoring-system Namespace is dropped after the…"` — documents the removal. |
| `tests/unit/deploy/namespaces_test.sh` | (b) | Regression guard — asserts `monitoring-system` is **absent** from the production overlay (REQ-003). |
| `tests/unit/deploy/helmrelease_namespaces_test.sh` | (b) | Regression guard — asserts `memcached-operator` carries **zero** `monitoring-system` strings (REQ-006). |
| `tests/unit/docs/namespace_consistency_test.sh` | (b) | Regression guard — fails closed if a future doc edit reintroduces `monitoring-system` outside the allow-listed migration comments. |
| `tests/integration/deploy/kustomize_render_test.sh` | (b) | Regression guard — asserts combined `kustomize build` output contains **zero** `monitoring-system` references (REQ-003). |
| `tests/integration/ci/full_kind_up_test.sh:26` | (b) | Header-comment context: explains why this gate exists (`"monitoring-system removal (REQ-003) would otherwise be invisible until a downstream e2e suite fails much later"`). |
| `architecture/docs/05-deployment/04-namespace-policy.md:63` | (b) | Namespace-inventory paragraph: `"monitoring replaces the previous monitoring-system Namespace"` — explicit migration note. Submodule content; read-only from this worktree. |
| `.planwerk/progress/CC-0105-…json` | (b) | Feature requirements / progress doc — documents the migration. |
| `.planwerk/reviews/CC-0089-…json` | (b) | Historical review artefact pre-dating CC-0105 (TODO comment referencing the legacy service DNS). |
| `.planwerk/reviews/CC-0046-…json` | (b) | Historical review artefact pre-dating CC-0105. |

`tests/e2e/keystone/metrics/chainsaw-test.yaml` previously carried a
stale TODO URL `http://prometheus.monitoring-system.svc:9090/…`; this
PR rewrites it to `http://prometheus.monitoring.svc:9090/…` so the
deferred TODO target is correct when implemented (CC-0105).

### Operator-pod `-n openstack` matches (active code/docs/tests)

No matches outside `.planwerk/` and `architecture/docs/`:

```
$ rg -n -- '-n openstack.*app\.kubernetes\.io/name=keystone-operator|app\.kubernetes\.io/name=keystone-operator.*-n openstack' \
    tests/ docs/ hack/ deploy/ operators/ .github/
(no output)
```

Remaining matches:

| File | Category | Justification |
| --- | --- | --- |
| `architecture/docs/05-deployment/04-namespace-policy.md:38` | (b) | Pedagogical example: `"kubectl -n openstack get pods -l app.kubernetes.io/name=keystone-operator returns nothing — the controller is not there"`. Submodule; read-only. |
| `.planwerk/features/CC-0103-…json` | (b) | Historical CC-0103 task spec pre-dating CC-0105. Active e2e test files corresponding to those task specs (e.g. `tests/e2e/keystone/metrics/chainsaw-test.yaml`) have already been retargeted to `-n keystone-system`. |
| `.planwerk/progress/CC-0105-…json` | (b) | CC-0105 audit criteria — explicit `"every kubectl -n openstack … keystone-operator occurrence is audited"` requirement text. |

### Chaos-test gap caught and fixed (Task 8.2 follow-up)

The audit also surfaced a CC-0105 regression that none of the prior
tasks scoped: `tests/e2e-chaos/operator-pod-{crash,kill}/` carried
hard-coded `-n default` selectors (operator-pod kubectl commands,
PodChaos `selector.namespaces`, and diagnostic `--dep-ns=default`)
based on the pre-CC-0105 assumption that `hack/ci-deploy-operator.sh`
invoked `helm install` without `--namespace`. After CC-0105's Task 4.1
change to that script, the operator now lands in `keystone-system`,
not `default`, so the chaos PodChaos selectors would no longer match
any pod and the wait-for-replacement loops would time out.

`make e2e` only runs `tests/e2e/` (not `tests/e2e-chaos/`), so this
gap was invisible to Task 7.3's local re-run. This PR retargets the
two affected chaos suites to `keystone-system`:

- `tests/e2e-chaos/operator-pod-crash/01-podchaos.yaml` —
  `selector.namespaces: [default] → [keystone-system]`.
- `tests/e2e-chaos/operator-pod-kill/01-podchaos.yaml` — same.
- `tests/e2e-chaos/operator-pod-crash/chainsaw-test.yaml` — all four
  `kubectl … -n default` calls and both `--dep-ns=default`
  invocations now use `keystone-system`; NOTE comment rewritten to
  reflect the new `helm install --namespace keystone-system
  --create-namespace`.
- `tests/e2e-chaos/operator-pod-kill/chainsaw-test.yaml` — same
  (four kubectl calls, three `--dep-ns` invocations).

### Known non-blocker (out-of-scope diagnostics gap)

`tests/e2e-chaos/diagnostics.sh` collects `--log-label` matches from
`$NAMESPACE` (the chainsaw `spec.namespace`, which is `openstack` for
the mariadb-* and openbao-* chaos suites). After CC-0105, those
suites' catch-block calls
`../diagnostics.sh chaos … --log-label=app.kubernetes.io/name=keystone-operator`
will no longer find operator pods in `openstack` — diagnostic output
will be empty rather than wrong. This only degrades failure-time
diagnostics; it does not fail any test. A follow-up should add a
`--log-ns` option to `diagnostics.sh` and pass `--log-ns=keystone-system`
from the affected suites, tracked separately.

## Architecture submodule follow-up

`.gitmodules` pins `architecture → https://github.com/C5C3/C5C3`. The
new `architecture/docs/05-deployment/04-namespace-policy.md` and the
linked `index.md` change committed on the submodule's `main` branch
are visible at the commit pinned by this worktree (`7beebd6f`).
Per repository rule `NO_SUBMODULE_MODIFICATIONS`, all submodule edits
must land via a separate upstream PR to
`https://github.com/C5C3/C5C3`; this PR only bumps the submodule
pointer to the upstream commit that contains them. Once the
upstream architecture PR merges, the submodule pin in this worktree
will be advanced in the same merge.
