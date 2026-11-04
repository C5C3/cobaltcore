---
title: Tempest Test Infrastructure
quadrant: infrastructure
---

::: v-pre

# Tempest Test Infrastructure

Reference documentation for the Tempest API test infrastructure. This
covers the Tempest container image, version management, per-service test configuration,
the image verification script, local execution via `hack/run-tempest.sh`, the `make
tempest-test` target, and CI integration in both `ci.yaml` and `build-images.yaml`.
The infrastructure supports multiple releases: the `tempest` job in
`ci.yaml` uses a release matrix to validate each OpenStack release independently,
and `build-images.yaml` dynamically discovers releases for the Tempest image pipeline.

## File Locations

| File | Purpose |
| --- | --- |
| `images/tempest/image.nix` | Tempest container image, built by Nix from `nix/locks/<release>/tempest/` |
| `releases/<release>/test-refs.yaml` | PyPI version pins for test tooling (single source of truth), per release |
| `tests/tempest/keystone-2026-1/` | Keystone 2026.1 Tempest configuration (`tempest.conf`, `include-tests.txt`, `exclude-tests.txt`) |
| `tests/tempest/keystone-2026-2/` | Keystone 2026.2 Tempest configuration |
| `tests/tempest/glance-2026-1/` | Glance 2026.1 Tempest configuration: the `tempest.conf` / `include-tests.txt` / `exclude-tests.txt` triplet, a `00-keystone-cr.yaml` identity CR named `keystone-glance-tempest-2026-1`, and four extra fixtures the CI job applies — `01-catalog-setup-job.yaml` (image-catalog bootstrap Job), `02-glance-cr.yaml` (Glance CR), `03-glancebackend-cr.yaml` (default GlanceBackend CR), `04-glancebackend2-cr.yaml` (second, non-default store so the copy-image import test has a copy target) |
| `tests/tempest/glance-2026-2/` | Glance 2026.2 Tempest configuration (same file set; identity CR `keystone-glance-tempest-2026-2`) |
| `tests/tempest/barbican-2026-1/` | Barbican 2026.1 Tempest configuration: the `tempest.conf` / `include-tests.txt` / `exclude-tests.txt` triplet, a `00-keystone-cr.yaml` identity CR named `keystone-barbican-tempest-2026-1`, and four extra fixtures the CI job applies (`01-catalog-setup-job.yaml`, the key-manager catalog bootstrap Job; `02-barbican-cr.yaml`, the Barbican CR; `03-barbicansecretstore-cr.yaml`, the default BarbicanSecretStore the job waits on before the Barbican CR; `04-policy-check-job.yaml`, the Job that checks Barbican enforces its secure-RBAC policy defaults) |
| `tests/tempest/barbican-2026-2/` | Barbican 2026.2 Tempest configuration (same file set; identity CR `keystone-barbican-tempest-2026-2`) |
| `tests/tempest/neutron-2026-1/` | Neutron 2026.1 Tempest configuration: the `tempest.conf` / `include-tests.txt` / `exclude-tests.txt` triplet, a `00-keystone-cr.yaml` identity CR named `keystone-neutron-tempest-2026-1`, and four extra fixtures the CI job applies (`01-catalog-setup-job.yaml`, the network catalog bootstrap Job; `02-messaging-secret.yaml`, the RabbitMQ credentials the Neutron mounts; `03-ovncentral-cr.yaml`, the OVNCentral the job waits on before the Neutron CR; `04-neutron-cr.yaml`, the Neutron CR) |
| `tests/tempest/neutron-2026-2/` | Neutron 2026.2 Tempest configuration (same file set; identity CR `keystone-neutron-tempest-2026-2`) |
| `tests/tempest/cinder-2026-1/` | Cinder 2026.1 Tempest configuration: the `tempest.conf` / `include-tests.txt` / `exclude-tests.txt` triplet, a `00-keystone-cr.yaml` identity CR named `keystone-cinder-tempest-2026-1`, and fifteen extra fixtures the CI job applies (`01-catalog-setup-job.yaml`, the catalog bootstrap Job for the five services this leg registers: block-storage, image, compute, placement and network; `02-glance-cr.yaml` and `03-glancebackend-cr.yaml`, the image service the volume tests create volumes from and its default S3 store; `04-cinderbackend-cr.yaml` and `05-cinderbackupbackend-cr.yaml`, the NFS volume backend and backup target; `06-cinder-cr.yaml`, the Cinder CR; `07-image-seed-job.yaml`, the Job that uploads the image `[compute] image_ref` points at; `08-messaging-secret.yaml` through `13-nova-cr.yaml`, the compute stack the compute-tagged volume tests boot a server on (the Neutron's transport-URL Secret, an OVNCentral, a Neutron, a Placement, the metadata shared secret and the Nova); `14-fake-compute.yaml`, one fake-driver `nova-compute` registered under the host name `fake-1`; `15-flavor-seed-job.yaml`, the Job that creates the two flavors `tempest.conf` pins) |
| `tests/tempest/cinder-2026-2/` | Cinder 2026.2 Tempest configuration (same file set; identity CR `keystone-cinder-tempest-2026-2`) |
| `tests/tempest/nova-2026-1/` | Nova 2026.1 Tempest configuration: the `tempest.conf` / `include-tests.txt` / `exclude-tests.txt` triplet, a `00-keystone-cr.yaml` identity CR named `keystone-nova-tempest-2026-1`, and fourteen extra fixtures the CI job applies (`01-catalog-setup-job.yaml`, the compute, placement, image and network catalog bootstrap Job; `02-messaging-secret.yaml`, the transport-URL Secret the Neutron's messaging step reads; `03-ovncentral-cr.yaml`, the OVN databases the Neutron reads its addresses off; `04-neutron-cr.yaml`, the network service every server binds its port on; `05-placement-cr.yaml`, the inventory the compute publishes; `06-glance-cr.yaml` and `07-glancebackend-cr.yaml`, the image service the servers boot from and its default S3 store; `08-image-seed-job.yaml`, the Job that uploads the two 1 MiB raw images `[compute] image_ref` and `image_ref_alt` point at; `09-metadata-secret.yaml` and `10-nova-cr.yaml`, the metadata shared secret and the Nova CR; `11-fake-compute.yaml`, one fake-driver `nova-compute` whose `[DEFAULT] host` is the kind node's name; `12-ovnchassis-cr.yaml`, the chassis a port binds to; `13-neutronmetadataagent-cr.yaml`, the agent that answers 169.254.169.254; `14-flavor-seed-job.yaml`, the Job that creates `m1.nano` and `m1.micro`) |
| `tests/tempest/nova-2026-2/` | Nova 2026.2 Tempest configuration (same file set; identity CR `keystone-nova-tempest-2026-2`) |
| `tests/container-images/verify_tempest.sh` | Image verification script (PASS/FAIL counters) |
| `hack/run-tempest.sh` | Local orchestration script for running Tempest against a kind cluster |
| `hack/ci-run-tempest.sh` | CI-specific Tempest wrapper with port-forwarding and config generation |
| `hack/ci-generate-tempest-matrix.sh` | Generates the `tempest` job matrix from `releases/*/`, emitting one leg per service (`keystone`, `glance`, `barbican`, `neutron`, `cinder`, `nova`) per release, plus one `keystone-upgrade` entry for the newest release when `keystone` is selected and two or more releases exist |
| `hack/tempest/extract-failed.py` | Print anchored regex patterns for failed testcases in a JUnit report (used to build the retry include-list) |
| `hack/tempest/merge-retry-junit.py` | Merge a retry subunit stream into a JUnit report, rewriting resolved failures as flakes |
| `hack/tempest/run-tests.sh` | Shared in-container runner invoked by both runners; holds the phase + retry + exit-code logic so it stays identical between CI and local runs |
| `Makefile` | `tempest-test` target delegates to `hack/run-tempest.sh` |
| `.github/workflows/ci.yaml` | `tempest` job with release matrix |
| `.github/workflows/build-images.yaml` | `build-tempest` and `merge-tempest-image` jobs (release-parameterized via `generate-matrix`) |

## Architecture

![From the Tempest version pins to the JUnit report. releases/<release>/test-refs.yaml holds the version pins of tempest and the barbican, keystone, neutron and cinder Tempest plugins; hack/nix-lock.sh writes them into nix/locks/<release>/tempest/, the uv lock of the five pins and the clients; images/tempest/image.nix builds the image from that lock over nix/lib/service-image.nix. Two workflows consume the expression: build-images.yaml, where generate-matrix discovers the releases from releases/*/, build-tempest builds per release and platform and merge-tempest-image writes the multi-arch manifest, the SBOM, the Grype scan and the cosign signature; and ci.yaml, where build-e2e-images builds the image per release through hack/ci-nix-build-e2e-images.sh and the tempest matrix job loads the image, deploys the services, runs Tempest and uploads the JUnit per leg. The tempest job takes tests/tempest/<config-dir>/, prepared in _output/tempest/config and mounted at /etc/tempest: tempest.conf rendered with the localhost URI and the admin password, include-tests.txt split into phases/phase-1-core.txt and phases/phase-2-plugin.txt, and exclude-tests.txt copied. The result is tempest-results.xml in _output/tempest/, uploaded as the JUnit artifact.](../../diagrams/test-tempest-image-chain.svg)

## Version Management

### test-refs.yaml

**Location:** `releases/<release>/test-refs.yaml`

Maps each test tool to a PyPI version pin. This file is the single source of truth for
what version of each test tool is installed in the Tempest container image. It is separate
from `source-refs.yaml` (which tracks git refs for OpenStack services) so that test
tooling versions can evolve independently.

**Format:**

```yaml
tempest: "46.3.0"
keystone-tempest-plugin: "0.22.0"
```

The tempest pin may run ahead of the release line a directory represents:
tempest is branchless (released tags never receive fixes) and upstream
validates current plus recent stable releases with the latest tag. The
2026.1 directory pins tempest `46.3.0` and the 2026.2 directory `47.0.0`;
`renovate.json` disables major updates under `releases/**/test-refs.yaml`, so
2026.1 stays on 46.x. The glance legs
need `>= 46.2.0`, where the web-download bad-URL negative test accepts the
synchronous 400 that glance's DNS-based URI filtering returns.

Each key is a PyPI package name. Values are quoted strings representing exact version
pins. `hack/nix-lock.sh` reads the five keys into the tempest lock
`nix/locks/<release>/tempest/`, and a missing key fails it with
`nix-lock.sh: no key '<key>' in releases/<release>/test-refs.yaml`. The
`merge-tempest-image` job reads the tempest version for its version tag:

```bash
TEMPEST_VERSION=$(hack/resolve-test-ref.sh releases/<release>/test-refs.yaml tempest)
```

To update versions, edit `test-refs.yaml` and run `make nix-lock`; the
`nix-lock-check` job fails until the lock matches the pins.

## Container Image

### Image expression

**Location:** `images/tempest/image.nix`, built from the lock
`nix/locks/<release>/tempest/`

The Tempest image is built by Nix like the service images
([Nix Image Build](../ci-cd/nix-image-build.md)) but differs in three ways: (1) its
lock installs from PyPI instead of a git source tree, (2) it has no WSGI entrypoint,
and (3) it has no unit-test run. The lock pins these packages, resolved against the
release's constraints:

| Package | Purpose |
| --- | --- |
| `tempest` | OpenStack Tempest testing framework |
| `barbican-tempest-plugin` | Barbican-specific Tempest test plugins; pinned per release in `test-refs.yaml` (every release pins `4.6.0`) |
| `keystone-tempest-plugin` | Keystone-specific Tempest test plugins |
| `neutron-tempest-plugin` | Neutron-specific Tempest test plugins; pinned per release (`3.2.0` for 2026.1, `3.3.0` for 2026.2). Version 3.3.0 imports `neutron_lib.services.pvlan`, which the neutron-lib 2026.2 constrains (`5.0.1`) ships and the one 2026.1 constrains (`3.24.0`) does not |
| `cinder-tempest-plugin` | Cinder-specific Tempest test plugins; every release pins `1.23.0`, since the plugin needs only `pbr`, `oslo.config`, `oslo.serialization` and `tempest>=40.0.0` and no release's `upper-constraints.txt` narrows those |
| `python-openstackclient` | `openstack` CLI, used by the full-chain ControlPlane E2E verify Job (`token issue` / `catalog list`); version pinned by `upper-constraints.txt` |
| `osc-placement` | `openstack` CLI placement plugin, used by the full-chain ControlPlane E2E placement round-trip (`resource class list`); version pinned by `upper-constraints.txt` |
| `python-barbicanclient` | `openstack secret` commands, used by the full-chain ControlPlane E2E verify Job round-trip; version pinned by `upper-constraints.txt` |
| `python-subunit` | Subunit test result streaming protocol |
| `junitxml` | Subunit-to-JUnit XML conversion (`subunit2junitxml`) |
| `defusedxml` | Hardened XML parsing for the result-processing tooling |

The image creates `/etc/tempest/`, owned by root, where the tempest Job mounts its
configuration. `hack/tempest/run-tests.sh` sets `HOME=/tmp`, so the read-only
`/var/lib/openstack` needs no owner change. The labels are the five of
[Labels and reproducibility](../ci-cd/nix-image-build.md#labels-and-reproducibility);
`org.opencontainers.image.version` is the tempest version of the lock.

**Final image properties:**

| Property | Value |
| --- | --- |
| User | `openstack` (UID 42424) |
| `tempest` CLI | Available via `PATH` (`/var/lib/openstack/bin/tempest`) |
| `openstack` CLI | Available via `PATH`; verified by `verify_tempest.sh` (`openstack --version`) |
| `subunit2junitxml` | Available via `PATH` |
| Build tools | Absent (`gcc`, `cc`, `uv` and `python3-config` are not on `PATH`; `/nix/store` holds no compiler, binutils or uv package) |

### Local Build

Build the Tempest image of a release with Nix and load it into Docker:

```bash
RELEASE=2026.1   # or 2026.2
nix build ".#tempest-${RELEASE//./_}-image" && ./result | docker load
bash tests/container-images/verify_tempest.sh "tempest:${RELEASE}"
```

`hack/run-tempest.sh` runs the same build when `BUILD_IMAGE` is `true` (the default),
naming the attribute of the host's system (`packages.x86_64-linux.` or
`packages.aarch64-linux.`), and tags the loaded `tempest:<release>` as
`TEMPEST_IMAGE`. Without `nix` on `PATH` it exits 1 with `run-tempest.sh: nix not
found; install Nix or rerun with BUILD_IMAGE=false
TEMPEST_IMAGE=ghcr.io/c5c3/tempest:<release>`. The flake builds images for those two
Linux systems only, so on any other host (macOS) it exits 1 before Nix runs, naming
the host and the same `BUILD_IMAGE=false` rerun.
`tests/unit/hack/run_tempest_build_image_test.sh` pins the build with stub `nix`,
`docker` and `uname` commands.

## Test Configuration

Per-service Tempest configuration lives under `tests/tempest/<service>/`. Each service
directory contains three files: a Tempest configuration file, an include list, and an
exclude list.

### tempest.conf

**Location:** `tests/tempest/<service>/tempest.conf`

INI-format configuration file for the Tempest testing framework. Key sections for the
Keystone service:

| Section | Key | Value | Purpose |
| --- | --- | --- | --- |
| `[DEFAULT]` | `log_dir` | `/tmp/tempest-logs` | Log output directory inside container |
| `[identity]` | `uri_v3` | `http://keystone-tempest-<slug>.openstack.svc:5000/v3` | Keystone v3 API endpoint (in-cluster DNS) |
| `[auth]` | `use_dynamic_credentials` | `true` | Use dynamic credentials (Tempest creates the projects and users of its tests) |
| `[auth]` | `admin_username` | `admin` | Admin user for API authentication |
| `[auth]` | `admin_password` | `${KEYSTONE_ADMIN_PASSWORD}` | Injected at runtime from K8s secret |
| `[auth]` | `admin_project_name` | `admin` | Admin project scope |
| `[auth]` | `admin_domain_name` | `Default` | Admin domain scope |
| `[identity-feature-enabled]` | `api_v3` | `true` | Enable v3 identity API tests |
| `[service_available]` | `nova` | `false` / `true` | Nova is deployed on the nova and cinder legs; the keystone, glance, barbican and neutron legs set `false` |
| `[service_available]` | `neutron` | `false` / `true` | Neutron is deployed on the neutron, nova and cinder legs; the keystone, glance and barbican legs set `false` |
| `[service_available]` | `cinder` | `false` / `true` | Cinder is deployed on the cinder legs; the keystone, glance, barbican, neutron and nova legs set `false` |
| `[service_available]` | `glance` | `false` / `true` | Glance is deployed on the glance, cinder and nova legs; the keystone, barbican and neutron legs set `false` |
| `[service_available]` | `placement` | `true` | Placement is deployed on the nova and cinder legs; no other leg writes the key |
| `[service_available]` | `barbican` | `false` / `true` | Barbican is deployed on the barbican legs; the neutron, cinder and nova legs set `false`; the keystone and glance legs do not write the key |
| `[service_available]` | `swift` | `false` | Swift is not deployed |
| `[service_available]` | `horizon` | `false` | Every leg sets `false` |

The `admin_password` placeholder `${KEYSTONE_ADMIN_PASSWORD}` is resolved at runtime:
- **Local execution:** `hack/run-tempest.sh` extracts it from the `keystone-admin` K8s
  secret and substitutes it into a generated copy of `tempest.conf` using `sed`
- **CI execution:** `hack/ci-run-tempest.sh` extracts it via `kubectl get secret` and
  substitutes it into a generated copy of `tempest.conf` using `sed`

### include-tests.txt

**Location:** `tests/tempest/<service>/include-tests.txt`

One regex pattern per line. Lines starting with `#` are comments. Blank lines are
ignored. Patterns are matched against Tempest test IDs.

For Keystone, two patterns include all identity-related tests:

| Pattern | Matches |
| --- | --- |
| `tempest.api.identity` | Core Tempest identity API tests |
| `keystone_tempest_plugin.tests` | Keystone-specific plugin tests |

The neutron legs select the core `tempest.api.network` CRUD modules
(networks, ports, security groups, subnet pools) and the matching
`neutron_tempest_plugin.api` ones. The router, floating-IP and
provider-network suites stay out until the devstack has an external
provider network; `include-tests.txt` records that reason next to the
patterns it keeps.

The cinder legs take the whole volume tree of both scopes,
`tempest.api.volume` and `cinder_tempest_plugin.api.volume`. What the
environment cannot serve is switched off through
`[volume-feature-enabled]` in `tempest.conf` instead: the NFS driver
takes no snapshots.

The nova legs take the whole compute tree in one pattern,
`tempest.api.compute`. Nothing lands in their plugin phase, which the
runner allows.

#### Scope-split invariant

Both runners (`hack/ci-run-tempest.sh` and `hack/run-tempest.sh`) split this
include list into two phase files at runtime and run `stestr` twice in the
same workspace:

1. `phase-1-core.txt` — every non-comment line that starts with `tempest.`
2. `phase-2-plugin.txt` — every non-comment line matching
   `^[a-z0-9_]+_tempest_plugin\.`, which covers `keystone_tempest_plugin.`,
   `barbican_tempest_plugin.`, and any plugin added later without naming it in
   the runners

The two phases run **sequentially**; within each phase `stestr` runs at
`TEMPEST_CONCURRENCY` (default 4).

Each runner enforces that every non-comment, non-empty line in
`include-tests.txt` lands in exactly one phase. A line with any other prefix
causes the runner to abort with a clear error — this guards against silent
drops when new include patterns are added.

Either phase may be empty, and both runners treat that case the same way. The
glance legs ship a pure core-tempest include list (`tempest.api.image` only),
which leaves `phase-2-plugin.txt` empty; the barbican legs ship a pure plugin
list (`barbican_tempest_plugin.tests.api` only), which leaves
`phase-1-core.txt` empty. `hack/tempest/run-tests.sh` skips whichever phase
file is empty and writes an empty subunit stream in its place so the
concatenations still line up, instead of running `stestr` against an empty
include list. Only a split that leaves **both** phases empty is a hard error,
in `hack/ci-run-tempest.sh` and `hack/run-tempest.sh` alike: an include list
with no non-comment pattern at all lands there and exits 1.

**Why sequential.** Keystone re-resolves the list of enabled federation
service providers on every `POST /v3/auth/tokens` and `GET /v3/auth/tokens`
call and injects it into the response body; the token itself does not cache
it. `tempest.api.identity.v3.test_tokens.TokensV3Test.test_validate_token`
issues a token via POST, validates it via GET, and asserts the two response
bodies are equal. When `keystone_tempest_plugin.tests.api.identity.v3.
test_service_providers.ServiceProvidersTest` runs concurrently on another
stestr worker, its per-test `addCleanup` deletes the service provider it
created — and if that cleanup lands in the ~20 ms window between
`test_validate_token`'s POST and GET, the two responses diverge on the
`service_providers` key and the assertion fails.

Upstream's `openstack/keystone` `keystone-tempest` gate job sets
`tempest_test_regex: "keystone_tempest_plugin"` and therefore only runs
`keystone_tempest_plugin.*` tests — the core `tempest.api.identity.*` suite
(which contains `test_validate_token`) never runs in the same Tempest
invocation as the service-providers tests, so upstream never observes this
race. We run both suites for fuller coverage and replicate the isolation by
running them in two separate stestr invocations.

The two phases each emit a subunit stream (`phase-1-core.subunit`,
`phase-2-plugin.subunit`); the runner concatenates them into
`tempest.subunit` and converts that to JUnit XML. Subunit v2 is
stream-concatenation safe by design.

#### Serial retry of failing tests

After both phases, the runner inspects the JUnit report: if any test is
marked failed or errored, those test IDs are extracted and rerun once in a
third `stestr run` invocation with `--concurrency 1`. The retry output is
written to `retry.subunit`, appended to the combined subunit stream, and
merged into the JUnit report: tests that pass on retry have their
`<failure>`/`<error>` children removed, the enclosing `<testsuite>` counters
are decremented, and a `<system-out>` note records `flaky: failed on first
run, passed on retry`. Tests that still fail after retry stay as failures.

The two helpers live at `hack/tempest/extract-failed.py` (reads the JUnit
report, prints anchored regex patterns for each failed `classname.method`)
and `hack/tempest/merge-retry-junit.py` (rewrites the JUnit report from the
retry subunit stream). The phase + retry + exit-code sequence itself lives
in `hack/tempest/run-tests.sh`, which is invoked inside the container by
both `hack/ci-run-tempest.sh` and `hack/run-tempest.sh`. All three files
are staged next to `tempest.conf` so they are available at `/etc/tempest/`
inside the container. A failure inside either Python helper (missing
dependency, parse error) is caught by the runner and falls back to the
original stestr exit code rather than aborting the whole Tempest run.

The final exit code is derived from the (possibly retry-adjusted) JUnit
report: any remaining failures or errors fail the job. If the retry
resolved every failure the runner exits 0. If the initial `stestr` process crashed hard
enough that no test was recorded, the runner exits 1 at its zero-test checks. The
captured phase exit code decides the result only when the report lists tests, no
failures or errors remain, and no retry was merged.

### exclude-tests.txt

**Location:** `tests/tempest/<service>/exclude-tests.txt`

Same format as `include-tests.txt`. Patterns exclude tests that require services or
infrastructure not available in the CI kind cluster:

| Pattern | Reason for exclusion |
| --- | --- |
| `keystone_tempest_plugin\.tests\..*ldap` | Requires a live LDAP server |
| `keystone_tempest_plugin\.tests\..*federation` | Requires an external IdP (SAML2/OAuth2) |
| `keystone_tempest_plugin\.tests\..*oauth2` | Requires an external authorization server |

#### Tracking version-coupled RBAC excludes

Unlike the infrastructure excludes above, the RBAC excludes are coupled to a
specific tempest plugin version — they exist because the plugin's expected
status codes disagree with the service's default policies for that release.
They must not silently outlive their cause. Each RBAC exclude group therefore
carries a `# tracked-by:` / `# re-evaluate-on:` comment pair:

```text
# tracked-by: <issue URL>
# re-evaluate-on: keystone-tempest-plugin > <current pinned version>
```

On every `keystone-tempest-plugin` bump, re-run the excluded RBAC groups against
the new plugin and drop any pattern upstream has fixed. The `re-evaluate-on` version is
per group and per release: two groups carry `> 0.20.0` and the third `> 0.22.0`
in the 2026.1 list (`tests/tempest/keystone-2026-1/exclude-tests.txt:27,31,37`).
Both `test-refs.yaml` pin `0.22.0`; the 2026.2 list carries no RBAC group
(below).

The RBAC suite runs only when the plugin sees scope enforcement. From 0.22.0 on
it reads `[identity-feature-enabled] enforce_scope`, which every keystone
`tempest.conf` sets to `true`. Plugin 0.21.0 read only Tempest's
`[enforce_scope] keystone`, which they leave unset, and skipped all 325 RBAC
classes. The first 0.22.0 run failed three tests the release's default policy
answers differently from the plugin, and a group tracked by
[#1127](https://github.com/C5C3/cobaltcore/issues/1127) excludes them. On
2026.1 that is `test_ec2_credential`, where a reader persona gets `201` creating
its own EC2 credential and the plugin expects `403`. The group carries
`re-evaluate-on: keystone-tempest-plugin > 0.22.0` in that release list. The
barbican legs follow the same convention for their one exclude,
`barbican_tempest_plugin\.tests\.api\.test_quotas\.ProjectQuotasTest\.test_manage_project_quotas`.
The plugin runs that test with a credential that holds only
`key-manager:service-admin`, which Barbican's
[new policy defaults](../barbican/index.md#design-decisions) refuse for project
quotas. The exclude is tracked by
[#991](https://github.com/C5C3/cobaltcore/issues/991) and carries
`re-evaluate-on: barbican-tempest-plugin > 4.5.0` on 2026.1 and
`re-evaluate-on: barbican-tempest-plugin > 4.6.0` on 2026.2. The excluded
test was the leg's only runtime sign of that policy posture, so the leg's
`04-policy-check-job.yaml` Job asserts the posture before Tempest runs. The
2026.2 keystone list carries neither the #1127 group nor the
[#480](https://github.com/C5C3/cobaltcore/issues/480) RBAC groups of the 2026.1
list, so the 2026.2 keystone leg runs those tests until a 2026.2 run shows
their outcome.

### Compute configuration of the nova legs

`tests/tempest/nova-<slug>/tempest.conf` carries the
`[compute-feature-enabled]` table the Phase-0 lab of
[#1015](https://github.com/C5C3/cobaltcore/issues/1015#issuecomment-5685726178)
recorded in its section (d.6), kept as the lab measured it: 29 flags true,
three false. That run covered 522 tests in 42 minutes at two stestr workers.
With every flag on, the same suite took 91 minutes and failed 18 tests.

| Flag | Value | Reason |
| --- | --- | --- |
| `snapshot` | `false` | The fake driver's snapshot sets `IMAGE_UPLOADING` and never uploads, so the Glance image stays queued. Each test that takes one waits about 300 s and then fails |
| `stable_rescue` | `false` | Its rescue image is a snapshot of a server, so it runs into the same wait |
| `spice_console` | `false` | The API answers 400 `Unavailable console type spice-direct` |

The migration flags stay true. The tests they gate skip on `[compute]
min_compute_nodes = 1` instead of on the flag: the live-migration,
cold-migration and multi-node classes all need a second host. One kind node
carries one chassis hostname, and neutron binds a port only to a host with a
live chassis, so a second compute could not bind ports.

Four tests are excluded outright. `exclude-tests.txt` holds them as one group
under the `# tracked-by:` / `# re-evaluate-on:` convention described above,
tracked by [#1014](https://github.com/C5C3/cobaltcore/issues/1014), with two
conditions that retire it: a compute driver other than nova's fake driver on
this leg for the last three patterns, and a leg that creates a default external
network and subnet pool for the first.

| Excluded test | Reason |
| --- | --- |
| `test_auto_allocate_network.AutoAllocateNetworkTest.test_server_multi_create_auto_allocate` | 400 `Unable to automatically allocate a network for project`. neutron loads `auto_allocate` as a default service plugin, so tempest finds the auto-allocated-topology extension and runs the test, and this leg has no default external network and no subnet pool to allocate from |
| `test_hypervisor.HypervisorAdminTestJSON.test_get_hypervisor_uptime` | The fake driver has no `get_host_uptime`, and the base driver raises `NotImplementedError` |
| `test_server_actions.ServerActionsTestOtherB.test_get_console_output_with_unlimited_size` | The fake console output is three fixed lines. The other `console_output` tests pass, which is why that flag stays true |
| `test_novnc.NoVNCConsoleTestJSON.test_novnc` | The console HTML and the websocket upgrade pass, then the proxy dials the fake driver's console target `fakevncconsole.com:6969`, cannot resolve it and closes the socket. The pattern ends in `\[`, which keeps `test_novnc_bad_token` in the run |

The cinder legs take the other side of the same stack. They run
`tempest.api.volume` and `cinder_tempest_plugin.api.volume` with
`[service_available] nova = true` against a fake compute named `fake-1` whose
servers carry no NIC, which is what lets `test_incremental_backup` run there:
that test boots a server between its full and its incremental backup and has no
compute guard.

Two compute-tagged volume tests are excluded on every cinder leg, as their own
group under the same convention and tracked by
[#1014](https://github.com/C5C3/cobaltcore/issues/1014):
`test_volumes_backup.VolumesBackupsTest.test_backup_create_attached_volume` and
`test_volumes_backup.VolumesBackupsTest.test_volume_backup_incremental`. Both
back up a volume while it is attached to a server. On the NFS backend that
backup is served from a temporary clone, and cloning an in-use volume takes an
online snapshot through Nova's assisted-snapshot API; the fake driver has no
`volume_snapshot_create`, so cinder-volume waits 600 s for a progress update
that never comes, the backup stays `creating` past tempest's 300 s, and the
test spends another 300 s failing to delete the still-attached volume. The
group retires with a compute driver other than the fake one on this leg, or an
NFS backend that supports snapshots (#979 D3 keeps `nfs_snapshot_support` at
false).

### Adding a New Service

Tempest coverage is keyed per **service × release**. `glance` is the worked
example: `hack/ci-generate-tempest-matrix.sh` emits a `keystone`, a `glance`,
a `barbican`, a `neutron`, a `cinder` and a `nova` leg for every release, so a
new service needs one config directory per release, not a single directory. To
add another service:

1. Create a `tests/tempest/<service>-<slug>/` directory for each release (e.g.
   `<service>-2026-1` and `<service>-2026-2`), each with `tempest.conf`,
   `include-tests.txt`, `exclude-tests.txt`, and the `00-keystone-cr.yaml`
   identity CR the job waits on. The matrix generator fails the `changes` job if a
   release is missing the directory, so no `tempest` leg starts. A service with its own
   operator payload also ships the CRs the job applies before Tempest runs — glance
   carries `01-catalog-setup-job.yaml` (registers the image service + endpoints in
   Keystone), `02-glance-cr.yaml`, `03-glancebackend-cr.yaml`, and
   `04-glancebackend2-cr.yaml` (a second, non-default store: the copy-image import test
   only executes a real copy when there is a store the image is not already in).
2. Set `[service_available]` flags to match the deployed services and point
   `[identity]` `uri_v3` at the leg's identity CR (glance authenticates against
   its own `keystone-glance-tempest-<slug>` CR, not the shared keystone leg's CR).
3. Extend `hack/ci-generate-tempest-matrix.sh` to emit the new `service` leg. If
   the service needs an extra port-forward, thread it through
   `hack/ci-run-tempest.sh` the way glance uses the optional `GLANCE_K8S_NAME`
   env (9292 forward + `/healthcheck` poll + host mappings), barbican the
   optional `BARBICAN_K8S_NAME` (the same three steps on 9311), neutron the
   optional `NEUTRON_K8S_NAME` (the same three steps on 9696), cinder the
   optional `CINDER_K8S_NAME` (the same three steps on 8776), nova the
   optional `NOVA_K8S_NAME` (the same three steps on 8774, polled on `/`) and
   placement the optional `PLACEMENT_K8S_NAME` (the same three steps on 8778,
   polled on `/`),
   then add the conditional deploy steps to the `tempest` job in `ci.yaml`,
   gated on `matrix.service == '<service>'`.

The scope-split needs no edit for a new plugin: `phase-2-plugin.txt` is filled
by `^[a-z0-9_]+_tempest_plugin\.`, which already matches any
`<service>_tempest_plugin.` pattern. A leg that selects only plugin tests, or
only core `tempest.` ones, leaves the other phase empty, which both runners
allow.

A service tested through core `tempest.api.*` alone needs no image change. One
that brings its own Tempest plugin adds its key to the plugin list of
`hack/nix-lock.sh` plus a version pin to every release's `test-refs.yaml`,
and runs `make nix-lock`, the way `barbican-tempest-plugin`,
`neutron-tempest-plugin` and `cinder-tempest-plugin` are wired.
`hack/run-tempest.sh` (local execution) runs a single keystone leg and accepts
`SERVICE`, `RELEASE`, and `ADMIN_SECRET` overrides.

## Image Verification

**Location:** `tests/container-images/verify_tempest.sh`

Validates that the built Tempest container image meets requirements. Uses the same
PASS/FAIL counter pattern as other `verify_*.sh` scripts and sources
`tests/lib/assertions.sh` for assertion helpers.

**Usage:**

```bash
bash tests/container-images/verify_tempest.sh [image_name]
# Default image: c5c3/tempest:46.3.0
```

**Test cases:**

| Test | Assertion | Validates |
| --- | --- | --- |
| `test_tempest_version` | `tempest --version` exits 0 with non-empty output | Tempest CLI is installed and functional |
| `test_keystone_tempest_plugin_importable` | `python3 -c 'import keystone_tempest_plugin'` exits 0 | Plugin is installed in the virtualenv |
| `test_barbican_tempest_plugin_importable` | `python3 -c 'import barbican_tempest_plugin'` exits 0 | Plugin is installed in the virtualenv |
| `test_neutron_tempest_plugin_importable` | `python3 -c 'import neutron_tempest_plugin'` exits 0 | Plugin is installed in the virtualenv |
| `test_cinder_tempest_plugin_importable` | `python3 -c 'import cinder_tempest_plugin'` exits 0 | Plugin is installed in the virtualenv |
| `test_barbicanclient_importable` | `python3 -c 'import barbicanclient'` exits 0 | python-barbicanclient is installed in the virtualenv |
| `test_subunit2junitxml_available` | `which subunit2junitxml` exits 0 with non-empty path | JUnit XML converter is on PATH |
| `test_openstack_cli_available` | `openstack --version` exits 0 with non-empty output | `openstack` CLI is installed and functional |
| `test_osc_placement_cli_registered` | `openstack resource class list --help` exits 0 and prints `usage: openstack resource class list` | osc-placement registers its `openstack` CLI commands |
| `test_runs_as_openstack_user` | `whoami` outputs `openstack` | Container runs as non-root user |
| `test_no_build_tools_in_final_image` | `command -v` finds none of `gcc`, `cc`, `uv` and `python3-config`, and `ls /nix/store` lists no compiler, binutils or uv package | Build tools are not in the runtime image |

All test functions use the exit-code guard pattern (`|| exit_code=$?`) to prevent
`set -e` from aborting the script before assertions run.

In CI, this script runs during the `build-tempest` job on pull requests to catch image
build regressions independently of the full E2E pipeline.

## Local Execution

### hack/run-tempest.sh

**Location:** `hack/run-tempest.sh`

Orchestration script for running Tempest API tests against a deployed OpenStack service
in a local kind cluster. Follows the infrastructure deployment script pattern:
`set -euo pipefail`, `log()` with ISO 8601 timestamps, `SCRIPT_DIR`/`REPO_ROOT`
resolution, and configurable variables.

**Usage:**

```bash
SERVICE=keystone hack/run-tempest.sh
```

**Environment variables:**

| Variable | Default | Description |
| --- | --- | --- |
| `SERVICE` | *(required)* | OpenStack service to test (e.g., `keystone`) |
| `RELEASE` | `2026.1` | Release version (selects `test-refs.yaml` and `upper-constraints.txt`) |
| `TEMPEST_IMAGE` | `c5c3/tempest:local` | Docker image name for the Tempest container |
| `OUTPUT_DIR` | `_output/tempest` | Directory for test results (JUnit XML, subunit stream) |
| `TEMPEST_TIMEOUT` | `1800` | Timeout for Tempest execution in seconds |
| `NAMESPACE` | `openstack` | Kubernetes namespace for the service under test |

**Execution steps:**

| Step | Description | Failure behavior |
| --- | --- | --- |
| Pre-flight checks | Validates `SERVICE` is set, required tools (`docker`, `kubectl`, `yq`) are installed, Docker is running, service config directory exists, `test-refs.yaml` exists | Exits 1 with descriptive error |
| Build Tempest image | Builds the flake attribute `tempest-<rel_>-image` with `nix build` and loads the stream with `docker load`; the tempest lock pins the `test-refs.yaml` versions | Exits on build failure (set -e) |
| Extract admin password | Reads `keystone-admin` secret from the K8s cluster via `kubectl get secret` | Exits 1 if secret is not found or empty |
| Run Tempest | Mounts `tempest.conf`, `phases/phase-1-core.txt`, `phases/phase-2-plugin.txt`, `exclude-tests.txt`, `extract-failed.py`, `merge-retry-junit.py` into container; initializes Tempest workspace; runs `stestr run --subunit` once per phase; concatenates the subunit streams; converts to JUnit XML; reruns any failed tests serially; merges the retry outcome into the JUnit report | Non-zero if any failure remains after retry, or on hard `stestr` crash |

**Output files:**

| File | Format | Description |
| --- | --- | --- |
| `_output/tempest/tempest-results.xml` | JUnit XML | Test results for CI artifact upload (retry-adjusted) |
| `_output/tempest/tempest.subunit` | Subunit v2 | Raw test result stream (phase 1 + phase 2 + retry) |
| `_output/tempest/retry.subunit` | Subunit v2 | Retry stream (only present if any tests failed on first run) |

The script exits non-zero if the retry-adjusted JUnit report still lists
failures or errors, and otherwise exits zero. Both phases always run, so a
failure in phase 1 does not short-circuit phase 2. If the initial `stestr` invocations
crashed hard enough that no test was recorded, the script exits 1 at its zero-test
checks. The captured phase exit code is used as a fallback when the report lists tests,
no failures or errors remain, and no retry was merged, so infra failures are still
reported.

### make tempest-test

**Location:** `Makefile`

```bash
make tempest-test SERVICE=keystone
```

The `tempest-test` target validates that `SERVICE` is set (using the `$(if)` guard
pattern consistent with other Makefile targets) and delegates to `hack/run-tempest.sh`.
Omitting `SERVICE` produces an error message:

```
*** tempest-test requires SERVICE, e.g. make tempest-test SERVICE=keystone.  Stop.
```

## CI Integration

### ci.yaml — tempest Job

The `tempest` job is a dedicated job that deploys services into a kind
cluster and runs the OpenStack Tempest test suite. `hack/ci-generate-tempest-matrix.sh`
fans the matrix out over two dimensions — a `release` and a `service` — so each
OpenStack release is validated independently for `keystone`, `glance`,
`barbican`, `neutron`, `cinder` and `nova`, each leg with its own Tempest
configuration and identity CR (and, for glance, an image CR and backend; for
barbican, a key-manager CR and its secret store; for neutron, an OVNCentral and
a network CR; for cinder, a volume CR with its NFS backend and backup target,
plus an image CR of its own).

The nova leg carries a compute stack: an OVNCentral, a Neutron, a Placement, a
Glance, a Nova, one fake compute registered under the kind node's name, an OVN
chassis and a metadata agent. The cinder legs carry the same stack without the
chassis and the agent, because the servers their volume tests boot bind no
port. "Compute stack" names those two legs everywhere below.

**Release matrix:**

The generator scans `releases/*/` and emits one `keystone`, one `glance`, one
`barbican`, one `neutron`, one `cinder` and one `nova` leg per release. One
`keystone-upgrade` entry for the newest release follows them (see
[Keystone upgrade leg](#keystone-upgrade-leg)), which makes thirteen entries
across the two releases in the tree. Each service requires a matching
`tests/tempest/<service>-<slug>` directory or the generator fails the `changes` job.
`service-k8s-name` always equals `cr-name` (the Keystone identity CR the job waits on
and port-forwards on 5000). The glance legs additionally carry `glance-cr-name` (the
Glance CR the job waits on and port-forwards on 9292), the barbican legs
`barbican-cr-name` (the Barbican CR, waited on and port-forwarded on 9311) and the
neutron legs `neutron-cr-name` (the Neutron CR, waited on and port-forwarded on 9696)
plus `ovn-cr-name` (the OVNCentral the Neutron needs Ready before it renders
`ml2_conf.ini`, waited on but not port-forwarded). The cinder legs carry
`cinder-cr-name` (the Cinder CR, waited on and port-forwarded on 8776) and a
`glance-cr-name` of their own, for the image service their volume tests create volumes
from.

Both compute-stack legs add `nova-cr-name` (the Nova CR, waited on and
port-forwarded on 8774), a `neutron-cr-name` (waited on and port-forwarded on
9696), `placement-cr-name` (waited on and port-forwarded on 8778) and
`ovn-cr-name`. The last one is waited on and not forwarded: nothing outside the
cluster talks to the OVN databases. The Placement is forwarded because both
legs register a placement endpoint in their catalog on a cluster-internal name
and declare the service available in `tempest.conf`; a test that builds the
placement client would otherwise die on name resolution, which Tempest reports
as an error rather than a skip. The nova legs carry a
`glance-cr-name` too, for the images their servers boot from, and they are the
only legs that reach a console: the run step derives
`<nova-cr-name>-novncproxy` and the runner forwards it on 6080, which is what
`test_novnc_bad_token` dials. The neutron, cinder and nova legs all carry
`tempest-concurrency` (`2`), which the run step passes to
`hack/ci-run-tempest.sh` in place of its default of four workers. The
`keystone-upgrade` entry carries the five keys of the newest `keystone` entry
plus `upgrade-from`, the release its Keystone is deployed at before the upgrade.

| Service | Release | Config directory | Keystone CR (`cr-name` = `service-k8s-name`) | Glance CR (`glance-cr-name`) | Barbican CR (`barbican-cr-name`) | Neutron CR (`neutron-cr-name`) | Cinder CR (`cinder-cr-name`) | Nova CR (`nova-cr-name`) | OVNCentral (`ovn-cr-name`) | Placement CR (`placement-cr-name`) |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `keystone` | `2026.1` | `tests/tempest/keystone-2026-1` | `keystone-tempest-2026-1` | — | — | — | — | — | — | — |
| `keystone` | `2026.2` | `tests/tempest/keystone-2026-2` | `keystone-tempest-2026-2` | — | — | — | — | — | — | — |
| `keystone-upgrade` | `2026.2` (from `2026.1`) | `tests/tempest/keystone-2026-2` | `keystone-tempest-2026-2` | — | — | — | — | — | — | — |
| `glance` | `2026.1` | `tests/tempest/glance-2026-1` | `keystone-glance-tempest-2026-1` | `glance-tempest-2026-1` | — | — | — | — | — | — |
| `glance` | `2026.2` | `tests/tempest/glance-2026-2` | `keystone-glance-tempest-2026-2` | `glance-tempest-2026-2` | — | — | — | — | — | — |
| `barbican` | `2026.1` | `tests/tempest/barbican-2026-1` | `keystone-barbican-tempest-2026-1` | — | `barbican-tempest-2026-1` | — | — | — | — | — |
| `barbican` | `2026.2` | `tests/tempest/barbican-2026-2` | `keystone-barbican-tempest-2026-2` | — | `barbican-tempest-2026-2` | — | — | — | — | — |
| `neutron` | `2026.1` | `tests/tempest/neutron-2026-1` | `keystone-neutron-tempest-2026-1` | — | — | `neutron-tempest-2026-1` | — | — | `ovn-neutron-tempest-2026-1` | — |
| `neutron` | `2026.2` | `tests/tempest/neutron-2026-2` | `keystone-neutron-tempest-2026-2` | — | — | `neutron-tempest-2026-2` | — | — | `ovn-neutron-tempest-2026-2` | — |
| `cinder` | `2026.1` | `tests/tempest/cinder-2026-1` | `keystone-cinder-tempest-2026-1` | `glance-cinder-tempest-2026-1` | — | `neutron-cinder-tempest-2026-1` | `cinder-tempest-2026-1` | `nova-cinder-tempest-2026-1` | `ovn-cinder-tempest-2026-1` | `placement-cinder-tempest-2026-1` |
| `cinder` | `2026.2` | `tests/tempest/cinder-2026-2` | `keystone-cinder-tempest-2026-2` | `glance-cinder-tempest-2026-2` | — | `neutron-cinder-tempest-2026-2` | `cinder-tempest-2026-2` | `nova-cinder-tempest-2026-2` | `ovn-cinder-tempest-2026-2` | `placement-cinder-tempest-2026-2` |
| `nova` | `2026.1` | `tests/tempest/nova-2026-1` | `keystone-nova-tempest-2026-1` | `glance-nova-tempest-2026-1` | — | `neutron-nova-tempest-2026-1` | — | `nova-tempest-2026-1` | `ovn-nova-tempest-2026-1` | `placement-nova-tempest-2026-1` |
| `nova` | `2026.2` | `tests/tempest/nova-2026-2` | `keystone-nova-tempest-2026-2` | `glance-nova-tempest-2026-2` | — | `neutron-nova-tempest-2026-2` | — | `nova-tempest-2026-2` | `ovn-nova-tempest-2026-2` | `placement-nova-tempest-2026-2` |

**Step sequence:**

| Step | Description |
| --- | --- |
| Load E2E images | The `load-e2e-images` action pulls the images the `build-e2e-images` job pushed and re-tags them to their local references: the keystone-operator, `keystone:<release>` and the Tempest image, `ghcr.io/c5c3/tempest:<release>`, plus the operator and service images of the neutron, cinder and nova legs, and `keystone:<upgrade-from>` on the keystone-upgrade leg |
| Load images into kind | Loads operator and release-specific service images. On the keystone-upgrade leg `keystone:<release>` is the image the upgrade moves to |
| Load the previous release's Keystone image into kind *(keystone-upgrade leg only)* | Loads `keystone:<upgrade-from>`. kubelet skips the registry only for a tag already on the node, so without this load the node would pull the published `ghcr.io/c5c3/keystone:<upgrade-from>`, and the upgrade would start from main's image instead of this run's |
| Load compute stack images into kind *(nova and cinder legs)* | One `kind load docker-image` call for the four compute-stack operator images, the `neutron`, `placement` and `nova` service images, and the `ovn` daemon image at the resolved pin. One call rather than eight: each is a `docker save` piped into the node's containerd, and the three service images share their Nix store layers while the four operator images share their own base. Each leg's glance pair and tempest image — the latter for the catalog-setup, image-seed and flavor-seed Jobs that run in-cluster — come from a step above: the cinder leg's from its own, the nova leg's from the one it shares with the glance leg |
| Setup E2E infrastructure | The `setup-e2e-infra` action runs `make deploy-infra`. The `Deploy cinder operator` row names the `WITH_NFS`, `WITH_MESSAGING` and `WITH_OVN_KERNEL_MODULES` switches it sets per leg |
| Deploy operator (keystone) | `hack/ci-deploy-operator.sh` with `OPERATOR=keystone` |
| Deploy glance operator *(glance, cinder and nova legs)* | `hack/ci-deploy-operator.sh` with `OPERATOR=glance`, `NAMESPACE=glance-system` (before the Keystone CR reconciles). The cinder leg needs it for the image service its volume tests create volumes from, the nova leg for the one its servers boot from |
| Deploy barbican operator *(barbican leg only)* | `hack/ci-deploy-operator.sh` with `OPERATOR=barbican`, `NAMESPACE=barbican-system`, and `BARBICAN_SECRET_STORE_GRANTS=openstack=openbao-instance-provisioner` (the managed store lives in `openstack`, and the operator mints its provisioner token only where the chart granted it a TokenRequest Role — restricted to that one account, because `openstack` also hosts the accounts that read credentials out of OpenBao) |
| Deploy ovn operator, Deploy neutron operator *(neutron, nova and cinder legs)* | Two `hack/ci-deploy-operator.sh` runs: `OPERATOR=ovn`, `NAMESPACE=ovn-system` and `OPERATOR=neutron`, `NAMESPACE=neutron-system`. A Neutron never reaches Ready without a live OVNCentral, whose published database addresses its controller reads before it renders `ml2_conf.ini`. Both compute-stack legs run a Neutron of their own, so the same two runs cover them |
| Deploy cinder operator *(cinder leg only)* | `hack/ci-deploy-operator.sh` with `OPERATOR=cinder`, `NAMESPACE=cinder-system`. The `Setup E2E infrastructure` step ahead of it sets `WITH_NFS` on this leg alone and `WITH_MESSAGING` on it and the nova leg, which is what puts the NFS export and the `shared-rabbitmq` broker on the cluster the CinderBackend, the Cinder and both legs' Nova need. `WITH_OVN_KERNEL_MODULES` is set on the nova leg alone, where a real OVN chassis opens a Geneve tunnel off the kind node |
| Deploy placement operator, Deploy nova operator *(nova and cinder legs)* | Two `hack/ci-deploy-operator.sh` runs, into `placement-system` and `nova-system`. A Nova claims its inventory in a Placement, so both watch before the CRs below are applied. The glance-, ovn- and neutron-operator these legs also need come from the rows above, whose conditions cover them |
| Deploy Keystone CR for Tempest *(every leg but keystone-upgrade)* | Applies `matrix.config-dir/00-keystone-cr.yaml`, waits for `matrix.cr-name` Ready |
| Deploy Keystone CR at the previous release *(keystone-upgrade leg only)* | Renders `matrix.config-dir/00-keystone-cr.yaml` with `spec.image.tag` set to `matrix.upgrade-from` (`yq` with `strenv()`, so the tag stays a string), applies it and waits 300 s for Ready. It then waits 60 s for `status.installedRelease` to read `upgrade-from` and otherwise fails with an `::error::` naming the value it found: a Keystone synced fresh at the newest release runs no upgrade, and Tempest would test a fresh database |
| Upgrade Keystone to the leg's release *(keystone-upgrade leg only)* | Patches `spec.image.tag` to `matrix.release` and waits 600 s for `status.installedRelease` to follow, failing with an `::error::` that points at the `DatabaseReady` condition in the diagnostics dump. Then it waits 60 s for the `<cr>-db-expand`, `<cr>-db-migrate` and `<cr>-db-contract` Jobs to complete. The status write that moves `installedRelease` reports Ready before the operator re-runs the `<cr>-db-sync` and `<cr>-schema-check` Jobs at the new image, so the step polls 300 s for `<cr>-schema-check` to run at `:<release>` and waits 300 s for it to complete, each failing with an `::error::`. That Job runs `keystone-manage db_sync --check`, so a schema left behind a head fails the step before Tempest runs. Then the step waits 300 s for Ready and 300 s for the rollout of the three API replicas, and fails with an `::error::` when the Deployment's image does not end in `:<release>` |
| Bootstrap image catalog *(glance leg only)* | Applies `matrix.config-dir/01-catalog-setup-job.yaml`, waits for the `glance-tempest-catalog-setup` Job to complete (registers the image service + endpoints in Keystone that the Glance CR needs to reconcile) |
| Deploy Glance CR for Tempest *(glance leg only)* | Applies `matrix.config-dir/02-glance-cr.yaml`, `03-glancebackend-cr.yaml`, and `04-glancebackend2-cr.yaml`, waits for `matrix.glance-cr-name` Ready |
| Bootstrap key-manager catalog *(barbican leg only)* | Applies `matrix.config-dir/01-catalog-setup-job.yaml`, waits for the `barbican-tempest-catalog-setup` Job to complete (registers the key-manager service + endpoints in Keystone that the Barbican CR needs to reconcile, plus the `key-manager:service-admin` role the plugin's quota tests assign via dynamic credentials) |
| Deploy Barbican CR for Tempest *(barbican leg only)* | Applies `matrix.config-dir/02-barbican-cr.yaml` and `03-barbicansecretstore-cr.yaml`, waits for `barbicansecretstore/<barbican-cr-name>-store` Ready first, then for `matrix.barbican-cr-name` Ready. Barbican renders no config and never starts without a credential-ready default store, so the separate wait keeps a store-side failure from surfacing as an opaque Barbican timeout |
| Verify key-manager policy defaults *(barbican leg only)* | Applies `matrix.config-dir/04-policy-check-job.yaml`, waits 300 s for the `barbican-tempest-policy-check` Job to complete. A user holding only `member` on a probe project must get 201 creating a secret and 204 deleting it, and a user holding only `key-manager:service-admin` must get 403 listing project quotas. Every plugin API test runs as `project_admin`, which the legacy and the secure-RBAC rules both accept, so the Tempest run alone cannot show that `enforce_new_defaults` took effect |
| Bootstrap network catalog *(neutron leg only)* | Applies `matrix.config-dir/01-catalog-setup-job.yaml`, waits 300 s for the `neutron-tempest-catalog-setup` Job to complete (registers the network service + endpoints in Keystone that the Neutron CR authenticates against) |
| Deploy OVNCentral for Tempest *(neutron leg only)* | Applies `matrix.config-dir/02-messaging-secret.yaml` and `03-ovncentral-cr.yaml`, waits 300 s for `ovncentral/<matrix.ovn-cr-name>` Ready. `ovn-cr-name` is emitted by the matrix generator, like every other CR name this job waits on |
| Deploy Neutron CR for Tempest *(neutron leg only)* | Applies `matrix.config-dir/04-neutron-cr.yaml`, waits 600 s for `matrix.neutron-cr-name` Ready. The longer timeout covers the db-sync Job on top of the api and rpc-worker Deployments |
| Bootstrap block-storage catalog *(cinder leg only)* | Applies `matrix.config-dir/01-catalog-setup-job.yaml`, waits 300 s for the `cinder-tempest-catalog-setup` Job to complete. It registers the block-storage, image, compute, placement and network services with their endpoints: the Cinder authenticates against the first and resolves Glance from the second, and the compute stack resolves its siblings from the rest. It also grants the `service` role to the bootstrap admin the leg's CRs use as their service user, because cinder accepts nova-compute's `attachment_delete` on server delete only from a service token carrying one of its `service_token_roles` (a caller without it gets `409 ConflictNovaUsingAttachment` and the volume stays `in-use`); the ControlPlane grants the same role to every service account it registers |
| Bootstrap compute catalog *(nova leg only)* | Applies `matrix.config-dir/01-catalog-setup-job.yaml`, waits 300 s for the `nova-tempest-catalog-setup` Job to complete. It registers the compute, placement, image and network services with a public and an internal endpoint each: the Nova resolves its siblings on the internal interface, and tempest reads the public rows. From 2026.2 on the Job also grants the `service` role to the bootstrap admin the leg's CRs use as their service user: keystonemiddleware 13.0.0 defaults `service_token_roles_required` to true, and nova sends a service token beside the user token on every call to Neutron and Glance, so without the grant every server show and delete fails with 401 |
| Apply the independent compute-stack CRs *(nova and cinder legs)* | Applies `*-messaging-secret.yaml`, `*-ovncentral-cr.yaml` and `*-placement-cr.yaml` and waits on nothing. Neither CR has an upstream in this set, so both reconcile while the cinder leg brings up its Glance, its Cinder and its seed image and while the nova leg waits out its OVNCentral and its Neutron, instead of after them. The steps below re-apply the same manifests, which is a no-op, and then wait. The Neutron is deliberately not applied here: its controller error-requeues until the OVNCentral publishes its database addresses |
| Deploy Glance CR for the cinder leg *(cinder leg only)* | Applies `matrix.config-dir/02-glance-cr.yaml` and `03-glancebackend-cr.yaml`, waits 300 s for `matrix.glance-cr-name` Ready. The seed image below is uploaded to it |
| Deploy Cinder CR for Tempest *(cinder leg only)* | Applies `matrix.config-dir/04-cinderbackend-cr.yaml`, `05-cinderbackupbackend-cr.yaml` and `06-cinder-cr.yaml`, waits 600 s for `matrix.cinder-cr-name` Ready. The longer timeout covers the db-sync Job on top of the api, scheduler, volume and backup Deployments |
| Seed the image the volume tests boot from *(cinder leg only)* | Applies `matrix.config-dir/07-image-seed-job.yaml`, waits 300 s for the `cinder-tempest-image-seed` Job to complete. It uploads the 16 MiB raw image whose fixed UUID `tempest.conf` pins as `[compute] image_ref`, which every create-volume-from-image case reads |
| Deploy OVNCentral for the compute stack *(nova and cinder legs)* | Applies `*-messaging-secret.yaml` and `*-ovncentral-cr.yaml`, waits 300 s for `ovncentral/<matrix.ovn-cr-name>` Ready. The Neutron renders no `ml2_conf.ini` without the published database addresses, so an OVN-side failure surfaces here rather than as an opaque Neutron timeout |
| Deploy Neutron CR for the compute stack *(nova and cinder legs)* | Applies `*-neutron-cr.yaml`, waits 600 s for `matrix.neutron-cr-name` Ready, the timeout the neutron leg's own step uses for the same db-sync Job |
| Deploy Placement CR *(nova and cinder legs)* | Applies `*-placement-cr.yaml`, waits 300 s for `matrix.placement-cr-name` Ready. The Placement holds the inventory the compute publishes and the scheduler reads, so it is Ready before the Nova is applied |
| Deploy Glance CR for the nova leg *(nova leg only)* | Applies `matrix.config-dir/06-glance-cr.yaml` and `07-glancebackend-cr.yaml`, waits 300 s for `matrix.glance-cr-name` Ready. The seed Job below uploads to it |
| Seed the images the compute tests boot from *(nova leg only)* | Applies `matrix.config-dir/08-image-seed-job.yaml`, waits 300 s for the `nova-tempest-image-seed` Job to complete. It creates the two 1 MiB raw images whose fixed UUIDs `tempest.conf` pins as `[compute] image_ref` and `image_ref_alt`, the second of which the rebuild tests swap a running server onto |
| Deploy Nova CR *(nova and cinder legs)* | Applies `*-metadata-secret.yaml` and `*-nova-cr.yaml`, waits 900 s for `matrix.nova-cr-name` Ready. The first CI run of the nova e2e leg put Ready about 8 minutes after the apply: 3 minutes for the eight MariaDB CRs at the operator's 30 s requeue and 3 more for the six `nova-manage` runs of the db-sync |
| Deploy the fake compute *(nova and cinder legs)* | Applies `*-fake-compute.yaml`, waits 300 s for the `<matrix.nova-cr-name>-fake-compute` Deployment to roll out, then runs `tests/e2e/nova/discover-hosts.sh` with the leg's host: `fake-1` on the cinder legs, the kind node's name on the nova legs. nova-scheduler places a server only on a host that has a mapping in the API database, and it writes those mappings from a 300-second periodic |
| Deploy the OVN chassis *(nova leg only)* | Labels the node `openstack.c5c3.io/chassis=true`, applies `matrix.config-dir/12-ovnchassis-cr.yaml`, waits 300 s for `ovnchassis/ovn-<matrix.nova-cr-name>-chassis` Ready. Without a live chassis row in the Southbound database every server the suite boots ends in ERROR on a binding failure |
| Deploy the metadata agent *(nova leg only)* | Applies `matrix.config-dir/13-neutronmetadataagent-cr.yaml`, waits 300 s for `neutronmetadataagent/neutron-<matrix.nova-cr-name>-agent` Ready. The agent answers the 169.254.169.254 requests the servers on that chassis make, which is what the `metadata_service` tests read back |
| Seed the flavors *(nova and cinder legs)* | Applies `*-flavor-seed-job.yaml`, waits 300 s for the `<matrix.service>-tempest-flavor-seed` Job to complete. It creates `m1.nano` and `m1.micro`, the two ids `tempest.conf` pins as `[compute] flavor_ref` and `flavor_ref_alt`. It talks to the Nova API, so it comes after the Nova is Ready |
| Run Tempest API tests | `hack/ci-run-tempest.sh` with `CONFIG_DIR`, `TEMPEST_IMAGE`, and `SERVICE_K8S_NAME` from matrix; the barbican leg also passes `BARBICAN_K8S_NAME`, the cinder leg `CINDER_K8S_NAME`, the neutron, cinder and nova legs `NEUTRON_K8S_NAME`, the glance, cinder and nova legs `GLANCE_K8S_NAME`, the nova and cinder legs `NOVA_K8S_NAME` and `PLACEMENT_K8S_NAME`, and the nova legs `NOVA_CONSOLE_K8S_NAME` (`<nova-cr-name>-novncproxy`). Each is empty on the other legs, which disables the matching 9292, 9311, 9696, 8776, 8774, 8778 or 6080 port-forward |
| Upload Tempest results | Uploads `_output/tempest/` (minus the rendered `tempest.conf`, which carries the substituted admin password) as artifact with 14-day retention |
| Dump diagnostic info (compute stack) *(nova and cinder legs)* | `hack/ci-dump-diagnostics.sh` under `always()`, once per extra operator with `OPERATOR_ONLY=1`: `nova`, `placement`, `ovn`, `neutron`, `glance`, plus `cinder` on the cinder legs. Each of those lives in a Namespace the keystone dump above never reads |

The compute-stack steps name their fixtures by suffix glob
(`matrix.config-dir/*-ovncentral-cr.yaml`), unquoted so the shell expands it,
because the numeric prefixes differ between `tests/tempest/nova-<slug>/` and
`tests/tempest/cinder-<slug>/`, where the volume fixtures come first. Each
pattern matches one file in either directory, and a pattern that matches
nothing reaches `kubectl` as its own literal text, which fails the step instead
of skipping it.

**Job wall:** `timeout-minutes` is 68 on most legs, 150 on the nova legs and
120 on the cinder legs. Run 35083301017 measured the cinder 2025.2 leg at 56
minutes, 40 of them bring-up, and the Phase-0 lab of #1015 ran the compute
suite in 42 minutes at two workers. Both raised numbers are estimates;
re-derive each from the first green run of its leg. Run 39564032378 measured
the fresh keystone 2026.2 leg at 45.6 minutes: 8.7 in `Setup E2E
infrastructure`, 40 s in `Deploy Keystone CR for Tempest` and 33.1 in the
Tempest run. The keystone-upgrade leg adds one image pull and kind load plus the
upgrade, an estimated 5 minutes, which puts it near 51 of its 68.

**CI-specific adaptations** (compared to local execution):

| Aspect | Local (`hack/run-tempest.sh`) | CI (`hack/ci-run-tempest.sh`) |
| --- | --- | --- |
| Service endpoint | Port-forwarded to `localhost:5000` (reuses a forward that already answers there; `host.docker.internal` on macOS) | Port-forwarded to `localhost:5000` |
| Glance endpoint | Not supported (local runs are keystone-only) | When `GLANCE_K8S_NAME` is set (glance, cinder and nova legs), additionally forwards `svc/<glance-cr-name>` on 9292, polls its `/healthcheck`, and add-hosts its cluster DNS names to `127.0.0.1` so the catalog's image endpoint resolves to the forwarded port |
| Barbican endpoint | Not supported (local runs are keystone-only) | When `BARBICAN_K8S_NAME` is set (barbican legs), additionally forwards `svc/<barbican-cr-name>` on 9311, polls its `/healthcheck`, and add-hosts its cluster DNS names to `127.0.0.1` so the catalog's key-manager endpoint resolves to the forwarded port |
| Neutron endpoint | Not supported (local runs are keystone-only) | When `NEUTRON_K8S_NAME` is set (neutron, cinder and nova legs), the script also forwards `svc/<neutron-cr-name>` on 9696, polls its root path, and add-hosts its cluster DNS names to `127.0.0.1` so the catalog's network endpoint resolves to the forwarded port |
| Cinder endpoint | Not supported (local runs are keystone-only) | When `CINDER_K8S_NAME` is set (cinder legs), the script also forwards `svc/<cinder-cr-name>` on 8776, polls its `/healthcheck`, and add-hosts its cluster DNS names to `127.0.0.1` so the catalog's block-storage endpoint resolves to the forwarded port |
| Nova endpoint | Not supported (local runs are keystone-only) | When `NOVA_K8S_NAME` is set (nova and cinder legs), the script also forwards `svc/<nova-cr-name>` on 8774, polls its root path, and add-hosts its cluster DNS names to `127.0.0.1` so the catalog's compute endpoint resolves to the forwarded port. Nova registers no healthcheck middleware, so the poll asks for `/`, the path the operator probes |
| Nova console endpoint | Not supported (local runs are keystone-only) | When `NOVA_CONSOLE_K8S_NAME` is set (nova legs), the script also forwards `svc/<nova-cr-name>-novncproxy` on 6080, polls its `/vnc_lite.html`, and add-hosts its cluster DNS names to `127.0.0.1`. `test_novnc_bad_token` dials the `novncproxy_base_url` nova hands out, which the operator renders as the cluster-local Service URL when no gateway is set. That name is in no catalog |
| Placement endpoint | Not supported (local runs are keystone-only) | When `PLACEMENT_K8S_NAME` is set (nova and cinder legs), the script also forwards `svc/<placement-cr-name>` on 8778, polls its root path, and add-hosts its cluster DNS names to `127.0.0.1` so the catalog's placement endpoint resolves to the forwarded port. Placement registers no healthcheck middleware either, so the poll asks for `/`, the path the operator probes |
| Credential injection | `sed` substitution into generated config copy | `sed` substitution into generated config copy |
| Tempest image | Built locally from the flake (`nix build .#tempest-<rel_>-image`) | Built by the `Build Nix images` step of the `build-e2e-images` job, or pulled by the digest main published |
| Artifact upload | Manual inspection of `_output/` | `actions/upload-artifact` with `tempest-<service>-<release>-results` name (`tempest-keystone-upgrade-2026.2-results` for the keystone-upgrade leg) |

**Artifact name:** `tempest-<service>-<release>-results` (e.g., `tempest-keystone-2026.1-results`, `tempest-glance-2026.2-results`, `tempest-keystone-upgrade-2026.2-results`)
**Retention:** 14 days

#### Keystone upgrade leg

The `keystone-upgrade` entry runs the newest release's keystone suite against a
Keystone that reached that release by an in-place upgrade. Its Keystone comes up
at `upgrade-from`, so the previous release's `db_sync` and bootstrap write the
database. The keystone-operator then moves it to `release` through the
`<cr>-db-expand` and `<cr>-db-migrate` Jobs, a rolling update of the three API
replicas and the `<cr>-db-contract` Job. The fresh `keystone` entries only see a
database that `keystone-manage db_sync` created at one release. The
[`release-upgrade`](./keystone-e2e-tests.md#release-upgrade) Chainsaw suite
checks `/v3`, the admin token and the continuity of a token across the same
hop, and does not exercise the identity API beyond that.

No data is seeded before the upgrade. The rows that pass through the migration
are the ones the operator's bootstrap writes: the `Default` domain, the `admin`
user and project, the `admin`, `member` and `reader` roles, `RegionOne`, and the
identity service with its admin, internal and public endpoints. Tempest's
dynamic credentials create everything else after the upgrade, on the new
schema.

The generator takes the pair from the last two `releases/*/` directories in
glob order. That is the newest transition, the one the L6 check of the
`check-release-wiring` audit holds every upgrade suite to, and adding a release
moves the entry to the new transition without a workflow edit. The entry reuses the newest
release's `tests/tempest/keystone-<slug>/` directory and Keystone CR name, so
both keystone entries of that release run the same `include-tests.txt` and
`exclude-tests.txt`.

A test that fails on `keystone-upgrade` and passes on the fresh `keystone` entry
of the same release in the same run is a finding against the migration. Report
it with the test id and the traceback from `tempest-results.xml` in the
`tempest-keystone-upgrade-<release>-results` artifact. Keep it out of
`exclude-tests.txt`: the fresh entry reads the same file and would lose the test
too.

### build-images.yaml — build-tempest Job

Builds the Tempest container image per release and per platform, runs verification on PRs,
and pushes by digest on push events. The job is parameterized by release via the
`generate-matrix` job, which discovers all releases from `releases/*/` directories.

**Dependencies:** `needs: [changes, generate-matrix, nix-flake-check, nix-lock-check, prepare]`

**Condition:** `needs.changes.outputs.build-tempest == 'true'`. On a pull request the
job runs when `images/tempest/**`, the tempest locks under `nix/locks/`,
`tests/container-images/verify_tempest.sh`, or one of the flake and
release-configuration inputs changed; on a push it always runs.
See [the changes job](/reference/ci-cd/build-images-workflow#changes).

**Matrix strategy:** `release × platform × runner` (from `generate-matrix.tempest-matrix`; ARM64 excluded on PRs)

**Step sequence:**

| Step | Condition | Description |
| --- | --- | --- |
| Set up Nix | Always | The `setup-nix` action: Nix and the `cobaltcore` cache |
| Resolve flake attribute | Always | `tempest-<release with _>-image`, because a flake attribute cannot contain a dot |
| Build Tempest image | PR: amd64 only; push: both platforms | The `build-nix-image` action: `nix build` of the attribute and a vulnix scan of its store paths (artifact `vulnix-tempest-<release>-<platform-pair>`) |
| Push and export digest | Push only | skopeo pushes the stream by digest; artifact `digests-tempest-<release>-<platform-pair>` |
| Scan for vulnerabilities | PR, amd64 only | Grype scan against the loaded image |
| Upload SARIF | Skipped | The composite uploads SARIF on push events only; `merge-tempest-image` publishes `grype-tempest-<release>` |
| Verify Tempest image | PR, amd64 only | Runs `verify_tempest.sh` against the loaded image |

**PR vs push behavior:**

| Behavior | Pull Request | Push to main/stable |
| --- | --- | --- |
| Platforms built | amd64 only | amd64 + arm64 |
| Image destination | Loaded locally (`docker load`) | Pushed by digest to GHCR |
| Verification | `verify_tempest.sh` runs | Deferred to `merge-tempest-image` |
| Vulnerability scan | Against local image | Against SBOM (in merge job) |
| SARIF upload | None | `grype-tempest-<release>` (in merge job) |

### build-images.yaml — merge-tempest-image Job

Assembles per-platform digests into a multi-arch manifest list with full supply chain
security. The job is parameterized by release via the `generate-matrix` job.

**Dependencies:** `needs: [build-tempest, generate-matrix, prepare]`
**Condition:** `if: github.event_name != 'pull_request'`
**Matrix strategy:** `release` (from `generate-matrix.tempest-release-matrix`)

**Permissions:**

| Permission | Purpose |
| --- | --- |
| `contents: read` | Repository checkout |
| `packages: write` | Push manifest list to GHCR |
| `id-token: write` | Sigstore OIDC signing for cosign and build provenance |
| `attestations: write` | GitHub Attestations API |
| `security-events: write` | SARIF upload to GitHub Security tab |

**Image tags applied:**

| Tag | Example | Description |
| --- | --- | --- |
| `<release>` | `ghcr.io/<owner>/tempest:2026.1` | Release series tag |
| `<tempest-version>` | `ghcr.io/<owner>/tempest:46.3.0` | Tempest PyPI version (main branch only) |
| `<release>-<commit-sha>` | `ghcr.io/<owner>/tempest:2026.1-<sha>` | Release + git commit for traceability |

**Supply chain security steps:**

| Step | Tool | Output |
| --- | --- | --- |
| SBOM generation | `anchore/sbom-action` | `sbom-tempest-<release>.cyclonedx.json` (CycloneDX format) |
| Vulnerability scan | `anchore/scan-action` (Grype) | SARIF uploaded to GitHub Security tab (`grype-tempest-<release>`) |
| SBOM attestation | `actions/attest` | Signed attestation pushed to GHCR registry |
| Image signing | `sigstore/cosign` | Keyless signature via Sigstore OIDC |

### nix-flake-check

The Tempest image has no Dockerfile. Its expression
`images/tempest/image.nix` is linted by the `nix-lint` check that `nix-flake-check`
builds (nixfmt, statix and deadnix), and `build-tempest` needs that job and
`nix-lock-check` ([Lint](../ci-cd/nix-image-build.md#lint)).

## Data Flow (CI End-to-End)

The figure shows where the parts of one job run: the Tempest container on the
runner host, the port-forwards and host aliases that take it to the Services,
and the seed Jobs inside the cluster. The second figure lists the commands in
the order the job runs them.

![The test bed of one tempest job in CI. On the runner host, the step Run Tempest API tests runs hack/ci-run-tempest.sh. The script reads the admin password from the Secret keystone-admin in the namespace openstack of the kind cluster, starts one kubectl port-forward per Service on localhost and, once every port answers, starts the Tempest container with docker run on the host network. The container mounts the directory with the rendered tempest.conf and the test lists at /etc/tempest and the output directory at /output. It calls the APIs over plain http: --add-host points the cluster DNS names of the Services at 127.0.0.1, where the forwards listen. Every job forwards the Keystone Service on port 5000. Depending on the service under test, further forwards reach Glance on 9292, Barbican on 9311, Neutron on 9696, Cinder on 8776, Nova on 8774, the noVNC proxy on 6080 and Placement on 8778. Inside the cluster, seed Jobs run the same Tempest image and call the APIs by their cluster DNS names: a catalog set-up Job, an image seed Job, a flavor seed Job and, for Barbican, a policy check Job.](../../diagrams/test-tempest-bed.svg)

![The commands of one tempest job in CI, in six steps. hack/nix-lock.sh locks releases/<release>/test-refs.yaml into nix/locks/<release>/tempest/, the committed pins of tempest and the four plugins. The build-e2e-images job runs hack/ci-nix-build-e2e-images.sh per release, which runs nix build .#tempest-<rel_>-image in the nixos/nix container and docker load of .nix-e2e/tempest-<release>.tar. hack/ci-run-tempest.sh runs per matrix.service and matrix.release: it reads the admin password from the keystone-admin Secret, port-forwards svc/<service-k8s-name> to 5000 plus one forward per <SERVICE>_K8S_NAME the leg sets, each polled on localhost for up to 10 attempts, renders tempest.conf with the localhost URI and the password into _output/tempest/config next to the copied exclude-tests.txt and runner scripts, and splits include-tests.txt into phases/phase-1-core.txt and phases/phase-2-plugin.txt. docker run starts ghcr.io/c5c3/tempest:<release> with --rm, --network host, one --add-host pair per forwarded Service mapping <name>.openstack.svc.cluster.local and <name>.openstack.svc to 127.0.0.1, the config directory mounted read-only at /etc/tempest, _output/tempest mounted at /output and the TEMPEST_CONCURRENCY, TEMPEST_GROUP_START, TEMPEST_GROUP_END and TEMPEST_ERROR_PREFIX variables, and runs bash /etc/tempest/run-tests.sh. run-tests.sh runs tempest init, copies tempest.conf, runs stestr for the core phase and the plugin phase with subunit output, concatenates the streams and converts them with subunit2junitxml, retries the failed tests once serially from the list extract-failed.py prints, and merge-retry-junit.py rewrites the resolved failures in tempest-results.xml. actions/upload-artifact uploads tempest-<service>-<release>-results with a 14-day retention. The runner scripts never inline the steps of run-tests.sh, so CI and local runs share one code path.](../../diagrams/test-tempest-job-steps.svg)

## Dependencies on Prior Features

| Artifact | Used by Tempest infrastructure |
| --- | --- |
| `images/tempest/image.nix`, `nix/lib/service-image.nix` | The Nix expression of the Tempest image |
| `nix/locks/<release>/tempest/` | The lock the image's environment is built from |
| `releases/<release>/upper-constraints.txt` | Dependency constraints of the lock |
| `tests/lib/assertions.sh` | Assertion helpers sourced by `verify_tempest.sh` |
| SBOM attestation pattern in `build-images.yaml` | Reused by `merge-tempest-image` for Tempest SBOM |
| Cosign signing pattern in `build-images.yaml` | Reused by `merge-tempest-image` for Tempest signing |
| OCI labels of `nix/lib/service-image.nix` | The four static labels plus the tempest version |
| Grype scanning pattern in `build-images.yaml` | Reused by `build-tempest` and `merge-tempest-image` |

:::
