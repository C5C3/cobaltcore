# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# The image nova-compute runs from on a compute cluster's hypervisor nodes. It
# is built from nova's own source pin, patches and constraints, so
# nova-compute:<release> and nova:<release> carry the same nova. The
# control-plane image of images/nova/ stays without the libvirt binding and
# without host tooling (#1014 D14); this one adds both. A directory of its own
# rather than a second stage of images/nova/Dockerfile: the nightly GHCR
# cleanup derives its package list from images/<name>/, every nova build takes
# the last stage as its default target, and each contract script describes one
# Dockerfile.

# ---------- Stage 1: build ----------
# hadolint ignore=DL3006
FROM venv-builder AS build

# Comma-separated Python extras (empty for nova-compute; kept for parity with
# the extra-packages.yaml wiring shared by all service images).
# Passed by CI from the nova-compute block of
# releases/<release>/extra-packages.yaml.
ARG PIP_EXTRAS=""
# Space-separated additional pip packages to install alongside nova. CI passes
# libvirt-python from the nova-compute block of
# releases/<release>/extra-packages.yaml; upper-constraints.txt fixes its
# version.
ARG PIP_PACKAGES=""

# libvirt-python publishes no wheels, so the install below builds it from its
# sdist against the libvirt API description and the pkg-config file that
# libvirt-dev ships. noble's libvirt 10.0.0 builds both pins (11.6.0 at
# 2025.2, 12.0.0 at 2026.1). Build-only: the runtime stage installs libvirt0,
# the shared libraries the binding's extension modules link.
# hadolint ignore=DL3008
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    DEBIAN_FRONTEND=noninteractive apt-get update && \
    apt-get install -y --no-install-recommends libvirt-dev pkg-config

# Install Nova and the libvirt binding into the shared virtualenv.
# Named build contexts supply the source tree and constraints file:
#   --build-context nova=<path-to-nova-source>
#   --build-context upper-constraints=<path-to-release-dir>
#
# 'set -f' disables pathname expansion for the install step: PIP_PACKAGES stays
# unquoted so multiple space-separated packages word-split, but a PEP 508
# extras suffix (e.g. some_lib[s3]) is also a glob bracket expression that the
# shell would rewrite if a matching file existed in the build CWD.
RUN --mount=type=bind,from=upper-constraints,source=upper-constraints.txt,target=/tmp/upper-constraints.txt \
    --mount=type=bind,from=nova,target=/tmp/nova,readwrite \
    PKG="/tmp/nova" && \
    if [ -n "$PIP_EXTRAS" ]; then PKG="${PKG}[${PIP_EXTRAS}]"; fi && \
    set -f && \
    uv pip install --prefix /var/lib/openstack \
        --constraint /tmp/upper-constraints.txt \
        "$PKG" $PIP_PACKAGES

# ---------- Stage 2: runtime ----------
# hadolint ignore=DL3006
FROM python-base

# DEVIATION from the original build-pipeline design:
# Uses the generic 'openstack' user (UID/GID 42424) from python-base instead
# of a per-service user. See python-base/Dockerfile for rationale.

# Space-separated runtime system packages.
# Passed by CI from the nova-compute block of
# releases/<release>/extra-packages.yaml.
ARG EXTRA_APT_PACKAGES=""

COPY --from=build --link /var/lib/openstack /var/lib/openstack

# Runtime system packages: the libvirt client libraries, qemu-img, the os-brick
# host tools for iSCSI, multipath and NVMe, cryptsetup and genisoimage. The
# extra-packages.yaml entries name the code that runs each of them. No uWSGI
# runs here, so no libpython3.12t64; sudo comes from python-base.
# The postinst scripts of open-iscsi and nvme-cli generate
# /etc/iscsi/initiatorname.iscsi, /etc/nvme/hostnqn and /etc/nvme/hostid at
# build time, and multipath-tools ships /etc/multipath.conf with
# user_friendly_names yes. os-brick reports the initiator name and the host
# NQN to Cinder as the node's identity, so baked files would give every
# compute node the same IQN and NQN, and a baked multipath configuration can
# disagree with the host's multipathd. The rm removes all four; the consumer
# mounts the host's files (#1061). It sits outside the guard, so a local build
# without --build-arg EXTRA_APT_PACKAGES stays a clean no-op.
# /var/lib/apt/lists is a cache mount for the duration of the RUN, so its
# contents never reach the layer.
# hadolint ignore=DL3008
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    if [ -n "${EXTRA_APT_PACKAGES}" ]; then \
    DEBIAN_FRONTEND=noninteractive apt-get update && \
    apt-get install -y --no-install-recommends ${EXTRA_APT_PACKAGES}; \
    fi && \
    rm -f /etc/iscsi/initiatorname.iscsi /etc/nvme/hostnqn /etc/nvme/hostid \
        /etc/multipath.conf

