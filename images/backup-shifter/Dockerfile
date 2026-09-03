# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# rclone shifter for the OVN database backups. It is the 'shifter' container
# of the OVNCentral backup CronJob and exists only when spec.backup.s3 is set:
# the 'backup' init container writes the northbound and southbound snapshots
# to a PVC, then this container copies that PVC to the configured S3 bucket
# with 'rclone copy'. Release-independent: rclone comes from the distro
# (ubuntu:noble) package archive, not from an OpenStack release.
FROM ubuntu:noble@sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517

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

# Runtime dependencies: rclone and the CA bundle it verifies the HTTPS S3
# endpoint against.
# Cache mounts speed up rebuilds under BuildKit; the trailing rm is a no-op
# when the mount is active but keeps the layer clean on non-BuildKit engines.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    DEBIAN_FRONTEND=noninteractive apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    rclone \
    && rm -rf /var/lib/apt/lists/*

# DEVIATION from the original build-pipeline design:
# This image does not derive from python-base, so it creates the generic
# 'openstack' user/group (UID/GID 42424) itself instead of inheriting it.
# The rclone package installs no service account of its own, and the same
# identity across all images lets the CronJob keep one pod security context
# for the 'backup' init container and this one.
RUN groupadd -g 42424 openstack && \
    useradd -u 42424 -g 42424 -M -d /var/lib/openstack -s /usr/sbin/nologin openstack

# Static OCI Image Spec annotations for local builds.
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="backup-shifter" \
      org.opencontainers.image.description="rclone shifter that copies OVN database backups from a PVC to S3" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"

# No ENTRYPOINT or CMD: the operator renders the rclone command onto the
# CronJob container together with the RCLONE_S3_* environment.
USER openstack
