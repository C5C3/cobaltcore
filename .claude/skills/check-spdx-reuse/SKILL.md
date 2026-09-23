---
name: check-spdx-reuse
description: >-
  Audit SPDX / REUSE compliance across the CobaltCore source tree — every
  *.go, *.sh, hand-authored YAML, and CI workflow file should carry
  matching SPDX-FileCopyrightText and SPDX-License-Identifier headers,
  and every license referenced in a header has a corresponding text
  under LICENSES/. Use when
  asked to check SPDX or REUSE coverage, after adding a new source
  file, or before tagging a release where SAP supply-chain audits
  require clean REUSE output.
---

# Check SPDX / REUSE coverage

This skill verifies that the CobaltCore source tree keeps its **SPDX
headers**: every file class that carries a copyright/licence header by
convention has one, and every header references a licence that ships
under `LICENSES/`.

It is repeatable — run it any time, especially after adding a new file
type or directory, or before cutting a release.

## What SPDX / REUSE means here

The headers follow the REUSE specification (`https://reuse.software`),
but only inline: the repo has no `REUSE.toml` and no `.reuse/dep5`, so
no file is covered by an annotation. Two things carry the licence
statement:

| Mechanism | Where it applies | Source of truth |
|---|---|---|
| Inline header | Every hand-authored `*.go`, `*.sh`, `*.py`, `Dockerfile`, `Makefile`, and `*.yaml` outside Helm charts and controller-gen output | Two comment lines: `SPDX-FileCopyrightText: …` and `SPDX-License-Identifier: …` |
| Licence text inventory | Every licence identifier used anywhere | A matching file under `LICENSES/` (only `Apache-2.0.txt` today) |

Three file classes carry no inline header by convention, and the script
does not fail them:

- **Generated YAML** — `make manifests` regenerates the CRDs under
  `operators/<op>/config/crd/bases/`, `config/rbac/role.yaml` and
  `config/webhook/manifests.yaml` without a comment block. The Helm CRD
  copies under `helm/<op>-operator/crds/` start with the `# SOURCE:`
  cross-reference `make sync-crds` prepends, not with SPDX.
- **Helm charts** — every chart (the ten operator charts, the two
  `operators/shared/helm/` library charts, and
  `deploy/target-cluster/target-cluster-access/`) ships its YAML
  header-less; `operators/neutron/helm/neutron-operator/tests/role_test.yaml`
  and `operators/ovn/helm/ovn-operator/tests/role_test.yaml` are the only
  exceptions.
- **Docs** — the `docs/` corpus is outside the script's scope and
  mixed: most guides, the quick-starts and `docs/contributing/` carry
  the pair in an HTML comment below the frontmatter, most
  `docs/reference/` pages carry none.

**No authoritative gate exists today.** No CI workflow runs `reuse
lint`, and nothing else checks headers tree-wide; a handful of shell
unit tests pin the header on the files they own
(`tests/unit/deploy/nfs_overlay_test.sh`, `metrics_server_overlay_test.sh`,
`chaos_mesh_overlay_test.sh`, `dizzy_overlay_test.sh`). The S1–S5
checks below are the only mechanised check. `reuse lint` would fail on
the three header-less classes above (and on JSON files such as
`values.schema.json` and the option catalogs, `*.tpl` helpers, lock
files) until a `REUSE.toml` annotates them — adopting it is a
separate decision, not a step of this audit.

A coverage finding is a file in a header-carrying class without the
pair, a header that references a missing licence text, or a licence
text in `LICENSES/` that no file references.

## Procedure

Work through these steps in order and report findings at the end.

### 1. Run the deterministic audit

```bash
bash .claude/skills/check-spdx-reuse/scripts/audit-spdx-reuse.sh
```

The script catches the mechanically-checkable gaps and prints an
inventory. Exit code `1` means at least one `[FAIL]`. Interpret:

- **S1** — every hand-authored `*.go` under `operators/`, `internal/`,
  `tests/` has both `SPDX-FileCopyrightText` and `SPDX-License-Identifier`
  within its first 20 lines. Generated files (`zz_generated.*.go`, or a
  `// Code generated … DO NOT EDIT.` banner in the first 10 lines) are
  exempt; controller-gen's `zz_generated.deepcopy.go` carries the pair
  anyway, below its `//go:build` line.
- **S2** — every `*.sh` under `hack/`, `scripts/`, `tests/scripts/`,
  `tests/unit/`, `tests/lib/` has both headers.
