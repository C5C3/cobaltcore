---
name: prepare-new-release
description: >-
  Analyze and prepare the addition of a new OpenStack release (e.g. 2026.2)
  into CobaltCore — inventory the touch points the auto-discovery does not
  cover (release config files under releases/<version>/, per-operator option
  catalogs, constraint overrides and service patches, the Tempest config
  directory of every Tempest-covered service that the CI matrix generator
  hard-requires, every service's per-release basic-deployment e2e variant,
  the upgrade-path suites), and walk the decision points: moving the default
  release and retiring an old release. Use when asked to add or onboard a
  new OpenStack release, to bump the release matrix, or to remove an old
  release from the repo.
---

# Prepare a new OpenStack release

This skill turns "add OpenStack release X" into a **complete, ordered
change list**. Most of the release machinery auto-discovers new
directories under `releases/` — the failure mode is the hand-enumerated
remainder, which this skill walks explicitly. It analyzes and guides;
`docs/contributing/adding-a-new-release.md` is the contributor-facing
checklist, and where it and this list disagree, the inventory and
[[check-release-wiring]] are the ones verified against HEAD.

## What auto-extends and what does not

The repo carries eight services (`source-refs.yaml` keys), six
Tempest-covered services (eighteen legs across three releases), seven
`release-upgrade` suites, and seven operators with option catalogs. Every
hand-maintained row applies once per service.

| Touch point | Mechanism | Auto-extends? |
|---|---|---|
| Build/test/verify matrices | `hack/ci-generate-build-matrix.sh` scans `releases/*/`, services from `source-refs.yaml` keys | **yes** |
| Per-operator release list, e2e image loads | `hack/ci-service-image-releases.sh` — the e2e legs load `<svc>:<release>` for every release | **yes** |
| Renovate tracking | globs over `releases/**/source-refs.yaml` + `test-refs.yaml` and `overrides/**/constraints.txt` in `renovate.json` | **yes** (per-release `packageRules` holds are not) |
| Chainsaw suite discovery | `make e2e` finds every `chainsaw-test.yaml` recursively | **yes** (once the suite exists) |
| Version validity | `release.ParseRelease`, eight CRD `Pattern` markers, the ControlPlane webhook regexp all accept `YYYY.[12]` | **yes** for the next cadence release (a `YYYY.3` changes all of them) |
| Release config files | `releases/<version>/{source-refs,test-refs,extra-packages}.yaml`, `upper-constraints.txt`, `test-excludes/` | **no** — created by hand |
| Option catalogs | `operators/<op>/api/v1alpha1/catalogs/<version>.json` via `hack/gen-option-catalog.sh <op> <version>` (needs the built image) | **no** — the verifier (Test 8) and build-images `--check` fail without them |
| Constraint overrides | `overrides/<version>/constraints.txt` (`-horizon` strip, `lhafile===` pin) via `scripts/apply-constraint-overrides.sh` | **no** — carried over per release |
| Service patches | `patches/<svc>/<version>/*.patch` (cinder, glance today) | **no** — a missing dir builds the service unpatched, silently |
| Tempest config | `tests/tempest/<svc>-<slug>/` for every service in `ALL_TEMPEST_SERVICES` of `hack/ci-generate-tempest-matrix.sh` (keystone, glance, barbican, neutron, cinder, nova) — the generator **hard-fails the whole pipeline** when any one is missing | **no** — six multi-CR directories |
| Per-release e2e variant | `tests/e2e/<svc>/basic-deployment-<slug>/` for each of the eight services | **no** — hand-cloned, hard-coded names and image refs |
| Upgrade-path e2e | seven `release-upgrade/`, keystone `upgrade-flow/` and `upgrade-abort/` | **no** — must move to the newest transition |
| Placed-services release pins | `tests/e2e-multicluster/placed-services/*.yaml` and the ci.yaml `e2e-multicluster` preloads | **no** — one pinned release; bumped only when that pin moves |
| Default-release references | kind ControlPlane, `deploy-infra` preload, `${VAR:-YYYY.N}` fallbacks in `hack/`, `ci.yaml` image tags, the eight plain `basic-deployment` suites | **no** — a decision, not a mechanical bump |

## Procedure

### 1. Run the deterministic inventory

