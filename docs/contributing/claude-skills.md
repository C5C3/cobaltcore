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
Most skills are focused **audits** that compare one surface of the
codebase against its source of truth — the operator CRDs, the
sub-reconciler chains, the validation rules, the release wiring, the
FluxCD infrastructure stack, the Renovate configuration, the e2e
fixture corpus, the SPDX/REUSE coverage, the Go workspace — and report
drift before it reaches a release. The suite also carries two
**planners** (`prepare-new-service`, `prepare-new-release`) that turn
an onboarding task into a phased checklist, and a **runbook**
(`debug-e2e-failure`) for diagnosing failing chainsaw e2e jobs.

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

Every audit skill ships a `scripts/audit-<name>.sh` companion that runs
the deterministic part of the audit and exits non-zero on a `[FAIL]`;
the planners ship `inventory-*` scripts and the e2e runbook ships a log
collector (`collect-e2e-failure.sh`). Audit and inventory scripts are
safe to run by hand from a shell — they read files and print
inventories; they never write to the tree. The collector downloads CI
evidence into the untracked `_output/` directory.

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

The thirteen skills are grouped by the surface they cover. Each entry
links to the skill's `SKILL.md` (the procedure); every skill also ships
a companion Bash script (the deterministic part).

### CRDs, configuration, and dependencies

