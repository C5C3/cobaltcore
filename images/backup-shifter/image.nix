# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# rclone shifter for the OVN database backups. It is the 'shifter' container
# of the OVNCentral backup CronJob and exists only when spec.backup.s3 is set:
# the 'backup' init container writes the northbound and southbound snapshots
# to a PVC, then this container copies that PVC to the configured S3 bucket
# with 'rclone copy'. Release-independent: rclone comes from the
# nixpkgs-images revision flake.lock pins, not from an OpenStack release.
#
# The image is the FHS façade of nix/lib/fhs-layout.nix without a Python
# environment, rclone, and coreutils. The façade gives the operator's
# '/bin/sh -c "rclone copy ..."' its shell and rclone the CA bundle it
# verifies the HTTPS S3 endpoint against; coreutils gives the verify script
# its 'id' and 'test'. /tmp exists with mode 1777, so rclone's temporary
# files have a home when the image runs without the CronJob's emptyDir.
#
# There is no Cmd and no Entrypoint: the operator renders the rclone command
# onto the CronJob container together with the RCLONE_S3_* environment.
#
# DEVIATION from the original build-pipeline design: the image derives from
# no python-base, so it carries the generic 'openstack' user and group
# (UID/GID 42424) itself, through the fakeNss entry of the façade. rclone
# brings no service account of its own, and the same identity across all
# images lets the CronJob keep one pod security context for the 'backup'
# init container and this one.
{ pkgs, fhsLayout }:
let
  defaults = import ../../nix/lib/image-defaults.nix;
in
pkgs.dockerTools.streamLayeredImage {
  name = "backup-shifter";
  tag = "latest";
  inherit (defaults) created;

  contents = [
    (fhsLayout { })
    pkgs.rclone
    pkgs.coreutils
  ];

  extraCommands = "mkdir -m 1777 tmp";

  config = {
    User = "${toString defaults.uid}:${toString defaults.gid}";
    Env = defaults.env;
    Labels = defaults.labels // {
      "org.opencontainers.image.title" = "backup-shifter";
      "org.opencontainers.image.description" =
        "rclone shifter that copies OVN database backups from a PVC to S3";
    };
  };
}
