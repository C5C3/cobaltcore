---
name: check-renovate-coverage
description: >-
  Audit whether every pinned version in the CobaltCore repo — OpenStack
  release tags under releases/<version>/source-refs.yaml, shell-script
  VERSION constants under hack/, kind / FluxCD HelmRelease versions
  under deploy/kind/ and deploy/flux-system/, and tool-version pins in
  Makefile + .github/workflows — is covered by either a native Renovate
  manager or a customManager rule in renovate.json with a paired
  packageRules entry, and that tool pins duplicated between the
  Makefile and ci.yaml stay in lockstep. Use when asked to check
  Renovate coverage, after adding a new pinned dependency, or when a
  previously-bumped pin silently stopped receiving updates.
---

# Check Renovate coverage

This skill verifies that the CobaltCore **dependency pins are all reachable
by Renovate**: every version literal that a human edits when bumping a
dependency must be paired with a matcher (native or custom) that
Renovate can crank, and every customManager must have a packageRules
entry that decides triage rules (major/minor split, automerge, release
age, grouping). A pin without a manager is a pin that silently goes
stale.

It is repeatable — run it any time, especially after introducing a new
version literal or after editing `renovate.json`.

## What Renovate coverage means here

A version pin in CobaltCore typically threads through three things, all of
which Renovate has to understand:

| Layer | Where it lives | Renovate handle |
|---|---|---|
| OpenStack release tags | `releases/<release>/source-refs.yaml` (one line per component: `keystone: "29.0.0"`) | customManager — `^(?<depName>[\w.-]+):\s*"(?<currentValue>\d+\.\d+\.\d+)"` over `releases/.*/source-refs.yaml`, git-tags on opendev |
| Release test and constraint pins | `releases/<release>/test-refs.yaml`, `overrides/<release>/constraints.txt` | customManager each (pypi) |
| Shell script constants | `hack/deploy-infra.sh` + `hack/deploy-mgmt-cluster.sh` (`FLUX_OPERATOR_VERSION="v…"`, deliberately duplicated, one customManager over both files; `GATEWAY_API_VERSION`, `ENVOY_GATEWAY_VERSION`, `REGISTRY_CACHE_IMAGE`), `hack/install-test-deps.sh` (chainsaw, flux, kind, kubectl), `hack/dizzy.sh` | customManager, one regex per constant |
| kind manifests | `deploy/kind/base/{flux-web,envoy-gateway,headlamp}.yaml`, `deploy/kind/infrastructure/openbao-instance.yaml`, `deploy/kind/nfs/{nfs-server,release}.yaml` | customManager, one regex per file shape |
| Flux sources and images pinned by tag/commit/digest | `deploy/flux-system/sources/{k-orc,openbao-operator,rabbitmq-cluster-operator}.yaml`, `deploy/flux-system/releases/rabbitmq-cluster-operator.yaml` | customManager each. The K-ORC image in `deploy/flux-system/releases/k-orc.yaml` is **not** tracked (pattern 7) |
| FluxCD HelmRelease chart versions | `deploy/flux-system/releases/*.yaml` (`spec.chart.spec.version: ">=0.1.0 <1.0.0"`) | **not Renovate-tracked**: floating semver ranges Flux resolves at reconcile time. The native `flux` manager's default file pattern is `gotk-components.yaml` only and `renovate.json` sets no `flux.managerFilePatterns`, so raising a `<1.0.0` ceiling is a manual edit |
| Pins in Go source | `operators/c5c3/internal/controller/reconcile_barbican_openbao.go` (`defaultOpenBaoVersion`), `operators/ovn/internal/controller/image.go` (`defaultOVNVersion`) | customManager each, paired with the matching manifest/Dockerfile pin |
| Go module deps | `operators/*/go.mod`, `internal/common/go.mod` | native `gomod` manager |
| GitHub Actions versions | `.github/workflows/*.yaml` (`uses: org/action@v…`) | native `github-actions` manager |
| Dockerfile base images | `images/*/Dockerfile` and the single `operators/Dockerfile` (`FROM image:tag`) | native `dockerfile` manager; the `ARG OVN_VERSION` / `ARG NOVNC_VERSION` + `NOVNC_COMMIT` pins in `images/{ovn,nova}/Dockerfile` carry a customManager each |
| Nix flake, venv-builder requirements | `flake.nix` + `flake.lock`, `images/venv-builder/requirements.txt` | native `nix` (enabled in `renovate.json`, weekly lockFileMaintenance) and `pip_requirements` |
| e2e fixture images | the openldap, keycloak, aws-cli and nfs-server images pinned by tag + digest in `tests/e2e*/` fixtures | customManager per image |
| Tool pins in Makefile + workflows | `GOFUMPT_VERSION` (customManager over `/Makefile$/` + workflows); `CONTROLLER_GEN_VERSION`, `GOLANGCI_LINT_VERSION`, `KIND_VERSION`, `YQ_VERSION`, `ACTIONLINT_VERSION` in workflow `env:` blocks; `RENOVATE_VALIDATOR_VERSION` in a `tests/unit/renovate/` test | customManager each. `ENVTEST_K8S_VERSION ?= 1.35` has **no manager** (bumped by hand) |
| Duplicated Makefile ↔ ci.yaml pins | `GOFUMPT_VERSION` lives in both `Makefile` and the `ci.yaml` `env:` block ("Must be kept in sync" comment); `ENVTEST_K8S_VERSION` is single-sourced (ci.yaml `awk`-reads the Makefile) | one customManager bumps both files; R7 enforces the lockstep mechanically |

