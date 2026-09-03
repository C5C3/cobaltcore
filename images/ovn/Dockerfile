# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# OVN and Open vSwitch daemons and client tools, compiled from upstream
# sources in a two-stage build and shipped in one runtime image.
# Release-independent: the image has no key in releases/*/source-refs.yaml
# and is not part of the per-service, per-release build matrix, so it is
# built once per OVN version instead of once per OpenStack release.
#
# Open vSwitch comes from the commit that OVN's 'ovs' git submodule points
# to at the pinned OVN tag (decision D1 of #898: one pin, one Renovate
# rule). OVN's Documentation/intro/install/general.rst notes under Build
# Requirements that the submodule is "not recommended to be used as a
# source for OVS build". ovn-kubernetes builds its OVS from exactly that
# gitlink in dist/images/Dockerfile.fedora, and D1 keeps the gitlink.

# ---------- Stage 1: build ----------
FROM ubuntu:noble@sha256:561618e2c15bf2397621dd04f96926663a3b5616c189cf7e38db7e82f5c538ea AS build

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

# Git tag of ovn-org/ovn and the single version pin of this image, bumped
# by the Renovate customManager for images/ovn/Dockerfile in renovate.json.
# Open vSwitch follows from the 'ovs' submodule gitlink at this tag, so
# there is no second version to choose. hack/ci-resolve-ovn-version.sh
# is the only parser of this line.
ARG OVN_VERSION=v26.03.2

# The commit ${OVN_VERSION} names, and the ref the checkout below fetches. A
# content pin, not a second version: it stands to OVN_VERSION as the sha256
# digest above stands to ubuntu:noble, and like a digest it is what the fetch
# asks for. A git tag is a mutable ref — whoever can push to ovn-org/ovn can
# move or delete it — so nothing here reaches the source through the tag.
# Bump it with OVN_VERSION; the new value is
# `git -C <ovn-clone-at-tag> rev-parse HEAD`, and a stale one makes
# verify_ovn.sh fail on the version the binaries report.
ARG OVN_COMMIT=3facc3b5e99ba2c863ec5f47f37466397f735802

# The commit the 'ovs' gitlink names at ${OVN_COMMIT}. The same kind of content
# pin for the other half of the tree. Naming it here keeps the OVS source off
# upstream's .gitmodules and turns a changed gitlink into a failed build
# instead of a silently different image. Bump it with OVN_VERSION; the new
# value is `git -C <ovn-clone-at-tag> rev-parse HEAD:ovs`, and a stale one is
# printed by the build error below.
ARG OVS_COMMIT=bdb95cc1920d4ab66fe062a9470eeb33a51d33e2

