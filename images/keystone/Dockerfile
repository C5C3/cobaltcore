# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ---------- Stage 1: build ----------
FROM venv-builder AS build

# Install Keystone and extras into the shared virtualenv.
# Named build contexts supply the source tree and constraints file:
#   --build-context keystone=<path-to-keystone-source>
#   --build-context upper-constraints=<path-to-release-dir>
RUN --mount=type=bind,from=upper-constraints,source=upper-constraints.txt,target=/tmp/upper-constraints.txt \
    --mount=type=bind,from=keystone,target=/tmp/keystone \
    cp -a /tmp/keystone /tmp/keystone-build && \
    uv pip install --prefix /var/lib/openstack \
        --constraint /tmp/upper-constraints.txt \
        "/tmp/keystone-build[ldap,memcache_pool,oauth1]" && \
    rm -rf /tmp/keystone-build

# ---------- Stage 2: runtime ----------
FROM python-base

# DEVIATION from architecture/docs/08-container-images/01-build-pipeline.md:
# Uses the generic 'openstack' user (UID/GID 42424) from python-base instead
# of a per-service user. See python-base/Dockerfile for rationale.

COPY --from=build --link /var/lib/openstack /var/lib/openstack

# Runtime system packages required by Keystone
RUN DEBIAN_FRONTEND=noninteractive apt-get update && apt-get install -y --no-install-recommends \
    libapache2-mod-wsgi-py3 \
    libldap2 \
    libsasl2-2 \
    libxml2 \
    && rm -rf /var/lib/apt/lists/*

USER openstack