The authoritative gate is the shell unit tests under
`tests/unit/renovate/`: one of them runs `renovate-config-validator`,
the rest run each customManager's `matchStrings` through `perl` against
the file on disk. This skill
defers to those tests for `renovate.json` correctness and adds the
coverage checks the validator cannot express: "is every version literal
on disk claimed by some manager, and does a packageRule triage it?"

A coverage finding is any version literal that no Renovate manager
matches, a customManager whose file patterns match nothing, a
customManager no packageRule applies to, or a customManager no
regression test exercises.

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
  Renovate ignores it. Runtime-resolved values — command substitutions
  and `${VAR:?}` required-env passthroughs — are exempt; they are not
  pins Renovate could bump.
- **R3** — every `version: "…"` literal in `deploy/kind/base/*.yaml`
  is matched by a customManager pattern. The existing managers cover
  `flux-web.yaml`, `envoy-gateway.yaml` and `headlamp.yaml`; any other
  file in the same dir is flagged.
- **R4** — per customManager, two findings. First, its
  `managerFilePatterns` must match at least one `git ls-files` path; a
  manager matching nothing is a **dead manager**, and one pattern
  matching nothing beside live siblings is a **stale pattern** (a
  renamed file). Second, some `packageRules` entry must apply to it,
  otherwise updates land untriaged: no major-bump gate, no
  minimumReleaseAge, no automerge policy. A rule applies when its
  `matchFileNames` matches one of the manager's tracked files
  (minimatch semantics: `**` spans directories only as a whole path
  segment, so `tests/unit/renovate/**.sh` behaves like `*.sh`; `*`
  never crosses `/`; `/…/` entries are regexes) or its
  `matchPackageNames` / `matchDepNames` names the manager's dependency
  (`packageNameTemplate` / `depNameTemplate`, or a literal
  `(?<depName>…)` capture). As in Renovate, the rule's conditions AND:
  a `matchPackageNames` naming a different package, or a
  `matchManagers` without `custom.regex`, rules it out. The `[PASS]`
  line names the pairing rule and the condition that matched.
- **R5** — every entry in `releases/*/source-refs.yaml` has a paired
  packageRule that disables major bumps (the OpenStack tags rule).
  A new entry that bypasses the rule silently allows major bumps.
