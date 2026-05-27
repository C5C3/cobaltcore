---
name: check-crd-drift
description: >-
  Audit whether every forge operator CRD is in sync across its three
  representations — the Go +kubebuilder source under operators/<op>/api/,
  the controller-gen output under operators/<op>/config/crd/bases/, and
  the Helm chart copy under operators/<op>/helm/<op>-operator/crds/.
  Use when asked to check CRD drift, after editing a *_types.go file or
  a kubebuilder marker, or before a release to catch a CRD that the
  cluster will reject because the schema is older than the controller.
---

# Check CRD drift

This skill verifies that the forge generated CRDs still **reflect their
Go +kubebuilder source**: every kubebuilder marker is regenerable into
the YAML under `config/crd/bases/`, every Helm chart `crds/` file is a
byte-for-byte mirror of the corresponding base, and `make verify-crd-sync`
leaves the working tree clean.

It is repeatable — run it any time, especially after touching a type
file or a marker, before tagging a release.

## What CRD drift means here

Each operator owns one or more CRDs, and each CRD threads through three
layers — Go source, generator output, Helm chart copy. Drift can land
in any one of them:

| Layer | Where it lives | Source of truth |
|---|---|---|
| Go +kubebuilder source | `operators/<op>/api/v1alpha1/*_types.go` | the `// +kubebuilder:*` markers, struct fields, JSON tags |
| Generated CRD YAML | `operators/<op>/config/crd/bases/*.yaml` | `make manifests` output (controller-gen) |
| Helm chart copy | `operators/<op>/helm/<op>-operator/crds/*.yaml` | `make sync-crds` output — a comment-prefixed mirror of the base |
| DeepCopy stubs | `operators/<op>/api/v1alpha1/zz_generated.deepcopy.go` | `make generate-common` output (controller-gen object) |

The authoritative drift gate is `make verify-crd-sync` (and, behind it,
`make sync-crds` ⇢ `make manifests`). It strips the cross-reference
comment header (`^#` lines) from each `helm/.../crds/*.yaml` and diffs
the result against the corresponding `config/crd/bases/` file; any
non-comment delta fails the build. This skill defers to that gate as
the source of truth and adds the inventories and orphan checks the
gate cannot express on its own.

A drift finding is any place a generated artefact and its source
disagree, or a CRD is present that no live operator claims.

## Procedure

Work through these steps in order and report findings at the end.

### 1. Run the deterministic audit

```bash
bash .claude/skills/check-crd-drift/scripts/audit-crd-drift.sh
```

The script catches the mechanically-checkable gaps and prints an
inventory. Exit code `1` means at least one `[FAIL]`. Interpret:

- **C1** — every `operators/<op>/` with an `api/` directory has a
  matching `config/crd/bases/` directory and at least one CRD YAML
  inside it. A missing base means `make manifests` was never run for
  this operator, or the chart was added without wiring controller-gen.
- **C2** — every `config/crd/bases/*.yaml` for an operator has a
  byte-equivalent copy under `helm/<op>-operator/crds/` (after
  stripping leading `#` comment lines, matching the gate). A missing
  or differing copy means `make sync-crds` was skipped after the last
  marker edit.
- **C3** — every `helm/<op>-operator/crds/*.yaml` traces back to a
  `config/crd/bases/` file. An orphan copy is a CRD that controller-gen
  no longer emits but the chart still ships — the next install will
  register a CRD with no controller behind it.
- **C4** — every `*_types.go` with a `+kubebuilder:object:root=true`
  marker has a corresponding generated DeepCopy block in
  `zz_generated.deepcopy.go`. A missing block means `make generate-common`
  was skipped — the operator will fail to build.
- The **inventory** lists, per operator, every CRD with its
  `spec.versions[].name`, its kubebuilder printer columns, and the size
  delta between the base and the helm copy in bytes (after stripping
  comments). Cross-reference each by hand in step 2.

### 2. Cross-reference the inventory

The script cannot run controller-gen (it would require the toolchain on
`PATH`). Using the printed inventory, confirm:

1. Every CRD listed under `config/crd/bases/` corresponds to a Go type
   in the same operator's `api/v1alpha1/` with a matching `kind`.
2. Every `+kubebuilder:printcolumn` in the Go source is present as a
   `additionalPrinterColumns` entry in the generated CRD.
