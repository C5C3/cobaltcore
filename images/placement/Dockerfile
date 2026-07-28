# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ---------- Stage 1: build ----------
# hadolint ignore=DL3006
FROM venv-builder AS build

# Comma-separated Python extras (unused by placement today; kept for parity
# with the extra-packages.yaml wiring shared by all service images).
# Passed by CI from releases/<release>/extra-packages.yaml.
ARG PIP_EXTRAS=""
# Space-separated additional pip packages to install alongside the service.
# Empty for placement today; kept for parity with the other service images.
# Passed by CI from releases/<release>/extra-packages.yaml.
ARG PIP_PACKAGES=""

# Install Placement and extras into the shared virtualenv.
# Named build contexts supply the source tree and constraints file:
#   --build-context placement=<path-to-placement-source>
#   --build-context upper-constraints=<path-to-release-dir>
#
# 'set -f' disables pathname expansion for the install step: PIP_PACKAGES stays
# unquoted so multiple space-separated packages word-split, but a PEP 508
# extras suffix (e.g. some_lib[s3]) is also a glob bracket expression that the
# shell would rewrite if a matching file existed in the build CWD.
RUN --mount=type=bind,from=upper-constraints,source=upper-constraints.txt,target=/tmp/upper-constraints.txt \
    --mount=type=bind,from=placement,target=/tmp/placement,readwrite \
    PKG="/tmp/placement" && \
    if [ -n "$PIP_EXTRAS" ]; then PKG="${PKG}[${PIP_EXTRAS}]"; fi && \
    set -f && \
    uv pip install --prefix /var/lib/openstack \
        --constraint /tmp/upper-constraints.txt \
        "$PKG" $PIP_PACKAGES

# Generate the WSGI entry-point script that uWSGI loads at runtime.
# 2025.2 declares placement-api as a PBR wsgi_scripts entry in setup.cfg, which
# uv's --prefix install mode does not generate; 2026.1 moved packaging to
# pyproject.toml and declares no WSGI script at all. The entry therefore ships
# by hand for both releases under the upstream name, with no release
# conditional. The placement-operator, once it lands, must launch uWSGI with
# `--wsgi-file /var/lib/openstack/bin/placement-api`; the two paths have to
# stay in lockstep from that point on.
# The image itself stays config-free: placement locates its configuration via
# the OS_PLACEMENT_CONFIG_DIR environment variable set by the operator.
RUN printf '#!/var/lib/openstack/bin/python\n\
from placement.wsgi import init_application\n\
application = init_application()\n' \
    > /var/lib/openstack/bin/placement-api && \
    chmod +x /var/lib/openstack/bin/placement-api

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

# Runtime system packages required by Placement. Placement is otherwise pure
# Python at runtime, but the venv-builder-compiled uwsgi binary links
# libpython3.12.so.1.0, which python-base does not ship — CI passes
# libpython3.12t64 from releases/<release>/extra-packages.yaml (the same
# rationale as glance and horizon). The guard keeps local builds without
# --build-arg EXTRA_APT_PACKAGES a clean no-op.
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
LABEL org.opencontainers.image.title="placement" \
      org.opencontainers.image.description="OpenStack placement service" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"

USER openstack
