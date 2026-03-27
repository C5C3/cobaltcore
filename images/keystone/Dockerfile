# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ---------- Stage 1: build ----------
FROM venv-builder AS build

# Comma-separated Python extras (e.g. "ldap,oauth1").
# Passed by CI from releases/<release>/extra-packages.yaml.
ARG PIP_EXTRAS=""
# Space-separated additional pip packages to install alongside the service.
ARG PIP_PACKAGES=""

# Install Keystone and extras into the shared virtualenv.
# Named build contexts supply the source tree and constraints file:
#   --build-context keystone=<path-to-keystone-source>
#   --build-context upper-constraints=<path-to-release-dir>
RUN --mount=type=bind,from=upper-constraints,source=upper-constraints.txt,target=/tmp/upper-constraints.txt \
    --mount=type=bind,from=keystone,target=/tmp/keystone,readwrite \
    PKG="/tmp/keystone" && \
    if [ -n "$PIP_EXTRAS" ]; then PKG="${PKG}[${PIP_EXTRAS}]"; fi && \
    uv pip install --prefix /var/lib/openstack \
        --constraint /tmp/upper-constraints.txt \
        "$PKG" $PIP_PACKAGES

# Generate the WSGI entry-point script that uWSGI loads at runtime.
# PBR registers this as a wsgi_scripts entry point, but uv's --prefix
# install mode does not generate entry-point scripts, so we create it
# manually. The callable matches keystone's setup.cfg wsgi_scripts entry.
RUN printf '#!/var/lib/openstack/bin/python\n\
from keystone.server.wsgi import initialize_public_application\n\
application = initialize_public_application()\n' \
    > /var/lib/openstack/bin/keystone-wsgi-public && \
    chmod +x /var/lib/openstack/bin/keystone-wsgi-public

# ---------- Stage 2: runtime ----------
FROM python-base

# DEVIATION from architecture/docs/08-container-images/01-build-pipeline.md:
# Uses the generic 'openstack' user (UID/GID 42424) from python-base instead
# of a per-service user. See python-base/Dockerfile for rationale.

# Space-separated runtime system packages (e.g. "libapache2-mod-wsgi-py3 libldap2 ...").
# Passed by CI from releases/<release>/extra-packages.yaml.
ARG EXTRA_APT_PACKAGES=""

COPY --from=build --link /var/lib/openstack /var/lib/openstack

# Runtime system packages required by Keystone.
# Guard: when EXTRA_APT_PACKAGES is empty (e.g. local build without
# --build-arg EXTRA_APT_PACKAGES=...), skip the install to avoid
# "E: No packages specified" from apt-get. CI always provides packages
# via the workflow's "Resolve extra packages" step.
RUN if [ -n "${EXTRA_APT_PACKAGES}" ]; then \
    DEBIAN_FRONTEND=noninteractive apt-get update && \
    apt-get install -y --no-install-recommends ${EXTRA_APT_PACKAGES} && \
    rm -rf /var/lib/apt/lists/*; \
    fi

# CC-0031: Static OCI Image Spec annotations for local builds (runtime stage only).
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="keystone" \
      org.opencontainers.image.description="OpenStack keystone service" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"

USER openstack