| Skill | Audits | Use when |
|---|---|---|
| [`check-crd-drift`](https://github.com/c5c3/forge/blob/main/.claude/skills/check-crd-drift/SKILL.md) | Every operator CRD across its three representations — the Go `+kubebuilder` source under `operators/<op>/api/`, the controller-gen output under `operators/<op>/config/crd/bases/`, and the Helm chart copy under `operators/<op>/helm/<op>-operator/crds/`. Defers to `make verify-crd-sync` as the authoritative gate. | After editing a `*_types.go` file or a kubebuilder marker, before a release. |
| [`check-renovate-coverage`](https://github.com/c5c3/forge/blob/main/.claude/skills/check-renovate-coverage/SKILL.md) | Every pinned version in the repo — OpenStack release tags under `releases/<version>/source-refs.yaml`, shell-script `VERSION` constants under `hack/`, FluxCD HelmRelease versions under `deploy/flux-system/releases/`, and tool-version pins in `Makefile` and `.github/workflows/` — for coverage by either a native Renovate manager or a `customManager` in `renovate.json` with a paired `packageRules` entry, and that tool pins duplicated between the `Makefile` and `ci.yaml` stay in lockstep. | After adding a new pinned dependency; when a previously-bumped pin silently stopped receiving updates. |
| [`check-go-workspace-deps`](https://github.com/c5c3/forge/blob/main/.claude/skills/check-go-workspace-deps/SKILL.md) | Shared dependency versions across `internal/common/go.mod` and every `operators/<op>/go.mod` — `sigs.k8s.io/controller-runtime`, `k8s.io/api`, `k8s.io/apimachinery`, `k8s.io/client-go`, `k8s.io/apiextensions-apiserver` — plus the Go directive and the `go.work` member list. | After running `go get` in one module; after Renovate bumps controller-runtime in only one of the modules. |

### Reconcilers, conditions, and fixtures

| Skill | Audits | Use when |
|---|---|---|
| [`check-condition-coverage`](https://github.com/c5c3/forge/blob/main/.claude/skills/check-condition-coverage/SKILL.md) | Status condition coverage end to end for every forge operator — every condition type set by a sub-reconciler must appear in that operator's `subReconcilerConditionTypes` map, every sub-reconciler must have a paired unit test, and every condition the operator's reference docs document must be set somewhere in code. Defers to `TestSubReconcilerConditionTypesCoversAllNames` as the authoritative gate. | After adding or renaming a sub-reconciler or a condition type; when a Prometheus `condition_type` label suddenly shows up as `UNKNOWN`. |
| [`check-fixture-drift`](https://github.com/c5c3/forge/blob/main/.claude/skills/check-fixture-drift/SKILL.md) | Every Keystone / c5c3 CR fixture under `tests/e2e/` and `tests/e2e-chaos/` against the *current* CRD schema — no removed `Spec` field is still referenced, every fixture's `apiVersion` / `kind` matches a real CRD, every invalid-cr fixture is reachable from a Chainsaw test. Defers to `make verify-invalid-cr-fixtures` as the authoritative gate. | After editing the Keystone CRD or the validating webhook, before a release. |
| [`check-validation-parity`](https://github.com/c5c3/forge/blob/main/.claude/skills/check-validation-parity/SKILL.md) | Every CR validation rule across its four representations — the declarative kubebuilder markers and XValidation/CEL rules in `operators/<op>/api/`, the validating webhook in `*_webhook.go`, the webhook unit tests, and the invalid-cr rejection corpus under `tests/e2e/<op>/invalid-cr/`. | After adding or changing a validation rule or webhook; after a CEL rule had to be demoted to webhook-only enforcement. |

### Releases and services

| Skill | Audits | Use when |
|---|---|---|
| [`check-release-wiring`](https://github.com/c5c3/forge/blob/main/.claude/skills/check-release-wiring/SKILL.md) | Every OpenStack release under `releases/<version>/` for full wiring — the mandatory release config files, the per-release Tempest config directory the CI matrix generator hard-requires, the per-release basic-deployment e2e variant, the default-release references in `deploy/kind/`, `hack/`, and `ci.yaml`, the Renovate regression tests, the upgrade-path e2e suites, and the version-pattern lockstep across CRD marker, webhook, and `release.ParseRelease`. | After adding or removing a `releases/<version>/` directory; before a release when a stale default tag or an orphan Tempest directory would fail the pipeline. |
| [`check-service-parity`](https://github.com/c5c3/forge/blob/main/.claude/skills/check-service-parity/SKILL.md) | Every onboarded OpenStack service for structural lockstep with the keystone reference implementation across the five onboarding layers — container image under `images/<svc>/`, service operator under `operators/<svc>/`, CI/e2e/deploy wiring, ControlPlane integration in `operators/c5c3/`, and the documentation set under `docs/`. | After merging or while reviewing a service-onboarding PR; when a second (or later) service starts drifting from the scaffolding conventions keystone defines. |

### Documentation and compliance

| Skill | Audits | Use when |
|---|---|---|
| [`check-doc-drift`](https://github.com/c5c3/forge/blob/main/.claude/skills/check-doc-drift/SKILL.md) | The forge documentation against the implementation — the `architecture/docs/` chapters vs the operator code, and the `docs/` user-facing reference vs the `deploy/` infrastructure stack. | After adding or removing a sub-reconciler, status condition, operator binary, or infrastructure component, before tagging a release. |
| [`check-spdx-reuse`](https://github.com/c5c3/forge/blob/main/.claude/skills/check-spdx-reuse/SKILL.md) | SPDX / REUSE compliance across the source tree — every `*.go`, `*.sh`, hand-authored YAML, and CI workflow file should carry matching `SPDX-FileCopyrightText` and `SPDX-License-Identifier` headers; every licence referenced has a corresponding text under `LICENSES/`; `architecture/REUSE.toml` stays well-formed. Defers to `reuse lint` as the authoritative gate. | After adding a new source file; before tagging a release where SAP supply-chain audits require clean REUSE output. |

### Planners and runbooks

| Skill | Does | Use when |
|---|---|---|
| [`prepare-new-service`](https://github.com/c5c3/forge/blob/main/.claude/skills/prepare-new-service/SKILL.md) | Plans the onboarding of a new OpenStack service across the five layers against the keystone reference — inventories what already exists, checks what keystone scaffolding must be generalized into `internal/common` first, and drafts the phased meta issue. | When asked to onboard a new OpenStack service (Glance, Nova, Neutron, Placement, …); when assessing readiness for the next service operator. |
| [`prepare-new-release`](https://github.com/c5c3/forge/blob/main/.claude/skills/prepare-new-release/SKILL.md) | Plans the addition of a new OpenStack release — inventories the touch points auto-discovery does not cover (release config files, the Tempest config directory, per-release e2e variants, constraint overrides) and walks the decision points: moving the default release, extending the upgrade-path tests, retiring an old release. | When asked to add a new OpenStack release, bump the release matrix, or remove an old release. |
| [`debug-e2e-failure`](https://github.com/c5c3/forge/blob/main/.claude/skills/debug-e2e-failure/SKILL.md) | Diagnoses a failing chainsaw e2e job — resolves the failed run, pulls the failed-step logs and JUnit/diagnostic evidence, maps the failure back to the suite directory under `tests/`, classifies it against the known flake patterns, and reproduces it locally against a kind cluster. | When any CI e2e job fails (`e2e-operator`, `e2e-chaos`, `e2e-controlplane`, …); when reproducing an e2e failure locally. |

## What a skill is, structurally

Every skill in this suite follows the same layout:

```text
.claude/skills/<name>/
├── SKILL.md                 # the procedure Claude follows
└── scripts/
    └── audit-<name>.sh      # the deterministic companion script
```

Audit skills name the script `audit-<name>.sh`; the planners ship
`inventory-*` scripts, and the e2e runbook ships
`collect-e2e-failure.sh`.

`SKILL.md` opens with a YAML frontmatter block carrying `name` and
`description` — the description is what Claude Code matches the user's
prompt against when deciding whether to load the skill. The body
documents the audit's procedure (a numbered step list with the script
invocation, the hand-cross-reference work, the authoritative gate to
run alongside, and the report format), the drift patterns that recur
on the surface, and the severity-grouped reporting format with
`file:line` citations on both the source and the source-of-truth side.

The Bash script is the deterministic part of the skill. Audit and
inventory scripts perform no build, write nothing, and read from a
fixed list of source-of-truth files. Exit code `0` means no `[FAIL]`;
`1` means at least one mechanically-checkable assertion failed. Scripts are written to be
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
- **SPDX headers.** The Bash script carries an SPDX-FileCopyrightText /
  SPDX-License-Identifier pair. `SKILL.md` files follow the suite
  convention and start directly with the YAML frontmatter — no header
  comment. The [`check-spdx-reuse`](https://github.com/c5c3/forge/blob/main/.claude/skills/check-spdx-reuse/SKILL.md)
  skill audits the script coverage.
- **Stay in English.** Skill bodies, frontmatter, and script comments
  follow the repository-wide English-only rule.

The skills are not a substitute for CI gates — they are an aid for the
contributor (or the agent) who has to read a surface deeply and explain
what changed. When in doubt, run the gate listed in the skill's
`SKILL.md` and trust its verdict.
