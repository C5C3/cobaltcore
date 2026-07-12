---
name: prepare-new-guide
description: >-
  Scaffold and validate a how-to guide under docs/guides/ against the guide
  conventions — one devstack per guide, ControlPlane-first naming, no
  projected-child edits without a revert warning, raw helm framed against
  Flux ownership, a Tested by suite reference, code-import for mirrored
  fixtures. Use when asked to write a new guide, scaffold a guide skeleton,
  or check a draft guide for convention violations.
---

# Prepare a new guide

This skill makes a new guide under `docs/guides/` **conform by
construction** and turns convention review into a **check**. The contract
it applies is `docs/contributing/guide-conventions.md` — the standardized
prerequisites block, ControlPlane-first naming, the projected-children
rule, the terminal `## Tested by` section, and the code-import discipline.
Encode the conventions by content; guides never carry internal feature or
requirement IDs, or issue references.

## Scaffold a new guide

### 1. Settle the devstack anchor with the user

Every guide declares exactly one devstack. Default to the ControlPlane
Quick Start (`quick-start-controlplane`) — the ControlPlane-first rule —
and fall back to a standalone devstack only when the knob is
standalone-only or the guide targets the operator chart itself. Collect
every `WITH_*` opt-in the guide depends on (Prometheus, Chaos Mesh, ...);
opt-in flags compose onto the bring-up command.

### 2. Generate the skeleton

```bash
bash .claude/skills/prepare-new-guide/scripts/scaffold-guide.sh \
  <slug> --devstack quick-start-controlplane \
  --title "<Title>" \
  --opt-in WITH_PROMETHEUS=true \
  --suite tests/e2e/keystone/<suite> \
  > docs/guides/<slug>.md
```

The script prints the skeleton to stdout and never writes to the tree —
you perform the write. The skeleton carries the `::: info Devstack`
container with the devstack's verbatim bring-up command, the section
structure (`## Steps`, `## Verification`, `## See also`), and the terminal
`## Tested by` fence; the ControlPlane variant additionally carries the
operator-owned `::: warning` block and a
`## Standalone Keystone, without a ControlPlane` stub. Until the
tested-by suite path resolves to a real `chainsaw-test.yaml`, the
skeleton fails the docs gate — that is intended, not a scaffold bug.

### 3. Register the guide in the sidebar

Add a `{ text: '<Title>', link: '/guides/<slug>' }` entry to the `Guides`
sidebar section in `docs/.vitepress/config.ts`.

### 4. Author the body with the devstack's real names

Use only the resource names the declared devstack actually produces — the
table in `docs/contributing/guide-conventions.md` lists them per devstack.
Standalone differences go in the terminal
`## Standalone Keystone, without a ControlPlane` section (modeled on
`docs/guides/end-to-end-sso.md`); never interleave the two naming worlds
in the primary walkthrough.

### 5. Wire the Tested by section

Point `## Tested by` at the mirroring suite (`chainsaw test --test-dir
tests/e2e/keystone/<suite>`, one line per suite) and embed mirrored
fixtures via `<<< @/../tests/e2e/...#<region>` imports instead of
hand-maintained copies. The walkthrough YAML stays devstack-named; the
imported fixture is isolation-named and lives as a labelled `::: details`
exhibit inside `## Tested by`, prefaced by a sentence that states which
names the exhibit uses and why.

## Validate a draft guide

### 1. Run the deterministic checks

```bash
bash .claude/skills/prepare-new-guide/scripts/validate-guide.sh [<guide.md>...]
```

Without arguments it walks every `docs/guides/*.md`. It checks what the
docs gate does not:

- **V1** — banned placeholder names (`keystone-default`).
- **V2** — the devstack link and the `WITH_CONTROLPLANE=true` flag agree
  inside the `::: info Devstack` container.
- **V3** — a bash fence running raw `helm upgrade`/`helm install` against
  the published operator chart (`oci://ghcr.io/c5c3/charts/`) requires a
  `HelmRelease` mention (Flux-ownership framing).
- **V4** — a mutating kubectl verb on a projected child CR
  (`controlplane-keystone` / `controlplane-horizon`) requires a
  `::: warning` container with revert/projected language.

### 2. Run the authoritative gate

```bash
bash tests/unit/docs/guide_devstack_and_tested_by_test.sh
```

or `validate-guide.sh --full`, which chains it. The gate owns the
structural half of the conventions — the `## Prerequisites` heading, the
`::: info Devstack` container with exactly one tutorial link and a bash
bring-up fence, the `## Tested by` heading with resolvable `--test-dir`
suites, and resolvable code-imports with balanced region markers. Trust
the gate's verdict over the script's.

### 3. Walk the prose checklist

What the scripts cannot decide:

- Every example name exists on the declared devstack — cross-reference the
  names table in `docs/contributing/guide-conventions.md`.
- Revert warnings sit adjacent to the operations they cover, not in a
  distant section.
- Standalone differences live in a terminal
  `## Standalone Keystone, without a ControlPlane` section with no
  interleaved naming worlds.
- Walkthrough YAML is devstack-named; the imported fixture exhibit is
  isolation-named and labelled as such.
- Frontmatter (`title`, `quadrant: operator`) starts on line 1.
- The guide is registered in the `Guides` sidebar in
  `docs/.vitepress/config.ts`.

## Findings report

Group findings by severity, with a `file:line` reference on both the
guide side and the convention side:

- **HIGH** — the reader's cluster cannot execute the guide: a placeholder
  name no tutorial produces, a devstack/flag mismatch, a projected-child
  edit with no revert warning.
- **MEDIUM** — drift and revert traps: raw helm without Flux-ownership
  framing, interleaved naming worlds, a hand-maintained fixture copy where
  a code-import belongs.
- **LOW** — housekeeping: missing sidebar registration, frontmatter not on
  line 1, a `## Tested by` invocation not in `--test-dir` form.

## Known gotchas (verified 2026-07, re-verify at HEAD)

- **Raw helm against the OCI chart is not itself a violation** — it is the
  canonical non-kind path for the operator charts. The violation is raw
  helm with no Flux-ownership framing; the model is the `::: tip On kind`
  block in `docs/guides/enable-keystone-operator-metrics.md`, which frames
  the raw-helm sections as the non-kind path and routes kind users through
  the HelmRelease.
- **Not every `controlplane-keystone-*` mutation is a projected-CR edit** —
  Secrets and ExternalSecrets named with that prefix (rotation Secrets,
  admin-credential ExternalSecrets) are legitimately user-mutable; V4 keys
  on the resource kind adjacent to the child name for exactly this reason.
- **The docs gate is structural only** — it checks headings, containers,
  and paths, and makes no assertions on prose. A guide can pass the gate
  and still violate the naming or projected-children conventions.
- **A multi-line `kubectl apply` that overwrites a projected child is not
  mechanically detectable** — V4 matches single-line mutating verbs; the
  prose checklist owns the apply-a-manifest case.

## Notes

- The scripts are read-only with respect to the tree: `scaffold-guide.sh`
  prints the skeleton to stdout (the caller writes the file), and
  `validate-guide.sh` only reads and reports.
- The per-devstack bring-up commands and name sets are sourced from the
  devstack table in `docs/contributing/guide-conventions.md`; the scripts
  carry a machine copy that must stay in sync with it (both scripts say so
  in a header note).
- Pair with [[check-doc-drift]] after guide edits — it audits the wider
  docs surface against the implementation.