```bash
bash .claude/skills/prepare-new-release/scripts/inventory-release-touchpoints.sh 2026.2
```

It prints `[DONE]`/`[TODO]` per touch point for the target version plus
the global decision points with their current values. For a fresh
release everything is `[TODO]` by design; re-run it mid-effort to catch
partial wiring. Without an argument it inventories every existing
release. It exits `1` when `ALL_TEMPEST_SERVICES` cannot be parsed from
the generator — it never guesses the Tempest service list.

### 2. Create the release config

Copy `releases/<newest>/` as the template and adjust:

- **`source-refs.yaml`** — one `service: "<tag>"` line per service, using
  the upstream git tags of the new coordinated release. This file's keys
  *are* the service list: every key activates build/test/verify matrix
  entries for the release.
- **`test-refs.yaml`** — `tempest` and every plugin the newest release
  pins (keystone, barbican, neutron, cinder `-tempest-plugin`) at
  versions current at the release date that the new
  `upper-constraints.txt` can install. `renovate.json` holds
  `neutron-tempest-plugin` for 2025.2 (`<3.1.0`, testtools) and 2026.1
  (`<3.3.0`, `neutron_lib.services.pvlan`); 2026.2 needs none. Decide per
  release, state the reason in a comment above the pin, and add a
  `packageRules` entry only when a hold is needed. Majors are disabled
  under `releases/**/test-refs.yaml`, so older releases keep their
  tempest major.
- **`extra-packages.yaml`** — usually carried over; per-service
  `pip_extras`/`pip_packages`/`apt_packages`. A new tag can move runtime
  packages into extras (cinder 29.0.0: boto3, google-api-python-client,
  python-swiftclient into `s3`/`gcs`/`swift`) or ship a wheel without a
  dependency (os-vif 5.2.1 without pyroute2); `verify_<svc>.sh` and
  `hack/gen-option-catalog.sh` show both. Fix it in the new release's
  block only.
- **`upper-constraints.txt`** — snapshot of the upstream
  `stable/<series>` upper constraints.
- **`test-excludes/<svc>.txt`** — carry over per service and re-triage:
  an exclude that was a workaround for the previous series may be fixed
  upstream. Record the first run of each suite in the file header (tag,
  test count, outcome), as `releases/2026.2/test-excludes/*.txt` do. A
  tag that drops the test driver (horizon 27.0.0 removed
  `tools/unit_tests.sh`) needs a branch in `hack/ci-run-unit-tests.sh`
  keyed on the missing file, not on `RELEASE`.
- **`overrides/<version>/constraints.txt`** (repo root) — carry over:
  the `-horizon` line strips the pin horizon has in its own
  upper-constraints, and `lhafile===` pins the glance import plugin's
  reader (`tests/container-images/verify_glance.sh` Test 11).
- **`patches/<svc>/<version>/`** — re-triage every patch of the newest
  release against the new upstream tag: carry it over (it must still
  apply, and `test-service-images` runs the upstream unit suite against
  the patched source) or drop it once the fix landed upstream. A patch
  with no twin in the new release says why in its header
  (`No <version> twin: …`, [[add-image-patch]]
  `references/patch-conventions.md`). The inventory reports the service
  `[DONE]` once every patch has a same-slug twin or carries that sentence;
  a service whose patches all carry it needs no directory. The Source patch
  paragraphs of `docs/reference/ci-cd/container-images.md` state the
  outcome.
- **Option catalogs** — once the new service images build, run
  `hack/gen-option-catalog.sh <op> <version>` for every operator with a
  `catalogs/` directory (barbican, cinder, glance, keystone, neutron,
  nova, placement). Raise the `HaveLen(N)` count and add the `HaveKey`
  of the seven `…EmbeddedReleasesParse` tests in
  `operators/<op>/api/v1alpha1/option_catalog_test.go`. Diff the new
  catalog against the previous one
  (`jq -r '.sections | to_entries[] | .key as $s | .value.opts[] | "[\($s)] \(.)"'`
  on both, sorted, `comm -23`) and resolve every dropped option the
  operator renders (`config_ownership.go`): gate it on a release
  predicate (`glanceReleaseDropsWorkersOption`,
  `keystoneReleaseEnforcesScopeAlways`) with a `reconcile_config` case
  per release. An option the catalog dropped but the service still reads
  (neutron `[DEFAULT] api_paste_config` at 29.0.0) stays rendered, pinned
  by a golden.

