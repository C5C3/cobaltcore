# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The placement service, the project openstack-placement. The
# placement-operator runs it under uWSGI with
# `--wsgi-file /var/lib/openstack/bin/placement-api`. The image stays
# config-free: placement locates its configuration through the
# OS_PLACEMENT_CONFIG_DIR environment variable the operator sets.
#
# DEVIATION from the original build-pipeline design: the image runs as the
# generic 'openstack' user (UID/GID 42424) instead of a per-service user; see
# the DEVIATION paragraph of nix/lib/service-image.nix.
{
  pkgs,
  serviceImage,
  env,
  ...
}:
serviceImage {
  inherit env;
  description = "OpenStack placement service";

  # Neither wired release declares a placement-api WSGI script: 2026.1 moved
  # packaging to pyproject.toml with no wsgi_scripts entry, and 2026.2 keeps
  # it that way. The entry therefore ships by hand under the upstream name.
  # The path and the operator's --wsgi-file flag have to stay in lockstep.
  extraRoots."bin/placement-api" = pkgs.writeScript "placement-api" ''
    #!/var/lib/openstack/bin/python
    from placement.wsgi import init_application
    application = init_application()
  '';
}
