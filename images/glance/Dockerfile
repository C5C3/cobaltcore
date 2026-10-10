# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ---------- Stage 1: build ----------
# hadolint ignore=DL3006
FROM venv-builder AS build

# Comma-separated Python extras (unused by glance today; kept for parity
# with the extra-packages.yaml wiring shared by all service images).
# Passed by CI from releases/<release>/extra-packages.yaml.
ARG PIP_EXTRAS=""
# Space-separated additional pip packages to install alongside the service.
# Carries glance_store[s3] — the S3 store driver's extra lives on glance_store,
# not glance, and pulls boto3/botocore/s3transfer (all pinned in
# upper-constraints.txt).
ARG PIP_PACKAGES=""

# Install Glance and extras into the shared virtualenv.
# Named build contexts supply the source tree and constraints file:
#   --build-context glance=<path-to-glance-source>
#   --build-context upper-constraints=<path-to-release-dir>
#
# Both launch modes ship in this one image by construction: uv's --prefix
# install generates the glance-api and glance-manage console scripts from
# setup.cfg (it only skips PBR wsgi_scripts). Every wired release (2026.1
# and later) runs uWSGI with the hand-shipped glance-wsgi-api script
# (keystone's technique, see images/keystone/Dockerfile), and none starts the
# eventlet glance-api console script. The stock module path
# glance.wsgi.api:application is unusable under the operator's layout:
# wsgi_app.init_app() ignores sys.argv (and so uWSGI's --pyargv) and reads only
# $OS_GLANCE_CONFIG_DIR/glance-api.conf, never the two --config-dir mounts —
# see glance-wsgi-api. uwsgi itself comes from the venv-builder base
# requirements.
#
# 'set -f' disables pathname expansion for the install step: PIP_PACKAGES stays
# unquoted so multiple space-separated packages word-split, but its
# glance_store[s3] value is also a glob bracket expression that the shell would
# rewrite if a matching file existed in the build CWD.
RUN --mount=type=bind,from=upper-constraints,source=upper-constraints.txt,target=/tmp/upper-constraints.txt \
    --mount=type=bind,from=glance,target=/tmp/glance,readwrite \
    PKG="/tmp/glance" && \
    if [ -n "$PIP_EXTRAS" ]; then PKG="${PKG}[${PIP_EXTRAS}]"; fi && \
    set -f && \
    uv pip install --prefix /var/lib/openstack \
        --constraint /tmp/upper-constraints.txt \
        "$PKG" $PIP_PACKAGES

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

# The uWSGI entry script for the 2026.1+ launch mode: it redirects glance's
# WSGI config discovery to the --config-dir roots passed via uWSGI --pyargv
# (glance's stock module path ignores sys.argv — see the script header). The
# glance-operator references it as `--wsgi-file /var/lib/openstack/bin/
# glance-wsgi-api` in reconcile_deployment.go; the two paths must stay in
# lockstep.
COPY --chmod=0755 glance-wsgi-api /var/lib/openstack/bin/glance-wsgi-api

# Runtime system packages required by Glance. The venv-builder-compiled uwsgi
# binary links libpython3.12.so.1.0, which python-base does not ship — CI
# passes libpython3.12t64 from releases/<release>/extra-packages.yaml (the
# same rationale as horizon). python3-rados and python3-rbd carry the rados
# and rbd modules the RBD store (glance_store/_drivers/rbd.py) imports behind
# a guard. The store runs no ceph or rbd binary, so ceph-common stays out. The
# step after this one makes the venv see the two modules. The image itself
# stays config-free: the operator mounts glance-api.conf, glance-api-paste.ini,
# and policy, and provides the staging/tasks paths as emptyDir mounts. The
# guard keeps local builds without --build-arg EXTRA_APT_PACKAGES a clean
# no-op.
# hadolint ignore=DL3008
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    if [ -n "${EXTRA_APT_PACKAGES}" ]; then \
    DEBIAN_FRONTEND=noninteractive apt-get update && \
    apt-get install -y --no-install-recommends ${EXTRA_APT_PACKAGES} && \
    rm -rf /var/lib/apt/lists/*; \
    fi

# The venv of images/venv-builder/Dockerfile excludes the system site
# packages, and the Ceph bindings ship only as noble packages, which install
# into /usr/lib/python3/dist-packages. That directory also receives every
# other python3-* package an apt entry pulls in, outside upper-constraints, so
# the venv must not search it. The step links the two extension modules into
# /var/lib/openstack/ceph-bindings, and a .pth file in the venv's
# site-packages names that directory. Python's site module appends it to
# sys.path: rados and rbd resolve, and no other module from dist-packages
# does. sysconfig answers the site-packages path, so no Python version is
# hard-coded. The step sits outside the guard: in a build without --build-arg
# EXTRA_APT_PACKAGES the globs match nothing and the entry names an empty
# directory. With python3-rbd installed, the step imports rados and rbd
# through the venv (rbd imports rados), so a module file the globs miss or a
# failed link fails the build. On a push to main, verify_glance.sh runs only
# after the tag is published.
RUN site_packages="$(/var/lib/openstack/bin/python -c 'import sysconfig; print(sysconfig.get_path("purelib"))')" && \
    mkdir -p /var/lib/openstack/ceph-bindings && \
    for so in /usr/lib/python3/dist-packages/rados.cpython-*.so \
        /usr/lib/python3/dist-packages/rbd.cpython-*.so; do \
    if [ -e "$so" ]; then ln -s "$so" /var/lib/openstack/ceph-bindings/ || exit 1; fi; \
    done && \
    echo /var/lib/openstack/ceph-bindings > "${site_packages}/ceph-bindings.pth" && \
    if [ "$(dpkg-query -W -f='${db:Status-Status}' python3-rbd 2>/dev/null)" = installed ]; then \
    /var/lib/openstack/bin/python -c 'import rados, rbd'; \
    fi

# Static OCI Image Spec annotations for local builds (runtime stage only).
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="glance" \
      org.opencontainers.image.description="OpenStack glance service" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"

USER openstack
