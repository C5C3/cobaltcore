---
name: check-doc-drift
description: >-
  Audit the CobaltCore documentation for drift against the implementation —
  the docs/reference/ pages vs the operator code, the guides and
  quick-starts vs the deploy/ infrastructure stack. Use when
  asked to check documentation drift, after adding or
  removing a sub-reconciler, status condition, operator binary, or
  infrastructure component, or before tagging a release.
---

# Check documentation drift

This skill verifies that the CobaltCore documentation still **describes what
the code actually does**: every sub-reconciler, status condition,
operator binary, CRD field, and FluxCD component
named in `docs/` (and `README.md`) is checked
against its source of truth, so a reader is never handed a reference
page that contradicts the implementation.

It is repeatable — run it any time, especially after shipping a feature
that touched a documented surface, or before cutting a release.

## What documentation drift means here

Drift is any place a doc and the code it documents disagree. The audit
splits the corpus into three areas, each with a single source of truth:

| Doc area | Files | Source of truth |
|---|---|---|
| Operator reference | `docs/reference/<op>/` reconciler pages (e.g. `keystone-reconciler.md`, `controlplane-reconciler.md`, `horizon-reconciler.md`) | `operators/<op>/internal/controller/reconcile_*.go` + `operators/<op>/api/v1alpha1/*_types.go` + `internal/common/` |
| CRD reference | `docs/reference/<op>/*-crd.md` | `operators/<op>/api/v1alpha1/*_types.go` and the generated CRDs under `operators/<op>/config/crd/bases/` |
| Infrastructure stack | `docs/guides/`, `docs/quick-start*.md`, `docs/reference/infrastructure/` | `deploy/` kustomize tree + `hack/deploy-infra.sh` (incl. `INFRA_ONLY`) + `hack/deploy-mgmt-cluster.sh` + the `deploy/target-cluster/target-cluster-access` helm chart + `releases/<version>/source-refs.yaml` |
| Cross-cluster reference | `docs/reference/target-clusters.md` (the placement contract: ownership labels, teardown order, capability probing, per-service placement notes) | `internal/common/multicluster/` + the placement paths in `operators/*/internal/controller/` + the `Makefile` `e2e-multicluster` target |

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

- **D1** — `OPERATORS ?=` default in `Makefile` vs the operator modules
  under `operators/` (directories with a `go.mod`; `operators/shared/`
  is a helm library, not an operator). An operator added under
  `operators/` but not added to the Makefile default (or vice versa)
  means the new binary never gets built/tested by `make build` /
  `make test` / `make lint`.
- **D2** — every `### reconcile…` heading under `docs/reference/`
  names a function that exists in an
  `operators/*/internal/controller/` package. A renamed or removed
  sub-reconciler leaves the heading stranded.
- **D3** — every `deploy/<component>/` directory mentioned by name in
  `docs/` or `README.md` exists. A renamed or removed infra component
  leaves the doc page pointing at nothing.
- The **inventories** are review aids, not pass/fail: every spelled-out
  numeric claim in the docs ("11 sub-conditions", "8-step deployment",
  "three states"), every FluxCD release name documented vs declared.
  Cross-reference each by hand in step 2.

Condition-type doc coverage (every condition set in code appears in the
`docs/reference/<op>/` pages, and vice versa) is audited by
[[check-condition-coverage]] (K4/K5) — do not duplicate it here.

### 2. Cross-reference the three areas by hand

The script cannot read prose meaning. For each area, open the doc and
its source of truth and confirm they agree. This is the real work —
delegate the areas to parallel sub-agents for a large corpus.

- **Operator reference** — for each `### reconcile…` section in the
  `docs/reference/<op>/` reconciler page, confirm the description
  matches the current implementation under
  `operators/<op>/internal/controller/` (what it touches, what
  condition it sets, what it requeues on).
- **CRD reference** — for each Spec field listed in
  `docs/reference/<op>/*-crd.md`, confirm the type, JSON tag, default,
  required-ness, and CEL validation rule match the
  `*_types.go` source. Then walk the generated CRD under
  `operators/<op>/config/crd/bases/` and confirm no Spec field is
  undocumented.
