# Image patch conventions and local test recipes

Reference for [add-image-patch](../SKILL.md). Derived from the seven patches
under `patches/` and the commits that added them (f3370106, 0ed260e9,
45fe3122, 64754d84).

## House header

Every patch opens with prose that `git apply` skips; the diff starts at the
first `diff --git`. The layout, top to bottom:

```text
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0

<Subject: imperative, what the patch changes>

<What upstream does, in which file and function, and how it breaks
CobaltCore: the failure mode, the evidence (issue, spike, CI run).>

<Why this change and not another; what it does not cover; any operator
configuration it depends on.>

<Tests: which upstream unit tests pin the old behaviour and move with the
patch (hunks included), which keep their old value and why — or why no
upstream test pins it ("The patch therefore carries no test hunk.").>

<Proof: tests/container-images/verify_<svc>.sh asserts ... against the
built image.>

Applies to <svc> <tag> (<release>).

Upstream status: not yet proposed.

diff --git a/<path> b/<path>
...
```

Conventions the existing patches follow:

- The SPDX pair is bare text (no `#`), because the file is not a script.
- The subject line is the commit subject upstream would get, not a
  Conventional Commit.
- **Applies to** names the service and the exact tag, which lets
  `check-image-patches.sh` (P3) warn once `source-refs.yaml` moves on. A
  byte-identical twin may name both tags, and says where the files differ
  when the hunk applies at an offset (the glance twins: the patched function
  is identical in 31.1.0 and 32.0.0, the rest of `store_utils.py` is not).
- **Upstream status** takes one of three forms:

  ```text
  Upstream status: not yet proposed.
  Upstream status: proposed as <review URL>.
  Upstream status: merged on master as <sha>, backported to stable/<series> as <sha> (<date>, Closes-bug: #<n>).
  ```

- A backport starts with its provenance: "Backport of upstream commit
  cc981d81b6 (master, 2025-09-17, Change-Id I50c4…). Cinder 28.0.0
  contains it, 27.0.0 does not, and no 27.0.x tag exists that could be
  pinned instead."
- A release without a twin says why, in the patch that exists: "No 2026.2
  twin: 29.0.0 carries upstream commit c07c49c586, which replaced both
  tests."
- No diffstat and no `-- ` signature trailer: the `--no-signature` flag of
  `git format-patch` drops the trailer, the diffstat is deleted by hand.

Converting `git format-patch` output: delete the `From <sha> <date>` line
and the `From:` and `Date:` headers, turn `Subject: [PATCH] <subject>` into
the bare subject line, insert the SPDX pair above it, delete the `---`
diffstat block before the first `diff --git`, and add the Applies-to and
Upstream-status lines.

## Proving the patch applied exactly once

0ed260e9 recorded the check its author ran against pristine checkouts of
both tags (from the repository root):

```bash
git -C /tmp/cinder-28.0.0 apply --check "$PWD"/patches/cinder/2026.1/0001-*.patch
git -C /tmp/cinder-28.0.0 apply "$PWD"/patches/cinder/2026.1/0001-*.patch
git -C /tmp/cinder-28.0.0 apply "$PWD"/patches/cinder/2026.1/0001-*.patch   # must fail: "patch does not apply"
```

`check-image-patches.sh --apply` automates the first two for every patch in
build order; a patch that fails forward but applies with `--reverse` is
already contained in the tag.

## Running the affected upstream tests

**Targeted, in the published runtime image.** The service image ships the
package with its `tests/` tree and `pip`; only test-only dependencies are
missing. Mount the patched package over the installed one:

```bash
img=ghcr.io/c5c3/cinder:2026.1
pkg=$(docker run --rm --entrypoint /var/lib/openstack/bin/python "$img" \
  -c 'import cinder, os; print(os.path.dirname(cinder.__file__))')
docker run --rm --user 0 -v /tmp/cinder-28.0.0/cinder:"$pkg" --entrypoint /bin/sh "$img" -c \
  '/var/lib/openstack/bin/python -m pip install --quiet ddt oslotest &&
   /var/lib/openstack/bin/python -m unittest cinder.tests.unit.test_image_utils'
```

**The full suite, as `test-service-images` runs it.** `git add` the patch,
write the lock, and build the test run of the service and release:

```bash
rel=2026.1; svc=cinder
git add patches/$svc/$rel/
make nix-lock
nix build ".#${svc}-${rel//./_}-test-run" && cat result/summary.txt
cat result/exit-status    # 0 when the suite passed
```

The run uses the source of the lock with every listed patch applied, the
release exclude list, and MySQL and PostgreSQL inside the build sandbox
(`nix/lib/service-tests.nix`). `result/testresults.subunit` is the stream CI
uploads. The cinder 2026.1 suite counted 18,076 tests on its first 28.0.0 run
(`releases/2026.1/test-excludes/cinder.txt:10-11`). Order-dependent failures
need a single worker, because stestr spreads tests over workers in hash order.
Some flakes do not reproduce at all on demand: the tpool flake behind
`patches/cinder/2026.1/0003-tests-collect-garbage-before-the-backup-tpool-size-tests.patch`
failed one CI run and reproduces only when a garbage collection lands
between the two assertions.

## Building the image locally

The flake builds the image from the same patched source CI uses:

```bash
nix build .#cinder-2026_1-image && ./result | docker load
bash tests/container-images/verify_cinder.sh cinder:2026.1
```

`docker load` names the image `<svc>:<release>`. A patch the lock does not list
is not in the image, so run `make nix-lock` after adding one.

## Where the patch shows up in CI

| Job | Proves | Red when |
|---|---|---|
| `nix-lock-check` | the lock lists the patch set of the tree | a patch was added, renumbered or retired without `make nix-lock` |
| `build-service-images (<svc>, <release>)` | the patch applies, the image builds, `verify_<svc>.sh` passes (PR only) | the patch does not apply; a verify assertion fails |
| `test-service-images (<svc>, <release>)` | the upstream unit suite passes on the patched tree | an upstream test pins the old behaviour and its hunk is missing |
| `e2e-operator` leg of `<svc>` (`ci.yaml`, `image_<svc>` filter) | the operator runs against the freshly built image | the behaviour change breaks the service at runtime |
| `verify-service-images` (push to main) | the published image carries the patch | as for the PR verify step |
