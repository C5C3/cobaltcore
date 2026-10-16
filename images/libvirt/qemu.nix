# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The QEMU of the libvirt image: nixpkgs' qemu for x86 guests with the rbd
# block driver and without the drivers that open a network connection on a
# guest's behalf. nixpkgs builds QEMU without modules, so every block driver
# is linked into the binaries and the choice is made here, at configure time.
{ pkgs }:
(pkgs.qemu.override {
  # The one target noble's qemu-system-x86 carried, on both platforms.
  hostCpuTargets = [ "x86_64-softmmu" ];

  # The image ships no manual, so the Sphinx build of the doc output goes.
  enableDocs = false;

  # The rbd block driver behind the lab's RBD volumes (#1345).
  cephSupport = true;

  # QEMU would otherwise connect to a URL that a disk or a backing file
  # names, from the node's address. Nothing here uses the iSCSI, curl (http,
  # https, ftp, ftps), NFS or SSH drivers: libiscsi is switched off here and
  # curl by the configure flag below, and nixpkgs passes neither libnfs nor
  # libssh. QEMU's NBD client is compiled in, so a disk or a backing file
  # that names an NBD server still makes QEMU connect to it; nova rejects a
  # downloaded Glance image that has a backing file.
  libiscsiSupport = false;

  # The UI, audio and spice parts noble keeps in Recommends
  # (qemu-system-gui, qemu-system-modules-spice), which the Docker image
  # installed without. VNC, ALSA, seccomp, numa and TPM keep their defaults.
  gtkSupport = false;
  sdlSupport = false;
  openGLSupport = false;
  pulseSupport = false;
  pipewireSupport = false;
  jackSupport = false;
  spiceSupport = false;
  smartcardSupport = false;
}).overrideAttrs
  (old: {
    configureFlags = old.configureFlags ++ [ "--disable-curl" ];
  })