- **Infrastructure stack** — for each component named in
  `docs/guides/`, `docs/quick-start*.md`, and
  `docs/reference/infrastructure/`, confirm a matching directory
  exists under `deploy/` and that `hack/deploy-infra.sh` still
  installs it in the documented order.
- **Cross-cluster reference** — `docs/reference/target-clusters.md`
  spans every operator: confirm the documented ownership-label set,
  teardown order, registration-Secret contract (`cobaltcore-target` in
  `c5c3-clusters`), and per-service placement notes still match
  `internal/common/multicluster/` and the operators' placement code.
  Its condition-type mentions are cross-checked mechanically by
  [[check-condition-coverage]] (K6) — do not duplicate that here.
Flag any pair where the doc and the source disagree.

### 3. Report

Produce a concise summary grouped by severity:

- **HIGH** — `OPERATORS` Makefile default disagrees with the
  `operators/` tree; a `### reconcile…` doc heading names a function
  that does not exist; a documented condition type is never set by
  any sub-reconciler; an infrastructure component named in the docs
  has no matching `deploy/` directory.
- **MEDIUM** — a sub-reconciler with no doc section; a condition type
  set in the code that is not documented; a Spec field with a
  `+kubebuilder` marker that is not described in the CRD reference
  page; a numeric claim ("11 sub-conditions") that no longer matches
  the code count.
- **LOW** — a stale link target inside `docs/`; a typo or
  formatting drift.

For each finding give one line with a `file:line` reference for both
the doc side and the source side. End with a two- to three-sentence
health verdict per doc area.

### 4. Verification guardrails

Before finalizing findings:

- Quote the exact doc text and the exact source-side text that disagree.
- Include `file:line` for both sides of every finding.
- If you cannot quote or locate one side precisely, downgrade the finding
  to **MEDIUM** or **LOW** and mark it **needs research**.
- Re-open and re-check every **HIGH** finding against the current
  checkout before output.

A zero-finding run is valid. If docs and source match for the audited
scope, report a clean result.

### 5. Suppressions (do not report)

- Pure style or tone issues with no factual mismatch (hand those to
  [[check-doc-expressions]]).
- Naming preferences that are not contradictory and do not mislead the
  reader.
- Weak "might be stale" guesses without a concrete conflicting source.

## Drift patterns

These recurring shapes are worth grepping for first:

1. **New sub-reconciler, no doc section.** A `reconcile_<thing>.go`
   was added but the `docs/reference/<op>/` reconciler page still
   describes the old sub-reconciler set. The new condition type is
   then set by the controller but not described in the CRD reference
   page either.
2. **Renamed condition type.** A condition was renamed in
   `*_types.go` and the sub-reconciler code, but the docs
   still use the old type name in a table row. Operators looking up
   the new name in the docs find nothing.
3. **Removed Spec field.** A field was deleted from `*_types.go`
   but the CRD reference page still lists it. CR fixtures that use the
   field then fail with an obscure unknown-field error and the doc
   is the only place that knew what it meant.
4. **OPERATORS drift.** A new operator binary was added under
   `operators/` but the Makefile default still lists only the
   originals. `make test` / `make lint` silently skip the new
   operator until someone notices a CI gap.
5. **Renamed infra release.** `deploy/<component>/` was renamed
   (e.g. `cert-manager` ⇢ `cert-manager-istio`) but a guide or
   quick-start still uses the old name. New operators copy the wrong
   reference.

## Notes

- This skill is read-only; the deterministic script edits nothing.
  Apply fixes (update the doc, delete the stale citation, add the
  missing condition row) as a separate, explicitly-scoped task.
- Pair this with [[check-crd-drift]] — that skill confirms the CRD
  YAML still mirrors the Go source; this skill confirms the prose
  reference still mirrors both.
- Pair this with [[check-condition-coverage]] — that skill confirms
  every condition type set in the code is wired to the instrumentation
  map and documented; this skill covers the remaining doc surface.
