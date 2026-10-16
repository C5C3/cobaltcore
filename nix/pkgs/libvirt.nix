# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The one libvirt both hypervisor images build on: images/libvirt/image.nix
# runs its daemon, and the libvirt-python row of nix/lib/python-env.nix
# builds nova-compute's binding against it, so client and daemon are one
# derivation (checks.<system>.libvirt-pairing in flake.nix).
#
# nixpkgs' libvirt passes --sysconfdir=/var/lib and reads
# /var/lib/libvirt/libvirtd.conf and its TLS files below /var/lib/pki. The
# lab (deploy/lab/metal-stack/hypervisor/libvirt-configmap.yaml) and the e2e
# suite (tests/e2e-nova-libvirt/server-boot/00-libvirt.yaml) write
# /etc/libvirt/libvirtd.conf and /etc/libvirt/qemu.conf, mount the TLS files
# below /etc/pki and start libvirtd without --config. With --sysconfdir=/etc
# the daemon reads those, keeps its secrets below /etc/libvirt/secrets, and
# installs its default configuration below $out/etc/libvirt.
#
# nixpkgs leaves qemu_datadir empty, so libvirt looks for the firmware
# descriptors below its own store path. /usr/share/qemu is where the image
# links QEMU's share/qemu.
#
# nixpkgs passes no qemu_user or qemu_group, so the compiled default QEMU
# user is root; the lab and the e2e suite write user = "root" into qemu.conf
# themselves.
#
# nixpkgs enables libvirt's test suite, which meson compiles whenever tests
# are enabled. This build runs no tests (doCheck below), so it disables them.
{ libvirt, lib }:
let
  # Each flag nixpkgs passes, and what this build passes in its place.
  replacements = {
    "--sysconfdir=/var/lib" = "--sysconfdir=/etc";
    "-Dqemu_datadir=" = "-Dqemu_datadir=/usr/share/qemu";
    "-Dtests=enabled" = "-Dtests=disabled";
    # meson refuses expensive tests without tests.
    "-Dexpensive_tests=enabled" = "-Dexpensive_tests=disabled";
  };
in
libvirt.overrideAttrs (old: {
  mesonFlags =
    let
      missing = lib.filter (flag: !lib.elem flag old.mesonFlags) (lib.attrNames replacements);
    in
    if missing != [ ] then
      throw "nix/pkgs/libvirt.nix: nixpkgs' libvirt no longer passes ${lib.head missing}; re-check the override"
    else
      map (flag: replacements.${flag} or flag) old.mesonFlags;

  # The override changes install paths and drops the tests. Hydra ran the
  # suite on the same source; verify_libvirt.sh and the lab run are the
  # contract.
  doCheck = false;
})
