---
name: check-spdx-reuse
description: >-
  Audit SPDX / REUSE compliance across the forge source tree — every
  *.go, *.sh, hand-authored YAML, and CI workflow file should carry
  matching SPDX-FileCopyrightText and SPDX-License-Identifier headers,
  every license referenced in a header has a corresponding text under
  LICENSES/, and architecture/REUSE.toml stays well-formed. Use when
  asked to check SPDX or REUSE coverage, after adding a new source
  file, or before tagging a release where SAP supply-chain audits
  require clean REUSE output.
---

# Check SPDX / REUSE coverage

This skill verifies that the forge source tree stays **REUSE-compliant**:
every file that needs a copyright/licence header has one, every header
references a licence that ships under `LICENSES/`, and the
`architecture/REUSE.toml` rules cover the prose corpus that lives
without inline headers.

It is repeatable — run it any time, especially after adding a new file
type or directory, or before cutting a release.

## What SPDX / REUSE means here

The repo follows the REUSE specification (`https://reuse.software`).
There are two ways a file can carry a licence statement:

| Mechanism | Where it applies | Source of truth |
|---|---|---|
| Inline header | Every hand-authored `*.go`, `*.sh`, `*.py`, `Dockerfile`, `Makefile`, `*.yaml` (excluding controller-gen / sqlc output and Helm chart copies) | Two adjacent comment lines: `SPDX-FileCopyrightText: …` and `SPDX-License-Identifier: …` |
| `REUSE.toml` annotation | The `docs/` prose corpus, vendored chart copies, and a few well-known asset paths | `architecture/REUSE.toml` (top-level `[[annotations]]` blocks with `path` globs) |
| Licence text inventory | Every licence identifier used anywhere | A matching file under `LICENSES/` (e.g. `Apache-2.0.txt`) |

The authoritative gate is `reuse lint` from the
[reuse-tool](https://github.com/fsfe/reuse-tool) Python package. This
skill defers to that gate as the source of truth and adds the
inventory checks the lint output is verbose about.

A coverage finding is any file without a header that the REUSE.toml
also does not cover, a header that references a missing licence text,
or a licence text in `LICENSES/` that no file references.

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
  headers in the first 5 lines. Generated files
  (`zz_generated.*.go`, `// Code generated … DO NOT EDIT.` banner)
  are exempted from the inline check — they are expected to be
  REUSE.toml-covered.
- **S2** — every `*.sh` under `hack/`, `scripts/`, `tests/scripts/`,
  `tests/unit/`, `tests/lib/` has both headers.
- **S3** — every hand-authored YAML / TOML under `deploy/`,
  `operators/<op>/config/`, `releases/` has both headers (controller-gen
  CRD output is exempted — its first line is the CRD apiVersion).
- **S4** — every licence identifier appearing in any `SPDX-License-Identifier:`
  header has a matching `<id>.txt` under `LICENSES/`.
- **S5** — every `<id>.txt` under `LICENSES/` is referenced by at
  least one file (no unused licence inventory).
- **S6** — `architecture/REUSE.toml` is valid TOML (parses with
  `python3 -c 'import tomllib; tomllib.load(open(…, "rb"))'`).
- The **inventory** lists, per check, the number of files scanned,
  the number that passed, and the per-extension breakdown of
  failures.

### 2. Cross-reference the inventory

The script applies a coarse "first 5 lines" header check; the real
REUSE rules are subtler (multi-line comments, `<!-- … -->` HTML, etc.).
For each `[FAIL]`, confirm by hand:

1. The flagged file is genuinely hand-authored (not a vendored copy,
   not generated, not a binary blob).
2. The fix is either (a) add the SPDX header inline, or (b) add a
   matching `[[annotations]]` block to `architecture/REUSE.toml` that
   covers the path.
3. For S4 failures, add the missing licence text to `LICENSES/`
   (most often by running `reuse download <SPDX-ID>` to fetch the
   canonical text).

### 3. Run the authoritative gate

The script defers to `reuse lint`. Run it explicitly and report the
outcome:

```bash
# Requires the reuse-tool Python package
pipx install reuse  # one-time
reuse lint
```

`reuse lint` is the authoritative gate — trust it over the S1–S6
smoke checks when they disagree. It understands every comment style
the spec recognises and walks the full file tree.

### 4. Report

Produce a concise summary grouped by severity:

- **HIGH** — `reuse lint` fails; an SPDX header references a licence
  that ships no text under `LICENSES/`; a `REUSE.toml` parse error.
- **MEDIUM** — a hand-authored source file with no SPDX header that
  is also not covered by `REUSE.toml`; a `LICENSES/<id>.txt` referenced
  by zero files (unused licence inventory).
- **LOW** — a header on the wrong line (S1–S3 expect first-five-lines
  placement but the spec is more flexible); a `[[annotations]]` block
  whose `path` glob does not match any current file (over-broad cover).

For each finding give one line with a `file:line` reference. End with
a two- to three-sentence verdict per file type (Go / shell / YAML).

## Coverage patterns

These recurring shapes are worth grepping for first:

1. **New Go file, missing SPDX header.** A new `reconcile_<thing>.go`
   was added by hand without copying the header from a sibling. The
   build passes; only `reuse lint` flags it.
2. **New licence used, no text shipped.** A vendored dependency was
   added with an unusual SPDX-License-Identifier (e.g. `MPL-2.0`)
   without the corresponding `LICENSES/MPL-2.0.txt`. The next REUSE
   audit fails.
3. **Helm chart copy missing exemption.** The `helm/<op>-operator/crds/`
   files are byte-mirrors of `config/crd/bases/`. They carry SPDX in
   comment form because `sync-crds` prepends a `#`-comment block; if
   that step is skipped, the copies fail S3.
4. **REUSE.toml over-broad path glob.** A `[[annotations]]` block
   uses `**` and accidentally covers a file that *should* carry its
   own inline header. The lint silently passes; the file's
   provenance is wrong.
5. **Generated file picks up an inline header.** A controller-gen
   regeneration overwrites a file that previously carried a manual
   header. Counter-intuitively, the regeneration removes the SPDX
   line because the generator's template does not include it. The
   REUSE.toml exemption is what is supposed to cover it; check the
   path glob still matches after the rename.

## Notes

- This skill is read-only; the deterministic script edits nothing.
  Apply fixes (add the header, extend REUSE.toml, fetch the licence
  text) as a separate, explicitly-scoped task.
- The S1–S3 checks are coarse heuristics. `reuse lint` is the
  authoritative source for compliance and is what the SAP supply-chain
  audit consumes. The audit script's job is to triage findings the
  lint reports in machine-friendly form.
- Generated files are exempted in the script by detecting either
  `zz_generated.` in the file name or a `// Code generated … DO NOT
  EDIT.` banner in the first 10 lines. If a new generator's output
  pattern differs, extend the exemption list at the top of the script.
