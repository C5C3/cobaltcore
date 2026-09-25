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
- A release without a twin says why, in the patch that exists: "No 2026.1
  twin: 28.0.0 already carries the commit."
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
git -C /tmp/cinder-27.0.0 apply --check "$PWD"/patches/cinder/2025.2/0001-*.patch
git -C /tmp/cinder-27.0.0 apply "$PWD"/patches/cinder/2025.2/0001-*.patch
git -C /tmp/cinder-27.0.0 apply "$PWD"/patches/cinder/2025.2/0001-*.patch   # must fail: "patch does not apply"
```

`check-image-patches.sh --apply` automates the first two for every patch in
build order; a patch that fails forward but applies with `--reverse` is
already contained in the tag.

## Running the affected upstream tests

**Targeted, in the published runtime image.** The service image ships the
package with its `tests/` tree and `pip`; only test-only dependencies are
missing. Mount the patched package over the installed one:

```bash
img=ghcr.io/c5c3/cinder:2025.2
pkg=$(docker run --rm --entrypoint /var/lib/openstack/bin/python "$img" \
  -c 'import cinder, os; print(os.path.dirname(cinder.__file__))')
docker run --rm --user 0 -v /tmp/cinder-27.0.0/cinder:"$pkg" --entrypoint /bin/sh "$img" -c \
  '/var/lib/openstack/bin/python -m pip install --quiet ddt oslotest &&
   /var/lib/openstack/bin/python -m unittest cinder.tests.unit.test_image_utils'
```

**The full suite, as `test-service-images` runs it.** Build the base images
once (`docker build -t python-base images/python-base/`, then
`docker build -t venv-builder images/venv-builder/`) and point
`hack/ci-run-unit-tests.sh` at a scratch workspace; it writes only below
`WORKSPACE_DIR`:

```bash
ws=$(mktemp -d); rel=2025.2; svc=cinder
tag=$(yq ".${svc}" releases/$rel/source-refs.yaml)
git clone --depth 1 --branch "$tag" https://github.com/openstack/$svc.git "$ws/src/$svc"
git -C "$ws/src/$svc" apply "$PWD"/patches/$svc/$rel/*.patch
mkdir -p "$ws/releases" && cp -R releases/$rel "$ws/releases/"
extras=$(yq -r ".${svc}.pip_extras // [] | join(\",\")" releases/$rel/extra-packages.yaml)
WORKSPACE_DIR="$ws" SERVICE_NAME=$svc SERVICE_VERSION="$tag" RELEASE=$rel \
  INSTALL_SPEC=".${extras:+[$extras]}" VENV_BUILDER_IMAGE=venv-builder \
  bash hack/ci-run-unit-tests.sh
```

The cinder 2025.2 suite runs about 17,900 tests in roughly six minutes on
four workers. Order-dependent failures need a single worker: the NetApp
flake behind cinder 0003 failed every time under
`stestr run --concurrency 1 <module>` and only about one run in four in CI,
because stestr spreads tests over workers in hash order.

## Building the image locally

`hack/ci-build-service-image.sh` applies the patches exactly as CI does:

```bash
OPERATOR=cinder IMAGE_PREFIX=c5c3 RELEASE=2025.2 bash hack/ci-build-service-image.sh
bash tests/container-images/verify_cinder.sh c5c3/cinder:2025.2
```

Two host caveats, both from `scripts/apply-constraint-overrides.sh`, which
the build script calls after the patches:

- **Linux:** its `sed -i` edits the tracked
  `releases/<release>/upper-constraints.txt` in place whenever
  `overrides/<release>/constraints.txt` exists. Restore the file after the
  build (`git diff --stat releases/` shows it).
- **macOS:** BSD `sed` rejects the same `sed -i` ("sed: -I or -i may not be
  used with stdin") and the build stops after the clone. Build by hand:

```bash
rel=2025.2; svc=cinder; tag=$(yq ".${svc}" releases/$rel/source-refs.yaml)
ovr=$(mktemp -d); mkdir -p "$ovr/scripts" "$ovr/releases/$rel" "$ovr/overrides/$rel"
cp scripts/apply-constraint-overrides.sh "$ovr/scripts/"
cp releases/$rel/upper-constraints.txt "$ovr/releases/$rel/"
cp overrides/$rel/constraints.txt "$ovr/overrides/$rel/"
docker run --rm -v "$ovr":/repo -w /repo --user 0 python-base bash scripts/apply-constraint-overrides.sh $rel
git clone --depth 1 --branch "$tag" https://github.com/openstack/$svc.git "/tmp/$svc-src-$tag"
git -C "/tmp/$svc-src-$tag" apply "$PWD"/patches/$svc/$rel/*.patch
docker build -t c5c3/$svc:$rel \
  --build-arg PIP_EXTRAS="$(yq -r ".${svc}.pip_extras // [] | join(\",\")" releases/$rel/extra-packages.yaml)" \
  --build-arg PIP_PACKAGES="$(yq -r ".${svc}.pip_packages // [] | join(\" \")" releases/$rel/extra-packages.yaml)" \
  --build-arg EXTRA_APT_PACKAGES="$(yq -r ".${svc}.apt_packages // [] | join(\" \")" releases/$rel/extra-packages.yaml)" \
  --build-context $svc="/tmp/$svc-src-$tag" \
  --build-context upper-constraints="$ovr/releases/$rel" \
  images/$svc/
```

The default Docker driver resolves the Dockerfiles' `FROM python-base` and
`FROM venv-builder` to the local images built above.

## Where the patch shows up in CI

| Job | Proves | Red when |
|---|---|---|
| `build-service-images (<svc>, <release>)` | the patch applies, the image builds, `verify_<svc>.sh` passes (PR only) | the patch does not apply; a verify assertion fails |
| `test-service-images (<svc>, <release>)` | the upstream unit suite passes on the patched tree | an upstream test pins the old behaviour and its hunk is missing |
| `e2e-operator` leg of `<svc>` (`ci.yaml`, `image_<svc>` filter) | the operator runs against the freshly built image | the behaviour change breaks the service at runtime |
| `verify-service-images` (push to main) | the published image carries the patch | as for the PR verify step |
