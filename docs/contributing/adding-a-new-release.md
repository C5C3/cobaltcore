---
title: Adding a New Release
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Adding a New Release

This checklist captures everything a new OpenStack release (e.g. `2026.2`)
touches. Most of the machinery auto-discovers release directories. The CI
build/test matrices scan `releases/*/` (`hack/ci-generate-build-matrix.sh`),
the per-operator release lists and the e2e image loads derive from
`source-refs.yaml` keys (`hack/ci-service-image-releases.sh`), Renovate's
custom managers glob `releases/**` and `overrides/**` (see
[Dependency Management](./dependency-management.md)), and Chainsaw discovers
new e2e suites recursively. The remainder is a finite, hand-enumerated list;
work through it top to bottom.

Every row applies once per service, and the service lists grow as services
are onboarded, so let the inventory script enumerate them for you. It prints
a `[DONE]` or `[TODO]` line per touch point for the target release, plus the
current value of every default-release reference:

```bash
bash .claude/skills/prepare-new-release/scripts/inventory-release-touchpoints.sh 2026.2
```

The version itself needs no code change: `release.ParseRelease`, the
`Pattern` marker on every `OpenStackRelease` field (the ControlPlane CRD and
the service CRDs), and the ControlPlane webhook regexp already accept any
`YYYY.N` with `N` in `{1,2}`. A change to the two-releases-per-year cadence
would have to update all three layers together.

## Release configuration in `releases/<version>/`

Copy the newest existing release directory as the template and adjust every
file:

- **`source-refs.yaml`**: one `service: "<tag>"` line per service, pinned to
  the upstream git tags of the coordinated release. The keys of this file are
  the service list: every key activates build, unit-test, and verify matrix
  entries for the release.
- **`test-refs.yaml`**: PyPI pins for `tempest` and every plugin the newest
  release pins (`keystone-`, `barbican-`, `neutron-`, and
  `cinder-tempest-plugin`), at versions current at the release date that the
  new `upper-constraints.txt` can install. `renovate.json` holds
  `neutron-tempest-plugin` below a ceiling for each existing release because
  newer plugin versions conflict with that release's constraints; decide
  whether the new release needs such a rule too.
- **`extra-packages.yaml`**: per-service `pip_extras` / `pip_packages` /
  `apt_packages`; usually carried over unchanged.
- **`upper-constraints.txt`**: a snapshot of the upstream `stable/<series>`
  upper constraints file.
- **`test-excludes/<svc>.txt`**: optional stestr exclude lists, consumed by
  `hack/ci-run-unit-tests.sh`. Carry them over per service and re-triage:
  excludes that worked around bugs in the previous series may be fixed
  upstream. Every file's basename must match a `source-refs.yaml` key
  (`tests/container-images/verify_release_config.sh` Test 7).

`tests/container-images/verify_release_config.sh` validates the structure of
all of these; run it locally before pushing.

## Build inputs outside `releases/`

Three more per-release inputs live elsewhere in the tree:

- **`overrides/<version>/constraints.txt`** (repo root, required). Carry it
  over from the previous release. `scripts/apply-constraint-overrides.sh`
  merges it into the constraints file before every image build and unit-test
  run. The `-horizon` line strips the `horizon===` pin that upstream
  `upper-constraints.txt` carries, so the source install can build against
  the release ref. The `lhafile===` line pins the LHA reader the glance
  image-import plugin loads, which has no upper-constraints entry of its own;
  `tests/container-images/verify_glance.sh` Test 11 fails when any
  `overrides/*/constraints.txt` lacks that pin.
- **`patches/<svc>/<version>/`**. Downstream source patches, applied by
  `hack/ci-build-service-image.sh` and the image build workflow before the
  install (cinder and glance carry some today). A missing directory builds
  the service unpatched without any error, so re-triage every patch of the
  previous release against the new upstream tag: carry it over while it
  still applies and is still needed, and drop it once the fix ships
  upstream. `test-service-images` runs the upstream unit suite against the
  patched source. The `add-image-patch` skill covers the patch format.
