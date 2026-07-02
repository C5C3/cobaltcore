# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Tempest API testing image
# 2-stage build: venv-builder → python-base runtime.
# Unlike service images, Tempest is installed from PyPI (no git source mount).

# ---------- Stage 1: build ----------
# hadolint ignore=DL3006
FROM venv-builder AS build

# PyPI versions passed by CI from releases/<release>/test-refs.yaml.
ARG TEMPEST_VERSION=45.0.0
ARG KEYSTONE_TEMPEST_PLUGIN_VERSION=0.19.0

# Install Tempest and plugins into the shared virtualenv.
# upper-constraints.txt is passed as a named build context from the release dir.
# python-openstackclient provides the `openstack` CLI used by the
# full-controlplane-keystone E2E verify Job (token issue / catalog list);
# its version is pinned by upper-constraints like the other unversioned deps.
RUN --mount=type=bind,from=upper-constraints,source=upper-constraints.txt,target=/tmp/upper-constraints.txt \
    uv pip install --prefix /var/lib/openstack \
        --constraint /tmp/upper-constraints.txt \
        tempest==${TEMPEST_VERSION} \
        keystone-tempest-plugin==${KEYSTONE_TEMPEST_PLUGIN_VERSION} \
        python-openstackclient \
        python-subunit \
        junitxml \
        defusedxml

# ---------- Stage 2: runtime ----------
# hadolint ignore=DL3006
FROM python-base

COPY --from=build --link /var/lib/openstack /var/lib/openstack

# Static OCI Image Spec annotations for local builds (runtime stage only).
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="tempest" \
      org.opencontainers.image.description="OpenStack Tempest testing framework" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"

RUN mkdir -p /etc/tempest && \
    chown openstack:openstack /var/lib/openstack

USER openstack