# Build requirements from OVN's Documentation/intro/install/general.rst:
# GNU make, a C compiler, libssl, libunbound, autoconf, automake and
# libtool. OVS also needs python3 for its code generation.
# Cache mounts speed up rebuilds under BuildKit; the trailing rm is a no-op
# when the mount is active but keeps the layer clean on non-BuildKit engines.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    DEBIAN_FRONTEND=noninteractive apt-get update && apt-get install -y --no-install-recommends \
    autoconf \
    automake \
    build-essential \
    ca-certificates \
    git \
    libcap-ng-dev \
    libssl-dev \
    libtool \
    libunbound-dev \
    pkg-config \
    python3 \
    && rm -rf /var/lib/apt/lists/*

# Source checkout. Both halves of the tree are fetched by the exact commit
# their pin names, so no mutable ref decides what is compiled: OVN comes from
# OVN_COMMIT rather than from the tag, and Open vSwitch from the commit the
# 'ovs' gitlink names there, straight from openvswitch/ovs rather than from
# whatever URL upstream's .gitmodules carries at that commit. A gitlink that
# no longer matches OVS_COMMIT fails the build loudly.
#
# GitHub does not reliably serve unauthenticated fetches to the git noble
# ships (2.43): since 2026-09-02 the anonymous upload-pack request can come
# back as a 401 challenge, which a build without a terminal reports as "could
# not read Username for 'https://github.com'". A build that mounts the
# github_token secret (hack/ci-build-ovn-image.sh and the build-ovn job pass
# the workflow token) sends it the way actions/checkout does, as a basic-auth
# header scoped to github.com, through git's environment config: it lives in
# this one RUN and in no layer. A build without the secret stays anonymous.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
RUN --mount=type=secret,id=github_token \
    export GIT_TERMINAL_PROMPT=0 && \
    if [ -s /run/secrets/github_token ]; then \
        auth="$(printf 'x-access-token:%s' "$(cat /run/secrets/github_token)" | base64 -w0)" && \
        export GIT_CONFIG_COUNT=1 \
            GIT_CONFIG_KEY_0="http.https://github.com/.extraheader" \
            GIT_CONFIG_VALUE_0="AUTHORIZATION: basic ${auth}"; \
    fi && \
    git init /src/ovn && \
    git -C /src/ovn remote add origin https://github.com/ovn-org/ovn.git && \
    git -C /src/ovn fetch --depth 1 origin "${OVN_COMMIT}" && \
    git -C /src/ovn checkout --detach FETCH_HEAD && \
    gitlink="$(git -C /src/ovn rev-parse "HEAD:ovs")" && \
    if [ "${gitlink}" != "${OVS_COMMIT}" ]; then \
        echo "ovs gitlink ${gitlink} != pinned OVS_COMMIT ${OVS_COMMIT}" >&2; \
        exit 1; \
    fi && \
    git init /src/ovs && \
    git -C /src/ovs remote add origin https://github.com/openvswitch/ovs.git && \
    git -C /src/ovs fetch --depth 1 origin "${OVS_COMMIT}" && \
    git -C /src/ovs checkout --detach FETCH_HEAD

# Open vSwitch, built in tree and staged into /out. --prefix=/usr puts the
# clients in /usr/bin and ovs-vswitchd and ovsdb-server in /usr/sbin;
# --localstatedir=/var and --sysconfdir=/etc keep the runtime paths where
# the daemons and ovs-ctl look for them.
WORKDIR /src/ovs
RUN ./boot.sh && \
    ./configure --prefix=/usr --localstatedir=/var --sysconfdir=/etc && \
    make -j"$(nproc)" && \
    make install DESTDIR=/out

# OVN, built against the OVS source tree above. --with-ovs-source is enough
# for an in-tree OVS build; --with-ovs-build is only required when OVS was
# configured in a separate build directory.
WORKDIR /src/ovn
RUN ./boot.sh && \
    ./configure --prefix=/usr --localstatedir=/var --sysconfdir=/etc --with-ovs-source=/src/ovs && \
    make -j"$(nproc)" && \
    make install DESTDIR=/out

# ---------- Stage 2: runtime ----------
FROM ubuntu:noble@sha256:561618e2c15bf2397621dd04f96926663a3b5616c189cf7e38db7e82f5c538ea

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

# Runtime dependencies. libssl3t64 is noble's OpenSSL 3 runtime, libunbound8
# its unbound runtime, libcap-ng0 the privilege-drop library the daemons
# link against. kmod (modprobe) and iproute2 (ip) serve the chassis
# DaemonSets of #903, which load host kernel modules from inside this image
# and inspect interfaces. No python3, no compiler and no git here.
# Cache mounts speed up rebuilds under BuildKit; the trailing rm is a no-op
# when the mount is active but keeps the layer clean on non-BuildKit engines.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    DEBIAN_FRONTEND=noninteractive apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    iproute2 \
    kmod \
    libcap-ng0 \
    libssl3t64 \
    libunbound8 \
    && rm -rf /var/lib/apt/lists/*

# DEVIATION from the original build-pipeline design:
# This image does not derive from python-base, so it creates the generic
# 'openstack' user/group (UID/GID 42424) itself instead of inheriting it.
# The distro packages would install per-daemon 'openvswitch' and 'ovn'
# users, but the source build carries no such packaging and one identity
# across all service images keeps the pod security contexts uniform.
RUN groupadd -g 42424 openstack && \
    useradd -u 42424 -g 42424 -M -d /var/lib/openstack -s /usr/sbin/nologin openstack

# Binaries plus the OVSDB schemas and the ovs-ctl and ovn-ctl helper scripts
# from the staged install. /out/usr/include, the static libraries under
# /out/usr/lib and /out/usr/share/man stay behind.
COPY --from=build --link /out/usr/bin /usr/bin
COPY --from=build --link /out/usr/sbin /usr/sbin
COPY --from=build --link /out/usr/share/openvswitch /usr/share/openvswitch
COPY --from=build --link /out/usr/share/ovn /usr/share/ovn

# Runtime state, configuration and log directories for both daemon
# families, owned by the user the daemons run as.
RUN mkdir -p \
        /var/run/openvswitch /var/run/ovn \
        /var/lib/openvswitch /var/lib/ovn \
        /var/log/openvswitch /var/log/ovn \
        /etc/openvswitch /etc/ovn && \
    chown -R openstack:openstack \
        /var/run/openvswitch /var/run/ovn \
        /var/lib/openvswitch /var/lib/ovn \
        /var/log/openvswitch /var/log/ovn \
        /etc/openvswitch /etc/ovn

# Static OCI Image Spec annotations for local builds.
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="ovn" \
      org.opencontainers.image.description="OVN and Open vSwitch daemons and client tools built from pinned upstream sources" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"

# ovsdb-server, ovn-northd and the client tools need no privileges, so the
# default is non-root. The chassis pods of #903 set runAsUser 0 and the
# capabilities they need themselves. The image has no ENTRYPOINT or CMD and
# no component switch: every workload names the daemon it runs.
USER openstack
