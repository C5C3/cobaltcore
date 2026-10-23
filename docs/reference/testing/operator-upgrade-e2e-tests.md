---
title: Operator Upgrade E2E Tests
quadrant: operator
---

# Operator Upgrade E2E Tests

Reference documentation for the operator helm-upgrade-in-place E2E suites, one per
operator. The other "upgrade" suites upgrade an OpenStack *service* image. These
upgrade the **operator and its CRDs** in place and assert that a CR deployed under
the previous operator keeps running. Every deployment takes this step with each
operator release.

For happy-path per-CR E2E tests, see [Keystone E2E Test Suites](./keystone-e2e-tests.md);
for fault-injection tests, see [Chaos E2E Test Suites](./chaos-e2e-tests.md).

## Overview

The suites live under `tests/e2e-operator-upgrade/<op>-helm-upgrade/`, outside
`tests/e2e/`, because each manages its operator's Helm release itself. `make e2e`
and the per-CR `e2e-operator` CI job both assume a single, already-deployed
operator, so they must not pick these suites up. All ten share
`tests/e2e-operator-upgrade/chainsaw-config.yaml`: a 300-second assert timeout and
`parallel: 1`, since a suite rewrites the cluster-wide operator Deployment and CRDs.

The flow is:

1. The job installs the operator chart and image that the suite's `baseline.yaml`
   pins.
