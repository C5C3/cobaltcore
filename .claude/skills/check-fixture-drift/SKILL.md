---
name: check-fixture-drift
description: >-
  Audit whether every Keystone / c5c3 CR fixture under tests/e2e/ and
  tests/e2e-chaos/ still validates against the *current* CRD schema —
  no removed Spec field is still referenced, every fixture's
  apiVersion / kind matches a real CRD, every invalid-cr fixture is
  reachable from a Chainsaw test, and the existing
  verify-invalid-cr-fixtures generator stays in sync with its hand-edited
  outputs. Use when asked to check fixture drift, after editing the
  Keystone CRD or the validating webhook, or before a release.
---

# Check fixture drift

This skill verifies that the forge **test fixtures still match the CRD
they claim to instantiate**: every `apiVersion: keystone.openstack.c5c3.io/v1alpha1`
fixture under `tests/e2e/` and `tests/e2e-chaos/` uses only Spec fields
that the current CRD schema accepts, every invalid-cr fixture is
referenced from a Chainsaw test that exercises it, and the
`verify-invalid-cr-fixtures` generator's outputs stay in sync.

It is repeatable — run it any time, especially after editing
`operators/keystone/api/v1alpha1/keystone_types.go`, the validating
webhook, or any `kubebuilder:validation:*` marker.

## What fixture drift means here

A fixture lives in one of three roles, each anchored to its own source
of truth:

| Fixture role | Where it lives | Source of truth |
|---|---|---|
| Happy-path e2e CR | `tests/e2e/keystone/<scenario>/*.yaml` (filename pattern `<NN>-*.yaml`, referenced from `chainsaw-test.yaml`) | the Keystone CRD `operators/keystone/config/crd/bases/keystone.openstack.c5c3.io_keystones.yaml` and the webhook in `operators/keystone/internal/controller/` |
| Chaos / scale e2e CR | `tests/e2e-chaos/<scenario>/*.yaml`, `tests/e2e/infrastructure/*` | same CRD |
| Invalid-CR webhook reject | `tests/e2e/keystone/invalid-cr/<NN>-*.yaml` (paired with the Python generator `_generate.py` and the unit test `test_generate.py`) | `keystone_types.go` `+kubebuilder:validation:*` markers and the webhook validation logic |
| Chainsaw test wiring | `tests/e2e/<area>/<scenario>/chainsaw-test.yaml` | references the local `<NN>-*.yaml` files by relative path |

The authoritative gate for the invalid-cr corpus is
`make verify-invalid-cr-fixtures` — it re-runs the Python generator in
`--check` mode plus the generator's own unit tests. This skill defers
to that gate and adds the broader fixture inventory checks the gate
does not cover.

A drift finding is any place a fixture references a Spec field that
the current CRD does not declare, a fixture file that no Chainsaw test
references, or a Chainsaw test that points at a fixture that has been
moved or renamed.

## Procedure

Work through these steps in order and report findings at the end.

### 1. Run the deterministic audit

```bash
bash .claude/skills/check-fixture-drift/scripts/audit-fixture-drift.sh
```

The script catches the mechanically-checkable gaps and prints an
inventory. Exit code `1` means at least one `[FAIL]`. Interpret:

- **X1** — every fixture with `kind: Keystone` uses the current
  apiVersion (`keystone.openstack.c5c3.io/v1alpha1`). A fixture on
  an old apiVersion fails to apply but the failure is generic.
- **X2** — every top-level Spec field in every `kind: Keystone`
  fixture appears in the CRD schema. A removed/renamed field is the
  most common fixture-drift source after a CRD edit; the cluster
  rejects the CR with `unknown field`, which is hard to diff against
  the original intent.
- **X3** — every `<NN>-*.yaml` fixture next to a `chainsaw-test.yaml`
  is referenced from that Chainsaw test (via `apply`, `assert`,
  `error`, or `patch`). An orphan fixture is a dead test artefact —
  the scenario it was written for was removed but the file stayed.
- **X4** — every file referenced from a `chainsaw-test.yaml` exists
  on disk under the same directory. A renamed fixture leaves a
  Chainsaw step pointing at nothing.
- **X5** — the invalid-cr generator + unit tests still pass when
  invoked as `make verify-invalid-cr-fixtures` (gated check; skipped
  if `python3` is not on PATH).
