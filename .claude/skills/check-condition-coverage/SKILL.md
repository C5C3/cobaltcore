---
name: check-condition-coverage
description: >-
  Audit Status condition coverage end-to-end for every forge operator —
  every condition type set by a sub-reconciler must appear in that
  operator's subReconcilerConditionTypes map (so metrics labels
  resolve), every sub-reconciler must have a unit test, and every
  condition the operator's reference docs document must be set
  somewhere in code. Use when asked to check condition coverage, after
  adding or renaming a sub-reconciler or a condition type, or when a
  Prometheus condition_type label suddenly shows up as UNKNOWN.
---

# Check condition coverage

This skill verifies that each forge operator's **status conditions are
wired end-to-end**: every condition type set in the sub-reconcilers is
registered in that operator's `subReconcilerConditionTypes` map (so the
`<op>_operator_reconcile_errors_total{condition_type=…}` Prometheus
label resolves to a known value), every sub-reconciler has a unit
test, and every condition type the reference docs name is set
somewhere in code. Operators are discovered dynamically via
`operators/*/internal/controller/instrumentation.go`, so a newly
onboarded service operator is audited without touching this skill.

It is repeatable — run it any time, especially after editing a
`reconcile_*.go`, a `subReconcilerConditionTypes` map, or the
condition-type constants in an operator's controller package.

## What condition coverage means here

An operator condition threads through five layers. Drift in any one
shows up as a missed alert, a stale doc, an orphan condition type, or
an `UNKNOWN` Prometheus label:

| Layer | Where it lives | Source of truth |
|---|---|---|
| Declared condition type | the const blocks in `operators/<op>/internal/controller/` (e.g. `conditionTypeDatabaseTLSReady`, `conditionTypeCatalogReady`) and the Status struct docs in `operators/<op>/api/v1alpha1/` | the literal `"<Name>Ready"` string |
| Set in code | `operators/<op>/internal/controller/reconcile_*.go` (`conditions.SetCondition` with `Type: "<Name>Ready"` or `Type: conditionType<X>Ready`) | the sub-reconciler that actually computes the status |
| Instrumentation map | `operators/<op>/internal/controller/instrumentation.go` (`subReconcilerConditionTypes`) | the canonical sub-reconciler → condition-type label binding for Prometheus |
| Unit tests | `operators/<op>/internal/controller/reconcile_*_test.go` plus the drift-guard test `TestSubReconcilerConditionTypesCoversAllNames` (in `instrumentation_test.go`) | test functions named after the sub-reconciler |
| Docs | `docs/reference/<op>/*.md` (the reconciler and CRD reference pages); keystone additionally `architecture/docs/09-implementation/03-crd-implementation.md` when the submodule is checked out | the condition tables / sections in the reference docs |

The authoritative gate is the per-operator drift-guard test
`TestSubReconcilerConditionTypesCoversAllNames` in
`instrumentation_test.go` (map values ⊆ `subConditionTypes`). This
skill defers to it as the source of truth for that invariant and adds
the inventory checks the guard cannot express on its own — most
importantly the call-site → map direction.

A coverage finding is any place a condition type is set without being
registered in the map (alerts emit `UNKNOWN`), declared but never set
(orphan declaration), or set/declared but undocumented (operator
discovers a condition the docs do not describe).

The bare aggregate `Ready` condition is exempt from the map: it is the
top-level roll-up, not a sub-reconciler condition (the drift-guard
test comment documents that aggregated conditions carry no dedicated
sub-reconciler).

## Procedure

Work through these steps in order and report findings at the end.

### 1. Run the deterministic audit

```bash
bash .claude/skills/check-condition-coverage/scripts/audit-condition-coverage.sh
```

The script discovers every instrumented operator and catches the
mechanically-checkable gaps, printing a per-operator inventory. Exit
code `1` means at least one `[FAIL]`. Interpret:

- **K1** — every condition type set in `reconcile_*.go` — as a
  literal `Type: "<Name>Ready"` or a resolved
  `Type: conditionType<X>Ready` constant — appears in that operator's
  `subReconcilerConditionTypes`. A miss means the `condition_type`
  Prometheus label emits as `UNKNOWN` whenever this sub-reconciler
  errors. Constants resolving to the bare aggregate `"Ready"` are
  exempt.
