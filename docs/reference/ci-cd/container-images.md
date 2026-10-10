---
title: Container Images
quadrant: infrastructure
---

# Container Images

Reference documentation for the container image build system. This covers
the Dockerfile hierarchy, base image contents, release configuration file formats,
named build context patterns, constraint override tooling, and local build instructions.

## Dockerfile Hierarchy

The OpenStack service images follow a three-layer hierarchy. Each layer builds on the
previous one, separating concerns between runtime base, build tooling, and
service-specific code.

The figure follows one service image from the files of the repository to the
image: which file feeds which step before the build, which stage starts from
which base image, and what the two contexts and the three build args carry. The
table names both stages of every image.

![The build of a service image, from the files of the repository to the image. Before the build, the job resolves the git ref of the service from releases/{release}/source-refs.yaml, checks out openstack/{service} at that ref into src/{service} and applies the patches under patches/{service}/{release}. It applies overrides/{release}/constraints.txt to releases/{release}/upper-constraints.txt, which rewrites that file in place, and it turns the block of the image in releases/{release}/extra-packages.yaml into three build args. docker build then runs images/{service}/Dockerfile in two stages. The build stage starts from the venv-builder image and installs the source tree into /var/lib/openstack, with the constraints file, the pip extras and the pip packages. The runtime stage starts from the python-base image, copies /var/lib/openstack from the build stage and installs the apt packages. venv-builder is built on python-base, and python-base on ubuntu:noble. The nova-compute image is a second build for nova: its own Dockerfile with the same two stages, on the same source tree, patches and constraints, with the block nova-compute of extra-packages.yaml.](../../diagrams/ci-service-image-build.svg)

| Image | Build stage, on `venv-builder` | Runtime stage, on `python-base` |
| --- | --- | --- |
| `keystone` | installs Keystone into the virtualenv | copies the virtualenv, adds runtime apt packages |
| `horizon` | installs Horizon, pre-builds the static assets | copies the virtualenv and the static assets |
| `glance` | installs Glance, `glance_store[s3]` and `lhafile` | copies the virtualenv, adds runtime apt packages |
| `placement` | installs Placement, writes the WSGI entry | copies the virtualenv, adds runtime apt packages |
| `barbican` | installs Barbican into the virtualenv | copies the virtualenv, adds runtime apt packages |
| `neutron` | installs Neutron into the virtualenv | copies the virtualenv, adds runtime apt packages |
| `cinder` | installs Cinder into the virtualenv | copies the virtualenv, adds runtime apt packages |
| `nova` | installs Nova into the virtualenv. A stage `novnc`, on `python-base`, fetches the pinned noVNC tree | copies the virtualenv and noVNC, adds runtime apt packages |
| `nova-compute` | installs Nova and `libvirt-python`, built against `libvirt-dev` | copies the virtualenv, adds host tools and the rootwrap posture |

