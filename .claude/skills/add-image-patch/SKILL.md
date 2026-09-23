---
name: add-image-patch
description: >-
  Add, backport, forward-port, or retire a downstream source patch that the
  CobaltCore service image builds apply to an upstream OpenStack tree —
  patches/<svc>/<release>/NNNN-<slug>.patch cut with git format-patch against
  the tag releases/<release>/source-refs.yaml pins, carrying the house header
  (SPDX pair, rationale, Applies-to line, Upstream status), the upstream
  unit-test hunks a behaviour change needs, a verify_<svc>.sh assertion, and
  a Source patch paragraph in docs/reference/ci-cd/container-images.md. Use
  when a service needs a code fix no upstream tag ships yet, when a patch
  must follow to another release, when test-service-images turns red on an
  upstream unit test a patch changed, or when a Renovate "OpenStack upstream
  tags" bump fails the "Apply patches" step with "patch does not apply"
  because the new tag already carries the fix.
---

# Add, backport, or retire an image patch

A downstream patch changes the upstream source **before** it is installed
into the service image. It is the last resort: a fix in the operator, a
config option, or a newer upstream tag is always preferable, because every
patch has to be carried through every Renovate tag bump until upstream ships
it. The repository carries seven today (cinder 0001–0003, glance 0001, per
release); `check-image-patches.sh` lists them.

## How a patch reaches an image

| Consumer | Where | What it does with `patches/<svc>/<release>/*.patch` |
|---|---|---|
| `build-service-images` | `build-images.yaml` via `.github/actions/checkout-service-source` | `git -C src/<svc> apply` all files in glob order, then builds; on a PR it runs `tests/container-images/verify_<svc>.sh` against the fresh image (the `verify-script` input is PR-only) |
| `test-service-images` | same action, then `hack/ci-run-unit-tests.sh` | runs the **upstream** stestr suite (minus `releases/<release>/test-excludes/<svc>.txt`) against the patched tree |
| `build-e2e-images` | `ci.yaml` via `hack/ci-build-service-image.sh` step 4 | the same `git apply` glob, so e2e images match the published ones |
| `verify-service-images` | `build-images.yaml`, push to main only | `verify_<svc>.sh` against the published image |
| `derive-service-tags` | `.github/actions/derive-service-tags` | counts `*.patch` into the composite tag `<version>-p<N>-<branch>-<sha>` |

`git apply` takes the files one after another and stops at the first that
does not apply (the earlier ones are already written), failing the step and
with it the build. Paths filters
`patches/<svc>/**` exist in `build-images.yaml` (`changes` job) and
`ci.yaml` (`image_<svc>`, which runs that service's `e2e-operator` leg with
a fresh image) for every service.

## Procedure

### 1. Decide per release

A patch exists **once per `<release>` it applies to**; there is no shared
directory. For each release under `releases/`, read the pinned tag in
`releases/<release>/source-refs.yaml` and answer: does that tag have the
bug?

- **New fix** — write it against the newest affected release, then
  forward-port or backport it to the others.
- **Backport of an upstream commit** — carry it only where the pinned tag
  lacks it. Cinder 0003 exists for 2025.2 only: 28.0.0 carries upstream
  `cc981d81b6`, 27.0.0 does not, and no 27.0.x tag exists to pin instead.
  Prefer bumping `source-refs.yaml` to a tag that contains the fix whenever
  one exists.
