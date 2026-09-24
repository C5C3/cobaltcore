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
[`.claude/skills/`](https://github.com/c5c3/cobaltcore/tree/main/.claude/skills).
They come in five kinds:

- **Audits** (`check-*`) compare one surface of the codebase against its
  source of truth and report drift: the operator CRDs, the sub-reconciler
  conditions, the validation rules, the test fixtures, the release
  wiring, service parity, the Renovate configuration, the Go workspace,
  SPDX coverage, and the documentation.
- **Planners** (`prepare-new-service`, `prepare-new-release`,
  `prepare-new-guide`) turn an onboarding or authoring task into an
  ordered change list or a phased meta issue.
- **Procedures** (`add-validation-rule`, `add-image-patch`,
  `bump-korc-pin`) walk a recurring change through every file it has to
  touch, with a script that checks the result.
- **Runbooks and doc workflows** (`debug-e2e-failure`, `fix-docs`,
  `review-docs-human-issue`) diagnose a red CI job, apply docs audit
  findings, or produce a reader-focused review issue.
- **`check-skills`** audits the suite itself.

The skills complement the CI gates in
[`.github/workflows/`](https://github.com/c5c3/cobaltcore/tree/main/.github/workflows)
and the Makefile drift guards (`make verify-crd-sync`,
`make verify-invalid-cr-fixtures`, `make verify-go-tidy`, the
`TestSubReconcilerConditionTypesCoversAllNames` Go tests). CI is the
authoritative gate. The skills give a contributor, or an agent acting on
the contributor's behalf, a repeatable way to walk a surface and explain
what they find.

## When to use a skill

- After changing a surface a skill audits (a `+kubebuilder` marker, a
  sub-reconciler, a condition type, a pinned version, a CR fixture, a
  documentation page, …).
- Before starting a recurring change a procedure covers (a new admission
  rule, a downstream source patch, a K-ORC pin bump).
- Before opening a release PR, as a final sweep across every surface.
- When a CI gate is red and you want a per-surface diagnostic that goes
  deeper than the gate's failure message.

## How to invoke a skill

Claude Code loads the skills automatically from the repository's
`.claude/` directory. There are two ways to run one:

- **Explicit slash command.** Type `/<skill-name>` in the Claude Code
  prompt, for example `/check-doc-drift`, and Claude follows the
  procedure in the skill's `SKILL.md`.
- **Implicit trigger.** Each skill's description names the situations
  where it applies ("after editing a `*_types.go` file or a kubebuilder
  marker", "when four e2e legs fail on ImagePullBackOff for the K-ORC
  image", …). Claude Code may load the skill on its own when it
  recognises one.

Most skills ship a script under `scripts/` that runs the deterministic
part: `audit-<name>.sh` for audits, `inventory-*` scripts for the
planners, a scaffold/validate pair for guides, a log collector for the
e2e runbook, and a status or check script for the procedures. The
scripts are safe to run by hand. They read files and print to stdout;
they never write to the tree. The exceptions are explicit: the e2e
collector downloads CI evidence into the untracked `_output/` directory,
and the network modes of the procedure scripts query quay.io, GitHub, or
an upstream clone in a temporary directory.

```bash
bash .claude/skills/check-doc-drift/scripts/audit-doc-drift.sh
```

Where applicable, `--full` chains the authoritative gate after the
script (for example,
`bash .claude/skills/check-crd-drift/scripts/audit-crd-drift.sh --full`
runs `make verify-crd-sync`). The skill's `SKILL.md` covers what a
script cannot do (reading prose against the source of truth, judging
severity), the gates to run alongside it, and the report format.

## Catalogue

The skills are grouped by the surface they cover. Each entry links to
the skill's `SKILL.md`.

### CRDs, configuration, and dependencies

| Skill | Does | Use when |
|---|---|---|
| [`check-crd-drift`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/check-crd-drift/SKILL.md) | Audits every operator CRD across the Go `+kubebuilder` source under `operators/<op>/api/`, the controller-gen output under `operators/<op>/config/crd/bases/`, and the Helm chart copy under `operators/<op>/helm/<op>-operator/crds/`, plus the DeepCopy stubs. Defers to `make verify-crd-sync` and the CI `verify-codegen` job. | After editing a `*_types.go` file or a kubebuilder marker, before a release. |
| [`check-renovate-coverage`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/check-renovate-coverage/SKILL.md) | Audits every pinned version (OpenStack release tags, `hack/` version constants, kind and FluxCD manifests, Makefile and workflow tool pins) for a Renovate manager, pairs each `customManager` with the `packageRules` entry that matches its files and a regression test under `tests/unit/renovate/`, and checks that tool pins duplicated between the `Makefile` and `ci.yaml` agree. | After adding a pinned dependency; when a previously bumped pin silently stopped receiving updates. |
| [`check-go-workspace-deps`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/check-go-workspace-deps/SKILL.md) | Audits the Go workspace: the `go.work` member set, the `go` and `toolchain` directives, and one version per shared dependency across `internal/common/go.mod` and every `operators/<op>/go.mod` (the k8s/controller-runtime family, and every direct requirement two modules share). Defers to `make verify-go-tidy`. | After `go get` in one module; after a partial Renovate bump; when PR CI shows `sum.golang.org` errors that do not reproduce on the branch. |
| [`bump-korc-pin`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/bump-korc-pin/SKILL.md) | Moves the K-ORC main-commit pin in lockstep across its four sites: the Flux source commit, the image tag and digest, the infrastructure reference docs, and the `operators/c5c3` module version when the upstream API changed. Its status script reports the pin, the expiry of the per-commit quay tag, and whether the pinned digest still resolves. | When the K-ORC-deploying e2e legs fail together on `ImagePullBackOff`; when a Renovate commit bump leaves the image tag behind; before the pinned tag expires. |

### Reconcilers, conditions, validation, and fixtures

| Skill | Does | Use when |
|---|---|---|
| [`check-condition-coverage`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/check-condition-coverage/SKILL.md) | Audits status conditions end to end for every operator: each condition a sub-reconciler sets is in the `subReconcilerConditionTypes` map, each sub-reconciler has a paired unit test, and each condition the reference docs name is set in code. Defers to `TestSubReconcilerConditionTypesCoversAllNames`. | After adding or renaming a sub-reconciler or a condition type; when a Prometheus `condition_type` label shows up as `UNKNOWN`. |
| [`check-fixture-drift`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/check-fixture-drift/SKILL.md) | Audits every CR fixture under `tests/` against the current CRD schema (known kind on a served version, no removed field), Chainsaw step references in both directions, and the wiring of every invalid-CR generator into `make verify-invalid-cr-fixtures`. | After editing a CRD or a validating webhook, before a release. |
| [`check-validation-parity`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/check-validation-parity/SKILL.md) | Audits every admission rule across its representations: markers and CEL rules, the validating webhook, the webhook unit tests, and the invalid-CR rejection corpora under `tests/e2e/<op>/invalid-*`. Checks that every error substring a Chainsaw suite asserts still anchors to a current rule. | After adding or changing a validation rule or webhook; after a CEL rule was demoted to webhook-only enforcement. |
| [`add-validation-rule`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/add-validation-rule/SKILL.md) | Walks a new or changed admission rule through every representation: choosing the layer (marker, CEL, webhook, or both), regenerating the CRD, the webhook unit test, the invalid-CR generator entry and its Chainsaw assertion, the ControlPlane mirror, and the CRD reference docs. | When adding or changing a validation rule, a CEL rule, or a webhook check on any CRD. |

### Releases, services, and images

| Skill | Does | Use when |
|---|---|---|
| [`check-release-wiring`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/check-release-wiring/SKILL.md) | Audits every release under `releases/<version>/` for full wiring: the release config files, a Tempest config directory per Tempest-covered service (the CI matrix generator hard-fails without one), the per-service `basic-deployment-<slug>` variants and `release-upgrade` suites, the placed-services pins, the default-release references, the Renovate regression tests, and the version-pattern lockstep. | After adding or removing a `releases/<version>/` directory; before a release. |
| [`check-service-parity`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/check-service-parity/SKILL.md) | Audits every onboarded service for lockstep with the keystone reference across the five onboarding layers: container image, service operator, CI/e2e/deploy wiring, ControlPlane integration, and documentation. | While reviewing or after merging a service-onboarding PR; when a later service drifts from the conventions keystone defines. |
| [`prepare-new-service`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/prepare-new-service/SKILL.md) | Plans a service onboarding across the five layers: profiles the service, inventories what exists, checks what must be generalized into `internal/common` first, and drafts the phased meta issue. | When onboarding a new OpenStack (or OpenStack-adjacent) service; when assessing readiness for the next service operator. |
| [`prepare-new-release`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/prepare-new-release/SKILL.md) | Plans a new OpenStack release: inventories the touch points auto-discovery does not cover (release config files, per-service Tempest directories and e2e variants, constraint overrides) and walks the decisions (moving the default release, the upgrade path, retiring the oldest release). | When adding a release, bumping the release matrix, or removing an old release. |
| [`add-image-patch`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/add-image-patch/SKILL.md) | Adds, backports, or retires a downstream source patch under `patches/<svc>/<release>/`: producing it against the pinned upstream tag, carrying the upstream unit-test hunks, the `verify_<svc>.sh` assertion, and the container-images documentation. Its check script validates naming, placement, and documentation, and can `git apply --check` every patch against upstream. | When a service image needs a fix upstream has not released; when a Renovate bump stops a patch from applying. |

### Documentation and compliance

| Skill | Does | Use when |
|---|---|---|
| [`check-doc-drift`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/check-doc-drift/SKILL.md) | Audits the documentation against the implementation: the `docs/reference/` pages against the operator code, the guides and quick starts against the `deploy/` stack. | After adding or removing a sub-reconciler, condition, operator, or infrastructure component; before tagging a release. |
| [`check-doc-consistency`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/check-doc-consistency/SKILL.md) | Audits the docs for contradictions: terminology, versions and defaults against their source files, repeated examples, cross-page claims. | After changing shared terminology; when one page seems to disagree with another. |
| [`check-doc-expressions`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/check-doc-expressions/SKILL.md) | Audits prose quality: clarity, voice, terminology, jargon, command examples, and the `STYLE_GUIDE.md` rhetorical-device budget, which its script counts per page. | After drafting or editing prose; when a page reads correctly but not clearly. |
| [`check-doc-structure`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/check-doc-structure/SKILL.md) | Audits structure and navigation: frontmatter, heading hierarchy, links and `#anchors` (with VitePress' slug rules, which the build does not check), sidebar entries, and orphan pages. | After adding, moving, or renaming a page or a heading; when a page is hard to find from the nav. |
| [`check-spdx-reuse`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/check-spdx-reuse/SKILL.md) | Audits SPDX headers on Go, shell, and hand-written YAML files, and the licence texts under `LICENSES/`. Generated files and Helm charts, which carry no inline headers by convention, are reported separately. | After adding a source file; before a release that must pass a REUSE check. |
| [`prepare-new-guide`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/prepare-new-guide/SKILL.md) | Scaffolds a how-to guide under `docs/guides/` for a chosen devstack and validates drafts against the guide conventions. Defers to `tests/unit/docs/guide_devstack_and_tested_by_test.sh`. | When writing a new guide; when checking a draft guide. |
| [`fix-docs`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/fix-docs/SKILL.md) | Applies findings from the three `check-doc-*` prose audits, or runs them first: classifies each finding as mechanical, a judgment call (confirmed before editing), or needs-research, edits the docs, and re-verifies. | When actioning a docs audit report or fixing documentation issues. |
| [`review-docs-human-issue`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/review-docs-human-issue/SKILL.md) | Reviews documentation end to end through reader personas and drafts one issue listing every user-facing problem with evidence and severity; files it only after confirmation and within GitHub's body limit. | Before a release; after major doc edits; when asked for a reader-focused docs review. |

### CI and the skill suite

| Skill | Does | Use when |
|---|---|---|
| [`debug-e2e-failure`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/debug-e2e-failure/SKILL.md) | Diagnoses a failing e2e or Tempest job: collects the failed-step logs and artifacts, scans them for known infrastructure and suite failure signatures, maps the failure to its suite under `tests/`, and reproduces it locally. | When any e2e, Tempest, or chaos job fails; when deciding between a rerun and a root-cause fix. |
| [`check-skills`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/check-skills/SKILL.md) | Audits the skill suite: frontmatter and trigger descriptions, `SKILL.md` size, script hygiene (SPDX, Bash 3.2, shellcheck, read-only), cross-references, quoted `make` targets, this catalogue, and a staleness radar of the files each skill names. | After adding or editing a skill; when an audit's findings look like false positives; periodically. |

## What a skill is, structurally

```text
.claude/skills/<name>/
├── SKILL.md          # frontmatter + the procedure Claude follows
├── scripts/          # the deterministic companion (optional for prose skills)
│   └── audit-<name>.sh
└── references/       # optional: long reference material, read on demand
    └── <topic>.md
```

`SKILL.md` opens with a YAML frontmatter block carrying `name` (equal to
the directory name) and `description`. Claude Code matches the user's
prompt against the description alone, so it names both what the skill
does and when to use it. The body loads every time the skill triggers;
material only one step needs (build sheets, tables of past cases,
per-service notes) goes into `references/`, and the step says which file
to read.

The script is the deterministic part. Audit and inventory scripts build
nothing, write nothing, and print `[PASS]`/`[FAIL]`/`[INFO]` lines. Exit
code `0` means no `[FAIL]`; `1` means at least one mechanically checkable
assertion failed. Scripts target Bash 3.2 (the macOS default) and BSD
`awk`/`sed`, so they run on a contributor laptop without GNU coreutils.
A check that needs a real parser may use a python3 helper beside the
script, as long as the script degrades to `[INFO] … skipped` without it.

## Authoring or modifying a skill

When you add or change a skill, keep the suite consistent and run
[`check-skills`](https://github.com/c5c3/cobaltcore/blob/main/.claude/skills/check-skills/SKILL.md)
before committing:

```bash
bash .claude/skills/check-skills/scripts/audit-skills.sh
```

- **Frontmatter.** `name` and `description`. Write the description as
  `<what it does> — <scope>. Use when <concrete triggers>`, and name the
  symptom a user would type or the CI signal they would see.
- **Derive, don't list.** Read operators, services, releases, and Tempest
  legs from the files that define them (`Makefile` `OPERATORS`,
  `releases/<latest>/source-refs.yaml`, `ls releases/`,
  `hack/ci-generate-tempest-matrix.sh`). A hard-coded list is how an
  audit ends up checking one service out of eight while reporting green.
- **Read-only.** The companion script reads files and prints reports; it
  never writes to the tree, never runs `make generate`, never triggers a
  build. A write into a temporary directory carries a trailing
  `# audit-skills: allow` marker.
- **No noise.** A check that reports false positives teaches readers to
  ignore the whole audit. Fix the heuristic, or record the deviation in
  the script's allowlist with its reason.
- **Findings report.** Group findings by severity (HIGH / MEDIUM / LOW),
  one line each, with a `file:line` reference on both sides of the drift.
- **Pair with a gate.** If the audit overlaps a CI gate or a Makefile
  drift guard, name the gate and tell the reader to trust its verdict
  over the script's.
- **Portable shell.** Bash 3.2 (no `mapfile`, no associative arrays, no
  `[[ ]]` regex captures), BSD `awk` (no three-argument `match()`), BSD
  `sed` (no `\s`; use `[[:space:]]`). `check-skills` parses every script
  with the macOS `/bin/bash` and runs `shellcheck`.
- **SPDX headers.** Scripts carry the SPDX pair. `SKILL.md` and
  reference files start directly with their content, without a header
  comment.
- **Catalogue.** Add or update the skill's row in this page.
- **Stay in English.** Skill bodies, frontmatter, and script comments
  follow the repository-wide English-only rule.

The skills are not a substitute for CI gates. They help the contributor
(or the agent) who has to read a surface closely and explain what
changed. When in doubt, run the gate the skill names and trust its
verdict.