The `tempest` image builds on the same pair
([Container Image](../testing/tempest-test-infrastructure.md#container-image)).

The `venv-builder` image never runs in production. It is the `FROM` target of
the build stages and the container the service unit tests run in. Service images
(e.g., `keystone`) use a multi-stage build: stage 1 extends `venv-builder` to
install the service, then stage 2 extends `python-base` and copies only the
virtualenv from stage 1. This ensures the final image contains no build tools.

The release-independent images sit outside that lineage. They carry no
OpenStack code, and all but two build straight on `ubuntu:noble`:

```text
ubuntu:noble
├── ovn                        Stage 1 (build): compile OVS + OVN from pinned upstream git
├── ovn                        Stage 2 (runtime): copy binaries, schemas and ctl scripts, add runtime apt packages
├── keystone-federation-proxy  Single stage: distro apache2 + mod_auth_openidc + mod_auth_mellon
├── backup-shifter             Single stage: distro rclone
└── libvirt                    Single stage: distro libvirt + QEMU + OVMF
```

`openstack-hypervisor-operator` and `kvm-node-agent` compile Go programs and
build on the two images the CobaltCore operator images use:

```text
golang:1.27
├── openstack-hypervisor-operator  Stage 1 (build): fetch the pinned upstream commit, apply the patches, go test + go build
└── kvm-node-agent                 Stage 1 (build): fetch the pinned upstream commit, apply the patches, two test runs + go build
gcr.io/distroless/static:nonroot
├── openstack-hypervisor-operator  Stage 2 (runtime): copy the manager binary
└── kvm-node-agent                 Stage 2 (runtime): copy the manager binary, run as 0:0
```

Each is described under [Release-independent images](#release-independent-images).

## Base Images

### python-base

**Location:** `images/python-base/Dockerfile`

The foundational runtime image for all OpenStack service containers.

| Property | Value |
| --- | --- |
| Base image | `ubuntu:noble` (Ubuntu 24.04 LTS) |
| Python | 3.12 (from Ubuntu Noble package repository) |
| User | `openstack` (UID 42424, GID 42424, shell `/usr/sbin/nologin`) |
| Home directory | `/var/lib/openstack` |

**Environment variables:**

| Variable | Value | Purpose |
| --- | --- | --- |
| `PATH` | `/var/lib/openstack/bin:$PATH` | Ensures virtualenv binaries take precedence |
| `LANG` | `C.UTF-8` | Consistent locale for Python string handling |

**Runtime packages:**

| Package | Purpose |
| --- | --- |
| `ca-certificates` | TLS certificate verification |
| `netbase` | `/etc/protocols` and `/etc/services` for network operations |
| `python3` | Python 3.12 runtime |
| `sudo` | Privilege escalation for entrypoint scripts |
| `tzdata` | Timezone data for datetime operations |

**User convention:** All service images share a single `openstack` user (UID/GID 42424)
rather than creating per-service users. This is a deliberate deviation from the architecture
document — see [Design Deviations](#design-deviations) for rationale.

**OCI labels:** The Dockerfile includes static `LABEL` instructions for baseline
OCI Image Spec annotations (`title`, `description`, `licenses`, `vendor`). These are
always present on locally-built images. In CI, `docker/metadata-action` supplements these
with dynamic labels (created, revision, source, url, version) — see
[Build Images Workflow — OCI Annotations](build-images-workflow.md#oci-annotations).

### venv-builder

**Location:** `images/venv-builder/Dockerfile`

Build-stage image that extends `python-base` with compilation tools and a prepared
Python virtualenv. This image is never deployed. It is the `FROM` target of the service
build stages and the container the service unit tests run in.

| Property | Value |
| --- | --- |
| Base image | `python-base` (local) |
| Package manager | `uv` 0.12.5 (copied from the digest-pinned `ghcr.io/astral-sh/uv:0.12.5`; tracked by Renovate) |
| Virtualenv path | `/var/lib/openstack` |

**Build-time packages:**

| Package | Purpose |
| --- | --- |
| `build-essential` | C compiler and make (for building Python C extensions) |
| `git` | Fetching Python packages from git repositories |
| `libffi-dev` | cffi/cryptography compilation |
| `libldap2-dev` | OpenLDAP development headers and libraries |
| `libpq-dev` | psycopg2 compilation (PostgreSQL client) |
| `libsasl2-dev` | Cyrus SASL development headers and libraries |
| `libssl-dev` | cryptography/pyOpenSSL compilation |
| `python3-dev` | Python headers for C extensions |
| `python3-venv` | `venv` module for virtualenv creation |
| `qemu-utils` | `qemu-img`, which cinder's `TestFormatInspectors` shell out to when they build their fixture images. Not a build dependency: this container also runs the service unit tests |

**Pre-installed common packages:**

The virtualenv includes five packages shared by all OpenStack services, version-pinned in
`images/venv-builder/requirements.txt`:

| Package | Purpose |
| --- | --- |
| `cryptography` | TLS, token encryption, Fernet keys |
| `pymemcache` | Memcached client (pure-Python `pymemcache` backend) |
| `pymysql` | MySQL/MariaDB database driver |
| `python-memcached` | Memcached client for caching |
| `uwsgi` | WSGI application server |

These packages are **version-pinned** in `requirements.txt` so the `venv-builder` image is
reproducible — without pins they would resolve to whatever is latest on PyPI at build time.
The image stays release-independent: the pins are deliberately not taken from any single
release's `upper-constraints.txt`. The OpenStack-dependency subset (`cryptography`,
`pymemcache`, `pymysql`, `python-memcached`) is authoritatively re-pinned per release by
service Dockerfiles via `uv pip install --constraint upper-constraints.txt`; `uwsgi` is not
an OpenStack dependency (it is absent from `upper-constraints.txt`), so its version is fixed
here. Renovate tracks these pins through its native `pip_requirements` manager — major bumps
are gated for manual review, minor/patch are automerged after a three-day soak.

**OCI labels:** Same static `LABEL` pattern as `python-base` — title, description,
licenses, and vendor are embedded in the Dockerfile for local build visibility.

## Service Images

CI builds, verifies and publishes these images through the
[Build Images Workflow](build-images-workflow.md). The figure shows that
workflow on a pull request and on a push.
[PR vs Push Behavior](build-images-workflow.md#pr-vs-push-behavior) lists the
differences aspect by aspect.

![The image build workflow on its two paths. On every run, build-base-images builds python-base and venv-builder for amd64 and arm64 and pushes them by digest, merge-base-images joins each pair into one manifest and pushes it to ghcr.io, verify-base-images checks both, and generate-matrix works out the services, releases and platforms to build. On a pull request, build-service-images builds each service image for linux/amd64 only and loads it into the local Docker daemon, scans it with Grype, runs its verify script and checks the option catalog. test-service-images runs the upstream unit tests. The run ends there, and no service image is pushed. On a push to main or stable/**, or on a manual run, build-service-images builds for amd64 and arm64, each on its own runner, pushes by digest and exports the digest. merge-service-images creates and pushes the manifest with its tags, generates an SBOM, scans the SBOM with Grype and uploads the report, attests the SBOM and the build provenance, and signs the image with cosign. verify-service-images pulls the image from ghcr.io and runs the verify script and the option catalog check, once merge-service-images and test-service-images have passed.](../../diagrams/ci-image-publish.svg)

### keystone

**Location:** `images/keystone/Dockerfile`

The Keystone identity service image uses a two-stage build:

**Stage 1 (`build`)** — extends `venv-builder`:

- Declares `ARG PIP_EXTRAS` and `ARG PIP_PACKAGES` for build-time injection of extras
  and additional packages from `extra-packages.yaml` (passed by the CI workflow)
- Mounts `upper-constraints.txt` and the Keystone source tree via named build contexts
- Installs Keystone with extras into the virtualenv using `uv pip install --constraint`

**Stage 2 (runtime)** — extends `python-base`:

- Declares `ARG EXTRA_APT_PACKAGES` for build-time injection of runtime system packages
  from `extra-packages.yaml` (passed by the CI workflow)
- Copies `/var/lib/openstack` from the build stage using `COPY --from=build --link`
  (the `--link` flag enables parallel layer extraction and deduplication)
- Installs runtime system packages via `apt-get install ${EXTRA_APT_PACKAGES}`
- Sets `USER openstack` for non-root execution

**Runtime packages:**

| Package | Purpose |
| --- | --- |
| `libapache2-mod-wsgi-py3` | Apache WSGI module for serving Keystone |
| `libldap2` | LDAP client library (python-ldap runtime dependency) |
| `libsasl2-2` | SASL authentication library (LDAP SASL bind support) |
| `libxml2` | XML parsing library (lxml runtime dependency) |

**Final image properties:**

- Runs as `openstack` user (UID 42424, GID 42424)
- Contains no build tools (`gcc`, `python3-dev`, `build-essential`, `uv` are absent)
- Virtualenv at `/var/lib/openstack` with all Keystone dependencies
- `keystone-manage` CLI available via `PATH`

**OCI labels:** The `LABEL` instruction is placed in Stage 2 (runtime) before
the `USER` instruction. Labels added in Stage 1 (build) are discarded by Docker's
multi-stage build process — only the runtime stage labels appear on the final image. In
CI, `docker/metadata-action` overrides `org.opencontainers.image.version` with the
upstream OpenStack release version from `source-refs.yaml` via a `type=raw` tag strategy.

### horizon

**Location:** `images/horizon/Dockerfile`

The Horizon dashboard image uses the same two-stage build as Keystone, with two
horizon-specific twists: static assets are pre-built at image-build time, and the
`horizon===` pin in `upper-constraints.txt` must be stripped before the build
(see [Constraint Overrides](#constraint-overrides)).

**Stage 1 (`build`)** — extends `venv-builder`:

- Declares `ARG PIP_EXTRAS` and `ARG PIP_PACKAGES` (both empty for horizon today;
  the wiring mirrors keystone so `extra-packages.yaml` stays the single edit point)
- Mounts `upper-constraints.txt` and the Horizon source tree via named build contexts
  (`--build-context horizon=...` / `--build-context upper-constraints=...`)
- Installs Horizon into the virtualenv using `uv pip install --constraint`
- Pre-builds static assets: a throwaway `local_settings.py` is written into the
  installed `openstack_dashboard/local/` package, `collectstatic --noinput` and
  `compress --force` (django-compressor offline compression) run against it, and the
  throwaway file is removed. Assets land in `/var/lib/openstack/horizon-static` with
  the offline manifest at `dashboard/manifest.json`

**Stage 2 (runtime)** — extends `python-base`:

- Declares `ARG EXTRA_APT_PACKAGES`, which carries `libpython3.12t64`: the
  venv-builder-compiled uwsgi binary links `libpython3.12.so.1.0`, which python-base
  does not ship. The dashboard is otherwise pure Python; the pymemcache session-cache
  client comes from the venv-builder base venv
- Copies `/var/lib/openstack` (virtualenv plus pre-built static assets) from the build
  stage using `COPY --from=build --link`
- Creates `/etc/openstack-dashboard/` and symlinks the packaged
  `openstack_dashboard/local/local_settings.py` to
  `/etc/openstack-dashboard/local_settings.py`, where the horizon-operator mounts the
  rendered Django settings ConfigMap. The symlink dangles at build time by design
- Sets `USER openstack` for non-root execution

**Runtime packages:**

| Package | Purpose |
| --- | --- |
| `libpython3.12t64` | Shared `libpython3.12.so.1.0` for the venv-builder-compiled uwsgi |

**Final image properties:**

- Runs as `openstack` user (UID 42424, GID 42424)
- Contains no build tools (`gcc`, `python3-dev`, `build-essential`, `uv` are absent)
- Serves via uWSGI loading `openstack_dashboard.wsgi` directly (the module ships
  `application`) — no hand-written wsgi script, and static assets are served through
  `uwsgi --static-map /static=/var/lib/openstack/horizon-static`
- i18n message catalogs are not compiled (`compilemessages` needs gettext at build
  time); the dashboard renders in English. Deferred until a locale requirement lands

**Unit tests:** horizon ships no `.stestr.conf` — its Django suite runs under pytest.
`hack/ci-run-unit-tests.sh` branches on `.stestr.conf` presence and delegates to
horizon's upstream `tools/unit_tests.sh` driver in the pytest path. Horizon
27.0.0 (2026.2) no longer ships that driver, so for it the runner calls pytest
four times the way that tag's `tox.ini` does, once per project with its own
Django settings module (`openstack_auth`, `horizon`, `openstack_dashboard` and
its plugin tests).

### glance

**Location:** `images/glance/Dockerfile`

The Glance service image uses the same two-stage build as Keystone. The
operator runs the API under uWSGI on every supported release with the
hand-shipped `/var/lib/openstack/bin/glance-wsgi-api` entry script. Glance's
stock module path (`glance.wsgi.api:application`) is unusable under the
operator's config layout: `wsgi_app.init_app()` ignores `sys.argv` (and so
uWSGI's `--pyargv`) and reads only `$OS_GLANCE_CONFIG_DIR/glance-api.conf`, so
the shim redirects config discovery to the two mounted `--config-dir` roots
instead.

**Stage 1 (`build`)** — extends `venv-builder`:

- Declares `ARG PIP_EXTRAS` (unused by glance today; kept for parity) and
  `ARG PIP_PACKAGES`, which carries two packages: `glance_store[s3]` (the S3 store
  driver's extra lives on `glance_store`, not `glance`, and pulls `boto3`, `botocore`,
  and `s3transfer`, all pinned in `upper-constraints.txt`) and `lhafile` (optional
  LHA-archive support for the `image_decompression` image-import plugin, pinned in
  `overrides/<release>/constraints.txt`)
- Mounts `upper-constraints.txt` and the Glance source tree via named build
  contexts (`--build-context glance=...` / `--build-context upper-constraints=...`)
- Installs Glance into the virtualenv using `uv pip install --constraint`. The
  `--prefix` install generates the `glance-api` and `glance-manage` console
  scripts from `setup.cfg` (it only skips PBR `wsgi_scripts`); the uWSGI entry
  script is not generated but copied in during the runtime stage

**Stage 2 (runtime)** — extends `python-base`:

- Declares `ARG EXTRA_APT_PACKAGES`, which carries `libpython3.12t64`, `qemu-utils`,
  `python3-rados` and `python3-rbd`: the venv-builder-compiled uwsgi binary links
  `libpython3.12.so.1.0`, which python-base does not ship (the same rationale as
  horizon), and the `image_conversion` image-import plugin shells out to `qemu-img info`
  and `qemu-img convert` on the staged image. The two bindings carry the `rados` and
  `rbd` modules of the RBD store. The store runs no `ceph` or `rbd` binary, so the
  image carries no `ceph-common`
- Copies `/var/lib/openstack` from the build stage using `COPY --from=build --link`
- Copies the `glance-wsgi-api` uWSGI entry script to
  `/var/lib/openstack/bin/glance-wsgi-api` (the path the glance-operator's
  `--wsgi-file` flag references)
- Links the Ceph bindings into `/var/lib/openstack/ceph-bindings` and writes
  `ceph-bindings.pth` into the virtualenv's `site-packages` (see below)
- Sets `USER openstack` for non-root execution

The image stays config-free: the glance-operator mounts `glance-api.conf`,
`glance-api-paste.ini`, and policy, and provides the staging/tasks paths as
`emptyDir` mounts.

**Runtime packages:**

| Package | Purpose |
| --- | --- |
| `libpython3.12t64` | Shared `libpython3.12.so.1.0` for the venv-builder-compiled uwsgi |
| `qemu-utils` | `qemu-img`, which the `image_conversion` image-import plugin runs on the staged image (`qemu-img info`, `qemu-img convert`) |
| `python3-rados` | The `rados` module. `glance_store/_drivers/rbd.py` imports it behind a guard, and `configure_add` raises `BadStoreConfiguration` ("The required libraries(rbd and rados) are not available") without it |
| `python3-rbd` | The `rbd` module, imported behind the same guard and checked by the same `configure_add` test |

**Ceph bindings in the virtualenv:** `images/venv-builder/Dockerfile` creates
the virtualenv with `python3 -m venv`, which excludes the system site
packages. The Ceph bindings have no PyPI wheels: noble installs `rados` and
`rbd` into `/usr/lib/python3/dist-packages`, a directory
`/var/lib/openstack/bin/python` never searches. It stays that way, because the
directory also receives every other `python3-*` package an apt entry pulls in,
at versions the upper constraints never resolved. After the package install,
the runtime stage links the two extension modules (`rados.cpython-*.so`,
`rbd.cpython-*.so`) into `/var/lib/openstack/ceph-bindings` and writes
`ceph-bindings.pth` into the directory that `sysconfig.get_path("purelib")`
reports inside the virtualenv
(`/var/lib/openstack/lib/python3.12/site-packages`), with the single line
`/var/lib/openstack/ceph-bindings`. Python's `site` module appends that
directory to `sys.path`. `rados` and `rbd` resolve, and no other module from
`dist-packages` does. The step runs outside the build-arg guard. Without
`--build-arg EXTRA_APT_PACKAGES` the two globs match nothing, and the entry
names an empty directory. When `python3-rbd` is installed, the step imports
`rados` and `rbd` with `/var/lib/openstack/bin/python` (`rbd` imports
`rados`), so a module file the globs miss or a failed link fails the build. On
a push to `main` the verify scripts run only after the tag is published, so
this check is the one that keeps such an image from shipping. The cinder and
nova-compute images write the same file and run the same check.

**Source patch:**
`patches/glance/2026.1/0001-normalize-scheme-prefixed-s3-host-in-location-repair.patch`
strips the `http://` or `https://` prefix of `s3_store_host` in
`_construct_s3_url` (`glance/common/store_utils.py`).
Upstream's S3 credential-rotation repair, `_update_s3_location_and_store_id`,
compares every S3 image location against a URL that function builds from the
raw option value. The location URLs the S3 driver stores carry only the bare
authority, because `Store._set_url_prefix` and `StoreLocation` strip the prefix,
so the two never match when the option has one. The glance-operator always
renders a prefix: the GlanceBackend CRD requires `^https?://` on `spec.s3.host`,
and boto3 needs the scheme in `endpoint_url` for a non-AWS endpoint such as
Garage. Unpatched, every API request that touches an S3 image logs
"S3 URL mismatch for image ..., updating URL" and rewrites the location row,
and a genuine credential rotation cannot be told apart from the permanent false
positive. The patch strips the prefix the same way the store does. No upstream
test pins the old behaviour:
`S3CredentialUpdateTestCase` in `glance/tests/unit/common/test_utils.py` gives
every mocked store the bare host `s3.amazonaws.com`, and the single-store S3
tests in `glance/tests/unit/test_store_image.py` never reach
`_construct_s3_url`. The patch therefore carries no test hunk. Upstream
status: not yet proposed.

Glance 33.0.0 (2026.2) carries no twin of that patch, because it removed `_construct_s3_url`
together with the comparison it fed. Its `_update_s3_location_credentials`
runs on every image read and strips the credentials a legacy location URL
embeds, rewriting the location to the credential-free form the S3 driver of
glance_store 5.7.0 writes. The rewrite is lazy: a location no 2026.2 pod reads
keeps its embedded credentials. During a 2026.1 → 2026.2 rolling update the
patched 2026.1 pods, whose glance_store 5.4.0 reads the credentials from the
URL, put them back on each read and log "S3 URL mismatch", so a location can
flip back and forth until the last 2026.1 pod is gone.

**Source patch:**
`patches/glance/2026.2/0001-test-new-image-with-location-do-not-rely-on-dns-resolution.patch`
carries upstream commit `a37c43135e` (master, 2026-09-28), cherry-picked to
stable/2026.2 as `a3e5f490b7` (2026-09-30), until a glance tag contains it;
33.0.0 is the newest 33.x tag as of 2026-10-10. The test
`TestImageFactory.test_new_image_with_location` in
`glance/tests/unit/test_store_image.py` builds an image whose location is
`http://storeurl.com/container/<uuid>`. Glance 33.0.0 added an SSRF filter for
HTTP(S) locations: `_check_location_uri` (`glance/location.py`) calls
`store_utils.validate_external_location`, which hands `http` and `https`
locations to `validate_uri` (`glance/common/utils.py`), and
`get_validated_address` resolves the host with `socket.getaddrinfo`. The
domain `storeurl.com` answers NXDOMAIN since about 2026-10-09, the lookup
raises `socket.gaierror`, and the test fails with `BadStoreUri: Invalid
location` before any store backend runs. A suite that resolves a public host
cannot pass without DNS either. The patch mocks
`glance.common.utils.socket.getaddrinfo` for that one test and returns a
single fake address; the hunk is the upstream one verbatim, so the patch
reverse-applies on the first tag that contains the commit and is retired on
that Renovate bump. It changes a test module only, nothing in the built image,
so `verify_glance.sh` carries no assertion for it. There is no 2026.1 twin:
glance 32.0.0 has no `validate_uri`, so the test never resolves the host there.

`tests/container-images/verify_glance.sh` Test 12 drives the repair the built
image carries, under an `http://` and an `https://` host. On 2026.1 it calls
`_update_s3_location_and_store_id`: a location the driver's `StoreLocation` wrote has to stay unchanged while the credentials match, and
still has to be rewritten once the access key rotates. On 2026.2 it calls
`_update_s3_location_credentials`: a location the driver wrote carries no
credentials and stays unchanged, a legacy location with embedded credentials
becomes the driver-written URL, and a second pass changes nothing. An image
that carries neither function fails the test.

**Final image properties:**

- Runs as `openstack` user (UID 42424, GID 42424)
- Contains no build tools (`gcc`, `python3-dev`, `build-essential`, `uv` are absent)
- Virtualenv at `/var/lib/openstack` with all Glance dependencies and the S3 store driver
- `glance-manage` and `glance-api` CLIs available via `PATH`

**Unit tests:** glance ships a `.stestr.conf`, so `hack/ci-run-unit-tests.sh`
runs its suite under stestr (the default path, as for keystone). No release
carries an exclude file. The first CI run of the 33.0.0 suite counted 2,423
tests with the one `test_new_image_with_location` failure the source patch
above removes; a run of the patched tree in the venv-builder container
(local, arm64, 2026-10-10) counted 2,423 tests and passed with no failure:
2,422 passed, 1 skipped.

**Image contract check:** `tests/container-images/verify_glance.sh` is the hard
gate — it verifies the CLIs, importability, the uWSGI entry script, the S3 store
driver's boto3 resolution, non-root execution, and the absence of build tools.
On 2026.1, its Test 12 fails against an image built without the
source patch above. Test 13 covers the RBD store's bindings with the checks
of `tests/lib/ceph_bindings.sh`, which the cinder and nova-compute scripts
share: `ceph-bindings.pth` holds its single line, `rados` and `rbd` import and
resolve to the files in `/usr/lib/python3/dist-packages`, and that directory is
absent from the virtualenv's `sys.path`. The store's guard attributes `rbd` and
`rados` must not be `None`, and `command -v ceph` and `command -v rbd` must
fail, so a manifest edit that adds `ceph-common` to the glance image fails the
build. Against an image built
without `--build-arg EXTRA_APT_PACKAGES`, the script fails tests 8, 9 and 13,
and Test 13 names `No module named 'rados'`.

### placement

**Location:** `images/placement/Dockerfile`

The Placement service image uses the same two-stage build as Keystone. Its one
service-specific twist is the WSGI entry script: upstream ships no usable one
for either release. 15.0.0 (2026.1) packages with `pyproject.toml` and
declares no WSGI script at all. 16.0.0 (2026.2) declares only
`placement-manage` and `placement-status` under `[project.scripts]` and no WSGI script either. The
entry is therefore written by hand in the build stage under the upstream name,
for every release, with no release conditional.

**Stage 1 (`build`)** — extends `venv-builder`:

- Declares `ARG PIP_EXTRAS` and `ARG PIP_PACKAGES` (both empty for placement
  today; the wiring mirrors the other service images so `extra-packages.yaml`
  stays the single edit point)
- Mounts `upper-constraints.txt` and the Placement source tree via named build
  contexts (`--build-context placement=...` /
  `--build-context upper-constraints=...`)
- Installs Placement into the virtualenv using `uv pip install --constraint`.
  The `--prefix` install generates the `placement-manage` and `placement-status`
  console scripts (it only skips PBR `wsgi_scripts`)
- Writes `/var/lib/openstack/bin/placement-api` — a two-line entry that calls
  `placement.wsgi.init_application()` — and marks it executable

**Stage 2 (runtime)** — extends `python-base`:

- Declares `ARG EXTRA_APT_PACKAGES`, which carries `libpython3.12t64`: the
  venv-builder-compiled uwsgi binary links `libpython3.12.so.1.0`, which
  python-base does not ship (the same rationale as glance and horizon).
  Placement is otherwise pure Python at runtime
- Copies `/var/lib/openstack` from the build stage using `COPY --from=build --link`
- Sets `USER openstack` for non-root execution

The image stays config-free: placement locates its configuration via the
`OS_PLACEMENT_CONFIG_DIR` environment variable set by the operator.

**Runtime packages:**

| Package | Purpose |
| --- | --- |
| `libpython3.12t64` | Shared `libpython3.12.so.1.0` for the venv-builder-compiled uwsgi |

**Final image properties:**

- Runs as `openstack` user (UID 42424, GID 42424)
- Contains no build tools (`gcc`, `python3-dev`, `build-essential`, `uv` are absent)
- Virtualenv at `/var/lib/openstack` with all Placement dependencies
- `placement-manage` and `placement-status` CLIs available via `PATH`
- The uWSGI entry script at `/var/lib/openstack/bin/placement-api`

**Image contract check:** `tests/container-images/verify_placement.sh` is the
hard gate — it verifies the CLIs, importability, the uWSGI entry script
(present, executable, parsable, and its import target resolvable), that uwsgi
runs, non-root execution, and the absence of build tools.

### barbican

**Location:** `images/barbican/Dockerfile`

The Barbican service image uses the same two-stage build as Keystone. It ships
no WSGI entry script at all. Both releases ship `barbican/wsgi/api.py` with a
module-level `application`, so the barbican-operator launches uWSGI with the
stock module path `barbican.wsgi.api:application` against config mounted at
`/etc/barbican/`. Placement's entry script is written by hand because upstream
ships no usable one, and glance carries the `glance-wsgi-api` shim because its
stock module path ignores the operator's config layout. Barbican needs neither.

**Stage 1 (`build`)** — extends `venv-builder`:

- Declares `ARG PIP_EXTRAS` and `ARG PIP_PACKAGES` (both empty for barbican
  today; the wiring mirrors the other service images so `extra-packages.yaml`
  stays the single edit point)
- Mounts `upper-constraints.txt` and the Barbican source tree via named build
  contexts (`--build-context barbican=...` /
  `--build-context upper-constraints=...`)
- Installs Barbican into the virtualenv using `uv pip install --constraint`.
  The `--prefix` install generates the console scripts from `setup.cfg`. It
  skips only the PBR `wsgi_scripts` entry `barbican-wsgi-api`, which no
  hand-written script replaces

**Stage 2 (runtime)** — extends `python-base`:

- Declares `ARG EXTRA_APT_PACKAGES`, which carries `libpython3.12t64`: the
  venv-builder-compiled uwsgi binary links `libpython3.12.so.1.0`, which
  python-base does not ship (the same rationale as glance, horizon, and
  placement). Barbican is otherwise pure Python at runtime
- Copies `/var/lib/openstack` from the build stage using `COPY --from=build --link`
- Sets `USER openstack` for non-root execution

The image stays config-free: the barbican-operator mounts `barbican.conf` and
`barbican-api-paste.ini` under `/etc/barbican/`.

**Runtime packages:**

| Package | Purpose |
| --- | --- |
| `libpython3.12t64` | Shared `libpython3.12.so.1.0` for the venv-builder-compiled uwsgi |

**Final image properties:**

- Runs as `openstack` user (UID 42424, GID 42424)
- Contains no build tools (`gcc`, `python3-dev`, `build-essential`, `uv` are absent)
- Virtualenv at `/var/lib/openstack` with all Barbican dependencies, including
  the in-tree vault secret-store plugin and castellan
- `barbican-manage` and `barbican-status` CLIs available via `PATH`
- No WSGI entry script; the service is launched via the
  `barbican.wsgi.api:application` module path

**Unit tests:** barbican ships a `.stestr.conf`, so `hack/ci-run-unit-tests.sh`
runs its suite under stestr (the default path, as for keystone).

**Image contract check:** `tests/container-images/verify_barbican.sh` is the
hard gate — it verifies the CLIs, importability, that the `vault_plugin` and
`castellan.drivers` `vault` stevedore entry points load, that
`barbican.wsgi.api` resolves and binds `application` to something other than
the `None` sentinel, that uwsgi runs, non-root execution, and the absence of
build tools. Both plugin halves are loaded through their entry-point metadata
rather than imported by module path, because that is how barbican and
castellan resolve them at runtime — an import would go green off the `.py`
file on disk while the runtime lookup raises stevedore `NoMatches`. The WSGI
check inspects the module instead of importing it: importing
`barbican.wsgi.api` executes `get_api_wsgi_script()`, which reads paste config
from `/etc/barbican/barbican-api-paste.ini`, absent from the bare image. So it
pairs `importlib.util.find_spec` for the module path with an `ast.parse` of the
module source for the `application` symbol. Both pinned releases open with
`application = None` and only rebind it inside a `threading.Lock()` block, so
the check walks into nested statement bodies but stops at every function,
class and lambda boundary — uWSGI looks `application` up as a module global,
and a binding in a nested scope is a different symbol. A module whose only
module-level binding is that sentinel is rejected; otherwise uWSGI binds
`application` to `None` and every request fails.

### neutron

**Location:** `images/neutron/Dockerfile`

The Neutron service image uses the same two-stage build as Keystone. It ships
no WSGI entry script. No release ships a `neutron-server` script.
`neutron/wsgi/api.py` is byte-identical at 28.0.1 (2026.1) apart from one
argument 29.0.0 (2026.2) adds, `prog='neutron-api'`, to the `boot_server` call.
Both tags bind a module-level `application` inside a
`threading.Lock()` block, so the neutron-operator launches uWSGI with
`--module neutron.wsgi.api` and lets the process find its configuration
through the `OS_NEUTRON_CONFIG_DIR` and `OS_NEUTRON_CONFIG_FILES` environment
variables. 28.0.1 declares no WSGI script.

**Stage 1 (`build`)** extends `venv-builder`:

- Declares `ARG PIP_EXTRAS` and `ARG PIP_PACKAGES` (both empty for neutron
  today; the wiring mirrors the other service images so `extra-packages.yaml`
  stays the single edit point)
- Mounts `upper-constraints.txt` and the Neutron source tree via named build
  contexts (`--build-context neutron=...` /
  `--build-context upper-constraints=...`)
- Installs Neutron into the virtualenv using `uv pip install --constraint`.
  The `--prefix` install generates the console scripts declared in
  `pyproject.toml` (28.0.1): `neutron-db-manage`,
  `neutron-status`, `neutron-periodic-workers`,
  `neutron-ovn-maintenance-worker`, `neutron-ovn-metadata-agent` and
  `neutron-ovn-db-sync-util`

**Stage 2 (runtime)** extends `python-base`:

- Declares `ARG EXTRA_APT_PACKAGES`, which carries four packages: the shared
  libpython the venv-builder-compiled uwsgi links against, plus the
  `ovsdb-client`, `haproxy` and `ip` binaries neutron shells out to
- Copies `/var/lib/openstack` from the build stage using `COPY --from=build --link`
- Sets `USER openstack` for non-root execution

The image stays config-free. The neutron-operator supplies the configuration
through `OS_NEUTRON_CONFIG_DIR` and `OS_NEUTRON_CONFIG_FILES`, and either
points `[DEFAULT] api_paste_config` at the shipped package data file
`/var/lib/openstack/etc/neutron/api-paste.ini` or mounts that file to
`/etc/neutron/api-paste.ini`. That file is byte-identical at both tags.

**Runtime packages:**

| Package | Purpose |
| --- | --- |
| `haproxy` | The metadata agent spawns `haproxy -f <cfg>` per network (`neutron/agent/metadata/driver_base.py`) |
| `iproute2` | `ip`, which that agent uses for `ip netns exec` (`neutron/agent/linux/ip_lib.py`) |
| `libpython3.12t64` | Shared `libpython3.12.so.1.0` for the venv-builder-compiled uwsgi |
| `openvswitch-common` | `ovsdb-client`, which `pre_fork_initialize` shells out to (`neutron/common/ovn/utils.py`); without it every API worker dies with `[Errno 2] No such file or directory: 'ovsdb-client'` |

Noble ships Open vSwitch 3.3.9. The OVSDB wire protocol is schema-independent,
so that client talks to the OVN 26.03 server.

The virtualenv holds `ovs` 3.7.0 (2026.1) and 3.7.1 (2026.2), whose
`ovs/dns_resolve.py` resolves no hostname without the `unbound` Python module.
The image ships no `python3-unbound`: the distribution package installs into
the system interpreter, which the virtualenv under `/var/lib/openstack` does
not see, so `import unbound` inside the image raises `ModuleNotFoundError`.
Per decision D9 of issue #898, `OVNCentral` publishes IP addresses in
`status.dbAddress` and `status.internalDbAddress`, and the neutron-operator
renders `[ovn] ovn_nb_connection` and `ovn_sb_connection` from those.

**Final image properties:**

- Runs as `openstack` user (UID 42424, GID 42424)
- Contains no build tools (`gcc`, `python3-dev`, `build-essential`, `uv` are absent)
- Virtualenv at `/var/lib/openstack` with all Neutron dependencies
- The console scripts listed above available via `PATH`
- No WSGI entry script; the service is launched via the `neutron.wsgi.api`
  module path

**Unit tests:** neutron ships a `.stestr.conf`, so `hack/ci-run-unit-tests.sh`
runs its suite under stestr (the default path, as for keystone). The script
appends `--exclude-list /workspace/test-excludes/neutron.txt` only when
`releases/<release>/test-excludes/neutron.txt` exists, and no release
excludes a test: the first runs at 27.0.3, 28.0.1 and 29.0.0 (22,093 tests)
hit no environment-dependent failure. At roughly 21,000 tests the suite is the
largest in the tree.

**Image contract check:** `tests/container-images/verify_neutron.sh` is the
hard gate. Its 12 tests cover `neutron-db-manage --help` and
`neutron-status --help`, the importability of `neutron` and of the `ovs` and
`ovsdbapp` client libraries, the presence of `api-paste.ini`, and `--help` on
the four companion console scripts. `ovsdb-client --version` naming Open
vSwitch, `haproxy -v` and `ip -V` prove the apt wiring. The
remaining tests check non-root execution, the absence of build tools, and that
uwsgi runs. The WSGI check inspects the module instead of importing it:
importing `neutron.wsgi.api` executes `server.boot_server(api.api_server)`,
which reads the configuration the operator mounts and the bare image does not
carry. So it pairs `importlib.util.find_spec` for the module path with an
`ast.parse` of the module source, and rejects a module whose only module-level
binding of `application` is the `None` sentinel. Pointed at a barbican image,
the script fails in each of its first nine tests.

### cinder

**Location:** `images/cinder/Dockerfile`

The Cinder service image uses the same two-stage build as Keystone. It ships
no WSGI entry script. `cinder/wsgi/api.py` is byte-identical at 28.0.0
(2026.1) and 29.0.0 (2026.2), and both tags bind a
module-level `application` inside a `threading.Lock()` block. The
cinder-operator launches uWSGI with
`--module cinder.wsgi.api:application` and `--pyargv "--config-dir <dir>"`,
because `initialize_application()` reads `CONF(sys.argv[1:])`.

**Stage 1 (`build`)** extends `venv-builder`:

- Declares `ARG PIP_EXTRAS` and `ARG PIP_PACKAGES` (both empty for cinder
  today; the wiring mirrors the other service images so `extra-packages.yaml`
  stays the single edit point)
- Mounts `upper-constraints.txt` and the Cinder source tree via named build
  contexts (`--build-context cinder=...` /
  `--build-context upper-constraints=...`)
- Installs Cinder into the virtualenv using `uv pip install --constraint`.
  The `--prefix` install generates the nine console scripts declared in
  `pyproject.toml` (28.0.0): `cinder-api`,
  `cinder-backup`, `cinder-manage`, `cinder-rootwrap`, `cinder-rtstool`,
  `cinder-scheduler`, `cinder-status`, `cinder-volume` and
  `cinder-volume-usage-audit`

**Stage 2 (runtime)** extends `python-base`:

- Declares `ARG EXTRA_APT_PACKAGES`, which carries six packages: the shared
  libpython the venv-builder-compiled uwsgi links against, the `mount.nfs` and
  `qemu-img` binaries the volume drivers reach for, and the Ceph client the RBD
  volume and backup drivers use (`ceph-common` for the `ceph` and `rbd` tools,
  `python3-rados` and `python3-rbd` for the bindings). The `sudo` of the root
  helper comes from `python-base`
- Copies `/var/lib/openstack` from the build stage using `COPY --from=build --link`
- Creates the five state directories under `/var/lib/cinder`, empty and owned
  by UID/GID 42424
- Copies `cinder-amqp-ready` to `/var/lib/openstack/bin/cinder-amqp-ready`
  with mode 0755
- Links the Ceph bindings and writes `ceph-bindings.pth`, as the
  [glance](#glance) image does
- Sets `USER openstack` for non-root execution

The image stays config-free. The package data files `api-paste.ini`,
`resource_filters.json`, `rootwrap.conf` and `rootwrap.d/volume.filters` land
under `/var/lib/openstack/etc/cinder/` at both tags, declared under
`[tool.setuptools.data-files]` in `pyproject.toml` at 28.0.0 and 29.0.0.
Nothing in the Dockerfile copies `etc/cinder/` by hand, and the contract script asserts the four files. The
cinder-operator points `api_paste_config` and `resource_query_filters_file` at
those absolute paths.

**Runtime packages:**

| Package | Purpose |
| --- | --- |
| `libpython3.12t64` | Shared `libpython3.12.so.1.0` for the venv-builder-compiled uwsgi |
| `nfs-common` | `mount.nfs`, which `NfsDriver.do_setup` probes for through the root helper (`cinder/volume/drivers/nfs.py`) and raises `NfsException` without. Under the restricted posture the pod never mounts and the probe's non-zero exit is tolerated, but the binary has to exist |
| `qemu-utils` | `qemu-img`, which create-from-image, clone and extend shell out to (`fetch_to_raw`, `resize_image`, `convert_image` in `cinder/image/image_utils.py`), along with the LUKS qcow2 pre-create in `cinder/volume/drivers/remotefs.py` |
| `ceph-common` | `ceph` and `rbd`. The RBD volume driver (`cinder/volume/drivers/rbd.py`) runs `ceph mon dump --format=json` in `_get_mon_addrs`, `rbd import` in `_create_encrypted_volume` and `_copy_image_to_volume`, `rbd export` in `copy_volume_to_image` and `rbd status` in `_get_image_status`. The Ceph backup driver (`cinder/backup/drivers/ceph.py`) pipes `rbd export-diff` into `rbd import-diff` in `_rbd_diff_transfer` |
| `python3-rados` | The `rados` module. Both drivers import it behind a guard, and `check_for_setup_error` raises "rados and rbd python libraries not found" without it (`VolumeBackendAPIException` in the volume driver, `BackupDriverException` in the backup driver). `ceph-common` depends on it; the list names it because the drivers import it |
| `python3-rbd` | The `rbd` module, imported behind the same guard and named for the same reason |
| `sudo` | The root helper is `sudo cinder-rootwrap` and the image carries no sudoers entry. `sudo` comes from `python-base`, not from `extra-packages.yaml` |

**Ceph client:** the image writes the `ceph-bindings.pth` of the
[glance](#glance) section, because the virtualenv does not search
`/usr/lib/python3/dist-packages`, the only directory noble installs the two
bindings into. `ceph-common` also brings `python3-requests`, `python3-yaml`
and their dependencies, `python3-chardet` among them, into that directory, and
the virtualenv imports none of them. The postinst of `ceph-common` adds the
system user `ceph` (UID 64045) and the directories `/var/lib/ceph` (mode 0750)
and `/var/log/ceph` (mode 3770), both owned by it. The service user 42424 can
write neither. The `ceph.conf` the cinder-operator renders for an RBD backend
sets `log_file = /dev/null` and an admin socket under `/tmp`, so the client
writes neither to `/var/log/ceph` nor to `/var/run/ceph`. noble serves Ceph
19.2.3 (Squid). Its `librbd` reads cephx keys of the classic `aes` type and not
the `aes256k` keys a fresh Ceph Tentacle prefers. The cipher pin therefore
belongs to the lab's Ceph (decision D7 of issue #1338), and the image does not
set it.

**Source patch:**
`patches/cinder/2026.1/0001-nfs-run-qemu-img-info-as-the-service-user.patch`
and its 2026.2 twin flip the one `run_as_root=True` in
`NfsDriver._qemu_img_info` to `run_as_root=False`. `_qemu_img_info_base` in
`cinder/volume/drivers/remotefs.py` then follows `nas_secure_file_operations`
for that call, like every other file operation of the driver. Upstream forced
root in commit `dbc9c7ca59` (2017) for files a Nova instance had attached;
under the restricted posture of decision D3 of issue #979 there is no root to
fall back to, and every file is owned by the service user.

The flip is necessary but not sufficient, and the cinder-operator of issue
#987 owes the other half. `_qemu_img_info_base` resolves
`run_as_root or self._execute_as_root`, and `_execute_as_root` starts out
`True`: only `NfsDriver.set_nas_security_options` clears it, and only when
`nas_secure_file_operations` resolves to `true`. The option defaults to
`auto`, which resolves to `true` only for a new install that can write
`.cinderSecureEnvIndicator` onto the share, and to `false` for every existing
one. The operator therefore has to write `nas_secure_file_operations = true`
explicitly; without it `qemu-img info` still goes through
`sudo cinder-rootwrap`, which has no sudoers entry, and every
create-from-image, clone and extend fails. Both build paths
apply every `patches/<service>/<release>/*.patch` before installing:
`.github/actions/checkout-service-source/action.yaml` for the build and
unit-test jobs, `hack/ci-build-service-image.sh` for the e2e image build. A
local build runs `git -C src/cinder apply "$PWD"/patches/cinder/<release>/*.patch`
between Step 2 and Step 3 of the
[local build instructions](#local-build-instructions). Cinder's
`test_copy_volume_from_snapshot` keeps expecting `run_as_root=True` and stays
green, because its test driver never calls `set_nas_security_options` and
`_execute_as_root` keeps its default `True`, so the patch carries no test
hunk. Upstream status: not yet proposed.

`patches/cinder/2026.1/0002-create-from-image-run-qemu-img-as-the-service-user.patch`
and its 2026.2 twin flip two more forced-root `qemu-img info`
calls. Both sit above the NFS driver on the create-from-image path and neither
goes through it, so the flip in `0001` never reaches them.
`CreateVolumeFromSpecTask._create_from_image_cache_or_download`
(`cinder/volume/flows/manager/create_volume.py`) inspects the image it has just
downloaded with `image_utils.qemu_img_info(tmp_image)`, whose `run_as_root`
defaults to `True`. `fetch_verify_image` (`cinder/image/image_utils.py`)
inspects the same file through `get_qemu_data(image_id, has_meta, format_raw,
dest, True)`, which passes the flag positionally. In both cases the file is the
temporary copy the service itself wrote into `image_conversion_dir`, owned by
UID 42424, so reading it as root gains nothing. The two calls fail differently
where `sudo cinder-rootwrap` has no sudoers entry: the manager call raises the
`ProcessExecutionError` uncaught and every create-from-image ends in `error`
status, while `fetch_verify_image` swallows it. `get_qemu_data` reads a failing
`qemu-img` as "qemu-img is not installed", returns `None` for a raw image and
skips the backing-file and data-file checks the function exists for, and raises
`ImageUnacceptable` for every other disk format. Three upstream tests in
`cinder/tests/unit/test_image_utils.py` pin the `fetch_verify_image` argument
and move with it, so this patch does carry test hunks; no upstream test pins
the manager call. `tests/container-images/verify_cinder.sh` asserts both call
sites against the built image. Upstream status: not yet proposed.

`patches/cinder/2026.1/0003-tests-collect-garbage-before-the-backup-tpool-size-tests.patch`
is test-only. `BackupTestCase.test_default_tpool_size` and `test_tpool_size`
assert that eventlet's native thread pool is empty before and after they build
a `BackupManager`. The Ceph backup driver tests leave os-brick
`RBDVolumeIOWrapper` objects to the garbage collector, and a collected wrapper
closes itself: `close()` flushes, the flush reaches the image through a
`tpool.Proxy`, and that starts the pool's 20 threads. A collection between the
two assertions fails the test with "Second list contains 20 additional
elements", as it did once in `test-service-images (cinder, 2025.2)` on
2026-10-01. Both tests now start with `gc.collect()` and `tpool.killall()`, so
every such finalizer has run before the first assertion; the assertions are
unchanged. The patch changes nothing at runtime. Upstream status: not yet
proposed; master replaced both tests when the backup service moved to native
threads (`c07c49c586`). Cinder 29.0.0 carries that commit, so 2026.2 needs no
twin.

**Readiness probe:** `images/cinder/cinder-amqp-ready` is the exec readiness
probe of the cinder-scheduler, cinder-volume and cinder-backup processes
(decision D2 of issue #979). None of the three serves an HTTP port, so
readiness here is "a process of this container holds a socket established to
the message broker port", the semantics of kolla's `healthcheck_port`. The
script reads `/proc/net/tcp` and `/proc/net/tcp6` and skips a table that does
not exist. It exits 0 and prints `established to broker port <n>` when a row
in state `01` has a remote port equal to `CINDER_AMQP_PORT` (default `5672`)
**and** an inode one of the container's own processes holds; otherwise it
exits 1 and prints `no established connection to broker port <n>`. The inode
match is what keeps the answer local: `/proc/net/tcp*` is scoped to the
network namespace every container of a pod shares, so the row alone would let
one healthy connection report ready for every co-located cinder process.
A process counts as the container's own when it shares the probe's mount
namespace, which a container keeps to itself whatever the pod spec says:
`shareProcessNamespace: true` lists the mates' processes in `/proc`, and the
probe walks past them. The probe cannot see a broker that died without a
`FIN` or an `RST` — that socket stays `ESTABLISHED` until the TCP
keepalive expires, long after oslo.messaging's heartbeat gave up on it — so
like `healthcheck_port` it answers "the connection exists", not "the broker
answers". A `CINDER_AMQP_PORT` that is not a port number — not a number at
all, or a number outside `1`–`65535`, such as the `0` an unset field renders
as — exits with a one-line message naming the variable but not its value, and
not with a traceback: kubelet copies an exec probe's output verbatim into the
`Unhealthy` event, and the key this misconfiguration is confused with carries
the broker password. Refusing the out-of-range value is what keeps it from
reading as a broker outage, the message a port no row can match would print
for the life of the deployment. It needs no capability and no writable
filesystem. The cinder-operator of issue #987 wires it as
`readinessProbe.exec.command: ["/var/lib/openstack/bin/cinder-amqp-ready"]`.

**State directories:** `/var/lib/cinder` is `[DEFAULT] state_path`. `mnt` and
`backup_mount` are the two os-brick mount bases (`nfs_mount_point_base`,
`backup_mount_point_base`), `conversion` is `image_conversion_dir`, `tmp` is
`[oslo_concurrency] lock_path`, and `coordination` is the tooz `file://`
directory (decisions D2 and D12 of issue #979). The cinder-operator mounts an
`emptyDir` over `/var/lib/cinder` under `readOnlyRootFilesystem`. The five
directories in the image are what a plain `docker run` gets.

**Final image properties:**

- Runs as `openstack` user (UID 42424, GID 42424)
- Contains no build tools (`gcc`, `python3-dev`, `build-essential`, `uv` are absent)
- Virtualenv at `/var/lib/openstack` with all Cinder dependencies
- The nine console scripts and `cinder-amqp-ready` available via `PATH`
- No WSGI entry script; the API is launched via the
  `cinder.wsgi.api:application` module path
- `sudo` present with no sudoers entry

**Unit tests:** cinder ships a `.stestr.conf`, so `hack/ci-run-unit-tests.sh`
runs its suite under stestr. Both releases carry an exclude file. The 2026.1
file records the 13 `TestFormatInspectors` failures of its first run (they
build their fixture images with `qemu-img create` and died with exit status
127) and does not exclude them: six of the 13 are the safety checks between a
tenant-uploaded image and the volume host, and this leg is the only gate in the
pipeline that runs them, so `images/venv-builder/Dockerfile` installs
`qemu-utils` instead. `releases/2026.1/test-excludes/cinder.txt` excludes one
test, `test_put_container_disabled`, which passes upstream only because tox
runs unprivileged and `os.makedirs` raises `PermissionError`, whereas the
container runs it as root; the first run of the 28.0.0 suite counted 18,076
tests. `releases/2026.2/test-excludes/cinder.txt` keeps that one pattern, and
the first run of the 29.0.0 suite counted 19,003 tests with no failure (18,984
passed, 19 skipped).

**Image contract check:** `tests/container-images/verify_cinder.sh` is the
hard gate. Its 16 test functions cover `cinder-manage --version` and
`cinder-status --help`, the importability of `cinder` and of `cinder.wsgi.wsgi`
together with the driver and backend libraries (`os_brick`, castellan's
Barbican key manager, `boto3`, `tooz`, `taskflow`, `oslo_privsep`), the four
package data files, and `--help` on `cinder-scheduler`, `cinder-volume`,
`cinder-backup` and `cinder-api`. `mount.nfs` being present and executable,
`qemu-img --version`, `sudo --version` and a refused `sudo -n true` prove the
apt wiring. The Ceph test, which runs right after it as test 16, runs the
bindings checks of the [glance](#glance) Test 13. After
`cinder.objects.register_all()`, the guard attributes `rados` and `rbd` of
`cinder.volume.drivers.rbd` and `cinder.backup.drivers.ceph` must not be
`None`. `ceph --version` and
`rbd --version` must print a line starting with `ceph version `; the script
logs that line for the cipher revisit of decision D7 of issue #1338 and
asserts no version. `rbd help export-diff` and `rbd help import-diff` must exit
0, and `ceph --conf /nonexistent --connect-timeout 1 mon dump` must fail fast
with `error calling conf_read_file`, which is what a cinder-volume pod with a
broken `ceph.conf` shows. An image without `ceph-bindings.pth` fails the `.pth`
and import assertions while the tools still run. An image built without
`--build-arg EXTRA_APT_PACKAGES` fails tests 8, 14 and 16, and test 16 names
`No module named 'rados'`. The probe test covers its presence and
executability, the exit 1
that names port 5672 in a bare container, that it honours
`CINDER_AMQP_PORT=1`, that a `CINDER_AMQP_PORT` carrying a URL is refused
without a traceback, the exit 0 against a connection the container itself
holds, and the exit 1 against the same connection seen from a second
container joined to its network namespace. The two patch tests assert
resolved values rather than the source text. The first reads the value
`_qemu_img_info_base` resolves: `run_as_root` has to come out `False` once
`_execute_as_root` is `False`, and `True` on the constructor default, which
is the configuration dependency the operator has to satisfy. The second
covers the create-from-image path: the `run_as_root` with which the volume
manager (`CreateVolumeFromSpecTask._create_from_image_cache_or_download`) and
`image_utils.fetch_verify_image` inspect the downloaded image has to come out
`False`. The remaining tests check non-root execution, the absence of build
tools, that uwsgi runs, and the five state directories, empty and owned by
42424 along with their parent. The WSGI check
inspects `cinder.wsgi.api` instead of
importing it: an import runs `initialize_application()` at module level and
dies with `oslo_service.wsgi.ConfigNotFound` in a bare image. So it pairs
`importlib.util.find_spec` with an `ast.parse` of the module source, and
rejects a module whose only module-level binding of `application` is the
`None` sentinel. Pointed at a neutron image, the script fails tests 1 to 11,
15 and 16, and passes only the three shared checks (non-root, no build tools,
uwsgi).

### nova

**Location:** `images/nova/Dockerfile`

The Nova service image uses the same two-stage build as Keystone, plus a
`novnc` stage for the noVNC console assets `nova-novncproxy` serves. It is
built from nova 33.0.0 (2026.1) and 34.0.0 (2026.2) and ships no WSGI entry
script. The nova-operator
of issue #1017 launches uWSGI on the module paths
`nova.wsgi.osapi_compute:application` for the compute API and
`nova.wsgi.metadata:application` for the metadata API, and passes the
configuration through `OS_NOVA_CONFIG_DIR` and `OS_NOVA_CONFIG_FILES`
(decision D1 of issue #1014). Both modules build their application at import
time through `wsgi_app.init_application`, so importing either one inside a
bare image raises `ConfigFilesNotFoundError`. At 33.0.0 both also call
`monkey_patch.patch(backend='threading')` at import.

**Stage 0 (`novnc`)** extends `python-base`:

- Declares `ARG NOVNC_VERSION` and `ARG NOVNC_COMMIT`, the noVNC pin
- Installs `git` from the signed noble archive. The stage does not extend
  `venv-builder`, which also carries git, and stays out of the `build` stage:
  both run third-party build code from PyPI as root (`venv-builder` builds
  uwsgi from its sdist after installing git), and the `github_token` secret
  must not reach a git binary or a git configuration that code could have
  replaced. `tests/unit/images/github_token_stage_base_test.sh` fails on any
  stage mounting the secret that takes files from `venv-builder`, through its
  `FROM`, a `COPY --from` or a bind mount
- Fetches the pinned noVNC tree from github.com, with up to three attempts and
  a linear backoff, and stages nine paths of it under `/opt/novnc`. A stage of
  its own keeps the fetch off the install's cache chain, so only a pin bump
  fetches again

**Stage 1 (`build`)** extends `venv-builder`:

- Declares `ARG PIP_EXTRAS` and `ARG PIP_PACKAGES` (both empty for nova today;
  nova's `osprofiler`, `zvm` and `vmware` extras are not wanted, and the
  wiring mirrors the other service images so `extra-packages.yaml` stays the
  single edit point)
- Mounts `upper-constraints.txt` and the Nova source tree via named build
  contexts (`--build-context nova=...` /
  `--build-context upper-constraints=...`)
- Installs Nova into the virtualenv using `uv pip install --constraint`. The
  `--prefix` install generates the eleven console scripts declared in
  `[project.scripts]` of `pyproject.toml` (33.0.0): `nova-compute`,
  `nova-conductor`, `nova-manage`, `nova-novncproxy`, `nova-policy`,
  `nova-rootwrap`, `nova-rootwrap-daemon`, `nova-scheduler`,
  `nova-serialproxy`, `nova-spicehtml5proxy` and `nova-status`

**Stage 2 (runtime)** extends `python-base`:

- Declares `ARG EXTRA_APT_PACKAGES`, which carries one package: the shared
  libpython the venv-builder-compiled uwsgi links against. The `sudo` that
  `nova-rootwrap` would use comes from `python-base`
- Copies `/var/lib/openstack` from the build stage using `COPY --from=build --link`
- Copies the staged `/opt/novnc` from the `novnc` stage to `/usr/share/novnc`
- Creates the two state directories under `/var/lib/nova`, empty and owned by
  UID/GID 42424
- Copies `nova-amqp-ready` to `/var/lib/openstack/bin/nova-amqp-ready` with
  mode 0755
- Sets `USER openstack` for non-root execution

The image stays config-free. The package data files `api-paste.ini`,
`rootwrap.conf` and `rootwrap.d/compute.filters` land under
`/var/lib/openstack/etc/nova/` at both tags, declared under
`[tool.setuptools.data-files]` in `pyproject.toml` at 33.0.0 and 34.0.0.
`api-paste.ini` and `rootwrap.conf` are byte-identical at both tags.
Nothing in the Dockerfile copies `etc/nova/` by hand, and the contract script
asserts the three files.

**Runtime packages:**

| Package | Purpose |
| --- | --- |
| `libpython3.12t64` | Shared `libpython3.12.so.1.0` for the venv-builder-compiled uwsgi |
| `sudo` | The root helper `nova-rootwrap` is a compute-side tool, and the image carries no sudoers entry. `sudo` comes from `python-base`, not from `extra-packages.yaml` |

`qemu-utils` and `libvirt0` stay out (decision D14 of issue #1014). The
control-plane roles convert no images, and the fake driver of issue #1018
needs no libvirt: `nova.virt.fake` imports without it.

**noVNC pin:** `ARG NOVNC_VERSION` names the upstream tag, `v1.7.0`, and
`ARG NOVNC_COMMIT` the commit that tag resolves to. `v1.7.0` is an annotated
tag, so the pin names the peeled commit, not the tag object. The build fetches
by commit and then greps the checked-out `package.json` for the version the
tag carries. A tree that disagrees fails the build with
`noVNC package.json version does not match NOVNC_VERSION=<tag>: NOVNC_COMMIT
is stale`, so a bump that moved one line and left the other behind stops there
instead of shipping old assets under a new version. A fetch that fails three
times ends the build with `noVNC fetch of <commit> failed after 3 attempts`
instead, so a rejected token or a network error does not read as a stale pin.

Nine paths are copied: `app/`, `core/`, `vendor/`, `vnc.html`,
`vnc_lite.html`, `defaults.json`, `mandatory.json`, `package.json` and
`LICENSE.txt`. `vnc_lite.html` is the page `[vnc] novncproxy_base_url` names
(decision D8 of issue #1014; the tree ships no `vnc_auto.html`) and it imports
`core/rfb.js`. `vnc.html` loads `app/ui.js`, which reads `defaults.json`,
`mandatory.json` and `package.json`, the version the console shows. `vendor/`
carries pako, the inflate library `core/` imports. `LICENSE.txt` is the
licence notice of the assets: MPL-2.0 for the core library, plus the licences
it lists for the bundled parts. The tests, the docs, `po/`, `snap/`, `utils/`
and the lint configuration are not copied.

The fetching `RUN` mounts the `github_token` BuildKit secret that the
`Build service image` step of
[build-service-images](./build-images-workflow.md#build-service-images) and
`hack/ci-build-service-image.sh` pass; a build without it, such as a local
`docker build`, fetches anonymously. Renovate tracks both ARGs through one
custom manager (`github-tags` on `novnc/noVNC`, regex versioning for the
leading `v`), whose single `matchStrings` entry spans the two adjacent lines,
so the tag and the commit move in one PR. Majors are disabled; minors and
patches wait the 3-day cooldown and are not automerged, because the console
page is user-facing and no e2e suite loads it before issue #1018. Digest
updates are disabled too: a tag moved upstream to another commit is not a
release, and the pin stays on the reviewed commit.
`tests/unit/renovate/novnc_pin_custommanager_test.sh` replays the regex over
the Dockerfile, checks the three rules, and resolves `refs/tags/v1.7.0^{}`
upstream when it has network access, which is where a moved tag shows up.

**Readiness probe:** `images/nova/nova-amqp-ready` is the exec readiness probe
of the nova-scheduler and nova-conductor processes (decision D1 of issue
#1014). Neither serves an HTTP port, so readiness here is "a process of this
container holds a socket established to the message broker port", the
semantics of kolla's `healthcheck_port`. The script is cinder's
`cinder-amqp-ready` with `NOVA_AMQP_PORT` in place of `CINDER_AMQP_PORT`, same
default of `5672` and same semantics; the two images are independent build
contexts, so each carries its own copy, and
`tests/unit/images/amqp_ready_probe_copies_test.sh` fails when the code of the
two copies drifts apart. It reads `/proc/net/tcp` and
`/proc/net/tcp6`, skips a table that does not exist, and exits 0 with
`established to broker port <n>` when a row in state `01` has a remote port
equal to `NOVA_AMQP_PORT` **and** an inode one of the container's own
processes holds. Otherwise it exits 1 with `no established connection to
broker port <n>`. It walks the processes' descriptors only once a table holds
such a row, and stops at the first socket of its own, because the walk is the
expensive half on a service running a worker per CPU. The inode match is what
keeps the answer local:
`/proc/net/tcp*` is scoped to the network namespace every container of a pod
shares, so the row alone would let one healthy connection report ready for
every co-located nova process. A process counts as the container's own when it
shares the probe's mount namespace, which a container keeps to itself whatever
the pod spec says. A `NOVA_AMQP_PORT` that is not a port number, including the
`0` an unset field renders as, exits with a one-line message naming the
variable but not its value: kubelet copies an exec probe's output verbatim
into the `Unhealthy` event, and the key this misconfiguration is confused with
carries the broker password. Like `healthcheck_port`, the probe answers "the
connection exists", not "the broker answers": a broker that died without a
`FIN` or an `RST` leaves the socket `ESTABLISHED` until the TCP keepalive
expires. It needs no capability and no writable filesystem. The nova-operator
wires it as
`readinessProbe.exec.command: ["/var/lib/openstack/bin/nova-amqp-ready"]`.

**State directories:** `/var/lib/nova` is `[DEFAULT] state_path`, where a
compute stores its node identity `compute_id`. `tmp` is
`[oslo_concurrency] lock_path`, and `instances` is `instances_path`, whose
default `$state_path/instances` a compute expects to exist. The nova-operator
mounts an `emptyDir` over `/var/lib/nova`, so the two directories in the image
are what a plain `docker run` gets.

**Final image properties:**

- Runs as `openstack` user (UID 42424, GID 42424)
- Contains no build tools (`gcc`, `python3-dev`, `build-essential`, `uv` are absent)
- Virtualenv at `/var/lib/openstack` with all Nova dependencies
- The eleven console scripts and `nova-amqp-ready` available via `PATH`
- No WSGI entry script; the two APIs are launched via the
  `nova.wsgi.osapi_compute:application` and `nova.wsgi.metadata:application`
  module paths
- The noVNC console assets under `/usr/share/novnc`
- `sudo` present with no sudoers entry

**Unit tests:** nova ships a `.stestr.conf`, so `hack/ci-run-unit-tests.sh`
runs its suite under stestr. That script sets
`OS_NOVA_DISABLE_EVENTLET_PATCHING=False` for every service, the value nova's
tox py3 env uses: at 33.0.0 `nova/cmd/scheduler.py` selects the threading
backend at import while `nova/tests/unit/__init__.py` has already selected
eventlet, and oslo.service raises `BackendAlreadySelected` during stestr
discovery, before any exclude list applies. The other services ignore the
variable. Both releases carry an exclude file that excludes nothing. The first
runs counted 16,803 tests at 33.0.0, with 63 skips and 2 expected failures, and
16,613 at 34.0.0 with 67 skips and 2 expected failures; none hit an
environment-dependent failure.

**Image contract check:** `tests/container-images/verify_nova.sh` is the hard
gate. Its 13 tests cover `nova-manage --version` and `nova-status --help`, the
importability of `nova` and of `nova.virt.fake`, `os_vif`, `os_brick`,
`websockify`, `oslo_privsep` and `oslo_limit`, together with the
`ModuleNotFoundError` an `import libvirt` has to raise. Then come the three
package data files, the eleven console scripts as executables, and `--help` on
`nova-scheduler`, `nova-conductor`, `nova-novncproxy` and `nova-compute`. The
noVNC test asserts that the copied files are present and readable by the
service user and that the `package.json` version equals the
`ARG NOVNC_VERSION` pin it reads out of the Dockerfile; with that pin edited
to `v9.9.9` it is the only test that fails, reporting `1.7.0` against `9.9.9`.
The probe test covers its presence and executability, the exit 1 that names
port 5672 in a bare container, that it honours `NOVA_AMQP_PORT=1`, that a
`NOVA_AMQP_PORT` carrying a transport URL and one carrying `0` are both
refused without a traceback, the exit 0 against a connection the container
itself holds and past a descriptor that vanished mid-scan, that a connection
to 5671 counts with `NOVA_AMQP_PORT=5671` and one to 5672 does not, and the
exit 1 against the same connection seen from a second container joined to its
network namespace, with and without a shared PID namespace, once that
container has been shown the connection's row. The connection is held against
a listener that never accepts, so the container owns the client end alone and
a probe that read a row's local port would fail the ready checks.
`uwsgi --version`, the absence of `qemu-img`, `sudo --version` and a refused
`sudo -n true` prove the apt wiring. The remaining tests check non-root
execution, the absence of build tools, and the two state directories, empty
and owned by 42424 along with their parent. The WSGI check inspects the two
modules instead of importing them: an import runs
`wsgi_app.init_application` at module level and dies with
`ConfigFilesNotFoundError` in a bare image. So it pairs
`importlib.util.find_spec` for each module path with an `ast.parse` of the
module source, and rejects a module whose only module-level binding of
`application` is the `None` sentinel.
Both release images pass all 56 assertions.
Pointed at a cinder image the script exits 1 with 37 of them failing: every
test but the non-root and build-tool ones fails, while the wrong image still
satisfies the uwsgi and sudo halves of the apt test and the libvirt-absence
half of the import test.

### nova-compute

**Location:** `images/nova-compute/Dockerfile`, `images/nova-compute/sudoers`

nova-compute runs from this image on a compute cluster's hypervisor nodes. It
is built from nova's own source pin (33.0.0 for 2026.1, 34.0.0 for 2026.2) with
nova's patches and constraint overrides, and it is published under the same
four tags as `ghcr.io/c5c3/nova` (see
[Tag Schema](./build-images-workflow.md#tag-schema)). `nova-compute:2026.1`
and `nova:2026.1` therefore carry the same nova. On top of nova the image
carries the libvirt binding and client libraries, `qemu-img`, the host tools
for iSCSI, multipath and NVMe that os-brick (the library nova attaches volumes
with) runs, `mount.nfs` for the NFS exports nova mounts itself, `cryptsetup`,
`genisoimage`, the Ceph client, and a rootwrap and sudo posture that lets the
unprivileged `openstack` user start nova's privileged helpers.
The [nova](#nova) control-plane image carries none of it (decision D14 of
issue #1014). Its consumer is the [NovaCompute](../nova/novacompute-crd.md)
node pool, which runs it under the tag of the Nova's installed release.

The image has a directory of its own instead of a second stage in
`images/nova/Dockerfile`. `hack/ci-generate-cleanup-matrix.sh` turns every
`images/<name>/` directory into a package the nightly GHCR cleanup prunes. The only
other packages it knows are the `<name>-operator` images, one per `operators/<name>/`
directory with a `go.mod`. Every nova build takes the last stage as its default target,
so a second stage would make the order of stages matter. And `verify_nova.sh` would
share a Dockerfile with an image it must not describe. The price is one repeated install
step.

**Stage 1 (`build`)** extends `venv-builder`:

- Declares `ARG PIP_EXTRAS` (empty) and `ARG PIP_PACKAGES`, which CI fills
  with `libvirt-python` from the `nova-compute` block of
  `extra-packages.yaml`. `upper-constraints.txt` fixes its version: 12.0.0 at
  2026.1, 12.6.0 at 2026.2
- Installs `libvirt-dev` and `pkg-config`. PyPI ships libvirt-python as an
  sdist only, so the install compiles it against the libvirt API description
  and the pkg-config file of noble's libvirt 10.0.0. Both packages stay in
  this stage, together with the compilers
- Installs Nova and the binding with the same named build contexts (`nova`
  and `upper-constraints`) and the same `uv pip install --constraint` step as
  the nova image

**Stage 2 (runtime)** extends `python-base`:

- Declares `ARG EXTRA_APT_PACKAGES` and copies `/var/lib/openstack` from the
  build stage using `COPY --from=build --link`
- Installs the runtime packages, then removes the node identities they bake
  (see below). The removal sits outside the build-arg guard, so a build
  without `--build-arg EXTRA_APT_PACKAGES` stays a clean no-op
- Links the Ceph bindings and writes `ceph-bindings.pth`, as the
  [glance](#glance) image does
- Creates the two state directories under `/var/lib/nova`, empty and owned by
  UID/GID 42424, as the nova image does
- Generates `/etc/nova/rootwrap.conf`, copies the sudoers file and checks it
  with `visudo`
- Sets `USER openstack` for non-root execution

There is no noVNC stage and no `nova-amqp-ready`: the NovaCompute pod runs no
probe, because the service state Nova reports is its health signal.

**Runtime packages** (`nova-compute.apt_packages`, the same list in both
releases):

| Package | What runs it |
| --- | --- |
| `libvirt0` | `libvirt.so.0`, `libvirt-qemu.so.0` and `libvirt-lxc.so.0`, linked by the binding's `libvirtmod*` extensions; `nova/virt/libvirt/host.py` imports `libvirt` |
| `qemu-utils` | `qemu-img create` (`nova/virt/libvirt/utils.py`), `qemu-img info` and `convert` (`nova/privsep/qemu.py`) |
| `open-iscsi` | `iscsiadm` (`os_brick/initiator/connectors/iscsi.py`) |
| `multipath-tools` | `multipath`, `multipathd` (`os_brick/initiator/linuxscsi.py`) |
| `nvme-cli` | `nvme` (`os_brick/initiator/connectors/nvmeof.py`, `os_brick/privileged/nvmeof.py`) |
| `lsscsi` | `lsscsi` (`os_brick/initiator/linuxscsi.py`) |
| `udev` | `/lib/udev/scsi_id` (`get_scsi_wwn` in `os_brick/initiator/linuxscsi.py`) |
| `nfs-common` | `mount.nfs`, the helper `mount -t nfs` runs. nova mounts a Cinder NFS export itself, without os-brick: `LibvirtNFSVolumeDriver` (`nova/virt/libvirt/volume/nfs.py`) goes through `nova/virt/libvirt/volume/mount.py` to `mount` in `nova/privsep/fs.py`, below `[libvirt] nfs_mount_point_base` (default `$state_path/mnt`) |
| `cryptsetup-bin` | `cryptsetup` (`os_brick/encryptors/luks.py`) for encrypted volumes; the compute contract renders `[key_manager] backend = barbican` when Barbican is enabled |
| `genisoimage` | The default of `[DEFAULT] mkisofs_cmd` (`nova/conf/configdrive.py`), which builds config drives |
| `ceph-common` | `ceph` and `rbd` (`nova/storage/rbd_utils.py`): `ceph mon dump` in `get_mon_addrs`, `rbd import` in `import_image`, `rbd export` in `export_image` and `ceph df` in `get_pool_info` |
| `python3-rados` | The `rados` module. `nova/storage/rbd_utils.py` imports it behind a guard, and `RBDDriver.__init__` raises `RuntimeError('rbd python libraries not found')` through `_check_for_import_failure` without it. `ceph-common` depends on it; the list names it because nova imports it |
| `python3-rbd` | The `rbd` module, imported behind the same guard and named for the same reason |

`open-iscsi` and `multipath-tools` pull `systemd`, `initramfs-tools` and
`sg3-utils` as hard dependencies. Measured on the 2025.2 images, the compute
image is about 170 MB larger than the nova image. The Ceph client adds about
167 MB on amd64 (166,989,790 bytes):
`docker image inspect` on two `linux/amd64` builds of this package layer on
the pinned `ubuntu:noble` base, one with and one without `ceph-common`,
`python3-rados` and `python3-rbd`. On arm64 the same probe gives 161,471,190
bytes, equal to the delta between the 2026.1 image and a build of it without
the three packages. `nfs-common` pulls `rpcbind`, `keyutils`, `libnfsidmap1`,
`libevent-core-2.1-7t64`, `libwrap0` and `ucf`, about 3 MB together. No
process in the pod starts `rpcbind` or `rpc.statd`; an NFSv4 mount needs
neither, and the in-cluster server of `deploy/kind/nfs/nfs-server.yaml` speaks
NFSv4 only. There is no `libpython3.12t64`, because nothing in this image runs
uWSGI, and `sudo` comes from `python-base`.

**Node identities:** the postinst scripts of `open-iscsi` and `nvme-cli`
write `/etc/iscsi/initiatorname.iscsi`, `/etc/nvme/hostnqn` and
`/etc/nvme/hostid` at build time, and `multipath-tools` ships
`/etc/multipath.conf` with `user_friendly_names yes`. os-brick reports the
initiator name and the host NQN to Cinder as the node's identity, so baked
files would give every compute node the same IQN and NQN. A baked multipath
configuration can also disagree with the host's `multipathd`. The image removes
all four files, and `iscsiadm --version` and `nvme version` still run without
them. `nfs-common` and `rpcbind` bake none: their postinst scripts create the
`statd` and `_rpc` users and install `/etc/idmapd.conf`,
`/etc/default/nfs-common` and `/etc/nfs.conf`, and none of these names the
node. `ceph-common` bakes none either: its postinst installs
`/etc/ceph/rbdmap`, `/etc/default/ceph` and `/etc/logrotate.d/ceph-common`,
none of which names the node, so the removal list has no Ceph entry.

**Ceph client:** the image writes the `ceph-bindings.pth` of the
[glance](#glance) section, because the virtualenv does not search
`/usr/lib/python3/dist-packages`, the only directory noble installs the two
bindings into. `ceph-common` also brings `python3-requests`, `python3-yaml`
and their dependencies, `python3-chardet` among them, into that directory, and
the virtualenv imports none of them. The postinst of `ceph-common` adds the
system user `ceph` (UID 64045) and the directories `/var/lib/ceph` (mode 0750)
and `/var/log/ceph` (mode 3770), both owned by it. The `openstack` user (UID
42424) can write neither. The
NovaCompute still offers no `imagesType: rbd`, because it carries no Ceph
credential contract (see the [NovaCompute reference](../nova/novacompute-crd.md)).

**Rootwrap and sudo posture:** nova's root helper is
`sudo nova-rootwrap <[DEFAULT] rootwrap_config>`, and `rootwrap_config`
defaults to `/etc/nova/rootwrap.conf`. nova-compute hands that helper to every
privsep context it starts: os-brick's, nova's `sys_admin_pctxt` and os-vif's.
A privsep context is a root daemon oslo.privsep starts through
`privsep-helper`, and the helper is how it gets root.
The compute contract renders no `[privsep*]`, `[workarounds]` or
`rootwrap_config` key, so nova's default helper is the one that runs. Two gaps
would stop it in an image built like the nova image. The installed
`rootwrap.conf` points `filters_path` and `exec_dirs` at system directories,
where neither `compute.filters` nor `privsep-helper` lives. And noble's
`secure_path` does not contain `/var/lib/openstack/bin`, so
`sudo nova-rootwrap` finds no command. The image closes both:

- `/etc/nova/rootwrap.conf` is generated from the installed file with
  `filters_path=/var/lib/openstack/etc/nova/rootwrap.d` and with
  `/var/lib/openstack/bin` first in `exec_dirs`. Two `grep`s fail the build
  when an upstream release reshapes the file and the `sed` matches nothing.
  The file is root-owned with mode 0644. It records where this image keeps its
  binaries, which is image plumbing, so the config-free rule of the nova image
  does not cover it
- `/etc/sudoers.d/nova-compute`, mode 0440, sets a global `secure_path` that
  starts with `/var/lib/openstack/bin` and holds one rule:
  `openstack ALL = (root) NOPASSWD: /var/lib/openstack/bin/nova-rootwrap /etc/nova/rootwrap.conf *`.
  `visudo -csf` checks it at build time. The strict flag makes a reference to
  an undefined alias fatal, such as a `NOPASSWD` that lost its colon and reads
  as an alias name; plain `visudo -c` only warns about it

oslo.rootwrap always admits `privsep-helper` run as root, whatever the filters
say, so one rule covers every privsep daemon nova-compute starts, and
`sudo -n true` stays refused. Through `privsep-helper` the rule is
root-equivalent, which is what nova's own design grants its service user. The
pod that runs this image is privileged on a hypervisor node anyway. Because
`secure_path` is global, a consumer that runs nova-compute as root resolves the
same helper. The neutron metadata agent runs as root in the same way.

**Open vSwitch client:** os-vif plugs OVS ports through
`[os_vif_ovs] ovsdb_interface`, whose default `native` is the ovsdbapp IDL.
ovsdbapp is a Python library already in the virtualenv (2.16.1 at 2026.1,
2.19.0 at 2026.2), and nova runs no `ovs-*` binary itself. The image therefore
installs no OVS package. `openvswitch-common` carries `ovsdb-client`,
`ovs-appctl` and `ovs-ofctl`, which neither project runs. Pointing
`[os_vif_ovs] ovsdb_connection` at the host's OVSDB socket is the consumer's
configuration.

**What the image expects from its pod** (the
[NovaCompute node contract](../nova/novacompute-crd.md#node-contract) is the pod
spec that meets it):

- Configuration mounted below `/etc/nova`, such as `/etc/nova/compute.conf.d`,
  and never a volume at `/etc/nova` itself, which would hide `rootwrap.conf`
- Privilege escalation allowed (`allowPrivilegeEscalation` not `false`), since
  `sudo` is a setuid binary
- The host's `/etc/iscsi`, `/etc/nvme` and `/etc/multipath*` mounted, so
  os-brick reports the node's own identity and agrees with the host's
  `multipathd`
- `/var/lib/nova` mounted from the host (a `hostPath`, never an `emptyDir`):
  it holds `compute_id`, the node identity nova-compute writes on first start
  and must find again after every pod recreation, and `instances` below it.
  Without it the next start writes a new node UUID, which collides with the
  existing `ComputeNode` record of the host
- For a consumer that runs nova-compute as the image's `openstack` user:
  `/var/lib/nova`, `/var/lib/nova/instances` and `/var/lib/nova/tmp` on the
  host owned by 42424:42424 before nova-compute starts. The NovaCompute pod
  runs nova-compute as root, because a stock host's libvirt socket is
  `root:libvirt` 0660 with a host-specific group ID, so it needs no ownership
  change. It creates `instances` itself, `root:root` 0755, in its
  `create-instances-dir` init container (see the
  [node contract](../nova/novacompute-crd.md#node-contract)). A non-root
  consumer needs the owner change. The mount hides the
  image's own directories, the kubelet creates a missing `hostPath` directory
  as `root:root` 0755, and `fsGroup` does not apply to a `hostPath`. On a
  directory nova-compute cannot write, the first start fails to write
  `compute_id` and exits with `InvalidNodeConfiguration`. An init container
  running as root sets the owner with
  `install -d -o 42424 -g 42424 /var/lib/nova /var/lib/nova/instances /var/lib/nova/tmp`,
  which leaves everything below the three directories alone. Never
  `chown -R`: the instance disks below `instances` belong to libvirt and QEMU
- `[DEFAULT] state_path = /var/lib/nova` in the compute configuration, so
  `compute_id` lands on that mount rather than in nova's default `$pybasedir`

**Final image properties:**

- Runs as `openstack` user (UID 42424, GID 42424)
- Contains no build tools (`gcc`, `pkg-config`, `uv`, `python3-dev` and
  `libvirt-dev` are absent)
- Virtualenv at `/var/lib/openstack` with Nova, its dependencies and
  `libvirt-python`
- `sudo` present with the one `nova-rootwrap` rule

**Image contract check:** `tests/container-images/verify_nova_compute.sh` runs
inline on pull requests and in `verify-nova-compute-image` on push. Its ten
tests, of which test 10 runs right after test 4:

1. `nova-compute`, `nova-manage`, `nova-rootwrap` and `privsep-helper` are
   executable, and `nova-compute --help` exits 0.
2. `import libvirt` succeeds, `libvirt.getVersion()` is at least `10000000`,
   and the installed `libvirt-python` equals the effective pin of the image's
   release. The release is `NOVA_COMPUTE_RELEASE`, which both CI jobs set
   from `matrix.release`; unset, it is the one whose `source-refs.yaml` names
   the image's nova, which needs a tag pin. The pin comes from
   `overrides/<release>/constraints.txt` when that file carries one and from
   `upper-constraints.txt` otherwise, because `checkout-service-source`
   rewrites `upper-constraints.txt` on pull requests. A nova version that no
   release or more than one release carries, a `-libvirt-python` override and a missing
   pin each fail with a message naming the value.
3. `nova.virt.libvirt.driver`, the os-brick iSCSI and NVMe connectors, the
   LUKS encryptor and `vif_plug_ovs.ovsdb.impl_idl` import.
4. The eleven host tools run, `ceph --version` and `rbd --version` among
   them, one assertion per tool, so a missing package names itself.
5. The four identity files are absent.
6. The posture holds: the two `rootwrap.conf` lines, `compute.filters`, six
   trusted paths that the service user cannot write, one `NOPASSWD` entry in
   `sudo -n -l` and a refused `sudo -n true`.
   `sudo -n nova-rootwrap /etc/nova/rootwrap.conf id` exits 99 with
   `Unauthorized command: id`, as the service user and as root.
   `privsep-helper` run through rootwrap exits 1 with
   `ConfigFilesNotFoundError`, which proves rootwrap found it through
   `exec_dirs` (without that entry rootwrap exits 96 with
   `Executable not found`).
7. The container runs as `openstack`.
8. `gcc`, `pkg-config`, `uv`, `python3-dev` and `libvirt-dev` are absent.
9. The state directories match `verify_nova.sh` test 13.
10. The bindings checks of the [glance](#glance) Test 13 pass.
    `nova.storage.rbd_utils.rbd` and `nova.storage.rbd_utils.rados`, the
    attributes `RBDDriver.__init__` checks, are not `None`. `rbd help export`
    and `rbd help import` exit 0, and the
    `ceph --version` line starts with `ceph version `; the script logs that
    line and asserts no version.

The 2026.1 and 2026.2 images pass all 61 assertions. Pointed at the nova
control-plane image, the script exits 1: test 2 reports
`ModuleNotFoundError: No module named 'libvirt'`, test 4 fails once per tool,
and test 10 fails on the missing `ceph-bindings.pth` and
`No module named 'rados'`. A build without any `--build-arg` succeeds and fails
the same three tests, which is how a missing `nova-compute` block in
`extra-packages.yaml` shows up.

## Release-independent images

These images ship software from outside the OpenStack release matrix. They have
no key in `releases/*/source-refs.yaml`, take no `PIP_EXTRAS` or
`EXTRA_APT_PACKAGES` build args, and are built once per upstream version
instead of once per release.

### ovn

**Location:** `images/ovn/Dockerfile`

OVN and Open vSwitch daemons and client tools, compiled from upstream git in a
two-stage build. Both stages start from the same digest-pinned `ubuntu:noble`.

| Property | Value |
| --- | --- |
| Base image | `ubuntu:noble` (Ubuntu 24.04 LTS), both stages, pinned by digest |
| Version pin | `ARG OVN_VERSION=v26.03.2`, the only version input, content-pinned by `ARG OVN_COMMIT` |
| Open vSwitch | The commit the `ovs` submodule gitlink names at that OVN commit, pinned by `ARG OVS_COMMIT` |
| User | `openstack` (UID 42424, GID 42424), created in this Dockerfile |
| Entrypoint | None; every workload names the daemon it runs |

**Stage 1 (`build`):**

- Installs the build requirements from OVN's
  `Documentation/intro/install/general.rst`: `autoconf`, `automake`,
  `build-essential`, `libtool`, `libssl-dev`, `libunbound-dev`,
  `libcap-ng-dev` and `pkg-config`, plus `git`, `ca-certificates` and the
  `python3` that the OVS code generation needs
- Fetches `$OVN_COMMIT` from `https://github.com/ovn-org/ovn.git` — the
  commit, not the tag — reads the OVS commit out of that checkout with
  `git -C /src/ovn rev-parse HEAD:ovs`, aborts the build when it differs from
  `$OVS_COMMIT`, and fetches that commit from
  `https://github.com/openvswitch/ovs.git`
- Builds OVS, then OVN against the OVS source tree
  (`--with-ovs-source=/src/ovs`). Both configure with
  `--prefix=/usr --localstatedir=/var --sysconfdir=/etc` and stage their
  install into `/out` via `make install DESTDIR=/out`

**Stage 2 (runtime):**

- Installs the runtime packages listed below
- Creates the `openstack` user and group
- Copies `/out/usr/bin`, `/out/usr/sbin`, `/out/usr/share/openvswitch` and
  `/out/usr/share/ovn`. Headers, static libraries and man pages stay in the
  build stage
- Creates `/var/run/openvswitch`, `/var/run/ovn`, `/var/lib/openvswitch`,
  `/var/lib/ovn`, `/var/log/openvswitch`, `/var/log/ovn`, `/etc/openvswitch`
  and `/etc/ovn`, all owned by `openstack`
- Sets the OCI labels (title `ovn`, description "OVN and Open vSwitch daemons
  and client tools built from pinned upstream sources", licenses `Apache-2.0`,
  vendor `SAP SE`) and `USER openstack`

**Installed paths:**

| Path | Contents |
| --- | --- |
| `/usr/bin` | `ovn-northd`, `ovn-controller`, `ovn-nbctl`, `ovn-sbctl`, `ovn-appctl`, `ovsdb-tool`, `ovsdb-client`, `ovs-vsctl`, `ovs-appctl`, `ovs-ofctl` |
| `/usr/sbin` | `ovs-vswitchd`, `ovsdb-server` |
| `/usr/share/ovn` | OVN's OVSDB schemas and `scripts/ovn-ctl` |
| `/usr/share/openvswitch` | The OVS OVSDB schemas and `scripts/ovs-ctl` |

**Runtime packages:**

| Package | Purpose |
| --- | --- |
| `ca-certificates` | TLS trust store |
| `iproute2` | `ip`, which the chassis DaemonSets of issue #903 use to inspect interfaces |
| `iputils-ping` | `ping`, which the OVN datapath e2e probes send across the Geneve tunnel |
| `kmod` | `modprobe`, which those DaemonSets use to load host kernel modules |
| `libcap-ng0` | Privilege-drop library the daemons link against |
| `libssl3t64` | OpenSSL 3 runtime for the TLS-protected OVSDB connections |
| `libunbound8` | DNS resolution |

**Version output:** OVN programs print `<prog> 26.03.2` on `--version`,
followed by `Open vSwitch Library 3.7.0`, the OVS revision they were built
against. OVS programs print `<prog> (Open vSwitch) 3.7.0`. Those two numbers
agreeing is what the single pin buys, and
`tests/container-images/verify_ovn.sh` compares them.

**Non-root by default:** `ovsdb-server`, `ovn-northd` and the client tools need
no privileges, so the image runs as `openstack`. The chassis pods of issue #903
set `runAsUser: 0` and the capabilities they need in their own pod spec.

**Version pin:** Open vSwitch has no version of its own to choose. It follows
the `ovs` submodule gitlink (decision D1 of issue #898): one version line to
bump, one Renovate rule. OVN's `Documentation/intro/install/general.rst` notes
under Build Requirements that the submodule is "not recommended to be used as a
source for OVS build"; ovn-kubernetes builds its OVS from that gitlink in
`dist/images/Dockerfile.fedora`, and this image does the same.

`ARG OVN_VERSION` names a git tag, which is a mutable ref: upstream can move or
delete it, and no check on the built image would notice. So each half of the
source tree carries a content pin next to it, standing to `OVN_VERSION` as the
`sha256` digest stands to `ubuntu:noble` — `ARG OVN_COMMIT` for the commit the
tag resolves to, `ARG OVS_COMMIT` for the SHA the `ovs` gitlink names at that
commit. Like a digest, both are what the build fetches: it never resolves the
tag, and it fails when the gitlink at `$OVN_COMMIT` disagrees with
`$OVS_COMMIT`. Neither a moved OVN tag nor a changed gitlink can alter the image
without a change in this repository. Bump both together with `OVN_VERSION`; an
`OVN_COMMIT` left behind by a tag bump surfaces in the version
`tests/container-images/verify_ovn.sh` reads off the binaries, and a stale
`OVS_COMMIT` in the build error.

A Renovate `customManager` tracks `ovn-org/ovn` github-tags on the
`ARG OVN_VERSION` line. Major and minor bumps are disabled to hold the image on
the 26.03 LTS line; patch bumps wait a three-day cooldown and are **not**
automerged, because no datasource can rewrite the two content pins and the
reviewer of the Renovate PR carries them across. See
[Dependency Management](../../contributing/dependency-management.md).

**Image contract check:** `tests/container-images/verify_ovn.sh` runs nine
tests against a built image. Five cover the software: the pinned OVN version on
`ovn-northd`, `ovn-controller`, `ovn-nbctl` and `ovn-sbctl`, the OVS daemons
and clients, the shipped OVS matching the revision OVN links against,
`ovn-ctl` and `ovs-ctl` running under the image's shell, and `ovsdb-tool
create` building a database from each of `ovn-nb.ovsschema`, `ovn-sb.ovsschema`
and `vswitch.ovsschema` — the schemas are the runtime artifact this image
exists to serve, and no other assertion here reads them. The other four cover
the packaging: `ldd` free of unresolved libraries with `libssl.so.3` linked
into the TLS-speaking daemons, no build toolchain and no development headers,
UID 42424 with writable state directories, and `modprobe`, `ip` and `ping` on
`PATH`.

### keystone-federation-proxy

**Location:** `images/keystone-federation-proxy/Dockerfile`

The Apache reverse proxy that terminates OIDC and SAML in front of Keystone: a
single stage on `ubuntu:noble` with the distro `apache2`,
`libapache2-mod-auth-openidc` and `libapache2-mod-auth-mellon`. Every component
comes from the Ubuntu archive, so the image has no version pin of its own and
follows the `noble` package set. Its build, verification and tag scheme are
described in
[build-keystone-federation-proxy / merge-keystone-federation-proxy-image](./build-images-workflow.md#build-keystone-federation-proxy-merge-keystone-federation-proxy-image).

### backup-shifter

**Location:** `images/backup-shifter/Dockerfile`

The rclone shifter for the OVN database backups: a single stage on
`ubuntu:noble` with the distro `rclone` and `ca-certificates`. It is the
`shifter` container of the `OVNCentral` backup CronJob and runs only when
`spec.backup.s3` is set, copying the northbound and southbound snapshots the
`backup` init container left on the PVC to the configured bucket. rclone comes
from the Ubuntu archive, so the image has no version pin of its own and follows
the `noble` package set. The Dockerfile sets no `ENTRYPOINT`: the operator
renders the `rclone copy` command and the `RCLONE_S3_*` environment onto the
CronJob container.

```bash
docker build -t c5c3/backup-shifter:latest images/backup-shifter/

# Run the full image contract check
bash tests/container-images/verify_backup_shifter.sh c5c3/backup-shifter:latest
```

Its build, verification and tag scheme are described in
[build-backup-shifter / merge-backup-shifter-image](./build-images-workflow.md#build-backup-shifter-merge-backup-shifter-image).

### libvirt

**Location:** `images/libvirt/Dockerfile`

The libvirt daemon and QEMU for hypervisor nodes whose host image has neither:
a single stage on `ubuntu:noble` with every package taken from the Ubuntu
archive, so the image has no version pin of its own and follows the `noble`
package set. The hypervisor package of issue #1142 runs it privileged as a
DaemonSet beside `nova-compute`. The `libvirt-e2e` DaemonSet of the
`e2e-nova-libvirt` CI job is its second consumer: it runs the image on a kind
node, where QEMU emulates the guest without KVM.

| Package | Why it is installed |
| --- | --- |
| `dmidecode` | libvirt reads the host's SMBIOS (firmware system information) data with it |
| `iproute2` | `ip`; a Recommends of `libvirt-daemon-system`, which `--no-install-recommends` skips |
| `kmod` | `modprobe`, for loading the `vhost_net` kernel module from the host's module tree |
| `libvirt-clients` | `virsh`, and `virt-admin` for reloading the daemon's TLS certificates |
| `libvirt-daemon-system` | `libvirtd`, `virtlogd` and the QEMU driver for `qemu:///system`, through its dependency `libvirt-daemon`; the configuration under `/etc/libvirt`, the `libvirt` and `kvm` groups and the `libvirt-qemu` user. It pulls in `systemd`, which brings `systemd-run` |
| `ovmf` | UEFI firmware for guests: `OVMF_CODE_4M.fd` and `OVMF_VARS_4M.fd` |
| `qemu-system-x86` | `qemu-system-x86_64`, the emulator for x86 guests; `libvirt-daemon` only recommends a QEMU |
| `qemu-utils` | `qemu-img`, for disk images |

**Why noble:** `images/nova-compute/` builds `libvirt-python` against noble's
libvirt 10.0.0 and installs `libvirt0` at runtime. Building this image on noble
makes the client in `nova-compute` and the daemon here one libvirt.
`tests/container-images/verify_libvirt.sh` pins the major version and fails
unless `libvirtd --version` reports 10. The apt packages carry no version pin
(see `.hadolint.yaml`), so a rebuild can move the libvirt point release; only a
major move fails the check.

**What the image does not carry:** the image has no configuration of its own
and no `ENTRYPOINT` or `CMD`. The consumer renders `libvirtd.conf`, `qemu.conf`
and the start command. `libvirt-daemon-system` depends on
`libvirt-daemon-config-network`, which defines the `default` NAT network and
links it into `/etc/libvirt/qemu/networks/autostart/`. The consumer runs
libvirtd in the host's network namespace, where that network would create
`virbr0` and NAT rules on the node, so the Dockerfile removes the autostart
link and keeps the definition. `dnsmasq-base`, which the network would serve
DHCP and DNS with, is a Recommends and is not installed.

**Runs as root:** libvirtd has to run as root, so the image has no `USER`
instruction and creates no `openstack` user (see
[Design Deviations](#design-deviations)). The packages create the
`libvirt-qemu` user (UID 64055) and the `libvirt` and `kvm` groups, and QEMU
runs guests as `libvirt-qemu` with the group `kvm`. The package scripts assign
the two group IDs at build time, and they are not part of the image's
contract: the consumer sets the socket's ownership in its own configuration.

The same applies to `/dev/kvm`. A privileged container gets its own device
node with the host's group ID and mode. Debian and Ubuntu hosts create it as
`root:kvm` with mode `0660`, and their `kvm` group ID usually differs from the
image's, so QEMU cannot open the device and a guest with `virt_type=kvm` does
not start. Before it starts libvirtd, the consumer runs
`chown root:kvm /dev/kvm && chmod 0660 /dev/kvm` on the container's own device
node. On a hostPath mount of the host's `/dev` the same command would change
the host's device.

**Tags:** CI publishes `ghcr.io/c5c3/libvirt` as `latest` and `<sha>`, and on
`main` as `<libvirt-package-version>-r<N>`, such as `10.0.0-2ubuntu8.19-r1` (see
the [tag table](./build-images-workflow.md#release-independent-images)). Every
run of `build-images.yaml` that is not a pull request rebuilds the image and
moves `latest` to the new manifest: a push that touches any image, a change
under `images/libvirt/` included, and the dispatch a base image refresh starts.
The keeper tag is minted once per value and never moved, so it stays on the
first build that had it, and
[Retention](./build-images-workflow.md#retention) keeps every manifest that
carries one. A manifest left with its `<sha>` tag alone is deleted once it is
older than 24 hours. A consumer therefore pins the keeper tag by digest, as the
lab does with `ghcr.io/c5c3/libvirt:<tag>@sha256:<digest>`.

```bash
docker build -t c5c3/libvirt:latest images/libvirt/

# Run the full image contract check
bash tests/container-images/verify_libvirt.sh c5c3/libvirt:latest
```

The contract check runs without `--privileged`. Its last test starts `libvirtd`
in the container, waits at most 30 seconds for `/run/libvirt/libvirt-sock` and
asks the QEMU driver for its version and its x86_64 domain capabilities.

Its build, verification and tag scheme are described in
[build-libvirt / merge-libvirt-image](./build-images-workflow.md#build-libvirt-merge-libvirt-image).

### openstack-hypervisor-operator

**Location:** `images/openstack-hypervisor-operator/Dockerfile`

openstack-hypervisor-operator (hvo) from `cobaltcore-dev`, compiled from a
pinned commit of its upstream `main` branch with the patches under
`images/openstack-hypervisor-operator/patches/` applied. The hypervisor package
of issue #1142 deploys it with the upstream Helm chart of the same commit:
`deploy/lab/metal-stack/hypervisor/hvo-release.yaml` runs it on the
metal-stack lab (see
[Lab hypervisors](../infrastructure/infrastructure-manifests.md#lab-hypervisors)).

| Property | Value |
| --- | --- |
| Build stage | `golang:1.27`, pinned by the digest `operators/Dockerfile` carries |
| Runtime base | `gcr.io/distroless/static:nonroot`, pinned by the digest `operators/Dockerfile` carries |
| Version pin | `ARG HVO_COMMIT`, a 40-character commit of upstream `main` (`hack/ci-resolve-hvo-commit.sh` prints it) |
| User | `65532:65532`, the `nonroot` user of the distroless base |
| Entrypoint | `/usr/bin/manager` |
| Label | `io.c5c3.upstream-commit`, set to the pinned commit |

**Build stage:**

- Fetches `$HVO_COMMIT` from
  `https://github.com/cobaltcore-dev/openstack-hypervisor-operator.git` by its
  SHA, so no branch or tag decides what is compiled. The workflow token reaches
  the fetch as the optional `github_token` secret, as for [ovn](#ovn)
- Downloads the Go modules in a layer of their own before the patches, so CI's
  layer cache keeps them when only a patch or a later step changes
- Refreshes the git index, then applies every `*.patch` under `patches/` with
  `git apply --index`. A patch that does not apply fails the build with
  `patch does not apply: /patches/<file>`. The refresh is for a checkout layer
  that CI restores from the build cache: its files have new inodes and change
  times, and `git apply --index` would reject each one with
  `does not match index`
- Fails when Go code outside the tests still names gophercloud's
  `servers.LiveMigrateOpts`, whose `BlockMigration` cannot carry `"auto"`.
  The grep matches the bare type name, so a literal, a `var`, `new()`, a
  pointer and an aliased import all fail. Only a `//` outside a double-quoted
  string before the name lets the line pass, so a comment passes while a `/`
  in a string or a division does not. That catches a live migration a pin
  move adds or reshapes, and a re-cut patch that lost its `controller.go` hunk
- Runs `TestLiveMigrateAutoBody`, the test patch 0001 brings, and fails unless
  the log shows `--- PASS: TestLiveMigrateAutoBody`. `go test -run` exits 0
  with `[no tests to run]` when the test is missing, so the grep is what
  catches a patch that lost its test hunk
- Runs `TestHypervisorCreatedWithDefaultHighAvailability` and
  `TestTraitsInSyncSetsTraitsUpdated`, the tests of patches 0002 and 0003, in
  one `go test` of `./internal/controller/`, and fails unless the log shows the
  top-level `--- PASS:` line of each. The `-run` expression does not match the
  package's Ginkgo entry point, `TestControllers` in `suite_test.go`, so the
  run needs no envtest binary. The space after each test name in the grep
  keeps a subtest's PASS line from matching
- Runs `TestServiceClientInterface`, the test of patch 0004, in a `go test` of
  `./internal/openstack/`, and fails unless the log shows
  `--- PASS: TestServiceClientInterface ` with the same trailing space. The
  `-run` expression does not match that package's Ginkgo entry point,
  `TestOpenstack` in `suite_test.go`, so the suite does not run
- Builds `./cmd` with `CGO_ENABLED=0`, `GOTOOLCHAIN=local` and upstream's
  ldflags, with the version set to `sha-<commit>`, the tag the image is
  published under. `manager --version` therefore prints
  `manager sha-<commit> (linux/<arch>) <commit>`. Upstream's `generate` step
  is skipped: the generated files are committed

**Why `main`:** `main` has the required `--agent-namespaces` flag, the
offboarding taint and parallel migrations in an Eviction
(`--eviction-concurrency`). The latest tag, v1.2.3, has none of them. Upstream
publishes a chart for every `main` commit, version `1.2.3+sha-<short>` with
`appVersion` `sha-<full commit>`, and that chart renders the image as
`<repository>:<appVersion>`. Upstream pushes no image under that tag for a
`main` commit, only a moving `latest`. This image is published as
`sha-<commit>`, so the chart of the pinned commit runs it with only the
repository overridden.

**Source patches:** four files under
`images/openstack-hypervisor-operator/patches/`, applied in order. Each brings a
plain Go test outside upstream's Ginkgo suite, which the build step runs.

`0001-eviction-let-nova-choose-block-migration.patch`. Upstream's `liveMigrate` (`internal/controller/eviction/controller.go`) asks
Nova for a live migration with `block_migration: false`. Nova refuses that for
a server on the hypervisor's local disks with `InvalidSharedStorage`. Every
server on the metal-stack lab boots from a local disk, so the Eviction cannot
move it. From compute microversion 2.25 on, Nova accepts the string `"auto"`
and chooses block migration per server. gophercloud types
`LiveMigrateOpts.BlockMigration` as `*bool`, so the patch adds a
`LiveMigrateOptsBuilder` of its own that sends
`{"os-migrateLive": {"block_migration": "auto", "host": null}}`, plus the plain
Go test `TestLiveMigrateAutoBody` that pins the body; no upstream test pins
it.

`0002-hypervisor-make-the-high-availability-default-configurable.patch`.
Upstream's `HypervisorController.Reconcile`
(`internal/controller/hypervisor_controller.go`) creates every `Hypervisor`
with `spec.highAvailability: true`, and onboarding then waits for the
`HaEnabled` condition, which only SAP's kvm-ha-service sets. The patch adds the
flag `--default-high-availability` (default `true`, upstream's behaviour) and
writes its value on create; an existing `Hypervisor` keeps its value.
`TestHypervisorCreatedWithDefaultHighAvailability` reconciles a Node with the
fake client and asserts `true` for the default and `false` once the flag's
variable is `false`. The lab release sets the flag to `false`.

`0003-traits-report-traitsupdated-when-nothing-differs.patch`. Upstream's
`TraitsController.Reconcile` (`internal/controller/traits_controller.go`) sets
`TraitsUpdated` only on the path that calls Placement, so only when a custom
trait differs, while onboarding and the aggregates controller wait for
`TraitsUpdated=True`. The patch sets the condition, reason `Succeeded` and the
message `Custom traits are in sync`, when nothing differs, without calling
Placement and without writing `status.traits`. `TestTraitsInSyncSetsTraitsUpdated`
runs the controller with no Placement client: the condition in `Handover`, no
second write on a repeat, and no condition in `Testing`.

`0004-openstack-select-the-catalog-interface-with-os-interface.patch`.
Upstream's `ServiceClientFromProvider` (`internal/openstack/service_client.go`)
builds every OpenStack client of the operator from an empty
`gophercloud.EndpointOpts`, so hvo reads the catalog's `public` endpoints and
no other. The patch reads the environment variable `OS_INTERFACE`: `public`,
`internal` or `admin` selects that interface, and unset or empty keeps
`public`, upstream's behaviour. Any other value, `Internal` included, is an
error before the catalog is read, and hvo exits at start. An error of the
catalog lookup, gophercloud's `ErrEndpointNotFound` when the catalog has no
endpoint of that interface, is returned unchanged.
`TestServiceClientInterface` builds a compute client on a provider whose
endpoint locator records the options it gets: for each of the three values,
for unset and empty, for `Internal`, `internalURL` and `internal` with a
leading space or a trailing newline (no lookup happens) and for a locator
error. The lab release sets `internal`, whose endpoints are the in-cluster
Service URLs.

Each patch header records `Upstream status: not submitted`. Their author
submits them upstream, and issue #1066 tracks them until `main` carries them.

**Tags:** CI publishes `ghcr.io/c5c3/openstack-hypervisor-operator` as
`sha-<hvo-commit>-<sha>` on every push, and on `main` also as
`sha-<hvo-commit>`, `upstream-<hvo-commit>` and `latest` (see the
[tag table](./build-images-workflow.md#release-independent-images)). The
upstream chart resolves `sha-<hvo-commit>`. That shape is a build artifact to
[Retention](./build-images-workflow.md#retention), which would delete it once a
newer pin holds `latest`, while a chart of the older pin still names it.
`upstream-<hvo-commit>` sits on the same manifest and is a keeper tag, so every
pin that reached `main` stays pullable under both tags.

**Pin moves:** Renovate tracks the `ARG HVO_COMMIT` line as a git-refs digest
of upstream `main`, weekly and without automerge (see
[Dependency Management](../../contributing/dependency-management.md)).
`hack/ci-resolve-hvo-commit.sh` is the only parser of the line. A new commit
on which a patch no longer applies fails the build at the `git apply` step;
that patch is then re-cut against the new commit. Once upstream carries a
patch's change, the patch is dropped together with its checks in the build
step: for 0001 the `servers.LiveMigrateOpts` grep and the
`TestLiveMigrateAutoBody` run, for 0002 and 0003 the test's name in the
controller `go test` run and the `grep` for its `--- PASS:` line, and that run
itself once neither test is left; for 0004 the `TestServiceClientInterface` run
and its `grep`. With the last patch gone, the
`COPY patches/` and `git apply` steps go too, because both fail without a
patch. A pin move also checks what
[Connect a Compute Cluster](../../guides/nova/connect-a-compute-cluster.md),
[Drain a Compute Node](../../guides/nova/drain-a-compute-node.md) and the
table [Namespaces on a compute cluster](../target-clusters.md#namespaces-on-a-compute-cluster)
say about hvo against the new commit, and updates the commit the two guides
name.

**Image contract check:** `tests/container-images/verify_hvo.sh` runs four
tests against a built image. `manager --version` names `sha-<pin>` and ends
with the pin. `manager --help` lists `-agent-namespaces` and
`-eviction-concurrency`, which v1.2.3 does not have, and
`-default-high-availability`, which only patch 0002 brings. The image runs
`/usr/bin/manager` as `65532:65532`, and its `io.c5c3.upstream-commit` label
equals the pin. The script reads the usage text instead of starting the binary
without flags: upstream logs its `--agent-namespaces is required` error before
it installs a logger, so that message never reaches the output.

Its build, verification and tag scheme are described in
[build-hvo / merge-hvo-image / verify-hvo-image](./build-images-workflow.md#build-hvo-merge-hvo-image-verify-hvo-image).

### kvm-node-agent

**Location:** `images/kvm-node-agent/Dockerfile`

kvm-node-agent (kna) from `cobaltcore-dev`, compiled from a pinned commit of
its upstream `main` branch with the patches under
`images/kvm-node-agent/patches/` applied. The agent runs on every hypervisor
node: it reports libvirt's state into the node's `Hypervisor` and installs the
node's libvirt TLS files under the host's `/etc/pki`.
`deploy/lab/metal-stack/hypervisor/kna-release.yaml` runs it with the upstream
Helm chart of the same commit (see
[Lab hypervisors](../infrastructure/infrastructure-manifests.md#lab-hypervisors)).

| Property | Value |
| --- | --- |
| Build stage | `golang:1.27`, pinned by the digest `operators/Dockerfile` carries |
| Runtime base | `gcr.io/distroless/static:nonroot`, pinned by the digest `operators/Dockerfile` carries |
| Version pin | `ARG KNA_COMMIT`, a 40-character commit of upstream `main` (`hack/ci-resolve-kna-commit.sh` prints it) |
| User | `0:0` (see [Design Deviations](#design-deviations)) |
| Entrypoint | `/usr/bin/manager` |
| Label | `io.c5c3.upstream-commit`, set to the pinned commit |

**Build stage:**

- Fetches `$KNA_COMMIT` from
  `https://github.com/cobaltcore-dev/kvm-node-agent.git` by its SHA, with the
  optional `github_token` secret, as for
  [openstack-hypervisor-operator](#openstack-hypervisor-operator)
- Downloads the Go modules in a layer of their own, then refreshes the git
  index and applies every `*.patch` under `patches/` with `git apply --index`,
  as for [openstack-hypervisor-operator](#openstack-hypervisor-operator). A
  patch that does not apply fails the build with
  `patch does not apply: /patches/<file>`
- Compiles the test binary of `internal/certificates` once, with `-trimpath`
  as in the build below, so that the build reuses the packages compiled for
  the test, and runs the two tests patch 0001 brings from it:
  `TestUpdateTLSCertificateKeyMode` as root, then
  `TestUpdateTLSCertificateKeyGroupNotPermitted` as uid 65534 through
  `setpriv --reuid=65534 --regid=65534 --clear-groups`. The build fails unless
  each log shows the test's `--- PASS:` line followed by a space, which a
  subtest's line does not match. A test binary exits 0 with
  `testing: warning: no tests to run` when a test is missing, so the greps are
  what catch a patch that lost its test file. The second test skips as root
  and prints `--- SKIP`, so its grep also fails when that run is not
  unprivileged. `tests/unit/images/kna_patch_test_guard_test.sh` runs both
  greps against such logs
- Builds `./cmd` with `CGO_ENABLED=0`, `GOTOOLCHAIN=local` and upstream's
  ldflags without the build date, with the version set to `sha-<commit>`, the
  tag the image is published under. `manager --version` therefore prints
  `manager version sha-<commit> ()`; the parentheses hold the unset build
  date. Upstream's `generate` step is skipped: the generated files are
  committed

**Why `main`:** upstream publishes a chart for every `main` commit, version
`0.2.0+sha-<short>` with `appVersion` `sha-<full commit>`, and that chart
renders the image as `<repository>:<appVersion>`. Upstream pushes no image
under that tag: `ghcr.io/cobaltcore-dev/kvm-node-agent:sha-<commit>` answers
404. A fix the agent needs on hosts outside SAP ships here as a patch while it
is proposed upstream. This image is published as `sha-<commit>`, so the chart
of the pinned commit runs it with only the repository overridden.

**Source patch:**
`images/kvm-node-agent/patches/0001-certificates-restrict-private-key-file-modes.patch`.
Upstream's `UpdateTLSCertificate` (`internal/certificates/manage_libvirt.go`)
writes every file of the node's TLS Secret with mode 0644, the private key
included (#1175). Every local user of the node can then read the key that
authenticates it to every libvirtd of its migration domain. With the patch:

- `libvirt/private/serverkey.pem`, `qemu/server-key.pem` and
  `ch/server-key.pem` get mode 0600, and the six certificate files keep 0644.
  A key target that a later upstream commit adds gets 0600 too.
- When the environment variable `PKI_KEY_GROUP` holds a numeric group ID,
  `qemu/server-key.pem` and `ch/server-key.pem` get that group and mode 0640.
  With native TLS migration, QEMU opens its key itself, as the user it runs
  as, and Cloud Hypervisor reads its own the same way.
  `libvirt/private/serverkey.pem` keeps mode 0600, but it holds the same key,
  and libvirt's client key links to it: the group can read the key the node
  authenticates with to the libvirtd of its peers as well. Only a number is
  accepted, because the distroless image has no group database, and an
  invalid value fails the update before any file is written. The agent has to
  belong to the group or hold `CAP_CHOWN`; otherwise the update fails with
  `operation not permitted` and replaces no file. Unset or empty, the variable
  changes no group.
  The upstream chart has no value for it, so a host that needs it sets it
  with a post-renderer.
- Every file is written to a temporary file in its own directory, and the
  temporary files are renamed over their targets only once all of them are
  written. `os.WriteFile` keeps the mode of a file that already exists, so the
  rename is what tightens a key an earlier version left at 0644. It needs
  write permission on the directory and no `CAP_FOWNER`, and no reader sees a
  partly written key. A write that fails leaves the files of the previous
  Secret in place instead of a new certificate beside an old key.

The agent writes the files only when the Secret's `resourceVersion` changes,
so a node keeps the keys it has until its certificate is reissued.
`TestUpdateTLSCertificateKeyMode` walks the PKI directory in eight subtests and
fails for any file with the key's content that is not a key target, for a key
file with another mode or group, and for any other file whose mode is not
0644. `TestUpdateTLSCertificateKeyGroupNotPermitted` sets a group the process
does not belong to and expects a failed update that leaves every file of the
previous Secret in place and no temporary file behind. A root process may give
a file any group, so that test skips as root, and the build runs it a second
time as uid 65534.
The patch header records `Upstream status: not submitted`. Its author submits
it upstream; #1175 stays open until upstream `main` carries the change, and
issue #1066 tracks it.

**Tags:** CI publishes `ghcr.io/c5c3/kvm-node-agent` as
`sha-<kna-commit>-<sha>` on every push, and on `main` also as
`sha-<kna-commit>`, `upstream-<kna-commit>` and `latest` (see the
[tag table](./build-images-workflow.md#release-independent-images)). The
upstream chart resolves `sha-<kna-commit>`, and `upstream-<kna-commit>` keeps
that manifest through [Retention](./build-images-workflow.md#retention), as
for openstack-hypervisor-operator.

**Pin moves:** Renovate tracks the `ARG KNA_COMMIT` line as a git-refs digest
of upstream `main`, weekly and without automerge (see
[Dependency Management](../../contributing/dependency-management.md)).
`hack/ci-resolve-kna-commit.sh` is the only parser of the line. The lab's chart
ref in `deploy/lab/metal-stack/hypervisor/sources.yaml` moves with the pin by
hand, and `tests/unit/deploy/metal_stack_hypervisor_test.sh` fails while the
chart's short SHA differs from it. A new commit on which the patch no longer
applies fails the build at the `git apply` step; the patch is then re-cut
against the new commit. Once upstream carries the change, the patch goes
together with its two test runs in the build step, and with the last patch
the `COPY patches/` and `git apply` steps go too.

**Image contract check:** `tests/container-images/verify_kna.sh` runs four
tests against a built image. The first line of `manager --version` is
`manager version sha-<pin> ()`. `manager --help` exits 0 and lists
`-health-probe-bind-address`, the flag the chart passes; a binary that needed
a C library would not start on the static base at all. The image runs
`/usr/bin/manager` as `0:0`, and its `io.c5c3.upstream-commit` label equals
the pin.

Its build, verification and tag scheme are described in
[build-kna / merge-kna-image / verify-kna-image](./build-images-workflow.md#build-kna-merge-kna-image-verify-kna-image).

## Named Build Contexts

Service Dockerfiles use Docker's named build context feature (`--build-context`) to inject
release-specific files without embedding them in the Dockerfile or using `COPY` from the
build directory. This keeps Dockerfiles release-independent.

A local build passes two named build contexts (shown here for Keystone; the
Horizon build is identical with `horizon` in place of `keystone`). CI passes two
more, `python-base` and `venv-builder`, as `docker-image://` references to the
digests `merge-base-images` published. The figure under
[Dockerfile Hierarchy](#dockerfile-hierarchy) shows where each context and each
build arg enters the build.

| Context name | Contents | Mounted as |
| --- | --- | --- |
| `upper-constraints` | Release directory containing `upper-constraints.txt` | `/tmp/upper-constraints.txt` |
| `keystone` | Keystone source tree (git checkout at the version from `source-refs.yaml`) | `/tmp/keystone` |

These are passed to `docker build` via `--build-context` flags:

```bash
docker build images/keystone \
  --build-context keystone=src/keystone \
  --build-context upper-constraints=releases/2026.1/
```

Inside the Dockerfile, named build contexts are consumed via `--mount=type=bind,from=`.
Extras are injected via `ARG PIP_EXTRAS` (comma-separated; keystone passes `ldap`)
which the CI workflow reads from `extra-packages.yaml`:

```dockerfile
ARG PIP_EXTRAS=""
ARG PIP_PACKAGES=""

RUN --mount=type=bind,from=upper-constraints,source=upper-constraints.txt,target=/tmp/upper-constraints.txt \
    --mount=type=bind,from=keystone,target=/tmp/keystone,readwrite \
    PKG="/tmp/keystone" && \
    if [ -n "$PIP_EXTRAS" ]; then PKG="${PKG}[${PIP_EXTRAS}]"; fi && \
    uv pip install --prefix /var/lib/openstack \
        --constraint /tmp/upper-constraints.txt \
        "$PKG" $PIP_PACKAGES
```

The `from=upper-constraints` directive tells Docker to resolve the file from the named
build context rather than the Dockerfile's primary build context. The `source=` parameter
selects a specific file within that context.

## Release Configuration

All release-specific configuration lives under `releases/<release>/` (e.g.,
`releases/2026.1/`). These files are the single source of truth for what gets built.
Adding a new service or updating a version requires editing only these files — not
Dockerfiles.

### source-refs.yaml

**Location:** `releases/<release>/source-refs.yaml`

Maps each OpenStack component to a git ref (tag, branch, or commit SHA) specifying
the version to build.

**Format:**

```yaml
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

keystone: "28.0.0"
```

Each key is a service name matching the Dockerfile directory under `images/`. Values
are quoted strings representing git refs — typically release tags (e.g., `"28.0.0"`).

To add a new service, add a single line: `<service>: "<git-ref>"`.

### upper-constraints.txt

**Location:** `releases/<release>/upper-constraints.txt`

Contains pinned Python dependency versions from the OpenStack requirements repository
(`stable/<release>` branch). This file is committed as-is from the upstream repository
to enable Renovate tracking and `git diff` for constraint changes.

**Format:**

```text
cryptography===44.0.0
oslo.limit===2.8.0
keystonemiddleware===10.9.0
```

Each line pins a single package using the `===` (arbitrary equality) operator. This is
the standard format used by OpenStack's global requirements process.

**Source:** `https://raw.githubusercontent.com/openstack/requirements/stable/<release>/upper-constraints.txt`

### extra-packages.yaml

**Location:** `releases/<release>/extra-packages.yaml`

Defines per-service Python extras and runtime system packages that are not part of the
core OpenStack package.

**Format:**

```yaml
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

keystone:
  pip_extras:
    - ldap
  pip_packages: []
  apt_packages:
    - libapache2-mod-wsgi-py3
    - libldap2
    - libsasl2-2
    - libxml2
```

| Key | Purpose |
| --- | --- |
| `<service>.pip_extras` | Bare Python extra names combined with the service name to form install arguments (e.g. `keystone[ldap]`). Passed as the `PIP_EXTRAS` build arg. |
| `<service>.pip_packages` | Additional pip packages to install alongside the service (space-separated in the build arg `PIP_PACKAGES`). Use an empty list (`[]`) when none are needed. |
| `<service>.apt_packages` | Runtime system packages installed via `apt` in the final image. Passed as the `EXTRA_APT_PACKAGES` build arg. |

To add packages for a new service, add a new top-level key matching the service name
with both `pip_extras` and `apt_packages` lists.

`nova-compute` is the one key that is not a service. It configures the image of
`images/nova-compute/`, which is built from the `nova` pin and has no
`source-refs.yaml` key of its own; `verify_release_config.sh` checks its block
through its `DERIVED_IMAGES` list.

## Constraint Overrides

The constraint override system allows selective modification of individual package
pins in `upper-constraints.txt` without replacing the entire file. This is useful for
applying security fixes or version bumps for individual packages.

### Override Format

Override files are placed at `overrides/<release>/constraints.txt`. Each line is one
of three types:

| Syntax | Action | Example |
| --- | --- | --- |
| `package===version` | Set the pin for `package`: an existing pin is removed and the line is appended, so a package without a pin gets one | `cryptography===44.0.1` |
| `-package` | Remove `package` from constraints entirely | `-oslo.messaging` |
| `# comment` or blank | Skipped (no action) | `# Security fix for CVE-2025-1234` |

**Example override file** (`overrides/2026.1/constraints.txt`):

```text
# Security fix: bump cryptography for CVE-2025-1234
cryptography===44.0.1

# Remove oslo.messaging pin to allow newer version
-oslo.messaging
```

**Real-world use — the horizon self-pin:** `upper-constraints.txt` pins `horizon===`
itself (unlike keystone, which never appears there). A source install with
`--constraint` refuses to install the horizon source tree against its own pin, so
`overrides/<release>/constraints.txt` ships a `-horizon` removal line for every
release. The git ref in `source-refs.yaml` stays the single source of truth for what
is built, independent of the upstream pin.

### Script Usage

**Location:** `scripts/apply-constraint-overrides.sh`

```bash
# Apply overrides for the 2026.1 release
./scripts/apply-constraint-overrides.sh 2026.1
```

**Behavior:**

| Condition | Result |
| --- | --- |
| `overrides/<release>/constraints.txt` exists | Each line is processed: a `package===version` line deletes any existing pin with `sed` and is appended to the file, a `-package` line deletes the pin with `sed` |
| `overrides/<release>/constraints.txt` does not exist | Script exits with code 0, no changes made (idempotent) |

The script reads `releases/<release>/upper-constraints.txt` relative to the current working
directory (must be invoked from the repository root) and modifies it in-place. It uses GNU
`sed -i` for modifications (default on Ubuntu/CI runners — BSD `sed` is not supported).

**Arguments:**

| Argument | Required | Description |
| --- | --- | --- |
| `<release>` | Yes | Release identifier (e.g., `2026.1`), used to locate `overrides/<release>/constraints.txt` |

## Local Build Instructions

Build the complete image chain locally for development and verification:

### Step 1: Build base images

```bash
# Build python-base (tag must match FROM python-base in downstream Dockerfiles)
docker build images/python-base -t python-base

# Build venv-builder (tag must match FROM venv-builder in keystone Stage 1)
docker build images/venv-builder -t venv-builder
```

The tag names (`python-base`, `venv-builder`) must match the `FROM` directives in
downstream Dockerfiles. Docker resolves `FROM python-base` to the local image.

To also apply the tags that `verify_python_base.sh` and `verify_venv_builder.sh` use as
their default image, add a second `-t` flag:

```bash
docker build images/python-base -t python-base -t c5c3/python-base:3.12-noble
docker build images/venv-builder -t venv-builder -t c5c3/venv-builder:3.12-noble
```

### Step 2: Clone the service source

```bash
git clone --branch 29.0.0 --depth 1 \
  https://github.com/openstack/keystone.git src/keystone
```

The branch/tag must match the version specified in `releases/2026.1/source-refs.yaml`.

### Step 3: Build the service image

Extras are read from `extra-packages.yaml` and passed as `--build-arg`:

```bash
docker build images/keystone \
  -t c5c3/keystone:29.0.0 \
  --build-arg PIP_EXTRAS=ldap \
  --build-arg "EXTRA_APT_PACKAGES=libapache2-mod-wsgi-py3 libldap2 libsasl2-2 libxml2" \
  --build-context keystone=src/keystone \
  --build-context upper-constraints=releases/2026.1/
```

### Step 4: Verify the image

```bash
# Verify Keystone CLI is functional
docker run --rm c5c3/keystone:29.0.0 keystone-manage --version

# Verify non-root execution
docker run --rm c5c3/keystone:29.0.0 whoami
# Expected output: openstack

# Verify no build tools in final image
docker run --rm c5c3/keystone:29.0.0 which gcc \
  && echo "FAIL: gcc found in image" \
  || echo "PASS: gcc not found"
```

### Building horizon locally

The horizon build follows the same steps with two differences: the constraint override
must be applied first (it strips the `horizon===` self-pin in-place), and one build arg
is needed, `EXTRA_APT_PACKAGES=libpython3.12t64` (the other horizon lists in
`extra-packages.yaml` are empty):

```bash
# Strip the horizon=== pin from upper-constraints.txt (GNU sed; run on Linux/CI)
./scripts/apply-constraint-overrides.sh 2026.1

git clone --branch 25.7.0 --depth 1 \
  https://opendev.org/openstack/horizon.git src/horizon

docker build images/horizon \
  -t c5c3/horizon:25.7.0 \
  --build-arg EXTRA_APT_PACKAGES=libpython3.12t64 \
  --build-context horizon=src/horizon \
  --build-context upper-constraints=releases/2026.1/

# Run the full image contract check
bash tests/container-images/verify_horizon.sh c5c3/horizon:25.7.0
```

### Building nova-compute locally

The compute image builds from the nova source at the release's pin, with the
two build args read from the `nova-compute` block by mikefarah `yq` v4 (the
version CI pins). After Step 1:

```bash
git clone --branch 33.0.0 --depth 1 \
  https://opendev.org/openstack/nova.git src/nova

docker build images/nova-compute \
  -t c5c3/nova-compute:33.0.0 \
  --build-context nova=src/nova \
  --build-context upper-constraints=releases/2026.1/ \
  --build-arg "PIP_PACKAGES=$(yq -r '."nova-compute".pip_packages | join(" ")' releases/2026.1/extra-packages.yaml)" \
  --build-arg "EXTRA_APT_PACKAGES=$(yq -r '."nova-compute".apt_packages | join(" ")' releases/2026.1/extra-packages.yaml)"

# Run the full image contract check
bash tests/container-images/verify_nova_compute.sh c5c3/nova-compute:33.0.0
```

For 2026.2, clone `34.0.0` and read `releases/2026.2/`. The contract script
finds the release from the nova version inside the image, so a local run
needs no release argument. `NOVA_COMPUTE_RELEASE=<release>` names it instead,
as CI does.

The install step compiles libvirt-python. When it stops with
``Failed to build `libvirt-python==<pin>` ``, the build stage could not find the
libvirt headers or their pkg-config file, which `libvirt-dev` and `pkg-config`
provide. A build without the two build args succeeds but ships neither the
binding nor the host tools, and the contract script then fails tests 2 and 4.

### Building ovn locally

The ovn build needs no source checkout and no build args: the Dockerfile fetches OVN at
the pinned commit (`OVN_COMMIT`) and Open vSwitch at the pinned commit (`OVS_COMMIT`)
itself, and fails when the `ovs` gitlink of the OVN checkout names a different commit.
One script covers it. Set `GITHUB_TOKEN` when the anonymous fetch inside the build fails
with `could not read Username for 'https://github.com'`; the script mounts it as a
BuildKit secret and the fetch is authenticated (CI always does).

```bash
# Builds c5c3/ovn:<pinned version>
hack/ci-build-ovn-image.sh

# Same build under a different name
OVN_IMAGE=ghcr.io/c5c3/ovn:dev hack/ci-build-ovn-image.sh

# Print the pin the build used (26.03.2, without the leading v)
hack/ci-resolve-ovn-version.sh

# Run the full image contract check
bash tests/container-images/verify_ovn.sh c5c3/ovn:$(hack/ci-resolve-ovn-version.sh)
```

Both projects are compiled from source, so the first build takes a while. The
Dockerfile's BuildKit cache mounts keep the apt steps of a rebuild short, but
the two `make -j"$(nproc)"` runs dominate either way.

### Building openstack-hypervisor-operator locally

The build needs no source checkout and no build args: the Dockerfile fetches
the pinned commit, applies the patches and runs their tests itself. Pass
`GITHUB_TOKEN` as a BuildKit secret when the anonymous fetch inside the build
fails; CI always does.

```bash
docker build images/openstack-hypervisor-operator -t openstack-hypervisor-operator

# The same build with an authenticated fetch
docker build --secret id=github_token,env=GITHUB_TOKEN \
  images/openstack-hypervisor-operator -t openstack-hypervisor-operator

# Print the pinned upstream commit
hack/ci-resolve-hvo-commit.sh

# Run the full image contract check against openstack-hypervisor-operator
bash tests/container-images/verify_hvo.sh
```

`--progress=plain` keeps the `--- PASS:` lines of `TestLiveMigrateAutoBody`,
`TestHypervisorCreatedWithDefaultHighAvailability`,
`TestTraitsInSyncSetsTraitsUpdated` and `TestServiceClientInterface` in the
build output.

### Building kvm-node-agent locally

The build needs no source checkout and no build args: the Dockerfile fetches
the pinned commit, applies the patches and runs the patch's two tests itself.
Pass `GITHUB_TOKEN` as a BuildKit secret when the anonymous fetch inside the
build fails; CI always does.

```bash
docker build images/kvm-node-agent -t kvm-node-agent

# The same build with an authenticated fetch
docker build --secret id=github_token,env=GITHUB_TOKEN \
  images/kvm-node-agent -t kvm-node-agent

# Print the pinned upstream commit
hack/ci-resolve-kna-commit.sh

# Run the full image contract check against kvm-node-agent
bash tests/container-images/verify_kna.sh
```

`--progress=plain` keeps the `--- PASS: TestUpdateTLSCertificateKeyMode` and
`--- PASS: TestUpdateTLSCertificateKeyGroupNotPermitted` lines in the build
output.

## Design Deviations

The implementation deviates from the original design document in four areas,
documented with `# DEVIATION` comments in the affected Dockerfiles:

**Generic `openstack` user instead of per-service users:**

The original design's Keystone Dockerfile example creates a per-service user
(e.g., `groupadd keystone` / `useradd keystone`). The implementation uses a single
generic `openstack` user (UID/GID 42424) defined in `python-base` and shared by all
service images. This reduces complexity and image layers — each service image inherits
the user via `USER openstack` without needing its own user creation step.

The `# DEVIATION` comment appears in `images/python-base/Dockerfile` (where the
user is created) and in every service Dockerfile that uses it instead of a
per-service user (`images/keystone/Dockerfile`, `images/horizon/Dockerfile`,
`images/glance/Dockerfile`, `images/placement/Dockerfile`,
`images/barbican/Dockerfile`, `images/neutron/Dockerfile`,
`images/cinder/Dockerfile`, `images/nova/Dockerfile`,
`images/nova-compute/Dockerfile`).

`images/ovn/Dockerfile` and `images/backup-shifter/Dockerfile` carry the comment
for the other half of the same decision. Neither derives from `python-base`, so
each creates the `openstack` user and group itself. For `ovn` the distro
packages would install separate `openvswitch` and `ovn` users, and the source
build carries no such packaging. The `rclone` package that `backup-shifter`
installs brings no service account at all. One identity across all non-root
images keeps the pod security contexts uniform, and in the `OVNCentral` backup
CronJob it lets the `backup` init container and the `shifter` container share a
single pod security context.

**Root instead of the `openstack` user (libvirt):**

`images/libvirt/Dockerfile` keeps root and creates no `openstack` user, and
carries a `# DEVIATION` comment saying so. libvirtd has to run as root to
manage domains, devices and cgroups on the node, and the libvirt packages
create the `libvirt-qemu` user that QEMU runs guests as.

**Distroless and UID 65532 instead of `python-base` (openstack-hypervisor-operator):**

`images/openstack-hypervisor-operator/Dockerfile` does not derive from
`python-base` and creates no `openstack` user, and carries a `# DEVIATION`
comment saying so. The operator is a static Go binary, so it runs on the
`gcr.io/distroless/static:nonroot` base the CobaltCore operator images use
(`operators/Dockerfile`), as that base's `nonroot` user, UID 65532. Upstream's
own image is Alpine with UID 4200. Its chart sets only `runAsNonRoot: true` and
no command, which any non-root user and the image's `ENTRYPOINT` satisfy.

**Distroless and UID 0 instead of `python-base` (kvm-node-agent):**

`images/kvm-node-agent/Dockerfile` does not derive from `python-base` and
creates no `openstack` user, and carries a `# DEVIATION` comment saying so.
The agent is a static Go binary on the same `gcr.io/distroless/static:nonroot`
base, but it runs as root (`USER 0:0`) instead of that base's `nonroot` user.
kna authenticates to the host's system bus and starts units there, and a
Debian host's dbus-daemon drops the connection of a UID it cannot resolve
(#1167). Upstream's own image is Alpine with `USER 42438:42438`, a user that
exists on SAP's hosts only. Every consumer of the image therefore runs the
agent as root unless its pod sets `runAsUser`: the upstream chart sets none
for the manager, and `runAsNonRoot: true` rejects the image. hadolint reports
`DL3002` for a root `USER`, so the line above it carries
`# hadolint ignore=DL3002`.
