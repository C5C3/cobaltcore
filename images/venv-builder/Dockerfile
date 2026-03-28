# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

FROM python-base

# Install uv (fast Python package manager, pinned via Renovate)
COPY --from=ghcr.io/astral-sh/uv:0.11.2@sha256:c4f5de312ee66d46810635ffc5df34a1973ba753e7241ce3a08ef979ddd7bea5 /uv /bin/

# Build dependencies
RUN DEBIAN_FRONTEND=noninteractive apt-get update && apt-get install -y --no-install-recommends \
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

# Common base packages for all services (no --constraint: venv-builder is
# release-independent; constraints are applied in service Dockerfiles)
RUN uv pip install --prefix /var/lib/openstack \
    cryptography \
    pymemcache \
    pymysql \
    python-memcached \
    uwsgi

# CC-0031: Static OCI Image Spec annotations for local builds.
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="venv-builder" \
      org.opencontainers.image.description="Python virtualenv builder image for OpenStack services" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"