- **Option catalogs**, `operators/<op>/api/v1alpha1/catalogs/<version>.json`,
  one for every operator that has a `catalogs/` directory. The validating
  webhooks check `spec.extraConfig` against them and only warn, without
  validating, for a release that has no catalog. Generate each with
  `hack/gen-option-catalog.sh <op> <version> [image-ref]`, which runs the
  service image's own `oslo-config-generator`. It needs the built image: pass
  a locally built one while `ghcr.io/c5c3/<op>:<version>` is not published
  yet. `verify_release_config.sh` Test 8 requires the keystone and glance
  catalogs, and the build-images workflow's "Verify option catalog" step
  diffs several services' catalogs against a fresh extraction (`--check`).

## Tempest configuration: a hard CI dependency

`hack/ci-generate-tempest-matrix.sh` requires a `tests/tempest/<svc>-<slug>/`
directory (slug = version with `.` → `-`, e.g. `glance-2026-2`) for every
release and every service in its `ALL_TEMPEST_SERVICES` list. It checks all
of them before it emits a single entry, even when `TEMPEST_SERVICES` narrows
the legs that run.

::: warning
A missing directory for any one service fails **every** CI run with
`::error::Missing Tempest config directory`. A keystone-only clone blocks the
whole pipeline.
:::

For each service in `ALL_TEMPEST_SERVICES`, clone
`tests/tempest/<svc>-<prev-slug>/` to `<svc>-<slug>/`. Each directory holds
`tempest.conf`, `include-tests.txt`, `exclude-tests.txt`, and a numbered stack
of CR fixtures: one for keystone, sixteen for cinder, whose leg deploys its
own image, network, placement, and compute services. The previous release
appears in three spellings, and every non-comment line must change:

- `<prev-slug>` (`2026-1`): CR, Service, Secret, and ConfigMap names,
  `app.kubernetes.io/instance` labels, hostnames in Job URLs and
  `keystoneEndpoint` fields, and `uri_v3` in `tempest.conf`;
- `<prev_slug>` (`2026_1`): database names such as
  `keystone_nova_tempest_2026_1`;
- `<prev>` (`2026.1`): every `tag:`, `openStackRelease:`, and
  `ghcr.io/c5c3/<img>:<prev>` reference, the tempest client image and the
  fake compute's nova image included.

A comment-preserving rewrite covers all three (BSD sed; GNU sed takes
`-i` without the empty argument):

```bash
sed -i '' -e '/^[[:space:]]*#/!{s/2026-1/2026-2/g; s/2026_1/2026_2/g; s/2026\.1/2026.2/g;}' \
  tests/tempest/*-2026-2/*
```

Then read the comments by hand, because some describe release-specific
behaviour. The CR names the CI job waits on come from the generator
(`keystone-tempest-<slug>`, `keystone-<svc>-tempest-<slug>`,
`<svc>-tempest-<slug>`, and the helper CRs of the neutron, cinder, and nova
legs), so renaming every slug keeps both sides aligned;
`tests/unit/ci/{neutron,cinder,nova}_e2e_matrix_test.sh` hold them together.
Re-triage `exclude-tests.txt` and `include-tests.txt` against the new plugin
versions (the `re-evaluate-on:` comments name them), and list the new
directories in
[Tempest Test Infrastructure](../reference/testing/tempest-test-infrastructure.md).

## Per-release e2e variants

Every service with a `tests/e2e/<svc>/basic-deployment/` suite needs a
`basic-deployment-<slug>/` variant for each non-default release. Clone
`basic-deployment-<prev-slug>/` and apply the same three-spelling rename: CR
and helper-CR names, databases, the slug-bearing resource names asserted in
`chainsaw-test.yaml`, and the `ghcr.io/c5c3/<svc>:<version>` references in
poke commands and Jobs. Leave `ghcr.io/c5c3/tempest:<default>` alone: the e2e
legs load one tempest tag, and every e2e fixture's Jobs run it whatever
release the suite deploys. A missed rename silently tests the wrong release,
and the suite still passes.

