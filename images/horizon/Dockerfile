# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ---------- Stage 1: build ----------
# hadolint ignore=DL3006
FROM venv-builder AS build

# Comma-separated Python extras (unused by horizon today; kept for parity
# with the extra-packages.yaml wiring shared by all service images).
# Passed by CI from releases/<release>/extra-packages.yaml.
ARG PIP_EXTRAS=""
# Space-separated additional pip packages to install alongside the service.
ARG PIP_PACKAGES=""

# Install Horizon and extras into the shared virtualenv.
# Named build contexts supply the source tree and constraints file:
#   --build-context horizon=<path-to-horizon-source>
#   --build-context upper-constraints=<path-to-release-dir>
# NOTE: upper-constraints.txt pins horizon=== itself; the pin is stripped
# beforehand by scripts/apply-constraint-overrides.sh reading
# overrides/<release>/constraints.txt (-horizon), otherwise pip refuses to
# install the source tree against its own pin.
# UV_BUILD_CONSTRAINT: several XStatic pins in the 2025.2 upper-constraints
# ship only sdists whose legacy setup.py imports pkg_resources; setuptools 81
# removed it from uv's isolated build env, so sdist builds need the last
# release that still bundles it. Runtime resolution is unaffected.
RUN --mount=type=bind,from=upper-constraints,source=upper-constraints.txt,target=/tmp/upper-constraints.txt \
    --mount=type=bind,from=horizon,target=/tmp/horizon,readwrite \
    PKG="/tmp/horizon" && \
    if [ -n "$PIP_EXTRAS" ]; then PKG="${PKG}[${PIP_EXTRAS}]"; fi && \
    printf 'setuptools<81\n' > /tmp/build-constraints.txt && \
    UV_BUILD_CONSTRAINT=/tmp/build-constraints.txt \
    uv pip install --prefix /var/lib/openstack \
        --constraint /tmp/upper-constraints.txt \
        "$PKG" $PIP_PACKAGES

# Build static assets at image-build time (collectstatic + offline
# compression) so the runtime image serves pre-built assets and needs no
# init container. A throwaway local_settings.py provides the minimal
# settings the Django management commands need; it is removed afterwards so
# the runtime image only ever reads the operator-mounted
# /etc/openstack-dashboard/local_settings.py (see the Stage 2 symlink).
#
# CACHES must be a WORKING in-process backend: horizon wraps the Angular
# template-cache preloads in {% cache %} (horizon/_scripts.html), and with
# the default PyMemcacheCache pointing at an unreachable 127.0.0.1:11211
# the cache lookup raises, django-compressor silently skips that block, and
# the shipped manifest.json lacks its key — every page render then 500s at
# runtime with OfflineGenerationError once a real Memcached is reachable.
RUN SITE_PKG="$(/var/lib/openstack/bin/python -c 'import os, openstack_dashboard; print(os.path.dirname(openstack_dashboard.__file__))')" && \
    printf 'DEBUG = False\nSECRET_KEY = "build-time-throwaway"\nSTATIC_ROOT = "/var/lib/openstack/horizon-static"\nCOMPRESS_OFFLINE = True\nALLOWED_HOSTS = ["*"]\nCACHES = {"default": {"BACKEND": "django.core.cache.backends.locmem.LocMemCache"}}\n' \
        > "${SITE_PKG}/local/local_settings.py" && \
    DJANGO_SETTINGS_MODULE=openstack_dashboard.settings \
        /var/lib/openstack/bin/python -m django collectstatic --noinput && \
    DJANGO_SETTINGS_MODULE=openstack_dashboard.settings \
        /var/lib/openstack/bin/python -m django compress --force && \
    rm -f "${SITE_PKG}/local/local_settings.py" "${SITE_PKG}/local/.secret_key_store" && \
    find "${SITE_PKG}/local" -type d -name '__pycache__' -exec rm -rf {} +

# ---------- Stage 2: runtime ----------
# hadolint ignore=DL3006
FROM python-base

# DEVIATION from architecture/docs/08-container-images/01-build-pipeline.md:
# Uses the generic 'openstack' user (UID/GID 42424) from python-base instead
# of a per-service user. See python-base/Dockerfile for rationale.

# Space-separated runtime system packages.
# Passed by CI from releases/<release>/extra-packages.yaml.
ARG EXTRA_APT_PACKAGES=""

COPY --from=build --link /var/lib/openstack /var/lib/openstack

# Runtime system packages required by Horizon. The dashboard itself is a
# pure-Python Django app, but the venv-builder-compiled uwsgi binary links
# libpython3.12.so.1.0, which python-base does not ship — CI passes
# libpython3.12t64 from releases/<release>/extra-packages.yaml (keystone
# gets it transitively via libapache2-mod-wsgi-py3). The guard keeps local
# builds without --build-arg EXTRA_APT_PACKAGES a clean no-op.
# hadolint ignore=DL3008
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    if [ -n "${EXTRA_APT_PACKAGES}" ]; then \
    DEBIAN_FRONTEND=noninteractive apt-get update && \
    apt-get install -y --no-install-recommends ${EXTRA_APT_PACKAGES} && \
    rm -rf /var/lib/apt/lists/*; \
    fi

# The horizon-operator mounts the rendered Django settings ConfigMap at
# /etc/openstack-dashboard/. The symlink makes the installed
# openstack_dashboard package import it as openstack_dashboard.local
# .local_settings (the upstream settings.py extension point). The link
# dangles at build time by design — it resolves once the ConfigMap is
# mounted at runtime. uWSGI loads the dashboard via
# `--module openstack_dashboard.wsgi` (the module ships `application`),
# so no hand-written wsgi script is needed.
RUN mkdir -p /etc/openstack-dashboard && \
    SITE_PKG="$(/var/lib/openstack/bin/python -c 'import os, openstack_dashboard; print(os.path.dirname(openstack_dashboard.__file__))')" && \
    ln -sf /etc/openstack-dashboard/local_settings.py "${SITE_PKG}/local/local_settings.py"

# Static OCI Image Spec annotations for local builds (runtime stage only).
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="horizon" \
      org.opencontainers.image.description="OpenStack horizon dashboard" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"

USER openstack
