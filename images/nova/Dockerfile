# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ---------- Stage 0: noVNC assets ----------
# hadolint ignore=DL3006
FROM python-base AS novnc

# Git tag of novnc/noVNC and the version pin of the console assets, bumped by
# the Renovate customManager for images/nova/Dockerfile in renovate.json. The
# commit below is the content pin: the fetch asks for it, never for the tag,
# and the same customManager rewrites both lines in one PR (the github-tags
# datasource resolves the tag to its commit). Keep the two lines adjacent: the
# matchStrings entry spans both.
ARG NOVNC_VERSION=v1.7.0
ARG NOVNC_COMMIT=63107bd06d9e1f6136ff21aeda8cd62cbf0d433e

# git comes from the signed noble archive, not from venv-builder: venv-builder
# builds uwsgi from its sdist as root after installing git, and the uv pip
# install of the build stage builds nova from source as root, both running
# build code fetched from PyPI. The token below must not reach a git binary or
# a git configuration that code could have replaced;
# tests/unit/images/github_token_stage_base_test.sh fails on a token stage
# that takes files from venv-builder, through its FROM, a COPY --from or a
# bind mount. A stage of its own also keeps the fetch off the install's cache
# chain, so only a pin bump fetches again.
# hadolint ignore=DL3008
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    DEBIAN_FRONTEND=noninteractive apt-get update && \
    apt-get install -y --no-install-recommends git

# GitHub does not reliably serve anonymous upload-pack requests to the
# git noble ships, so a mounted github_token secret (hack/ci-build-service-
# image.sh and the Build service image step pass the workflow token) is sent
# the way actions/checkout does, as a basic-auth header scoped to github.com
# through git's environment config: it lives in this one RUN and in no layer.
# A build without the secret stays anonymous. The fetch is tried three times
# with a linear backoff, like the clone in hack/ci-build-service-image.sh: a
# failed nova leg on a main push skips merge-service-images for every service.
# The grep is the guard against a Renovate bump that moved NOVNC_VERSION
# without NOVNC_COMMIT: the build fails naming the stale pin instead of
# shipping old assets under a new version. It is grouped, so a failed init,
# fetch or checkout is not reported as a stale pin.
# The copied set is what the two pages load: vnc_lite.html imports
# core/rfb.js; vnc.html loads app/ui.js, which reads defaults.json,
# mandatory.json and package.json (the version the UI shows) and the styles,
# images, locales and sounds under app/; vendor/ carries pako, the inflate
# library core/ imports; LICENSE.txt is the licence notice of the assets.
# The tests, docs, po/, snap/, utils/ and the lint configuration of the
# upstream tree are not copied.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
RUN --mount=type=secret,id=github_token \
    export GIT_TERMINAL_PROMPT=0 && \
    if [ -s /run/secrets/github_token ]; then \
        auth="$(printf 'x-access-token:%s' "$(cat /run/secrets/github_token)" | base64 -w0)" && \
        export GIT_CONFIG_COUNT=1 \
            GIT_CONFIG_KEY_0="http.https://github.com/.extraheader" \
            GIT_CONFIG_VALUE_0="AUTHORIZATION: basic ${auth}"; \
    fi && \
    git init /src/novnc && \
    git -C /src/novnc remote add origin https://github.com/novnc/noVNC.git && \
    for attempt in 1 2 3; do \
        git -C /src/novnc fetch --depth 1 origin "${NOVNC_COMMIT}" && break; \
        [ "$attempt" -lt 3 ] || { echo "noVNC fetch of ${NOVNC_COMMIT} failed after 3 attempts" >&2; exit 1; }; \
        sleep $((attempt * 5)); \
    done && \
    git -C /src/novnc checkout --detach FETCH_HEAD && \
    { grep -q "\"version\": \"${NOVNC_VERSION#v}\"" /src/novnc/package.json || \
        { echo "noVNC package.json version does not match NOVNC_VERSION=${NOVNC_VERSION}: NOVNC_COMMIT is stale" >&2; exit 1; }; } && \
    mkdir -p /opt/novnc && \
    cp -r /src/novnc/app /src/novnc/core /src/novnc/vendor \
        /src/novnc/vnc.html /src/novnc/vnc_lite.html \
        /src/novnc/defaults.json /src/novnc/mandatory.json \
        /src/novnc/package.json /src/novnc/LICENSE.txt /opt/novnc/

# ---------- Stage 1: build ----------
# hadolint ignore=DL3006
FROM venv-builder AS build

# Comma-separated Python extras (unused by nova today; kept for parity
# with the extra-packages.yaml wiring shared by all service images).
# Passed by CI from releases/<release>/extra-packages.yaml.
ARG PIP_EXTRAS=""
# Space-separated additional pip packages to install alongside the service.
# Empty for nova today; kept for parity with the other service images.
# Passed by CI from releases/<release>/extra-packages.yaml.
ARG PIP_PACKAGES=""