- The **inventories** are review aids: per Chainsaw test directory,
  the count of `<NN>-*.yaml` fixtures vs `chainsaw-test.yaml` step
  references; per invalid-cr fixture, the matching `+kubebuilder`
  marker the rejection should reference.

### 2. Cross-reference the inventory

The script does no schema validation against the live cluster. Using
the printed inventory, confirm:

1. For each Spec field that X2 flagged, decide: was the field renamed
   (update the fixture), removed (delete the fixture lines), or
   moved into a sub-struct (re-indent the fixture)?
2. For each orphan fixture from X3, confirm whether the scenario was
   intentionally removed (delete the YAML) or whether the
   `chainsaw-test.yaml` lost a step (restore the reference).
3. For each invalid-cr fixture, walk the corresponding webhook
   rejection path to confirm the error message the Chainsaw test
   asserts still matches the webhook output. The Python generator
   carries the expected error string in its source — cross-check.
4. For server-side validation, optionally run a dry-run apply against
   a kind cluster:
   ```bash
   for f in $(find tests/e2e/keystone -name '*keystone*.yaml' | xargs grep -l '^kind: Keystone'); do
     kubectl apply --dry-run=server -f "${f}" 2>&1 | tail -1
   done
   ```
   This requires `kubectl` configured against a cluster with the
   Keystone CRD installed.

### 3. Run the authoritative gate

The script defers to the existing make target for the invalid-cr
corpus. Run it explicitly and report the outcome:

```bash
make verify-invalid-cr-fixtures
```

This re-runs `_generate.py --check` and `test_generate.py` and fails
on any divergence between the generator and the committed YAML files.

### 4. Report

Produce a concise summary grouped by severity:

- **HIGH** — a Spec field referenced in a fixture is absent from the
  current CRD; a Chainsaw step references a file that does not exist;
  `make verify-invalid-cr-fixtures` fails.
- **MEDIUM** — an orphan fixture under a `chainsaw-test.yaml`
  directory; an invalid-cr fixture whose expected-error string no
  longer matches the webhook output; a fixture using an older
  apiVersion than the CRD currently serves.
- **LOW** — formatting drift inside fixtures (inconsistent
  indentation, comment-style differences between siblings); a
  scenario directory whose name suggests a different focus than its
  fixtures actually exercise.

For each finding give one line with a `file:line` reference for both
the fixture side and the CRD-source side. End with a verdict per
fixture role.

## Drift patterns

These recurring shapes are worth grepping for first:

1. **Removed Spec field still referenced.** A `+kubebuilder` field
   was renamed in `keystone_types.go`; the CRD regenerated; the
   fixtures still use the old name. `kubectl apply --dry-run=server`
   would catch this but is not in the existing CI surface.
2. **Orphan fixture under a test directory.** A scenario was reduced
   from 5 steps to 3 but the now-unused `04-*.yaml` and `05-*.yaml`
   files were not deleted. The Chainsaw test still passes; the dead
   files outlast their purpose.
3. **Renamed test directory, stale Chainsaw reference.** A directory
   was renamed (e.g. `basic-deployment` ⇢ `basic-deployment-2026-1`)
   and the new `chainsaw-test.yaml` still references files via the
   old relative path.
4. **Webhook rejection string drift.** The webhook reject message
   was reworded; the invalid-cr Chainsaw assertion still matches on
   the old string. The test fails with an obscure mismatch instead
   of confirming the new error path.
5. **apiVersion lag.** The CRD declares `v1alpha1` and `v1beta1` as
   served, but the fixtures still use `v1alpha1` exclusively. Either
   the fixtures need an update (after the bump) or the served list
   needs the older version dropped.

## Notes

- This skill is read-only; the deterministic script edits nothing.
  Apply fixes (rename the fixture field, delete the orphan, update
  the Chainsaw step) as a separate, explicitly-scoped task.
- Server-side validation is intentionally not in the script — it
  requires a live cluster. The X1–X4 checks are the lightweight
  smoke; X5 wraps the existing make-target gate.
- Pair this with [[check-crd-drift]] — that skill confirms the CRD
  YAML mirrors the Go source; this skill confirms the fixtures
  exercise that CRD correctly.
- Pair this with [[check-condition-coverage]] — that skill confirms
  every condition is set by some sub-reconciler; the e2e fixtures
  exercise those conditions and the Chainsaw assertions verify they
  reach the expected status.
