---
title: Claude Code Skills
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Claude Code skills

This repository ships a suite of repository-specific
[Claude Code](https://docs.claude.com/en/docs/claude-code) skills under
[`.claude/skills/`](https://github.com/c5c3/forge/tree/main/.claude/skills).
Each skill is a focused **audit** that compares one surface of the
codebase against its source of truth — the Keystone CRD, the
sub-reconciler chain, the FluxCD infrastructure stack, the Renovate
configuration, the e2e fixture corpus, the SPDX/REUSE coverage, the Go
workspace — and reports drift before it reaches a release.

The skills complement the CI gates configured in
[`.github/workflows/`](https://github.com/c5c3/forge/tree/main/.github/workflows)
and the Makefile drift-guard targets (`make verify-crd-sync`,
`make verify-invalid-cr-fixtures`, the
`TestSubReconcilerConditionTypesCovers*` Go tests). CI is the
authoritative gate; the skills give a contributor (or an agent acting on
the contributor's behalf) a structured, repeatable way to walk a
surface and explain the findings in prose.

## When to use a skill

- After changing a surface the skill audits (a `+kubebuilder` marker, a
  sub-reconciler, a condition type, a pinned version, a CR fixture, a
  documentation page, …).
- Before opening a release PR, as a final sweep across every surface.
- When a CI gate is red and you want a per-surface diagnostic that
  goes deeper than the gate's failure message.

The skills are read-only by design: each one runs a deterministic Bash
script and walks the relevant sources, then produces a findings report
grouped by severity. Fixes are a separate, explicitly-scoped task.

## How to invoke a skill

The skills are loaded automatically by Claude Code when it sees the
repository's `.claude/` directory. There are two ways to run one:

- **Explicit slash command.** Type `/<skill-name>` in the Claude Code
  prompt — for example `/check-doc-drift` — and Claude follows the
  procedure in the skill's `SKILL.md`.
- **Implicit trigger.** Each skill's description names the situations
  where it should be used proactively ("after editing a `*_types.go`
  file or a kubebuilder marker", "after adding a new pinned
  dependency", …). Claude Code may invoke the skill on its own when
  it recognises the trigger.

Every skill ships a `scripts/audit-<name>.sh` companion that runs the
deterministic part of the audit and exits non-zero on a `[FAIL]`. The
script is safe to run by hand from a shell — it reads files and prints
inventories; it never writes to the tree.

```bash
bash .claude/skills/check-doc-drift/scripts/audit-doc-drift.sh
```

Where applicable, passing `--full` chains the authoritative gate after
the smoke inventory (for example,
`bash .claude/skills/check-crd-drift/scripts/audit-crd-drift.sh --full`
runs `make verify-crd-sync` after the inventory). The skill's
`SKILL.md` describes the prose-level checks the script cannot do
(cross-referencing the doc to the source of truth, judging severity),
the gate commands to run alongside the audit, and the report format.

## Catalogue

The seven skills are grouped by the surface they audit. Each entry
links to the skill's `SKILL.md` (the procedure) and its companion Bash
script (the deterministic part of the audit).

### CRDs, configuration, and dependencies

| Skill | Audits | Use when |
|---|---|---|
| [`check-crd-drift`](https://github.com/c5c3/forge/blob/main/.claude/skills/check-crd-drift/SKILL.md) | Every operator CRD across its three representations — the Go `+kubebuilder` source under `operators/<op>/api/`, the controller-gen output under `operators/<op>/config/crd/bases/`, and the Helm chart copy under `operators/<op>/helm/<op>-operator/crds/`. Defers to `make verify-crd-sync` as the authoritative gate. | After editing a `*_types.go` file or a kubebuilder marker, before a release. |
| [`check-renovate-coverage`](https://github.com/c5c3/forge/blob/main/.claude/skills/check-renovate-coverage/SKILL.md) | Every pinned version in the repo — OpenStack release tags under `releases/<version>/source-refs.yaml`, shell-script `VERSION` constants under `hack/`, FluxCD HelmRelease versions under `deploy/flux-system/releases/`, and tool-version pins in `Makefile` and `.github/workflows/` — for coverage by either a native Renovate manager or a `customManager` in `renovate.json` with a paired `packageRules` entry. | After adding a new pinned dependency; when a previously-bumped pin silently stopped receiving updates. |
| [`check-go-workspace-deps`](https://github.com/c5c3/forge/blob/main/.claude/skills/check-go-workspace-deps/SKILL.md) | Shared dependency versions across `internal/common/go.mod` and every `operators/<op>/go.mod` — `sigs.k8s.io/controller-runtime`, `k8s.io/api`, `k8s.io/apimachinery`, `k8s.io/client-go`, `k8s.io/apiextensions-apiserver` — plus the Go directive and the `go.work` member list. | After running `go get` in one module; after Renovate bumps controller-runtime in only one of the modules. |

### Reconcilers, conditions, and fixtures

| Skill | Audits | Use when |
|---|---|---|
| [`check-condition-coverage`](https://github.com/c5c3/forge/blob/main/.claude/skills/check-condition-coverage/SKILL.md) | Keystone Status condition coverage end to end — every condition type set by a sub-reconciler must appear in the `subReconcilerConditionTypes` map, every sub-reconciler must have a paired unit test, and every condition the docs document must be set somewhere in code. Defers to `TestSubReconcilerConditionTypesCoversAllNames` and `TestSubReconcilerConditionTypesCoversAllCallSites` as the authoritative gate. | After adding or renaming a sub-reconciler or a condition type; when a Prometheus `condition_type` label suddenly shows up as `UNKNOWN`. |
| [`check-fixture-drift`](https://github.com/c5c3/forge/blob/main/.claude/skills/check-fixture-drift/SKILL.md) | Every Keystone CR fixture under `tests/e2e/` and `tests/e2e-chaos/` against the *current* CRD schema — no removed `Spec` field is still referenced, every fixture's `apiVersion` / `kind` matches a real CRD, every invalid-cr fixture is reachable from a Chainsaw test. Defers to `make verify-invalid-cr-fixtures` as the authoritative gate. | After editing the Keystone CRD or the validating webhook, before a release. |

### Documentation and compliance

| Skill | Audits | Use when |
|---|---|---|
| [`check-doc-drift`](https://github.com/c5c3/forge/blob/main/.claude/skills/check-doc-drift/SKILL.md) | The forge documentation against the implementation — the `architecture/docs/` chapters vs the operator code, the `docs/` user-facing reference vs the `deploy/` infrastructure stack, and the `CC-NNNN` feature IDs cited in prose vs the markers that appear in the code. | After adding or removing a sub-reconciler, status condition, operator binary, or infrastructure component, before tagging a release. |
| [`check-spdx-reuse`](https://github.com/c5c3/forge/blob/main/.claude/skills/check-spdx-reuse/SKILL.md) | SPDX / REUSE compliance across the source tree — every `*.go`, `*.sh`, hand-authored YAML, and CI workflow file should carry matching `SPDX-FileCopyrightText` and `SPDX-License-Identifier` headers; every licence referenced has a corresponding text under `LICENSES/`; `architecture/REUSE.toml` stays well-formed. Defers to `reuse lint` as the authoritative gate. | After adding a new source file; before tagging a release where SAP supply-chain audits require clean REUSE output. |

## What a skill is, structurally

Every skill in this suite follows the same layout:

```text
.claude/skills/<name>/
├── SKILL.md                 # the procedure Claude follows
└── scripts/
    └── audit-<name>.sh      # the deterministic, read-only audit
```

`SKILL.md` opens with a YAML frontmatter block carrying `name` and
`description` — the description is what Claude Code matches the user's
prompt against when deciding whether to load the skill. The body
documents the audit's procedure (a numbered step list with the script
invocation, the hand-cross-reference work, the authoritative gate to
run alongside, and the report format), the drift patterns that recur
on the surface, and the severity-grouped reporting format with
`file:line` citations on both the source and the source-of-truth side.

The Bash script is the deterministic part of the audit. It performs no
build, writes nothing, and reads from a fixed list of source-of-truth
files. Exit code `0` means no `[FAIL]`; `1` means at least one
mechanically-checkable assertion failed. Scripts are written to be
portable across Bash 3.2 (macOS default) and BSD `awk` / `sed`, so they
run cleanly on a contributor laptop without GNU coreutils.

## Authoring or modifying a skill

When you add a new skill or modify an existing one, keep the suite
internally consistent:

- **Frontmatter.** Every `SKILL.md` carries `name` and `description`.
  The description is the discoverability surface — write it as a
  single sentence in the form "Audit … — … . Use when …".
- **Read-only.** The companion script reads files and prints reports;
  it never writes to the tree, never runs `make generate`, never
  triggers a build.
- **Findings report.** The report groups findings by severity (HIGH /
  MEDIUM / LOW) with one line per finding and a `file:line` reference
  on both sides of the drift.
- **Pair with a gate.** If the audit overlaps a CI gate or a Makefile
  drift-guard, name the gate in `SKILL.md` and tell the reader to
  trust the gate's verdict over the script's. The script is a
  debugging aid; the gate is the source of truth.
- **Portable shell.** Target Bash 3.2 (no `mapfile`, no `[[ ]]`
  regex captures), BSD `awk` (no `match($0, /re/, arr)`), and BSD
  `sed` (no `\s`, use `[[:space:]]`). Verify by running the script
  on macOS before committing.
- **SPDX headers.** Both the `SKILL.md` (HTML-comment form) and the
  Bash script carry an SPDX-FileCopyrightText / SPDX-License-Identifier
  pair. The [`check-spdx-reuse`](https://github.com/c5c3/forge/blob/main/.claude/skills/check-spdx-reuse/SKILL.md)
  skill audits this.
- **Stay in English.** Skill bodies, frontmatter, and script comments
  follow the repository-wide English-only rule.

The skills are not a substitute for CI gates — they are an aid for the
contributor (or the agent) who has to read a surface deeply and explain
what changed. When in doubt, run the gate listed in the skill's
`SKILL.md` and trust its verdict.
