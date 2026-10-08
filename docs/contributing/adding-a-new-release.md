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
  `neutron-tempest-plugin` for 2025.2 (`<3.1.0`, testtools) and 2026.1
  (`<3.3.0`, `neutron_lib.services.pvlan`) because those releases'
  constraints conflict with newer plugins (the two `packageRules` entries
  whose `matchFileNames` name `releases/2025.2/test-refs.yaml` and
  `releases/2026.1/test-refs.yaml`); 2026.2 needs no hold. Decide per release,
  state the reason in a comment above the pin, and add a `packageRules` entry
  only when a hold is needed. Major updates are disabled under
  `releases/**/test-refs.yaml`, so older releases keep their tempest major.
- **`extra-packages.yaml`**: per-service `pip_extras` / `pip_packages` /
  `apt_packages`; usually carried over unchanged. A new tag can move runtime
  packages into extras (cinder 29.0.0 moved boto3, google-api-python-client
  and python-swiftclient into the `s3`, `gcs` and `swift` extras) or ship a
  wheel that omits a dependency (os-vif 5.2.1 without pyroute2);
  `tests/container-images/verify_<svc>.sh` and `hack/gen-option-catalog.sh`
  show both. Fix it in the new release's block only, so the older images do
  not change.
- **`upper-constraints.txt`**: a snapshot of the upstream `stable/<series>`
  upper constraints file.
- **`test-excludes/<svc>.txt`**: optional stestr exclude lists, consumed by
  `hack/ci-run-unit-tests.sh`. Carry them over per service and re-triage:
  excludes that worked around bugs in the previous series may be fixed
  upstream. Every file's basename must match a `source-refs.yaml` key
  (`tests/container-images/verify_release_config.sh` Test 7). Record the
  first run of each suite in the file header (tag, test count, outcome), as
  `releases/2026.2/test-excludes/*.txt` do. A tag that drops the service's
  test driver (horizon 27.0.0 removed `tools/unit_tests.sh`) needs a branch
  in `hack/ci-run-unit-tests.sh` keyed on the missing file (the
  `[ -f tools/unit_tests.sh ]` test) instead of on `RELEASE`.

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
  upstream. A patch without a twin in the new release says why in its own
  header (`No <version> twin: …`, see
  `.claude/skills/add-image-patch/references/patch-conventions.md`). The
  inventory script reports the service `[DONE]` once every patch of the
  previous release has a twin of the same slug or carries that sentence; a
  service whose patches all carry it needs no directory. The Source patch
  paragraphs of
  [Container Images](../reference/ci-cd/container-images.md) state the new
  release's outcome, patch or none. `test-service-images` runs the upstream
  unit suite against the patched source. The `add-image-patch` skill covers
  the patch format.
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

  The seven `…EmbeddedReleasesParse` tests in
  `operators/<op>/api/v1alpha1/option_catalog_test.go` pin the catalog count
  (`HaveLen(N)`) and keys (`HaveKey`); raise them. Then diff the new catalog
  against the previous release's (run
  `jq -r '.sections | to_entries[] | .key as $s | .value.opts[] | "[\($s)] \(.)"'`
  on both files and compare the sorted outputs with `comm -23`) and resolve
  every option the operator renders (registered in
  `operators/<op>/api/v1alpha1/config_ownership.go`) that the new release
  dropped: gate the rendering on a release predicate and add a
  `reconcile_config` test case per release. `glanceReleaseDropsWorkersOption`
  in `operators/glance/internal/controller/reconcile_deployment.go` and
  `keystoneReleaseEnforcesScopeAlways` in
  `operators/keystone/internal/controller/reconcile_config.go` are the 2026.2
  precedents. An option the catalog dropped that the service still reads
  stays rendered and is pinned by a golden: neutron 29.0.0 still loads
  `[DEFAULT] api_paste_config` through `oslo_service.wsgi.Loader`.

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

The rename reaches only names that carry the previous slug. Four 2026-1
variants (horizon, glance, placement, barbican) name their CR
`<svc>-basic-2026` and their database `<svc>_basic_2026`. A clone that keeps
those names collides with the running sibling, because the suites of one
`e2e-operator` job share the `openstack` Namespace
(`tests/e2e/chainsaw-config.yaml` sets `parallel: 4`). Name every slugged resource `<svc>-basic-<slug>` and
`<svc>_basic_<slug>`, and check with
`grep -rnE 'basic[-_]<year>([^-_0-9]|$)' tests/e2e/*/basic-deployment-<slug>/`.
Add the nova variant to `shard_two` in `.github/workflows/ci.yaml`
(`tests/unit/ci/nova_e2e_matrix_test.sh` asserts every suite runs in one
shard), and add the suite's row, section and tree entry to the reference
pages that list suites
([Keystone E2E Tests](../reference/testing/keystone-e2e-tests.md),
[Cinder E2E Tests](../reference/testing/cinder-e2e-tests.md),
[Nova E2E Tests](../reference/testing/nova-e2e-tests.md)) and the shard list
in [CI Workflow](../reference/ci-cd/ci-workflow.md). A variant that fails on
an operator defect gets its own issue and keeps its assertion.

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
  `chainsaw-test.yaml`;
