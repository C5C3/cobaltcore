# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ---------- Stage 1: build ----------
# hadolint ignore=DL3006
FROM venv-builder AS build

# PyPI version pins resolved from releases/2025.2/test-refs.yaml by CI.
ARG TEMPEST_VERSION="45.0.0"
ARG KEYSTONE_TEMPEST_PLUGIN_VERSION="0.19.0"

# Install Tempest and test dependencies into the shared virtualenv.
# Unlike service images, Tempest is installed from PyPI (no git source mount).
# Named build context supplies the constraints file:
#   --build-context upper-constraints=<path-to-release-dir>
RUN --mount=type=bind,from=upper-constraints,source=upper-constraints.txt,target=/tmp/upper-constraints.txt \
    uv pip install --prefix /var/lib/openstack \
        --constraint /tmp/upper-constraints.txt \
        tempest=="${TEMPEST_VERSION}" \
        keystone-tempest-plugin=="${KEYSTONE_TEMPEST_PLUGIN_VERSION}" \
        python-subunit \
        junitxml

# ---------- Stage 2: runtime ----------
# hadolint ignore=DL3006
FROM python-base

COPY --from=build --link /var/lib/openstack /var/lib/openstack

# CC-0035: Static OCI Image Spec annotations for local builds (runtime stage only).
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="tempest" \
      org.opencontainers.image.description="OpenStack Tempest testing framework" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"

USER openstack