The plain `basic-deployment` suites cover the default release (their
fixtures pin the default tag), so only non-default releases need a suffixed
variant.

## Upgrade path

Every `tests/e2e/<svc>/release-upgrade/` suite and
`tests/e2e/keystone/upgrade-flow/` test the newest sequential transition, so
each moves to `<prev> -> <version>`:

- the start release in the service's own `NN-<svc>-cr.yaml` (`tag:`, plus
  `openStackRelease:` where the kind has one);
- the target in `NN-patch-upgrade.yaml`;
- helper CRs pinned at the target, such as nova's Keystone and Placement;
- the `installedRelease` and `ghcr.io/c5c3/<svc>:<v>` assertions in
  `chainsaw-test.yaml`.

The skip-level fixture (`upgrade-flow/02-patch-skip-level.yaml`) must keep
targeting a version that is neither under `releases/` nor the sequential
successor of the new release. That is what makes it a rejection test.

`tests/e2e/keystone/upgrade-abort/` needs no move. Its stuck patch pulls the
target from `registry.invalid/…`, so it wedges whatever releases exist; it
only has to start at a release that is still under `releases/`.

## Decision points

None of these are mechanical; decide and record each in the PR description:

- **Move the default release?** The default is named in the plain
  `basic-deployment` suites, `deploy/kind/controlplane/controlplane.yaml`
  (`openStackRelease`), the `hack/deploy-infra.sh` image preload, the
  `${VAR:-YYYY.N}` fallbacks in `hack/` (`RELEASE` in
  `ci-build-service-image.sh`, `ci-build-tempest-image.sh`, and
  `run-tempest.sh`; `IMAGE_TAG` in `perf-reconcile-benchmark.sh`), the image
  tags hard-coded in `.github/workflows/ci.yaml` (image-upgrade re-tagging and
  kind preloads), the tempest client image `ghcr.io/c5c3/tempest:<default>`
  that e2e fixture Jobs run, and several hundred release pins across the
  `tests/e2e*/` fixture trees. Moving the default is one coordinated sweep;
  the inventory script prints every current value and the pin count.
- **Retire the oldest release?** See the next section.

## Removing an old release

Deleting `releases/<old>/` shrinks the matrices automatically, but leaves
orphans that must go in the same PR:

- every `tests/tempest/<svc>-<old-slug>/` directory and its row in
  [Tempest Test Infrastructure](../reference/testing/tempest-test-infrastructure.md);
- every `tests/e2e/<svc>/basic-deployment-<old-slug>/` variant (when the old
  release is the default, move the default first and re-pin the plain
  suites);
- `operators/<op>/api/v1alpha1/catalogs/<old>.json` in every operator;
- `overrides/<old>/` and every `patches/<svc>/<old>/`;
- the `tests/unit/renovate/*_test.sh` probes that pin a concrete
  `releases/<old>/` file (repoint them at a surviving release, or
  `make test-shell` breaks) and the per-release `renovate.json` rules whose
  `matchFileNames` name it;
- any upgrade-path suite that still starts at the old release,
  `upgrade-abort` included;
- the placed-services pins in `tests/e2e-multicluster/placed-services/` and
  their ci.yaml `e2e-multicluster` preloads, if they name the old release;
- every default-release reference listed above, if it named the old release;
- the code import of `tests/tempest/glance-2025-2/02-glance-cr.yaml` in
  [Filter web-download Image Imports](../guides/glance/filter-web-download-imports.md),
  when `2025.2` is the release being removed.

## Verification

```bash
bash .claude/skills/prepare-new-release/scripts/inventory-release-touchpoints.sh 2026.2
bash .claude/skills/check-release-wiring/scripts/audit-release-wiring.sh --full
make test-shell
```

Two repository [Claude Code skills](./claude-skills.md) support this
workflow: `prepare-new-release` walks the touch points and decision points
interactively, and `check-release-wiring` is the repeatable audit that
catches missing wiring and orphan references after the fact. The audit's
`--full` mode also runs `verify_release_config.sh` and the CI matrix
generators.
