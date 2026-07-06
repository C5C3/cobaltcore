---
name: prepare-new-release
description: >-
  Analyze and prepare the addition of a new OpenStack release (e.g. 2026.2)
  into forge — inventory the touch points the auto-discovery does not cover
  (release config files under releases/<version>/, the Tempest config
  directory the CI matrix generator hard-requires, per-release e2e variants,
  constraint overrides), and walk the decision points: moving the default
  release, extending the upgrade-path tests, and retiring an old release.
  Use when asked to add or onboard a new OpenStack release, to bump the
  release matrix, or to remove an old release from the repo.
---

# Prepare a new OpenStack release

This skill turns "add OpenStack release X" into a **complete, ordered
change list**. Most of the release machinery auto-discovers new
directories under `releases/` — the failure mode is the hand-enumerated
remainder, which this skill walks explicitly. It analyzes and guides; the
edits themselves follow `docs/contributing/adding-a-new-release.md`.

## What auto-extends and what does not

| Touch point | Mechanism | Auto-extends? |
|---|---|---|
| Build/test/verify matrices | `hack/ci-generate-build-matrix.sh` scans `releases/*/`, services from `source-refs.yaml` keys | **yes** |
| Per-operator release list | `hack/ci-service-image-releases.sh` | **yes** |
| Renovate tracking | globs over `releases/**/source-refs.yaml` + `test-refs.yaml` in `renovate.json` | **yes** |
| Chainsaw suite discovery | `make e2e` finds every `chainsaw-test.yaml` recursively | **yes** (once the suite exists) |
| Version validity | `release.ParseRelease`, CRD pattern, webhook regexp all accept `YYYY.[12]` | **yes** for the next cadence release (a `YYYY.3` would need all three changed) |
| Release config files | `releases/<version>/{source-refs,test-refs,extra-packages}.yaml`, `upper-constraints.txt`, `test-excludes/` | **no** — created by hand |
| Tempest config | `tests/tempest/keystone-<slug>/` — `hack/ci-generate-tempest-matrix.sh` **hard-fails the whole pipeline** without it | **no** |
| Per-release e2e variant | `tests/e2e/keystone/basic-deployment-<slug>/` | **no** — hand-cloned, hard-coded names and image refs |
| Constraint overrides | repo-root `overrides/<version>/constraints.txt` via `scripts/apply-constraint-overrides.sh` | **no** — only when a service is pinned in its own upper-constraints |
| Default-release references | kind ControlPlane, `deploy-infra` preload, `RELEASE:-` fallbacks, `ci.yaml` image tags | **no** — a decision, not a mechanical bump |
| Upgrade-path e2e | `release-upgrade/`, `upgrade-flow/` fixture tags | **no** — must move to the newest transition |

## Procedure

### 1. Run the deterministic inventory

```bash
bash .claude/skills/prepare-new-release/scripts/inventory-release-touchpoints.sh 2026.2
```

It prints `[DONE]`/`[TODO]` per touch point for the target version plus
the global decision points with their current values. For a fresh
release everything is `[TODO]` by design; re-run it mid-effort to catch
partial wiring. Without an argument it inventories every existing
release.

### 2. Create the release config files

Copy `releases/<newest>/` as the template and adjust:

- **`source-refs.yaml`** — one `service: "<tag>"` line per service, using
  the upstream git tags of the new coordinated release. This file's keys
  *are* the service list: every key activates build/test/verify matrix
  entries for the release.
- **`test-refs.yaml`** — the `tempest` and `keystone-tempest-plugin`
  PyPI pins current at the release date.
- **`extra-packages.yaml`** — usually carried over; per-service
  `pip_extras`/`pip_packages`/`apt_packages`.
- **`upper-constraints.txt`** — snapshot of the upstream
  `stable/<series>` upper constraints.
- **`test-excludes/<svc>.txt`** — carry over per service and re-triage:
  an exclude that was a workaround for the previous series may be fixed
  upstream.
- **`overrides/<version>/constraints.txt`** (repo root) — only when a
  service is pinned inside its own upper-constraints (the inventory
  warns): a `-<svc>` line strips the pin so the source install can
  proceed.

