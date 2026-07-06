---
name: check-release-wiring
description: >-
  Audit whether every OpenStack release under releases/<version>/ is fully
  wired through the forge repo — the mandatory release config files, the
  per-release Tempest config directory the CI matrix generator hard-requires,
  the per-release basic-deployment e2e variant, the default-release
  references in deploy/kind/, hack/, and ci.yaml, the Renovate regression
  tests, the upgrade-path e2e suites, and the version-pattern lockstep
  across CRD marker, webhook, and release.ParseRelease. Use when asked to
  check release wiring, after adding or removing a releases/<version>/
  directory, or before a release when a stale default tag or an orphan
  Tempest directory would fail the pipeline.
---

# Check release wiring

This skill verifies that the set of OpenStack releases under `releases/`
and the rest of the repo **stay in lockstep**: every release directory is
fully wired into CI, tests, and deploy defaults, and no reference points
at a release that no longer (or does not yet) exist.

It is repeatable — run it any time, especially after adding a new
`releases/<version>/` directory, after removing an old one, or before
tagging a release.

## What release wiring means here

The build/test matrices auto-discover releases (`hack/ci-generate-build-matrix.sh`
scans `releases/*/`, services come from `source-refs.yaml` keys, and the
Renovate globs cover `releases/**` without config changes) — but several
touch points are enumerated by hand and drift silently:

| Layer | Where it lives | Source of truth |
|---|---|---|
| Release config files | `releases/<version>/{source-refs,test-refs,extra-packages}.yaml`, `upper-constraints.txt`, `test-excludes/<svc>.txt` | `tests/container-images/verify_release_config.sh` |
| Tempest config | `tests/tempest/keystone-<slug>/` (slug = version with `.` → `-`), four files | `hack/ci-generate-tempest-matrix.sh` — **hard-fails the pipeline** when the directory is missing |
| Per-release e2e variant | `tests/e2e/keystone/basic-deployment-<slug>/` (or the plain `basic-deployment` suite for the default release) | hand-maintained fixtures with hard-coded names and image refs |
| Default-release references | `deploy/kind/controlplane/controlplane.yaml` `openStackRelease`, `hack/deploy-infra.sh` `cp_release`, `RELEASE:-` fallbacks in `hack/ci-build-*.sh` + `hack/run-tempest.sh`, image tags in `.github/workflows/ci.yaml` | the `releases/` directory set |
| Renovate regression tests | `tests/unit/renovate/*_test.sh` reference a concrete `releases/<version>/` file | the `releases/` directory set |
| Upgrade-path e2e | `tests/e2e/keystone/release-upgrade/`, `tests/e2e/keystone/upgrade-flow/` fixture tags | the newest sequential transition implied by the sorted release list |
| Version pattern | `+kubebuilder:validation:Pattern` on `OpenStackRelease`, `controlPlaneReleaseRegexp` in the webhook, `release.ParseRelease` minor guard, generated CRD YAMLs | the three-layer agreement documented at `operators/c5c3/api/v1alpha1/controlplane_types.go` |

The authoritative gates are `tests/container-images/verify_release_config.sh`,
the shell unit tests under `tests/unit/` (`make test-shell`), and the CI
matrix generators themselves. This skill defers to those for file-format
correctness and adds the cross-cutting check they cannot express: "is
every release fully wired, and does every release-shaped reference
resolve?"

A wiring finding is any release directory missing a hand-enumerated touch
point, or any reference to a version that has no `releases/<version>/`
directory.

## Procedure

Work through these steps in order and report findings at the end.

### 1. Run the deterministic audit

```bash
bash .claude/skills/check-release-wiring/scripts/audit-release-wiring.sh
```

The script catches the mechanically-checkable gaps and prints an
inventory. Exit code `1` means at least one `[FAIL]`. Pass `--full` to
chain `verify_release_config.sh` and the CI matrix generators after the
inventory. Interpret:

- **L1** — every `releases/<version>/` carries the four mandatory files
  (`source-refs.yaml`, `test-refs.yaml`, `extra-packages.yaml`,
  `upper-constraints.txt`) and every `test-excludes/*.txt` maps back to
  a `source-refs.yaml` service key. A missing file breaks the image
  build or the release-config gate; an orphan excludes file fails
  `verify_release_config.sh` Test 7.
- **L2** — every release has `tests/tempest/keystone-<slug>/` with the
  four expected files and a CR tag matching the release, and no orphan
  Tempest directory survives a removed release. A missing directory
  makes `hack/ci-generate-tempest-matrix.sh` fail the **whole**
  pipeline with `::error::Missing Tempest config directory`.
- **L3** — every release is covered by a basic-deployment suite: the
  plain suite (whose fixture tag names the default release) or a
  `basic-deployment-<slug>/` variant. Variant fixtures and
  `ghcr.io/...:<tag>` image refs must carry the variant's version —
  these are hand-maintained and rot first.
