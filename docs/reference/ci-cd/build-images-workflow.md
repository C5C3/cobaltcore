---
title: Build Images Workflow
quadrant: infrastructure
---

::: v-pre

# Build Images Workflow

Reference documentation for the GitHub Actions build-images workflow and the verify-container-images workflow. The build-images workflow builds, tags, and publishes container images for
OpenStack services to GHCR (GitHub Container Registry). Each pushed image receives OCI
Image Spec annotations, a CycloneDX SBOM, a Sigstore-signed attestation, and a Grype vulnerability scan with SARIF upload to the GitHub Security tab. The verify-container-images workflow runs static verification tests against
container infrastructure files (Nix expressions, locks, workflows, release configs) without requiring
Docker.

Repeated inline step sequences are extracted into reusable composite GitHub Actions
(`.github/actions/setup-docker-registry/`, `.github/actions/supply-chain-attest/`,
`.github/actions/checkout-service-source/`, `.github/actions/export-digest/`) and
standalone CI scripts (`hack/ci-merge-manifest.sh`, `hack/nix-lock.sh`), reducing
duplication across jobs. All jobs produce identical outputs and maintain the same
security pipeline. See [Reusable Components](#reusable-components) for details.

## File Locations

| File | Path |
| --- | --- |
| Build Images workflow | `.github/workflows/build-images.yaml` |
| Verify Container Images workflow | `.github/workflows/verify-container-images.yaml` |
| Setup Docker Registry action | `.github/actions/setup-docker-registry/action.yaml` |
| Supply Chain Attest action | `.github/actions/supply-chain-attest/action.yaml` |
| Checkout Service Source action | `.github/actions/checkout-service-source/action.yaml` |
| Export Digest action | `.github/actions/export-digest/action.yaml` |
| Build Nix Image action | `.github/actions/build-nix-image/action.yaml` |
| Setup Nix action | `.github/actions/setup-nix/action.yaml` |
| Merge Manifest script | `hack/ci-merge-manifest.sh` |
| Lock generator | `hack/nix-lock.sh` |
| Resolve image changes script | `hack/ci-resolve-image-changes.sh` |
| Generate build matrix script | `hack/ci-generate-build-matrix.sh` |
| Resolve OVN version script | `hack/ci-resolve-ovn-version.sh` |
| Resolve hvo commit script | `hack/ci-resolve-hvo-commit.sh` |
| Resolve kna commit script | `hack/ci-resolve-kna-commit.sh` |
| Libvirt keeper tag script | `hack/ci-tag-libvirt-keeper.sh` |

Both workflow files use the `.yaml` extension and quote the trigger key as `"on"` to
prevent YAML boolean interpretation. They start with the standard SPDX license
header (matching `ci.yaml`).

## Trigger Events

The workflow triggers on three events:

| Event | Scope | Description |
| --- | --- | --- |
| `push` | `branches: [main, stable/**]` | Runs on every push to `main` or any `stable/**` branch (recursive glob) |
| `pull_request` | all branches | Runs when a changed path is one of the workflow's inputs; the [`changes`](#changes) job then decides which images the run builds |
| `workflow_dispatch` | manual run | Builds every image, like a push |

Push events produce multi-arch images pushed to GHCR. Pull request events produce
single-arch images loaded locally for testing (see [PR vs Push Behavior](#pr-vs-push-behavior)).

The two path lists differ. The push list is the broad one it has always been:
`hack/ci-*` and `.github/actions/**` as catch-alls, so any CI script or composite
action republishes the images. The pull-request list names the inputs this workflow
actually reads: the images and their build contexts, `releases/**`, `patches/**`,
`scripts/**`, `overrides/**`, the option catalogs it verifies, and the ten
composite actions and ten `hack/` scripts its jobs call, directly or through
`build-nix-image`, `setup-docker-registry` and `merge-manifest-and-attest`. Both lists name `flake.nix`, `flake.lock`,
`nix/lib/**`, `nix/pkgs/**`, `nix/locks/**` and `nix/base-requirements.txt`, the
inputs of the Nix-built images and their locks, and `hack/nix-lock.sh`, which
`nix-lock-check` runs, so a Renovate pull request that moves only the base pins still
runs that check; the pull-request list also names
`.github/actions/build-nix-image/**`, which the push list's `.github/actions/**`
covers. Four
negative patterns exclude `tests/container-images/verify_build_images_workflow.sh`,
`verify_deviation_comments.sh`, `verify_release_config.sh` and
`verify_spdx_headers.sh`: those four belong to
[Verify Container Images](#verify-container-images-workflow), which runs them, while
this workflow never does. GitHub evaluates the list in order, so each negation follows
the positive `tests/container-images/**` entry it carves out of. Both lists also name
`tests/lib/ceph_bindings.sh`, the helper the glance, cinder and nova-compute verify
scripts source, and so do the `svc_glance`, `svc_cinder` and `svc_nova` filters of the
[`changes`](#changes) job. They name `tests/lib/build_tools.sh` too, the build-tools
check every service and Tempest verify script sources, which the `base` filter names
because it rebuilds those images (the `ovn` and `libvirt` filters name it as well), and
`tests/lib/static_image.sh`, the
static-image check the hvo and kna verify scripts source, which the `hvo` and `kna`
filters name.

> **Fork PRs are not supported.** Fork PRs receive a read-only `GITHUB_TOKEN`
> that cannot write packages. The workflow detects fork PRs and fails fast with a
> clear error message.

## Permissions

Top-level permissions grant least privilege. Registry write access and
attestation permissions are scoped to merge and build jobs only:

```yaml
# Top-level (applies to all jobs)
permissions:
  contents: read

# Job-level (merge jobs)
permissions:
  contents: read
  packages: write
  id-token: write
  attestations: write
  artifact-metadata: write
  security-events: write

# Job-level (build-service-images, build-nova-compute-image)
permissions:
  contents: read
  packages: write
  id-token: write
  attestations: write
  artifact-metadata: write

# Verification jobs (verify-service-images, verify-nova-compute-image)
permissions:
  contents: read
  packages: read
```

`contents: read` allows repository checkout. `packages: write` is granted to
the build jobs, `build-service-images` and `build-tempest` among them (for pushing
per-platform digests), and to the merge jobs, `merge-tempest-image` and
`merge-service-images` among them (for pushing manifest lists).
`id-token: write` enables Sigstore keyless OIDC signing — the GitHub Actions runner
requests a short-lived OIDC token bound to the workflow identity, which Sigstore uses to
sign the attestation without managing keys. `attestations: write` grants
access to the GitHub Attestations API for storing signed attestations.
`security-events: write` allows uploading Grype vulnerability scan results in SARIF
format to the GitHub Security tab; it is scoped to the `merge-*` jobs, the only jobs that
upload. `id-token`, `attestations` and `artifact-metadata` are scoped to the merge jobs,
`build-service-images` and `build-nova-compute-image`. The per-platform build jobs run
the PR-only Grype scan via `supply-chain-attest` with `scan-mode: image` but upload no
SARIF, so none of them holds `security-events`. The verification jobs
(`verify-service-images`, `verify-nova-compute-image` and the other `verify-*` jobs)
do **not** receive `id-token`,
`attestations`, or `security-events` permissions — they only need `contents: read` (for
checkout and test scripts) and `packages: read` (for pulling images from GHCR),
following the principle of least privilege.

## Concurrency

The workflow uses a concurrency group scoped per-branch per-workflow:

```yaml
concurrency:
  group: ${{ github.ref }}-${{ github.workflow }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}
```

For pull requests, pushing new commits cancels any in-progress build for that same PR
branch. For pushes to `main` or `stable/**`, in-progress runs are **not** cancelled,
ensuring every merge commit produces a complete set of images.

## Reusable Components

Repeated inline step sequences are extracted into eleven composite GitHub Actions and
eleven `hack/` scripts. These components encapsulate patterns that were previously
duplicated across multiple jobs, ensuring consistency. This section documents six of
the actions and four of the scripts. All components follow the repository's CI script
and composite action conventions.

### setup-docker-registry

Configures Docker Buildx, authenticates to a container registry, and optionally installs
cosign for image signing. Replaces the three-step setup sequence (buildx + login + cosign)
that was previously inlined in every job.

| Input | Required | Default | Description |
| --- | --- | --- | --- |
| `registry` | no | `ghcr.io` | Container registry URL |
| `username` | yes | — | Registry username |
| `password` | yes | — | Registry password/token |
| `install-cosign` | no | `'true'` | Whether to install cosign |

**Usage:** Every job that pulls or pushes images calls this composite action: 23 of the
29 jobs, all but `changes`, `nix-flake-check`, `nix-lock-check`,
`prepare`, `generate-matrix` and `test-service-images`. Jobs
that do not need cosign (build jobs, verification jobs) pass `install-cosign: 'false'`.
Only merge jobs (which run attestation and signing) leave it at the default `'true'`.

### supply-chain-attest

Runs the full supply chain security pipeline for a container image: CycloneDX SBOM
generation, Grype vulnerability scanning, SARIF upload to the GitHub Security tab (push
events only), SBOM attestation, build provenance attestation, and cosign signing.
Replaces the ~65-line
inline sequence that was previously duplicated for each image type.

| Input | Required | Default | Description |
| --- | --- | --- | --- |
| `image-name` | yes | — | Bare image name without tag/digest (e.g. `ghcr.io/c5c3/keystone-federation-proxy`) |
| `image-digest` | no | `''` | Image digest (`sha256:...`); required when `scan-mode` is `sbom` |
| `sbom-output-file` | no | `''` | SBOM output filename (e.g. `sbom-keystone-federation-proxy.cyclonedx.json`); required when `scan-mode` is `sbom` |
| `grype-category` | yes | — | SARIF category for the GitHub Security tab |
| `scan-mode` | no | `'sbom'` | `'sbom'` for full supply chain (non-PR) or `'image'` for scan-only (PR) |
| `image-ref-for-scan` | no | `''` | Full image ref for image-mode scan |

**Scan modes:**

- **`sbom` mode** (push events): Generates SBOM, scans via SBOM, uploads SARIF, creates
  SBOM attestation, creates build provenance attestation, and signs with cosign. This is
  the full supply chain pipeline.
- **`image` mode** (PR events): Scans the image directly with Grype. No SARIF upload, no
  SBOM generation, no attestation, no signing. The scan keeps the tooling exercised on
  PRs and is where `fail-build` turns into a severity gate once one is wanted.

**Usage:** Every merge job calls this composite action through
`merge-manifest-and-attest`, and the build jobs through `build-nix-image`.
Merge jobs use `sbom` mode; build jobs use `image` mode for PR-only
scans. This replaces the per-image-type inline SBOM + Grype + SARIF + attest + provenance
+ cosign sequences.

### checkout-service-source

Resolves the upstream source ref for an OpenStack service from `source-refs.yaml`, checks
out the upstream repository, applies release-specific patches, and runs constraint
overrides. Replaces the multi-step source preparation sequence that was previously
duplicated (with "MUST stay in sync" comments) between `build-service-images` and
`test-service-images`.

| Input | Required | Default | Description |
| --- | --- | --- | --- |
| `service` | yes | — | OpenStack service name (e.g. `keystone`) |
| `release` | yes | — | Release directory name (e.g. `2026.1`) |

| Output | Description |
| --- | --- |
| `source-ref` | Resolved version string from `source-refs.yaml` |

**Internal steps:**

1. Installs `yq` (SHA-pinned `mikefarah/yq`)
2. Reads the version from `releases/<release>/source-refs.yaml` via `yq` (fails with
   `::error::` if not found or null)
3. Checks out `openstack/<service>` at the resolved ref into `src/<service>`
4. Applies patches from `patches/<service>/<release>/*.patch` via `git apply` (skipped
   if no patches exist, guarded by a shell `compgen -G` check)
5. Runs `scripts/apply-constraint-overrides.sh <release>`

**Usage:** `build-service-images` calls this composite action for the option catalog
check, which reads the patched source.

### export-digest

Writes an image digest to a staging directory and uploads it as a GitHub Actions artifact.
Replaces the inline `mkdir` + `touch` + `upload-artifact` pattern that was previously
duplicated for each image type.

| Input | Required | Default | Description |
| --- | --- | --- | --- |
| `digest` | yes | — | Image digest (`sha256:...`) |
| `artifact-name` | yes | — | Name for the uploaded artifact |
| `digest-dir` | no | `/tmp/digests` | Directory for digest files |

**Usage:** `build-nix-image` calls this composite action after a push by digest,
replacing inline `mkdir`/`touch`/`upload-artifact` steps.

### build-nix-image

Builds a flake image attribute. Its inputs and steps are documented under
[The build-nix-image action](./nix-image-build.md#the-build-nix-image-action).

**Usage:** `build-keystone-federation-proxy`, `build-backup-shifter`,
`build-libvirt`, `build-ovn`, `build-hvo`, `build-kna`, `build-tempest`,
`build-service-images` and `build-nova-compute-image` call this composite action.

### setup-nix

Installs Nix with `cachix/install-nix-action` and adds the Cachix cache `cobaltcore`
with `cachix/cachix-action`, which runs with `continue-on-error: true`: a cache that
does not exist or does not answer fails no build, which then substitutes from
`cache.nixos.org`. A composite action cannot read secrets, so the job passes the
token in; without one the action reads the cache and pushes nothing. The
[binary cache](./nix-image-build.md#binary-cache) section of Nix Image Build
describes the cache.

| Input | Required | Default | Description |
| --- | --- | --- | --- |
| `cachix-auth-token` | no | `''` | The `CACHIX_AUTH_TOKEN` secret; empty pushes nothing (`skipPush`) |
| `push-filter` | no | `''` | Regular expression of the store paths never pushed (`pushFilter`) |

**Usage:** `nix-flake-check`, every job that calls `build-nix-image` and
`test-service-images` call this composite action with
<code v-pre>cachix-auth-token: ${{ secrets.CACHIX_AUTH_TOKEN }}</code>; `test-service-images` also
passes `push-filter: '-test-run$'`. `nix-lock-check` installs Nix itself, because it
hands the GitHub token to `install-nix-action` and reads no cache.

### hack/ci-merge-manifest.sh

Merges per-platform digest files into a multi-arch manifest using
`docker buildx imagetools create`, then inspects the result to extract and output the
final manifest digest. Follows the repo's CI-script conventions: shebang, SPDX header,
`set -euo pipefail`, env var interface with `::error::` annotations.

| Env var | Required | Default | Description |
| --- | --- | --- | --- |
| `IMAGE` | yes | — | Full image name without tag (e.g. `ghcr.io/c5c3/keystone-federation-proxy`) |
| `DIGEST_DIR` | yes | — | Path to directory containing digest files |
| `TAGS` | yes | — | Space-separated list of full tag references to apply |
| `INSPECT_TAG` | no | first entry in `TAGS` | Tag for post-creation digest inspection |

Writes `digest=sha256:<hex>` to `$GITHUB_OUTPUT`.

**Usage:** The `Create and push manifest` step of the `merge-manifest-and-attest`
composite action calls this script via `run:` with env vars passed through the step's
`env:` block. Every merge job reaches it through that action.

### hack/nix-lock.sh

Writes the locks under `nix/locks/` the Python images are built from, one per
service and release plus one per release for Tempest, the noVNC hash of
`images/nova/novnc.json`, and the hashes of the four pins
`images/ovn/source.json`, `images/keystone-federation-proxy/source.json`,
`images/openstack-hypervisor-operator/source.json` and
`images/kvm-node-agent/source.json`, with the OVS commit of the `ovs` gitlink
([Source-built images](./nix-image-build.md#source-built-images)). `--check`
regenerates every lock and pin and exits 1 with
`nix-lock.sh: the locks are stale; run make nix-lock and commit the result`
when the result differs from the committed files. The lock format, the
seeding and the Renovate flow are documented under
[Python environments and locks](./nix-image-build.md#python-environments-and-locks).

**Usage:** `nix-lock-check` runs `hack/nix-lock.sh --check` in the `lock-tools`
shell of the flake; `make nix-lock` and `make nix-lock-check` run it locally.

### hack/ci-resolve-image-changes.sh

Turns the paths-filter outputs of the [`changes`](#changes) job into the service list
and the image flags the build and test jobs gate on.

| Env var | Required | Default | Description |
| --- | --- | --- | --- |
| `EVENT_NAME` | yes | — | `github.event_name` |
| `ALL_SERVICES` | yes | — | Space-separated list of every service in the build matrix |
| `FILTER_svc_<service>` | no | `false` | One per name in `ALL_SERVICES` |
| `FILTER_base` | no | `false` | Flake inputs, Nix library and packages, release configs, build scripts, constraint overrides |
| `FILTER_tempest` / `FILTER_ovn` / `FILTER_proxy` / `FILTER_shifter` / `FILTER_libvirt` / `FILTER_hvo` / `FILTER_kna` | no | `false` | The release-independent images, one each |
| `FILTER_plumbing` | no | `false` | The workflow, its composite actions, its `hack/` scripts |
| `GITHUB_OUTPUT` | no | `/dev/null` | GitHub Actions output file |

Anything but the literal `true` is false, including the empty string the skipped filter
step yields on `push` and `workflow_dispatch`.

| Output | Value |
| --- | --- |
| `services` | `all`, a space-separated subset of `ALL_SERVICES` in `ALL_SERVICES` order, or empty |
| `has-services` | `true` when `services` is `all` or non-empty |
| `build-tempest` / `build-ovn` / `build-proxy` / `build-shifter` / `build-libvirt` / `build-hvo` / `build-kna` | `true` or `false` |

Three inputs resolve to everything, because everything is built from them: an event
that is not a `pull_request`, a `plumbing` match, and a `base` match. The `base` class
also sets `build-tempest`, since Nix builds the Tempest image from its lock with the
same flake inputs and library and it reads `releases/<release>/`. The federation proxy,
OVN, the backup shifter, libvirt, openstack-hypervisor-operator and kvm-node-agent are
built by Nix from their own pins and `flake.lock`. None of them reads a release, so they follow their own filters,
which also name the flake inputs, `nix/lib/` and the two Nix actions.

Every output line is echoed as well as written, so running the script prints what it
decided:

```console
$ EVENT_NAME=pull_request ALL_SERVICES="keystone glance" FILTER_svc_glance=true \
    bash hack/ci-resolve-image-changes.sh
services=glance
has-services=true
build-tempest=false
build-ovn=false
build-proxy=false
build-shifter=false
build-libvirt=false
build-hvo=false
build-kna=false
```

### hack/ci-generate-build-matrix.sh

Scans `releases/*/` and writes the four matrices the build, test and verify jobs
consume, plus the two nova-compute outputs. `SERVICES` restricts the two service
matrices to a subset:

| `SERVICES` | `matrix` and `build-matrix` | Exit |
| --- | --- | --- |
| unset or `all` | every `{service, release}` pair | 0 |
| a space-separated list | the pairs of those services, across every release | 0 |
| the empty string | `{"include":[]}` | 0 |
| a name no `source-refs.yaml` carries | not written | 1, with an `::error::` naming it |

`tempest-matrix` and `tempest-release-matrix` are never filtered: the Tempest image is
per release, not per service. The empty case reaches only jobs that gate on
`has-services`, so an empty `include` never reaches a job that runs.

`nova-compute-releases` and `nova-compute-build-matrix` follow the nova pairs
that remain after `SERVICES`, because `images/nova-compute/` is built from
nova's lock. `nova-compute-releases` is a JSON array of release names, `[]` when
no nova pair remains. `nova-compute-build-matrix` holds
`{release, platform, runner}` entries on the same platforms as `build-matrix`, and
is `{"include":[]}` when empty. The three
[nova-compute jobs](#build-nova-compute-image-merge-nova-compute-image-verify-nova-compute-image)
gate on the array, so an empty `include` never reaches one of them either. A
tree without `releases/` writes both empty next to the other four, and an
unknown service exits 1 before either is written.

## Jobs

The workflow's jobs form this dependency graph:

![The jobs of the build-images workflow in three families, with an arrow from every job a job needs. The release-independent images need changes, nix-flake-check and prepare: build-keystone-federation-proxy, build-backup-shifter and build-libvirt, each a matrix of amd64 and arm64, each followed by its merge job on push only. The Nix-locked images need nix-lock-check as well: build-ovn, build-hvo and build-kna, each followed by a merge job and a verify job on push only. The release-matrix images need generate-matrix as well, which itself needs only changes: build-tempest over release and platform with merge-tempest-image on push only, build-service-images over service, release and platform with merge-service-images and verify-service-images, which also needs test-service-images, the nix build of each service's test run, and build-nova-compute-image over release and platform with merge-nova-compute-image and verify-nova-compute-image on push only. The merge and verify jobs also need prepare or generate-matrix.](../../diagrams/ci-build-images-jobs.svg)

The figure leaves out the `needs` entries that a drawn path already implies:
`prepare` on every merge job except `merge-nova-compute-image`, and
`generate-matrix` on the merge and verify jobs of the release-matrix family.
`changes` supplies the service list the matrix is built from and the flags the
other jobs gate on. `test-service-images` needs `nix-lock-check` and
`generate-matrix` but not `nix-flake-check` or `prepare`. `build-libvirt` needs
no `nix-lock-check`, since no lock pin feeds the libvirt image.

Each platform (linux/amd64 on `ubuntu-latest`, linux/arm64 on `ubuntu-24.04-arm`) is
built on a native runner and pushed by digest. The merge job of the image then
assembles the multi-arch manifest list and runs the supply chain security pipeline via
the `supply-chain-attest` composite action, as `build-service-images` and
`merge-service-images` do for the service images and `build-tempest` and
`merge-tempest-image` for Tempest. Every build job builds a flake attribute with
`build-nix-image`. Every image this repository publishes is a flake attribute:
the CI workflow builds the operator images through the same action
([CI Workflow](./ci-workflow.md#build-and-push)).

The `test-service-images`
job runs the upstream unit tests of each service as a Nix build
(`nix/lib/service-tests.nix`), in parallel with `build-service-images`. On push events, the
`verify-service-images` job validates service images pulled from GHCR and gates on
`merge-service-images` and `test-service-images`. On PRs, the equivalent image
verification runs as an inline step within `build-service-images` because the loaded
image is available only on the same runner (ARM64 is excluded on PRs).

Every job that pulls or pushes images uses the `setup-docker-registry` composite action
for Docker Buildx setup, registry authentication, and optional cosign installation,
replacing the previously duplicated three-step setup sequence.

### changes

Classifies the changed paths of a pull request into paths filters — one per service
image, one for what every service image is built from (the flake inputs, the Nix
library and packages, the lock generator, the release configuration), one per
release-independent image, and one for the workflow's own plumbing — and hands them to
[`hack/ci-resolve-image-changes.sh`](#hack-ci-resolve-image-changes-sh), which resolves
them into the service list and the image flags.

| Property | Value |
| --- | --- |
| `runs-on` | `ubuntu-latest` |
| `timeout-minutes` | `8` |
| Permissions | `contents: read` |
| Outputs | `services`, `has-services`, `build-tempest`, `build-ovn`, `build-proxy`, `build-shifter`, `build-libvirt`, `build-hvo`, `build-kna` |

The `dorny/paths-filter` step carries `if: github.event_name == 'pull_request'`, since
the action diffs against the pull request base. On `push` and `workflow_dispatch` it is
skipped, its outputs resolve to the empty string, and the resolver answers `all` and
sets every image flag to `true` regardless. That is why no gated job needs a
`github.event_name` term in its condition.

`ALL_SERVICES` is a hand-maintained list, because `dorny/paths-filter` has no dynamic
filter names. `tests/unit/ci/build_images_services_lockstep_test.sh` pins it to the
union of the keys in `releases/*/source-refs.yaml` and fails when a service is missing
its filter or its `FILTER_svc_<service>` env line. It runs under `make test-shell` on
every pull request.

### nix-flake-check

The gate of every Nix-built image.

| Property | Value |
| --- | --- |
| `runs-on` | `ubuntu-latest` |
| `timeout-minutes` | `15` |
| Permissions | `contents: read` |

The job installs Nix and adds the Cachix cache `cobaltcore` through the
[`setup-nix`](#setup-nix) composite action, and runs three commands:

```bash
nix flake check --no-build --print-build-logs
nix build .#checks.x86_64-linux.nix-lint .#checks.x86_64-linux.fhs-layout \
  .#checks.x86_64-linux.service-tests --print-build-logs
nix eval --json .#packages.aarch64-linux --apply 'builtins.mapAttrs (_: p: p.drvPath)'
```

`nix flake check --no-build` evaluates every output of the runner's system and fails on
an attribute under `packages` or `checks` that is not a derivation. Without
`--no-build` it would also build every check, the unit tests of all sixteen service
locks included, which `test-service-images` runs one matrix entry each. The second
command builds the three checks that need no service lock: `nix-lint` (nixfmt,
statix, deadnix), `fhs-layout` and `service-tests`. `nix flake check` leaves the
other systems unevaluated, and `--all-systems` would try to build the arm64 checks on
the x86 runner, so `nix eval` instantiates every arm64 output without building it. The
`setup-nix` action pushes only when the job hands it the secret `CACHIX_AUTH_TOKEN`
(`skipPush: ${{ inputs.cachix-auth-token == '' }}`), and a cache it cannot reach fails
no build (`continue-on-error: true`), as described under
[Binary cache](./nix-image-build.md#binary-cache).

### nix-lock-check

Fails when the locks under `nix/locks/`, the noVNC hash or the hashes of the three
`images/<name>/source.json` pins differ from what the pins produce.

| Property | Value |
| --- | --- |
| `runs-on` | `ubuntu-latest` |
| `timeout-minutes` | `30` |
| Permissions | `contents: read` |

The job runs on every event. It installs Nix with the workflow token as
`github_access_token`, which lifts the GitHub API rate limit of the source prefetches,
and runs:

```bash
nix develop .#lock-tools --command bash hack/nix-lock.sh --check
```

A failure adds the annotation `the Nix locks or image pins are stale: run make nix-lock
and commit the result`. `build-service-images`, `build-nova-compute-image`,
`build-tempest`, `test-service-images`, `build-ovn`, `build-hvo` and `build-kna` need
the job, so no image and no unit test is built from a stale lock or pin. A Renovate pull request that
moves a pin the locks are derived from fails here until `make nix-lock` is pushed to
its branch ([Renovate](./nix-image-build.md#renovate)).

### build-keystone-federation-proxy / merge-keystone-federation-proxy-image

The Apache federation reverse-proxy sidecar for Keystone, with `mod_auth_openidc`
(OIDC) and `mod_auth_mellon` (SAML) in one image. The root flake builds it from
`images/keystone-federation-proxy/image.nix`: mod_auth_openidc from the commit
`images/keystone-federation-proxy/source.json` pins, Apache and mod_auth_mellon from
the `nixpkgs-images` revision `flake.lock` pins
([Nix Image Build](./nix-image-build.md#source-built-images)). The image carries no
OpenStack code, so the job pair has no release matrix. The two-platform build job
depends on `changes`, `nix-flake-check` and `prepare`, installs Nix, adds the Cachix
cache `cobaltcore` and calls [`build-nix-image`](#build-nix-image) with the attribute
`keystone-federation-proxy-image`. On a pull request the action loads the amd64 image
locally for the inline Grype scan and the
`tests/container-images/verify_keystone_federation_proxy.sh` verify script; on a push
it pushes each platform by digest with skopeo. The PR-skipped merge job assembles the
multi-arch manifest with the `:latest` + `:<sha>` tags, followed by the supply-chain
pipeline (SBOM, attestation, cosign).

Condition: `needs.changes.outputs.build-proxy == 'true'`. On a pull request the
build job runs when `images/keystone-federation-proxy/**`,
`tests/container-images/verify_keystone_federation_proxy.sh`,
`tests/lib/build_tools.sh`, `flake.nix`, `flake.lock`, `nix/lib/**`,
`.github/actions/build-nix-image/**` or `.github/actions/setup-nix/**` changed; on a
push it always runs. The arm64 image is built on a push only.

### build-backup-shifter / merge-backup-shifter-image

The rclone shifter that copies OVN database backups from a PVC to S3, the first
Nix-built image: the root flake builds it from `images/backup-shifter/image.nix`
([Nix Image Build](./nix-image-build.md)). It runs as the `shifter` container of the
`OVNCentral` backup CronJob, which the operator renders only when `spec.backup.s3` is
set. Like the federation proxy, the image carries no OpenStack code, so the job pair
has no release matrix. The two-platform build job depends on `changes`,
`nix-flake-check` and `prepare`, installs Nix, adds the Cachix cache `cobaltcore` and
calls [`build-nix-image`](#build-nix-image) with the attribute `backup-shifter-image`.
On a pull request the action loads the amd64 image locally for the inline Grype scan
and the `tests/container-images/verify_backup_shifter.sh` verify script; on a push it
pushes each platform by digest with skopeo. The PR-skipped merge job assembles the
multi-arch manifest with the `:latest` + `:<sha>` tags, followed by the supply-chain
pipeline, as for every release-independent image.

Condition: `needs.changes.outputs.build-shifter == 'true'`. On a pull request the
build job runs when `images/backup-shifter/**`,
`tests/container-images/verify_backup_shifter.sh`, `flake.nix`, `flake.lock`,
`nix/lib/**` or `.github/actions/build-nix-image/**` changed; on a push it always
runs. The arm64 image is built on a push only.

### build-libvirt / merge-libvirt-image

The libvirt daemon and QEMU for containerized hypervisor nodes, built by Nix: the root
flake builds `libvirt-image` from `images/libvirt/` with the libvirt of
`nix/pkgs/libvirt.nix`
([Hypervisor images](./nix-image-build.md#hypervisor-images)). The hypervisor package
of issue #1142 runs it as a DaemonSet beside `nova-compute`. Like the backup shifter,
the image carries no OpenStack code, so the job pair has no release matrix. The
two-platform build job depends on `changes`, `nix-flake-check` and `prepare`, has 60
minutes for the first build per `nixpkgs-images` revision, which compiles libvirt and
QEMU, installs Nix with the Cachix cache `cobaltcore` and calls
[`build-nix-image`](#build-nix-image) with the attribute `libvirt-image`. On a pull
request the action loads the amd64 image locally for the inline Grype scan and the
`tests/container-images/verify_libvirt.sh` verify script, which starts `libvirtd` and
queries the QEMU driver; on a push it pushes each platform by digest with skopeo and
uploads the vulnix report `vulnix-libvirt-<platform-pair>`. The PR-skipped merge job
assembles the multi-arch manifest with the `:<sha>` tag, plus `:latest` on `main`,
followed by the supply-chain pipeline. The arm64 build runs on a push only.

On `main`, the step `Mint the libvirt keeper tag` of the merge job runs
`hack/ci-tag-libvirt-keeper.sh` on the merged index and gives it the third tag,
`<libvirt-version>-r<N>` (see
[Release-independent images](#release-independent-images)). The script counts
commits, so on `main` the merge job checks out the full history
(`fetch-depth: 0`). The count reads commits and trees only, so the checkout
leaves the blobs outside `HEAD` on the server (`filter: blob:none`). Its
log ends in one of two ways: `<image>:<tag> already names <digest>; not moved`
when an earlier build holds the tag, or `tag=<tag>` and `minted=true` once the
new index carries the tag and a read-back returned its digest. Both lines are
also the step's outputs `tag` and `minted`. An existence check that fails with
anything but `not found` fails the step before it tags anything: moving an
existing tag would take the keeper off a digest the lab pins.

A `workflow_dispatch` run on a branch publishes `ghcr.io/c5c3/libvirt:<sha>` and moves
neither `latest` nor a keeper tag: the `e2e-nova-libvirt` leg of a pull request that
changes no libvirt input runs the image `latest` names. The lab pins no such build,
because the cleanup removes a branch's `<sha>` tag after 24 hours: a pull request that
changes the image leaves the lab pins alone, and Renovate moves them to the keeper tag
main's build mints after the merge.

Condition: `needs.changes.outputs.build-libvirt == 'true'`. On a pull request the
build job runs when `images/libvirt/**`, `nix/pkgs/libvirt.nix`,
`tests/container-images/verify_libvirt.sh`, `tests/lib/build_tools.sh`, `flake.nix`,
`flake.lock`, `nix/lib/**` or one of the two Nix actions changed; on a push it always
runs.

### build-ovn / merge-ovn-image / verify-ovn-image

OVN and Open vSwitch daemons and client tools, compiled from upstream source by
Nix: the root flake builds `ovn-image` from `images/ovn/`
([Source-built images](./nix-image-build.md#source-built-images)). The
`version` key of `images/ovn/source.json` is the whole version input.
`hack/ci-resolve-ovn-version.sh` reads it, and every job that needs the number
calls the script instead of reading the file. Open vSwitch comes from the `ovs`
submodule gitlink at the pinned OVN commit, so one bump moves both projects
(decision D1 of issue #898). Because a tag is a mutable ref, the pin records
`rev`, the commit the tag names, and `ovs.rev`, the commit the gitlink names
there, each with its NAR hash; `make nix-lock` derives `ovs.rev` from the
gitlink, and `nix-lock-check` fails while the committed pin differs. OVN's
`Documentation/intro/install/general.rst` notes under Build Requirements that
the submodule is "not recommended to be used as a source for OVS build";
ovn-kubernetes builds its OVS from that gitlink in
`dist/images/Dockerfile.fedora`, and this image does the same. Both trees are
fixed-output fetches by hash, so no token enters the build.

`build-ovn` needs `changes`, `nix-flake-check`, `nix-lock-check` and `prepare`,
and carries `if: needs.changes.outputs.build-ovn == 'true'`: on a pull request
it runs when `images/ovn/**`, `tests/container-images/verify_ovn.sh`,
`tests/lib/build_tools.sh`, `flake.nix`, `flake.lock`, `nix/lib/**`,
`.github/actions/build-nix-image/**` or `.github/actions/setup-nix/**` changed,
and on a push it always runs. Its `linux/amd64` + `linux/arm64` include matrix
is written out in the workflow, because the image has no `source-refs.yaml` key
for `generate-matrix` to discover and no release axis. The job reads the Cachix
cache through `setup-nix` and calls [`build-nix-image`](#build-nix-image) with
the attribute `ovn-image` and the vulnix artifact `vulnix-ovn-<pair>`. Without a
cached build of the pin both projects are compiled from source on a four-vCPU
runner, so the job carries `timeout-minutes: 60`, the longest allowance of any
build job here. On pull requests it builds amd64 only, loads the result locally
as `:pr-verify`, and runs the inline Grype scan and
`tests/container-images/verify_ovn.sh` against that tag. On push events it
pushes per-platform digests for the merge job.

`merge-ovn-image` (push only) assembles the manifest through
`merge-manifest-and-attest`: SBOM `sbom-ovn.cyclonedx.json`, Grype scan,
attestation, cosign signature. It publishes `<ovn-version>-<sha>` on every push
to `main` or `stable/**`, and adds `<ovn-version>` and `latest` on `main` (see
[Release-independent images](#release-independent-images)). Its `ovn-image`
output is the merged manifest as `ghcr.io/<owner>/ovn@<digest>`.

`verify-ovn-image` (push only) pulls that digest and runs `verify_ovn.sh`
against the multi-arch manifest, on a `[ubuntu-latest, ubuntu-24.04-arm]`
runner matrix. `docker pull` resolves the manifest to the runner's own
architecture, so the two legs execute the amd64 and the arm64 variant
respectively — pull requests compile amd64 only, and this job is where the
arm64 binaries first run. It holds `contents: read` and `packages: read` and
nothing else, so the post-merge check cannot write to the registry.

### build-hvo / merge-hvo-image / verify-hvo-image

openstack-hypervisor-operator, compiled by Nix from a pinned commit of its
upstream `main` branch with the patches under
`images/openstack-hypervisor-operator/patches/`: the root flake builds
`openstack-hypervisor-operator-image` from
`images/openstack-hypervisor-operator/`, the manager with `buildGo127Module` and
the image on `nix/lib/static-image.nix`. The `rev` key of
`images/openstack-hypervisor-operator/source.json` is the whole version input.
The Nix build and `hack/nix-lock.sh` read it themselves; every other job calls
`hack/ci-resolve-hvo-commit.sh`, which prints the 40-character commit. See
[openstack-hypervisor-operator](./container-images.md#openstack-hypervisor-operator)
for the patches and why the pin is a `main` commit.

`build-hvo` needs `changes`, `nix-flake-check`, `nix-lock-check` and `prepare`,
and carries `if: needs.changes.outputs.build-hvo == 'true'`: on a pull request
it runs when `images/openstack-hypervisor-operator/**`,
`tests/container-images/verify_hvo.sh`, `tests/lib/static_image.sh`, the flake
inputs, `nix/lib/**` or the `build-nix-image` and `setup-nix` actions changed,
and on a push it always
runs. Its `linux/amd64` + `linux/arm64` include matrix is written out in the
workflow, and it carries `timeout-minutes: 30`. It reads the Cachix cache
through `setup-nix` and calls [`build-nix-image`](#build-nix-image) with the
attribute `openstack-hypervisor-operator-image` and the vulnix artifact
`vulnix-hvo-<pair>`. The patch checks run inside the build. On pull requests it
builds amd64 only, loads the result locally as `:pr-verify`, and runs the
inline Grype scan and `tests/container-images/verify_hvo.sh` against that tag.
On push events it pushes per-platform digests for the merge job.

`merge-hvo-image` (push only) assembles the manifest through
`merge-manifest-and-attest`: SBOM `sbom-hvo.cyclonedx.json`, Grype scan,
attestation, cosign signature. It publishes `sha-<hvo-commit>-<sha>` on every
push to `main` or `stable/**`, and adds `sha-<hvo-commit>`,
`upstream-<hvo-commit>` and `latest` on `main` (see
[Release-independent images](#release-independent-images)). Its `hvo-image`
output is the merged manifest as
`ghcr.io/<owner>/openstack-hypervisor-operator@<digest>`.

`verify-hvo-image` (push only) pulls that digest and runs `verify_hvo.sh` on a
`[ubuntu-latest, ubuntu-24.04-arm]` runner matrix, so the arm64 binary, which
pull requests never build, runs here first. It holds `contents: read` and
`packages: read` and nothing else.

### build-kna / merge-kna-image / verify-kna-image

kvm-node-agent, compiled by Nix from a pinned commit of its upstream `main`
branch with the patches under `images/kvm-node-agent/patches/`: the root flake
builds `kvm-node-agent-image` from `images/kvm-node-agent/`, the same way as
`openstack-hypervisor-operator-image`. The jobs are copies of the hvo jobs
above with the short name `kna`. The `rev` key of
`images/kvm-node-agent/source.json` is the whole version input. The Nix build
and `hack/nix-lock.sh` read it themselves; every other job calls
`hack/ci-resolve-kna-commit.sh`. The build runs the
patch's two tests as a Nix build user. See
[kvm-node-agent](./container-images.md#kvm-node-agent) for the patch and why
the pin is a `main` commit.

`build-kna` needs `changes`, `nix-flake-check`, `nix-lock-check` and `prepare`,
and carries `if: needs.changes.outputs.build-kna == 'true'`: on a pull request
it runs when `images/kvm-node-agent/**`, `tests/container-images/verify_kna.sh`,
`tests/lib/static_image.sh`, the flake inputs, `nix/lib/**`, the
`build-nix-image` and `setup-nix` actions or the plumbing changed, and on a push
it always runs. It builds the
`linux/amd64` + `linux/arm64` include matrix with `timeout-minutes: 30`,
through `build-nix-image` with the attribute `kvm-node-agent-image` and the
vulnix artifact `vulnix-kna-<pair>`. On pull requests it builds amd64 only,
loads the result locally as `:pr-verify`, and runs the inline Grype scan and
`tests/container-images/verify_kna.sh` against that tag. On push events it
pushes per-platform digests as `digests-kna-<pair>` for the merge job.

`merge-kna-image` (push only) assembles the manifest through
`merge-manifest-and-attest`: SBOM `sbom-kna.cyclonedx.json`, Grype scan
`grype-kna`, attestation, cosign signature. It publishes
`sha-<kna-commit>-<sha>` on every push to `main` or `stable/**`, and adds
`sha-<kna-commit>`, `upstream-<kna-commit>` and `latest` on `main` (see
[Release-independent images](#release-independent-images)). Its `kna-image`
output is the merged manifest as `ghcr.io/<owner>/kvm-node-agent@<digest>`.

`verify-kna-image` (push only) pulls that digest and runs `verify_kna.sh` on a
`[ubuntu-latest, ubuntu-24.04-arm]` runner matrix, with `contents: read` and
`packages: read` only.

### build-service-images

Builds service images per platform on native runners and pushes each single-arch image by
digest. Each image is the flake attribute `<service>-<rel_>-image`, built from
`images/<service>/image.nix` over the lock `nix/locks/<release>/<service>/` by the
`build-nix-image` action ([Nix Image Build](./nix-image-build.md)). The job runs behind
`nix-flake-check` and `nix-lock-check`. The multi-arch manifest list is assembled by
`merge-service-images`.

On pull requests, ARM64 is excluded (only `linux/amd64` is built) and the image is loaded
locally for inline verification instead of being pushed to GHCR.

The figure shows the files a service image build reads, the lock `hack/nix-lock.sh`
writes from them, and the `nix build` of this job.

![The build of a service image on the Nix path, in three frames. Repository: releases/{release}/source-refs.yaml, upper-constraints.txt and extra-packages.yaml, overrides/{release}/constraints.txt, nix/base-requirements.txt, flake.nix with flake.lock, patches/{service}/{release}/*.patch, nix/lib/python-env.nix, images/{service}/image.nix, nix/lib/service-image.nix and nix/lib/fhs-layout.nix. Before the build, hack/nix-lock.sh, run as make nix-lock on a developer machine or a Renovate branch, resolves the source ref to a commit and its NAR hash, merges the constraint overrides on a copy, adds the base pins and runs uv lock. It writes sources.json, pyproject.toml and uv.lock under nix/locks/{release}/{service}/. The lock is committed, and nix-lock-check fails a pull request whose locks lag the pins. The build, nix build .#{service}-{release_}-image in the job build-service-images, fetches the source by commit and hash and applies the patches sources.json lists, builds the venv of uv.lock with uv2nix, and assembles the image through nix/lib/service-image.nix: the venv under /var/lib/openstack, the FHS façade, the packages image.nix names and user 42424. The result is {service}:{release}, which a pull request loads into Docker and a push sends to the registry by digest. A second build for nova reads the nova lock's nova-compute dependency group and the block nova-compute of extra-packages.yaml: images/nova-compute/image.nix builds nova-compute:{release} against the libvirt of nix/pkgs/libvirt.nix.](../../diagrams/ci-service-image-build.svg)

| Property | Value |
| --- | --- |
| `runs-on` | <code v-pre>${{ matrix.runner }}</code> (`ubuntu-latest` for amd64, `ubuntu-24.04-arm` for arm64) |
| `timeout-minutes` | `45` |
| `needs` | `[changes, generate-matrix, nix-flake-check, nix-lock-check, prepare]` |
| Condition | `needs.changes.outputs.has-services == 'true'` |
| Matrix | `service × release × platform × runner` (from `generate-matrix.build-matrix`; ARM64 excluded on PRs) |

**Steps:**

::: v-pre

| # | Step | Action / Command | Details |
| --- | --- | --- | --- |
| 1 | Checkout | `actions/checkout@v7` | Checks out this repository |
| 2 | Derive tags | `.github/actions/derive-service-tags` | Composite action. Computes image name and all tags (see [Tag Schema](#tag-schema)). Passes no `image` input, so the image is named after the service |
| 3 | Prepare platform pair | `.github/actions/platform-pair` | Converts `linux/amd64` → `linux-amd64` for artifact names |
| 4 | Setup Docker registry | `.github/actions/setup-docker-registry` | Buildx + GHCR login (cosign disabled) |
| 5 | Set up Nix | `.github/actions/setup-nix` | Installs Nix and reads the `cobaltcore` cache; pushes only with `CACHIX_AUTH_TOKEN` set; an unreachable cache fails no build |
| 6 | Resolve flake attribute | Shell | Writes `attribute=<service>-<release with _>-image` (`keystone-2026_1-image`), because a flake attribute cannot contain a dot |
| 7 | Build service image | `.github/actions/build-nix-image` | Builds the attribute and scans its store paths with vulnix (artifact `vulnix-<service>-<release>-<platform-pair>`). Non-PR: pushes the stream by digest with skopeo, digest uploaded as artifact `digests-service-<service>-<release>-<platform-pair>`. PR: loads the stream, tags it with the composite tag, Grype scan of the loaded image (`scan-mode: image`; no SARIF upload), then `verify_${{ matrix.service }}.sh` against it |
| 8 | Checkout service source | `.github/actions/checkout-service-source` | Resolves source ref, clones upstream, applies patches and constraint overrides into `src/<service>`. It runs after the build because it edits `releases/<release>/upper-constraints.txt` in place, and the build has to evaluate an unmodified tree |
| 9 | Verify option catalog | `hack/gen-option-catalog.sh --check` | PR only, for `keystone`, `glance`, `neutron`, `cinder` and `nova`: diffs the option catalog generated from the locally loaded image against the committed catalog file, with `src/<service>` as `SOURCE_DIR` |

:::

### merge-service-images

Downloads per-platform digests from `build-service-images`, assembles multi-arch manifest
lists, then runs SBOM generation, vulnerability scanning, attestation, and cosign signing.
Runs only on push events.

| Property | Value |
| --- | --- |
| `runs-on` | `ubuntu-latest` |
| `timeout-minutes` | `45` |
| `needs` | `[build-service-images, generate-matrix, prepare]` |
| `if` | `github.event_name != 'pull_request'` |
| Matrix | `service × release` (from `generate-matrix.matrix`) |
| Permissions | `contents: read`, `packages: write`, `id-token: write`, `attestations: write`, `artifact-metadata: write`, `security-events: write` |

Tag derivation uses the same `.github/actions/derive-service-tags` composite action as
`build-service-images`, ensuring the manifest is assembled under the exact same tags that
were computed during the build.

**Steps:**

::: v-pre

| # | Step | Action / Command | Details |
| --- | --- | --- | --- |
| 1 | Checkout | `actions/checkout@v7` | Checks out the repository |
| 2 | Setup Docker registry | `.github/actions/setup-docker-registry` | Buildx + GHCR login + cosign |
| 3 | Derive tags | `.github/actions/derive-service-tags` | Composite action. Computes image name and all tags; no `image` input |
| 4 | Build service image tags | Shell | Assembles composite + SHA tags (all branches), version + release tags (main only) |
| 5 | Merge and attest service image | `.github/actions/merge-manifest-and-attest` | Downloads all `digests-service-<service>-<release>-*` artifacts (`actions/download-artifact@v8`), assembles per-platform digests into multi-arch manifest with `hack/ci-merge-manifest.sh`, then runs `supply-chain-attest`: SBOM + Grype scan + SARIF upload + attestation + provenance + cosign sign |

:::

This job does not declare outputs. The `verify-service-images` job derives its own image
refs independently via its own matrix strategy.

### test-service-images

Runs the upstream unit tests of each service and release as Nix builds
([Unit tests](./nix-image-build.md#unit-tests)). The run derivation
`<service>-<rel_>-test-run` executes the suite over the patched source with the venv
of the lock's test group, MySQL 8.4 and PostgreSQL 18 running on 127.0.0.1 inside the
build sandbox, and succeeds whenever the suite ran; the check
`<service>-<rel_>-tests` then fails the job unless the run's exit status is 0. The
split lets a failed run still upload its subunit stream. An optional per-service exclude-list
(`releases/<release>/test-excludes/<service>.txt`) skips known-failing tests via
`stestr run --exclude-list`.

This job runs in parallel with `build-service-images`, behind `nix-lock-check`. The
`verify-service-images` job gates on both.

| Property | Value |
| --- | --- |
| `runs-on` | `ubuntu-latest` |
| `timeout-minutes` | `90` |
| `needs` | `[changes, generate-matrix, nix-lock-check]` |
| Condition | `needs.changes.outputs.has-services == 'true'` |
| Permissions | `contents: read`, `packages: read` |
| Matrix | `service × release` (from `generate-matrix.matrix`) |

**Steps:**

::: v-pre

| # | Step | Action / Command | Details |
| --- | --- | --- | --- |
| 1 | Checkout | `actions/checkout@v7` | Checks out the repository |
| 2 | Set up Nix | `.github/actions/setup-nix` | As in `build-service-images`, plus `push-filter: '-test-run$'`, so a failed run never reaches the cache |
| 3 | Run tests | Shell | `nix build ".#<service>-<rel_>-test-run" --out-link result-test-run`, copies its files to `results/`, prints `results/summary.txt`, then `nix build ".#checks.x86_64-linux.<service>-<rel_>-tests"` |
| 4 | Upload test results | `actions/upload-artifact@v7` | Always runs. Uploads `results/testresults.subunit` as `test-results-<service>-<release>` with 30-day retention |

:::

**Test exclude-list:**

If `releases/<release>/test-excludes/<service>.txt` exists, stestr uses it as
`--exclude-list` to skip tests matching the regex patterns in the file. The file follows
stestr exclude-list format: blank lines are ignored, `#` lines are comments, all other
lines are regex patterns matching test IDs to skip. See
`releases/2026.1/test-excludes/keystone.txt` for an example. A test that fails only in
the build sandbox goes into the sandbox exclude list of `nix/lib/service-tests.nix`
instead, with its error.

**Test Coverage:**

The `verify_build_images_workflow.sh` script validates test-service-images job structure
and steps:

| Test | Validates |
| --- | --- |
| `test_test_service_images_job_structure` | `runs-on: ubuntu-latest`, `timeout-minutes: 90`, `contents: read`, `packages: read`, no `id-token`, `attestations`, or `security-events` |
| `test_test_service_images_has_matrix` | Matrix is a `fromJson` expression over a `generate-matrix` output, with `fail-fast: false` |
| `test_test_service_images_depends_on_base` | `needs` array contains `changes`, `generate-matrix` and `nix-lock-check`, and neither `merge-base-images` nor `verify-base-images` |
| `test_service_unit_tests_run_as_nix_checks` | No `services:` block, a `-test-run` build and a `checks.x86_64-linux.` build in `Run tests`, and `push-filter: '-test-run$'` on the `setup-nix` step |
| `test_test_service_images_subunit_output` | `Run tests` builds the `-test-run` attribute and copies `result-test-run/*` to `results/`, and the upload path is `results/testresults.subunit` |
| `test_test_service_images_upload_artifacts` | `Upload test results` step for subunit output with `if: always()` and 30-day retention |
| `test_test_service_images_artifact_name` | Artifact name includes `matrix.service` and `matrix.release` for disambiguation |
| `test_test_service_images_env_vars` | `Run tests` gets `MATRIX_SERVICE` and `MATRIX_RELEASE` from `env:` and interpolates no <code v-pre>${{ }}</code> expression into its run block |
| `test_verify_service_images_depends_on_service_images` | `verify-service-images` `needs` includes `merge-service-images`, `test-service-images` and `generate-matrix` |
| `test_timeout_minutes_on_all_jobs` | All jobs including `test-service-images` have `timeout-minutes` set |
| `test_runs_on_ubuntu_latest` | `test-service-images`, `merge-service-images`, `verify-service-images` and the six release-independent merge jobs use `runs-on: ubuntu-latest`; `build-service-images`, the six release-independent build jobs and `verify-ovn-image`, `verify-hvo-image`, `verify-kna-image` use `matrix.runner` |
| `test_matrix_jobs_fail_fast_false` | `build-service-images`, `test-service-images` and `verify-service-images` have `fail-fast: false` |

The `verify_release_config.sh` script validates test-excludes file structure:

| Test | Validates |
| --- | --- |
| `test_test_excludes_file_format` | `test-excludes/keystone.txt` contains valid stestr exclude-list format |
| `test_test_excludes_directory_structure` | All files in `test-excludes/` are `.txt` and named after services in `source-refs.yaml` |
| `test_test_excludes_files_match_services` | Each filename (sans `.txt`) corresponds to a key in `source-refs.yaml` |

### verify-service-images

::: v-pre

Validates that built service images are functional by running `verify_${{ matrix.service }}.sh`. This job replaces the former `smoke-test` job and runs only on push events
(when images are in GHCR). It uses its own matrix strategy matching
`build-service-images` to test every service independently.

:::

On PRs, the equivalent verification runs as an inline step within `build-service-images`
(step 7 above) because `--load` makes the image available only on the same runner.

| Property | Value |
| --- | --- |
| `runs-on` | `ubuntu-latest` |
| `timeout-minutes` | `15` |
| `needs` | `[merge-service-images, test-service-images, generate-matrix]` |
| `if` | `github.event_name != 'pull_request'` |
| Permissions | `contents: read`, `packages: read` |
| Matrix | `service × release` (from `generate-matrix.matrix`) |

**Steps:**

::: v-pre

| # | Step | Action / Command | Details |
| --- | --- | --- | --- |
| 1 | Checkout | `actions/checkout@v7` | Checks out the repository (needed for test scripts, `source-refs.yaml`, and patch counting) |
| 2 | Setup Docker registry | `.github/actions/setup-docker-registry` | GHCR login (cosign disabled); replaces inline login step |
| 3 | Derive tags | `.github/actions/derive-service-tags` | Composite action. Reconstructs tags using the same logic as `build-service-images` and `merge-service-images`; no `image` input |
| 4 | Pull and verify service image | Shell | `docker pull <image-ref>` then runs `verify_${{ matrix.service }}.sh` with the pulled image ref |
| 5 | Verify option catalog | `hack/gen-option-catalog.sh --check` | For `keystone`, `glance`, `neutron`, `cinder` and `nova`: diffs the option catalog generated from the pulled image against the committed catalog file |

:::

**Test script executed:**

| Script | Validates |
| --- | --- |
| `tests/container-images/verify_<service>.sh` | Service-specific checks (e.g. `keystone-manage --version` exits 0), runs as `openstack` user, no build tools (gcc, cc, uv and python3-config not on PATH, no compiler, binutils or uv package in /nix/store), runtime packages present |

The job fails the workflow if the verify script exits non-zero. Tag derivation uses the
`.github/actions/derive-service-tags` composite action, which is the single source of
truth shared by `build-service-images`, `merge-service-images`, and `verify-service-images`.

### build-nova-compute-image / merge-nova-compute-image / verify-nova-compute-image

These jobs build the compute image of `images/nova-compute/` (see
[nova-compute](./container-images.md#nova-compute)) from nova's lock with its
`nova-compute` dependency group. The image gets jobs of its own
because a failed matrix entry of `build-service-images` on a main push skips
`merge-service-images` for every service. Extra steps in the nova entry would add
a compiled binding and the host tools to that blast radius. A failed compute
build skips `merge-nova-compute-image` for every release and leaves every other
merge job alone.

`build-nova-compute-image` gates on `nova-compute-releases != '[]'` from
[generate-matrix](#hack-ci-generate-build-matrix-sh), and the merge and verify
jobs follow its skip, so a pull request that leaves nova alone skips them. A
compute-only change still reaches them:
`images/nova-compute/**` and `tests/container-images/verify_nova_compute.sh`
sit in the `svc_nova` paths filter, so the `changes` job resolves `nova`. Such
a pull request also rebuilds the nova image and runs nova's unit tests. That
cost is accepted, because the compute image has to rebuild whenever nova's
lock moves, and one filter keeps that coupling in one place. A change to
`nix/pkgs/` reaches it through the `base` filter, which sets every service.

`build-nova-compute-image` needs `changes`, `generate-matrix`,
`nix-flake-check`, `nix-lock-check` and `prepare`, has 60 minutes for the first
build per `nixpkgs-images` revision, which compiles libvirt, and runs over
`nova-compute-build-matrix` (`release × platform`, amd64 only on pull requests).
Its steps mirror `build-service-images`:

- `derive-service-tags` with `service: nova` and `image: nova-compute`: nova's
  version and patch count under the `nova-compute` name
- `setup-nix`, and a `Resolve flake attribute` step that writes
  `attribute=nova-compute-<release with _>-image`
- `build-nix-image` with that attribute, the image name of the tags step, the
  composite tag as `verify-image-ref` and the vulnix report
  `vulnix-nova-compute-<release>-<platform-pair>`

On pull requests the image is loaded locally, scanned by Grype
(`grype-nova-compute-<platform-pair>`) and checked by
`tests/container-images/verify_nova_compute.sh`. On push it is pushed by
digest and uploaded as `digests-nova-compute-<release>-<platform-pair>`.

`merge-nova-compute-image` (push only) copies `merge-service-images` over a
`release` matrix from `nova-compute-releases`. It merges
`digests-nova-compute-<release>-*`, a pattern nova's own
`digests-service-nova-<release>-*` cannot match, publishes the composite and
SHA tags from every branch and the version and release tags from `main`, and
writes `sbom-nova-compute-<release>.cyclonedx.json` with the Grype category
`grype-nova-compute-<release>`. Like Tempest's, the category carries the
release: both releases merge for the same commit, and under one category the
later SARIF upload would replace the earlier one and close its alerts as fixed.

`verify-nova-compute-image` (push only) pulls the composite tag and runs
`verify_nova_compute.sh` on a `release × [ubuntu-latest, ubuntu-24.04-arm]`
matrix. It and `build-nova-compute-image` pass the release to the script as
`NOVA_COMPUTE_RELEASE`. Pull requests build amd64 only, so this job is the
first place the arm64 build of the binding and the host tools runs. It holds
`contents: read` and `packages: read` only. There is no option-catalog step:
the compute image adds no oslo.config namespace to the nova catalog.

## Tag Schema

Each service image build produces two to four tags on push events:

| Tag | Format | Example | Branches |
| --- | --- | --- | --- |
| Composite | `<version>-p<N>-<branch>-<sha>` | `keystone:29.0.0-p0-main-a1b2c3d` | all |
| Version | `<version>` | `keystone:29.0.0` | `main` only |
| Release | `<release>` | `keystone:2026.1` | `main` only |
| SHA | `<sha>` | `keystone:a1b2c3d` | all |

The version-only and release tags are restricted to the `main` branch to prevent silent
overwrites when multiple branches build the same upstream version. The composite tag
already encodes the branch, so `stable/**` builds remain uniquely identifiable.

**Tag components:**

| Component | Source | Description |
| --- | --- | --- |
| `<version>` | `releases/<release>/source-refs.yaml` | Upstream OpenStack version tag (e.g., `29.0.0`) |
| `p<N>` | Count of `.patch` files in `patches/<service>/<release>/` | Patch count; defaults to `p0` when the directory is absent |
| `<branch>` | `GITHUB_REF_NAME` with `/` replaced by `-` | Branch name, sanitized for Docker tag compatibility (e.g., `stable/2026.1` becomes `stable-2026.1`) |
| `<sha>` | First 7 characters of `GITHUB_SHA` | Short commit SHA |

The composite tag uniquely identifies the exact build: upstream version, patch level,
branch, and commit. The version and SHA tags provide convenient shortcuts for deployment
systems.

`nova-compute` carries nova's `<version>` and patch count under its own name:
the compute jobs call `derive-service-tags` with `service: nova` and
`image: nova-compute`, so `nova-compute:2026.1` and `nova:2026.1` are cut from
the same nova (for example `nova-compute:33.0.0-p0-main-a1b2c3d`).

### Release-independent images

`keystone-federation-proxy`, `backup-shifter`, `libvirt`, `ovn`,
`openstack-hypervisor-operator` and `kvm-node-agent` have no OpenStack version
to tag with:

| Image | Tags | Branches |
| --- | --- | --- |
| `keystone-federation-proxy` | `latest`, `<sha>` | all |
| `backup-shifter` | `latest`, `<sha>` | all |
| `libvirt` | `<sha>` | all |
| `libvirt` | `latest`, `<libvirt-version>-r<N>` | `main` only |
| `ovn` | `<ovn-version>-<sha>` | all |
| `ovn` | `<ovn-version>`, `latest` | `main` only |
| `openstack-hypervisor-operator` | `sha-<hvo-commit>-<sha>` | all |
| `openstack-hypervisor-operator` | `sha-<hvo-commit>`, `upstream-<hvo-commit>`, `latest` | `main` only |
| `kvm-node-agent` | `sha-<kna-commit>-<sha>` | all |
| `kvm-node-agent` | `sha-<kna-commit>`, `upstream-<kna-commit>`, `latest` | `main` only |

`<sha>` is the full 40-character `github.sha` here. Restricting `ovn:<ovn-version>` and `ovn:latest` to `main` has the same
reason as the service version tag above: when two branches build the same
upstream version, a shared tag would be silently overwritten. The same holds
for the three `main`-only tags of `openstack-hypervisor-operator` and
`kvm-node-agent`, where `<hvo-commit>` and `<kna-commit>` are the full upstream
commits their `source.json` pins. `sha-<hvo-commit>` and `sha-<kna-commit>` are the
tags the upstream chart of that commit renders from its `appVersion`.

`libvirt:<libvirt-version>-r<N>`, such as `libvirt:12.2.0-r57`, names what a build
contains. `<libvirt-version>` is the third field of the first line `libvirtd --version`
prints in the image's `linux/amd64` variant. `<N>` is
`git rev-list --count HEAD -- images/libvirt nix/pkgs/libvirt.nix nix/lib/image-defaults.nix flake.lock`,
the number of commits on `main` that touched the image's directory, its libvirt
derivation, the creation date and labels every Nix image shares, or the flake's
lock file. The tag is created once and never moved. The first `main` build with a
new value gets it; every later build with the same value finds it in the registry,
keeps `latest` and `<sha>` only, and is pruned once superseded. A new tag appears
when a commit touches one of the four paths: a change to the image, to the libvirt
override or to the shared image defaults, or a `nixpkgs-images` re-lock, which can
rebuild QEMU, Ceph or glibc while libvirt keeps its version. A version that is not
`<major>.<minor>[.<patch>]`, such as `12.2.0-rc1`, fails the step, because the
prune's keep pattern and Renovate's `allowedVersions` assume that shape. The
Docker-built image before #1407 carried `<libvirt-package-version>-r<N>`, such as
`10.0.0-2ubuntu8.19-r2`; the prune keeps those as the lab's rollback target. The lab
manifests under `deploy/lab/metal-stack/` pin the current tag by digest (see
[Lab hypervisors](../infrastructure/infrastructure-manifests.md#lab-hypervisors)).

## Retention

The nightly `cleanup-images.yaml` workflow decides retention from each package
version's whole tag set, not from a "keep the newest N" counter. A version
survives while it carries a **keeper** tag:

| Kept | Example | Why |
| --- | --- | --- |
| `latest` | `backup-shifter:latest` | Current build of a release-independent image |
| Version | `keystone:29.0.0` | Names an upstream release |
| Release | `keystone:2026.1` | Names an OpenStack release |
| Semver prerelease | `keystone-operator:1.2.0-rc1` | Tagged operator build |
| Operator chart version | `keystone-operator:0.14.0` | `merge-operator-images` tags each operator image it publishes from `main` with its `Chart.yaml` version; the [operator upgrade suites](../testing/operator-upgrade-e2e-tests.md) pin their baseline to it |
| Pinned upstream commit | `openstack-hypervisor-operator:upstream-<commit>`, `kvm-node-agent:upstream-<commit>` | A chart of that commit names the image |
| Libvirt keeper | `libvirt:12.2.0-r57`, and `libvirt:10.0.0-2ubuntu8.19-r2` of the Docker-built image | The lab manifests pin that build by digest; the Docker-built keepers are the lab's rollback target |

Everything else is a build artifact with a successor and is deleted once it is
older than 24 hours: composite tags, SHA tags in all four shapes
(`<sha>`, `<sha40>`, `sha-<sha40>`, `<release>-<sha40>`), run-scoped
`e2e-<run_id>-*` tags, `dev`, and untagged leftovers.

The rule works per manifest, so the *current* main build is untouched: its
composite and SHA tags sit on the same manifest as `29.0.0` and `2026.1`, and
that release tag keeps the whole thing. Only once a later build moves `29.0.0`
and `2026.1` onto a new manifest does the old one (now holding nothing but its
composite and SHA tags) become deletable.

Two things survive without a tag of their own. A multi-arch image is an OCI
index whose per-platform and buildx attestation manifests are separate untagged
package versions; those are kept for as long as an index that references them is
kept. Cosign and SBOM artifacts attach as a `sha256-<digest>` tag naming their
subject and follow that subject.

`hack/ghcr-prune-stale-versions.py` implements this; run it with `--dry-run` to
see a plan without deleting anything.

## PR vs Push Behavior

The figure shows both paths for a service image: the three jobs every run
executes, where a pull request ends, and what a push merges, attests and signs.
The service images build from Nix through `build-nix-image` and run their unit
tests as Nix builds. The table lists the differences aspect by aspect.

![The image build workflow on its two paths. On every run, nix-flake-check runs nix flake check, the Nix lint and the checks of the flake, nix-lock-check runs hack/nix-lock.sh --check to compare the locks and the image pins with their inputs, and generate-matrix works out the services, releases and platforms. build-service-images waits for all three, test-service-images for nix-lock-check and generate-matrix. On a pull request, build-service-images builds each service image with nix build for linux/amd64 only, scans its store paths with vulnix and uploads the report as an artifact, loads the image into the local Docker daemon, scans it with Grype without a report upload, runs tests/container-images/verify_{service}.sh and checks the option catalog of the services that have one. test-service-images runs the upstream unit tests as the Nix build .#{service}-{release_}-test-run and its check. The run ends there, and no service image is pushed. On a push to main or stable/**, or on a manual run, build-service-images builds for amd64 and arm64, each on its own runner, scans the store paths with vulnix, pushes by digest with skopeo and exports the digest as one artifact per platform. merge-service-images creates and pushes the manifest with the tags of the Tag Schema, generates a CycloneDX SBOM, scans the SBOM with Grype and uploads the report to the Security tab, attests the SBOM and the build provenance, and signs the image with cosign. verify-service-images pulls the image from ghcr.io and runs the verify script and the option catalog check once merge-service-images and test-service-images have passed. ghcr.io holds {service} alone, pushed on a push. No base image exists: a build reads nothing from ghcr.io, and the Cachix cache cobaltcore serves store paths when it is reachable. A manual run, workflow_dispatch, takes the push path; the workflow has no tag trigger.](../../diagrams/ci-image-publish.svg)

The workflow behaves differently depending on the trigger event:

| Aspect | Pull Request | Push (main / stable/**) |
| --- | --- | --- |
| Service matrix | The services whose sources changed, every release each | Every service |
| Tempest images | On `images/tempest/**`, its locks, its verify script, a base change, or a plumbing change | Always |
| OVN, federation proxy, backup shifter, libvirt, openstack-hypervisor-operator, kvm-node-agent | On their own sources, or a plumbing change | Always |
| Service image platforms | `linux/amd64` only (ARM64 excluded) | `linux/amd64,linux/arm64` |
| Service image push | No (`docker load` on amd64 runner) | Yes (by digest with skopeo, tags assigned by `merge-service-images`) |
| Service image tags | Computed but not published | Published to GHCR |
| SBOM generation | Skipped | CycloneDX JSON for every merged manifest |
| Vulnerability scanning | Image-based scan on PR (`image:` input) in the build jobs | SBOM-based scan in the merge jobs (`sbom:` input) |
| SBOM attestation | Skipped | Sigstore-signed, pushed to GHCR |
| Cosign signing | Skipped | Keyless signature for every merged manifest |
| OIDC token request | None | Requested in merge jobs for Sigstore signing |
| Service image verification | Inline step in `build-service-images` | Separate `verify-service-images` job |
| Verification image source | Locally loaded image (same amd64 runner) | Pulled from GHCR |
| ovn image verification | Inline step in `build-ovn` (`verify_ovn.sh` on `:pr-verify`) | Separate `verify-ovn-image` job (pulled by digest from GHCR) |
| nova-compute image verification | Inline step in `build-nova-compute-image` (`verify_nova_compute.sh`, amd64) | Separate `verify-nova-compute-image` job on `ubuntu-latest` and `ubuntu-24.04-arm` |

On a pull request every image is scanned twice: Grype scans the loaded image, and vulnix scans the store
paths of the image
([The build-nix-image action](./nix-image-build.md#the-build-nix-image-action)).

**Why PRs use single-arch for service images:** `docker load` reads a single-platform
image, and a multi-platform image cannot be loaded into the local Docker daemon. Since the inline verification step needs the image locally,
PRs build only `linux/amd64`. The arm64 images are built and verified on push.

## GHA Caching

No job of this workflow uses a `type=gha` cache. Every image is built by Nix, and the
build jobs and `test-service-images` read the Cachix cache `cobaltcore`
([Binary cache](./nix-image-build.md#binary-cache)).

## Action Pinning

All actions are pinned to full commit SHAs with version comments, matching the
convention in `ci.yaml`:

```yaml
uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1  # v7
uses: docker/setup-buildx-action@f87e5991a6d7451dcb8d9637bfbc97413f497069  # v4
uses: anchore/sbom-action@e22c389904149dbc22b58101806040fa8d37a610  # v0
uses: anchore/scan-action@e1165082ffb1fe366ebaf02d8526e7c4989ea9d2  # v7
uses: actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6  # v4
uses: github/codeql-action/upload-sarif@2892aa5e19bbd11bc0cff5427e3b750a04d9e3c2  # v4
uses: sigstore/cosign-installer@faadad0cce49287aee09b3a48701e75088a2c6ad  # v4
```

(GHCR authentication runs `docker login` via the `registry-login` composite action,
with no third-party login action; the pinned SHAs above are kept current by
Renovate, so treat the workflow files as the authoritative source.)

This prevents supply-chain attacks via tag mutation while remaining auditable through
version comments.

## SBOM Generation and Attestation

Every container image pushed to GHCR on non-PR events receives a CycloneDX SBOM and a
Sigstore-signed attestation. This enables consumers to audit image contents,
check for known vulnerabilities, and verify that images have not been tampered with since
build time.

> **Consolidation note.** SBOM generation, attestation, vulnerability scanning,
> build provenance, and cosign signing are handled by a single parameterised
> composite action (`.github/actions/supply-chain-attest`). See
> [supply-chain-attest](#supply-chain-attest) for the composite action interface.

### How It Works

After per-platform images are pushed by digest and the multi-arch manifest list is
assembled by the merge job, the `supply-chain-attest` composite action runs in the
merge job and performs:

1. **SBOM generation** (`anchore/sbom-action`) — Syft scans the merged manifest
   (referenced by digest) and produces a CycloneDX JSON file covering the Nix store paths
   of the image (`pkg:nix` components) and its Python packages (`pkg:pypi`, from the
   dist-info directories).

2. **SBOM attestation** (`actions/attest`) — The CycloneDX file is signed via
   Sigstore keyless OIDC (using the GitHub Actions workflow identity) and pushed to GHCR
   as an OCI referrer artifact alongside the image. No signing keys are managed; the OIDC
   token binds the attestation to the specific workflow run.

This pattern is applied to every published image via the `supply-chain-attest` composite
action; the table lists four of them:

::: v-pre

| Image | SBOM output file | Job |
| --- | --- | --- |
| `libvirt` | `sbom-libvirt.cyclonedx.json` | `merge-libvirt-image` |
| `nova-compute` | `sbom-nova-compute-${{ matrix.release }}.cyclonedx.json` | `merge-nova-compute-image` |
| `tempest` | `sbom-tempest-${{ matrix.release }}.cyclonedx.json` | `merge-tempest-image` |
| Service (e.g., `keystone`) | `sbom-${{ matrix.service }}.cyclonedx.json` | `merge-service-images` |

:::

### PR Behavior

The `supply-chain-attest` composite action uses its `scan-mode` input to control PR vs
push behavior. On pull requests the merge jobs do not run, and the build jobs pass
`scan-mode: image`, which runs only the Grype vulnerability scan (no SBOM generation,
no attestation, no signing). On push events, merge jobs use the default
`scan-mode: sbom` for the full pipeline.

On pull requests:

- No SBOMs are generated or attested — `scan-mode: image` skips all SBOM/attestation steps.
- No OIDC token requests occur — SBOM/attestation steps are skipped so `id-token: write` is not exercised.
- No attestations are created for ephemeral PR builds.
- PR CI time is not increased by SBOM/attestation steps.

### Required Permissions

SBOM attestation requires three additional job-level permissions beyond the existing
`contents: read` and `packages: write`:

| Permission | Purpose |
| --- | --- |
| `id-token: write` | Allows the GitHub Actions runner to request a short-lived Sigstore OIDC token for keyless signing |
| `attestations: write` | Grants access to the GitHub Attestations API for storing signed attestations |
| `artifact-metadata: write` | Allows `actions/attest` to write its storage record |

These permissions are granted to every merge job, to `build-service-images` and to
`build-nova-compute-image`. Verification jobs (`verify-service-images`,
`verify-nova-compute-image` and the other `verify-*` jobs) do not receive these
permissions.

### Verifying Attestations

To verify that an image has a valid Sigstore-signed attestation:

```bash
gh attestation verify oci://ghcr.io/<owner>/<service>:<tag> --owner <owner>
```

To extract the SBOM predicate from the attestation, first inspect the raw
output to find the exact `predicateType` URI used in your environment:

```bash
gh attestation verify oci://ghcr.io/<owner>/<service>:<tag> \
  --owner <owner> \
  --format json | jq '.[].verificationResult.statement.predicateType'
```

Then filter for that predicate type (the URI below may differ by action version):

```bash
gh attestation verify oci://ghcr.io/<owner>/<service>:<tag> \
  --owner <owner> \
  --format json | jq -r '
    [ .[] | select(.verificationResult.statement.predicateType | test("cyclonedx")) ]
    | if length == 0 then error("no CycloneDX attestation found") else .[0].verificationResult.statement.predicate end
  '
```

### Test Coverage

The `verify_build_images_workflow.sh` script validates SBOM/attestation configuration:

| Test | Validates |
| --- | --- |
| `test_sbom_permissions_on_build_service_images` | `id-token: write` and `attestations: write` on `merge-service-images` |
| `test_verify_jobs_no_sbom_permissions` | `verify-service-images`, `test-service-images`, `verify-ovn-image`, `verify-hvo-image`, `verify-kna-image` and `verify-nova-compute-image` do **not** have `id-token` or `attestations` permissions |
| `test_sbom_generation_steps_exist` | `supply-chain-attest` has one `anchore/sbom-action` step; `merge-libvirt-image` and `merge-service-images` call `merge-manifest-and-attest` once each |
| `test_sbom_format_cyclonedx_json` | The SBOM step specifies `format: cyclonedx-json` |
| `test_sbom_generation_references_digest` | The SBOM step builds its image ref from `inputs.image-name` and `inputs.image-digest`; `merge-manifest-and-attest` forwards `steps.merge.outputs.digest`; the libvirt and service merge calls pass their image |
| `test_sbom_attestation_steps_exist` | `supply-chain-attest` has one `actions/attest` step |
| `test_sbom_attestation_push_to_registry` | The `actions/attest` step has `push-to-registry: true` |
| `test_sbom_steps_pr_skip_guard` | The SBOM and attestation steps have the `scan-mode == 'sbom'` guard; `merge-service-images` and `merge-libvirt-image` have the job-level `github.event_name != 'pull_request'` guard |

## Cosign Image Signing

Every container image pushed to GHCR on non-PR events is signed with cosign keyless
signing. This provides an independent signature layer alongside the SBOM
attestation, enabling consumers to verify image provenance using standard
Sigstore tooling.

### How It Works

Cosign is installed by the `setup-docker-registry` composite action when
`install-cosign: 'true'` (the default, used by merge jobs). The `supply-chain-attest`
composite action then runs `cosign sign --yes` as the final step of the supply chain
pipeline:

1. **cosign-installer** (`sigstore/cosign-installer@v4`) — Installed by
   `setup-docker-registry` in merge jobs.

2. **cosign sign** — Executed by `supply-chain-attest` in `sbom` mode. Signs the image
   by digest using Sigstore keyless OIDC. The `--yes` flag confirms non-interactive mode.
   No signing keys are managed; the GitHub Actions OIDC token binds the signature to the
   specific workflow run.

This pattern is applied to every published image via the `supply-chain-attest` composite
action; the table lists four of them:

| Image | Job | Digest source |
| --- | --- | --- |
| `libvirt` | `merge-libvirt-image` | `steps.merge-libvirt.outputs.digest` |
| `nova-compute` | `merge-nova-compute-image` | `steps.merge-nova-compute.outputs.digest` |
| `tempest` | `merge-tempest-image` | `steps.merge-tempest.outputs.digest` |
| Service (e.g., `keystone`) | `merge-service-images` | `steps.merge-service.outputs.digest` |

### PR Behavior

The `supply-chain-attest` composite action skips cosign signing in `image` mode. On
pull requests:

- No images are signed — `scan-mode: image` skips all signing steps.
- No OIDC token requests occur for signing.
- The `id-token: write` permission (shared with SBOM attestation) is not exercised.

### Required Permissions

Cosign keyless signing reuses the same `id-token: write` permission required by SBOM
attestation. No additional permissions are needed.

### Verifying Signatures

To verify that an image has a valid cosign signature:

```bash
cosign verify \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp "https://github.com/<owner>/<repo>/.github/workflows/build-images.yaml@refs/.*" \
  ghcr.io/<owner>/<service>@sha256:<digest>
```

### Test Coverage

The `verify_build_images_workflow.sh` script validates cosign signing configuration:

| Test | Validates |
| --- | --- |
| `test_cosign_installer_in_build_service_images` | `setup-docker-registry` has one `sigstore/cosign-installer` step, and `merge-service-images` leaves `install-cosign` at its default |
| `test_cosign_sign_steps_count` | `supply-chain-attest` has one `cosign sign` step |
| `test_cosign_sign_steps_pr_guard` | The sign step has the `scan-mode == 'sbom'` guard; `merge-service-images` has the job-level `github.event_name != 'pull_request'` guard |
| `test_cosign_sign_steps_reference_digest` | The sign step takes `IMAGE_NAME` and `IMAGE_DIGEST` from the action inputs and signs `${IMAGE_NAME}@${IMAGE_DIGEST}` |
| `test_cosign_sign_uses_yes_flag` | The sign step uses the `--yes` flag |

## Vulnerability Scanning

Every container image is scanned for known CVEs using Grype (via `anchore/scan-action`)
on every push and pull request. Unlike SBOM generation, attestation, and cosign
signing (which are skipped on PRs), vulnerability scanning runs on **both** event types.
Only push events upload the results to the GitHub Security tab; a pull request runs the
scan without uploading.

> **Consolidation note.** Vulnerability scanning is part of the
> `supply-chain-attest` composite action. In `sbom` mode (push), Grype scans the SBOM
> and uploads SARIF to the GitHub Security tab. In `image` mode (PR), Grype scans the
> image directly and uploads nothing.

### How It Works

The `supply-chain-attest` composite action includes two vulnerability scanning steps:

1. **Grype scan** (`anchore/scan-action`) — Scans the image for known CVEs. On push
   events, Grype consumes the CycloneDX SBOM file (faster, offline-capable). On PR
   events, Grype scans the image directly (since SBOM generation is skipped on PRs).
   The scan uses `severity-cutoff: high` with `fail-build: false`. All CVEs at or above
   high severity are reported in SARIF but do not currently fail the build. Build failure
   on high/critical CVEs will be activated later.

2. **SARIF upload** (`github/codeql-action/upload-sarif`) — Uploads Grype results to the
   GitHub Security tab in SARIF format on push events. Each upload uses a unique
   `category` value to distinguish findings per image. The upload step runs with
   `if: always()`, a `github.event_name != 'pull_request'` guard, and a guard that checks
   for non-empty SARIF output, ensuring the upload is skipped cleanly if the scan step
   crashes without producing output.

   The pull-request guard exists because code scanning compares the categories a pull
   request uploads with those present on the default branch. Main receives one upload per
   image from the merge jobs (`grype-<image>`), while a pull request scans per platform
   and only the images it changed (`grype-<image>-<platform>`), so the two sets never
   matched and every image-building pull request carried a neutral `grype` check reading
   "N configurations not found". Without a PR upload GitHub creates no such check.

This pattern is applied to every published image via the `supply-chain-attest` composite
action; the table lists four of them:

::: v-pre

| Image | SARIF category | Push job (uploads) | PR job (scan only) |
| --- | --- | --- | --- |
| `libvirt` | `grype-libvirt` | `merge-libvirt-image` | `build-libvirt` |
| `nova-compute` | `grype-nova-compute-${{ matrix.release }}` | `merge-nova-compute-image` | `build-nova-compute-image` |
| `tempest` | `grype-tempest-${{ matrix.release }}` | `merge-tempest-image` | `build-tempest` |
| Service (e.g., `keystone`) | <code v-pre>grype-${{ matrix.service }}</code> | `merge-service-images` | `build-service-images` |

:::

### Scan Input: PR vs Push

The `supply-chain-attest` composite action uses its `scan-mode` input to select the
scan strategy. Internally, the action has two mutually exclusive Grype scan steps — one
for `sbom` mode and one for `image` mode — because `anchore/scan-action` documents
`sbom` and `image` as mutually exclusive inputs:

| `scan-mode` | Grype input | Source |
| --- | --- | --- |
| `sbom` (push) | `sbom:` | CycloneDX SBOM file generated in a prior step |
| `image` (PR) | `image:` | Image reference passed via `image-ref-for-scan` input |

The SARIF upload step uses a fallback expression to reference whichever scan step
produced output, since exactly one runs per invocation.

On PRs, the image is referenced by the local tag the build job gave the loaded image:
the composite tag from `steps.tags.outputs.composite` for a service image,
`<image>:pr-verify` for a release-independent one.

### Severity Threshold

All Grype scan steps use `severity-cutoff: high` with `fail-build: false`:

- **Critical** and **High** severity CVEs are reported in SARIF but do not currently
  fail the build (build failure will be activated later)
- **Medium** and **Low** severity CVEs are reported in SARIF but do not block the build
- All findings from push events are visible in the GitHub Security tab regardless of
  severity; pull-request scans upload nothing

### CVE Suppression

A `.grype.yaml` configuration file at the repository root allows suppression of
known-accepted CVEs. Grype automatically reads this file from the working
directory (default behavior — no explicit configuration needed).

```yaml
# .grype.yaml — add entries to suppress false positives
ignore:
  - id: CVE-YYYY-NNNNN
    reason: "<justification>"
    fix-state: "not-fixed"
```

The ignore list is initially empty. To add a CVE suppression, append an entry with the
CVE identifier, a justification explaining why the CVE is acceptable, and optionally a
`fix-state` field (`fixed`, `not-fixed`, `wont-fix`, or `unknown`). This prevents
false-positive build failures on unfixable vulnerabilities without disabling
scanning entirely.

### SARIF Integration

Grype scan results are uploaded to the GitHub Security tab via
`github/codeql-action/upload-sarif`:

- Each image has a unique SARIF `category` value (e.g., `grype-libvirt`,
  `grype-<service>`) for per-image categorization in the Security dashboard
- Upload steps use `if: always()` with a guard for non-empty SARIF output, ensuring
  clean skip when a scan step crashes without producing output
- Uploads run on push events only (`github.event_name != 'pull_request'`); the SARIF
  upload step description above explains why pull requests upload nothing
- Results appear in the repository's **Security > Code scanning alerts** tab

### Required Permissions

SARIF upload requires `security-events: write` permission on the merge jobs
(`merge-tempest-image`, `merge-service-images` and the other `merge-*-image` jobs). The
per-platform build jobs scan on pull requests without uploading and hold no
`security-events` permission, and neither do the verification jobs
(`verify-service-images` and the other `verify-*` jobs): least privilege.

### Test Coverage

The `verify_build_images_workflow.sh` script validates vulnerability scanning
configuration:

| Test | Validates |
| --- | --- |
| `test_grype_scan_steps_in_supply_chain_attest` | 2 `anchore/scan-action` steps exist in `supply-chain-attest` (SBOM + image) |
| `test_grype_scan_step_in_build_service_images` | `build-nix-image` calls `supply-chain-attest` once, and the `build-service` step passes a `grype-category` with `matrix.service` |
| `test_grype_scan_action_sha_pinned` | `anchore/scan-action` is SHA-pinned with `# v7` version comment |
| `test_grype_scan_steps_cover_both_contexts` | The `grype-sbom` step has the `scan-mode == 'sbom'` guard and the `grype-image` step the `scan-mode == 'image'` guard; `merge-service-images` has the job-level pull-request guard |
| `test_grype_sbom_input_wiring` | The `grype-sbom` step reads `inputs.sbom-output-file`, and the merge calls pass the filenames (`sbom-libvirt.cyclonedx.json`, etc.) |
| `test_grype_image_input_wiring` | The `grype-image` step reads `inputs.image-ref-for-scan`; `merge-manifest-and-attest` builds it from `inputs.image` and the merged digest; the `build-service` step passes the composite tag |
| `test_grype_severity_threshold` | Both Grype steps use `severity-cutoff: high` |
| `test_grype_fail_build_false` | Both Grype steps use `fail-build: false` |
| `test_grype_output_format_sarif` | Both Grype steps use `output-format: sarif` |
| `test_sarif_upload_steps_exist` | 1 `upload-sarif` step in `supply-chain-attest` |
| `test_sarif_upload_categories` | The upload category forwards `inputs.grype-category`; the callers pass `grype-libvirt` and categories with `matrix.service` |
| `test_sarif_upload_always_condition` | The SARIF upload step has `if: always()`, the pull-request guard, and the SARIF output guard |
| `test_sarif_upload_action_sha_pinned` | `github/codeql-action/upload-sarif` is SHA-pinned with `# v4` version comment |
| `test_sarif_upload_references_grype_output` | SARIF upload `sarif_file` references Grype step output |
| `test_security_events_permission_scoped_to_merge_jobs` | The `merge-*` jobs have `security-events: write`; the per-platform build jobs do not |
| `test_verify_jobs_no_security_events_permission` | Verify jobs do **not** have `security-events` permission |

## OCI Annotations

Every image this repository publishes is Nix-built and sets its labels in its Nix
expression ([Nix-Built Images](#nix-built-images)). The operator images, which the CI
workflow builds from `nix/lib/operator-image.nix`, carry the four static labels of
`nix/lib/static-image.nix` and no per-build label:

| Label | Value (example for keystone-operator) |
| --- | --- |
| `org.opencontainers.image.title` | `keystone-operator` |
| `org.opencontainers.image.description` | `CobaltCore keystone operator` |
| `org.opencontainers.image.licenses` | `Apache-2.0` |
| `org.opencontainers.image.vendor` | `SAP SE` |

`merge-operator-images` in `ci.yaml` records the per-build metadata on the manifest list
instead. Its `docker/metadata-action` step runs with `DOCKER_METADATA_ANNOTATIONS_LEVELS: index`
and gives the tags and the index annotations `created`, `revision`, `source`, `url` and
`version`, plus the four values of the table above; `docker buildx imagetools create`
writes them with `--annotation`. The step also passes
`--annotation "index:org.opencontainers.image.revision=${GITHUB_SHA}"` itself, which
`hack/ci-resolve-e2e-images.sh` reads to decide whether a published `:latest` is current.
No job of this workflow runs `docker/metadata-action`.

### Nix-Built Images

A Nix-built image carries four static labels, set in its Nix expression, and no
metadata-action step runs for it. A service or Tempest image adds
`org.opencontainers.image.version`, the upstream ref its lock pins (`29.0.0` for
keystone 2026.1, the tempest version for Tempest); that label changes only with the
lock. The ovn, hvo and kna images add it from their pin, and hvo and kna also carry
`io.c5c3.upstream-commit`; an operator image adds neither. Without the per-build labels (`created`, `revision`, `source`, `url`) and the
`base.name` / `base.digest` pair, the digest of a Nix-built image moves only when an
input of the flake moves; the build-provenance attestation of the merge job records the
commit. See
[Labels and reproducibility](./nix-image-build.md#labels-and-reproducibility).

### Version Label from the Lock

The upstream version of a service image comes from its lock: `nix/lib/service-image.nix`
sets `org.opencontainers.image.version` to the `ref` of the lock's `sources.json`, the
value `releases/<release>/source-refs.yaml` pins. The OCI version annotation therefore
reflects the software version, not a Git-derived one, and no `type=raw` metadata
strategy is involved. nova-compute gets nova's ref the same way, from the nova lock.
The federation proxy carries no version label, like the backup shifter: it bundles
several upstream components, and no single version names the image.

### Test Coverage

`verify_build_images_workflow.sh` checks no label: every image of this workflow sets
its labels in its Nix expression. `verify_hvo.sh` and `verify_kna.sh` check that the
`io.c5c3.upstream-commit` label of their image names the pinned commit.

## Adding a New Service

To add a new service (e.g., `nova`) to the build matrix:

### 1. Create the image expression

Add `images/<service>/image.nix`, a call of `nix/lib/service-image.nix` in the shape of
`images/keystone/image.nix`: a header comment with the `# DEVIATION` line, the
description `OpenStack <service> service`, and whatever the image adds to the venv (a
WSGI script, runtime packages, state directories). Name only the arguments the file
uses and end the argument set with `...`, because deadnix fails on an unused one.
`checks.<system>.nix-lint` formats and lints it ([Lint](./nix-image-build.md#lint)).

### 2. Add the source ref

Add the service to `releases/<release>/source-refs.yaml`:

```yaml
keystone: "28.0.0"
nova: "31.0.0"        # ← new entry
```

### 3. Add extra-packages entry and write the locks

Add the service to `releases/<release>/extra-packages.yaml` with its Python extras and
additional pip packages; its runtime packages go into `image.nix`, and
`verify_release_config.sh` fails a service block with an `apt_packages` key. Then run
`make nix-lock`, which writes `nix/locks/<release>/<service>/` for every release, and
commit the result: the flake outputs follow the lock directories, and `nix-lock-check`
fails until the locks exist.

```yaml
nova:
  pip_extras:
    - oslo_vmware
  pip_packages: []
```

### 4. Wire the change class, then verify matrix discovery

The `generate-matrix` job discovers all services from `source-refs.yaml` in each
`releases/*/` directory, and produces the `service × release` matrices consumed by
`build-service-images`, `test-service-images`, `merge-service-images`, and
`verify-service-images`. Adding the service to `source-refs.yaml` (step 2) is what
puts it there.

The [`changes`](#changes) job needs three edits in `.github/workflows/build-images.yaml`,
because its filter names cannot be generated:

1. A `svc_<service>` paths filter listing `images/<service>/**`,
   `nix/locks/*/<service>/**`, `patches/<service>/**`,
   `tests/container-images/verify_<service>.sh`, and
   `operators/<service>/api/v1alpha1/catalogs/**` when the workflow verifies that
   service's option catalog.
2. A <code v-pre>FILTER_svc_&lt;service&gt;: ${{ steps.filter.outputs.svc_&lt;service&gt; }}</code>
   line in the `env:` of the resolve step.
3. The name appended to `ALL_SERVICES` in that same `env:` block.

`tests/unit/ci/build_images_services_lockstep_test.sh` fails under `make test-shell`
when any of the three is missing. Without them the service is in the matrix but never
resolves to `true`, so its images stop being built on pull requests while the pipeline
stays green.

### 5. (Optional) Add patches

If the service requires patches, create patch files at
`patches/<service>/<release>/*.patch` and run `make nix-lock`, which lists them in the
lock's `sources.json`; `nix/lib/python-env.nix` applies them to the source of the image
and of the unit tests. No workflow changes are needed.

### 6. (Optional) Add constraint overrides

If the service requires constraint overrides, add entries to
`overrides/<release>/constraints.txt` and run `make nix-lock`. The
`apply-constraint-overrides.sh` script merges them into the constraints of the lock.

### 7. (Optional) Add test exclusions

If the service has upstream unit tests that cannot pass in CI (environment-dependent,
flaky, or infrastructure-requiring tests), create an exclusion file at
`releases/<release>/test-excludes/<service>.txt`. The file uses stestr exclude-list
format: one regex pattern per line, `#` for comments, blank lines allowed.
`nix/lib/service-tests.nix` picks up the file automatically when present — no workflow
changes are needed.

The tag derivation, the flake attribute of the build, the unit test run (via
`nix/lib/service-tests.nix`), supply chain attestation (via `supply-chain-attest`), and
verification steps all use matrix variables and work automatically for new services. The `verify-service-images`
job derives its own image refs independently via its own matrix strategy. Note that
adding a new service also requires creating a corresponding `verify_<service>.sh` test
script in `tests/container-images/`; the inline PR verification in
`build-service-images` derives the script name from the matrix. A service whose option
catalog the workflow verifies also joins the `if` list of the `Verify option catalog`
steps in `build-service-images` and `verify-service-images`.

### Release-independent images (non-OpenStack upstream)

Not every image tracks an OpenStack release. `keystone-federation-proxy`
compiles mod_auth_openidc from a pinned upstream commit beside nixpkgs' Apache
and mod_auth_mellon, and `ovn` compiles a family of C daemons from pinned
upstream commits. Images on
this path have no entry in `source-refs.yaml` or `extra-packages.yaml`, and
`generate-matrix` never sees them. What they need in place of that wiring:

- The upstream version pinned beside the image. A Nix-built image keeps it in
  `images/<image>/source.json` with the commit and its hash, for example the
  `version` and `rev` keys of `images/ovn/source.json` or
  `images/keystone-federation-proxy/source.json`, and `hack/nix-lock.sh`
  writes the hashes (`make nix-lock`).
- `hack/ci-resolve-<image>-version.sh` as the parser of that pin in CI
  outside the Nix build and `hack/nix-lock.sh`, which read the file
  themselves. The workflow and the verify script call it.
- A build a contributor reproduces with one command: `nix build
  .#<image>-image`.
- An expression at `images/<image>/image.nix`
  ([Nix Image Build](./nix-image-build.md)) and a `build-<image>` job that
  needs `changes`, `nix-flake-check` and `prepare` with its platform include
  matrix written out and calls `build-nix-image`, a PR-skipped
  `merge-<image>-image` job, and a `verify-<image>-image` job when the merged
  manifest is checked after the push.
- Change detection: a paths filter for the image in the `changes` job, its
  `FILTER_<name>` env line on the resolve step and a `build-<name>` output; the
  flag in `hack/ci-resolve-image-changes.sh` (the header, both branches and the
  `emit` lines) and in `assert_all_flags` of
  `tests/unit/hack/ci_resolve_image_changes_test.sh`; and
  `if: needs.changes.outputs.build-<name> == 'true'` on the build job. GitHub
  resolves an unknown output to the empty string, so a missing link leaves the
  image unbuilt on every pull request without an error.
- `tests/container-images/verify_<image>.sh` as the image contract, wired into
  the inline PR verification and into the post-merge job.
- Static assertions: the job structure and tag scheme in
  `verify_build_images_workflow.sh`, a note in `verify_release_config.sh`
  recording why the image is absent from its `SERVICES` list, and a check in
  `verify_deviation_comments.sh` when the image deviates from the user of
  `nix/lib/service-image.nix`, by creating the `openstack` user itself or by
  keeping root.
- A Renovate `customManager` for the pin, `packageRules` for the update
  types the image accepts, and a regression test under `tests/unit/renovate/`.
  A pin with a hash fails `nix-lock-check` after a Renovate move until
  `make nix-lock` is pushed.

An image pinned to an upstream commit instead of a version names its resolver
`hack/ci-resolve-<image>-commit.sh` and reads the `rev` key of its
`source.json`. `<image>` is the short name the jobs and test files use:
`openstack-hypervisor-operator` has `hack/ci-resolve-hvo-commit.sh`,
`build-hvo` and `verify_hvo.sh`, and `kvm-node-agent` has
`hack/ci-resolve-kna-commit.sh`, `build-kna` and `verify_kna.sh`.

An image built from nixpkgs alone (`libvirt`, `backup-shifter`) has no upstream
pin of its own and therefore skips the resolver script and the Renovate
`customManager`. `keystone-federation-proxy` has a pin and a `customManager`
(the mod_auth_openidc manager) but no resolver script, because no job reads its
version: `verify_keystone_federation_proxy.sh` reads the `version` key itself,
as `verify_nova.sh` reads `images/nova/novnc.json`.

## Adding a New Release

To add a new release series (e.g., `2026.1`):

### 1. Create release configuration

Create the release directory with required files:

```text
releases/2026.1/
├── extra-packages.yaml       # Extra pip packages per service
├── source-refs.yaml          # Service versions for this release
├── test-refs.yaml            # PyPI version pins for test tooling
├── test-excludes/            # (Optional) Per-service stestr exclude-lists
│   └── <service>.txt
└── upper-constraints.txt     # From openstack/requirements stable/2026.1
```

`extra-packages.yaml` is required — `hack/nix-lock.sh` reads the extras and pip
packages of every service from it, and the `nova-compute` block into the nova lock's
`nova-compute` group. `test-refs.yaml` is required — the tempest lock pins its five keys (tempest
and the barbican, keystone, neutron and cinder plugins). Then run `make nix-lock`, which
writes the locks of the new release under `nix/locks/<release>/`, and commit them. See
[Container Images — extra-packages.yaml](container-images.md#extra-packages-yaml) for
the YAML schema and `releases/2026.1/extra-packages.yaml` for a working example.

### 2. Verify matrix discovery

The `generate-matrix` job automatically discovers all releases from `releases/*/`
directories. Creating the release directory in step 1 is sufficient — no manual workflow
changes are needed. The job produces `service × release` matrices for all downstream jobs
(`build-service-images`, `merge-service-images`, `test-service-images`,
`verify-service-images`) and `release`-only matrices for Tempest pipeline jobs
(`build-tempest`, `merge-tempest-image`). Verify discovery by checking the
`generate-matrix` job output in a CI run.

### 3. (Optional) Add patches and overrides

Create `patches/<service>/<release>/` and `overrides/<release>/constraints.txt` as
needed, and run `make nix-lock` afterwards.

### 4. (Optional) Add test exclusions

If any services have upstream tests that fail in the new release's CI environment, create
`releases/<release>/test-excludes/<service>.txt` with stestr exclude-list patterns. Copy
patterns from the previous release's exclusion file as a starting point and adjust as
needed.

### 5. (Optional) Add Tempest test configuration

If multi-release Tempest testing is needed, create a release-specific Tempest configuration
directory under `tests/tempest/` (e.g., `tests/tempest/keystone-<release>/`) containing
`tempest.conf`, `include-tests.txt`, `exclude-tests.txt`, and `00-keystone-cr.yaml`. Then
add a corresponding matrix entry to the `tempest` job in `ci.yaml` (see
[CI Workflow — tempest](ci-workflow.md#tempest)).

## Verify Container Images Workflow

A separate workflow (`.github/workflows/verify-container-images.yaml`) runs static
verification tests against container infrastructure files without requiring Docker. This workflow validates the Nix expressions and locks, workflow structure, SPDX compliance,
release configuration, and constraint override scripts.

### Trigger Events

The workflow triggers on pull requests only:

| Event | Scope | Description |
| --- | --- | --- |
| `pull_request` | all branches | Runs when a changed path matches `images/**`, `releases/**`, `patches/**`, `scripts/**`, `hack/tempest/**`, `hack/gen-option-catalog.sh`, the keystone or glance option catalogs, `tests/container-images/**`, `tests/scripts/**`, `tests/tempest/test_retry_helpers.py`, `flake.nix`, `nix/lib/**`, `nix/pkgs/**`, the `build-nix-image` action, `build-images.yaml` or `verify-container-images.yaml` |

### Permissions and Concurrency

Top-level permissions are `contents: read` (least privilege). The concurrency group
follows the standard pattern: <code v-pre>${{ github.ref }}-${{ github.workflow }}</code> with
`cancel-in-progress` limited to pull request events.

### Job: verify-static-tests

A single job that runs all static test scripts sequentially. If any script exits
non-zero, the job fails.

| Property | Value |
| --- | --- |
| `runs-on` | `ubuntu-latest` |
| `timeout-minutes` | `15` |

**Steps:**

| # | Step | Action / Command | Details |
| --- | --- | --- | --- |
| 1 | Checkout | `actions/checkout@v7` | Checks out the repository |
| 2 | Install yq | Shell | Downloads `yq` binary from GitHub releases (version-pinned via `YQ_VERSION` env var) |
| 3 | Verify build-images workflow structure | Shell | Runs `tests/container-images/verify_build_images_workflow.sh` |
| 4 | Verify deviation comments | Shell | Runs `tests/container-images/verify_deviation_comments.sh` |
| 5 | Verify release config | Shell | Runs `tests/container-images/verify_release_config.sh` |
| 6 | Verify SPDX headers | Shell | Runs `tests/container-images/verify_spdx_headers.sh` |
| 7 | Test apply-constraint-overrides | Shell | Runs `tests/scripts/test_apply_constraint_overrides.sh` |
| 8 | Install tempest retry helper test dependencies | Shell | Installs `python3-defusedxml` with `apt-get` |
| 9 | Test tempest retry helpers | Shell | Runs `tests/tempest/test_retry_helpers.py` |

**Test scripts executed:**

| Script | Validates |
| --- | --- |
| `verify_build_images_workflow.sh` | Workflow structure: job names, dependency chain, trigger events, permissions, action pinning, concurrency, matrix strategy, SBOM/attestation configuration, cosign signing configuration, OCI annotation configuration, vulnerability scanning configuration, test-service-images job structure and steps |
| `verify_deviation_comments.sh` | `# DEVIATION` paragraphs in `images/<name>/image.nix` and `nix/lib/service-image.nix` cross-reference the Design Deviations section |
| `verify_release_config.sh` | `source-refs.yaml`, `extra-packages.yaml` structure and content validity, test-excludes file format and directory structure |
| `verify_spdx_headers.sh` | SPDX Apache-2.0 license headers present on all infrastructure files |
| `test_apply_constraint_overrides.sh` | `scripts/apply-constraint-overrides.sh` correctly applies constraint overrides |

yq is installed before the test scripts because `verify_build_images_workflow.sh` and
`verify_release_config.sh` use `yq` to parse YAML files. The installation uses a direct
binary download from the mikefarah/yq GitHub releases (more reliable across runner
updates than snap).

### Relationship to build-images.yaml

The verify-container-images workflow is intentionally separate from `build-images.yaml`
because it tests container _infrastructure_ (file structure, conventions, configs) rather
than the container _images_ themselves. Separate workflows provide clear, independent
signals in GitHub's check status UI. The image verification tests, which load or pull a
built image (`verify_<service>.sh`, `verify_libvirt.sh` and the other per-image scripts),
run inside `build-images.yaml` where the images are actually built.

## Verification Coverage Summary

The following table summarizes which test scripts run where:

| Test Script | verify-container-images.yaml | build-images.yaml | Requires Docker |
| --- | --- | --- | --- |
| `verify_build_images_workflow.sh` | verify-static-tests | — | No |
| `verify_deviation_comments.sh` | verify-static-tests | — | No |
| `verify_release_config.sh` | verify-static-tests | — | No |
| `verify_spdx_headers.sh` | verify-static-tests | — | No |
| `test_apply_constraint_overrides.sh` | verify-static-tests | — | No |
| `verify_<service>.sh` | — | build-service-images (PR) / verify-service-images (push) | Yes |
| `verify_ovn.sh` | — | build-ovn (PR) / verify-ovn-image (push) | Yes |
| `verify_nova_compute.sh` | — | build-nova-compute-image (PR) / verify-nova-compute-image (push) | Yes |
| `verify_backup_shifter.sh` | — | build-backup-shifter (PR) | Yes |
| `verify_libvirt.sh` | — | build-libvirt (PR) | Yes |
| `verify_hvo.sh` | — | build-hvo (PR) / verify-hvo-image (push) | Yes |
| `verify_kna.sh` | — | build-kna (PR) / verify-kna-image (push) | Yes |

## SPDX Header

The file starts with the standard SPDX license header:

```text
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
---
```

## Dependencies on Release Configs

The build-images workflow depends on the following artifacts:

| Artifact | Used by | Purpose |
| --- | --- | --- |
| `images/<service>/image.nix` | `build-service-images` | Service image (Nix expression over `nix/lib/service-image.nix`) |
| `images/nova-compute/image.nix` | `build-nova-compute-image` | The compute image, built from the nova lock's `nova-compute` group |
| `images/libvirt/image.nix`, `nix/pkgs/libvirt.nix` | `build-libvirt` | The libvirt image and the libvirt it shares with nova-compute |
| `nix/locks/<release>/<service>/` | `build-service-images`, `build-nova-compute-image`, `test-service-images`, `nix-lock-check` | The lock of the service release: source pin, patches, resolved dependencies |
| `nix/base-requirements.txt` | `nix-lock-check` | The base pins of every lock |
| `flake.nix`, `flake.lock` | every Nix job | The flake inputs and outputs |
| `images/<name>/source.json` | `build-ovn`, `build-hvo`, `build-kna`, `nix-lock-check` | The pin of an image built from an upstream commit |
| `releases/2026.1/source-refs.yaml` | `nix-lock-check`, `build-service-images` | Upstream version resolution |
| `releases/2026.1/upper-constraints.txt` | `nix-lock-check` | Python dependency pins |
| `scripts/apply-constraint-overrides.sh` | `nix-lock-check`, `build-service-images` | Constraint override application |

:::
