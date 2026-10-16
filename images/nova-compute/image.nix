# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The image nova-compute runs from on a compute cluster's hypervisor nodes. It
# is built from the nova lock with the lock's nova-compute dependency group,
# so nova-compute:<release> and nova:<release> carry the same nova. The
# control-plane image of images/nova/ stays without the libvirt binding and
# without host tooling (#1014 D14); this one adds both. A directory of its own
# rather than part of images/nova/image.nix: the nightly GHCR cleanup derives
# its package list from images/<name>/, and each contract script describes one
# image. libvirt-python builds against nix/pkgs/libvirt.nix, the libvirt the
# libvirt image runs, so client and daemon are one libvirt
# (checks.<system>.libvirt-pairing in flake.nix).
#
# The host tools come from nixpkgs, whose packages run no postinst, but
# nixpkgs' open-iscsi installs an /etc/iscsi/initiatorname.iscsi it generated
# at build time. os-brick reports the initiator name and the host NQN to
# Cinder as the node's identity, so baked files would give every compute
# node the same IQN and NQN, and a baked multipath configuration can disagree
# with the host's multipathd. The rm removes /etc/iscsi/initiatorname.iscsi,
# /etc/nvme/hostnqn, /etc/nvme/hostid and /etc/multipath.conf, whichever
# package brings them; the consumer mounts the host's files (#1061).
#
# nova's default root helper is `sudo nova-rootwrap <[DEFAULT]
# rootwrap_config>` (nova/utils.py), and rootwrap_config defaults to
# /etc/nova/rootwrap.conf. nova-compute hands that helper to every privsep
# context it starts (os-brick, nova's sys_admin_pctxt, os-vif). The
# rootwrap.conf nova installs points filters_path at
# /etc/nova/rootwrap.d,/usr/share/nova/rootwrap and exec_dirs at the system
# directories, where neither the filters nor privsep-helper live. The file
# this image writes (nix/lib/rootwrap-conf.nix) names the venv's filters and
# searches /var/lib/openstack/bin, then the bin directory of every package
# below: image plumbing, not service configuration. A consumer mounts its
# configuration below /etc/nova, never a volume at /etc/nova itself, which
# would hide this file.
#
# The sudo posture: sudoers sets a global secure_path that resolves
# nova-rootwrap, also for a consumer that runs nova-compute as root, and one
# rule that lets the openstack user run nova-rootwrap with this image's
# rootwrap.conf. Every privsep daemon nova-compute starts goes through that
# call, and rootwrap always admits privsep-helper, so the rule is
# root-equivalent: what nova's own design grants its service user. sudo needs
# a container that allows privilege escalation (allowPrivilegeEscalation not
# false). visudo fails the build on a malformed file instead of letting it
# disable sudo. -s makes a reference to an undefined alias an error: without
# it, a rule that lost its tag colon (`NOPASSWD` read as a Cmnd_Alias name)
# parses with a warning and grants nothing. sudo runs the PAM stack of
# nix/lib/service-image.nix, whose account phase permits: fakeNss writes no
# /etc/shadow, and the one rule names a nologin service user with no
# password ageing to check.
#
# The rados and rbd modules come from nix/pkgs/ceph-bindings.nix through the
# ceph-bindings.pth that nix/lib/service-image.nix writes into the venv.
#
# DEVIATION from the original build-pipeline design: the image runs as the
# generic 'openstack' user (UID/GID 42424) instead of a per-service user; see
# the DEVIATION paragraph of nix/lib/service-image.nix.
{
  pkgs,
  serviceImage,
  env,
  cephBindings,
  ...
}:
let
  inherit (pkgs) lib;

  packages = [
    # qemu-img create (nova/virt/libvirt/utils.py), info and convert
    # (nova/privsep/qemu.py).
    pkgs.qemu-utils
    # iscsiadm (os_brick/initiator/connectors/iscsi.py); the root link below
    # gives /sbin/iscsiadm, which os-brick reads at import.
    pkgs.openiscsi
    # multipath and multipathd (os_brick/initiator/linuxscsi.py).
    pkgs.multipath-tools
    # nvme (os_brick/initiator/connectors/nvmeof.py,
    # os_brick/privileged/nvmeof.py).
    pkgs.nvme-cli
    # lsscsi (os_brick/initiator/linuxscsi.py).
    pkgs.lsscsi
    # mount.nfs, the helper mount(8) runs for `mount -t nfs`. nova mounts a
    # Cinder NFS export itself, below [libvirt] nfs_mount_point_base
    # ($state_path/mnt): nova/virt/libvirt/volume/nfs.py,
    # nova/virt/libvirt/volume/mount.py and mount() in nova/privsep/fs.py.
    # os-brick takes no part in it. nixpkgs installs it into bin/; the root
    # links below put it where mount(8) and noble looked for it.
    pkgs.nfs-utils
    # cryptsetup, run by the LUKS encryptors (os_brick/encryptors/luks.py)
    # when an encrypted volume attaches. The compute contract enables the
    # Barbican key manager whenever Barbican is enabled.
    pkgs.cryptsetup.bin
    # genisoimage, the default [DEFAULT] mkisofs_cmd
    # (nova/conf/configdrive.py), which builds config drives.
    pkgs.cdrkit
    # ceph and rbd (nova/storage/rbd_utils.py): `ceph mon dump` in
    # get_mon_addrs, `rbd import` in import_image, `rbd export` in
    # export_image and `ceph df` in get_pool_info.
    pkgs.ceph-client
    # The tools noble's package closure carried that nova's privsep and
    # os-brick run by name: mount, umount, blockdev, findmnt, lsblk, blkid,
    # losetup, mkswap, mkfs and nsenter (util-linux), mkfs.ext4, e2fsck and
    # resize2fs (e2fsprogs), modprobe (kmod, built with xz and zstd), sg_scan
    # (sg3_utils) and dmsetup (lvm2).
    pkgs.util-linux.bin
    pkgs.e2fsprogs.bin
    pkgs.kmod
    pkgs.sg3_utils
    pkgs.lvm2.bin
  ];

  rootwrapConf = pkgs.writeText "rootwrap.conf" (
    import ../../nix/lib/rootwrap-conf.nix {
      filtersPath = "/var/lib/openstack/etc/nova/rootwrap.d";
      execDirs = map (p: "${lib.getBin p}/bin") packages;
    }
  );
