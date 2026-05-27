---
name: check-condition-coverage
description: >-
  Audit Keystone Status condition coverage end-to-end — every condition
  type set by a sub-reconciler must appear in the
  subReconcilerConditionTypes map (so metrics labels resolve), every
  sub-reconciler must have a unit test, and every condition the docs
  document must be set somewhere in code. Use when asked to check
  condition coverage, after adding or renaming a sub-reconciler or a
  condition type, or when a Prometheus condition_type label suddenly
  shows up as UNKNOWN.
---

# Check condition coverage

This skill verifies that the Keystone operator's **status conditions
are wired end-to-end**: every condition type literal set in the
sub-reconcilers is registered in the
`subReconcilerConditionTypes` map (so the
`keystone_reconcile_errors_total{condition_type=…}` Prometheus label
resolves to a known value), every sub-reconciler has a unit test, and
every condition type the architecture docs name is set somewhere in
code.

It is repeatable — run it any time, especially after editing
`reconcile_*.go`, the `subReconcilerConditionTypes` map, or the
condition-type constants in `keystone_types.go`.

## What condition coverage means here

A Keystone condition threads through five layers. Drift in any one
shows up as a missed alert, a stale doc, an orphan condition type, or
an `UNKNOWN` Prometheus label:

| Layer | Where it lives | Source of truth |
|---|---|---|
| Declared condition type | `operators/keystone/api/v1alpha1/keystone_types.go` (Status struct doc) and the const blocks in `operators/keystone/internal/controller/` (e.g. `conditionTypeDatabaseTLSReady`, `conditionTypeLoggingHealthy`) | the literal `"<Name>Ready"` string |
| Set in code | `operators/keystone/internal/controller/reconcile_*.go` (`conditions.SetCondition` with `Type: "<Name>Ready"` or `Type: conditionType<X>Ready`) | the sub-reconciler that actually computes the status |
| Instrumentation map | `operators/keystone/internal/controller/instrumentation.go` (`subReconcilerConditionTypes`) | the canonical sub-reconciler → condition-type label binding for Prometheus |
| Unit tests | `operators/keystone/internal/controller/reconcile_*_test.go` plus drift-guard tests `TestSubReconcilerConditionTypesCoversAllNames` and `TestSubReconcilerConditionTypesCoversAllCallSites` (in `instrumentation_test.go`) | test functions named after the sub-reconciler |
| Docs | `architecture/docs/09-implementation/03-crd-implementation.md` | the table or list of condition types |

The authoritative gate is the pair of drift-guard tests in
`instrumentation_test.go`. This skill defers to them as the source of
truth for the map vs call-site invariant and adds the inventory checks
the guards cannot express on their own.

A coverage finding is any place a condition type is set without being
registered in the map (alerts emit `UNKNOWN`), declared but never set
(orphan declaration), or set/declared but undocumented (operator
discovers a condition the docs do not describe).

## Procedure

Work through these steps in order and report findings at the end.

### 1. Run the deterministic audit

```bash
bash .claude/skills/check-condition-coverage/scripts/audit-condition-coverage.sh
```

The script catches the mechanically-checkable gaps and prints an
inventory. Exit code `1` means at least one `[FAIL]`. Interpret:

- **K1** — every condition-type literal set in `reconcile_*.go`
  (`Type: "<Name>Ready"`) appears in
  `subReconcilerConditionTypes`. A miss means the `condition_type`
  Prometheus label emits as `UNKNOWN` whenever this sub-reconciler
  errors — the existing drift-guard test in `instrumentation_test.go`
  is the authoritative gate.
- **K2** — every condition-type constant used as `Type: conditionType<X>Ready`
  is defined exactly once and used in at least one sub-reconciler.
  A dead constant means a renamed condition that was not cleaned up.
- **K3** — every `reconcile_*.go` (excluding `_test.go`) has a paired
  `reconcile_*_test.go`. A missing test means the sub-reconciler can
  silently break.
- **K4** — every condition type set in code appears at least once in
  the docs (`architecture/docs/09-implementation/03-crd-implementation.md`).
  A miss means an operator reading the docs cannot find the condition.
