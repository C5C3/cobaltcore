---
name: check-renovate-coverage
description: >-
  Audit whether every pinned version in the forge repo — OpenStack
  release tags under releases/<version>/source-refs.yaml, shell-script
  VERSION constants under hack/, kind / FluxCD HelmRelease versions
  under deploy/kind/ and deploy/flux-system/, and tool-version pins in
  Makefile + .github/workflows — is covered by either a native Renovate
  manager or a customManager rule in renovate.json with a paired
  packageRules entry. Use when asked to check Renovate coverage, after
  adding a new pinned dependency, or when a previously-bumped pin
  silently stopped receiving updates.
---

# Check Renovate coverage

This skill verifies that the forge **dependency pins are all reachable
by Renovate**: every version literal that a human edits when bumping a
dependency must be paired with a matcher (native or custom) that
Renovate can crank, and every customManager must have a packageRules
entry that decides triage rules (major/minor split, automerge, release
age, grouping). A pin without a manager is a pin that silently goes
stale.

It is repeatable — run it any time, especially after introducing a new
version literal or after editing `renovate.json`.

## What Renovate coverage means here

A version pin in forge typically threads through three things, all of
which Renovate has to understand:

| Layer | Where it lives | Renovate handle |
|---|---|---|
| OpenStack release tags | `releases/<release>/source-refs.yaml` (one line per component: `keystone: "29.0.0"`) | customManager #1 — matches `^(?<depName>[\w.-]+):\s*"(?<currentValue>\d+\.\d+\.\d+)"` per file under `releases/.*/source-refs.yaml` |
| Shell script constants | `hack/deploy-infra.sh` (`FLUX_OPERATOR_VERSION="v…"`, `GATEWAY_API_VERSION="${…:-v…}"`) | customManager — one regex per constant; the existing managers cover `FLUX_OPERATOR_VERSION` only |
| kind base manifests | `deploy/kind/base/flux-web.yaml` (`- version: "…"`), `deploy/kind/base/envoy-gateway.yaml` (range like `">=0.0.0 <0.0.0"`) | customManager — one regex per file shape |
| FluxCD HelmRelease versions | `deploy/flux-system/releases/*.yaml` (`spec.chart.spec.version`) | native `flux` / `helm-values` manager (no customManager needed) |
| Go module deps | `operators/*/go.mod`, `internal/common/go.mod` | native `gomod` manager (no customManager needed) |
| GitHub Actions versions | `.github/workflows/*.yaml` (`uses: org/action@v…`) | native `github-actions` manager |
| Dockerfile base images | `operators/*/Dockerfile` (`FROM image:tag`) | native `dockerfile` manager |
| Tool pins in Makefile | `Makefile` (`GOFUMPT_VERSION ?= v0.9.2`, `ENVTEST_K8S_VERSION ?= 1.35`) | **no manager** — must be a customManager, or manually tracked |

The authoritative gate is the `renovate-config-validator` (run via the
existing shell unit tests under `tests/unit/renovate/`). This skill
defers to those tests for `renovate.json` correctness and adds the
coverage check the validator cannot express: "is every version literal
on disk claimed by some manager?"

A coverage finding is any version literal that no Renovate manager
matches, or a customManager with no packageRules to triage its PRs.

## Procedure

Work through these steps in order and report findings at the end.

### 1. Run the deterministic audit

```bash
bash .claude/skills/check-renovate-coverage/scripts/audit-renovate-coverage.sh
```

The script catches the mechanically-checkable gaps and prints an
inventory. Exit code `1` means at least one `[FAIL]`. Interpret:

- **R1** — every line in every `releases/*/source-refs.yaml` that
  looks like `<name>: "<x.y.z>"` is matched by the source-refs
  customManager regex. A line that does not match is either a
  formatting drift (added quotes, switched to single quotes) or a
  component whose version style the regex does not support yet.
- **R2** — every `<NAME>_VERSION="…"` constant in `hack/*.sh` is
  matched by at least one customManager pattern. A constant added
  without a paired customManager is a silent pin: humans edit it but
  Renovate ignores it.
- **R3** — every `version: "…"` literal in `deploy/kind/base/*.yaml`
  is matched by a customManager pattern. The two existing managers
  cover `flux-web.yaml` and `envoy-gateway.yaml`; any other file in
  the same dir is flagged.
- **R4** — every customManager entry in `renovate.json` has at least
  one paired entry in `packageRules` (otherwise updates land
  untriaged: no major-bump gate, no minimumReleaseAge, no automerge
  policy).
- **R5** — every entry in `releases/*/source-refs.yaml` has a paired
  packageRule that disables major bumps (the OpenStack tags rule).
  A new entry that bypasses the rule silently allows major bumps.