- assertions that encode a launch mode of the start release, once the start
  moves past that boundary (the glance suite asserted the eventlet
  `glance-api` command while it started below 2026.1);
- the transition cases in `internal/common/release/release_test.go`:
  `TestIsSequentialUpgrade` accepts `<prev> -> <version>` and
  `<version> -> <next>` and rejects the skip from the release before
  `<prev>`, next to the `TestIsDowngrade`, `TestIsPatchOnly` and
  `TestParseRelease` cases.

`tests/e2e/nova/release-upgrade/01-catalog-setup-job.yaml` keeps
`ghcr.io/c5c3/tempest:<default>`, like every other e2e fixture Job.

The skip-level fixture (`upgrade-flow/02-patch-skip-level.yaml`) must keep
targeting a version that is neither under `releases/` nor the sequential
successor of the new release. That is what makes it a rejection test.

`tests/e2e/keystone/upgrade-abort/` needs no move. Its stuck patch pulls the
target from `registry.invalid/…`, so it wedges whatever releases exist; it
only has to start at a release that is still under `releases/`.

The testing pages
([Keystone](../reference/testing/keystone-e2e-tests.md),
[Cinder](../reference/testing/cinder-e2e-tests.md) and
[Nova E2E Tests](../reference/testing/nova-e2e-tests.md)) name the tested
transition and follow the move, as do the `Accepted Transitions` tables of
the glance, cinder and nova upgrade-flow pages and the `Valid Upgrade Paths`
table of [Keystone Upgrade Flow](../reference/keystone/keystone-upgrade-flow.md).

## Tests and docs that count releases

Some tests and pages enumerate the releases instead of discovering them, so
each gains the new release by hand:

- **Go release lists.**
  `grep -rnE '\[\]string\{("[0-9]{4}\.[12]",?[[:space:]]*)+\}' --include='*_test.go' operators internal`
  prints every one-line list, and the inventory script checks that each names
  the release; each gains it. Neither sees a list that spans several lines or
  a table row such as `{release: "2026.1", wantTag: "2026.1"}`: read the hits
  of `grep -rnE --include='*_test.go' 'release:[[:space:]]+"<prev>"' operators internal`
  (table rows, gofmt-aligned ones included) and
  `grep -rnE --include='*_test.go' '^[[:space:]]+"<prev>"[,}]' operators internal`
  (lists that span several lines), and extend the tables and lists that
  enumerate releases.
- **Shell suites that pin counts.**
  `tests/unit/hack/ci_resolve_e2e_images_test.sh` pins the image-map size
  and the per-service release lists, and `tests/unit/ci/nova_e2e_matrix_test.sh`
  the number of images the nova e2e job loads and the number of nova Tempest
  jobs.
- **Prose.** Run
  `grep -rnE -i '(both|two|three|four) (releases|tags)|either release|neither release|(twelve|eighteen|twenty-four|12|18|24) (Tempest )?legs' README.md docs --include='*.md'`
  and read every hit, then grep for `<prev> and <version>` with the real
  versions substituted. When a release changes the count words, add the new
  ones to the pattern. The 2026.2 run rewrote `README.md` and these pages:
  `docs/reference/ci-cd/{ci-workflow,build-images-workflow,container-images}.md`,
  `docs/reference/testing/{cinder-e2e-tests,nova-e2e-tests,tempest-test-infrastructure,sizing-calibration}.md`,
  `docs/reference/placement/index.md`, `docs/reference/neutron/index.md` and
  `docs/reference/nova/nova-crd.md`.

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
- **CI budget.** The first run with the new release records the minutes of
  `build-e2e-images`, every `e2e-operator` job and every Tempest job, and the
  `=== Disk use on the kind node(s) ===` block that
  `hack/ci-dump-diagnostics.sh` prints in each e2e job. A job that overruns
  its `timeout-minutes` or its disk gets its own issue (decision D6 of
  [#1276](https://github.com/C5C3/cobaltcore/issues/1276)).

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
bash .claude/skills/check-service-parity/scripts/audit-service-parity.sh
bash .claude/skills/check-renovate-coverage/scripts/audit-renovate-coverage.sh
make test-shell
make chainsaw-lint
go test ./internal/common/release/...
npm run docs:build
```

Two repository [Claude Code skills](./claude-skills.md) support this
workflow: `prepare-new-release` walks the touch points and decision points
interactively, and `check-release-wiring` is the repeatable audit that
catches missing wiring and orphan references after the fact. The audit's
`--full` mode also runs `verify_release_config.sh` and the CI matrix
generators.
