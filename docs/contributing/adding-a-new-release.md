---
title: Adding a New Release
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Adding a New Release

This checklist captures everything a new OpenStack release (e.g. `2026.2`)
touches. Most of the machinery auto-discovers release directories — the CI
build/test matrices scan `releases/*/` (`hack/ci-generate-build-matrix.sh`),
the per-operator release lists derive from `source-refs.yaml` keys
(`hack/ci-service-image-releases.sh`), Renovate's custom managers glob
`releases/**` (see [Dependency Management](./dependency-management.md)), and
Chainsaw discovers new e2e suites recursively. The remainder is a finite,
hand-enumerated list — work through it top to bottom.

The version itself needs no code change: `release.ParseRelease`, the
ControlPlane CRD `Pattern` marker, and the webhook regexp already accept any
`YYYY.N` with `N` in `{1,2}`. (A change to the two-releases-per-year cadence
would have to update all three together.)

## Release configuration — `releases/<version>/`

Copy the newest existing release directory as the template and adjust every
file:

- **`source-refs.yaml`** — one `service: "<tag>"` line per service, pinned to
  the upstream git tags of the coordinated release. The keys of this file
  *are* the service list: every key activates build, unit-test, and verify
  matrix entries for the release.
- **`test-refs.yaml`** — PyPI pins for `tempest` and
  `keystone-tempest-plugin` current at the release date.
- **`extra-packages.yaml`** — per-service `pip_extras` / `pip_packages` /
  `apt_packages`; usually carried over unchanged.
- **`upper-constraints.txt`** — a snapshot of the upstream
  `stable/<series>` upper constraints file.
- **`test-excludes/<svc>.txt`** — optional stestr exclude lists, consumed by
  `hack/ci-run-unit-tests.sh`. Carry them over per service and re-triage:
  excludes that worked around bugs in the previous series may be fixed
  upstream. Every file's basename must match a `source-refs.yaml` key
  (`tests/container-images/verify_release_config.sh` Test 7).
- **`overrides/<version>/constraints.txt`** (repo root, not under
  `releases/`) — needed only when a service is pinned inside its own
  upper-constraints (e.g. `horizon===…`): a `-<svc>` line strips the pin so
  the source install can build against the release ref
  (`scripts/apply-constraint-overrides.sh`).

`tests/container-images/verify_release_config.sh` validates the structure of
all of these; run it locally before pushing.

## Tempest configuration — hard CI dependency

Create `tests/tempest/keystone-<slug>/` (slug = version with `.` → `-`, e.g.
`keystone-2026-2`) containing `00-keystone-cr.yaml`, `exclude-tests.txt`,
`include-tests.txt`, and `tempest.conf`. The CR uses the name
`keystone-tempest-<slug>`, its own database, and `tag: "<version>"`.

::: warning
`hack/ci-generate-tempest-matrix.sh` fails **every** CI run with
`::error::Missing Tempest config directory` when this directory is absent —
it is the one touch point that blocks the whole pipeline, not just one job.
:::

## Per-release e2e variant

Clone `tests/e2e/keystone/basic-deployment-<prev-slug>/` to
`basic-deployment-<slug>/` and rename everything that embeds the version:
the CR name and database in `00-keystone-cr.yaml`, every
`keystone-basic-<slug>-*` resource assertion in `chainsaw-test.yaml`, and
the `ghcr.io/c5c3/keystone:<version>` image reference in the poke command.
A missed rename silently tests the wrong release — the suite still passes.

The plain `basic-deployment` suite covers the *default* release (its fixture
pins the default tag), so only non-default releases need a suffixed variant.

## Decision points

None of these are mechanical; decide and record each in the PR description:

- **Move the default release?** The default is named in the plain
  `basic-deployment` fixture, `deploy/kind/controlplane/controlplane.yaml`
  (`openStackRelease`), the `hack/deploy-infra.sh` image preload, the
  `RELEASE:-` fallbacks in `hack/ci-build-service-image.sh`,
  `hack/ci-build-tempest-image.sh`, and `hack/run-tempest.sh`, and the
  image-tag pins in `.github/workflows/ci.yaml` (image-upgrade re-tagging
  and kind preloads). Moving the default is one coordinated sweep across
  all of them.
- **Extend the upgrade path?** `tests/e2e/keystone/release-upgrade/` and
  `tests/e2e/keystone/upgrade-flow/` must test the newest sequential
  transition (second-newest → newest release). The skip-level fixture
  (`upgrade-flow/02-patch-skip-level.yaml`) must keep targeting a version
  that does **not** exist under `releases/` — that is what makes it a
  rejection test.

## Removing an old release

Deleting `releases/<old>/` shrinks the matrices automatically, but leaves
orphans that must go in the same PR:

- `tests/tempest/keystone-<old-slug>/`
- `tests/e2e/keystone/basic-deployment-<old-slug>/` (if the old release was
  not the default)
- every default-release reference listed above, if it named the old release
- the `tests/unit/renovate/*_test.sh` probes, which pin a concrete
  `releases/<version>/` file — repoint them at a surviving release or
  `make test-shell` breaks

## Verification

```bash
bash .claude/skills/check-release-wiring/scripts/audit-release-wiring.sh --full
make test-shell
```

Two repository [Claude Code skills](./claude-skills.md) support this
workflow: `prepare-new-release` walks the touch points and decision points
interactively, and `check-release-wiring` is the repeatable audit that
catches missing wiring and orphan references after the fact.
