# CC-0103 — test(e2e): Cover operator metrics surface and steady-state event budget

Closes #316. Parent: #277 (Phase 1 / Observability basics).

## Summary

Adds two e2e suites and documents both, closing the two open Phase 1
checkboxes on #277:

- `tests/e2e/keystone/operator-metrics-endpoint/` — live scrape of the
  operator's `/metrics` endpoint via an in-cluster probe pod; asserts
  every Prometheus metric family registered by `globalCollectors()` is
  exposed with at least one labelled sample row (REQ-001..REQ-004).
  Complements the existing `tests/e2e/keystone/metrics/` suite, which
  only verifies `ServiceMonitor` CRD shape (Prometheus is not deployed
  in the kind overlay).
- `tests/e2e/keystone/event-budget/` — steady-state regression gate:
  after `Ready=AllReady`, captures `T0`, sleeps 5 minutes, then counts
  Keystone events newer than `T0` excluding the four lifecycle reasons
  asserted by `tests/e2e/keystone/events/`. Asserts the count is `<= 5`
  (REQ-005..REQ-008). The 5-minute window is chosen so the weekly
  fernet rotation `CronJob` (`0 0 * * 0`) cannot fire inside it; any
  non-lifecycle event in the window is signal.
- `docs/reference/testing/keystone-e2e-tests.md` — adds inventory rows
  and `## Test Suite Details` subsections for both new suites
  (REQ-009).

No operator, chart, deploy, hack, or workflow code is changed.

## Diff scope verification (Task 4.4, REQ-010)

`git diff --name-only main..HEAD` at the implementation HEAD
(`1df61141`):

```
.planwerk/progress/CC-0103-test-e2e-cover-operator-metrics-surface-and.json
docs/reference/testing/keystone-e2e-tests.md
tests/e2e/keystone/event-budget/00-keystone-cr.yaml
tests/e2e/keystone/event-budget/chainsaw-test.yaml
tests/e2e/keystone/operator-metrics-endpoint/00-keystone-cr.yaml
tests/e2e/keystone/operator-metrics-endpoint/chainsaw-test.yaml
```

(This very PR description, `.planwerk/CC-0103-pr-description.md`, will
also appear in the diff once committed by the planwerk harness.
Neither it nor the `.planwerk/progress/...json` is product code —
both are planwerk bookkeeping kept in-repo by convention, mirroring
CC-0093.)

REQ-010 forbids changes under `operators/`, `deploy/`, or `.github/`.
Cross-checking the file list against every strictly-forbidden
prefix:

| Forbidden prefix (REQ-010) | Files in this PR |
| --- | --- |
| `operators/` | none |
| `deploy/` | none |
| `.github/` | none |
| `helm/` (chart) | none |
| `hack/` | none |

Every product path under the diff falls into exactly one of the three
allowed locations:

- `tests/e2e/keystone/operator-metrics-endpoint/` — 2 files
- `tests/e2e/keystone/event-budget/` — 2 files
- `docs/reference/testing/keystone-e2e-tests.md` — 1 file

REQ-010 is satisfied.

## Manual sanity check (Task 4.3, REQ-011) — pre-merge gate, NOT in CI

REQ-011 calls for empirical validation that `BUDGET=5` actually
catches a per-reconcile event-spam regression — not a guess that
survives because no regression happened to fire. This is a one-shot
reviewer signoff procedure run against `make kind-up`. The patch
below is reverted before merge; the diff scope verification above is
the gate that proves no operator file reaches the merge commit.

### (a) The exact patch

File: `operators/keystone/internal/controller/keystone_controller.go`

Insertion point: immediately after the `r.Get(ctx, req.NamespacedName, &keystone)`
error-return block (which currently ends at line 191) and before the
deletion-handling block at line 200. This guarantees the synthetic
event fires on every Reconcile pass for a live CR, regardless of
sub-reconciler outcome.

```go
	// CC-0103 REQ-011 sanity check — DO NOT MERGE.
	r.Recorder.Eventf(&keystone, corev1.EventTypeNormal, "ReconcilePass",
		"synthetic per-reconcile event for CC-0103 event-budget regression check")
```

Notes:

- `&keystone` because the local at line 184 is declared
  `var keystone keystonev1alpha1.Keystone` (value type). Existing
  call sites at lines 400/408/472/484 pass `keystone` directly only
  because `reconcileDelete` receives a `*Keystone`.
- `corev1` and `r.Recorder` are already imported / wired (see lines
  400, 408, 472, 484 for the existing event-emission style). No new
  imports needed.

### (b) Command — run only the event-budget suite

```sh
make kind-up
hack/ci-deploy-operator.sh
chainsaw test --config tests/e2e/chainsaw-config.yaml \
  tests/e2e/keystone/event-budget/
```

(The targeted `chainsaw test` invocation matches the canonical
single-suite form documented in
`docs/reference/testing/keystone-e2e-tests.md:96-99`.)

### (c) Expected failure outcome

The default reconcile loop emits `ReconcilePass` on every pass, so
well over five events accumulate in the 5-minute steady-state window.
Step 3 of the suite logs:

```
count=<N> budget=5
```

with `<N> > 5` (typically several dozen). The next line
`test "$count" -le "$BUDGET"` exits non-zero, failing the test. The
catch block then prints, in order:

1. every event in the `openstack` namespace sorted by `lastTimestamp`
   (no truncation — diagnosis budget > byte budget),
2. the Keystone CR YAML for `keystone-event-budget`,
3. operator logs (tail 200).

The dumped event list shows the synthetic `ReconcilePass` reason
repeating, which is the smoking gun for a per-reconcile event-spam
regression. The `operator-metrics-endpoint` suite is unaffected by
the patch and continues to pass.

### (d) Revert

```sh
git checkout -- operators/keystone/internal/controller/keystone_controller.go
hack/ci-deploy-operator.sh
chainsaw test --config tests/e2e/chainsaw-config.yaml \
  tests/e2e/keystone/event-budget/
```

After revert, the test passes (typical `count` is 0). This pass-on-
revert step closes the loop: it confirms the budget is correctly
tuned — quiet under the real reconcile loop, loud under a synthetic
per-reconcile `Eventf`.

### CI scope statement

This procedure is **not** wired into CI. CI runs the unmodified
event-budget suite as part of the normal `chainsaw test` pass; the
synthetic `Eventf` patch above must never appear in the merge
commit. The diff scope verification in the previous section is the
mechanical guard that enforces this — if any file under
`operators/keystone/internal/controller/` shows in the diff, the PR
fails its own REQ-010 check.
