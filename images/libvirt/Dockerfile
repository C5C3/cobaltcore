# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# libvirt and QEMU for hypervisor nodes whose host image carries neither. The
# hypervisor package (#1142) runs this image privileged as a DaemonSet beside
# nova-compute. Release-independent: every package comes from the distro
# (ubuntu:noble) package archive, not from an OpenStack release. noble because
# images/nova-compute/ links noble's libvirt0, so the client in nova-compute
# and the daemon here are one libvirt.
FROM ubuntu:noble@sha256:008173c23f95b170204355c12626cb5a965d779a7e1283b09e9cffbb1bf33ca3

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

# Runtime dependencies: the libvirt daemon, QEMU for x86 guests, the QEMU disk
# tools and the OVMF UEFI firmware.
# libvirt-clients brings virsh and virt-admin (the TLS reload).
# kmod is for loading vhost_net from the host's module tree.
# dmidecode is what libvirt reads the host's SMBIOS data with.
# iproute2 brings ip, a Recommends of libvirt-daemon-system that
# --no-install-recommends drops.
# systemd comes in as a dependency and brings systemd-run.
# libvirt-daemon-config-network is a hard dependency and enables autostart of
# the 'default' NAT network. The consumer runs libvirtd in the host's network
# namespace, where that network would create virbr0 and NAT rules on the node,
# so the rm drops the autostart link. The definition
# /etc/libvirt/qemu/networks/default.xml stays.
# Cache mounts keep the package lists and the downloaded .debs between BuildKit
# builds, and neither lands in the layer. The base image's docker-clean hook
# deletes the .debs after every install, so it is removed first.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean \
    && DEBIAN_FRONTEND=noninteractive apt-get update && apt-get install -y --no-install-recommends \
    dmidecode \
    iproute2 \
    kmod \
    libvirt-clients \
    libvirt-daemon-system \
    ovmf \
    qemu-system-x86 \
    qemu-utils \
    && rm -f /etc/libvirt/qemu/networks/autostart/default.xml

# DEVIATION from the original build-pipeline design:
# This image keeps root. It has no USER instruction and creates no generic
# 'openstack' user/group, because libvirtd has to run as root to manage
# domains, devices and cgroups on the node. The libvirt packages bring their
# own accounts for the guests: the 'libvirt-qemu' user that QEMU runs as and
# the 'libvirt' and 'kvm' groups.

# Static OCI Image Spec annotations for local builds.
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="libvirt" \
      org.opencontainers.image.description="libvirt and QEMU/KVM for containerized hypervisor nodes" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"

# No ENTRYPOINT or CMD: the consumer renders the libvirtd command and its
# configuration onto the DaemonSet container.
