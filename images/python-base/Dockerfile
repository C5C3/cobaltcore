# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

FROM ubuntu:noble@sha256:84e77dee7d1bc93fb029a45e3c6cb9d8aa4831ccfcc7103d36e876938d28895b

ENV PATH=/var/lib/openstack/bin:$PATH
ENV LANG=C.UTF-8

# Runtime dependencies shared by all services
RUN DEBIAN_FRONTEND=noninteractive apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    netbase \
    python3 \
    sudo \
    tzdata \
    && rm -rf /var/lib/apt/lists/*

# DEVIATION from architecture/docs/08-container-images/01-build-pipeline.md:
# The architecture doc's Keystone Dockerfile example creates a per-service user
# (e.g., groupadd keystone / useradd keystone). We use a generic 'openstack'
# user/group (UID/GID 42424) shared by all service images to reduce complexity
# and image layers. Each service image inherits this user via USER openstack.
RUN groupadd -g 42424 openstack && \
    useradd -u 42424 -g 42424 -M -d /var/lib/openstack -s /usr/sbin/nologin openstack

# CC-0031: Static OCI Image Spec annotations for local builds.
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="python-base" \
      org.opencontainers.image.description="Python runtime base image for OpenStack services" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"
