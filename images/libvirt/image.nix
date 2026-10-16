# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# libvirt and QEMU for hypervisor nodes whose host image carries neither. The
# hypervisor package (#1142) runs this image privileged as a DaemonSet beside
# nova-compute. Release-independent: libvirt and QEMU come from the
# nixpkgs-images revision flake.lock pins, not from an OpenStack release.
# libvirt is nix/pkgs/libvirt.nix, the derivation nova-compute's
# libvirt-python builds against, so the client in nova-compute and the daemon
# here are one libvirt (checks.<system>.libvirt-pairing in flake.nix). QEMU
# is ./qemu.nix.
#
# The image writes its own root instead of nix/lib/fhs-layout.nix, whose
# /etc/passwd adds the openstack user this image has never had:
#   /bin/sh, /bin/bash      bash; /usr/bin/env is coreutils'
#   /usr/bin/qemu-system-x86_64
#                           libvirtd finds the emulator through PATH, where
#                           /usr/bin comes before /bin, and domain XML records
#                           this path, which stays the same across image
#                           updates and migrations
#   /usr/bin/virt-admin     the path a host unit runs through nsenter --mount
#                           to reload libvirtd's TLS files
#   /usr/bin/systemctl, /usr/bin/systemd-run
#                           what the lab's libvirtd.sh starts libvirtd with
#   /usr/share/qemu         QEMU's share/qemu, where libvirt's qemu_datadir
#                           looks for the firmware descriptors. QEMU's
#                           descriptors name the firmware by its store path,
#                           which libvirt would record in a UEFI guest's XML
#                           and which the next image would not have, so the
#                           image's copies name /usr/share/qemu instead
#   /usr/share/OVMF/OVMF_CODE_4M.fd, /usr/share/OVMF/OVMF_VARS_4M.fd
#                           QEMU's edk2 x86_64 code and vars, under the names
#                           noble's ovmf package used, which the XML of a
#                           guest started on the Docker image records
#
# libvirt's bin/ is linked entry by entry. libvirt itself is not a content,
# because its etc/ would make /etc/libvirt a store link the lab scripts
# cannot write; /etc/libvirt is a writable copy of libvirt's defaults. The
# copy keeps /etc/libvirt/qemu/networks/default.xml and drops the autostart
# link of that 'default' NAT network: the consumer runs libvirtd in the
# host's network namespace, where that network would create virbr0 and NAT
# rules on the node. qemu.conf keeps noble's mode 0600.
#
# kmod is for loading vhost_net and the other modules from the host's module
# tree; nixpkgs builds it with xz and zstd and searches /lib/modules last.
# dmidecode is what libvirt reads the host's SMBIOS data with. iproute2
# brings ip. getent reads the accounts below.
#
# DEVIATION from the original build-pipeline design:
# This image keeps root. It has no User and creates no generic 'openstack'
# user/group, because libvirtd has to run as root to manage domains, devices
# and cgroups on the node. The guest accounts noble's libvirt packages
# created are written by fakeNss with noble's IDs: the 'libvirt-qemu' user
# (64055) and the 'libvirt' (103) and 'kvm' (104) groups. nixpkgs' libvirt
# compiles root as its default QEMU user, and the consumers set user = "root"
# in qemu.conf themselves.
{
  pkgs,
  libvirt,
  ...
}:
let
  inherit (pkgs) lib;
  defaults = import ../../nix/lib/image-defaults.nix;
  qemu = import ./qemu.nix { inherit pkgs; };

  links = {
    "bin/sh" = "${pkgs.bash}/bin/bash";
    "bin/bash" = "${pkgs.bash}/bin/bash";
    "usr/bin/env" = "${pkgs.coreutils}/bin/env";
    "usr/bin/qemu-system-x86_64" = "${qemu}/bin/qemu-system-x86_64";
    "usr/bin/virt-admin" = "${libvirt}/bin/virt-admin";
    "usr/bin/systemctl" = "${pkgs.systemd}/bin/systemctl";
    "usr/bin/systemd-run" = "${pkgs.systemd}/bin/systemd-run";
    "usr/share/OVMF/OVMF_CODE_4M.fd" = "${qemu}/share/qemu/edk2-x86_64-code.fd";
    "usr/share/OVMF/OVMF_VARS_4M.fd" = "${qemu}/share/qemu/edk2-i386-vars.fd";
  };

  root = pkgs.runCommand "libvirt-root" { } (
    lib.concatStrings (
      lib.mapAttrsToList (rel: target: ''
        if [ ! -e ${lib.escapeShellArg target} ]; then
          echo ${lib.escapeShellArg "libvirt image: ${rel} names ${target}, which does not exist"} >&2
          exit 1
        fi
        mkdir -p "$(dirname "$out/"${lib.escapeShellArg rel})"
        ln -s ${lib.escapeShellArg target} "$out/"${lib.escapeShellArg rel}
      '') links
    )
    + ''
      mkdir -p "$out/usr/share/qemu/firmware"
      for entry in ${qemu}/share/qemu/*; do
        [ "''${entry##*/}" = firmware ] || ln -s "$entry" "$out/usr/share/qemu/"
      done
      for descriptor in ${qemu}/share/qemu/firmware/*.json; do
        sed 's|${qemu}/share/qemu/|/usr/share/qemu/|g' "$descriptor" \
          >"$out/usr/share/qemu/firmware/''${descriptor##*/}"
      done
      if grep -l /nix/store "$out"/usr/share/qemu/firmware/*.json; then
        echo "libvirt image: a firmware descriptor still names a store path" >&2
        exit 1
      fi
      # The glob skips the wrapped .libvirtd-wrapped beside the libvirtd wrapper.
      mkdir -p "$out/bin"
      for program in ${libvirt}/bin/*; do
        ln -s "$program" "$out/bin/"
      done
    ''
  );

  nss = pkgs.dockerTools.fakeNss.override {
    extraPasswdLines = [ "libvirt-qemu:x:64055:104:Libvirt Qemu:/var/lib/libvirt:/usr/sbin/nologin" ];
    extraGroupLines = [
      "libvirt:x:103:"
      "kvm:x:104:"
      "libvirt-qemu:x:64055:libvirt-qemu"
    ];
  };
in
pkgs.dockerTools.streamLayeredImage {
  name = "libvirt";
  tag = "latest";
  inherit (defaults) created;
  passthru = { inherit libvirt qemu; };

  contents = [
    root
    nss
    qemu
    pkgs.coreutils
    pkgs.findutils
    pkgs.gnugrep
    pkgs.gnused
    pkgs.gawk
    pkgs.kmod
    pkgs.dmidecode
    pkgs.iproute2
    pkgs.getent
  ];

  enableFakechroot = true;
  fakeRootCommands = ''
    mkdir -p -m 1777 /tmp
    chmod 1777 /tmp
    mkdir -p /run /var
    ln -s /run /var/run

    mkdir -p /etc/libvirt
    cp -rL ${libvirt}/etc/libvirt/. /etc/libvirt/
    chmod -R u+w /etc/libvirt
    rm -f /etc/libvirt/qemu/networks/autostart/default.xml
    mkdir -p /etc/libvirt/qemu/networks/autostart
    chmod 0600 /etc/libvirt/qemu.conf
    mkdir -p /var/lib/libvirt /var/log/libvirt/qemu /var/cache/libvirt
  '';

  config = {
    Env = [
      "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
      "LANG=C.UTF-8"
    ];
    Labels = defaults.labels // {
      "org.opencontainers.image.title" = "libvirt";
      "org.opencontainers.image.description" = "libvirt and QEMU/KVM for containerized hypervisor nodes";
      "org.opencontainers.image.version" = libvirt.version;
    };
  };
}
