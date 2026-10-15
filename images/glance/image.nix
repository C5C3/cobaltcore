# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The glance image service. Every wired release runs the API under uWSGI with
# the hand-shipped glance-wsgi-api script, which redirects glance's WSGI
# config discovery to the --config-dir roots passed through uWSGI --pyargv
# (see the script header); the glance-operator references it as
# `--wsgi-file /var/lib/openstack/bin/glance-wsgi-api`, and the two paths
# must stay in lockstep. The venv carries glance_store[s3] and lhafile from
# the pip_packages of releases/<release>/extra-packages.yaml. The image stays
# config-free: the operator mounts glance-api.conf, glance-api-paste.ini and
# the policy, and provides the staging and tasks paths as emptyDir mounts.
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
  description = "OpenStack glance service";

  extraRoots."bin/glance-wsgi-api" = pkgs.runCommand "glance-wsgi-api" { } ''
    install -m 0555 ${./glance-wsgi-api} "$out"
  '';

  # The image_conversion image-import plugin shells out to `qemu-img info` and
  # `qemu-img convert` on the staged image.
  packages = [ pkgs.qemu-utils ];

  # The rados and rbd modules of the RBD store. glance_store/_drivers/rbd.py
  # imports both behind a guard, and configure_add raises
  # BadStoreConfiguration ("The required libraries(rbd and rados) are not
  # available") without them. The store runs no ceph or rbd binary, so the
  # image carries no ceph CLI.
  inherit cephBindings;
}