in
serviceImage {
  env = env // {
    name = "nova-compute";
    venv = env.venvWith [ "nova-compute" ];
  };
  description = "OpenStack nova-compute with the libvirt driver and host storage tooling";

  inherit packages;

  rootLinks = {
    # get_scsi_wwn (os_brick/initiator/linuxscsi.py). nixpkgs' udev is
    # systemd-minimal-libs, which ships no binaries.
    "lib/udev/scsi_id" = "${pkgs.systemd}/lib/udev/scsi_id";
    # udevadm settle and trigger (os-brick). Full systemd is not a package of
    # the image, which would put reboot, halt and init on PATH.
    "usr/bin/udevadm" = "${pkgs.systemd}/bin/udevadm";
    # The links below make /sbin a directory of the image root, which takes
    # the place of the sbin -> bin link openiscsi ships.
    "sbin/iscsiadm" = "${pkgs.openiscsi}/bin/iscsiadm";
  }
  # util-linux looks for mount.<type> helpers in
  # /run/wrappers/bin:/run/current-system/sw/bin:/sbin, and noble put
  # mount.nfs at /usr/sbin.
  // lib.genAttrs [
    "sbin/mount.nfs"
    "sbin/mount.nfs4"
    "sbin/umount.nfs"
    "sbin/umount.nfs4"
    "usr/sbin/mount.nfs"
  ] (rel: "${pkgs.nfs-utils}/bin/${baseNameOf rel}");

  # The state directories Nova writes to. /var/lib/nova is meant as [DEFAULT]
  # state_path, where a compute stores its node identity, compute_id; nova's
  # default is $pybasedir, so the configuration has to set it.
  # /var/lib/nova/tmp is the [oslo_concurrency] lock_path, and
  # /var/lib/nova/instances is instances_path, the default
  # $state_path/instances. All three ship empty and owned by 42424. The
  # consumer (#1061) mounts /var/lib/nova from the host and sets state_path
  # to it: compute_id must survive pod recreation, or the next start
  # generates a new node UUID that collides with the existing ComputeNode
  # record. That mount replaces this layout, so a plain `docker run` is the
  # only one that gets it; the pod has to give the host directories the same
  # owner.
  stateDirs = [
    "var/lib/nova"
    "var/lib/nova/instances"
    "var/lib/nova/tmp"
  ];

  inherit cephBindings;

  extraFakeRootCommands = ''
    rm -f /etc/iscsi/initiatorname.iscsi /etc/nvme/hostnqn /etc/nvme/hostid /etc/multipath.conf
    mkdir -p /etc/nova
    install -o root -g root -m 0644 ${rootwrapConf} /etc/nova/rootwrap.conf
    install -o root -g root -m 0440 ${./sudoers} /etc/sudoers.d/nova-compute
    ${pkgs.sudo}/bin/visudo -csf /etc/sudoers.d/nova-compute
  '';
}
