---
title: Operator Upgrade E2E Tests
quadrant: operator
---

# Operator Upgrade E2E Tests

Reference documentation for the operator helm-upgrade-in-place E2E suite. Every
other "upgrade" suite upgrades the Keystone *service* image; this suite upgrades
the **operator and CRDs** in place and asserts that an already-deployed Keystone
survives the operator upgrade. Given the cross-release bootstrap history, an
operator upgrade-in-place is the highest-risk otherwise-untested path.

For happy-path per-CR E2E tests, see [Keystone E2E Test Suites](./keystone-e2e-tests.md);
for fault-injection tests, see [Chaos E2E Test Suites](./chaos-e2e-tests.md).

## Overview

The suite lives under `tests/e2e-operator-upgrade/`, deliberately **outside**
`tests/e2e/`, because it manages the operator Helm release itself. `make e2e`
and the per-CR `e2e-operator` CI job both assume a single, already-deployed
operator; this suite installs the baseline operator, then upgrades it, so it
must not be swept up by those flows.

The flow is:

1. Install the **last released** keystone-operator chart + image from GHCR as
   the baseline.
2. Bring a managed-mode Keystone CR (`keystone-op-upgrade`) to `Ready=True`.
3. `helm upgrade` the release to the **locally built** chart and apply the
   locally built CRDs.
4. Assert the deployed Keystone survives the operator upgrade.

## Baseline: what "last released" means

The repo has no `v*` git tags yet, so "last released" is the last artifact
published to GHCR from `main`:

- **Chart:** the highest semver tag under
  `oci://ghcr.io/c5c3/charts/keystone-operator` (pushed by the `helm-push` job on
  every `main` push). `helm pull` without `--version` resolves that tag. The
  packaged chart already vendors its `operator-library` dependency, so the
  baseline install never needs to resolve the in-repo `file://` dependency path.
- **Image:** `ghcr.io/c5c3/keystone-operator:latest` (pushed by
  `merge-operator-images` on every `main` push). The chart's default image tag
  (`appVersion`) points at a not-yet-published image, so the baseline install
  pins `image.tag=latest` explicitly.

`hack/ci-fetch-released-operator.sh` performs the pull, guards every step with an
actionable `::error::` message, and loads the released image into the kind
cluster. `hack/ci-deploy-operator.sh` installs the pulled chart via its optional
`CHART_DIR` input.

## Suite: keystone-helm-upgrade

**File:** `tests/e2e-operator-upgrade/keystone-helm-upgrade/chainsaw-test.yaml`

| # | Action | Details |
| --- | --- | --- |
| 1 | Deploy CR on released operator | Applies the CR and asserts `Ready=True` (AllReady) and `status.installedRelease == "2025.2"` under the released operator |
| 2 | Capture baseline | Reads the `keystone-op-upgrade-bootstrap` Job UID and stashes it in a ConfigMap (the bootstrap Job persists — `TTLSecondsAfterFinished` is unset — so its UID is a stable "no re-bootstrap" anchor) |
| 3 | helm upgrade | Applies the locally built CRDs, `helm dependency build`s the in-repo chart, `helm upgrade`s the release to the `:dev` image, and waits for the rollout |
| 4 | Assert operator rolled | Verifies the `manager` container runs the `:dev` image and `updatedReplicas == replicas` — proving the rollout actually happened, not just that the spec was patched |
| 5 | Poke reconcile | Annotates the CR to force one full reconcile by the new operator (`AnnotationChangedPredicate` admits it; generation is unchanged) |
| 6 | Assert after upgrade | Asserts `Ready=True` (AllReady), `status.observedGeneration == metadata.generation`, `status.installedRelease` still `2025.2`, the bootstrap Job UID is unchanged, and exactly one bootstrap Job exists |

`status.installedRelease` tracks the Keystone service image tag (`2025.2`), not
the operator version, so "unchanged" means it stays `2025.2` across the operator
upgrade. Bootstrap is gated on the admin-password digest (not the image), so the
operator upgrade must not re-run it — asserted via the unchanged bootstrap Job
UID and the exactly-one-bootstrap-Job count.

## Running locally

```bash
# Against a fresh kind cluster with the infra stack deployed but NO
# keystone-operator release yet (this target installs the baseline itself).
# Requires `helm registry login ghcr.io` for the baseline chart/image pull.
make e2e-operator-upgrade
```

The target runs two independent preflights (kubectl reachability, then a
pre-existing-operator check) before fetching the baseline, installing it,
and running the suite.

## CI wiring

The `e2e-operator-upgrade` job runs on `pull_request` when
`has-e2e-operators == 'true'` and `build-e2e-images` succeeded, in its own job
(not the `e2e-operator` matrix). It loads the run-scoped `:dev` operator and
`2025.2` service images, `helm registry login`s GHCR, fetches the released
baseline, deploys it, and runs the suite. See
[CI Workflow — e2e-operator-upgrade](../ci-cd/ci-workflow.md#e2e-operator-upgrade)
for the full job documentation.

## Related Resources

- [Keystone E2E Test Suites](./keystone-e2e-tests.md)
- [Chaos E2E Test Suites](./chaos-e2e-tests.md)
- [CI Workflow](../ci-cd/ci-workflow.md)