### 3. Create the test wiring

- **Tempest, every service** — for each service in `ALL_TEMPEST_SERVICES`,
  clone `tests/tempest/<svc>-<prev-slug>/` to `<svc>-<slug>/`. The
  directories are multi-CR stacks (keystone 1 fixture, nova 15, cinder
  16), and the previous release appears in three forms, all of which
  must change on every non-comment line:
  - `<prev-slug>` (`2026-1`) — CR, Service, Secret, and ConfigMap names,
    `app.kubernetes.io/instance` labels, hostnames in Job URLs and
    `keystoneEndpoint` fields, and `uri_v3` in `tempest.conf`;
  - `<prev_slug>` (`2026_1`) — database names (`keystone_nova_tempest_2026_1`,
    `nova_tempest_2026_1_api`);
  - `<prev>` (`2026.1`) — every `tag:`, `openStackRelease:`, and
    `ghcr.io/c5c3/<img>:<prev>` ref, the tempest client image and the
    fake compute's nova image included (the Tempest legs load
    `tempest:<matrix.release>`).

  A comment-preserving rewrite does all three:
  `sed -i '' -e '/^[[:space:]]*#/!{s/2026-1/2026-2/g; s/2026_1/2026_2/g; s/2026\.1/2026.2/g;}'`
  (GNU sed: `sed -i`). Then read the comments by hand — some state
  release-specific behavior (glance `02-glance-cr.yaml`: "2026.1 launches
  Glance under uWSGI"). The CR names the CI job waits on come from the
  generator, not from the fixtures, so they must match its header
  exactly: `keystone-tempest-<slug>` for the keystone leg;
  `keystone-<svc>-tempest-<slug>` and `<svc>-tempest-<slug>` for every
  other leg; plus `ovn-neutron-tempest-<slug>` (neutron),
  `glance-`, `nova-`, `placement-`, `neutron-`, `ovn-cinder-tempest-<slug>`
  (cinder), and `placement-`, `neutron-`, `ovn-`, `glance-nova-tempest-<slug>`
  (nova). A clean slug rename keeps them aligned;
  `tests/unit/ci/{neutron,cinder,nova}_e2e_matrix_test.sh` and
  [[check-release-wiring]] L2 hold the two sides together. Re-triage
  `exclude-tests.txt` / `include-tests.txt` against the new plugin
  versions (`re-evaluate-on:` comments), and add the directories to
  `docs/reference/testing/tempest-test-infrastructure.md`.
- **Per-release e2e variant, every service** — for each service with a
  `tests/e2e/<svc>/basic-deployment/` suite, clone
  `basic-deployment-<prev-slug>/` to `basic-deployment-<slug>/` with the
  same three-form rename (CR and helper-CR names, databases,
  `keystone-basic-<slug>-*`-style assertions in `chainsaw-test.yaml`,
  `ghcr.io/c5c3/<svc>:<version>` poke-command refs). Leave
  `ghcr.io/c5c3/tempest:2025.2` alone: the e2e legs load that one tempest
  tag for every suite. A missed rename silently tests the wrong release.
  The rename reaches only names that carry the previous slug: the horizon,
  glance, placement and barbican 2026-1 variants name their CR
  `<svc>-basic-2026` and database `<svc>_basic_2026`, and a clone that
  keeps them collides with the running sibling in the shared `openstack`
  Namespace (`parallel: 4`). Name them `<svc>-basic-<slug>` /
  `<svc>_basic_<slug>`; check with
  `grep -rnE 'basic[-_]<year>([^-_0-9]|$)' tests/e2e/*/basic-deployment-<slug>/`.
  Add the nova variant to `shard_two` in `ci.yaml`
  (`tests/unit/ci/nova_e2e_matrix_test.sh` asserts one shard per suite),
  and add the suite's row, section and tree entry to
  `docs/reference/testing/{keystone,cinder,nova}-e2e-tests.md` and the
  shard list in `docs/reference/ci-cd/ci-workflow.md`. A variant that
  fails on an operator defect gets its own issue and keeps its assertion.
  [[check-service-parity]] P6 also requires the latest-release variant
  for every service.
- **Upgrade path** — move every `tests/e2e/<svc>/release-upgrade/` and
  `tests/e2e/keystone/upgrade-flow/` to `<prev> -> <version>`: the
  service's own `NN-<svc>-cr.yaml` (`tag:` + `openStackRelease:`), the
  `NN-patch-upgrade.yaml` target, helper CRs pinned at the target
  (nova's Keystone and Placement), the `installedRelease` /
  `ghcr.io/c5c3/<svc>:<v>` assertions in `chainsaw-test.yaml`, and any
  assertion that encodes a launch mode of the start release once the
  start moves past that boundary (glance asserted the eventlet command
  below 2026.1). `tests/e2e/nova/release-upgrade/01-catalog-setup-job.yaml`
  keeps `ghcr.io/c5c3/tempest:<default>`.
  `tests/e2e/keystone/upgrade-abort/` needs no move: its stuck patch pulls
  the target from `registry.invalid/…`, so it wedges whatever releases
  exist; it only has to start at a release that is still wired (move it
  when that release is retired). Keep the skip-level target
  (`upgrade-flow/02-patch-skip-level.yaml`) neither wired nor the
  sequential successor of `<version>`. Add the transition cases to
  `internal/common/release/release_test.go` (`TestIsSequentialUpgrade`
  accepts `<prev> -> <version>` and `<version> -> <next>` and rejects the
  skip from the release before `<prev>`; `TestIsDowngrade`,
  `TestIsPatchOnly`, `TestParseRelease`), and move the tested transition
  on `docs/reference/testing/{keystone,cinder,nova}-e2e-tests.md`, the
  `Accepted Transitions` tables of the glance, cinder and nova
  upgrade-flow pages and keystone's `Valid Upgrade Paths` table.
- **Tests and docs that count releases** — every one-line Go release list
  (`grep -rnE '\[\]string\{("[0-9]{4}\.[12]",?[[:space:]]*)+\}' --include='*_test.go' operators internal`)
  gains the release; table rows
  (`grep -rnE --include='*_test.go' 'release:[[:space:]]+"<prev>"' operators internal`)
  and multi-line lists
  (`grep -rnE --include='*_test.go' '^[[:space:]]+"<prev>"[,}]' operators internal`)
  need a read; `tests/unit/hack/ci_resolve_e2e_images_test.sh` and
  `tests/unit/ci/nova_e2e_matrix_test.sh` pin image and leg counts; the
  prose grep of `docs/contributing/adding-a-new-release.md` finds the
  pages that count releases.

### 4. Walk the decision points

None of these are mechanical; decide and record each:

- **Move the default release?** The eight plain `basic-deployment`
  suites, the kind quick-start ControlPlane
  (`deploy/kind/controlplane/controlplane.yaml`), the
  `hack/deploy-infra.sh` preload, the `${VAR:-YYYY.N}` fallbacks
  (`RELEASE` in three `hack/` scripts, `IMAGE_TAG` in
  `perf-reconcile-benchmark.sh`), the `ci.yaml` image-tag pins across
  eight e2e jobs, the tempest client image the e2e fixtures run, and
  ~900 fixture pins under `tests/e2e*/` all name the default. Moving it
  is one coordinated sweep — the inventory prints every current value
  and the pin count.
- **Retire the oldest release?** Deleting `releases/<old>/` auto-shrinks
  the matrices, but leaves orphans: six Tempest directories, eight e2e
  variants (or, when the default retires, eight plain suites to re-pin),
  seven option catalogs, `overrides/<old>/`, `patches/*/<old>/`, the
  placed-services pins and their ci.yaml preloads, the
  `tests/unit/renovate/` probes and the `renovate.json` hold that name
  `releases/<old>/`, and the docs code-import of
  `tests/tempest/glance-2025-2/02-glance-cr.yaml` in
  `docs/guides/glance/filter-web-download-imports.md`. Run
  [[check-release-wiring]] after the removal — its L1–L6 checks exist
  precisely for this.
- **CI budget** (#1276 D6) — the first run with the new release records
  the minutes of `build-e2e-images`, every `e2e-operator` leg and every
  Tempest leg, and the `=== Disk use on the kind node(s) ===` block of
  `hack/ci-dump-diagnostics.sh` in each e2e leg. A leg that overruns its
  `timeout-minutes` or its disk gets its own issue.

### 5. Verify

```bash
bash .claude/skills/prepare-new-release/scripts/inventory-release-touchpoints.sh <version>
bash .claude/skills/check-release-wiring/scripts/audit-release-wiring.sh --full
bash .claude/skills/check-service-parity/scripts/audit-service-parity.sh
bash .claude/skills/check-renovate-coverage/scripts/audit-renovate-coverage.sh
make chainsaw-lint
go test ./internal/common/release/...
npm run docs:build
```

plus `make test-shell` and a full `make e2e` against a kind cluster when
the change moves defaults. [[check-release-wiring]] is the repeatable
audit for everything this skill sets up; run it as the gate on the PR.

## Known gotchas (verified 2026-10-08, re-verify at HEAD)

- **The Tempest matrix covers six services** —
  `hack/ci-generate-tempest-matrix.sh` checks every
  `tests/tempest/<svc>-<slug>/` of `ALL_TEMPEST_SERVICES` for every
  release, even when `TEMPEST_SERVICES` narrows the emitted legs, so a
  keystone-only clone fails every CI run. Horizon (chainsaw HTTP
  assertions) and placement (exercised by the nova and cinder legs) have
  no leg of their own.
- **`upgrade-abort` must not depend on a missing image** — it wedges on
  the next sequential release, and the e2e leg loads the image of every
  wired release. It used to rely on no 2026.2 image existing; its stuck
  patch now points `spec.image.repository` at `registry.invalid/…` (and
  the abort patch restores the real repository), so adding a release
  cannot break it. Keep that shape if you touch the suite.
- **`verify_release_config.sh` Test 7** rejects any
  `test-excludes/*.txt` whose basename is not a `source-refs.yaml` key —
  add the service key first, the excludes file second.
- **Option catalogs need the built image** — generate them after the
  service images build; until then Test 8 fails for keystone and glance
  and the webhooks skip `extraConfig` validation for the new release.
- **YYYY.N sorts lexicographically** only because the year is
  four-digit and N single-digit — scripts rely on plain `sort`.
- **A `YYYY.3` cadence change is rejected in three layers** —
  `release.ParseRelease`, the eight `OpenStackRelease` `Pattern` markers,
  and `controlPlaneReleaseRegexp` must change together (plus CRD regen).
- **The renovate regression tests probe concrete releases**
  (`tests/unit/renovate/*_test.sh`, including the per-release
  `neutron-tempest-plugin` holds) — when a probed release is retired,
  repoint the probe before deleting the directory or `make test-shell`
  breaks.
- **Go tests enumerate releases** — eight `[]string{"2025.2", "2026.1"}`
  loops and five nova pin goldens skipped 2026.2 until #1281; the grep in
  step 3 finds every one-line `[]string` list, and the inventory checks
  those. Neither sees a multi-line list or a `{release: "…"}` table row.
- **Two shell suites pin counts** —
  `tests/unit/hack/ci_resolve_e2e_images_test.sh` (image-map size,
  per-service release lists) and `tests/unit/ci/nova_e2e_matrix_test.sh`
  (nova ref count, nova Tempest leg count) move with every release.
- **Packages move into extras, wheels drop dependencies** — cinder
  29.0.0's backup libraries and os-vif 5.2.1's pyroute2; fix each in the
  new release's `extra-packages.yaml` block so older images stay as they
  are.
- **A tag can drop the unit-test driver** — horizon 27.0.0 removed
  `tools/unit_tests.sh`; `hack/ci-run-unit-tests.sh` branches on the
  missing file, not on the release.
- **e2e variants whose names carry no slug** — `<svc>-basic-2026` in four
  2026-1 variants keeps its name through the slug rename and collides with
  the running sibling; name every resource `<svc>-basic-<slug>`.

## Notes

- This skill is read-only with respect to the codebase; its output is
  the ordered change list (and, when asked, a meta issue in the style
  of [[prepare-new-service]]).
- Renovate needs **no** manager change for a new release — but run
  [[check-renovate-coverage]] afterwards to confirm the new files'
  entries match the customManager regexes (style drift like a `v`
  prefix falls out of the match set silently).
- Pair with [[check-doc-drift]] when the default release moves — docs
  examples reference the default tag in many places.
