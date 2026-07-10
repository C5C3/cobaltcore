# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Apache federation reverse-proxy sidecar for Keystone: mod_auth_openidc
# (OIDC) and mod_auth_mellon (SAML) in one image. Projected into the Keystone
# API pod only when at least one federation-typed (OIDC or SAML)
# KeystoneIdentityBackend is attached; it terminates the federation flows on
# the protected websso/auth/redirect paths and reverse-proxies everything to
# the localhost-bound uWSGI. Release-independent: both modules come from the
# distro (ubuntu:noble) package archive, not from an OpenStack release.
# mod_auth_mellon ships since the SAML federation phase (#573).
FROM ubuntu:noble@sha256:4fbb8e6a8395de5a7550b33509421a2bafbc0aab6c06ba2cef9ebffbc7092d90

ENV LANG=C.UTF-8

# Optional APT mirror override (e.g. https://azure.archive.ubuntu.com for
# GitHub Actions runners).  When empty the default Ubuntu mirrors are kept.
# On arm64 ports.ubuntu.com has no Azure equivalent so the sed is a no-op.
ARG APT_MIRROR=""
RUN if [ -n "$APT_MIRROR" ]; then \
        sed -i \
            -e "s|http://archive.ubuntu.com|${APT_MIRROR}|g" \
            -e "s|http://security.ubuntu.com|${APT_MIRROR}|g" \
            /etc/apt/sources.list.d/ubuntu.sources; \
    fi && \
    echo 'Acquire::Retries "3";' > /etc/apt/apt.conf.d/80-retries

# Runtime dependencies: Apache httpd and the distro mod_auth_openidc and
# mod_auth_mellon builds.
# Cache mounts speed up rebuilds under BuildKit; the trailing rm is a no-op
# when the mount is active but keeps the layer clean on non-BuildKit engines.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    DEBIAN_FRONTEND=noninteractive apt-get update && apt-get install -y --no-install-recommends \
    apache2 \
    ca-certificates \
    libapache2-mod-auth-mellon \
    libapache2-mod-auth-openidc \
    && rm -rf /var/lib/apt/lists/*

# Generic 'openstack' user/group (UID/GID 42424) shared by all service images
# so the sidecar runs under the same identity as the keystone container it
# fronts (see images/python-base/Dockerfile for the convention rationale).
RUN groupadd -g 42424 openstack && \
    useradd -u 42424 -g 42424 -M -d /var/lib/openstack -s /usr/sbin/nologin openstack

# Static server scaffold: module loads, the localhost-safe runtime paths (the
# container runs read-only with /tmp as its only writable mount), and the
# include hook the operator-rendered proxy.conf plugs into. conf.d/ and
# metadata/ are the mount points for the operator-projected federation Secret.
COPY httpd-base.conf /etc/keystone-federation-proxy/httpd-base.conf
RUN mkdir -p /etc/keystone-federation-proxy/conf.d /etc/keystone-federation-proxy/metadata

# Static OCI Image Spec annotations for local builds.
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="keystone-federation-proxy" \
      org.opencontainers.image.description="Apache reverse-proxy sidecar for Keystone federation (mod_auth_openidc + mod_auth_mellon)" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"

USER openstack