- **R6** — the shell-script tests under `tests/unit/renovate/` still
  exist for every customManager (so the rule has a regression test).
- The **inventories** are review aids: every version literal found on
  disk, grouped by file; every Renovate manager (native or custom)
  with its matched paths.

### 2. Cross-reference the inventory

The script cannot run Renovate itself. Using the printed inventory,
confirm:

1. For each `[FAIL]` from R1–R3, decide whether to add a new
   customManager or normalise the file to match an existing one.
2. For each `[FAIL]` from R4–R5, add the missing packageRules entry
   (with `matchUpdateTypes: [major]` disabled by default, paired
   automerge + 3-day `minimumReleaseAge` for minor/patch, matching
   the existing pattern).
3. Confirm by hand that every newly added customManager has a
   regression test under `tests/unit/renovate/`.
4. For tool pins in `Makefile` (`GOFUMPT_VERSION`, `ENVTEST_K8S_VERSION`),
   decide whether to add a customManager. These are often
   intentionally pinned and not auto-bumped — but document the
   decision in `renovate.json` (or in a comment in the Makefile)
   either way.

### 3. Run the authoritative gates

The script does not invoke Renovate. Run the real gates and report the
exact outcomes:

```bash
bash tests/unit/renovate/fluxoperator_custommanager_test.sh
bash tests/unit/renovate/flux_web_chart_custommanager_test.sh
bash tests/unit/renovate/envoy_gateway_manager_test.sh
# Optionally, with npx available:
npx --package renovate -- renovate-config-validator renovate.json
```

These confirm `renovate.json` is syntactically valid and that the
existing customManagers still match the on-disk constants. Trust their
outcome over the R1–R6 smoke checks when they disagree.

### 4. Report

Produce a concise summary grouped by severity:

- **HIGH** — `renovate-config-validator` fails; a version literal on
  disk is not matched by any manager; a customManager has no
  paired packageRules entry; a `releases/*/source-refs.yaml` entry
  has no major-bump-disable rule.
- **MEDIUM** — a tool pin in `Makefile` or `.github/workflows/*` is
  uncovered; a customManager has no regression test under
  `tests/unit/renovate/`; a customManager regex uses an inconsistent
  versioning template vs the existing rules.
- **LOW** — formatting drift inside a tracked file (single vs double
  quotes, extra whitespace) that the regex still matches but is
  inconsistent with siblings; a stale comment in `renovate.json`.

For each finding give one line with a `file:line` reference for both
the pin side and the renovate.json side. End with a verdict per layer.

## Coverage patterns

These recurring shapes are worth grepping for first:

1. **New shell constant, no customManager.** A `NEW_VERSION="…"`
   constant added to `hack/deploy-infra.sh` for a one-off install
   step. Renovate has no idea — the constant ages until a human spots
   the upstream release notes.
2. **New `releases/<release>/source-refs.yaml` entry style drift.**
   A component switched from `name: "x.y.z"` to `name: "vx.y.z"` (or
   to a SHA pin). The existing regex requires `\d+\.\d+\.\d+`; the
   new style silently falls outside its match set.
3. **customManager without packageRules.** A new manager was added
   to extend coverage but the `packageRules` block was not extended
   to triage its PRs. Renovate raises untriaged PRs (major bumps not
   gated, no minimumReleaseAge), so reviewers waste time closing them.
4. **New kind base manifest, no customManager.** A new YAML under
   `deploy/kind/base/` with a `version: "…"` line. The two existing
   managers are file-name-anchored; a sibling needs its own manager.
5. **Tool pin in Makefile.** `GOFUMPT_VERSION ?= v0.9.2` or similar
   is a real pin that gets bumped manually. No native Renovate
   manager catches Makefile constants; a customManager (with regex
   `^([A-Z_]+_VERSION) \?= (v[0-9.]+)`) would close the gap.

## Notes

- This skill is read-only; the deterministic script edits nothing.
  Apply fixes (add the customManager, add the packageRule, add the
  unit test) as a separate, explicitly-scoped task.
- Some pins are *intentionally* not Renovate-tracked (e.g. a tool
  whose upstream release cadence is too aggressive). When the
  decision is to skip, document it: either an empty customManager
  matchStrings (`"matchStrings": []` with a description), or an
  HCL-style comment in the Makefile near the pin.
- The existing tests under `tests/unit/renovate/` are the source of
  truth for "what is covered today". If you add a customManager, add
  a sibling test there — the audit script flags missing tests as a
  MEDIUM.
- Pair this with [[check-doc-drift]] — that skill checks that
  infrastructure version pins documented in the prose match the
  `deploy/` reality; this skill checks that the pins themselves are
  trackable.