3. Every `+kubebuilder:validation:XValidation` rule in the Go source
   shows up as a CEL `x-kubernetes-validations` entry in the CRD.
4. No two operators define the same CRD `metadata.name` — chart
   installs would overwrite each other.

Flag any pair where the source and the generated YAML disagree.

### 3. Run the authoritative drift gate

The script does no regeneration. Run the real gates and report the
exact outcomes:

```bash
make manifests          # regenerate CRD bases from kubebuilder markers
make sync-crds          # copy bases into the Helm chart crds/ dir
make verify-crd-sync    # authoritative diff gate (also runs in CI)
```

`make verify-crd-sync` is the authoritative gate — trust it over the
C1–C4 smoke checks. It is fast (a `find` + `diff` over a handful of
files) but does require `controller-gen` on `PATH` for `make manifests`.
Pass `--full` to the script to chain `make verify-crd-sync` after the
inventory.

### 4. Report

Produce a concise summary grouped by severity:

- **HIGH** — `make verify-crd-sync` fails, or a Helm `crds/` copy and
  its base disagree on a non-comment line, or a `*_types.go` exists
  without a generated CRD base.
- **MEDIUM** — an orphan helm `crds/*.yaml` with no live base; a
  printcolumn or XValidation in the source that is absent from the
  generated CRD; a CRD listed under one operator's `config/crd/bases/`
  whose Go type lives in another operator.
- **LOW** — a comment-only delta between base and helm copy (the gate
  strips these — informational only); a missing kubebuilder printer
  column on a CRD that has none documented either.

For each finding give one line with a `file:line` reference for both
the source side and the generated side. End with a two- to three-sentence
health verdict per operator. The C1–C4 results and the step-3 gate
outcome go in the report verbatim.

## Drift patterns

These recurring shapes are worth grepping for first:

1. **Stale Helm chart copy.** A marker edit hit `*_types.go`, `make
   manifests` ran (so `config/crd/bases/` is fresh), but `make
   sync-crds` was skipped. The chart still ships the previous schema.
   `make verify-crd-sync` catches this by definition.
2. **Hand-edit on a generated CRD.** A YAML change on top of a
   marker-clean HEAD that the next `make manifests` would silently
   overwrite. Often surfaces as a `+kubebuilder` marker that the source
   does not actually express.
3. **Orphan CRD in the Helm chart.** A type was renamed or deleted in
   Go but the corresponding `helm/<op>-operator/crds/*.yaml` was not
   removed. `helm install` registers the dead CRD, and the cluster
   has a kind no controller reconciles.
4. **DeepCopy drift.** A new struct field landed in `*_types.go`
   without `make generate-common` — the build fails before the CRD
   gate ever runs.
5. **Cross-operator CRD ownership.** A CRD `kind` ended up under the
   wrong operator's `config/crd/bases/` (e.g. duplicated during a
   refactor). Both operators' Helm charts then ship it, and `helm
   install` order decides which schema wins.

## Reference — regenerating after a marker change

When a marker or struct field changes, the checklist is:

1. **Edit the Go source.** Pick `operators/<op>/api/v1alpha1/*_types.go`.
2. **Regenerate.** `make manifests` rebuilds `config/crd/bases/*.yaml`;
   `make generate-common` rebuilds `zz_generated.deepcopy.go`;
   `make sync-crds` mirrors the bases into the Helm chart crds/ dir
   (and prepends the cross-reference comment header).
3. **Verify drift gate clean.** `make verify-crd-sync` must exit `0` —
   it is the same check CI runs.
4. **Commit all sides together.** The source change, the regenerated
   bases, the regenerated deepcopy, and the Helm copies go in the same
   commit so a future `git bisect` always sees a self-consistent tree.

## Notes

- This skill is read-only; the deterministic script edits nothing.
  Apply fixes (`make manifests && make sync-crds`, delete the orphan)
  as a separate, explicitly-scoped task.
- The audit script does not assume `controller-gen` is on `PATH` — it
  defers regeneration to `make verify-crd-sync`. The `--full` flag
  chains that gate after the inventory; without it the script is a
  fast smoke check that runs cleanly on a laptop without the kubebuilder
  toolchain installed.
- Pair this with [[check-fixture-drift]] — that skill confirms each
  CR fixture under `tests/e2e/` still validates against the *current*
  CRD schema; this skill confirms each CRD still mirrors its Go source.