### 3. Create the test wiring

- **Tempest** — `tests/tempest/keystone-<slug>/` with
  `00-keystone-cr.yaml` (CR name `keystone-tempest-<slug>`, its own
  database, `tag: "<version>"`), `exclude-tests.txt`,
  `include-tests.txt`, `tempest.conf`. Without this directory
  `hack/ci-generate-tempest-matrix.sh` fails **every** CI run with
  `::error::Missing Tempest config directory`.
- **Per-release e2e variant** — clone
  `tests/e2e/keystone/basic-deployment-<prev-slug>/` to
  `basic-deployment-<slug>/` and rename *everything*: the CR name and
  database, every `keystone-basic-<slug>-*` resource assertion in
  `chainsaw-test.yaml`, and the `ghcr.io/c5c3/keystone:<version>` ref in
  the poke command. A missed rename silently tests the wrong release.

### 4. Walk the decision points

None of these are mechanical; decide and record each:

- **Move the default release?** The plain `basic-deployment` suite, the
  kind quick-start ControlPlane (`deploy/kind/controlplane/controlplane.yaml`),
  the `hack/deploy-infra.sh` preload, the three `RELEASE:-` fallbacks,
  and the `ci.yaml` image-tag pins all name the default. Moving it is
  one coordinated sweep — the inventory prints every current value.
- **Extend the upgrade path?** `release-upgrade/` and `upgrade-flow/`
  must test the newest sequential transition (second-newest → newest);
  the skip-level fixture must keep targeting a version that does *not*
  exist. Add transition cases to
  `internal/common/release/release_test.go` when worthwhile.
- **Retire the oldest release?** Deleting `releases/<old>/` auto-shrinks
  the matrices, but leaves orphans: the Tempest directory, the e2e
  variant, default references, and the `tests/unit/renovate/` probes
  that pin a concrete `releases/<version>/` file. Run
  [[check-release-wiring]] after the removal — its L2–L6 checks exist
  precisely for this.

### 5. Verify

```bash
bash .claude/skills/check-release-wiring/scripts/audit-release-wiring.sh --full
```

plus `make test-shell` and a full `make e2e` against a kind cluster when
the change moves defaults. [[check-release-wiring]] is the repeatable
audit for everything this skill sets up; run it as the gate on the PR.

## Known gotchas (verified 2026-07, re-verify at HEAD)

- **The Tempest matrix is keystone-hardcoded** —
  `hack/ci-generate-tempest-matrix.sh` requires `keystone-<slug>` only;
  services without a maintained tempest plugin (e.g. horizon) are
  covered by chainsaw HTTP assertions instead. When the generator
  generalizes, this skill and [[check-release-wiring]] L2 must follow.
- **`verify_release_config.sh` Test 7** rejects any
  `test-excludes/*.txt` whose basename is not a `source-refs.yaml` key —
  add the service key first, the excludes file second.
- **YYYY.N sorts lexicographically** only because the year is
  four-digit and N single-digit — scripts rely on plain `sort`.
- **A `YYYY.3` cadence change is rejected in three layers** —
  `release.ParseRelease`, the CRD `Pattern` marker, and
  `controlPlaneReleaseRegexp` must change together (plus CRD regen).
- **The renovate regression tests probe one concrete release**
  (`tests/unit/renovate/*_test.sh`) — when that release is retired,
  repoint the probe before deleting the directory or `make test-shell`
  breaks.

## Notes

- This skill is read-only with respect to the codebase; its output is
  the ordered change list (and, when asked, a meta issue in the style
  of [[prepare-new-service]]). Implementation follows
  `docs/contributing/adding-a-new-release.md`.
- Renovate needs **no** config change for a new release — but run
  [[check-renovate-coverage]] afterwards to confirm the new files'
  entries match the customManager regexes (style drift like a `v`
  prefix falls out of the match set silently).
- Pair with [[check-doc-drift]] when the default release moves — docs
  examples reference the default tag in many places.
