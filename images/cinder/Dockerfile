# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ---------- Stage 1: build ----------
# hadolint ignore=DL3006
FROM venv-builder AS build

# Comma-separated Python extras (unused by cinder today; kept for parity
# with the extra-packages.yaml wiring shared by all service images).
# Passed by CI from releases/<release>/extra-packages.yaml.
ARG PIP_EXTRAS=""
# Space-separated additional pip packages to install alongside the service.
# Empty for cinder today; kept for parity with the other service images.
# Passed by CI from releases/<release>/extra-packages.yaml.
ARG PIP_PACKAGES=""

# Install Cinder and extras into the shared virtualenv.
# Named build contexts supply the source tree and constraints file:
#   --build-context cinder=<path-to-cinder-source>
#   --build-context upper-constraints=<path-to-release-dir>
#
# 'set -f' disables pathname expansion for the install step: PIP_PACKAGES stays
# unquoted so multiple space-separated packages word-split, but a PEP 508
# extras suffix (e.g. some_lib[s3]) is also a glob bracket expression that the
# shell would rewrite if a matching file existed in the build CWD.
RUN --mount=type=bind,from=upper-constraints,source=upper-constraints.txt,target=/tmp/upper-constraints.txt \
    --mount=type=bind,from=cinder,target=/tmp/cinder,readwrite \
    PKG="/tmp/cinder" && \
    if [ -n "$PIP_EXTRAS" ]; then PKG="${PKG}[${PIP_EXTRAS}]"; fi && \
    set -f && \
    uv pip install --prefix /var/lib/openstack \
        --constraint /tmp/upper-constraints.txt \
        "$PKG" $PIP_PACKAGES

# uv's --prefix install mode generates the nine console scripts declared in
# setup.cfg (27.0.0) and pyproject.toml (28.0.0): cinder-api, cinder-backup,
# cinder-manage, cinder-rootwrap, cinder-rtstool, cinder-scheduler,
# cinder-status, cinder-volume and cinder-volume-usage-audit. No entry script
# ships in this image, neither generated nor hand-written.
# cinder/wsgi/api.py is byte-identical at both tags and binds a module-level
# `application` inside a threading.Lock() block, so the cinder-operator (#987)
# launches uWSGI with `--module cinder.wsgi.api:application` and
# `--pyargv "--config-dir <dir>"`, because initialize_application() reads
# CONF(sys.argv[1:]) (spike #985, evidence part 1, section (c)). The PBR
# wsgi_scripts entry cinder-wsgi exists at 27.0.0 only and nothing calls it.
# The image itself stays config-free. The package data files api-paste.ini,
# resource_filters.json, rootwrap.conf and rootwrap.d/volume.filters land under
# /var/lib/openstack/etc/cinder/ at both tags, and
# tests/container-images/verify_cinder.sh asserts their presence.

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

# The state directories Cinder writes to. /var/lib/cinder is [DEFAULT]
# state_path; mnt and backup_mount are the two os-brick mount bases
# (nfs_mount_point_base, backup_mount_point_base); conversion is
# image_conversion_dir; tmp is [oslo_concurrency] lock_path; coordination is
# the tooz file:// directory of #979 decisions D2 and D12. All five ship empty
# and owned by 42424. The cinder-operator (#987) mounts an emptyDir over
# /var/lib/cinder under readOnlyRootFilesystem, so this layout is what a plain
# `docker run` gets, not a runtime dependency.
RUN mkdir -p /var/lib/cinder/mnt \
        /var/lib/cinder/backup_mount \
        /var/lib/cinder/conversion \
        /var/lib/cinder/tmp \
        /var/lib/cinder/coordination && \
    chown -R 42424:42424 /var/lib/cinder

# The exec readiness probe of the cinder-scheduler, cinder-volume and
# cinder-backup processes (#979 D2): it reports ready when a process of this
# container holds a socket established to the broker port, the semantics of
# kolla's healthcheck_port. The socket is matched by inode, because
# /proc/net/tcp* is scoped to the network namespace every container of the pod
# shares. The cinder-operator (#987) wires it as
# `readinessProbe.exec.command: ["/var/lib/openstack/bin/cinder-amqp-ready"]`;
# the two paths must stay in lockstep.
COPY --chmod=0755 cinder-amqp-ready /var/lib/openstack/bin/cinder-amqp-ready

# Runtime system packages required by Cinder. CI passes them from
# releases/<release>/extra-packages.yaml, which carries three entries.
# libpython3.12t64: the venv-builder-compiled uwsgi binary links
# libpython3.12.so.1.0, which python-base does not ship (the same rationale as
# the other service images).
# nfs-common: NfsDriver.do_setup (cinder/volume/drivers/nfs.py) probes for
# mount.nfs through the root helper and raises NfsException when the probe's
# argv[0] is missing. Under the restricted posture (#979 D3) the pod never
# mounts and the probe's non-zero exit is tolerated, but the binary must exist.
# The root helper is `sudo cinder-rootwrap` with no sudoers entry (#979 D3);
# sudo comes from python-base.
# qemu-utils: create-from-image, clone and extend go through
# cinder/image/image_utils.py (fetch_to_raw, resize_image, convert_image) and
# the LUKS qcow2 pre-create in cinder/volume/drivers/remotefs.py.
# The guard keeps local builds without --build-arg EXTRA_APT_PACKAGES a clean
# no-op. /var/lib/apt/lists is a cache mount for the duration of the RUN, so
# its contents never reach the layer and the sibling images' trailing
# `rm -rf /var/lib/apt/lists/*` only empties the cache the next build wants.
# hadolint ignore=DL3008
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    if [ -n "${EXTRA_APT_PACKAGES}" ]; then \
    DEBIAN_FRONTEND=noninteractive apt-get update && \
    apt-get install -y --no-install-recommends ${EXTRA_APT_PACKAGES}; \
    fi

# Static OCI Image Spec annotations for local builds (runtime stage only).
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="cinder" \
      org.opencontainers.image.description="OpenStack cinder service" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"

USER openstack
