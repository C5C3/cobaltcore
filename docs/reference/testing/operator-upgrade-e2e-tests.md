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
3. Apply the locally built CRDs and check the deployed Keystone while the
   released operator still runs it (the [CRD window](#the-crd-window)).
4. `helm upgrade` the release to the **locally built** chart and the `:dev`
   image.
5. Assert the deployed Keystone survives the operator upgrade.

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
| 1 | Deploy CR on released operator | Applies the CR and asserts `Ready=True` (AllReady) and `status.installedRelease == "2026.1"` under the released operator |
| 2 | Capture baseline | Reads the `keystone-op-upgrade-bootstrap` Job UID and the API Service backend count and stashes both in a ConfigMap (the bootstrap Job persists — `TTLSecondsAfterFinished` is unset — so its UID is a stable "no re-bootstrap" anchor) |
| 3 | Assert CRD window | Runs `hack/ci-operator-upgrade-crd-window.sh` with `OPERATOR=keystone`, `CRS=keystones.keystone.openstack.c5c3.io/keystone-op-upgrade` and `PROBE_PATCH={"spec":{"fernet":{"suspend":true}}}`, which applies the locally built CRDs and checks the CR under the released operator. Then asserts that CronJob `keystone-op-upgrade-fernet-rotate` has `spec.suspend: true`, so the released controller acted on the probed spec. See [The CRD window](#the-crd-window) |
| 4 | helm upgrade | `helm dependency build`s the in-repo chart, `helm upgrade`s the release to the `:dev` image, and waits for the rollout. Helm does not upgrade a chart's `crds/` directory; step 3 applied the CRDs |
| 5 | Assert operator rolled | Verifies the `manager` container runs the `:dev` image and `updatedReplicas == replicas` — proving the rollout actually happened, not just that the spec was patched |
| 6 | Assert endpoints survive upgrade | Samples the API Service EndpointSlices until the new operator has narrowed the Service selector to `app.kubernetes.io/component=api`, failing if the non-terminating backend count ever drops below the step-2 baseline, or if the narrowing never lands |
| 7 | Poke reconcile | Patches `spec.deployment.replicas` from 1 to 2 so that `metadata.generation` advances and only a reconcile by the new operator can bring `status.observedGeneration` level with it. Retries the patch for up to 90 seconds, because it is the first webhook-gated request after the rollout and the API server may still hold a connection to a terminated operator pod |
| 8 | Assert after upgrade | Asserts `Ready=True` (AllReady), `status.observedGeneration == metadata.generation`, `status.installedRelease` still `2026.1`, the bootstrap Job UID is unchanged, and exactly one bootstrap Job exists |

`status.installedRelease` tracks the Keystone service image tag (`2026.1`), not
the operator version, so "unchanged" means it stays `2026.1` across the operator
upgrade. Bootstrap is gated on the admin-password digest (not the image), so the
operator upgrade must not re-run it — asserted via the unchanged bootstrap Job
UID and the exactly-one-bootstrap-Job count.

Step 6 covers a failure mode the CR-level assertions cannot see: `Ready=True`
and `observedGeneration` are both derived from Deployment status, so they stay
green even if the API Service is left with no backends at all. The new operator
narrows the Service selector to `app.kubernetes.io/component=api`, a label the
pods running under the released operator do not carry, so the narrowing must
wait for the Deployment to roll onto the labelled template — see the
[Keystone Reconciler reference](../keystone/keystone-reconciler.md).

## The CRD window

An operator upgrade applies the new CRDs before it rolls the operator. Flux's
helm-controller follows the same order with `crds: CreateReplace` (see
[CRD Installation Behavior](../backend/keystone-operator-packaging.md#crd-installation-behavior)).
Until the rollout finishes, the released controller and its webhooks serve the
existing CRs under a schema they were not built for, and users can still edit
those CRs. Steps 5 to 8 run after the new operator took over, so they cannot
see this stretch. Step 3 checks the CR inside it, before step 4 runs
`helm upgrade`.

`hack/ci-operator-upgrade-crd-window.sh` first reads the `manager` image of
`deployment/<op>-operator` and fails if the image already ends in `:dev`. The
script then snapshots every CR in `CRS` and applies the CRDs of
`operators/<op>/helm/<op>-operator/crds/` server-side, with the same command
`hack/ci-deploy-operator.sh` uses for the baseline install. Once the API server
reports the CRDs `Established`, the script checks each CR:

- (a) The CR reads back with every spec value the released CRDs stored,
  `false`, `null` and empty lists or maps included. A new default passes. A value that the new schema pruned or rewrote fails, with one
  `FAIL:` line per spec path.
- (b) An unchanged update of the CR, a `kubectl replace` of the object as read,
  is admitted and keeps `metadata.generation`. A new generation means that the
  running mutating webhook rewrote the spec. When the update conflicts with a
  concurrent status write, the script reads the CR again and makes up to five
  attempts.
- (c) A server-side dry-run create of the stored spec, under the name
  `<name>-w`, is admitted.

None of these checks makes the released controller act. An unchanged update
triggers no reconcile, because the Keystone controller's watch admits only
generation, label, annotation and deletion changes. `Ready=True` on its own is
the status that the released controller wrote before the CRDs changed. The
script therefore patches the first CR with `PROBE_PATCH` and waits until
`status.observedGeneration` and the `READY_CONDITION` condition both carry the
new generation, with the condition `True`. The released controller writes that
status from its own Go types, and a new status schema can reject the write. The
keystone suite probes with `spec.fernet.suspend: true`, which suspends the
fernet rotation CronJob and restarts no pod.

After the probe, the script reads the operator Deployment again. Its `manager`
image and its generation must match the first read, which proves that the
operator did not roll during the window.

### Why the window runs four checks

Kubernetes 1.33 and later ratchet CRD validation: an update may keep a stored
value that the new schema rejects, as long as the update leaves that value
alone. An envtest kube-apiserver v1.37.0 gave these answers on 2026-10-23, with
the keystone CRD and this suite's fixture:

| New rule over the stored CR | Unchanged update | Dry-run create of the stored spec |
| --- | --- | --- |
| New `required` field in `spec.fernet` | admitted | rejected |
| `enum` that excludes the stored `rotationSchedule` | admitted | rejected |
| `maximum` below the stored `maxActiveKeys` | admitted | rejected |
| `maxActiveKeys` type changed from integer to string | admitted | rejected |
| CEL rule without `oldSelf` that the CR violates | admitted with a `Warning:` | rejected |
| CEL rule with `oldSelf` that fails when nothing changed | rejected | admitted |
| `maxActiveKeys` removed from the schema | admitted; the write pruned the value | admitted |

Only the CEL rule with `oldSelf` stops an unchanged update, and only check (a)
sees the removed property. A create skips ratcheting, so check (c) catches the
tightened rules. The probe adds the status write that none of the three checks
makes. A rule that ratcheting lets through still blocks later edits: a patch
to the sibling field `spec.fernet.suspend` was rejected under the `required`
row and both CEL rows, because it changed their parent object.

### Script variables and exit codes

The script takes the operator, its CRs and the probe as input, so the upgrade
suites of the other operators can call it with their own CRs and probe patch.

| Variable | Default | Meaning |
| --- | --- | --- |
| `OPERATOR` | required | Operator name matching `^[a-z0-9]+$`, such as `keystone`. The CRDs come from `operators/<op>/helm/<op>-operator/crds/`, and the controller is `deployment/<op>-operator` |
| `CRS` | required | Space-separated `<resource>/<name>` entries in `CR_NAMESPACE`. The first entry gets the probe |
| `PROBE_PATCH` | required | A JSON merge patch (an object) that advances `metadata.generation` of the first CR |
| `OPERATOR_NAMESPACE` | `<op>-system` | Namespace of the operator Deployment |
| `CR_NAMESPACE` | `openstack` | Namespace of the CRs |
| `READY_CONDITION` | `Ready` | Condition the probe waits for |
| `PROBE_TIMEOUT` | `120` | Seconds the probe waits, a positive integer |
| `DRY_RUN_CREATE` | `true` | `true` or `false`. `false` skips check (c) for every CR |
| `ALLOW_SPEC_CHANGES` | empty | Space-separated spec paths, written as check (a) prints them (`spec.fernet.maxActiveKeys`, `spec.a[0].b`), whose value may change under the new CRDs |
| `POLL_INTERVAL` | `2` | Seconds between update retries and probe reads, a non-negative integer |

The script prints `OK:`, `FAIL:`, `SKIP:` and `ALLOWED:` lines and one `crd/`
line per CRD on stdout. It also repeats every API server `Warning:` that check
(b) receives.

| Exit code | Meaning |
| --- | --- |
| 0 | Every check passed |
| 1 | A window check failed. The `FAIL:` line names it |
| 2 | Invalid input, a precondition the script cannot read, or a `PROBE_PATCH` that does not advance `metadata.generation`. An `::error::` line on stderr names the cause |

Each `crd/` line compares the generation of one CRD before and after the apply
and says `schema changed`, `unchanged` or `new in this chart`. For a CRD that
the change leaves equal to the released one, the window runs the released
schema. A rerun on a cluster whose CRDs already are the in-repo ones prints
`unchanged` on every `crd/` line, so the window then checks nothing new.

### Breaking an existing CR on purpose

The window fails when the new schema no longer admits the stored CR, even where
ratcheting would still let an unchanged update through. A pull request that
removes a field or tightens a rule on purpose declares that in the suite:

- For a removed or rewritten value, list its path in `ALLOW_SPEC_CHANGES`. The
  script then prints an `ALLOWED:` line for that path. An entry whose path kept
  its value on every CR fails the run with `remove the entry`. That happens
  once the baseline carries the change, and the pull request that meets the
  failure deletes the entry.
- For a tightened rule, change the fixture so that it satisfies both the
  released and the new schema. Step 1 still applies the fixture under the
  released CRDs.
- Set `DRY_RUN_CREATE=false` only where a create-time webhook rule makes a
  second CR impossible. The c5c3 ControlPlane webhook admits one ControlPlane
  per namespace, so check (c) cannot create a copy there.

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
`2026.1` service images, `helm registry login`s GHCR, fetches the released
baseline, deploys it, and runs the suite. See
[CI Workflow — e2e-operator-upgrade](../ci-cd/ci-workflow.md#e2e-operator-upgrade)
for the full job documentation.

## Related Resources

- [Keystone E2E Test Suites](./keystone-e2e-tests.md)
- [Chaos E2E Test Suites](./chaos-e2e-tests.md)
- [CI Workflow](../ci-cd/ci-workflow.md)
