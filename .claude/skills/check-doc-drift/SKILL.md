---
name: check-doc-drift
description: >-
  Audit the forge documentation for drift against the implementation —
  the architecture/docs/ chapters vs the operator code, the docs/
  user-facing reference vs the deploy/ infrastructure stack. Use when
  asked to check documentation drift, after adding or
  removing a sub-reconciler, status condition, operator binary, or
  infrastructure component, or before tagging a release.
---

# Check documentation drift

This skill verifies that the forge documentation still **describes what
the code actually does**: every sub-reconciler, status condition,
operator binary, CRD field, and FluxCD component
named in `architecture/docs/` or `docs/` (and `README.md`) is checked
against its source of truth, so a reader is never handed a reference
page that contradicts the implementation.

It is repeatable — run it any time, especially after shipping a feature
that touched a documented surface, or before cutting a release.

## What documentation drift means here

Drift is any place a doc and the code it documents disagree. The audit
splits the corpus into three areas, each with a single source of truth:

| Doc area | Files | Source of truth |
|---|---|---|
| Operator architecture | `architecture/docs/09-implementation/04-keystone-reconciler.md`, `architecture/docs/09-implementation/02-shared-library.md`, `architecture/docs/04-architecture/*.md` | `operators/keystone/internal/controller/reconcile_*.go` + `operators/keystone/api/v1alpha1/keystone_types.go` + `internal/common/` |
| CRD reference | `architecture/docs/09-implementation/03-crd-implementation.md` | `operators/keystone/api/v1alpha1/keystone_types.go` and the generated CRD under `operators/keystone/config/crd/bases/` |
| Infrastructure stack | `architecture/docs/09-implementation/01-project-setup.md`, `architecture/docs/09-implementation/05-keystone-dependencies.md`, `architecture/docs/09-implementation/09-openbao-deployment.md`, `docs/guides/`, `docs/quick-start*.md` | `deploy/` kustomize tree + `hack/deploy-infra.sh` + `releases/<version>/source-refs.yaml` |

A doc that contradicts its source is a defect even when the build is
green: the compiler never reads prose. The audit's job is to surface
every disagreement, then let you judge severity and fix.

## Procedure

Work through these steps in order and report findings at the end.

### 1. Run the deterministic audit

```bash
bash .claude/skills/check-doc-drift/scripts/audit-doc-drift.sh
```

The script catches the mechanically-checkable drift and prints an
inventory. Exit code `1` means at least one `[FAIL]`. Interpret:

- **D1** — `OPERATORS ?=` default in `Makefile` vs the actual
  `operators/*/` directories. An operator added under `operators/` but
  not added to the Makefile default (or vice versa) means the new
  binary never gets built/tested by `make build` / `make test` /
  `make lint`.
- **D2** — count of `reconcile_*.go` files under
  `operators/keystone/internal/controller/` vs the count of
  `### reconcile…()` sections in
  `architecture/docs/09-implementation/04-keystone-reconciler.md`. A
  delta means a new sub-reconciler shipped without a doc section, or
  a section names a sub-reconciler that was removed.
- **D3** — every `### reconcile…()` heading in the reconciler doc
  names a function that exists in the controller package. A renamed
  reconciler leaves the heading stranded.
- **D4** — every condition type set in the sub-reconcilers (literal
  string in `meta.SetStatusCondition` / `conditions.SetCondition`
  calls) is documented under
  `architecture/docs/09-implementation/03-crd-implementation.md`.
- **D5** — retired. The audit used to cross-reference internal feature-ID
  markers in the code against `architecture/docs/`; those IDs have been
  removed repo-wide (a gate, `scripts/check-no-feature-ids.sh`, now forbids
  them), so the check is obsolete and reports nothing.
- **D6** — every `deploy/<component>/` directory mentioned by name in
  `architecture/docs/09-implementation/` exists. A renamed or removed
  infra component leaves the doc page pointing at nothing.
- The **inventories** are review aids, not pass/fail: every spelled-out
  numeric claim in the docs ("11 sub-conditions", "8-step deployment",
  "three states"), every FluxCD release name documented vs declared.
  Cross-reference each by hand in step 2.

### 2. Cross-reference the three areas by hand

The script cannot read prose meaning. For each area, open the doc and
its source of truth and confirm they agree. This is the real work —
delegate the four areas to parallel sub-agents for a large corpus.

- **Operator architecture** — for each `### reconcile…()` section in
  `04-keystone-reconciler.md`, confirm the description matches the
  current implementation under `operators/keystone/internal/controller/`
  (what it touches, what condition it sets, what it requeues on).
- **CRD reference** — for each Spec field listed in
  `03-crd-implementation.md`, confirm the type, JSON tag, default,
  required-ness, and CEL validation rule match
  `keystone_types.go`. Then walk the generated CRD under
  `operators/keystone/config/crd/bases/` and confirm no Spec field is
  undocumented.
- **Infrastructure stack** — for each component named in
  `01-project-setup.md` and `05-keystone-dependencies.md`, confirm a
  matching directory exists under `deploy/` and that
  `hack/deploy-infra.sh` still installs it in the documented order.
Flag any pair where the doc and the source disagree.

### 3. Report

Produce a concise summary grouped by severity:

- **HIGH** — `OPERATORS` Makefile default disagrees with the
  `operators/` tree; a `### reconcile…()` doc heading names a function
  that does not exist; a documented condition type is never set by
  any sub-reconciler; an infrastructure component named in the docs
  has no matching `deploy/` directory.
- **MEDIUM** — sub-reconciler count drift; a condition type set in
  the code that is not documented; a Spec field with a `+kubebuilder`
  marker that is not described in the CRD reference doc; a numeric
  claim ("11 sub-conditions") that no longer matches the code count.
- **LOW** — a stale link target inside `architecture/docs/`; a typo or
  formatting drift.

For each finding give one line with a `file:line` reference for both
the doc side and the source side. End with a two- to three-sentence
health verdict per doc area.

## Drift patterns

These recurring shapes are worth grepping for first:

1. **New sub-reconciler, no doc section.** A `reconcile_<thing>.go`
   was added but `04-keystone-reconciler.md` still lists the old
   sub-reconciler set. The new condition type is then set by the
   controller but not described under `03-crd-implementation.md`
   either.
2. **Renamed condition type.** A condition was renamed in
   `keystone_types.go` and the sub-reconciler code, but the docs
   still use the old type name in a table row. Operators looking up
   the new name in the docs find nothing.
3. **Removed Spec field.** A field was deleted from `keystone_types.go`
   but the CRD reference doc still lists it. CR fixtures that use the
   field then fail with an obscure unknown-field error and the doc
   is the only place that knew what it meant.
4. **OPERATORS drift.** A new operator binary was added under
   `operators/` but the Makefile default still lists only the
   originals. `make test` / `make lint` silently skip the new
   operator until someone notices a CI gap.
5. **Renamed infra release.** `deploy/<component>/` was renamed
   (e.g. `cert-manager` ⇢ `cert-manager-istio`) but
   `01-project-setup.md` or `05-keystone-dependencies.md` still uses
   the old name. New operators copy the wrong reference.

## Notes

- This skill is read-only; the deterministic script edits nothing.
  Apply fixes (update the doc, delete the stale citation, add the
  missing condition row) as a separate, explicitly-scoped task.
- Pair this with [[check-crd-drift]] — that skill confirms the CRD
  YAML still mirrors the Go source; this skill confirms the prose
  reference still mirrors both.
- Pair this with [[check-condition-coverage]] — that skill confirms
  every condition type set in the code is wired to the instrumentation
  map; this skill adds the doc-side check.