# The state directories Nova writes to. /var/lib/nova is meant as [DEFAULT]
# state_path, where a compute stores its node identity, compute_id; nova's
# default is $pybasedir, so the configuration has to set it. /var/lib/nova/tmp
# is the [oslo_concurrency] lock_path, and /var/lib/nova/instances is
# instances_path, the default $state_path/instances. All three ship empty and
# owned by 42424. The consumer (#1061) mounts /var/lib/nova from the host and
# sets state_path to it: compute_id must survive pod recreation, or the next
# start generates a new node UUID that collides with the existing ComputeNode
# record. That mount replaces this layout, so a plain `docker run` is the only
# one that gets it; the pod has to give the host directories the same owner.
RUN mkdir -p /var/lib/nova/instances \
        /var/lib/nova/tmp && \
    chown -R 42424:42424 /var/lib/nova

# nova's default root helper is `sudo nova-rootwrap <[DEFAULT] rootwrap_config>`
# (nova/utils.py), and rootwrap_config defaults to /etc/nova/rootwrap.conf.
# nova-compute hands that helper to every privsep context it starts (os-brick,
# nova's sys_admin_pctxt, os-vif). The installed rootwrap.conf points
# filters_path at /etc/nova/rootwrap.d,/usr/share/nova/rootwrap and exec_dirs
# at the system directories, where neither the filters nor privsep-helper
# live. This file records where this image keeps them: image plumbing, not
# service configuration. The two greps fail the build when an upstream
# release reshapes the file and the sed matches nothing.
# A consumer mounts its configuration below /etc/nova, never a volume at
# /etc/nova itself, which would hide this file.
RUN mkdir -p /etc/nova && \
    sed -e 's|^filters_path=.*|filters_path=/var/lib/openstack/etc/nova/rootwrap.d|' \
        -e 's|^exec_dirs=|exec_dirs=/var/lib/openstack/bin,|' \
        /var/lib/openstack/etc/nova/rootwrap.conf > /etc/nova/rootwrap.conf && \
    grep -qx 'filters_path=/var/lib/openstack/etc/nova/rootwrap.d' /etc/nova/rootwrap.conf && \
    grep -q '^exec_dirs=/var/lib/openstack/bin,' /etc/nova/rootwrap.conf

# The sudo posture: a global secure_path that resolves nova-rootwrap, also for
# a consumer that runs nova-compute as root, and one rule that lets the
# openstack user run nova-rootwrap with this image's rootwrap.conf. Every
# privsep daemon nova-compute starts goes through that call, and rootwrap
# always admits privsep-helper, so the rule is root-equivalent: what nova's
# own design grants its service user. sudo needs a container that allows
# privilege escalation (allowPrivilegeEscalation not false). visudo fails the
# build on a malformed file instead of letting it disable sudo. -s makes a
# reference to an undefined alias an error: without it, a rule that lost its
# tag colon (`NOPASSWD` read as a Cmnd_Alias name) parses with a warning and
# grants nothing.
COPY --chmod=0440 sudoers /etc/sudoers.d/nova-compute
RUN visudo -csf /etc/sudoers.d/nova-compute

# Static OCI Image Spec annotations for local builds (runtime stage only).
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="nova-compute" \
      org.opencontainers.image.description="OpenStack nova-compute with the libvirt driver and host storage tooling" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"

USER openstack