- **Twins** keep the same slug and, where possible, the same number. Cut each
  from its own tag: the cinder twins differ in their `index` lines and
  context. The glance twins are byte-identical although `store_utils.py`
  is not (32.0.0 rewrote `_update_s3_url`): the patched `_construct_s3_url`
  is identical, and the hunk applies to 32.0.0 at an offset. Cut from each
  tag anyway and let `git apply --check` (`--apply`) prove it.
  A release without a twin says why in its header ("No 2026.1 twin: 28.0.0
  already carries the commit.").

### 2. Cut the patch against the pinned tag

```bash
tag=$(yq '.cinder' releases/2025.2/source-refs.yaml)
git clone --branch "$tag" https://github.com/openstack/cinder.git /tmp/cinder-$tag
cd /tmp/cinder-$tag
# edit code AND the upstream tests that pin the old behaviour, then
git commit -am "Run the NFS driver's qemu-img info as the service user"
git format-patch -1 --no-signature --stdout > /tmp/0001-<slug>.patch
```

For a backport, `git cherry-pick -x <sha>` the upstream commit onto the tag
(the clone above carries full history), resolve, and format-patch that
commit. Then turn the mail envelope into
the house header (`From <sha>`, `From:`, `Date:` and `Subject: [PATCH]` go;
the diffstat may go): see
[references/patch-conventions.md](references/patch-conventions.md) for the
template and the phrasing the existing patches use.

Name it `patches/<svc>/<release>/NNNN-<slug>.patch`: four digits, next free
number in that directory, lowercase slug from the subject. `git apply` takes
the files in glob order, so the number is the application order; a later
patch may depend on an earlier one.

### 3. Carry the upstream test hunks

`test-service-images` runs the upstream unit suite against the patched
source (cinder 2025.2: about 17,900 tests, six minutes on four workers). A
patch that changes behaviour turns that job red unless it also moves every
upstream test that asserts the old behaviour. Cinder 0002 flipped
`run_as_root` on `fetch_verify_image` and failed three tests in
`cinder/tests/unit/test_image_utils.py` until their hunks joined the patch.

- Grep the upstream tree for the function and the value you change
  (`grep -rn "fetch_verify_image" cinder/tests/`), run those modules, and
  put the test edits into the **same** `.patch` file.
- When no upstream test pins the behaviour, the header says so and why
  (cinder 0001: the test driver never calls `set_nas_security_options`, so
  `test_copy_volume_from_snapshot` stays green; "The patch therefore carries
  no test hunk.").
- Never answer a red upstream test with a `test-excludes` entry; the
  exclude hides the behaviour change instead of recording it.

### 4. Prove the patch is in the image

Add a test to `tests/container-images/verify_<svc>.sh` that asserts the
**behaviour** in the built image, not the source text: cinder's Test 10 and
Test 11 drive the patched methods through `docker run … python -c` with
mocks and fail against an unpatched image. Name the patch path in the
test's comment and call the function at the bottom of the script. It runs
inside `build-service-images` on the PR and in `verify-service-images` after
the merge. A test-only patch (cinder 0003) changes nothing at runtime and
needs no assertion.

### 5. Document it

Add a **Source patch:** paragraph to the service's section of
`docs/reference/ci-cd/container-images.md` (cinder has three): the path, its
twins, what upstream does, why CobaltCore cannot live with it, what the patch
changes, which upstream tests moved (or why none), the `verify_<svc>.sh`
assertion, and the upstream status. When the patch creates a configuration
requirement for the operator, say so there (cinder 0001 needs
`nas_secure_file_operations = true`).

### 6. Check and test locally

```bash
bash .claude/skills/add-image-patch/scripts/check-image-patches.sh
bash .claude/skills/add-image-patch/scripts/check-image-patches.sh --apply --service cinder
```

- **P1–P4** — layout and name, release and `source-refs.yaml` key, paths
  filters, a diff with the house header (SPDX pair, `Upstream status:`, the
  pinned tag named), gap-free numbering.
- **P5** — twins in the other releases, identical or not; a missing twin
  the header does not mention is flagged for a decision.
- **P6** — the file name appears in `container-images.md` (`[FAIL]`).
- **P7/P8** — test hunks, or a header that says why none (`[WARN]`); the
  slug in `verify_<svc>.sh` for any patch that changes non-test code
  (`[WARN]`).
- **P9** (`--apply`, network) — shallow-clones each pinned tag into a
  temporary directory and applies the patches cumulatively in glob order; a
  patch that only reverse-applies is already upstream.

Then run the affected upstream tests and, when it matters, the full suite
and an image build: the recipes (including the macOS build workaround) are
in [references/patch-conventions.md](references/patch-conventions.md).

### 7. Commit

Subjects of the previous patch commits:

```text
fix(patches): run the NFS driver's qemu-img info as the service user          (0ed260e9)
fix(patches): backport the NetApp mutable-fakes test fix for cinder 2025.2    (64754d84)
images(cinder): run create-from-image qemu-img as the service user            (45fe3122)
```

The body repeats the rationale,
names the tests that moved, and records how the patch was verified (for
example `git apply --check` against both pristine tags, and a second
`git apply` failing as proof the hunk landed once). The patch, its test
hunks, the verify assertion and the docs paragraph travel in one commit.

## Retiring a patch

Renovate bumps `source-refs.yaml` tags in the "OpenStack upstream tags"
group (minor and patch updates, three-day soak, automerge). When the new tag
contains the fix, or rewrites the patched lines, the bump PR fails in
`build-service-images (<svc>, <release>)` and
`test-service-images (<svc>, <release>)`, step "Checkout service source" →
"Apply patches", with `error: patch failed: <file>:<line>` and
`error: <file>: patch does not apply`; nothing builds, and the automerge
stalls.

1. Check out the bump branch and run the checker with
   `--apply --service <svc>`. "already contained … (reverse-applies)" means
   retire;
   "does not apply" means the context moved: re-cut the patch against the
   new tag (step 2) and re-run the tests.
2. Retire on the bump branch itself, since the bump cannot land without it:
   delete the file, renumber the later patches of that directory so the
   numbering stays gap-free, update the docs paragraph ("fixed upstream in
   <tag>" or remove it), and keep or drop the `verify_<svc>.sh` assertion
   (keeping it turns it into a regression check on upstream).
3. The composite image tag drops from `p<N>` to `p<N-1>`; nothing pins it.

## Gotchas

- **The mail envelope is optional, the diff is not.** `git apply` skips all
  prose before the first `diff --git`, so the header costs nothing, but a
  file without a diff fails with "No valid patches in input".
- **Only `*.patch` counts.** A README or a `.diff` under
  `patches/<svc>/<release>/` is ignored by the apply glob and by the `p<N>`
  count.
- **A new service needs its filter lines.** `patches/<svc>/**` must be in the
  `changes` filter of `build-images.yaml` and in `image_<svc>` of `ci.yaml`;
  `docs/contributing/adding-a-new-operator.md` and
  `docs/reference/ci-cd/build-images-workflow.md` list them, and P2 fails
  without them.
- **Patch order is file-name order.** Two patches touching the same hunk must
  be numbered in the order they were cut; P9 applies them cumulatively the
  way CI does.
- **`hack/ci-build-service-image.sh` rewrites a tracked file on Linux.**
  `scripts/apply-constraint-overrides.sh` runs `sed -i` on
  `releases/<release>/upper-constraints.txt` whenever
  `overrides/<release>/constraints.txt` exists (it does for 2025.2 and
  2026.1); restore the file after a local build. On macOS the same `sed -i`
  aborts the build (BSD sed); use the manual build in the reference.
- **The runtime image is not the test image.** `verify_<svc>.sh` runs against
  the service image; `test-service-images` installs the patched tree into the
  `venv-builder` image with the test requirements. A green verify script
  says nothing about the upstream suite.

## Notes

- `check-image-patches.sh` is read-only for the repository; `--apply` works
  in a `mktemp` directory and removes it on exit.
- Pair with [[check-renovate-coverage]] when a patch motivated a
  `source-refs.yaml` pin, and with [[prepare-new-release]] when a new release
  directory needs forward-ported twins.