2. The suite brings its CR to Ready under that released operator.
3. The keystone suite applies the in-repo CRDs and checks its Keystone while the
   released operator still runs it (the [CRD window](#the-crd-window)). The other
   nine suites go straight to step 4.
4. It `helm upgrade`s the release to the in-repo chart, CRDs and `:dev` image.
5. It asserts what the upgrade has to leave alone.

## Baseline

### The chart-version image tag

The `merge-operator-images` job tags every operator image it publishes from `main`
with the version the operator's `Chart.yaml` names, beside `sha-<sha>` and `latest`
(see [CI Workflow](../ci-cd/ci-workflow.md#merge-operator-images)). On `main`,
`<op>-operator:<chart version>` and `latest` therefore name one digest.
`hack/ghcr-prune-stale-versions.py` keeps a version with a bare `X.Y.Z` tag forever,
and charts are never pruned. Once `Chart.yaml` moves past a version, no push writes
that tag again, so the chart and the image of a past version stay fixed.

### The pin

Each suite directory holds a `baseline.yaml` with one key:

```yaml
version: "0.14.1"
```

The pin names two artifacts: the chart `oci://ghcr.io/c5c3/charts/<op>-operator` at
that version, and the image `ghcr.io/c5c3/<op>-operator:<version>`. The pin sits in
the suite directory, so moving one operator's baseline schedules only that
operator's CI job.

`hack/ci-fetch-released-operator.sh` reads it. With `OPERATOR` set (and
`BASELINE_FILE` to read another file), the script:

- checks the operator name and the pin before it pulls anything: a missing file, an
  empty version and a version that is not `X.Y.Z` each fail with an `::error::` line;
- pulls the chart at the pinned version into `_output/operator-upgrade/<op>-operator`,
  after clearing that directory, and refuses a pulled chart of another version;
- pulls the image and loads it into kind; a missing tag fails with an error that
  names it;
- writes `chart_dir` and `image_tag` to `GITHUB_OUTPUT`.

It also says whether the baseline is fixed. A pin equal to the checkout's
`Chart.yaml` version is still the version `main` publishes, so its chart and image
move with every push until `Chart.yaml` moves past it:

```text
::notice::baseline keystone-operator 0.15.0 is the version main still publishes; its chart and image move with every push until Chart.yaml moves past 0.15.0
```

Any lower pin is fixed:

```text
Baseline keystone-operator 0.14.1 is frozen (main publishes 0.15.0)
```

`tests/unit/ci/operator_upgrade_wiring_test.sh` fails when a chart has no suite, or
when a pin is not `X.Y.Z` or lies above its chart's version, compared field by field.

### Bump rule

The pull request that moves an operator's `Chart.yaml` version from V to V' sets that
operator's pin to V. The baseline is then always the version deployments run before
they upgrade. Renovate does not track the pins: its docker datasource would propose
the newest version tag, which is the one `main` still moves, and it cannot tell that
tag from a fixed one.

### The `v*` tag hazard

A `v*` tag push re-tags every chart and every operator image with the tag's version:
`helm-push` packages each chart under it, and `type=semver` tags each image with it.
A `v*` tag whose version equals a pin therefore overwrites both artifacts that pin
names. Release versioning does not avoid this today, so pick a tag version that no
pin uses.

### Bootstrap

The chart-version tags exist only for pushes made since the tagging step was added,
so the pull request that introduced the suites needed the ten tags created once by
hand. Before it merged, `main` had moved every chart past the version the suites
first pinned (e60973db4 released operator-library 0.12.0 to every chart), and the
nightly `cleanup-images` sweep had deleted every image of those versions, because
it removes a `sha-<sha40>` tag a day after its push. The ten pins therefore name
the versions of 8d01ec302, the last push before e60973db4, whose `sha-<sha40>` tags
still existed. For each operator, the person with a `write:packages` login:

1. picks the newest `sha-<sha40>` tag of `ghcr.io/c5c3/<op>-operator` whose
   commit's `operators/<op>/helm/<op>-operator/Chart.yaml` names the pinned version
   (`git show <sha40>:operators/<op>/helm/<op>-operator/Chart.yaml`);
2. runs `docker buildx imagetools create -t ghcr.io/c5c3/<op>-operator:<version> ghcr.io/c5c3/<op>-operator:sha-<sha40>`
   before the next sweep deletes the source tag.

A single-source `imagetools create` copies the index, so the new tag names the same
digest, and running the command again writes the same digest under the same tag. The
bare `X.Y.Z` tag is a keeper, so the sweep keeps the image from then on. Without a
`sha-<sha40>` tag of the pinned version, nothing is tagged by guess: pin a version
that still has one. The merge push of that pull request tags only the versions its
`Chart.yaml` files name, which are newer than the pins.

## Suite contract

Every suite except keystone's runs these six steps, each with a `catch` block that
dumps the CR, the operator logs, `helm history` and the events:

| # | Step | Details |
| --- | --- | --- |
| 1 | `deploy-cr-on-released-operator` | Applies the copied fixtures and asserts the Ready check of the e2e suite they come from, plus `status.installedRelease: "2026.1"` where the CR reports one |
| 2 | `capture-baseline` | Reads the UID of each workload the table below lists and stashes them in the ConfigMap `<op>-op-upgrade-baseline`, applied idempotently |
| 3 | `helm-upgrade-operator` | From the repository root, runs `HELM_UPGRADE=true hack/ci-deploy-operator.sh` for the in-repo chart and the `:dev` image, then waits for the rollout. The script applies the new CRDs first and re-passes the values the baseline install passed, the barbican TokenRequest grant included |
| 4 | `assert-operator-rolled` | Checks that the `manager` container runs the `:dev` image and that `updatedReplicas == replicas` |
| 5 | `poke-reconcile` | Patches the spec field the table names, retrying while the webhook comes back, and fails unless `metadata.generation` advanced |
| 6 | `assert-after-upgrade` | Waits up to 150 seconds for `status.observedGeneration == metadata.generation`, compares every stashed UID with the live one, checks that exactly one `<cr>-db-sync` Job exists and completes, asserts that the Deployment the patch targets runs the patched replica count, then asserts the Ready check and `installedRelease` of step 1 again. A `finally` block deletes the ConfigMap |

The invariants are:

- `status.installedRelease` is unchanged;
- the CR's workloads keep their UIDs, so the upgrade changed them in place and never
  deleted and re-created them;
- exactly one `<cr>-db-sync` Job exists, and it completes;
- Ready holds after the new operator processed a new generation, and the patched
  Deployment runs the replica count of that generation.

A re-created workload fails step 6 with a line such as
`FAIL: Deployment/placement-basic was re-created across the operator upgrade (uid <old> -> <new>)`.
The UID of the db-sync Job is not an invariant. `RunJob` re-creates a completed Job
whenever its pod template changes, an operator upgrade included
(`internal/common/job/job.go`), so step 6 counts the Jobs and waits for completion
instead.

Step 6 runs its script before the Ready assert. Ready was already True at the
generation before the patch, so only once `observedGeneration` caught up does the
Ready check describe the new operator's work. `observedGeneration` alone proves
little: the operators set it on every status write, a pass that stopped before
its Deployment step included. The replica count of the patched Deployment is what
shows that the new operator applied the new generation.

The fixtures are byte-for-byte copies of e2e fixtures, each with a header naming its
source. A copy stays valid under the baseline's older CRD; a fixture referenced by
path would break the suite as soon as the e2e suite adopted a new field.

## Per-operator suites

| Operator | Fixtures copied from | Ready check | `installedRelease` | Workload UIDs kept | db-sync Job | Generation patch |
| --- | --- | --- | --- | --- | --- | --- |
| c5c3 | `tests/e2e/c5c3/dedicated-backing-services/` | MariaDB `cp-keystone-db` Ready within 600 s, then `InfrastructureReady=True` within 180 s | none | MariaDB `cp-keystone-db`, Memcached `cp-keystone-cache`, `cp-horizon-cache` | none | `spec.sizing.keystone.api.replicas` 1 to 2 |
| horizon | `tests/e2e/horizon/basic-deployment/` | every sub-condition and Ready | none | Deployment `horizon-basic` | none | `spec.deployment.replicas` 3 to 2 |
| glance | `tests/e2e/glance/basic-deployment/` | every sub-condition and Ready, and the GlanceBackend | `2026.1` | Deployment `glance-basic` | `glance-basic-db-sync` | `spec.deployment.replicas` 3 to 2 |
| placement | `tests/e2e/placement/basic-deployment/` | every sub-condition and Ready | `2026.1` | Deployment `placement-basic` | `placement-basic-db-sync` | `spec.deployment.replicas` 3 to 2 |
| barbican | `tests/e2e/barbican/basic-deployment/` | every sub-condition, Ready and `projectedSecretStores`, and the BarbicanSecretStore | `2026.1` | Deployment `barbican-basic` | `barbican-basic-db-sync` | `spec.deployment.replicas` 3 to 2 |
| ovn | `tests/e2e/ovn/central-deletion-cleanup/` | Ready | none | StatefulSets `ovn-deletion-nb`, `ovn-deletion-sb`, Deployment `ovn-deletion-northd` | none | `spec.northd.deployment.replicas` 1 to 2 |
| neutron | `tests/e2e/neutron/basic-deployment/` | every sub-condition and Ready | `2026.1` | Deployments `neutron-basic`, `neutron-basic-periodic-workers`, `neutron-basic-ovn-maintenance-worker` | `neutron-basic-db-sync` | `spec.deployment.replicas` 2 to 1 |
| cinder | `tests/e2e/cinder/basic-deployment/` | every sub-condition, Ready and `volumeServices` | `2026.1` | Deployments `cinder-basic`, `cinder-basic-scheduler`, `cinder-basic-volume-basic-nfs1` | `cinder-basic-db-sync` | `spec.api.deployment.replicas` 2 to 1 |
| nova | `tests/e2e/nova/deletion-cleanup/` | Ready and the cleanup finalizer, with a 10-minute assert timeout | `2026.1` | Deployments `nova-cleanup`, `nova-cleanup-conductor`, `nova-cleanup-scheduler` | `nova-cleanup-db-sync` | `spec.api.deployment.replicas` 1 to 2 |

Notes on single suites:

- c5c3 proves the InfrastructureReady path. No ControlPlane reaches an aggregate
  `Ready=True` without the sibling controllers its job does not run. One ControlPlane
  is permitted per namespace, so the suite runs in chainsaw's ephemeral namespace
  and applies the fixture with `kubectl apply -n`. The ControlPlane webhook
  re-validates only the sizing bounds of the patched field. The patched field
  reaches only the projected Keystone child, which the suite does not wait for, so
  step 6 checks that the `InfrastructureReady` condition carries the new generation
  in place of a workload's replica count.
- The ovn fixture leaves `spec.image` unset, as a deployment does. When the
  baseline's default OVN image differs from the new one, the upgrade rolls the
  image. The database replicas are never patched: a CEL rule makes them immutable.
- cinder and nova create their vhost on the kind broker with
  `tests/e2e/cinder/broker-vhost.sh` before the CR, and delete it in the step
  cleanup.
- nova uses the light tier: a Nova reaches Ready on Secrets and the broker alone,
  about eight minutes after the apply, so no sibling operator runs.
- Patching to fewer replicas keeps the four three-replica fixtures inside one kind
  node.

## Suite: keystone-helm-upgrade

**File:** `tests/e2e-operator-upgrade/keystone-helm-upgrade/chainsaw-test.yaml`

The keystone suite predates the contract above and keeps its own eight steps:

| # | Action | Details |
| --- | --- | --- |
| 1 | Deploy CR on released operator | Applies the CR and asserts `Ready=True` (AllReady) and `status.installedRelease == "2026.1"` under the released operator |
| 2 | Capture baseline | Reads the `keystone-op-upgrade-bootstrap` Job UID and the API Service backend count and stashes both in a ConfigMap (the bootstrap Job persists — `TTLSecondsAfterFinished` is unset — so its UID is a stable "no re-bootstrap" anchor) |
| 3 | Assert CRD window | Runs `hack/ci-operator-upgrade-crd-window.sh` with `OPERATOR=keystone`, `CRS=keystones.keystone.openstack.c5c3.io/keystone-op-upgrade` and `PROBE_PATCH={"spec":{"fernet":{"suspend":true}}}`, which applies the locally built CRDs and checks the CR under the released operator. Then asserts that CronJob `keystone-op-upgrade-fernet-rotate` has `spec.suspend: true`, so the released controller acted on the probed spec. See [The CRD window](#the-crd-window) |
| 4 | helm upgrade | `helm dependency build`s the in-repo chart, `helm upgrade`s the release to the `:dev` image, and waits for the rollout. Helm does not upgrade a chart's `crds/` directory; step 3 applied the CRDs |
| 5 | Assert operator rolled | Verifies the `manager` container runs the `:dev` image and `updatedReplicas == replicas`, which proves the rollout happened |
| 6 | Assert endpoints keep their backends | Samples the API Service EndpointSlices until the new operator has narrowed the Service selector to `app.kubernetes.io/component=api`, failing if the non-terminating backend count ever drops below the step-2 baseline, or if the narrowing never happens |
| 7 | Poke reconcile | Patches `spec.deployment.replicas` from 1 to 2 so that `metadata.generation` advances and only a reconcile by the new operator can bring `status.observedGeneration` level with it. Retries the patch for up to 90 seconds, because it is the first webhook-gated request after the rollout and the API server may still hold a connection to a terminated operator pod |
| 8 | Assert after upgrade | Asserts `Ready=True` (AllReady), `status.observedGeneration == metadata.generation`, `status.installedRelease` still `2026.1`, the bootstrap Job UID is unchanged, and exactly one bootstrap Job exists |

`status.installedRelease` tracks the Keystone service image tag (`2026.1`), not
the operator version, so "unchanged" means it stays `2026.1` across the operator
upgrade. Bootstrap is gated on the admin-password digest (not the image), so the
operator upgrade must not re-run it, which the unchanged bootstrap Job UID and the
exactly-one-bootstrap-Job count assert.

Step 6 covers a failure mode the CR-level assertions cannot see: `Ready=True`
and `observedGeneration` are both derived from Deployment status, so they stay
green even if the API Service is left with no backends at all. The new operator
narrows the Service selector to `app.kubernetes.io/component=api`, a label the
pods running under the released operator do not carry, so the narrowing must
wait for the Deployment to roll onto the labelled template; see the
[Keystone Reconciler reference](../keystone/keystone-reconciler.md).

## The CRD window

An operator upgrade applies the new CRDs before it rolls the operator. Flux's
helm-controller follows the same order with `crds: CreateReplace` (see
[CRD Installation Behavior](../backend/keystone-operator-packaging.md#crd-installation-behavior)).
Until the rollout finishes, the released controller and its webhooks serve the
existing CRs under a schema they were not built for, and users can still edit
those CRs. Steps 5 to 8 of the keystone suite run after the new operator took
over, so they cannot see this stretch. Its step 3 checks the CR inside it,
before step 4 runs `helm upgrade`. The other nine suites do not run the window
yet.

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

## What each job loads

`hack/ci-operator-upgrade-leg.sh` is the one table of what an operator's upgrade job
loads and deploys. The reusable workflow and the main-side image map both read it.
Every ref carries the prefix `ghcr.io/c5c3/`, and the service images are the
`2026.1` ones every fixture pins:

| Operator | Images | Sibling operators | Infrastructure |
| --- | --- | --- | --- |
| keystone | `keystone-operator:dev`, `keystone:2026.1` | | |
| c5c3 | `c5c3-operator:dev` | | the keystone, horizon, glance and placement CRDs and K-ORC |
| horizon | `horizon-operator:dev`, `horizon:2026.1` | | |
| glance | `glance-operator:dev`, `glance:2026.1` | | |
| placement | `placement-operator:dev`, `placement:2026.1` | | |
| barbican | `barbican-operator:dev`, `barbican:2026.1` | | |
| ovn | `ovn-operator:dev`, `ovn:<OVN version>` | | `WITH_OVN_KERNEL_MODULES` |
| neutron | `neutron-operator:dev`, `neutron:2026.1`, `ovn-operator:dev`, `ovn:<OVN version>` | ovn | `WITH_OVN_KERNEL_MODULES` |
| cinder | `cinder-operator:dev`, `cinder:2026.1` | | `WITH_NFS`, `WITH_MESSAGING` |
| nova | `nova-operator:dev`, `nova:2026.1` | | `WITH_MESSAGING` |

The OVN version comes from `hack/ci-resolve-ovn-version.sh`. An operator without a
row fails the script, and `tests/unit/hack/ci_operator_upgrade_leg_test.sh` fails
when a chart under `operators/*/helm/` has none.

## Running locally

```bash
# Against a kind cluster with the infra stack deployed but NO <op>-operator
# release yet: the target installs the baseline itself. Requires
# `helm registry login ghcr.io` for the baseline chart and image pull.
make e2e-operator-upgrade OPERATOR=placement
```

`OPERATOR` defaults to `keystone`. The target checks that kubectl reaches a cluster
and that no `<op>-operator` release exists in `<op>-system`, then fetches the pinned
baseline, installs it with the barbican grant CI passes, and runs the operator's
suite. Preparing the cluster as the operator's row above requires is the caller's
job:

- deploy the infrastructure with the row's opt-ins, for example
  `WITH_MESSAGING=true make deploy-infra` for nova;
- build the operator's image as `ghcr.io/c5c3/<op>-operator:dev`
  (`make image OPERATOR=<op> IMG=ghcr.io/c5c3/<op>-operator:dev`) and
  `kind load docker-image` it;
- deploy each sibling operator at `:dev` first (neutron needs the ovn-operator);
- for c5c3, apply the sibling CRDs and run `hack/ci-deploy-korc.sh`.

## Running in CI

Two workflows run the suites, both through the reusable workflow
`.github/workflows/e2e-operator-upgrade.yaml`, which runs one job per operator:

- On a pull request, the `e2e-operator-upgrade` job of `ci.yaml` calls it once per
  operator whose Go code changed (`go_common` included), whose suite changed
  (`tests_operator_upgrade_<op>`), or for all ten when an input the suites share
  changed (`tests_operator_upgrade`). The images come from the `build-e2e-images`
  image map.
- After each successful CI or Build Images push run on `main`,
  `.github/workflows/operator-upgrade.yaml` calls it once per operator whose image
  that commit published. It skips a triggering run that was not a push, did not
  succeed, or came from another repository, and only a run it does not skip joins
  the concurrency group that cancels the run in flight.
  `hack/ci-operator-upgrade-main-matrix.sh` selects the operators and maps their
  `:dev` ref to the published digest. It leaves out an operator whose pin equals the
  commit's `Chart.yaml` version: that commit's publish moved the pinned tag and chart
  onto the commit itself, so the suite would upgrade the commit onto itself. The
  operator joins the matrix once a bump freezes its pin.

See [CI Workflow: e2e-operator-upgrade](../ci-cd/ci-workflow.md#e2e-operator-upgrade)
for the steps of the job and the filters.

## Moving a baseline

When a pull request moves an operator's `Chart.yaml` version from V to V':

1. Set `tests/e2e-operator-upgrade/<op>-helm-upgrade/baseline.yaml` to V. The change
   schedules that operator's upgrade job.
2. Check that the job logs `Baseline <op>-operator V is frozen (main publishes V')`.
3. When the new chart drops a field the copied fixtures still set, or the old CRD
   rejects a field they gained, edit the copies in the suite directory and leave the
   e2e fixtures they came from as they are.

V's image tag exists once `main` published V, which every push did while
`Chart.yaml` named it. The fetch script fails on a pin that `main` never published.

## Related Resources

- [Keystone E2E Test Suites](./keystone-e2e-tests.md)
- [Chaos E2E Test Suites](./chaos-e2e-tests.md)
- [CI Workflow](../ci-cd/ci-workflow.md)
