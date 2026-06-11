# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

FROM python-base

# Install uv (fast Python package manager, pinned via Renovate)
COPY --from=ghcr.io/astral-sh/uv:0.11.19@sha256:b46b03ddfcfbf8f547af7e9eaefdf8a39c8cebcba7c98858d3162bd28cf536f6 /uv /bin/

# Build dependencies
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    DEBIAN_FRONTEND=noninteractive apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    git \
    libffi-dev \
    libldap2-dev \
    libpq-dev \
    libsasl2-dev \
    libssl-dev \
    python3-dev \
    python3-venv \
    && rm -rf /var/lib/apt/lists/*

# Prepare virtual environment
RUN python3 -m venv /var/lib/openstack

# Common base packages for all services, version-pinned via requirements.txt
# for a reproducible venv. venv-builder stays release-independent: the
# OpenStack-dependency subset is re-pinned per release in service Dockerfiles
# via --constraint upper-constraints.txt, while uwsgi (not an OpenStack
# dependency) is fixed here. Renovate tracks the pins (see renovate.json).
RUN --mount=type=bind,source=requirements.txt,target=/tmp/requirements.txt \
    uv pip install --prefix /var/lib/openstack -r /tmp/requirements.txt

# CC-0031: Static OCI Image Spec annotations for local builds.
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="venv-builder" \
      org.opencontainers.image.description="Python virtualenv builder image for OpenStack services" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"