- **R6** — per customManager, some `tests/unit/renovate/*_test.sh`
  singles it out. The needles are the manager's file pattern (as a
  path, or its longest literal run), its package and dependency names,
  and the variable-like identifiers of its `matchStrings`
  (`FLUX_OPERATOR_VERSION`, `defaultOVNVersion`); a needle more than
  half the tests mention (`renovate`) is dropped. A test counts when
  the needles it mentions are not all shared by one other manager:
  `hack/install-test-deps.sh` alone fits four managers,
  `hack/install-test-deps.sh` + `KIND_VERSION` only one. The `[PASS]`
  line lists every covering test; uncovered managers fail one by one.
- **R7** — every `<NAME>_VERSION` pin that appears in **both** the
  Makefile and the `ci.yaml` `env:` block carries the same value.
  A drifted pair means local dev and CI run different tool versions
  (e.g. gofumpt formatting locally that `format-check` then rejects).
  Pins present on only one side are `[INFO]`: either single-sourced
  (ci.yaml derives `ENVTEST_K8S_VERSION` from the Makefile via `awk`)
  or PATH-resolved locally (`controller-gen`, `golangci-lint`).
- R4 and R6 need `jq` and report `[INFO]` skipped without it.
- The **inventory** is a review aid: every `<NAME>_VERSION` pin in the
  `Makefile` and in workflow `env:` blocks, each marked as tracked by
  a customManager (named), bumped by hand (no customManager claims it —
  a MEDIUM candidate), or resolved at run time (`${{ … }}`, not a pin).

### 2. Cross-reference the inventory

The script cannot run Renovate itself. Using the printed inventory,
confirm:

1. For each `[FAIL]` from R1–R3, decide whether to add a new
   customManager or normalise the file to match an existing one.
2. For each packageRules `[FAIL]` from R4–R5, add the missing entry
   (with `matchUpdateTypes: [major]` disabled by default, paired
   automerge + 3-day `minimumReleaseAge` for minor/patch, matching
   the existing pattern). For a dead manager or stale pattern, find
   the renamed file (`git log --follow --diff-filter=R`) and re-point
   the pattern, or delete the manager and its rules and test.
3. For each R6 `[FAIL]`, add a sibling test that selects the manager
   the way the existing ones do (`select(.packageNameTemplate ==
   $pkg)`, `select(any(.managerFilePatterns[]; test("…")))`) and runs
   its `matchStrings` against the file on disk.
4. For each inventory pin marked "bumped by hand"
   (`ENVTEST_K8S_VERSION` today), decide whether to add a
   customManager. Some pins are intentionally not auto-bumped —
   document the decision in `renovate.json` (or in a comment beside
   the pin) either way.
5. For each `[FAIL]` from R7, align the two values — and prefer
   eliminating the duplication over patching it: the
   `ENVTEST_K8S_VERSION` pattern (ci.yaml `awk`-reads the Makefile
   pin) makes future drift structurally impossible.

### 3. Run the authoritative gates

The script does not invoke Renovate. Run the real gates and report the
exact outcomes:

```bash
for t in tests/unit/renovate/*_test.sh; do bash "$t"; done
# or the whole shell-test suite, which includes them:
make test-shell
```

These confirm `renovate.json` is valid and that every customManager
still matches the on-disk constants. Trust their outcome over the R1–R6
smoke checks when they disagree. Read the `Results:` line of each test,
not `FAIL` hits in the log (test titles contain the word).

`fluxoperator_custommanager_test.sh` is the test that runs
`renovate-config-validator`, through `npx` at the
`RENOVATE_VALIDATOR_VERSION` it pins (~30 s on a cold cache, network
needed; skipped without `npx`). Locally it is red with
`14 passed, 1 failed` on a clean `main` while CI passes: the
npx-installed Renovate cannot
load its optional `re2` module (`Cannot find module 're2'`), falls back
to JavaScript `RegExp`, and rejects every `matchStrings` entry that
uses the `(?m)` inline flag as `Invalid regExp`. Treat that one failure
as environmental; any other red test is real.

