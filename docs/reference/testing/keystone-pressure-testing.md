---
title: Keystone Pressure Testing
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Keystone Pressure Testing

`dizzy keystone pressure`, new in [dizzy](https://github.com/B42Labs/dizzy)
v0.6.0, puts a closed-loop load on Keystone's token path: the validation of an
existing token, the revocation lookup inside it, and the listing of a user's
effective role assignments. A fixed number of workers each send the next
request as soon as the previous answer arrives, so the throughput at that
worker count and the latency per operation are the measurement. dizzy's
[Pressure mode](https://github.com/B42Labs/dizzy/blob/v0.6.0/docs/explanation/pressure-mode.md)
explains the load and the
[CLI reference](https://github.com/B42Labs/dizzy/blob/v0.6.0/docs/reference/cli.md#pressure)
every flag.

The load exists for the downstream Keystone and keystonemiddleware patches of
#1456 and #1462. Each patch is proven the same way: run one scenario with one
seed against a build without the patch (the base) and a build with it (the
patched build), and compare the two run records with
`dizzy keystone report --compare`. CobaltCore does that in three places:

| Where | Command | Keystone | Use it for |
| --- | --- | --- | --- |
| The `keystone-pressure` CI job | runs on its own | two standalone Keystones on kind, base and patched | every pull request that changes the Keystone image |
| A local A/B run on kind | `make keystone-pressure-ab` | the same two Keystones | reproducing the CI job, or comparing two local builds |
| A run against a ControlPlane | `make dizzy-keystone-pressure`, `make dizzy-keystone-compare` | the ControlPlane's Keystone | the middleware profiles, which need Nova, Neutron, Cinder or Glance, and the metal-stack lab |

For the chaos churn of `make dizzy-keystone` and the in-cluster soak, see
[dizzy Chaos Testing](dizzy-chaos-testing.md).

## Profiles

The profiles live in `tests/load/keystone-pressure/`, one dizzy Keystone
scenario per patch, each with a `pressure:` block and seed 42. The table of
dizzy's Pressure mode maps each patch to the load that shows it, and each
profile is one row of that table.

| Profile | Patch | Load | What to compare | Needs |
| --- | --- | --- | --- | --- |
| `warm-cache` | #1461, fewer cache round trips and an in-process cache tier in Keystone | `validate` only, 32 workers; nothing flushes a cache | validate p50 and `ops/s` | Keystone |
| `effective-fan` | #1459, linear implied-role expansion and one lookup per role | `validate` and `effective` for one fan user whose grants grow from 500 to 4000 in four stages | the `effective` rows of the `Stages` table, stage by stage, and their growth per doubling of the grants | Keystone; provisions 4000 projects |
| `cold-role-cache` | #1458, one validation and one role resolution per request | `validate` under `grant_churn`, which keeps the role caches cold | validate p50 and p95 | Keystone |
| `revocation-lookup` | #1457, a narrow revocation lookup and no cache flush per logout | `validate` while `revoke` adds revocation events | validate latency per bucket against the cumulative revocations: a climb without the patch, flat with it | Keystone |
| `token-propagation` | #1463, event-driven token eviction in keystonemiddleware | `probe` with 500 pool tokens and the propagation probe every 30 s, for 15 minutes | the `Propagation` table: up to 300 s without the patch, seconds with it | a ControlPlane with Nova, Neutron, Cinder or Glance |
| `middleware-cache` | #1464, an in-process cache tier in keystonemiddleware | `probe` with 10 pool tokens at 32 workers | probe p50 per service in the `Probes` table | a ControlPlane with Nova, Neutron, Cinder or Glance |
| `bad-token-flood` | #1465, offline signature and expiry checks with the JWS provider's key | `bad_token` beside `validate` | the `Bad tokens` table per service, and a validate p50 that holds under the flood | a ControlPlane with Nova, Neutron, Cinder or Glance |

The first four run against a standalone Keystone, whose catalog lists only
the identity service; they are the profiles of the CI job and of
`make keystone-pressure-ab`. The last three send tokens to the services, and a
run without one of them in the catalog stops at priming with
`pressure probe needs at least one of nova, neutron, cinder, glance in the pool users' catalog`.

A stage of `effective-fan` makes its grants inside its own window, and a stage
whose grants outlast the window records nothing. A Keystone on kind makes
about 45 grants a second, so the 2000 grants of the last stage take about
45 s. Give the profile at least six minutes, four windows of 90 s; with one
minute, stages 3 and 4 are missing from the `Stages` table.

#1459 measured about fourfold growth per doubling of the grants on SQLite.
On kind with MariaDB, an A/A run of `main`'s 2026.2 image grew the
`effective` p50 about twofold per doubling (0.36 s, 0.65 s, 1.18 s and 2.38 s
at 500, 1000, 2000 and 4000 grants), so the database round trips per grant
outweigh the quadratic expansion at these sizes. The patch's effect therefore
shows in the base and patched latencies of each stage, which the comparison
puts side by side.

Every `revoke`, every revocation of the propagation probe and every
`bad_token` affects every user of the Keystone or service it reaches. Run the
profiles on a kind stack or the lab, never on a shared cloud.

## Reading a comparison

`dizzy keystone report --run <base> --compare <patched>` prints the p50, p95,
p99 and `ops/s` of both records per operation, then per service, per
propagation probe and per stage, each with `DELTA` (patched minus base) and
`CHANGE` (the delta as a share of the base value). A negative latency delta and
a positive `ops/s` delta mean the patched build is faster.

The command refuses two records whose scenario, seed, workers, token pool,
mix, propagation probe or stages differ, because the delta would then mix the
patch with the change of the load. The same profile and the same
`--duration` always produce comparable records.

Two runs of the same build differ too. The percentiles are estimates within
1%, and a kind node shares its CPU with everything else on the runner. Run
the base twice, or read an A/A comparison of the CI job, to see the spread
before you trust a delta of a few percent. An A/A run of `main`'s 2026.2 image
on a workstation moved the p50 and `ops/s` by about 2%, the p99 by up to
70%, and the p95 of the late `effective-fan` stages by up to 105%. Some
deltas the patches promise are larger than that: seconds instead of minutes
for #1463, a flat line instead of a climb for #1457.

dizzy measures from the client. It cannot see the SQL statements or the
Memcached round trips a validation makes, so #1461 shows as latency and
throughput, not as round trips. With the dizzy stack installed, the
`--otel` export of `make dizzy-keystone-pressure` puts dizzy's `token/validate`
rate next to the server metrics.

## The CI job

The `keystone-pressure` job of `.github/workflows/ci.yaml` runs on a pull
request that changes the Keystone image (`images/keystone/**`,
`nix/locks/*/keystone/**`, `patches/keystone/**`) or the harness
(`hack/keystone-pressure-ab.sh`, `hack/dizzy.sh`,
`tests/load/keystone-pressure/**`). The `ci:pressure` label asks for it on any
pull request, and `ci:full` includes it.

It runs one matrix job per release that ships a Keystone image. The base is
the `keystone:<release>` image `main` last published, and the patched build is
the image of the pull request. A matrix job whose release image the pull
request did not change compares `main`'s image with itself: a 2026.2-only
patch such as #1457 gets an A/A comparison for 2026.1, which shows the spread
of the runner. Each matrix job runs the four Keystone-only profiles against
both builds, two minutes per run and six for `effective-fan`. It takes about
an hour, most of it spent provisioning and tearing down the 4000 projects of
`effective-fan` on each build.

The job summary carries each profile's comparison table. The
`keystone-pressure-<release>` artifact holds every run record, the comparison
as text, HTML and JSON, and `summary.md`; open `comparison.html` of a profile
for the charts. A latency delta never fails the job. A deployment, run or
comparison that fails does, and the job does not block the pull request.
[CI Workflow](../ci-cd/ci-workflow.md#keystone-pressure) describes its steps.

## A/B run on kind

`make keystone-pressure-ab` runs what the CI job runs for one release, on the
kind stack of the current kubeconfig context. It needs the stack of
`make deploy-infra`, the keystone-operator, and both images in the local
docker:

```bash
make deploy-infra

# hack/ci-deploy-operator.sh installs the operator with pullPolicy: Never.
docker pull ghcr.io/c5c3/keystone-operator:latest
kind load docker-image ghcr.io/c5c3/keystone-operator:latest --name cobaltcore
OPERATOR=keystone IMAGE_REPO=ghcr.io/c5c3/keystone-operator IMAGE_TAG=latest \
  NAMESPACE=keystone-system hack/ci-deploy-operator.sh

# The base: main's published image, under a tag of its own.
docker pull ghcr.io/c5c3/keystone:2026.2
docker tag ghcr.io/c5c3/keystone:2026.2 keystone-base:2026.2

# The patched build: the image of the checkout, with its patches/keystone/.
nix build .#keystone-2026_2-image && ./result | docker load

KEYSTONE_PRESSURE_BASE_IMAGE=keystone-base:2026.2 \
KEYSTONE_PRESSURE_PATCHED_IMAGE=keystone:2026.2 \
KEYSTONE_PRESSURE_RELEASE=2026.2 \
  make keystone-pressure-ab
```

On a docker with the containerd image store, `kind load docker-image` fails
for a pulled multi-platform image with `content digest ... not found`. Save
the node's platform with `docker save --platform linux/amd64 -o <file> <image>`
and load the file with `kind load image-archive <file>` instead.

`hack/keystone-pressure-ab.sh` tags both images as
`ghcr.io/c5c3/keystone:<release>-pressure-base` and `-pressure-patched` and
loads them and the dizzy image of the pin in `hack/dizzy.sh` into kind. It
deploys the Keystones `keystone-pressure-base` and `keystone-pressure-patched`
in `openstack`, each with the database `keystone_pressure_<side>` on
`openstack-db` and a Memcached of its own, because two Keystones on one
Memcached read each other's cached users. A pod `keystone-pressure-client` on
the dizzy image runs every profile against both, so the latencies carry no
Gateway, port-forward or TLS. The order of the two builds alternates from
profile to profile, and `revocation-lookup`, whose revocation events stay in
the database, runs last. The results go to `_output/keystone-pressure/<release>-<UTC time>/`:
per profile a `base/` and a `patched/` run record and `comparison.txt`,
`comparison.html` and `comparison.json`, and `summary.md` for all of them.
The script removes the two Keystones, their Memcacheds and the client pod at
the end; the two databases stay and are migrated again by the next run.

| Variable | Default | Effect |
| --- | --- | --- |
| `KEYSTONE_PRESSURE_BASE_IMAGE` | (required) | Local image of the build without the change |
| `KEYSTONE_PRESSURE_PATCHED_IMAGE` | (required) | Local image of the build with it |
| `KEYSTONE_PRESSURE_RELEASE` | the newest release under `releases/` | The release both images ship |
| `KEYSTONE_PRESSURE_PROFILES` | `warm-cache effective-fan cold-role-cache revocation-lookup` | The profiles to run, space-separated |
| `KEYSTONE_PRESSURE_DURATION` | each profile's own | dizzy `--duration` of every run, such as `2m` |
| `KEYSTONE_PRESSURE_PROFILE_DURATIONS` | none | `<profile>=<duration>` pairs, space-separated, that override `KEYSTONE_PRESSURE_DURATION` for one profile each, such as `effective-fan=6m` |
| `KEYSTONE_PRESSURE_ARGS` | none | Extra `dizzy keystone pressure` flags, such as `--workers 64` |
| `KEYSTONE_PRESSURE_REPLICAS` | `1` | Keystone API replicas per build |
| `KEYSTONE_PRESSURE_OUTPUT` | `_output/keystone-pressure/<release>-<UTC time>` | Output directory |
| `KEYSTONE_PRESSURE_LOAD_IMAGES` | `true` | `false` skips tagging and loading; the nodes then need the two side tags and the dizzy image already |
| `KEYSTONE_PRESSURE_KEEP` | `false` | `true` leaves the Keystones and the client pod; `hack/keystone-pressure-ab.sh cleanup` removes them |
| `DIZZY_VERSION` | the pin of `hack/dizzy.sh` | dizzy image tag |
| `KIND_CLUSTER` | `cobaltcore` | kind cluster the images go to |

The script stops before it changes anything when a setting is missing or
malformed, a profile has no file, or the stack lacks the keystone-operator,
the memcached-operator, `openstack-db` or the Secrets `keystone-db` and
`keystone-admin`; each check names its own cause. A docker on the containerd
image store fails `kind load docker-image` for a pulled multi-platform image;
the script then loads the node's platform from an archive, as above.

## Runs against a ControlPlane

`make dizzy-keystone-pressure` runs one profile against the Keystone of the
quick-start ControlPlane from the workstation, with the admin credentials of
`controlplane-keystone-admin-credentials`, like
[`make dizzy-keystone`](dizzy-chaos-testing.md#running-a-soak). Run it once
per build under two labels and compare the two records:

```bash
# On the build without the patch.
DIZZY_PRESSURE_PROFILE=token-propagation DIZZY_PRESSURE_LABEL=base \
  make dizzy-keystone-pressure

# Roll the patched image out, wait for the ControlPlane to be Ready, then:
DIZZY_PRESSURE_PROFILE=token-propagation DIZZY_PRESSURE_LABEL=patched \
  make dizzy-keystone-pressure

make dizzy-keystone-compare \
  DIZZY_BASE=_output/dizzy/pressure/base/token-propagation \
  DIZZY_OTHER=_output/dizzy/pressure/patched/token-propagation
```

`hack/dizzy.sh pressure <profile>` starts dizzy in
`_output/dizzy/pressure/<DIZZY_PRESSURE_LABEL>/<profile>/`, where dizzy writes
`run-<id>.json`. `DIZZY_SCENARIO` replaces the profile's file, and
`DIZZY_ARGS` adds flags such as `--duration 30m`. The run exports over OTLP
when the dizzy stack is installed (`WITH_DIZZY=true`) and runs without the
export otherwise. `hack/dizzy.sh compare <base> <other>` takes a run record or
a directory, whose newest `run-*.json` it uses, prints the comparison, and
keeps it as text and HTML in `_output/dizzy/pressure/compare/`. `DIZZY_ARGS`
reaches the comparison too, e.g. `--allow-mismatch`.

On the metal-stack lab the run goes through the two port-forwards of
[Running a soak](dizzy-chaos-testing.md#running-a-soak) with
`EXTERNAL_CLUSTER=true`. A `kubectl port-forward` adds more latency and spread
than #1461 or #1464 move, so compare only the profiles whose deltas are
seconds or growth rates there, `token-propagation` and `effective-fan`, or
run dizzy in the cluster.

## Related

- [dizzy Chaos Testing](dizzy-chaos-testing.md): the churn soak and the dizzy
  stack.
- [CI Workflow](../ci-cd/ci-workflow.md#keystone-pressure): the
  `keystone-pressure` job.
- [Container Images](../ci-cd/container-images.md): how a downstream source
  patch reaches the Keystone image.