- **L4** — every default-release reference resolves to an existing
  `releases/<version>/`: the kind quick-start ControlPlane, the
  deploy-infra image preload, the three `RELEASE:-` fallbacks, and
  every `:<YYYY.N>` image tag hard-coded in `ci.yaml` (the
  image-upgrade re-tag and kind preload steps).
- **L5** — the Renovate regression tests under `tests/unit/renovate/`
  reference only existing `releases/<version>/` paths. They pin a
  concrete release file as their probe; removing that release breaks
  `make test-shell`.
- **L6** — `release-upgrade` and `upgrade-flow` test exactly the newest
  sequential transition (second-newest → newest), and the skip-level
  fixture still targets a version that does **not** exist under
  `releases/` (otherwise the rejection test no longer tests a skip).
- **L7** — the version pattern is identical across the CRD marker, the
  webhook regexp, and both generated CRD YAMLs, and
  `release.ParseRelease` still enforces the `{1,2}` minor set the
  `[12]` character class encodes.

### 2. Cross-reference the inventory

The script checks shapes, not semantics. Using the printed inventory,
confirm by hand:

1. For each release, that the `source-refs.yaml` component versions are
   the intended upstream tags for that OpenStack series (the script
   cannot know that 2026.1 should carry keystone 29.x).
2. That `upper-constraints.txt` was snapshotted from the matching
   upstream `stable/<series>` branch, and that any repo-root
   `overrides/<version>/constraints.txt` entries are still needed.
3. That `ci.yaml`'s special-cased jobs (image-upgrade re-tagging, the
   `keystone:<version>` kind preloads) still name the release the
   fixtures actually use — the script verifies existence, not intent.
4. When the default release moves: sweep `docs/` examples for the old
   tag (display-only drift; [[check-doc-drift]] covers the load-bearing
   pages).

### 3. Run the authoritative gates

```bash
bash tests/container-images/verify_release_config.sh
make test-shell
GITHUB_OUTPUT=/dev/null GITHUB_EVENT_NAME=pull_request bash hack/ci-generate-build-matrix.sh
GITHUB_OUTPUT=/dev/null bash hack/ci-generate-tempest-matrix.sh
```

Trust these over the L1–L7 smoke checks when they disagree.

### 4. Report

Produce a concise summary grouped by severity:

- **HIGH** — a release with no Tempest config directory (pipeline-wide
  CI failure); a default-release reference or renovate test pointing at
  a non-existent release; a version-pattern divergence between marker,
  webhook, or generated CRDs.
- **MEDIUM** — a release without basic-deployment e2e coverage; an
  upgrade-path suite not covering the newest transition; a variant
  fixture pinning the wrong tag or image ref; an orphan Tempest or
  variant directory left behind by a removed release.
- **LOW** — a service without a `test-excludes` file where siblings
  have one; service sets differing across releases without a recorded
  reason; stale release examples in prose.

For each finding give one line with a `file:line` reference for both
the release side and the referencing side. End with a wired / not-wired
verdict per release.

## Drift patterns

These recurring shapes are worth grepping for first:

1. **New release, no Tempest directory.** `releases/2026.2/` lands but
   `tests/tempest/keystone-2026-2/` does not — the matrix generator
   hard-fails and blocks every CI run, not just Tempest.
2. **Removed release, orphan references.** `releases/2025.2/` is
   deleted but the plain basic-deployment fixture, the kind
   ControlPlane default, the `RELEASE:-2025.2` fallbacks, and the
   renovate unit tests still name it.
3. **Variant fixture cloned without retagging.** A
   `basic-deployment-<slug>/` variant copied from the previous one
   keeps the old `tag:` or the old `ghcr.io/...:<tag>` poke-command
   ref — the suite silently tests the wrong release.
4. **Upgrade suites stuck on the previous transition.** A new release
   is added but `release-upgrade`/`upgrade-flow` still test the old
   pair, so the newest sequential upgrade ships untested.
5. **Pattern widened in one layer only.** A future `YYYY.3` cadence
   change edits `release.ParseRelease` but not the CRD marker (or vice
   versa) — the API server and the webhook disagree about which
   versions are valid.

## Notes

- This skill is read-only; the deterministic script edits nothing.
  Apply fixes (create the Tempest directory, retag the fixture, move
  the default) as a separate, explicitly-scoped task — [[prepare-new-release]]
  is the guided path for adding a release.
- L2 and L3 mirror today's keystone-hardcoded contracts
  (`ci-generate-tempest-matrix.sh` only requires `keystone-<slug>`;
  only keystone has per-release e2e variants). When those generalize
  to more services, extend the audit script's L2/L3 loops.
- Pair this with [[check-renovate-coverage]] — that skill checks the
  release pins are *trackable* by Renovate; this skill checks the
  release directories are *wired*. And with [[check-doc-drift]] for
  the prose that references release versions.