### 4. Report

Produce a concise summary grouped by severity:

- **HIGH** — `renovate-config-validator` fails; a version literal on
  disk is not matched by any manager; a dead customManager or a stale
  pattern (R4); a customManager has no
  paired packageRules entry; a `releases/*/source-refs.yaml` entry
  has no major-bump-disable rule; a `<NAME>_VERSION` pin duplicated
  between the Makefile and ci.yaml carries two different values.
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
   `deploy/kind/base/` with a `version: "…"` line. The existing
   managers are file-name-anchored; a sibling needs its own manager.
5. **Tool pin in Makefile.** No native Renovate manager reads Makefile
   constants. `GOFUMPT_VERSION` got a customManager over `/Makefile$/`
   plus the workflows (`GOFUMPT_VERSION\s*[?=:]+\s*"?(?<currentValue>v…)`,
   one regex for both the `?=` and the `env:` spelling);
   `ENVTEST_K8S_VERSION ?= 1.35` still has none. A new `?=` pin needs
   the same treatment or a comment saying why it is bumped by hand.
6. **Duplicated pin bumped on one side only.** A tool version lives in
   both the Makefile (for local dev) and the ci.yaml `env:` block (for
   the workflow), guarded only by a "Must be kept in sync" comment. A
   bump lands in one file and the two environments quietly diverge —
   gofumpt is the canonical example: local `make fmt` then produces
   formatting that CI's `format-check` rejects (or vice versa). Fix by
   aligning, or better by single-sourcing one side from the other the
   way ci.yaml already `awk`-reads `ENVTEST_K8S_VERSION`.
7. **Half-tracked pin pair: the K-ORC main commit.** Renovate tracks
   the upstream main commit in `deploy/flux-system/sources/k-orc.yaml`
   (`ref.commit`, datasource `git-refs`, digest updates automerged) but
   **not** the controller image in `deploy/flux-system/releases/k-orc.yaml`
   (`spec.images[].newTag: commit-<short sha>` plus `digest`); that
   file's header says so. `hack/ci-deploy-korc.sh` fails CI when the tag
   does not match the pinned commit ("K-ORC image tag … does not match
   the pinned commit"), so a Renovate commit bump stays red until the
   image is re-pinned by hand. Quay `commit-<sha>` tags also expire
   after four weeks, after which the pinned digest returns `NotFound`
   and every leg that deploys K-ORC hangs in `ImagePullBackOff`. R4
   reports the source manager as paired; the gap is the image, which
   no manager claims. Move the pins together with
   [[bump-korc-pin]].
8. **Renamed file, stale pattern.** A pinned file moves (a fixture
   directory renamed, a script split) and the customManager's
   file-anchored regex keeps pointing at the old path. Renovate raises
   no error; the pin just stops receiving updates. R4 reports it as a
   stale pattern, or a dead manager when no pattern matches anything.

## Notes

- This skill is read-only; the deterministic script edits nothing.
  Apply fixes (add the customManager, add the packageRule, add the
  unit test) as a separate, explicitly-scoped task.
- Some pins are *intentionally* not Renovate-tracked (e.g. a tool
  whose upstream release cadence is too aggressive). When the
  decision is to skip, document it in a comment beside the pin (the
  K-ORC image in `deploy/flux-system/releases/k-orc.yaml` is the worked
  example).
- The existing tests under `tests/unit/renovate/` are the source of
  truth for "what is covered today". If you add a customManager, add
  a sibling test there — R6 fails on a manager no test singles out,
  and the report grades that MEDIUM.
- Pair this with [[check-doc-drift]] — that skill checks that
  infrastructure version pins documented in the prose match the
  `deploy/` reality; this skill checks that the pins themselves are
  trackable.