- **S3** — every hand-authored YAML / TOML under `deploy/`,
  `operators/<op>/config/`, `releases/`, `.github/` has both headers.
  Two classes are exempt and counted in `[INFO]` lines instead:
  controller-gen output, recognised by path **and** by the shape
  controller-gen writes (no comment block; `---`, then apiVersion and
  kind): the CRDs under `config/crd/`, `config/rbac/role.yaml` (a
  `ClusterRole` named `<op>-operator`, from `rbac:roleName`) and
  `config/webhook/manifests.yaml` (a `{Mutating,Validating}WebhookConfiguration`),
  so a hand-written file dropped beside them is still checked; and
  files inside a Helm chart (an ancestor directory holds `Chart.yaml`),
  listed per chart as "no inline header by repo convention".
- **S4** — every licence identifier appearing in any `SPDX-License-Identifier:`
  header has a matching `<id>.txt` under `LICENSES/`.
- **S5** — every `<id>.txt` under `LICENSES/` is referenced by at
  least one file (no unused licence inventory); an unused text is an
  `[INFO]`.
- The **inventory** lists, per file class, the number of files scanned
  and the number missing a header.

### 2. Cross-reference the inventory

The script applies a coarse "first 20 lines" header check; the REUSE
rules are subtler (multi-line comments, `<!-- … -->` HTML, etc.). For
each `[FAIL]`, confirm by hand:

1. The flagged file is genuinely hand-authored (not a vendored copy,
   not generated, not a binary blob). A new generator whose output
   lands in a scanned directory belongs in `is_generated_yaml` /
   `is_generated_go` with a path-plus-shape test, not in a blanket
   directory skip.
2. The fix is to add the SPDX header inline (copy the comment lines
   from a sibling file of the same type).
3. For S4 failures, add the missing licence text to `LICENSES/`
   (`reuse download <SPDX-ID>` fetches the canonical text).
4. For the Helm chart `[INFO]` line, confirm the directory really is a
   chart (a `Chart.yaml` beside `templates/`); a stray `Chart.yaml`
   would silence S3 for its whole subtree.

### 3. Optional: run `reuse lint`

No CI job runs it and the repo carries no `REUSE.toml`, so `reuse lint`
fails today on the header-less classes (generated YAML, Helm charts,
most `docs/reference/` pages, JSON, `*.tpl`, lock files). Run it only
to see what an adoption would have to annotate:

```bash
pipx install reuse  # one-time
reuse lint
```

Report its outcome as an inventory, not as a verdict: until a
`REUSE.toml` exists, S1–S5 are the checks the repo holds itself to.

### 4. Report

Produce a concise summary grouped by severity:

- **HIGH** — an SPDX header references a licence that ships no text
  under `LICENSES/` (S4).
- **MEDIUM** — a hand-authored file in a header-carrying class with no
  SPDX header (S1–S3); a `LICENSES/<id>.txt` referenced by zero files
  (S5).
- **LOW** — a header outside the first 20 lines the script scans; an
  exemption that no longer fits (a generator whose output changed
  shape, a directory with a stray `Chart.yaml`).

For each finding give one line with a `file:line` reference. End with
a two- to three-sentence verdict per file type (Go / shell / YAML).

## Coverage patterns

These recurring shapes are worth grepping for first:

1. **New Go file, missing SPDX header.** A new `reconcile_<thing>.go`
   was added by hand without copying the header from a sibling. The
   build passes; only S1 flags it.
2. **New licence used, no text shipped.** A vendored dependency was
   added with an unusual SPDX-License-Identifier (e.g. `MPL-2.0`)
   without the corresponding `LICENSES/MPL-2.0.txt`. S4 flags it.
3. **Helm CRD copies are not SPDX-headed.** The `helm/<op>-operator/crds/`
   files are copies of `config/crd/bases/` with a three-line
   `# SOURCE: …` cross-reference that `make sync-crds` prepends — no
   SPDX pair. They sit inside a chart, outside S3's scan, and follow
   the header-less chart convention.
4. **Generated file loses its header.** A generator's output template
   changes and no longer emits the SPDX boilerplate. The script's
   generated-file exemption hides it from S1, and no other check
   covers it; diff the generator output by hand when a generator is
   bumped.
5. **Generated file mistaken for hand-authored.** A new controller-gen
   output (a second RBAC role, a new webhook manifest path) lands in
   a scanned directory without a header and S3 fails it. Extend
   `is_generated_yaml` with the path and the first-document shape;
   do not skip the directory.

## Notes

- This skill is read-only; the deterministic script edits nothing.
  Apply fixes (add the header, fetch the licence
  text) as a separate, explicitly-scoped task.
- The S1–S3 checks are coarse heuristics, and they are the only
  tree-wide mechanised SPDX check the repo has; nothing in CI runs
  them either. Run the audit before a release.
- Generated files are exempted in the script by detecting either
  `zz_generated.` in the file name or a `// Code generated … DO NOT
  EDIT.` banner in the first 10 lines (Go), and by path plus
  first-document shape (YAML). If a new generator's output pattern
  differs, extend the exemption functions at the top of the script.