- **K5** — every condition type referenced in the docs is set in code
  by at least one sub-reconciler. A miss means a stale doc condition
  that was renamed in code but not in prose.
- The **inventory** lists, per sub-reconciler:
  - the condition type(s) it sets,
  - the entry in `subReconcilerConditionTypes` it expects,
  - the test function(s) that exercise it.

### 2. Cross-reference the inventory

The script does not resolve Go const ⇢ string values. Using the
printed inventory, confirm:

1. For each `conditionType<X>Ready` constant that K1 surfaced, walk
   the const definition to confirm the string literal value matches
   the map entry exactly (the map's right-hand side may itself use
   the constant — drift between the literal and the constant is the
   most common K1 false-positive).
2. For each sub-reconciler in the inventory, confirm by hand that
   the `*_test.go` actually exercises the condition transitions the
   `reconcile_*.go` performs (Reason / Message / Status). Counting
   tests by file existence is necessary but not sufficient.
3. For each `[FAIL]` from K4 / K5, decide: doc fix or code fix.

### 3. Run the authoritative gates

The script does not run Go tests. Run the drift-guard tests directly:

```bash
go test ./operators/keystone/internal/controller/... -run TestSubReconcilerConditionTypesCovers -count=1
```

These two tests (`TestSubReconcilerConditionTypesCoversAllNames` and
`TestSubReconcilerConditionTypesCoversAllCallSites`) are the
authoritative gate for the call-site / map invariant.

### 4. Report

Produce a concise summary grouped by severity:

- **HIGH** — a condition type set in code is missing from
  `subReconcilerConditionTypes` (Prometheus emits `UNKNOWN`); a
  drift-guard test fails; a constant is referenced but undefined.
- **MEDIUM** — a sub-reconciler without a paired test file; a
  condition type set in code but undocumented; a doc condition type
  with no code anchor.
- **LOW** — a constant defined but unused (dead code); a condition
  type with an inconsistent suffix (e.g. `Healthy` mixed in with
  `Ready` set); a sub-reconciler whose test file exists but is
  almost empty.

For each finding give one line with a `file:line` reference for both
the call-site and the map / doc / test side. End with a per-condition
health verdict.

## Coverage patterns

These recurring shapes are worth grepping for first:

1. **Renamed condition, stale map entry.** A `Type: "OldNameReady"`
   call was updated to `"NewNameReady"`, but the
   `subReconcilerConditionTypes` map still has `"OldNameReady"`. The
   metric label resolves to `UNKNOWN` until the map is patched.
2. **New sub-reconciler, no map entry.** A `reconcile_<new>.go` was
   added with `Type: "<New>Ready"`, but the
   `subReconcilerConditionTypes` map was not extended. The first
   error from the new reconciler shows up as `UNKNOWN` in alerts.
3. **Mixed suffix drift.** A condition is named `<Name>Healthy`
   instead of `<Name>Ready`. The instrumentation map and the
   meta.IsStatusConditionTrue helpers all expect the `Ready` suffix;
   the off-pattern name silently bypasses generic readiness wiring.
4. **Orphan constant.** A const like `conditionTypeOldReady = "OldReady"`
   stayed behind after the last call site was deleted. The constant
   is dead code that suggests the condition is still active.
5. **Doc-only condition.** The docs list a condition type that no
   sub-reconciler actually sets. Operators monitoring for it see it
   as perpetually `Unknown` and assume the controller is broken.

## Notes

- This skill is read-only; the deterministic script edits nothing.
  Apply fixes (extend the map, add the test, update the docs) as a
  separate, explicitly-scoped task.
- The Go `const` ⇢ string mapping is intentionally not resolved by
  the script — the existing `TestSubReconcilerConditionTypesCovers*`
  drift-guard tests do that with full Go AST awareness. Treat K1 as
  a smoke check and the test as the authoritative gate.
- Pair this with [[check-doc-drift]] — that skill confirms the prose
  reference matches the code (sub-reconciler chain, OPERATORS list);
  this skill drills into the condition-type wiring specifically.