- **K2** — every condition-type constant used as `Type: conditionType<X>Ready`
  is defined exactly once and used in at least one sub-reconciler.
  A dead constant means a renamed condition that was not cleaned up.
- **K3** — every `reconcile_*.go` (excluding `_test.go`) has a paired
  `reconcile_*_test.go`. A missing test means the sub-reconciler can
  silently break. Coverage that lives only in `integration_test.go`
  or a shared invariant test does not satisfy the pairing convention.
- **K4** — every condition type in the instrumentation map appears at
  least once in the operator's doc corpus (`docs/reference/<op>/`).
  A miss means an operator reading the docs cannot find the condition.
- **K5** — every condition type referenced in the doc corpus is set in
  code by at least one sub-reconciler. A miss means a stale doc
  condition that was renamed in code but not in prose. Diagram
  abbreviations (e.g. `InfraReady` for `InfrastructureReady` in ASCII
  art) and cross-operator references demote to `[INFO]` — confirm
  those by hand.
- The **inventory** lists, per operator and sub-reconciler:
  - the condition type(s) it sets (literals and constants),
  - the entry in `subReconcilerConditionTypes` it expects,
  - the test function(s) that exercise it.

### 2. Cross-reference the inventory

The script resolves Go const ⇢ string values with a line-oriented
grep, not a Go parser. Using the printed inventory, confirm:

1. For each constant K1 flagged as unresolvable, walk the const
   definition to confirm the string literal value matches the map
   entry exactly (the map's right-hand side may itself use the
   constant — drift between the literal and the constant is the most
   common K1 false-positive).
2. For each sub-reconciler in the inventory, confirm by hand that
   the `*_test.go` actually exercises the condition transitions the
   `reconcile_*.go` performs (Reason / Message / Status). Counting
   tests by file existence is necessary but not sufficient.
3. For each `[FAIL]` from K4 / K5, decide: doc fix or code fix.
4. For each `[INFO]` abbreviation or cross-operator reference from
   K5, confirm the prose really means the full condition type.

### 3. Run the authoritative gates

The script does not run Go tests. Run the drift-guard test for every
instrumented operator directly:

```bash
go test ./operators/keystone/internal/controller/... ./operators/c5c3/internal/controller/... \
  -run TestSubReconcilerConditionTypesCoversAllNames -count=1
```

Extend the package list when a new operator lands (any module with an
`internal/controller/instrumentation.go`). This test is the
authoritative gate for the map ⊆ `subConditionTypes` invariant.

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
the call-site and the map / doc / test side. End with a per-operator,
per-condition health verdict.

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
3. **New operator, no audit.** A freshly onboarded operator gained an
   `instrumentation.go` by copying keystone's, but its sub-reconcilers
   or tests diverged before the first release. The dynamic discovery
   picks the operator up automatically — check its section of the
   script output rather than assuming keystone parity.
4. **Mixed suffix drift.** A condition is named `<Name>Healthy`
   instead of `<Name>Ready`. The instrumentation map and the
   meta.IsStatusConditionTrue helpers all expect the `Ready` suffix;
   the off-pattern name silently bypasses generic readiness wiring.
5. **Orphan constant.** A const like `conditionTypeOldReady = "OldReady"`
   stayed behind after the last call site was deleted. The constant
   is dead code that suggests the condition is still active.
6. **Doc-only condition.** The docs list a condition type that no
   sub-reconciler actually sets. Operators monitoring for it see it
   as perpetually `Unknown` and assume the controller is broken.

## Notes

- This skill is read-only; the deterministic script edits nothing.
  Apply fixes (extend the map, add the test, update the docs) as a
  separate, explicitly-scoped task.
- The Go `const` ⇢ string mapping is resolved by grep, not by a Go
  parser — the per-operator `TestSubReconcilerConditionTypesCoversAllNames`
  drift-guard test does the map-side check with full Go AST awareness.
  Treat K1 as a smoke check and the test as the authoritative gate.
- The `architecture/` submodule is usually not checked out; the
  keystone architecture chapter is consulted only when present. The
  operator's `docs/reference/<op>/` pages are the primary doc corpus.
- Pair this with [[check-doc-drift]] — that skill confirms the prose
  reference matches the code (sub-reconciler chain, OPERATORS list);
  this skill drills into the condition-type wiring specifically.