# Install Nova and extras into the shared virtualenv.
# Named build contexts supply the source tree and constraints file:
#   --build-context nova=<path-to-nova-source>
#   --build-context upper-constraints=<path-to-release-dir>
#
# 'set -f' disables pathname expansion for the install step: PIP_PACKAGES stays
# unquoted so multiple space-separated packages word-split, but a PEP 508
# extras suffix (e.g. some_lib[s3]) is also a glob bracket expression that the
# shell would rewrite if a matching file existed in the build CWD.
# Nova has no os-win and imports no pkg_resources, so no setuptools<81
# constraint is needed. The 32.0.0 image resolves setuptools 84 and its
# oslo-config-generator run over etc/nova/nova-config-generator.conf exits 0,
# so no derived catalog image is needed either.
RUN --mount=type=bind,from=upper-constraints,source=upper-constraints.txt,target=/tmp/upper-constraints.txt \
    --mount=type=bind,from=nova,target=/tmp/nova,readwrite \
    PKG="/tmp/nova" && \
    if [ -n "$PIP_EXTRAS" ]; then PKG="${PKG}[${PIP_EXTRAS}]"; fi && \
    set -f && \
    uv pip install --prefix /var/lib/openstack \
        --constraint /tmp/upper-constraints.txt \
        "$PKG" $PIP_PACKAGES

# uv's --prefix install mode generates the eleven console scripts declared in
# [console_scripts] of setup.cfg (32.0.0) and [project.scripts] of
# pyproject.toml (33.0.0): nova-compute, nova-conductor, nova-manage,
# nova-novncproxy, nova-policy, nova-rootwrap, nova-rootwrap-daemon,
# nova-scheduler, nova-serialproxy, nova-spicehtml5proxy and nova-status.
# 32.0.0 also declares two PBR wsgi_scripts (nova-api-wsgi,
# nova-metadata-wsgi), which uv generates as well; 33.0.0 dropped them.
# Nothing uses them: the nova-operator (#1017) launches uWSGI on the module
# paths `nova.wsgi.osapi_compute:application` and
# `nova.wsgi.metadata:application` with OS_NOVA_CONFIG_DIR and
# OS_NOVA_CONFIG_FILES (#1014 decision D1), so no entry script ships, neither
# generated nor hand-written. The image stays config-free. The package data
# files api-paste.ini, rootwrap.conf and rootwrap.d/compute.filters land under
# /var/lib/openstack/etc/nova/ at both tags (data_files in setup.cfg at
# 32.0.0, [tool.setuptools.data-files] in pyproject.toml at 33.0.0); nothing
# copies etc/nova/ by hand, and tests/container-images/verify_nova.sh asserts
# their presence.

# ---------- Stage 2: runtime ----------
# hadolint ignore=DL3006
FROM python-base

# DEVIATION from the original build-pipeline design:
# Uses the generic 'openstack' user (UID/GID 42424) from python-base instead
# of a per-service user. See python-base/Dockerfile for rationale.

# Space-separated runtime system packages.
# Passed by CI from releases/<release>/extra-packages.yaml.
ARG EXTRA_APT_PACKAGES=""

COPY --from=build --link /var/lib/openstack /var/lib/openstack

# The noVNC web assets the nova-novncproxy serves. The nova-operator's
# `[vnc] novncproxy_base_url` names vnc_lite.html (#1014 D8), and the tree
# ships no vnc_auto.html.
COPY --from=novnc --link /opt/novnc /usr/share/novnc

# The state directories Nova writes to. /var/lib/nova is [DEFAULT] state_path,
# where a compute stores its node identity, compute_id. /var/lib/nova/tmp is
# the [oslo_concurrency] lock_path, and /var/lib/nova/instances is
# instances_path, the default $state_path/instances, which a compute expects
# to exist. All three ship empty and owned by 42424. The nova-operator (#1017)
# mounts an emptyDir over the tree, so this layout is what a plain
# `docker run` gets.
RUN mkdir -p /var/lib/nova/instances \
        /var/lib/nova/tmp && \
    chown -R 42424:42424 /var/lib/nova

# The exec readiness probe of the nova-scheduler and nova-conductor processes
# (#1014 D1): it reports ready when a process of this container holds a socket
# established to the broker port, the semantics of kolla's healthcheck_port.
# The socket is matched by inode, because /proc/net/tcp* is scoped to the
# network namespace every container of the pod shares. The nova-operator
# (#1017) wires it as
# `readinessProbe.exec.command: ["/var/lib/openstack/bin/nova-amqp-ready"]`;
# the two paths must stay in lockstep.
COPY --chmod=0755 nova-amqp-ready /var/lib/openstack/bin/nova-amqp-ready

# Runtime system packages required by Nova. CI passes them from
# releases/<release>/extra-packages.yaml, which carries one entry.
# libpython3.12t64: the venv-builder-compiled uwsgi binary links
# libpython3.12.so.1.0, which python-base does not ship.
# No qemu-utils: the control-plane roles convert nothing, and no libvirt0: the
# fake driver imports without it (#1014 D14). sudo comes from python-base; the
# image carries no sudoers entry.
# The guard keeps local builds without --build-arg EXTRA_APT_PACKAGES a clean
# no-op. /var/lib/apt/lists is a cache mount for the duration of the RUN, so
# its contents never reach the layer and the sibling images' trailing
# `rm -rf /var/lib/apt/lists/*` only empties the cache the next build wants.
# hadolint ignore=DL3008
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    if [ -n "${EXTRA_APT_PACKAGES}" ]; then \
    DEBIAN_FRONTEND=noninteractive apt-get update && \
    apt-get install -y --no-install-recommends ${EXTRA_APT_PACKAGES}; \
    fi

# Static OCI Image Spec annotations for local builds (runtime stage only).
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="nova" \
      org.opencontainers.image.description="OpenStack nova service" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"

USER openstack
