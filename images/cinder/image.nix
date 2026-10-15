# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The cinder block storage service. No WSGI entry script ships:
# cinder/wsgi/api.py binds a module-level `application`, so the
# cinder-operator launches uWSGI with `--module cinder.wsgi.api:application`
# and `--pyargv "--config-dir <dir>"`. The image stays config-free; the
# package data files api-paste.ini, resource_filters.json, rootwrap.conf and
# rootwrap.d/volume.filters ship with the venv under
# /var/lib/openstack/etc/cinder/.
#
# The root helper is `sudo cinder-rootwrap` with no sudoers entry, so the
# service user runs nothing as root (nix/lib/service-image.nix installs sudo
# without a rule for it).
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
serviceImage {
  inherit env;
  description = "OpenStack cinder service";

  # The exec readiness probe of the cinder-scheduler, cinder-volume and
  # cinder-backup processes: it reports ready when a process of this
  # container holds a socket established to the broker port. The
  # cinder-operator wires it as
  # `readinessProbe.exec.command: ["/var/lib/openstack/bin/cinder-amqp-ready"]`;
  # the two paths must stay in lockstep.
  extraRoots."bin/cinder-amqp-ready" = pkgs.runCommand "cinder-amqp-ready" { } ''
    install -m 0555 ${./cinder-amqp-ready} "$out"
  '';

  packages = [
    # qemu-img: create-from-image, clone and extend go through
    # cinder/image/image_utils.py (fetch_to_raw, resize_image, convert_image)
    # and the LUKS qcow2 pre-create in cinder/volume/drivers/remotefs.py.
    pkgs.qemu-utils
    # The NFS driver's do_setup probes for mount.nfs through the root helper
    # (cinder/volume/drivers/nfs.py) and raises NfsException when the probe's
    # argv[0] is missing; the pod never mounts, and the probe's non-zero exit
    # is tolerated. nixpkgs installs mount.nfs into bin/, so the root link
    # below keeps the path /usr/sbin/mount.nfs.
    pkgs.nfs-utils
    # ceph, rbd and rados. The RBD volume driver (cinder/volume/drivers/rbd.py)
    # runs `ceph mon dump --format=json` in _get_mon_addrs, `rbd import` in
    # _create_encrypted_volume and _copy_image_to_volume, `rbd export` in
    # copy_volume_to_image and `rbd status` in _get_image_status. The Ceph
    # backup driver (cinder/backup/drivers/ceph.py) pipes `rbd export-diff`
    # into `rbd import-diff` in _rbd_diff_transfer.
    pkgs.ceph-client
  ];

  rootLinks."usr/sbin/mount.nfs" = "${pkgs.nfs-utils}/bin/mount.nfs";

  # The state directories Cinder writes to, empty and owned by 42424 with
  # /var/lib/cinder itself. /var/lib/cinder is [DEFAULT] state_path; mnt and
  # backup_mount are the two os-brick mount bases (nfs_mount_point_base,
  # backup_mount_point_base); conversion is image_conversion_dir; tmp is
  # [oslo_concurrency] lock_path; coordination is the tooz file:// directory.
  # The cinder-operator mounts an emptyDir over /var/lib/cinder under
  # readOnlyRootFilesystem, so this layout is what a plain `docker run` gets.
  stateDirs = [
    "var/lib/cinder"
  ]
  ++ map (dir: "var/lib/cinder/${dir}") [
    "mnt"
    "backup_mount"
    "conversion"
    "tmp"
    "coordination"
  ];

  # The rados and rbd modules both drivers import behind a guard;
  # check_for_setup_error raises "rados and rbd python libraries not found"
  # without them.
  inherit cephBindings;
}
