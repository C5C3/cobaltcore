# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ---------- Stage 1: build ----------
# hadolint ignore=DL3006
FROM venv-builder AS build

# Comma-separated Python extras (unused by neutron today; kept for parity
# with the extra-packages.yaml wiring shared by all service images).
# Passed by CI from releases/<release>/extra-packages.yaml.
ARG PIP_EXTRAS=""
# Space-separated additional pip packages to install alongside the service.
# Empty for neutron today; kept for parity with the other service images.
# Passed by CI from releases/<release>/extra-packages.yaml.
ARG PIP_PACKAGES=""

# Install Neutron and extras into the shared virtualenv.
# Named build contexts supply the source tree and constraints file:
#   --build-context neutron=<path-to-neutron-source>
#   --build-context upper-constraints=<path-to-release-dir>
#
# 'set -f' disables pathname expansion for the install step: PIP_PACKAGES stays
# unquoted so multiple space-separated packages word-split, but a PEP 508
# extras suffix (e.g. some_lib[s3]) is also a glob bracket expression that the
# shell would rewrite if a matching file existed in the build CWD.
RUN --mount=type=bind,from=upper-constraints,source=upper-constraints.txt,target=/tmp/upper-constraints.txt \
    --mount=type=bind,from=neutron,target=/tmp/neutron,readwrite \
    PKG="/tmp/neutron" && \
    if [ -n "$PIP_EXTRAS" ]; then PKG="${PKG}[${PIP_EXTRAS}]"; fi && \
    set -f && \
    uv pip install --prefix /var/lib/openstack \
        --constraint /tmp/upper-constraints.txt \
        "$PKG" $PIP_PACKAGES

# No WSGI entry script ships in this image, neither generated nor
# hand-written. uv's --prefix install mode generates the console scripts
# declared in setup.cfg (27.0.3) and pyproject.toml (28.0.1), but neither
# release ships a neutron-server script. neutron/wsgi/api.py is byte-identical
# at both tags and binds a module-level `application` inside a
# threading.Lock() block, so the neutron-operator launches uWSGI with
# `--module neutron.wsgi.api` and locates configuration through
# OS_NEUTRON_CONFIG_DIR and OS_NEUTRON_CONFIG_FILES (spike #901 (b.2), (b.6);
# Meta #898 D10). 27.0.3 also generates the PBR wsgi_scripts entry neutron-api
# from neutron.cmd.server:main_api_uwsgi, which is unused; 28.0.1 declares no
# WSGI script.
# The image itself stays config-free. The package data file
# /var/lib/openstack/etc/neutron/api-paste.ini, byte-identical at both tags,
# is what #904 points [DEFAULT] api_paste_config at, or mounts to
# /etc/neutron/api-paste.ini.

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

# Runtime system packages required by Neutron. CI passes them from
# releases/<release>/extra-packages.yaml, which carries four entries.
# libpython3.12t64: the venv-builder-compiled uwsgi binary links
# libpython3.12.so.1.0, which python-base does not ship (the same rationale as
# barbican, glance, horizon and placement).
# openvswitch-common: neutron shells out to `ovsdb-client transact` in
# pre_fork_initialize (neutron/common/ovn/utils.py:116 at 27.0.3, :139 at
# 28.0.1) and dies with "[Errno 2] No such file or directory: 'ovsdb-client'"
# without the binary. Noble ships 3.3.9, and the ovsdb wire protocol is
# schema-independent, so it talks to the OVN 26.03 server from #897.
# haproxy and iproute2: the neutron-ovn-metadata-agent that #904 projects per
# Meta D7 spawns `haproxy -f <cfg>` (neutron/agent/metadata/driver_base.py:39
# and :249 at both tags) through `ip netns exec`
# (neutron/agent/linux/ip_lib.py:769,
# neutron/agent/linux/external_process.py:118).
# The guard keeps local builds without --build-arg EXTRA_APT_PACKAGES a clean
# no-op.
# hadolint ignore=DL3008
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    if [ -n "${EXTRA_APT_PACKAGES}" ]; then \
    DEBIAN_FRONTEND=noninteractive apt-get update && \
    apt-get install -y --no-install-recommends ${EXTRA_APT_PACKAGES} && \
    rm -rf /var/lib/apt/lists/*; \
    fi

# DNS resolution: the virtualenv holds ovs 3.5.1 (2025.2) and 3.7.0 (2026.1),
# whose ovs/dns_resolve.py resolves no hostname without the unbound Python
# module (importing it raises "ModuleNotFoundError: No module named
# 'unbound'"). This image deliberately ships no python3-unbound: the system
# package installs into the distribution interpreter, which is invisible to
# the virtualenv under /var/lib/openstack. Meta D9 publishes IP addresses in
# OVNCentral.status.dbAddress and status.internalDbAddress instead, and #904
# renders [ovn] ovn_nb_connection and ovn_sb_connection from those.

# Static OCI Image Spec annotations for local builds (runtime stage only).
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="neutron" \
      org.opencontainers.image.description="OpenStack neutron service" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"

USER openstack
