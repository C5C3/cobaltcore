# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ---------- Stage 1: build ----------
# hadolint ignore=DL3006
FROM venv-builder AS build

# Comma-separated Python extras (unused by barbican today; kept for parity
# with the extra-packages.yaml wiring shared by all service images).
# Passed by CI from releases/<release>/extra-packages.yaml.
ARG PIP_EXTRAS=""
# Space-separated additional pip packages to install alongside the service.
# Empty for barbican today; kept for parity with the other service images.
# Passed by CI from releases/<release>/extra-packages.yaml.
ARG PIP_PACKAGES=""

# Install Barbican and extras into the shared virtualenv.
# Named build contexts supply the source tree and constraints file:
#   --build-context barbican=<path-to-barbican-source>
#   --build-context upper-constraints=<path-to-release-dir>
#
# 'set -f' disables pathname expansion for the install step: PIP_PACKAGES stays
# unquoted so multiple space-separated packages word-split, but a PEP 508
# extras suffix (e.g. some_lib[s3]) is also a glob bracket expression that the
# shell would rewrite if a matching file existed in the build CWD.
RUN --mount=type=bind,from=upper-constraints,source=upper-constraints.txt,target=/tmp/upper-constraints.txt \
    --mount=type=bind,from=barbican,target=/tmp/barbican,readwrite \
    PKG="/tmp/barbican" && \
    if [ -n "$PIP_EXTRAS" ]; then PKG="${PKG}[${PIP_EXTRAS}]"; fi && \
    set -f && \
    uv pip install --prefix /var/lib/openstack \
        --constraint /tmp/upper-constraints.txt \
        "$PKG" $PIP_PACKAGES

# No WSGI entry script ships in this image, neither generated nor hand-written.
# uv's --prefix install mode generates the setup.cfg console scripts and skips
# only the PBR wsgi_scripts entry barbican-wsgi-api. Nothing replaces that
# entry: both releases ship barbican/wsgi/api.py with a module-level
# `application`, so the barbican-operator launches uWSGI with the module path
# `barbican.wsgi.api:application` against config mounted at /etc/barbican/.
# The module path recorded here and the operator's uWSGI flag have to stay in
# lockstep from that point on.
# The image itself stays config-free.

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

# Runtime system packages required by Barbican. Barbican is otherwise pure
# Python at runtime, but the venv-builder-compiled uwsgi binary links
# libpython3.12.so.1.0, which python-base does not ship — CI passes
# libpython3.12t64 from releases/<release>/extra-packages.yaml (the same
# rationale as glance, horizon and placement). The guard keeps local builds
# without --build-arg EXTRA_APT_PACKAGES a clean no-op.
# hadolint ignore=DL3008
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    if [ -n "${EXTRA_APT_PACKAGES}" ]; then \
    DEBIAN_FRONTEND=noninteractive apt-get update && \
    apt-get install -y --no-install-recommends ${EXTRA_APT_PACKAGES} && \
    rm -rf /var/lib/apt/lists/*; \
    fi

# Static OCI Image Spec annotations for local builds (runtime stage only).
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="barbican" \
      org.opencontainers.image.description="OpenStack barbican service" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"

USER openstack
